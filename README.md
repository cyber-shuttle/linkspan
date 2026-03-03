# Linkspan

Linkspan is a lightweight agent that runs on compute nodes (HPC clusters, cloud VMs, local machines) to orchestrate development environment setup. It manages VS Code remote sessions, dev tunnel connectivity, FUSE-based filesystem mounting, Jupyter kernels, and more — driven by a declarative workflow or REST API.

## Features

- **Workflow Engine** — Declarative YAML-based workflow with variable capture and Go template interpolation between steps
- **VS Code Remote Sessions** — Starts VS Code SSH servers for Remote-SSH connections
- **Dev Tunnels** — Creates and hosts Microsoft Dev Tunnels to expose ports from compute nodes (SDK-based creation + CLI-based relay hosting)
- **FUSE Filesystem** — TCP-based remote filesystem protocol with:
  - **Server**: Serves a local directory over TCP using a custom binary protocol (9 opcodes)
  - **Linux mount**: Direct kernel FUSE mount via go-fuse
  - **macOS mount**: NFSv3 proxy (go-nfs + billy adapter) mounted via `mount_nfs`
- **Jupyter Kernels** — Provision, manage, and connect to Jupyter kernels
- **Remote Filesystem** — REST API for file listing, reading, writing, and deletion

## Quick Start

### Build

```bash
go build -o linkspan .
```

### Run with a Workflow

Linkspan is typically launched with a workflow YAML that sets up the development environment in sequence:

```bash
linkspan --port 0 --tunnel-auth-token "$TOKEN" --workflow - <<'EOF'
name: "dev-setup"

steps:
  - action: "vscode.create_session"
    name: "Start SSH server"
    outputs:
      bind_port: "ssh_port"

  - action: "fuse.start_server"
    name: "Start FUSE server"
    outputs:
      fuse_port: "fuse_server_port"

  - action: "tunnel.devtunnel_create"
    name: "Create devtunnel"
    params:
      tunnel_name: "my-tunnel"
      expiration: "1d"
      auth_token: "{{.TunnelAuthToken}}"
      ports:
        - "{{.ssh_port}}"
        - "{{.fuse_server_port}}"
    outputs:
      tunnel_id: "tunnel_id"

  - action: "tunnel.devtunnel_host"
    name: "Host devtunnel"
    params:
      tunnel_name: "my-tunnel"
      auth_token: "{{.TunnelAuthToken}}"
    outputs:
      connection_url: "tunnel_url"
      token: "tunnel_token"
EOF
```

Steps execute sequentially. Each step's outputs can be referenced in later steps via `{{.variable_name}}`. Initial variables (`TunnelAuthToken`, `ServerPort`, etc.) are injected from CLI flags.

### Run as HTTP Server Only

```bash
linkspan --port 8080
```

This starts the REST API without running any workflow.

### Mount a Remote Filesystem via NFS (macOS)

Connect to a remote FUSE TCP server and mount it locally using NFS:

```bash
linkspan --mount-remote --session-id my-session --server-addr 127.0.0.1:40709
```

This creates a mount at `~/sessions/my-session/` backed by the remote filesystem. The process blocks until interrupted (SIGINT/SIGTERM), at which point it unmounts cleanly.

## CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `8080` | HTTP server port (`0` = random) |
| `--host` | `0.0.0.0` | HTTP server bind address |
| `--workflow` | | Workflow YAML file path (`-` for stdin) |
| `--tunnel-auth-token` | | Microsoft Entra ID bearer token for Dev Tunnels |
| `--tunnel-enable` | `false` | Enable standalone tunnel startup (outside workflow) |
| `--tunnel-retries` | `3` | Retry count for tunnel startup |
| `--tunnel-retry-delay` | `2s` | Delay between tunnel retries |
| `--tunnel-attempt-timeout` | `10s` | Timeout per tunnel attempt |
| `--mount-remote` | `false` | Mount a remote FUSE server locally via NFS |
| `--session-id` | | Session ID for mount point name (with `--mount-remote`) |
| `--server-addr` | | FUSE TCP server address `host:port` (with `--mount-remote`) |

