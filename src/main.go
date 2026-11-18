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

// PortSpec represents either a single port (0-65535) or a comma-separated list of ports.
// It unmarshals from both JSON numbers (backward compatibility) and JSON strings.
type PortSpec string

// UnmarshalJSON parses a port specification from a JSON number or string.
func (p *PortSpec) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*p = ""
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*p = PortSpec(strings.TrimSpace(s))
		return nil
	}
	var num int
	if err := json.Unmarshal(data, &num); err != nil {
		return err
	}
	*p = PortSpec(strconv.Itoa(num))
	return nil
}

// MarshalJSON encodes a single port as a JSON number and multi-port specs as a JSON string.
func (p PortSpec) MarshalJSON() ([]byte, error) {
	s := strings.TrimSpace(string(p))
	if s == "" {
		return json.Marshal(0)
	}
	if num, err := strconv.Atoi(s); err == nil {
		return json.Marshal(num)
	}
	return json.Marshal(s)
}

// Config represents runtime configurations stored in portable directory.
type Config struct {
	Port       PortSpec `json:"port"`
	Target     string   `json:"target"`
	Hostname   string   `json:"hostname"`
	AuthKey    string   `json:"auth_key,omitempty"`
	ControlURL string   `json:"control_url,omitempty"`
	Verbose    bool     `json:"verbose,omitempty"`
}

func main() {
	// For user-space tsnet without system TUN, disable binding sockets to a specific
	// physical interface on macOS so that traffic correctly traverses transparent proxies
	// or VPNs without black-holing.
	netns.SetDisableBindConnToInterface(func(string, ...any) {}, true)

	var (
		portFlag       string
		targetFlag     string
		hostnameFlag   string
		authKeyFlag    string
		controlURLFlag string
		dirFlag        string
		verboseFlag    bool
	)

	flag.StringVar(&portFlag, "p", "", "Port(s) to forward, comma-separated (e.g. 445 or 80,443; 0 to forward all local ports)")
	flag.StringVar(&portFlag, "port", "", "Port(s) to forward, comma-separated (e.g. 445 or 80,443; 0 to forward all local ports)")
	flag.StringVar(&targetFlag, "target", "", "Target address or host to forward to (default: 127.0.0.1:<port>, or 127.0.0.1 when port is 0 or multiple)")
	flag.StringVar(&hostnameFlag, "name", "", "Tailscale node hostname (default: gtsa-<port>, gtsa-<p1>-<p2>, or gtsa-all when port is 0)")
	flag.StringVar(&hostnameFlag, "hostname", "", "Tailscale node hostname (default: gtsa-<port>, gtsa-<p1>-<p2>, or gtsa-all when port is 0)")
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

	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "p", "port":
			cfg.Port = PortSpec(portFlag)
		case "target":
			cfg.Target = targetFlag
		case "name", "hostname":
			cfg.Hostname = hostnameFlag
		case "authkey":
			cfg.AuthKey = authKeyFlag
		case "control-url":
			cfg.ControlURL = controlURLFlag
		case "verbose":
			cfg.Verbose = verboseFlag
		}
	})

	ports, err := parsePorts(string(cfg.Port))
	if err != nil {
		flag.Usage()
		log.Fatalf("[FATAL] invalid port specification %q: %v", cfg.Port, err)
	}
	cfg.Port = formatPorts(ports)

	fallbackPort := 0
	if len(ports) == 1 {
		fallbackPort = ports[0]
	}
	effectiveTarget := normalizeTarget(cfg.Target, fallbackPort)
	effectiveHostname := cfg.Hostname
	if effectiveHostname == "" {
		effectiveHostname = defaultHostname(ports)
	}
	cfg.Target = effectiveTarget
	cfg.Hostname = effectiveHostname

	// Persist merged explicit configuration.
	saveConfig(configFile, cfg)

	stateDir := resolveStateDir(portableDir)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		log.Fatalf("[FATAL] failed to create tailscale state directory: %v", err)
	}

	log.Printf("[INFO] Portable directory: %s", portableDir)
	log.Printf("[INFO] Tailscale state directory: %s", stateDir)
	log.Printf("[INFO] Starting Tailscale node '%s'...", effectiveHostname)

	if cfg.AuthKey != "" {
		_ = os.Setenv("TSNET_FORCE_LOGIN", "1")
	}

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
	if len(ports) == 1 && ports[0] == 0 {
		if _, _, err := net.SplitHostPort(effectiveTarget); err == nil {
			fmt.Printf("Forwarding:     [Tailscale]:* -> %s (all local ports)\n", effectiveTarget)
		} else {
			fmt.Printf("Forwarding:     [Tailscale]:* -> %s:* (all local ports)\n", effectiveTarget)
		}
		fmt.Println("==================================================")

		deregister := tsServer.RegisterFallbackTCPHandler(func(src, dst netip.AddrPort) (func(net.Conn), bool) {
			dstPort := dst.Port()
			if dstPort == 0 {
				return nil, false
			}
			targetAddr := resolveForwardTarget(effectiveTarget, int(dstPort))
			return func(clientConn net.Conn) {
				handleClient(clientConn, targetAddr)
			}, true
		})
		defer deregister()

		log.Println("[INFO] Listening on all Tailscale TCP ports, ready for connections")
		<-ctx.Done()
		return
	}

	listeners := make([]net.Listener, 0, len(ports))
	for _, port := range ports {
		ln, err := tsServer.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			log.Fatalf("[FATAL] failed to listen on Tailscale port %d: %v", port, err)
		}
		listeners = append(listeners, ln)
	}

	for _, port := range ports {
		targetAddr := resolveForwardTarget(effectiveTarget, port)
		fmt.Printf("Forwarding:     [Tailscale]:%d -> %s\n", port, targetAddr)
	}
	fmt.Println("==================================================")

	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
	}()

	log.Printf("[INFO] Listening on Tailscale port(s) %s, ready for connections", formatPorts(ports))

	var wg sync.WaitGroup
	for i, ln := range listeners {
		port := ports[i]
		targetAddr := resolveForwardTarget(effectiveTarget, port)
		wg.Add(1)
		go func(listener net.Listener, listenPort int, dst string) {
			defer wg.Done()
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
						log.Printf("[WARN] Accept connection error on port %d: %v", listenPort, err)
						continue
					}
				}

				go handleClient(clientConn, dst)
			}
		}(ln, port, targetAddr)
	}

	<-ctx.Done()
	for _, ln := range listeners {
		_ = ln.Close()
	}
	wg.Wait()
}

