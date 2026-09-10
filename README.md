# Linkspan

[![CI](https://github.com/cyber-shuttle/linkspan/actions/workflows/ci.yml/badge.svg)](https://github.com/cyber-shuttle/linkspan/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/cyber-shuttle/linkspan)](https://github.com/cyber-shuttle/linkspan/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/cyber-shuttle/linkspan)](go.mod)
[![License](https://img.shields.io/github/license/cyber-shuttle/linkspan?color=blue)](LICENSE)

**Linkspan turns an HPC batch job into a workspace you can reach from your laptop.** It runs as the main
process of the job, hosts a [Dev Tunnel](https://learn.microsoft.com/en-us/azure/developer/dev-tunnels/overview)
you own, and on request launches VS Code servers, Jupyter servers and web terminals inside the allocation,
runs your workflow at each point of the job's life, and reports the job's metrics.

Compute nodes sit behind a login node and a firewall, so nothing outside the cluster can open a connection
to a job. Linkspan opens the tunnel outbound from inside the job; the cluster opens no inbound port, and
who may connect is decided by the tunnel you created, not by the node.

## Features

- **VS Code Remote-SSH**: an SSH server per public key, bound to loopback inside the job. It runs commands
  and serves SFTP, refuses PTYs and password login, and is what [cs-bridge](https://github.com/cyber-shuttle/CS-Bridge)
  attaches VS Code to.
- **Jupyter servers**: one per root directory, in an environment Linkspan builds with `uv` under
  `~/.cybershuttle` and secures with a token it mints.
- **Web terminals**: a login shell served by `ttyd`, fetched on first use, one per session.
- **Workflows**: a YAML file of steps, each on a trigger: `start`, `ready`, `stop`, or a signal such as
  `SIGUSR1` that Slurm sends before the time limit. A step runs a command or a Linkspan action such as
  `vscode.sessions.start`, or a list of them, so a whole workspace is bootstrapped from the file.
- **Tunnel hosting**: hosts the Dev Tunnel your client created; every Jupyter server and terminal is added
  to it and answered with its public URL.
- **Job metrics**: cgroup v2 memory and CPU, and per-GPU `nvidia-smi`, for the whole job.
- **Unix socket**: an optional second listener for callers in another step of the same job.
- **Single static binary** that runs as the submitting user and needs no privilege.

## Installation

```bash
curl -fsSL https://github.com/cyber-shuttle/linkspan/releases/latest/download/linkspan_Linux_x86_64.tar.gz |
  tar -xz linkspan
```

Archives are published for Linux and macOS on `x86_64` and `arm64`; versions are listed in
[CHANGELOG.md](CHANGELOG.md). To build from source, see [CONTRIBUTING.md](CONTRIBUTING.md#development-setup).

### Requirements

- Linux with cgroup v2 in Slurm's layout; the macOS archives run but report no metrics.
- `nvidia-smi` on `PATH` for GPU metrics; without it the field is omitted.
- Outbound HTTPS for `--tunnel-enable`: `tunnelsassetsprod.blob.core.windows.net` for the `devtunnel` CLI,
  then the Dev Tunnels service under `devtunnels.ms` and `rel.tunnels.api.visualstudio.com`.
- Outbound HTTPS for Jupyter: `astral.sh` and `github.com` for `uv` and its Python, `pypi.org` for the
  packages; and for terminals: `github.com` for `ttyd`, Linux only.
- A writable home directory; everything fetched or built lives under `~/.cybershuttle/`.

## Quick start

```bash
./linkspan --port 8080
```

That serves the HTTP API on loopback and nothing else. In another shell, start an SSH server for your key:

```bash
curl -X POST http://127.0.0.1:8080/api/v1/vscode/sessions \
  -H 'Content-Type: application/json' \
  -d "{\"authorized_key\": \"$(cat ~/.ssh/id_ed25519.pub)\"}"
```

## Usage

### As a batch job

The client creates the tunnel and mints the host-scoped token before submitting the job, and the job runs
Linkspan as its main process with a workflow that sets the workspace up.

```bash
#!/bin/bash
#SBATCH --signal=B:USR1@120
exec linkspan --port "$PORT" \
  --tunnel-enable \
  --tunnel-id "$CS_TUNNEL_ID" \
  --tunnel-cluster "$CS_TUNNEL_CLUSTER" \
  --tunnel-host-token "$CS_TUNNEL_HOST_TOKEN" \
  --workflow workflow.yaml
```

Hosting runs Microsoft's `devtunnel` CLI, the relay, downloaded to `~/.cybershuttle/bin/` on first use; the
tunnel's traffic transits the Dev Tunnels service. Linkspan exits non-zero if the relay fails to come up or
exits; it is not restarted. `--signal` makes Slurm send `SIGUSR1` two minutes before the time limit, which a
workflow step can act on.

### Workflows

A workflow is a list of steps. Each names a trigger with `on`, `start` by default, and either one action with
its `params` or a `tasks` list of them. `shell.exec` runs a command, and any other action is one Linkspan
offers, named by subsystem and verb, whose `params` are the request body of the matching route. A trigger's
tasks run in order and stop at the first failure, which exits Linkspan with status 1.

| Trigger | When |
|---|---|
| `start` | The listeners are up. |
| `ready` | After the `start` steps, once the tunnel relay is hosting; at once when no tunnel is hosted. |
| `stop` | Linkspan was told to exit, before anything is torn down, so the API still answers. |
| `SIGUSR1`, `SIGUSR2`, `SIGHUP` | The signal arrived, as Slurm's `--signal` sends it before the time limit. |

| Action | Does | `params` |
|---|---|---|
| `shell.exec` | Runs `command`, split on whitespace, without a shell | `command` |
| `vscode.sessions.start` | Starts an SSH server for a key | `authorized_key` |
| `jupyter.setup` | Builds the Jupyter environment ahead of the first server | |
| `jupyter.sessions.start` | Starts a Jupyter server | `root_dir`, `addr`, `token` |
| `jupyter.sessions.stop` | Stops one | `id` |
| `terminal.sessions.start` | Starts a web terminal | `cwd` |
| `terminal.sessions.stop` | Stops one | `id` |

```yaml
name: workspace
steps:
  - on: start
    tasks:
      - name: Warm the cache
        action: shell.exec
        params:
          command: /home/me/warm.sh
      - name: Build the Jupyter environment
        action: jupyter.setup
  - on: ready
    tasks:
      - name: VS Code
        action: vscode.sessions.start
        params:
          authorized_key: ssh-ed25519 AAAA... me@laptop
      - name: Jupyter
        action: jupyter.sessions.start
        params:
          root_dir: /home/me/project
  - name: Checkpoint before the time limit
    on: SIGUSR1
    action: shell.exec
    params:
      command: /home/me/checkpoint.sh
  - name: Sync results
    on: stop
    action: shell.exec
    params:
      command: /usr/bin/rsync -a /scratch/me/out/ /home/me/out/
```

A command runs without a shell, so there is no glob, variable expansion, pipe or redirection; a bare command
name is found on Linkspan's `PATH`. An action a disabled subsystem offers, or an unknown trigger, is refused
before anything binds. [`examples/workflow.yml`](examples/workflow.yml) exercises every trigger against a
local Linkspan: run it, list `/api/v1/jupyter/sessions`, send `SIGUSR1`, then `SIGTERM`, and read the lines
each step prints.

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
| GET | `/api/v1/metrics` | `{"memBytes":<n>,"cpuUsageUsec":<n>,"gpus":[{"index":<n>,"utilPct":<n>,"memUsedMiB":<n>,"memTotalMiB":<n>}]}`; a missing source omits its field, and the object is the last sample of a 5s loop |
| GET | `/api/v1/vscode/sessions` | `[{"id":"s-<port>","addr":"127.0.0.1:<port>","state":"running","error":""}]`, ordered by id, `[]` when none |
| POST | `/api/v1/vscode/sessions` | `201` with `{"id":"s-<port>","bind_port":<port>}` |
| GET | `/api/v1/jupyter/sessions` | `[{"id":"j-<port>","addr":"127.0.0.1:<port>","state":"<state>","error":"","url":"<public url>","root_dir":"<dir>","token":"<token>"}]`, ordered by id |
| POST | `/api/v1/jupyter/sessions` | `201` with one session object; takes `{"root_dir": "<dir>"}`, default Linkspan's own directory |
| DELETE | `/api/v1/jupyter/sessions/{id}` | `{"id":"j-<port>","state":"stopped"}`, `404` for an unknown id |
| GET | `/api/v1/terminal/sessions` | `[{"id":"t-<port>","addr":"127.0.0.1:<port>","state":"<state>","error":"","url":"<public url>","cwd":"<dir>"}]`, ordered by id |
| POST | `/api/v1/terminal/sessions` | `201` with one session object; takes `{"cwd": "<dir>"}`, default Linkspan's own directory |
| DELETE | `/api/v1/terminal/sessions/{id}` | `{"id":"t-<port>","state":"stopped"}`, `404` for an unknown id |

A VS Code session is one SSH server, bound on loopback for one public key, running commands through
`sh`. The POST takes `{"authorized_key": "<ssh public key>"}`, a bare key without `authorized_keys`
options; the port is accepting when the response is written. Every POST answers an error as
`{"error": "<message>"}`: `400` when the body or the key does not parse, `413` over 64KB.

A Jupyter server or terminal is created `starting` and becomes `running` once its port accepts, however
long that takes, `failed` with `error` set if it exits first, or `exited` if it ends on its own; poll the
list. A directory that does not exist fails the server rather than the request. `url` is the port's
address on the hosted tunnel, empty when no tunnel is hosted; the port is added as the server starts and
removed when it ends. A Jupyter server's port is anonymous on the tunnel, its token being the credential,
so a browser reaches it with the token alone; a terminal's is not, so the browser signs in as the tunnel's
owner, and a non-browser client sends `X-Tunnel-Authorization: tunnel <connect token>`. `addr` binds a
Jupyter server to a chosen loopback port and `token` sets its token; without them Linkspan picks a free
port and takes the token from `JUPYTER_TOKEN` in its own environment, or mints one. The first Jupyter
server builds the environment, which takes minutes; later ones start in seconds. Terminals are Linux only
and answer `501` elsewhere.

## Security

Linkspan runs as the submitting user, installs nothing outside `~/.cybershuttle/`, and binds loopback only. It
executes binaries it does not ship: `devtunnel`, `ttyd` and `uv` fetched over HTTPS on first use, the Python
`uv` installs, `nvidia-smi`, `sh` for SSH sessions, the user's shell for terminals, and whatever a workflow
step names. [SECURITY.md](SECURITY.md) states the security model and how to report a vulnerability.

## Used by

- **[cs-bridge](https://github.com/cyber-shuttle/CS-Bridge)** (VS Code extension): submits Linkspan as a
  time-bound job, starts an SSH server for the user's key, and points VS Code Remote-SSH at it.
- **cs-control** (Jupyter runtime service): submits Linkspan and creates its Jupyter session over
  `/api/v1/jupyter/sessions`, where it once shipped a `--workflow` that built the environment itself.

## Contributing

Issues and pull requests go through [GitHub](https://github.com/cyber-shuttle/linkspan/issues); see
[CONTRIBUTING.md](CONTRIBUTING.md) and [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## License

Apache-2.0. See [LICENSE](LICENSE).
