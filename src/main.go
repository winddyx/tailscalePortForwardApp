package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "tailscale.com/feature/useproxy"
	"tailscale.com/net/netns"
)

const (
	retryInitialBackoff = 5 * time.Second
	retryMaxBackoff     = 60 * time.Second
	retryResetWindow    = 1 * time.Minute
)

func main() {
	// For user-space tsnet without system TUN, disable binding sockets to a specific
	// physical interface on macOS so that traffic correctly traverses transparent proxies
	// or VPNs without black-holing.
	netns.SetDisableBindConnToInterface(func(string, ...any) {}, true)

	var configFlag string
	flag.StringVar(&configFlag, "config", "", "Path to config.json (default: <executable_dir>/config.json)")
	flag.StringVar(&configFlag, "c", "", "Path to config.json (default: <executable_dir>/config.json)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [-config path/to/config.json]\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	portableDir, err := resolvePortableDir()
	if err != nil {
		log.Fatalf("[FATAL] failed to resolve portable directory: %v", err)
	}

	configPath := strings.TrimSpace(configFlag)
	if configPath == "" {
		configPath = filepath.Join(portableDir, "config.json")
	} else if !filepath.IsAbs(configPath) {
		configPath, err = filepath.Abs(configPath)
		if err != nil {
			log.Fatalf("[FATAL] failed to resolve config path: %v", err)
		}
	}

	cfg, err := LoadConfig(configPath, portableDir)
	if err != nil {
		log.Fatalf("[FATAL] invalid configuration: %v", err)
	}

	if err := os.MkdirAll(cfg.StateRoot, 0700); err != nil {
		log.Fatalf("[FATAL] failed to create state root directory %s: %v", cfg.StateRoot, err)
	}

	log.Printf("[INFO] Portable directory: %s", portableDir)
	log.Printf("[INFO] Config file: %s", configPath)
	log.Printf("[INFO] State root: %s", cfg.StateRoot)
	log.Printf("[INFO] Hostname: %s", cfg.Hostname)
	log.Printf("[INFO] Configured users: %d", len(cfg.Users))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			log.Println("[INFO] Shutdown signal received, stopping all Tailscale nodes...")
			cancel()
		case <-ctx.Done():
		}
	}()

	var wg sync.WaitGroup

	for _, user := range cfg.Users {
		wg.Add(1)
		go func(u ResolvedUser) {
			defer wg.Done()
			runUserNodeWithRetry(ctx, u.Name, retryInitialBackoff, retryMaxBackoff, retryResetWindow, func(runCtx context.Context) error {
				return runUserNode(runCtx, cfg.Hostname, u, cfg.Verbose)
			})
		}(user)
	}

	wg.Wait()
	log.Println("[INFO] All Tailscale nodes stopped")
}

func runUserNodeWithRetry(
	ctx context.Context,
	userName string,
	initialBackoff time.Duration,
	maxBackoff time.Duration,
	resetWindow time.Duration,
	run func(context.Context) error,
) {
	if initialBackoff <= 0 {
		initialBackoff = time.Second
	}
	if maxBackoff < initialBackoff {
		maxBackoff = initialBackoff
	}

	backoff := initialBackoff
	for {
		start := time.Now()
		err := run(ctx)
		if ctx.Err() != nil || err == nil {
			return
		}

		if resetWindow > 0 && time.Since(start) >= resetWindow {
			backoff = initialBackoff
		}

		log.Printf("[user:%s] [ERROR] %v; retrying in %s...", userName, err, backoff)

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func resolvePortableDir() (string, error) {
	exePath, err := os.Executable()
	if err == nil {
		if realPath, err := filepath.EvalSymlinks(exePath); err == nil {
			exePath = realPath
		}
		exeDir := filepath.Dir(exePath)
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