func parsePorts(spec string) ([]int, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return []int{0}, nil
	}

	parts := strings.Split(trimmed, ",")
	ports := make([]int, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))

	for _, part := range parts {
		token := strings.TrimSpace(part)
		if token == "" {
			return nil, fmt.Errorf("empty port entry in %q", spec)
		}
		p, err := strconv.Atoi(token)
		if err != nil || p < 0 || p > 65535 {
			return nil, fmt.Errorf("port %q must be an integer between 0 and 65535", token)
		}
		if _, exists := seen[p]; !exists {
			seen[p] = struct{}{}
			ports = append(ports, p)
		}
	}

	if len(ports) > 1 {
		for _, p := range ports {
			if p == 0 {
				return nil, fmt.Errorf("port 0 (all ports) cannot be combined with specific ports")
			}
		}
	}

	return ports, nil
}

func formatPorts(ports []int) PortSpec {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return PortSpec(strings.Join(parts, ","))
}

func defaultHostname(ports []int) string {
	if len(ports) == 1 && ports[0] == 0 {
		return "gtsa-all"
	}
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	name := "gtsa-" + strings.Join(parts, "-")
	if len(name) <= 63 {
		return name
	}
	return "gtsa-multi"
}

func normalizeTarget(rawTarget string, fallbackPort int) string {
	rawTarget = strings.TrimSpace(rawTarget)
	if fallbackPort == 0 {
		if rawTarget == "" {
			return "127.0.0.1"
		}
		if portNum, err := strconv.Atoi(rawTarget); err == nil {
			if portNum == 0 {
				return "127.0.0.1"
			}
			return net.JoinHostPort("127.0.0.1", rawTarget)
		}
		if host, portStr, err := net.SplitHostPort(rawTarget); err == nil {
			if portStr == "0" {
				host = strings.Trim(host, "[]")
				if host == "" {
					return "127.0.0.1"
				}
				return host
			}
			return rawTarget
		}
		return strings.Trim(rawTarget, "[]")
	}

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

func resolveForwardTarget(normalizedTarget string, dstPort int) string {
	if _, _, err := net.SplitHostPort(normalizedTarget); err == nil {
		return normalizedTarget
	}
	host := strings.Trim(normalizedTarget, "[]")
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(dstPort))
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
		lastState      string
		mu             sync.Mutex
	)

	handleStateTransition := func(state ipn.State) {
		mu.Lock()
		defer mu.Unlock()

		stateStr := state.String()
		if stateStr == lastState {
			return
		}
		lastState = stateStr
		log.Printf("[INFO] Tailscale node state: %s", stateStr)

		switch state {
		case ipn.NeedsLogin:
			log.Println("[INFO] Tailscale node requires authentication, requesting login URL...")
			if err := lc.StartLoginInteractive(ctx); err != nil {
				log.Printf("[WARN] StartLoginInteractive: %v", err)
			}
		case ipn.NeedsMachineAuth:
			log.Println("[WARN] Tailscale node requires machine approval in Tailscale admin console (https://login.tailscale.com/admin/machines).")
		case ipn.Stopped:
			log.Println("[WARN] Tailscale node is in Stopped state, requesting start...")
			_ = lc.Start(ctx, ipn.Options{})
		}
	}

	handleAuthURL := func(urlStr string) {
		mu.Lock()
		defer mu.Unlock()
		if urlStr != "" && urlStr != lastPrintedURL {
			lastPrintedURL = urlStr
			printAuthBanner(lastPrintedURL)
		}
	}

	// Immediate check of initial state
	if st, err := lc.StatusWithoutPeers(ctx); err == nil && st != nil {
		if st.BackendState != "" {
			var ipnSt ipn.State
			for sVal := ipn.NoState; sVal <= ipn.Running; sVal++ {
				if sVal.String() == st.BackendState {
					ipnSt = sVal
					break
				}
			}
			handleStateTransition(ipnSt)
		}
		if st.AuthURL != "" {
			handleAuthURL(st.AuthURL)
		}
		if st.BackendState == ipn.Running.String() && len(st.TailscaleIPs) > 0 {
			if fullStatus, err := lc.Status(ctx); err == nil && fullStatus != nil && len(fullStatus.TailscaleIPs) > 0 {
				return fullStatus, nil
			}
		}
	}

	pollDone := make(chan struct{})
	defer close(pollDone)
	statusReady := make(chan *ipnstate.Status, 1)

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
				if err != nil || st == nil {
					continue
				}

				if st.BackendState != "" {
					var ipnSt ipn.State
					for sVal := ipn.NoState; sVal <= ipn.Running; sVal++ {
						if sVal.String() == st.BackendState {
							ipnSt = sVal
							break
						}
					}
					handleStateTransition(ipnSt)
				}
				if st.AuthURL != "" {
					handleAuthURL(st.AuthURL)
				}

				if st.BackendState == ipn.Running.String() && len(st.TailscaleIPs) > 0 {
					fullStatus, err := lc.Status(ctx)
					if err == nil && fullStatus != nil && len(fullStatus.TailscaleIPs) > 0 {
						select {
						case statusReady <- fullStatus:
						default:
						}
						return
					}
				}
			}
		}
	}()

	for {
		select {
		case status := <-statusReady:
			return status, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		n, err := watcher.Next()
		if err != nil {
			return nil, err
		}

		if n.ErrMessage != nil {
			return nil, fmt.Errorf("backend: %s", *n.ErrMessage)
		}

		if n.State != nil {
			handleStateTransition(*n.State)
		}

		if n.BrowseToURL != nil {
			handleAuthURL(*n.BrowseToURL)
		}

		select {
		case status := <-statusReady:
			return status, nil
		default:
		}

		status, err := lc.Status(ctx)
		if err == nil && status != nil && status.BackendState == ipn.Running.String() && len(status.TailscaleIPs) > 0 {
			return status, nil
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
