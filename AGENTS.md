# Agent Workspace Rules & Guidelines

- **Temporary Directory**: All temporary files, intermediate builds, scripts, and caches must be placed strictly within `./.tmp` (`$PWD/.tmp`). Never read or write outside the workspace (such as `/tmp`, `$TMPDIR`, or `~/go`).
- **Go Environment**: Always execute Go commands with local workspace paths:
  `TMPDIR=$PWD/.tmp GOTMPDIR=$PWD/.tmp GOCACHE=$PWD/.tmp/go-build GOPATH=$PWD/.tmp/gopath GOTOOLCHAIN=local`
- **Dependencies**: Use vendored dependencies (`-mod=vendor`) located in `./vendor` to ensure all operations remain 100% self-contained within the workspace without external network or path access.
