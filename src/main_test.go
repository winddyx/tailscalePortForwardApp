package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeTarget(t *testing.T) {
	tests := []struct {
		name         string
		rawTarget    string
		fallbackPort int
		want         string
	}{
		{
			name:         "empty target",
			rawTarget:    "",
			fallbackPort: 445,
			want:         "127.0.0.1:445",
		},
		{
			name:         "port only",
			rawTarget:    "8080",
			fallbackPort: 445,
			want:         "127.0.0.1:8080",
		},
		{
			name:         "host only",
			rawTarget:    "192.168.1.100",
			fallbackPort: 445,
			want:         "192.168.1.100:445",
		},
		{
			name:         "host with port",
			rawTarget:    "192.168.1.100:8443",
			fallbackPort: 445,
			want:         "192.168.1.100:8443",
		},
		{
			name:         "ipv6 with brackets and port",
			rawTarget:    "[::1]:8080",
			fallbackPort: 445,
			want:         "[::1]:8080",
		},
		{
			name:         "ipv6 bracketed without port",
			rawTarget:    "[::1]",
			fallbackPort: 445,
			want:         "[::1]:445",
		},
		{
			name:         "ipv6 raw without brackets or port",
			rawTarget:    "::1",
			fallbackPort: 445,
			want:         "[::1]:445",
		},
		{
			name:         "colon port only",
			rawTarget:    ":9000",
			fallbackPort: 445,
			want:         ":9000",
		},
		{
			name:         "all ports empty target",
			rawTarget:    "",
			fallbackPort: 0,
			want:         "127.0.0.1",
		},
		{
			name:         "all ports host only",
			rawTarget:    "192.168.1.100",
			fallbackPort: 0,
			want:         "192.168.1.100",
		},
		{
			name:         "all ports ipv6 bracketed",
			rawTarget:    "[::1]",
			fallbackPort: 0,
			want:         "::1",
		},
		{
			name:         "all ports host with zero port",
			rawTarget:    "127.0.0.1:0",
			fallbackPort: 0,
			want:         "127.0.0.1",
		},
		{
			name:         "all ports fixed target",
			rawTarget:    "192.168.1.100:8080",
			fallbackPort: 0,
			want:         "192.168.1.100:8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeTarget(tt.rawTarget, tt.fallbackPort)
			if got != tt.want {
				t.Errorf("normalizeTarget(%q, %d) = %q, want %q", tt.rawTarget, tt.fallbackPort, got, tt.want)
			}
		})
	}
}

func TestResolveForwardTarget(t *testing.T) {
	tests := []struct {
		name             string
		normalizedTarget string
		dstPort          int
		want             string
	}{
		{
			name:             "ipv4 host",
			normalizedTarget: "127.0.0.1",
			dstPort:          445,
			want:             "127.0.0.1:445",
		},
		{
			name:             "ipv6 host",
			normalizedTarget: "::1",
			dstPort:          8080,
			want:             "[::1]:8080",
		},
		{
			name:             "fixed host and port",
			normalizedTarget: "192.168.1.100:9000",
			dstPort:          8080,
			want:             "192.168.1.100:9000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveForwardTarget(tt.normalizedTarget, tt.dstPort)
			if got != tt.want {
				t.Errorf("resolveForwardTarget(%q, %d) = %q, want %q", tt.normalizedTarget, tt.dstPort, got, tt.want)
			}
		})
	}
}