## REST API

All endpoints are under `/api/v1/`.

### Jupyter Kernels
| Method | Path | Description |
|--------|------|-------------|
| GET | `/jupyter/kernels` | List running kernels |
| POST | `/jupyter/kernels` | Provision a new kernel |
| DELETE | `/jupyter/kernels/{id}` | Delete a kernel |
| GET | `/jupyter/kernels/{id}/connection` | Get kernel connection info |
| GET | `/jupyter/kernels/{id}/status` | Get kernel status |
| POST | `/jupyter/kernels/shutdown` | Shutdown a kernel |

### VS Code Sessions
| Method | Path | Description |
|--------|------|-------------|
| GET | `/vscode/sessions` | List active sessions |
| POST | `/vscode/sessions` | Create a new session |
| DELETE | `/vscode/sessions/{id}` | Terminate a session |
| GET | `/vscode/sessions/{id}/status` | Get session status |

### Remote Filesystem
| Method | Path | Description |
|--------|------|-------------|
| GET | `/fs/list` | List files in a directory |
| GET | `/fs/read` | Read a file |
| POST | `/fs/write` | Write a file |
| DELETE | `/fs/delete` | Delete a file |

### FUSE Mount
| Method | Path | Description |
|--------|------|-------------|
| POST | `/fuse/start-server` | Start a FUSE TCP server |
| POST | `/fuse/mount-remote` | Mount a remote FUSE server via NFS |
| GET | `/fuse/status` | Get FUSE server/mount status |

### Tunnels
| Method | Path | Description |
|--------|------|-------------|
| GET | `/tunnels/devtunnels` | List active Dev Tunnels |
| POST | `/tunnels/devtunnels` | Create a Dev Tunnel |
| DELETE | `/tunnels/devtunnels/{id}` | Close a Dev Tunnel |
| GET | `/tunnels/frp` | List FRP tunnels |
| POST | `/tunnels/frp` | Create an FRP tunnel proxy |
| DELETE | `/tunnels/frp/{id}` | Terminate an FRP tunnel |

## Workflow Actions

| Action | Description | Outputs |
|--------|-------------|---------|
| `vscode.create_session` | Start a VS Code SSH server | `session_id`, `bind_port` |
| `fuse.start_server` | Start FUSE TCP server on a random port | `fuse_port` |
| `fuse.mount_remote` | Mount a remote FUSE server locally | `mount_path`, `nfs_port` |
| `tunnel.devtunnel_create` | Create a Dev Tunnel with specified ports | `tunnel_id`, `tunnel_name` |
| `tunnel.devtunnel_host` | Host a Dev Tunnel (start relay) | `command_id`, `connection_url`, `token` |
| `tunnel.devtunnel_delete` | Delete a Dev Tunnel | |
| `tunnel.frp_proxy_create` | Create an FRP tunnel proxy | `tunnel_name`, `tunnel_type` |
| `shell.exec` | Execute a shell command | `output` |

## Architecture

```
linkspan
├── main.go                    # Entry point, CLI flags, HTTP router, workflow orchestration
├── internal/
│   ├── workflow/              # Workflow engine: YAML parsing, step execution, action registry
│   └── process/               # Process manager for background CLI processes
├── subsystems/
│   ├── fuse/                  # FUSE-over-TCP protocol, NFS proxy, mount management
│   ├── vscode/                # VS Code SSH server lifecycle
│   ├── tunnel/                # Dev Tunnels SDK + CLI, FRP tunnel management
│   ├── jupyter/               # Jupyter kernel provisioning
│   ├── vfs/                   # Remote filesystem REST handlers
│   └── env/                   # Environment detection
└── utils/                     # Shared utilities (port finding, JSON responses, etc.)
```

## Building Releases

```bash
goreleaser release --snapshot --clean    # Local snapshot build
goreleaser release --clean               # Tagged release (requires GITHUB_TOKEN)
```

Produces archives for Linux (amd64/arm64), macOS (amd64/arm64), and Windows (amd64).
