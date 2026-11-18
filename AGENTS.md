# Agent Workspace Rules & Guidelines

## Project Overview

`tspfa` is a pure user-space, no-TUN, multi-user Tailscale TCP port-forwarding proxy driven by `config.json`. Each configured user in `users` runs an independent `tsnet.Server` instance in its own tailnet, shares the configured `hostname`, obtains its own Tailscale IP, and forwards configured ports to the target (default `127.0.0.1:<port>`).

- Branch `v1`: legacy single-user CLI implementation.
- Branch `main`: current multi-user, config-file-only implementation.

## Repository Layout

- `src/main.go`: portable directory resolution, `-config` flag, multi-user goroutine orchestration, and signal handling.
- `src/config.go`: `config.json` parsing, validation, and per-user `state/<user.name>` path resolution.
- `src/node.go`: single-user `tsnet.Server` startup, authentication, and Tailscale IP discovery.
- `src/forwarder.go`: port listener and `pipeBridge` TCP forwarding.
- `src/main_test.go`: unit tests.
- `config.json`, `tspfa.service`, & `compose.yml`: default config, systemd unit, and Docker Compose templates packaged into `dist/`.
- `Makefile`: build, test, and `dist` packaging targets.

## Workspace & Go Environment

- **Temporary Directory**: All temporary files, intermediate builds, scripts, and caches must be placed strictly within `./.tmp` (`$PWD/.tmp`). Never read or write outside the workspace (such as `/tmp`, `$TMPDIR`, or `~/go`).
- **Go Environment**: Always execute Go commands with local workspace paths:
  `TMPDIR=$PWD/.tmp GOTMPDIR=$PWD/.tmp GOCACHE=$PWD/.tmp/go-build GOPATH=$PWD/.tmp/gopath GOTOOLCHAIN=local`
- **Dependencies**: Use vendored dependencies (`-mod=vendor`) located in `./vendor` to ensure all operations remain 100% self-contained within the workspace without external network or path access.

## Build & Test Commands

- Run unit tests: `make test`
- Build local binary: `make build`
- Build release artifacts: `make dist` (cleans `dist/` first, compiles `darwin-arm64`, `linux-amd64`, `linux-arm64`, `windows-amd64`, copies `config.json`, `tspfa.service`, and `compose.yml`, and generates `SHA256SUMS`).

## Design Invariants

- Keep `tsnet.Server` minimal: no TUN, no routes, no DNS takeover, no Serve/Funnel/SSH.
- Treat `config.json` as read-only at runtime; never overwrite it from the binary.
- Reject empty, omitted, or `0` `ports` in config validation; require explicit port(s) in `1-65535`.
- Keep per-user state isolated under `<state_dir>/<user.name>/` with path-safe and case-insensitive unique `user.name` validation.
- Do not set process-global `TSNET_FORCE_LOGIN` env vars, as multiple users with mixed auth-key and interactive login run concurrently in one process.
