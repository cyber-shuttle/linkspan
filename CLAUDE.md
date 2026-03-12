# Linkspan

Go agent that orchestrates dev environment setup on compute nodes. Manages VS Code SSH sessions, Dev Tunnels/FRP connectivity, FUSE overlay mounts, and Jupyter kernels via a declarative YAML workflow engine and REST API.

## Prerequisites

- Go 1.24+ (toolchain go1.24.13)
- Optional: FUSE (Linux) for overlay mounts, goreleaser for release builds

## Commands

```bash
go build -o linkspan .          # Build for current platform
go test ./...                   # Run tests
go vet ./...                    # Static analysis
make                            # Cross-compile all platforms (linux/darwin, amd64/arm64)
make clean                      # Remove bin/
goreleaser release --snapshot --clean  # Local snapshot build
```

## Running

```bash
./linkspan --port 8080                          # HTTP server only
./linkspan --workflow examples/cs-bridge-workflow.yaml --tunnel-auth-token "$TOKEN"
./linkspan --port 0                             # OS-assigned random port
```

## Architecture

```
main.go                         # Entry point, HTTP router (gorilla/mux), server lifecycle
internal/
  workflow/                     # YAML workflow engine: parse, execute, action registry
  process/                      # ManagedProcess tracking, GlobalProcessManager singleton
  logstream/                    # TCP-based real-time log broadcaster
subsystems/
  tunnel/                       # Dev Tunnels + FRP: TunnelProvider interface, managers
  vscode/                       # SSH server (gliderlabs/ssh) with PTY support
  jupyter/                      # Kernel provisioning lifecycle
  mount/                        # FUSE overlay filesystem (go-fuse)
  vfs/                          # VFS sync (mutagen) and mount (NFSv3 on macOS) modes
  env/venv/                     # Python venv detection
utils/                          # JSON helpers, port finding
```

Each subsystem has `api.go` (HTTP handlers) + domain logic files.

## REST API (`/api/v1/`)

- **Tunnels**: CRUD for devtunnels and FRP tunnels, generic provider-agnostic endpoints
- **VS Code**: `/vscode/sessions` — create/list/delete SSH sessions
- **Jupyter**: `/jupyter/kernels` — provision/status/shutdown kernels
- **Metadata**: In-memory key-value store (`/metadata/{key}`)
- **System**: `/health`, `/status` (workflow state + outputs)

## Workflow Engine

YAML workflows execute sequentially. Steps reference actions from a registry with Go template variable interpolation (`{{.VarName}}`).

Built-in actions: `tunnel.devtunnel_create`, `tunnel.frp_proxy_create`, `tunnel.create` (generic), `mount.setup_overlay`, `shell.exec`, and more.

Initial variables injected from CLI: `ServerPort`, `SshPort`, `LogPort`, `TunnelAuthToken`, `SessionID`, etc.

Step failure halts workflow but HTTP server keeps running. Status at `/api/v1/status`.

## Key Patterns

- **GlobalProcessManager** singleton tracks long-running processes (tunnel CLI, etc.)
- **TunnelProvider** interface enables pluggable backends (DevTunnel, FRP)
- SSH server has no auth (assumes secure tunnel/firewall)
- FUSE overlay: lower=SFTP (remote), upper=local cache. Stale mounts cleaned from `/proc/mounts` on startup
- VFS on macOS uses NFSv3 proxy instead of kernel FUSE
- Log broadcaster: TCP listener on random port, all connected clients get same stream

## CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | 8080 | HTTP server port (0=random) |
| `--host` | 0.0.0.0 | HTTP bind address |
| `--workflow` | — | YAML workflow path (`-` for stdin) |
| `--tunnel-api` | devtunnels | Tunnel provider name |
| `--tunnel-auth-token` | — | Microsoft Entra ID bearer token |
| `--tunnel-enable` | false | Auto-start tunnel on startup |
| `--vfs-mode` | — | `sync` or `mount` (also `CS_VFS_MODE` env) |
| `--vfs-session-id` | — | Session ID (also `CS_SESSION_ID` env) |

## Gotchas

- Port 0 binding: actual port extracted after bind, passed as `ServerPort` to workflows
- FUSE stale mount cleanup scans `/proc/mounts` — Linux-specific, no-op on macOS
- Mutagen binary resolution: searches ~/.cybershuttle, homebrew, system paths; auto-downloads if missing
- Workflow cancellation checks context between steps — current step is NOT preempted
- SSH server spawns shell via PTY (creack/pty) — resize handled via SSH window-change channel
