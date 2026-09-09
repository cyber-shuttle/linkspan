# Linkspan

[![CI](https://github.com/cyber-shuttle/linkspan/actions/workflows/ci.yml/badge.svg)](https://github.com/cyber-shuttle/linkspan/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/cyber-shuttle/linkspan)](https://github.com/cyber-shuttle/linkspan/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/cyber-shuttle/linkspan)](go.mod)
[![License](https://img.shields.io/github/license/cyber-shuttle/linkspan?color=blue)](LICENSE)

Reach a running HPC job from outside the cluster. Linkspan runs as the main process of a batch job, hosts a
[Microsoft Dev Tunnel](https://learn.microsoft.com/en-us/azure/developer/dev-tunnels/overview) that a client
such as [cs-bridge](https://github.com/cyber-shuttle/CS-Bridge) created, sets the job up from a YAML
workflow, and serves an HTTP API for job metrics and on-demand SSH servers.

Compute nodes sit behind a login node and a firewall, so nothing outside the cluster can open a connection
to a job. The tunnel is established outbound from inside the job, its access is the client's to control,
and each SSH server behind it accepts one public key.

## Features

- **Tunnel hosting**: hosts a Dev Tunnel the client created before submitting the job; the cluster opens
  no inbound port.
- **SSH servers**: job-local, bound to loopback, one per key. VS Code Remote-SSH attaches over them; they
  run commands and serve SFTP but refuse PTY allocation.
- **Job metrics**: cgroup v2 memory and CPU, and per-GPU `nvidia-smi`, for the whole job.
- **Workflows**: an ordered list of commands in YAML, run at startup.
- **Unix socket**: an optional second listener, reachable from another step of the same job with
  [Slurm](https://slurm.schedmd.com/)'s `srun --jobid --overlap`.
- **Single binary**: static, runs as the submitting user. It executes binaries it does not ship: the
  `devtunnel` CLI fetched on first use, `nvidia-smi`, a shell for SSH sessions, and whatever a workflow
  step names.

## Requirements

- Linux with cgroup v2 in Slurm's layout; the macOS archives run but report no metrics.
- `nvidia-smi` on `PATH` for GPU metrics; without it the field is omitted.
- Outbound HTTPS for `--tunnel-enable`: `tunnelsassetsprod.blob.core.windows.net` for the `devtunnel` CLI,
  then the Dev Tunnels service under `devtunnels.ms`.
- A writable home directory; the CLI is installed to `~/.linkspan/bin/`.

## Installation

```bash
curl -fsSL https://github.com/cyber-shuttle/linkspan/releases/latest/download/linkspan_Linux_x86_64.tar.gz |
  tar -xz linkspan
```

Archives are published for Linux and macOS on `x86_64` and `arm64`; versions are listed in
[CHANGELOG.md](CHANGELOG.md). To build from source, see [CONTRIBUTING.md](CONTRIBUTING.md#development-setup).

## Quick Start

```bash
./linkspan --port 8080
```

That serves the HTTP API on loopback and nothing else. In another shell:

```bash
curl http://127.0.0.1:8080/api/v1/health
curl -X POST http://127.0.0.1:8080/api/v1/vscode/sessions \
  -H 'Content-Type: application/json' \
  -d "{\"authorized_key\": \"$(cat ~/.ssh/id_ed25519.pub)\"}"
```

## Usage

### Hosting a tunnel

The client creates the tunnel and mints the host-scoped token before submitting the job.

```bash
linkspan --port "$PORT" \
  --tunnel-enable \
  --tunnel-id "$CS_TUNNEL_ID" \
  --tunnel-cluster "$CS_TUNNEL_CLUSTER" \
  --tunnel-host-token "$CS_TUNNEL_HOST_TOKEN" \
  --workflow /path/to/workflow.yaml
```

Hosting runs Microsoft's `devtunnel` CLI, the relay, downloaded to `~/.linkspan/bin/` on first use; the
tunnel's traffic transits the Dev Tunnels service. Linkspan exits non-zero if the relay fails to come up or
exits; it is not restarted.

### Workflows

Steps run in order at startup, alongside the HTTP server; `shell.exec` is the only action. An invalid
document is refused before anything binds, and a failing step exits Linkspan with status 1. Commands are
split on whitespace and run without a shell, so there is no glob, variable expansion, pipe or redirection;
use absolute paths.

```yaml
name: cs-runtime
steps:
  - action: shell.exec
    name: Create the Python environment
    params:
      command: "/home/me/.local/bin/uv venv --python 3.12 /home/me/.cybershuttle/jupyter-env"
```

### Reaching a job from inside the cluster

`--socket` adds a unix socket listener, so a caller elsewhere in the cluster can reach the job through a
Slurm step without a TCP port. Linkspan unlinks the socket on exit; the directory is yours to create and
remove. Create it mode `0700` without `-p`, so a directory another user pre-created on a shared node
fails the job. A stale socket at the path is replaced; any other file fails the bind.

```bash
mkdir -m 700 "/tmp/linkspan-$SLURM_JOB_ID" && linkspan --port "$PORT" --socket "/tmp/linkspan-$SLURM_JOB_ID/api.sock"
```

A unix socket connects same-node only, even on a shared filesystem, so the caller lands on the job's node
with a Slurm step. Each call is a step with sub-second overhead: suited to a request, not to polling.

```bash
srun --jobid=<id> --overlap --mem=0 curl --unix-socket /tmp/linkspan-<id>/api.sock http://localhost/api/v1/metrics
```

## Configuration

| Flag | Default | Description |
|---|---|---|
| `--port` | `8080` | HTTP port, bound on loopback; `0` picks a free one |
| `--socket` | | Also serve on this unix socket path |
| `--workflow` | | Workflow YAML file |
| `--tunnel-enable` | `false` | Host the tunnel named by `--tunnel-id` |
| `--tunnel-id` | | The client-created tunnel's id; required with `--tunnel-enable` |
| `--tunnel-cluster` | | Cluster id of `--tunnel-id`; required with `--tunnel-enable` |
| `--tunnel-host-token` | | Host-scoped access token; required with `--tunnel-enable` |
| `--version` | | Print the version and exit |

## HTTP API

Requests carry no credential; access control is the transport's: loopback bind, `0600` socket,
client-owned tunnel. The port admits every user on the node and assumes an exclusive allocation; the
socket admits the job's user alone ([SECURITY.md](SECURITY.md)).

| Method | Path | Answers |
|---|---|---|
| GET | `/api/v1/health` | `{"status":"ok"}` |
| GET | `/api/v1/metrics` | `{"memBytes":<n>,"cpuUsageUsec":<n>,"gpus":[{"index":<n>,"utilPct":<n>,"memUsedMiB":<n>,"memTotalMiB":<n>}]}`; a missing source omits its field |
| GET | `/api/v1/vscode/sessions` | `[{"id":"s-<port>","state":"running","addr":"127.0.0.1:<port>"}]`, ordered by id, `[]` when none |
| POST | `/api/v1/vscode/sessions` | `201` with `{"id":"s-<port>","bind_port":<port>}` |

A session is one SSH server, bound on loopback for one public key, running commands through `sh`. The
POST takes `{"authorized_key": "<ssh public key>"}`, a bare key without `authorized_keys` options; the
port is accepting when the response is written. An error answers `{"error": "<message>"}`: `400` when
the body or the key does not parse, `413` over 64KB.

## Used by

- **[cs-bridge](https://github.com/cyber-shuttle/CS-Bridge)** (VS Code extension): submits Linkspan as a
  time-bound job, starts an SSH server for the user's key, and points VS Code Remote-SSH at it.
- **cs-control** (Jupyter runtime service): submits Linkspan with a `--workflow` that builds a Python
  environment and starts a Jupyter server.

## Contributing

Issues and pull requests go through [GitHub](https://github.com/cyber-shuttle/linkspan/issues); see
[CONTRIBUTING.md](CONTRIBUTING.md) and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md). Report vulnerabilities
privately, as [SECURITY.md](SECURITY.md) describes.

## License

Apache-2.0. See [LICENSE](LICENSE).
