package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadConfigMultiUserAndStateIsolation(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	rawJSON := `{
  "hostname": "my-nas",
  "state_dir": "custom-state",
  "control_url": "https://headscale.example.com",
  "verbose": true,
  "ports": "445, 80, 443",
  "target": "",
  "users": [
    {"name": "alice", "auth_key": "tskey-auth-alice"},
    {"name": "bob", "auth_key": "", "control_url": "https://other.example.com", "ports": 8080, "target": "192.168.1.10"}
  ]
}`
	if err := os.WriteFile(cfgPath, []byte(rawJSON), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	resolved, err := LoadConfig(cfgPath, dir)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}

	if resolved.Hostname != "my-nas" {
		t.Fatalf("expected hostname my-nas, got %q", resolved.Hostname)
	}
	wantStateRoot := filepath.Join(dir, "custom-state")
	if resolved.StateRoot != wantStateRoot {
		t.Fatalf("expected state root %q, got %q", wantStateRoot, resolved.StateRoot)
	}
	if !resolved.Verbose {
		t.Fatalf("expected verbose=true")
	}
	if len(resolved.Users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(resolved.Users))
	}

	alice := resolved.Users[0]
	if alice.Name != "alice" || alice.AuthKey != "tskey-auth-alice" {
		t.Fatalf("unexpected alice: %+v", alice)
	}
	if alice.ControlURL != "https://headscale.example.com" {
		t.Fatalf("expected alice to inherit global control_url, got %q", alice.ControlURL)
	}
	if alice.StateDir != filepath.Join(wantStateRoot, "alice") {
		t.Fatalf("unexpected alice state dir: %q", alice.StateDir)
	}
	if len(alice.Forwards) != 3 ||
		alice.Forwards[0] != (ForwardRule{Port: 445, Target: "127.0.0.1:445"}) ||
		alice.Forwards[1] != (ForwardRule{Port: 80, Target: "127.0.0.1:80"}) ||
		alice.Forwards[2] != (ForwardRule{Port: 443, Target: "127.0.0.1:443"}) {
		t.Fatalf("unexpected alice forwards: %+v", alice.Forwards)
	}

	bob := resolved.Users[1]
	if bob.Name != "bob" || bob.AuthKey != "" {
		t.Fatalf("unexpected bob: %+v", bob)
	}
	if bob.ControlURL != "https://other.example.com" {
		t.Fatalf("expected bob control_url override, got %q", bob.ControlURL)
	}
	if bob.StateDir != filepath.Join(wantStateRoot, "bob") {
		t.Fatalf("unexpected bob state dir: %q", bob.StateDir)
	}
	if len(bob.Forwards) != 1 || bob.Forwards[0].Port != 8080 || bob.Forwards[0].Target != "192.168.1.10:8080" {
		t.Fatalf("unexpected bob forwards: %+v", bob.Forwards)
	}
	if alice.StateDir == bob.StateDir {
		t.Fatalf("state dirs must be distinct: %q", alice.StateDir)
	}

	// Ensure config.json is not modified on load.
	afterBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config after load: %v", err)
	}
	if string(afterBytes) != rawJSON {
		t.Fatalf("config file was mutated during LoadConfig")
	}
}

func TestLoadConfigNumericAndTargetOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	rawJSON := `{
  "hostname": "nas",
  "ports": 445,
  "users": [
    {"name": "u1"},
    {"name": "u2", "target": "192.168.1.20"}
  ]
}`
	if err := os.WriteFile(cfgPath, []byte(rawJSON), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	resolved, err := LoadConfig(cfgPath, dir)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if got := resolved.Users[0].Forwards[0]; got != (ForwardRule{Port: 445, Target: "127.0.0.1:445"}) {
		t.Fatalf("unexpected u1 forward: %+v", got)
	}
	if got := resolved.Users[1].Forwards[0]; got != (ForwardRule{Port: 445, Target: "192.168.1.20:445"}) {
		t.Fatalf("unexpected u2 forward: %+v", got)
	}
}

