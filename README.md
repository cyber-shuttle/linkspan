# Linkspan

Linkspan is a lightweight agent that runs inside a compute-node allocation. It hosts the dev tunnel its
client created, runs the workflow the client handed it, and serves an SSH server for VS Code Remote-SSH.

It has two consumers, and its surface is exactly what they use:

- **cs-bridge** launches it for VS Code and calls its HTTP API.
- **cs-control** launches it for Jupyter and drives it entirely through `--workflow`.

## Quick Start

```bash
go build -o linkspan .
./linkspan --port 8080
```

## Running in an allocation

The client creates the dev tunnel, registers its ports and mints a host-scoped token; linkspan hosts the
relay and never creates, forwards or deletes a tunnel of its own.

```bash
linkspan --port "$CS_CONTROL_PORT" --tunnel-enable \
  --tunnel-id "$CS_TUNNEL_ID" --tunnel-cluster "$CS_TUNNEL_CLUSTER" \
  --tunnel-host-token "$CS_TUNNEL_HOST_TOKEN" \
  --workflow /path/to/workflow.yaml
```

## Workflows

A workflow is a list of steps run in order, stopping at the first failure. `shell.exec` is the only action:
its command is split on whitespace and run without a shell, so nothing is expanded.

```yaml
name: cs-runtime
steps:
  - action: shell.exec
    name: Create the Python environment
    params:
      command: "/home/me/.local/bin/uv venv --python 3.12 /home/me/.cybershuttle/jupyter-env"
```

## CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `8080` | HTTP server port (`0` = random) |
| `--socket` | | Also serve on this unix socket path (in-cluster access via `srun --jobid`) |
| `--workflow` | | Workflow YAML file path |
| `--tunnel-enable` | `false` | Host the tunnel named by `--tunnel-id` on startup |
| `--tunnel-id` | | Id of the client-created dev tunnel to host |
| `--tunnel-cluster` | | Cluster id of `--tunnel-id` |
| `--tunnel-host-token` | | Host-scoped access token for `--tunnel-id` |
| `--version` | | Print the version and exit |

`--version` prints a bare `X.Y.Z[.commit]` as its only line of stdout, and `--help` names
`-tunnel-host-token`. Both consumers run `--version` to decide whether to install or replace the binary —
cs-bridge is broken by a second line, cs-control tolerates one — and cs-control greps `--help` for
`-tunnel-host-token` and refuses to submit a job without it. Neither output may change shape.

## REST API

Four endpoints, all under `/api/v1/`.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/health` | Liveness; answers `{"status":"ok"}` |
| GET | `/metrics` | Live cgroup-v2 memory/CPU and per-GPU `nvidia-smi` for the allocation. Each source degrades independently — a missing one omits its field rather than failing the request |
| GET | `/vscode/sessions` | List the SSH servers and their supervisor state |
| POST | `/vscode/sessions` | Start an SSH server authorized for one public key |

`POST /vscode/sessions` takes `{"authorized_key": "<ssh public key>"}` and answers
`{"id": "s-<port>", "bind_port": <port>}`. The port is bound before the response is written, so it is
already accepting. It is loopback only: it reaches clients through the tunnel, never the node's network.

## Architecture

```
linkspan
├── main.go                    # CLI flags, startup, shutdown
├── internal/
│   ├── httpapi/               # every route, handler and listener
│   ├── workflow/              # YAML workflow: load and run shell.exec steps in order
│   └── process/               # Process manager for background CLI processes
└── subsystems/
    ├── metrics/               # cgroup-v2 + nvidia-smi job metrics
    ├── sshd/                  # Supervised SSH server (gliderlabs/ssh) with PTY support
    └── tunnel/                # devtunnel CLI download + relay hosting
```

## Building Releases

```bash
goreleaser release --snapshot --clean    # Local snapshot build
goreleaser release --clean               # Tagged release (requires GITHUB_TOKEN)
```

Both consumers download `linkspan_Linux_${arch}.tar.gz` from `releases/latest` and extract a member named
`linkspan`, so the archive name template and the binary name inside it are a contract.
