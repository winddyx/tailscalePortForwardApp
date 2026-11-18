package main

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

var bannerMu sync.Mutex

func runUserNode(ctx context.Context, hostname string, user ResolvedUser, verbose bool) error {
	if err := os.MkdirAll(user.StateDir, 0700); err != nil {
		return fmt.Errorf("create state dir %s: %w", user.StateDir, err)
	}

	log.Printf("[user:%s] State directory: %s", user.Name, user.StateDir)
	log.Printf("[user:%s] Starting Tailscale node '%s'...", user.Name, hostname)

	tsServer := &tsnet.Server{
		Dir:        user.StateDir,
		Hostname:   hostname,
		AuthKey:    user.AuthKey,
		ControlURL: user.ControlURL,
		UserLogf: func(format string, args ...any) {
			msg := strings.TrimSpace(fmt.Sprintf(format, args...))
			if msg != "" {
				log.Printf("[user:%s] [TAILSCALE] %s", user.Name, msg)
			}
		},
		Logf: func(format string, args ...any) {
			if !verbose {
				return
			}
			msg := strings.TrimSpace(fmt.Sprintf(format, args...))
			if msg != "" {
				log.Printf("[user:%s] [TAILSCALE DEBUG] %s", user.Name, msg)
			}
		},
	}
	defer tsServer.Close()

	// Ensure tsServer closes immediately when context is cancelled.
	stopDone := make(chan struct{})
	defer close(stopDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = tsServer.Close()
		case <-stopDone:
		}
	}()

	status, err := waitAndAuthenticate(ctx, user.Name, tsServer)
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("[user:%s] Tailscale node stopped", user.Name)
			return nil
		}
		return fmt.Errorf("Tailscale connection failed: %w", err)
	}

	var ip4, ip6 netip.Addr
	for _, ip := range status.TailscaleIPs {
		if ip.Is4() && !ip4.IsValid() {
			ip4 = ip
		} else if ip.Is6() && !ip6.IsValid() {
			ip6 = ip
		}
	}

	nodeDNSName := hostname
	if status.Self != nil && status.Self.DNSName != "" {
		nodeDNSName = status.Self.DNSName
	}

	ipParts := make([]string, 0, 2)
	if ip4.IsValid() {
		ipParts = append(ipParts, "ipv4="+ip4.String())
	}
	if ip6.IsValid() {
		ipParts = append(ipParts, "ipv6="+ip6.String())
	}
	log.Printf("[user:%s] Online: node=%s %s", user.Name, nodeDNSName, strings.Join(ipParts, " "))

	cleanup, err := startForwarding(ctx, user.Name, tsServer, user.Forwards)
	if err != nil {
		return err
	}
	defer cleanup()

	<-ctx.Done()
	log.Printf("[user:%s] Shutting down node...", user.Name)
	return nil
}

func waitAndAuthenticate(ctx context.Context, userName string, s *tsnet.Server) (*ipnstate.Status, error) {
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
		log.Printf("[user:%s] Tailscale state: %s", userName, stateStr)

		switch state {
		case ipn.NeedsLogin:
			log.Printf("[user:%s] Node requires authentication, requesting login URL...", userName)
			if err := lc.StartLoginInteractive(ctx); err != nil {
				log.Printf("[user:%s] [WARN] StartLoginInteractive: %v", userName, err)
			}
		case ipn.NeedsMachineAuth:
			log.Printf("[user:%s] [WARN] Node requires machine approval in Tailscale admin console", userName)
		case ipn.Stopped:
			log.Printf("[user:%s] [WARN] Node is Stopped, requesting start...", userName)
			_ = lc.Start(ctx, ipn.Options{})
		}
	}

	handleAuthURL := func(urlStr string) {
		mu.Lock()
		defer mu.Unlock()
		if urlStr != "" && urlStr != lastPrintedURL {
			lastPrintedURL = urlStr
			printAuthBanner(userName, lastPrintedURL)
		}
	}

	parseState := func(backendState string) ipn.State {
		for sVal := ipn.NoState; sVal <= ipn.Running; sVal++ {
			if sVal.String() == backendState {
				return sVal
			}
		}
		return ipn.NoState
	}

	if st, err := lc.StatusWithoutPeers(ctx); err == nil && st != nil {
		if st.BackendState != "" {
			handleStateTransition(parseState(st.BackendState))
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
					handleStateTransition(parseState(st.BackendState))
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

func printAuthBanner(userName, authURL string) {
	bannerMu.Lock()
	defer bannerMu.Unlock()
	fmt.Println()
	fmt.Println("================================================================================")
	fmt.Printf("[user:%s] To authenticate this device, please visit:\n", userName)
	fmt.Println(authURL)
	fmt.Println("================================================================================")
	fmt.Println()
}