func TestResolveConfigValidationErrors(t *testing.T) {
	baseDir := t.TempDir()

	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "empty users",
			cfg: Config{
				Hostname: "nas",
				Ports:    "445",
				Users:    nil,
			},
		},
		{
			name: "empty user name",
			cfg: Config{
				Hostname: "nas",
				Ports:    "445",
				Users:    []UserConfig{{Name: "  "}},
			},
		},
		{
			name: "duplicate user name case insensitive",
			cfg: Config{
				Hostname: "nas",
				Ports:    "445",
				Users: []UserConfig{
					{Name: "Alice"},
					{Name: "alice"},
				},
			},
		},
		{
			name: "path traversal user name",
			cfg: Config{
				Hostname: "nas",
				Ports:    "445",
				Users:    []UserConfig{{Name: "../evil"}},
			},
		},
		{
			name: "slash in user name",
			cfg: Config{
				Hostname: "nas",
				Ports:    "445",
				Users:    []UserConfig{{Name: "a/b"}},
			},
		},
		{
			name: "leading dot in user name",
			cfg: Config{
				Hostname: "nas",
				Ports:    "445",
				Users:    []UserConfig{{Name: ".hidden"}},
			},
		},
		{
			name: "space in user name",
			cfg: Config{
				Hostname: "nas",
				Ports:    "445",
				Users:    []UserConfig{{Name: "user 1"}},
			},
		},
		{
			name: "omitted ports",
			cfg: Config{
				Hostname: "nas",
				Target:   "127.0.0.1:445",
				Users:    []UserConfig{{Name: "user1"}},
			},
		},
		{
			name: "empty port entry in list",
			cfg: Config{
				Hostname: "nas",
				Ports:    "445,",
				Users:    []UserConfig{{Name: "user1"}},
			},
		},
		{
			name: "port out of range",
			cfg: Config{
				Hostname: "nas",
				Ports:    "70000",
				Users:    []UserConfig{{Name: "user1"}},
			},
		},
		{
			name: "port zero rejected",
			cfg: Config{
				Hostname: "nas",
				Ports:    "0",
				Users:    []UserConfig{{Name: "user1"}},
			},
		},
		{
			name: "port zero numeric rejected",
			cfg: Config{
				Hostname: "nas",
				Ports:    "0",
				Users:    []UserConfig{{Name: "user1"}},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveConfig(tc.cfg, baseDir); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestRepoTemplateConfigValid(t *testing.T) {
	portableDir := t.TempDir()
	cfgPath := filepath.Join("..", "config.json")
	resolved, err := LoadConfig(cfgPath, portableDir)
	if err != nil {
		t.Fatalf("repo template config.json should be valid: %v", err)
	}
	if resolved.Hostname == "" {
		t.Fatalf("expected non-empty hostname")
	}
	if len(resolved.Users) < 1 {
		t.Fatalf("expected at least 1 user in template config.json")
	}
}

func TestNormalizeTarget(t *testing.T) {
	tests := []struct {
		name         string
		rawTarget    string
		fallbackPort int
		want         string
	}{
		{name: "empty target", rawTarget: "", fallbackPort: 445, want: "127.0.0.1:445"},
		{name: "port only", rawTarget: "8080", fallbackPort: 445, want: "127.0.0.1:8080"},
		{name: "host only", rawTarget: "192.168.1.100", fallbackPort: 445, want: "192.168.1.100:445"},
		{name: "host with port", rawTarget: "192.168.1.100:8443", fallbackPort: 445, want: "192.168.1.100:8443"},
		{name: "ipv6 with brackets and port", rawTarget: "[::1]:8080", fallbackPort: 445, want: "[::1]:8080"},
		{name: "ipv6 bracketed without port", rawTarget: "[::1]", fallbackPort: 445, want: "[::1]:445"},
		{name: "ipv6 raw without brackets or port", rawTarget: "::1", fallbackPort: 445, want: "[::1]:445"},
		{name: "colon port only", rawTarget: ":9090", fallbackPort: 445, want: ":9090"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeTarget(tc.rawTarget, tc.fallbackPort)
			if got != tc.want {
				t.Errorf("normalizeTarget(%q, %d) = %q, want %q", tc.rawTarget, tc.fallbackPort, got, tc.want)
			}
		})
	}
}

func TestIsTempDir(t *testing.T) {
	if !isTempDir("/tmp/go-build123") {
		t.Error("expected /tmp/go-build123 to be detected as temp dir")
	}
	if !isTempDir("/workspace/tspfa/.tmp/bin") {
		t.Error("expected .tmp path to be detected as temp dir")
	}
	if isTempDir("/opt/tspfa") {
		t.Error("did not expect /opt/tspfa to be detected as temp dir")
	}
}

func TestPipeBridge(t *testing.T) {
	c1a, c1b := net.Pipe()
	c2a, c2b := net.Pipe()

	done := make(chan struct{})
	go func() {
		pipeBridge(c1b, c2a)
		close(done)
	}()

	msg1 := []byte("hello-target")
	msg2 := []byte("hello-client")

	errCh := make(chan error, 2)
	go func() {
		_, err := c1a.Write(msg1)
		errCh <- err
	}()
	go func() {
		buf := make([]byte, len(msg1))
		_, err := c2b.Read(buf)
		if err == nil && string(buf) != string(msg1) {
			t.Errorf("got %q, want %q", buf, msg1)
		}
		_, wErr := c2b.Write(msg2)
		if wErr != nil {
			errCh <- wErr
			return
		}
		errCh <- err
	}()

	buf2 := make([]byte, len(msg2))
	if _, err := c1a.Read(buf2); err != nil {
		t.Fatalf("read back failed: %v", err)
	}
	if string(buf2) != string(msg2) {
		t.Fatalf("got %q, want %q", buf2, msg2)
	}

	_ = c1a.Close()
	_ = c2b.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeBridge did not exit after closing endpoints")
	}

	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("goroutine error: %v", err)
		}
	}
}

