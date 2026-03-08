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
- **VFS** — Virtual filesystem modes (`sync` and `mount`) for remote data access

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
  - action: "tunnel.devtunnel_create"
    name: "Create devtunnel"
    params:
      tunnel_name: "my-tunnel"
      expiration: "1d"
      auth_token: "{{.TunnelAuthToken}}"
      server_port: "{{.ServerPort}}"
      ssh_port: "{{.SshPort}}"
    outputs:
      tunnel_id: "tunnel_id"
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
| `--vfs-mode` | | VFS mode: `sync` or `mount` (also reads `CS_VFS_MODE` env) |
| `--vfs-session-id` | | Session ID for VFS (also reads `CS_SESSION_ID` env) |

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
| `tunnel.devtunnel_create` | Create a Dev Tunnel and forward ports | `tunnel_id`, `tunnel_name`, `connection_url`, `token`, `ssh_port`, `log_port` |
| `tunnel.devtunnel_forward` | Forward an additional port into a Dev Tunnel | `port` |
| `tunnel.devtunnel_delete` | Delete a Dev Tunnel | |
| `tunnel.devtunnel_connect` | Connect to a Dev Tunnel (client side) | `command_id`, `port_map` |
| `tunnel.frp_proxy_create` | Create an FRP tunnel proxy | `tunnel_name`, `tunnel_type` |
| `mount.setup_overlay` | Set up an overlay mount over a remote workspace via SSHFS | `merged_path`, `cache_path`, `source_path` |
| `shell.exec` | Execute a shell command | `output` |

## Architecture

```
linkspan
├── main.go                    # Entry point, CLI flags, HTTP router, workflow orchestration
├── internal/
│   ├── workflow/              # Workflow engine: YAML parsing, step execution, action registry
│   ├── process/               # Process manager for background CLI processes
│   └── logstream/             # Log broadcaster: tees log output to connected TCP clients
├── subsystems/
│   ├── mount/                 # FUSE overlay filesystem, SFTP-backed copy-up mounts
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

Produces archives for Linux (amd64/arm64) and macOS (amd64/arm64).