func TestResolveStateDir(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "gtsa-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// Case 1: Brand new directory defaults to primary <portableDir>/tailscaled
	got := resolveStateDir(tmpDir)
	want := filepath.Join(tmpDir, "tailscaled")
	if got != want {
		t.Errorf("resolveStateDir for fresh dir = %q, want %q", got, want)
	}

	// Case 2: Existing primary directory
	if err := os.Mkdir(want, 0700); err != nil {
		t.Fatal(err)
	}
	got = resolveStateDir(tmpDir)
	if got != want {
		t.Errorf("resolveStateDir for existing primary = %q, want %q", got, want)
	}

	// Case 3: Legacy directory fallback when primary does not exist
	legacyParent, err := os.MkdirTemp("", "gtsa-legacy-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(legacyParent)

	legacyDir := filepath.Join(legacyParent, "tailscale")
	if err := os.Mkdir(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	got = resolveStateDir(legacyParent)
	if got != legacyDir {
		t.Errorf("resolveStateDir for legacy dir = %q, want %q", got, legacyDir)
	}
}

func TestLoadSaveConfig(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "gtsa-cfg-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	cfgPath := filepath.Join(tmpDir, "config.json")
	initialCfg := Config{
		Port:       "445",
		Target:     "127.0.0.1:445",
		Hostname:   "gtsa-445",
		AuthKey:    "tskey-auth-dummy",
		ControlURL: "https://controlplane.tailscale.com",
		Verbose:    true,
	}

	saveConfig(cfgPath, initialCfg)

	loadedCfg := loadConfig(cfgPath)
	if loadedCfg.Port != initialCfg.Port ||
		loadedCfg.Target != initialCfg.Target ||
		loadedCfg.Hostname != initialCfg.Hostname ||
		loadedCfg.AuthKey != initialCfg.AuthKey ||
		loadedCfg.ControlURL != initialCfg.ControlURL ||
		loadedCfg.Verbose != initialCfg.Verbose {
		t.Errorf("loadConfig mismatch: got %+v, want %+v", loadedCfg, initialCfg)
	}

	multiCfg := Config{
		Port:     "80,443",
		Target:   "127.0.0.1",
		Hostname: "gtsa-80-443",
	}
	saveConfig(cfgPath, multiCfg)
	loadedMulti := loadConfig(cfgPath)
	if loadedMulti.Port != multiCfg.Port || loadedMulti.Target != multiCfg.Target || loadedMulti.Hostname != multiCfg.Hostname {
		t.Errorf("loadConfig multi-port mismatch: got %+v, want %+v", loadedMulti, multiCfg)
	}
}

func TestParsePorts(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    []int
		wantErr bool
	}{
		{name: "empty defaults to all", spec: "", want: []int{0}},
		{name: "zero all ports", spec: "0", want: []int{0}},
		{name: "single port", spec: "445", want: []int{445}},
		{name: "multiple ports", spec: "80,443,8080", want: []int{80, 443, 8080}},
		{name: "multiple ports with spaces", spec: " 80 , 443 , 8080 ", want: []int{80, 443, 8080}},
		{name: "deduplicate ports preserving order", spec: "443,80,443", want: []int{443, 80}},
		{name: "trailing comma error", spec: "80,", wantErr: true},
		{name: "empty middle segment error", spec: "80,,443", wantErr: true},
		{name: "zero combined with port error", spec: "0,80", wantErr: true},
		{name: "out of range high", spec: "80,70000", wantErr: true},
		{name: "negative port", spec: "-1", wantErr: true},
		{name: "non numeric", spec: "80,abc", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePorts(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsePorts(%q) expected error, got %v", tc.spec, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePorts(%q) unexpected error: %v", tc.spec, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parsePorts(%q) = %v, want %v", tc.spec, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parsePorts(%q) = %v, want %v", tc.spec, got, tc.want)
				}
			}
		})
	}
}

func TestDefaultHostname(t *testing.T) {
	if got := defaultHostname([]int{0}); got != "gtsa-all" {
		t.Errorf("defaultHostname([0]) = %q, want %q", got, "gtsa-all")
	}
	if got := defaultHostname([]int{445}); got != "gtsa-445" {
		t.Errorf("defaultHostname([445]) = %q, want %q", got, "gtsa-445")
	}
	if got := defaultHostname([]int{80, 443}); got != "gtsa-80-443" {
		t.Errorf("defaultHostname([80, 443]) = %q, want %q", got, "gtsa-80-443")
	}
	many := make([]int, 20)
	for i := range many {
		many[i] = 10000 + i
	}
	if got := defaultHostname(many); got != "gtsa-multi" {
		t.Errorf("defaultHostname(many) = %q, want %q", got, "gtsa-multi")
	}
}

func TestIsTempDir(t *testing.T) {
	if !isTempDir(filepath.Join("Z:", "project", ".tmp", "bin")) {
		t.Errorf("isTempDir should return true for paths with .tmp")
	}

	sysTemp := os.TempDir()
	if sysTemp != "" && !isTempDir(sysTemp) {
		t.Errorf("isTempDir should return true for os.TempDir(): %q", sysTemp)
	}

	if isTempDir(filepath.Join("Z:", "Users", "mod", "workspace", "portforwar-gotsnet")) {
		t.Errorf("isTempDir should return false for normal workspace path")
	}
}

func TestPipeBridge(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	done := make(chan struct{})
	go func() {
		pipeBridge(c1, c2)
		close(done)
	}()

	// Since net.Pipe in-memory conn does not support half-close, closing c1 should finish pipeBridge.
	_ = c1.Close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pipeBridge did not terminate after connection close")
	}
}
