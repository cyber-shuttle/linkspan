# Linkspan

[![CI](https://github.com/cyber-shuttle/linkspan/actions/workflows/ci.yml/badge.svg)](https://github.com/cyber-shuttle/linkspan/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/cyber-shuttle/linkspan)](https://github.com/cyber-shuttle/linkspan/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/cyber-shuttle/linkspan)](go.mod)
[![LICENSE](https://img.shields.io/github/license/cyber-shuttle/linkspan?color=blue)](LICENSE)

Reach a running HPC job securely from outside the cluster. Linkspan runs as the main process of a batch job,
hosts a tunnel the client created, sets the job up from a YAML workflow, and serves an HTTP API for job
metrics and on-demand SSH servers.

Compute nodes sit behind a login node and a firewall, so nothing outside the cluster can open a connection to
a running job. The tunnel Linkspan hosts is established outbound, from inside the job. Access to it is the
client's to control, the traffic it carries is encrypted, and the SSH servers behind it accept one public key
each.

## Features

- **Tunnel hosting** — hosts a tunnel the client created before submitting the job. The cluster opens no
  inbound port.
- **SSH servers** — starts a job-local SSH server bound to loopback and authorized for one public key. This
  is what VS Code Remote-SSH attaches over.
- **Allocation metrics** — cgroup-v2 memory and CPU, and per-GPU `nvidia-smi`, covering the whole job rather
  than one step.
- **Workflows** — an ordered list of commands, given in YAML and run at startup.
- **Unix socket** — an optional second listener, reachable in-cluster with `srun --jobid --overlap`.
- **Static binary** — no runtime dependencies, and runs as the submitting user.

## Installation

```bash
curl -fsSL https://github.com/cyber-shuttle/linkspan/releases/latest/download/linkspan_Linux_x86_64.tar.gz |
  tar -xz linkspan
```

Archives are published for Linux and macOS on `x86_64` and `arm64`. To build from source, see
[CONTRIBUTING.md](CONTRIBUTING.md#development-setup).

## Quick Start

```bash
./linkspan --port 8080
curl http://127.0.0.1:8080/api/v1/health
```

That serves the HTTP API on loopback and nothing else — no tunnel, no workflow.

## Usage

### Hosting a tunnel

Linkspan hosts a tunnel; it never creates one. The client creates it, registers its ports and mints a
host-scoped token before submitting the job, so the token Linkspan carries authorizes hosting and nothing else.

```bash
linkspan --port "$PORT" \
  --tunnel-enable \
  --tunnel-id "$CS_TUNNEL_ID" \
  --tunnel-cluster "$CS_TUNNEL_CLUSTER" \
  --tunnel-host-token "$CS_TUNNEL_HOST_TOKEN" \
  --workflow /path/to/workflow.yaml
```

The `devtunnel` CLI is downloaded to `~/.linkspan/bin/` on first use. Linkspan exits non-zero if the relay
fails to come up after three attempts.

### Workflows

An ordered list of steps run at startup, alongside the HTTP server. `shell.exec` is the only action. Every
step is validated before the first one runs, and a failing step exits Linkspan with status 1.

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

With `--socket`, and no tunnel or TCP port involved:

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

Access control sits at the transport rather than in the API. The HTTP listener binds loopback only, so nothing
off the node can reach it; the optional `--socket` listener is created mode `0600`, so only the job's own user
can connect; and remote callers arrive over the tunnel the client created and controls. Requests are not
separately authenticated, which is why those three boundaries are where the checks are. See
[SECURITY.md](SECURITY.md) for the full model.

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

- **[cs-bridge](https://github.com/cyber-shuttle/CS-Bridge)** — VS Code extension. Submits Linkspan as a
  time-bound job, has it start an SSH server for the user's key, and points VS Code Remote-SSH at it.
- **[cs-control](https://github.com/cyber-shuttle/cs-control)** — Jupyter runtimes. Submits Linkspan with a
  `--workflow` that builds a Python environment and starts a Jupyter server.

## Contributing

Issues and pull requests go through GitHub. See [CONTRIBUTING.md](CONTRIBUTING.md) for the source layout,
development setup, and release process.

## Security

Report vulnerabilities privately, as described in [SECURITY.md](SECURITY.md), rather than in a public issue.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