func TestRunUserNodeWithRetryRecoversAfterTransientErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var attempts atomic.Int32
	runUserNodeWithRetry(ctx, "alice", 10*time.Millisecond, 40*time.Millisecond, time.Minute, func(context.Context) error {
		n := attempts.Add(1)
		if n < 3 {
			return errors.New("transient network error")
		}
		return nil
	})

	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 attempts before recovery, got %d", got)
	}
}

func TestRunUserNodeWithRetryStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var attempts atomic.Int32
	done := make(chan struct{})
	go func() {
		runUserNodeWithRetry(ctx, "bob", 5*time.Second, 10*time.Second, time.Minute, func(context.Context) error {
			if attempts.Add(1) == 1 {
				go func() {
					time.Sleep(20 * time.Millisecond)
					cancel()
				}()
			}
			return errors.New("persistent error")
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("runUserNodeWithRetry did not exit promptly after context cancel")
	}

	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected 1 attempt before cancel during backoff, got %d", got)
	}
}

func TestRunUserNodeWithRetryResetsBackoffAfterStableRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var attempts atomic.Int32
	start := time.Now()
	runUserNodeWithRetry(ctx, "carol", 10*time.Millisecond, 500*time.Millisecond, 25*time.Millisecond, func(context.Context) error {
		switch attempts.Add(1) {
		case 1:
			// Short failure: next backoff becomes 20ms.
			return errors.New("quick fail")
		case 2:
			// Stable run >= resetWindow (25ms): backoff should reset to 10ms instead of 20ms/40ms.
			time.Sleep(30 * time.Millisecond)
			return errors.New("late fail")
		default:
			return nil
		}
	})

	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 attempts, got %d", got)
	}
	// Total elapsed should be roughly 10ms + 30ms + 10ms = ~50ms, well below 200ms.
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("expected backoff to reset after stable run, took %s", elapsed)
	}
}
