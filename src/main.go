package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "tailscale.com/feature/useproxy"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/net/netns"
	"tailscale.com/tsnet"
)

// Config represents runtime configurations stored in portable directory.
type Config struct {
	Port       int    `json:"port"`
	Target     string `json:"target"`
	Hostname   string `json:"hostname"`
	AuthKey    string `json:"auth_key,omitempty"`
	ControlURL string `json:"control_url,omitempty"`
	Verbose    bool   `json:"verbose,omitempty"`
}

func main() {
	// For user-space tsnet without system TUN, disable binding sockets to a specific
	// physical interface on macOS so that traffic correctly traverses transparent proxies
	// or VPNs without black-holing.
	netns.SetDisableBindConnToInterface(func(string, ...any) {}, true)

	var (
		portFlag       int
		targetFlag     string
		hostnameFlag   string
		authKeyFlag    string
		controlURLFlag string
		dirFlag        string
		verboseFlag    bool
	)

	flag.IntVar(&portFlag, "p", 0, "Port to forward (e.g. 445)")
	flag.IntVar(&portFlag, "port", 0, "Port to forward (e.g. 445)")
	flag.StringVar(&targetFlag, "target", "", "Target address to forward to (default: 127.0.0.1:<port>)")
	flag.StringVar(&hostnameFlag, "name", "", "Tailscale node hostname (default: pfgots-<port>)")
	flag.StringVar(&hostnameFlag, "hostname", "", "Tailscale node hostname (default: pfgots-<port>)")
	flag.StringVar(&authKeyFlag, "authkey", "", "Tailscale auth key (optional, login URL prompt if empty)")
	flag.StringVar(&controlURLFlag, "control-url", "", "Custom control server URL (e.g. Headscale)")
	flag.StringVar(&dirFlag, "dir", "", "Path to portable directory (default: directory containing the executable)")
	flag.BoolVar(&verboseFlag, "verbose", false, "Enable verbose Tailscale internal logs")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s -p <port> [options]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
	}

	flag.Parse()

	portableDir, err := resolvePortableDir(dirFlag)
	if err != nil {
		log.Fatalf("[FATAL] failed to resolve portable directory: %v", err)
	}

	if err := os.MkdirAll(portableDir, 0755); err != nil {
		log.Fatalf("[FATAL] failed to create portable directory %s: %v", portableDir, err)
	}

	configFile := filepath.Join(portableDir, "config.json")
	cfg := loadConfig(configFile)

	if portFlag != 0 {
		cfg.Port = portFlag
	}
	if targetFlag != "" {
		cfg.Target = targetFlag
	}
	if hostnameFlag != "" {
		cfg.Hostname = hostnameFlag
	}
	if authKeyFlag != "" {
		cfg.AuthKey = authKeyFlag
	}
	if controlURLFlag != "" {
		cfg.ControlURL = controlURLFlag
	}
	if verboseFlag {
		cfg.Verbose = true
	}

	if cfg.Port <= 0 || cfg.Port > 65535 {
		flag.Usage()
		log.Fatalf("[FATAL] invalid port: %d. Please specify a valid port via -p <port>", cfg.Port)
	}

	// Persist merged explicit configuration.
	saveConfig(configFile, cfg)

	effectiveTarget := normalizeTarget(cfg.Target, cfg.Port)
	effectiveHostname := cfg.Hostname
	if effectiveHostname == "" {
		effectiveHostname = fmt.Sprintf("pfgots-%d", cfg.Port)
	}

	stateDir := resolveStateDir(portableDir)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		log.Fatalf("[FATAL] failed to create tailscale state directory: %v", err)
	}

	log.Printf("[INFO] Portable directory: %s", portableDir)
	log.Printf("[INFO] Tailscale state directory: %s", stateDir)
	log.Printf("[INFO] Starting Tailscale node '%s'...", effectiveHostname)

	tsServer := &tsnet.Server{
		Dir:        stateDir,
		Hostname:   effectiveHostname,
		AuthKey:    cfg.AuthKey,
		ControlURL: cfg.ControlURL,
		UserLogf: func(format string, args ...any) {
			msg := strings.TrimSpace(fmt.Sprintf(format, args...))
			if msg != "" {
				log.Printf("[TAILSCALE] %s", msg)
			}
		},
		Logf: func(format string, args ...any) {
			msg := strings.TrimSpace(fmt.Sprintf(format, args...))
			if msg == "" {
				return
			}
			if cfg.Verbose {
				log.Printf("[TAILSCALE DEBUG] %s", msg)
			}
		},
	}

	defer tsServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			log.Println("[INFO] Shutdown signal received, closing listener and Tailscale node...")
			cancel()
			_ = tsServer.Close()
		case <-ctx.Done():
		}
	}()

	status, err := waitAndAuthenticate(ctx, tsServer)
	if err != nil {
		if ctx.Err() != nil {
			log.Println("[INFO] Tailscale node stopped")
			return
		}
		log.Fatalf("[FATAL] Tailscale connection failed: %v", err)
	}

	var ip4, ip6 netip.Addr
	for _, ip := range status.TailscaleIPs {
		if ip.Is4() && !ip4.IsValid() {
			ip4 = ip
		} else if ip.Is6() && !ip6.IsValid() {
			ip6 = ip
		}
	}

	nodeDNSName := effectiveHostname
	if status.Self != nil && status.Self.DNSName != "" {
		nodeDNSName = status.Self.DNSName
	}

	fmt.Println("==================================================")
	fmt.Printf("Tailscale Node: %s\n", nodeDNSName)
	if ip4.IsValid() {
		fmt.Printf("Tailscale IPv4: %s\n", ip4.String())
	}
	if ip6.IsValid() {
		fmt.Printf("Tailscale IPv6: %s\n", ip6.String())
	}
	fmt.Printf("Forwarding:     [Tailscale]:%d -> %s\n", cfg.Port, effectiveTarget)
	fmt.Println("==================================================")

	listener, err := tsServer.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		log.Fatalf("[FATAL] failed to listen on Tailscale port %d: %v", cfg.Port, err)
	}
	defer listener.Close()

	log.Printf("[INFO] Listening on Tailscale port %d, ready for connections", cfg.Port)

	for {
		clientConn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
				log.Printf("[WARN] Accept connection error: %v", err)
				continue
			}
		}

		go handleClient(clientConn, effectiveTarget)
	}
}

