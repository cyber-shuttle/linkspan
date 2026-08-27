# Linkspan

Linkspan is a small Go binary that runs as the main process of an HPC batch job. It gives a client outside
the cluster a way to reach that job: it hosts a tunnel the client created, runs a YAML workflow to set the
job up, and serves a small HTTP API for job metrics and on-demand SSH servers.

## Why

Compute nodes sit behind a login node and a firewall, so nothing outside the cluster can open a connection
to a running job. Linkspan dials out instead of listening: it runs inside the job and hosts a tunnel the
client created before submitting, so the client reaches the job through that tunnel rather than through an
inbound port.

It is a CyberShuttle component, used two ways:

- **cs-bridge** (VS Code extension) submits Linkspan as a time-bound job, asks it over the tunnel to start a
  job-local SSH server for the user's public key, and points VS Code Remote-SSH at it.
- **cs-control** (Jupyter runtimes) submits Linkspan with a `--workflow` that builds a Python environment and
  starts a Jupyter server, then reaches that server through the same tunnel.

## Install

Download the release build for the target platform:

```bash
curl -fsSL https://github.com/cyber-shuttle/linkspan/releases/latest/download/linkspan_Linux_x86_64.tar.gz |
  tar -xz linkspan
```

Archives are published for Linux and macOS on `x86_64` and `arm64`. To build from source instead (Go 1.24+):

```bash
go build -o linkspan .
```

## Quick start

```bash
./linkspan --port 8080
curl http://127.0.0.1:8080/api/v1/health
```

That serves the HTTP API on loopback and does nothing else — no tunnel, no workflow.

## Running behind a firewall

Linkspan hosts a tunnel; it never creates one. The client creates the tunnel, registers its ports and mints
a host-scoped token before submitting the job, and Linkspan runs the relay for the tunnel it is given. Keeping
the lifecycle outside the job means the job cannot create, extend or delete a tunnel, and the token it carries
authorizes hosting and nothing else.

```bash
linkspan --port "$PORT" \
  --tunnel-enable \
  --tunnel-id "$CS_TUNNEL_ID" \
  --tunnel-cluster "$CS_TUNNEL_CLUSTER" \
  --tunnel-host-token "$CS_TUNNEL_HOST_TOKEN" \
  --workflow /path/to/workflow.yaml
```

The `devtunnel` CLI is downloaded to `~/.linkspan/bin/` on first use. If the relay fails to come up after
three attempts, Linkspan exits non-zero rather than running on with no way to be reached.

## Workflows

A workflow is an ordered list of steps run at startup, alongside the HTTP server. `shell.exec` is the only
action. Every step is validated before the first one runs, and the run stops at the first failure — which
shuts Linkspan down with exit status 1, since a job whose setup failed has nothing left to offer.

Each command is split on whitespace and executed directly, without a shell: no globs, no variable expansion,
no pipes or redirection. Use absolute paths.

```yaml
name: cs-runtime
steps:
  - action: shell.exec
    name: Create the Python environment
    params:
      command: "/home/me/.local/bin/uv venv --python 3.12 /home/me/.cybershuttle/jupyter-env"
```

A step that daemonizes (`setsid --fork ...`) returns as soon as it has forked, so one step can start a
long-running server and the next can wait for it to answer.

## CLI flags

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

The three tunnel values are required whenever `--tunnel-enable` is set. Go's flag package accepts both `-name`
and `--name`.

## HTTP API

**The API has no authentication and binds to loopback only.** `POST /api/v1/vscode/sessions` starts an SSH
server for whatever public key it is handed, so reaching the API is equivalent to a shell as the job's user.
Callers are expected to arrive through the client-owned tunnel or through `--socket`, which is created
mode `0600`.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/v1/health` | Liveness; answers `{"status":"ok"}` |
| GET | `/api/v1/metrics` | Live cgroup-v2 memory/CPU and per-GPU `nvidia-smi` for the allocation. Each source degrades independently — a missing one omits its field rather than failing the request |
| GET | `/api/v1/vscode/sessions` | List the SSH servers and their supervisor state |
| POST | `/api/v1/vscode/sessions` | Start an SSH server authorized for one public key |

`POST /api/v1/vscode/sessions` takes `{"authorized_key": "<ssh public key>"}` and answers
`{"id": "s-<port>", "bind_port": <port>}`. The port is bound before the response is written, so it is already
accepting. Each SSH server is loopback-only too: it reaches clients through the tunnel, never through the
node's network.

## Reaching a job from inside the cluster

With `--socket`, and no tunnel or TCP port involved:

```bash
srun --jobid=<id> --overlap curl --unix-socket /tmp/linkspan.sock http://localhost/api/v1/metrics
```

## Architecture

```
linkspan
├── main.go                    # CLI flags, startup, shutdown
├── internal/
│   ├── httpapi/               # every route, handler and listener
│   └── workflow/              # YAML workflow: load and run shell.exec steps in order
└── subsystems/
    ├── metrics/               # cgroup-v2 + nvidia-smi job metrics
    ├── sshd/                  # supervised SSH server (gliderlabs/ssh) with PTY support
    └── tunnel/                # devtunnel CLI download + relay hosting
```

## Development

```bash
go build ./...
go test ./...
go vet ./...
```

CI runs all three on every pull request. `make` cross-compiles for Linux and macOS on `amd64`/`arm64`, but
refuses to build unless HEAD is tagged `X.Y.Z` or `X.Y.Z.<commit>`, because the tag is the version it stamps
in; use `go build` for a dev binary. GitHub Actions builds and uploads the release archives when a release is
published on GitHub, and `goreleaser release --snapshot --clean` builds the same archives locally.

## Compatibility

CyberShuttle clients install and drive Linkspan by the following, so they do not change without a coordinated
release:

- `--version` prints a bare `X.Y.Z[.commit]` as the only line on stdout.
- The release archive `linkspan_Linux_<arch>.tar.gz` contains a binary named `linkspan`.
- The four `/api/v1` paths above, their response shapes, and the session id `s-<port>`.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
