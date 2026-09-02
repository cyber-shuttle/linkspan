# Linkspan

[![CI](https://github.com/cyber-shuttle/linkspan/actions/workflows/ci.yml/badge.svg)](https://github.com/cyber-shuttle/linkspan/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/cyber-shuttle/linkspan)](https://github.com/cyber-shuttle/linkspan/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/cyber-shuttle/linkspan)](go.mod)
[![License](https://img.shields.io/github/license/cyber-shuttle/linkspan?color=blue)](LICENSE)

Reach a running HPC (high-performance computing) job securely from outside the cluster. Linkspan runs as the
main process of a batch job, hosts a
[Microsoft Dev Tunnel](https://learn.microsoft.com/en-us/azure/developer/dev-tunnels/overview) that a client
such as [cs-bridge](https://github.com/cyber-shuttle/CS-Bridge) created, sets the job up from a YAML workflow,
and serves an HTTP API for job metrics and on-demand SSH servers.

Compute nodes sit behind a login node and a firewall, so nothing outside the cluster can open a connection to
a running job. The tunnel Linkspan hosts is established outbound, from inside the job. Access to it is the
client's to control, and the SSH servers behind it accept one public key each.

## Features

- **Tunnel hosting** — hosts a Microsoft Dev Tunnel the client created before submitting the job. The
  cluster opens no inbound port.
- **SSH servers** — starts a job-local SSH server bound to loopback. This is what VS Code Remote-SSH attaches
  over; it runs commands and serves SFTP, but refuses PTY allocation, so there is no terminal session.
- **Allocation metrics** — cgroup-v2 memory and CPU, and per-GPU `nvidia-smi`, covering the whole job rather
  than one step.
- **Workflows** — an ordered list of commands, given in YAML and run at startup.
- **Unix socket** — an optional second listener, reachable from another step of the same allocation with the
  [Slurm](https://slurm.schedmd.com/) workload manager's `srun --jobid --overlap`.
- **Single binary** — no shared libraries, and runs as the submitting user. It execs binaries it does not
  ship: Microsoft's `devtunnel` CLI, fetched on first use, `nvidia-smi` for GPU metrics, a shell for SSH
  sessions, and whatever a workflow step names.

## Requirements

- Linux with cgroup v2, laid out as Slurm lays it out: the metrics read the job's own cgroup and strip the
  `/step_*` leaf. The macOS archives run, but report neither.
- `nvidia-smi` on `PATH` for GPU metrics; without it that field is omitted.
- Outbound HTTPS for `--tunnel-enable`: `tunnelsassetsprod.blob.core.windows.net` to fetch the `devtunnel`
  CLI, then the Microsoft Dev Tunnels service it connects to, which serves the tunnel under `devtunnels.ms`.
- A writable home directory: the `devtunnel` CLI is installed to `~/.linkspan/bin/`.

## Installation

```bash
curl -fsSL https://github.com/cyber-shuttle/linkspan/releases/latest/download/linkspan_Linux_x86_64.tar.gz |
  tar -xz linkspan
```

Archives are published for Linux and macOS on `x86_64` and `arm64`; every released version is listed in
[CHANGELOG.md](CHANGELOG.md). To build from source, see
[CONTRIBUTING.md](CONTRIBUTING.md#development-setup).

## Quick Start

In one shell:

```bash
./linkspan --port 8080
```

That serves the HTTP API on loopback and nothing else — no tunnel, no workflow. In another:

```bash
curl http://127.0.0.1:8080/api/v1/health
curl -X POST http://127.0.0.1:8080/api/v1/vscode/sessions \
  -H 'Content-Type: application/json' \
  -d "{\"authorized_key\": \"$(cat ~/.ssh/id_ed25519.pub)\"}"
```

## Usage

### Hosting a tunnel

The client creates the tunnel and mints the host-scoped token before submitting the job; Linkspan only hosts it.

```bash
linkspan --port "$PORT" \
  --tunnel-enable \
  --tunnel-id "$CS_TUNNEL_ID" \
  --tunnel-cluster "$CS_TUNNEL_CLUSTER" \
  --tunnel-host-token "$CS_TUNNEL_HOST_TOKEN" \
  --workflow /path/to/workflow.yaml
```

Hosting runs Microsoft's `devtunnel` CLI: Linkspan downloads it from
`https://tunnelsassetsprod.blob.core.windows.net` to `~/.linkspan/bin/` on first use and executes it, and the
traffic the tunnel carries transits the Microsoft Dev Tunnels service. Linkspan exits non-zero if the relay
fails to come up after three attempts.

### Workflows

An ordered list of steps run at startup, alongside the HTTP server. `shell.exec` is the only action. Every
step's action is validated before the first one runs, and a failing step exits Linkspan with status 1.

Commands are split on whitespace and executed without a shell — no globs, no variable expansion, no pipes or
redirection — so use absolute paths.

```yaml
name: cs-runtime
steps:
  - action: shell.exec
    name: Create the Python environment
    params:
      command: "/home/me/.local/bin/uv venv --python 3.12 /home/me/.cybershuttle/jupyter-env"
```

### Reaching a job from inside the cluster

With `--socket`, reachable without the tunnel or a TCP connection. In the job:

```bash
linkspan --port "$PORT" --socket /tmp/linkspan.sock
```

From another step of the same allocation:

```bash
srun --jobid=<id> --overlap curl --unix-socket /tmp/linkspan.sock http://localhost/api/v1/metrics
```

## Configuration

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `8080` | HTTP server port, bound on loopback (`0` picks a free one) |
| `--socket` | | Also serve on this unix socket path, for in-cluster access via `srun --jobid` |
| `--workflow` | | Workflow YAML file path |
| `--tunnel-enable` | `false` | Host the tunnel named by `--tunnel-id` on startup |
| `--tunnel-id` | | Id of the client-created tunnel to host |
| `--tunnel-cluster` | | Cluster id of `--tunnel-id` |
| `--tunnel-host-token` | | Host-scoped access token for `--tunnel-id` |
| `--version` | | Print the version and exit |

The three tunnel values are required whenever `--tunnel-enable` is set.

## HTTP API

Requests carry no credential; access control is at the transport: loopback bind, `0600` socket, client-owned
tunnel.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/health` | Liveness; answers `{"status":"ok"}` |
| GET | `/api/v1/metrics` | cgroup-v2 memory/CPU and per-GPU `nvidia-smi` for the allocation; a missing source omits its field rather than failing the request |
| GET | `/api/v1/vscode/sessions` | List the SSH servers and their supervisor state |
| POST | `/api/v1/vscode/sessions` | Start an SSH server authorized for one public key |

`POST /api/v1/vscode/sessions` takes `{"authorized_key": "<ssh public key>"}` and answers
`{"id": "s-<port>", "bind_port": <port>}`. The port is already accepting when the response is written, and is
bound on loopback like the API.

## Used by

CyberShuttle runs interactive workloads on HPC compute nodes and brings them back to an editor or browser on
the user's own machine. Its two clients run Linkspan inside the job:

- **[cs-bridge](https://github.com/cyber-shuttle/CS-Bridge)** — VS Code extension. Submits Linkspan as a
  time-bound job, has it start an SSH server for the user's key, and points VS Code Remote-SSH at it.
- **cs-control** — the Jupyter runtime service. Submits Linkspan with a `--workflow` that builds a Python
  environment and starts a Jupyter server.

## Contributing

Questions, issues and pull requests go through
[GitHub Issues](https://github.com/cyber-shuttle/linkspan/issues). See [CONTRIBUTING.md](CONTRIBUTING.md) for
the source layout, development setup and release process, and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for the
participation standard.

## Security

Report vulnerabilities privately, as described in [SECURITY.md](SECURITY.md), rather than in a public issue.

## License

Apache-2.0 — see [LICENSE](LICENSE).