func normalizeTarget(rawTarget string, fallbackPort int) string {
	rawTarget = strings.TrimSpace(rawTarget)
	if rawTarget == "" {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(fallbackPort))
	}
	if _, err := strconv.Atoi(rawTarget); err == nil {
		return net.JoinHostPort("127.0.0.1", rawTarget)
	}
	if _, _, err := net.SplitHostPort(rawTarget); err == nil {
		return rawTarget
	}
	host := strings.Trim(rawTarget, "[]")
	return net.JoinHostPort(host, strconv.Itoa(fallbackPort))
}

func waitAndAuthenticate(ctx context.Context, s *tsnet.Server) (*ipnstate.Status, error) {
	if err := s.Start(); err != nil {
		return nil, fmt.Errorf("tsnet.Start: %w", err)
	}

	lc, err := s.LocalClient()
	if err != nil {
		return nil, fmt.Errorf("LocalClient: %w", err)
	}

	watcher, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		return nil, fmt.Errorf("WatchIPNBus: %w", err)
	}
	defer watcher.Close()

	var (
		lastPrintedURL string
		isRunning      bool
		mu             sync.Mutex
	)

	// Background ticker to capture AuthURL from status as a fallback.
	pollDone := make(chan struct{})
	defer close(pollDone)
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-pollDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				st, err := lc.StatusWithoutPeers(ctx)
				if err == nil && st != nil && st.AuthURL != "" {
					mu.Lock()
					if st.AuthURL != lastPrintedURL {
						lastPrintedURL = st.AuthURL
						printAuthBanner(lastPrintedURL)
					}
					mu.Unlock()
				}
			}
		}
	}()

	for {
		n, err := watcher.Next()
		if err != nil {
			return nil, err
		}

		if n.ErrMessage != nil {
			return nil, fmt.Errorf("backend: %s", *n.ErrMessage)
		}

		if n.BrowseToURL != nil && *n.BrowseToURL != "" {
			mu.Lock()
			if *n.BrowseToURL != lastPrintedURL {
				lastPrintedURL = *n.BrowseToURL
				printAuthBanner(lastPrintedURL)
			}
			mu.Unlock()
		}

		if st := n.State; st != nil && *st == ipn.Running {
			isRunning = true
		}

		if isRunning {
			status, err := lc.Status(ctx)
			if err == nil && status != nil && len(status.TailscaleIPs) > 0 {
				return status, nil
			}
		}
	}
}

