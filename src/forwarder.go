package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"tailscale.com/tsnet"
)

func startForwarding(ctx context.Context, userName string, tsServer *tsnet.Server, rules []ForwardRule) (func(), error) {
	listeners := make([]net.Listener, 0, len(rules))
	for _, rule := range rules {
		ln, err := tsServer.Listen("tcp", fmt.Sprintf(":%d", rule.Port))
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			return nil, fmt.Errorf("listen on Tailscale port %d: %w", rule.Port, err)
		}
		listeners = append(listeners, ln)
		log.Printf("[user:%s] Forwarding: [Tailscale]:%d -> %s", userName, rule.Port, rule.Target)
	}

	var wg sync.WaitGroup
	for i, ln := range listeners {
		rule := rules[i]
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
						log.Printf("[user:%s] [WARN] Accept error on port %d: %v", userName, listenPort, err)
						continue
					}
				}
				go handleClient(userName, clientConn, dst)
			}
		}(ln, rule.Port, rule.Target)
	}

	cleanup := func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
		wg.Wait()
	}

	return cleanup, nil
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

func handleClient(userName string, clientConn net.Conn, targetAddr string) {
	defer clientConn.Close()

	remoteAddr := "unknown"
	if ra := clientConn.RemoteAddr(); ra != nil {
		remoteAddr = ra.String()
	}
	log.Printf("[user:%s] [CONN] Accepted %s -> %s", userName, remoteAddr, targetAddr)

	targetConn, err := net.DialTimeout("tcp", targetAddr, 5*time.Second)
	if err != nil {
		log.Printf("[user:%s] [ERROR] Failed to dial target %s for client %s: %v", userName, targetAddr, remoteAddr, err)
		return
	}
	defer targetConn.Close()

	pipeBridge(clientConn, targetConn)
	log.Printf("[user:%s] [CONN] Closed %s", userName, remoteAddr)
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