func printAuthBanner(authURL string) {
	fmt.Println()
	fmt.Println("================================================================================")
	fmt.Println("To authenticate this device, please visit:")
	fmt.Println(authURL)
	fmt.Println("================================================================================")
	fmt.Println()
}

func handleClient(clientConn net.Conn, targetAddr string) {
	defer clientConn.Close()

	remoteAddr := "unknown"
	if ra := clientConn.RemoteAddr(); ra != nil {
		remoteAddr = ra.String()
	}
	log.Printf("[CONN] Accepted incoming connection from %s", remoteAddr)

	targetConn, err := net.DialTimeout("tcp", targetAddr, 5*time.Second)
	if err != nil {
		log.Printf("[ERROR] Failed to dial local target %s for client %s: %v", targetAddr, remoteAddr, err)
		return
	}
	defer targetConn.Close()

	pipeBridge(clientConn, targetConn)
	log.Printf("[CONN] Closed connection from %s", remoteAddr)
}

func pipeBridge(c1, c2 io.ReadWriteCloser) {
	var once sync.Once
	closeBoth := func() {
		_ = c1.Close()
		_ = c2.Close()
	}

	errc := make(chan error, 2)
	copyHalf := func(dst, src io.ReadWriteCloser) {
		_, err := io.Copy(dst, src)
		if err == nil {
			if cw, ok := dst.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			} else {
				_ = dst.Close()
			}
		}
		errc <- err
	}

	go copyHalf(c1, c2)
	go copyHalf(c2, c1)

	firstErr := <-errc
	if firstErr != nil {
		once.Do(closeBoth)
	}
	<-errc
	once.Do(closeBoth)
}

func resolvePortableDir(customDir string) (string, error) {
	if customDir != "" {
		return filepath.Abs(customDir)
	}

	// In portable mode, the folder holding the executable is the self-contained portable directory.
	exePath, err := os.Executable()
	if err == nil {
		if realPath, err := filepath.EvalSymlinks(exePath); err == nil {
			exePath = realPath
		}
		exeDir := filepath.Dir(exePath)
		// Avoid treating temporary build/run directory as portable root.
		if !isTempDir(exeDir) {
			return filepath.Abs(exeDir)
		}
	}

	return filepath.Abs(".")
}

func isTempDir(path string) bool {
	if strings.Contains(path, ".tmp") {
		return true
	}
	dirs := []string{os.TempDir(), "/tmp", "/private/tmp"}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if realDir, err := filepath.EvalSymlinks(d); err == nil && realDir != "" {
			dirs = append(dirs, realDir)
		}
	}
	for _, d := range dirs {
		d = strings.TrimRight(d, string(filepath.Separator))
		if d != "" && (path == d || strings.HasPrefix(path, d+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

func resolveStateDir(portableDir string) string {
	// Follow the standard portable layout: <portableDir>/tailscaled
	primaryDir := filepath.Join(portableDir, "tailscaled")
	if fi, err := os.Stat(primaryDir); err == nil && fi.IsDir() {
		return primaryDir
	}

	// Legacy backward compatibility if old "tailscale" directory exists.
	legacyDir := filepath.Join(portableDir, "tailscale")
	if fi, err := os.Stat(legacyDir); err == nil && fi.IsDir() {
		return legacyDir
	}

	return primaryDir
}

func loadConfig(path string) Config {
	var cfg Config
	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			log.Printf("[WARN] Failed to parse config file %s: %v", path, err)
		}
	}
	return cfg
}

func saveConfig(path string, cfg Config) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0644)
}
