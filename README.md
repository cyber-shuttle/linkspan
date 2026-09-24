# Linkspan

[![CI](https://github.com/cyber-shuttle/linkspan/actions/workflows/on-pr-and-main.yml/badge.svg)](https://github.com/cyber-shuttle/linkspan/actions/workflows/on-pr-and-main.yml)
[![Release](https://img.shields.io/github/v/release/cyber-shuttle/linkspan)](https://github.com/cyber-shuttle/linkspan/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/cyber-shuttle/linkspan)](go.mod)
[![License](https://img.shields.io/github/license/cyber-shuttle/linkspan?color=blue)](LICENSE)

**Linkspan turns an HPC batch job into a workspace you can reach from your laptop.**

- **Jupyter IDE.** A Jupyter server in a folder of the job, in a Python environment Linkspan builds.
- **VS Code IDE.** An SSH server for one public key, for VS Code Remote-SSH.
- **Metrics.** The job's CPU, GPU and memory use.
- **Terminals.** A PTY inside the job, in a browser tab.
- **Filesystem (WIP).** Datasets mounted or synced into the job.
- **Pause/Resume (WIP).** A `shell.exec` command paused by its `ref` and resumed, in this job or a later one.
- **Workflows (WIP).** Steps run at set points in the job's life, from a file.

![Architecture: clients reach Linkspan's tasks through cs-plane](docs/assets/architecture.png)

Compute nodes sit behind a login node and a firewall. To get through, the client creates a
[Dev Tunnel](https://learn.microsoft.com/en-us/azure/developer/dev-tunnels/overview) and Linkspan hosts it from
inside the job. Most people reach Linkspan through [cs-bridge](https://github.com/cyber-shuttle/cs-bridge), the VS
Code extension, or [cs-jupyter](https://github.com/cyber-shuttle/cs-jupyter), the JupyterLite distribution, both
driving [cs-plane](https://github.com/cyber-shuttle/cs-plane), the control plane. This document covers running
Linkspan yourself.

## Installation

```bash
curl -fsSL https://github.com/cyber-shuttle/linkspan/releases/latest/download/linkspan_Linux_x86_64.tar.gz |
  tar -xz linkspan
```

Archives exist for `Linux` and `Darwin` on `x86_64` and `arm64`. To build from source, see
[CONTRIBUTING.md](CONTRIBUTING.md#development-setup).

### Requirements

| Feature | Needs |
|---|---|
| Everything | A writable home directory; Linkspan fetches and builds only under `~/.cybershuttle/` |
| Metrics | Linux, cgroup v2 as Slurm lays it out; macOS builds report none |
| GPU metrics | `nvidia-smi` on `PATH`; otherwise `gpus` is omitted |
| `--tunnel-enable` | HTTPS to `tunnelsassetsprod.blob.core.windows.net`, `devtunnels.ms` and `rel.tunnels.api.visualstudio.com` |
| Jupyter | `curl` on `PATH`; HTTPS to `astral.sh`, `github.com`, `release-assets.githubusercontent.com` and `pypi.org` |
| Terminals | Linux; HTTPS to `github.com` and `release-assets.githubusercontent.com` |
| Pause/Resume | `criu` on `PATH`, allowed to run unprivileged |

## Quick start

Inside the job:

```bash
./linkspan --port 8080 &

curl -X POST http://127.0.0.1:8080/api/v1/jupyter/sessions \
  -H 'Content-Type: application/json' -d '{"root_dir": "/home/me/project"}'
curl -X POST http://127.0.0.1:8080/api/v1/vscode/sessions \
  -H 'Content-Type: application/json' \
  -d "{\"authorized_key\": \"$(cat ~/.ssh/id_ed25519.pub)\"}"
```

Each reply names a loopback port: the Jupyter server's `addr`, with its `token`, and the SSH server's `bind_port`.
The first Jupyter server builds the Python environment, which takes a few minutes. `/api/v1/forward/<port>` carries
a WebSocket to either through the one API port, so whoever reaches that port reaches every server.

To reach the job from off the node, host a Dev Tunnel: create it with `devtunnel create`, declare only the API port with
`devtunnel port create <id> -p 8080`, mint a token with `devtunnel token <id> --scopes host`, and add
`--tunnel-enable --tunnel-id <id> --tunnel-cluster <cluster> --tunnel-host-token <token>`. Clients reach every server
through `/api/v1/forward` on that port.

Every server stops when Linkspan stops, so a job running Linkspan as its main process ends its workspace at the
time limit.

## Usage

### As a batch job

cs-plane creates the tunnel, mints a host-scoped token and submits a job like this:

```bash
#!/bin/bash
exec linkspan --port "$PORT" \
  --tunnel-enable \
  --tunnel-id "$CS_TUNNEL_ID" \
  --tunnel-cluster "$CS_TUNNEL_CLUSTER" \
  --tunnel-host-token "$CS_TUNNEL_HOST_TOKEN" \
  --workflow workflow.yaml
```

Linkspan fetches the `devtunnel` CLI on first use and exits non-zero if the relay exits. Add
`#SBATCH --signal=B:USR1@120` to get `SIGUSR1` two minutes before the limit.

### Workflows

A workflow is a list of tasks, each a trigger (`on`, default `start`) and its `steps`. A step is an action with
`params`; `shell.exec` runs a command, and every other action is a subsystem command named `<subsystem>.<verb>`
whose `params` are the matching route's request body. Steps run in order; the first failure exits Linkspan with
status 1. Each trigger's run then waits on the sessions its steps started. Once `start` and `ready` complete,
Linkspan exits, running the `stop` steps first, so a batch job ends with its payload and a workspace with its
servers.

| Trigger | Runs when |
|---|---|
| `start` | The listeners are up |
| `ready` | The `start` run is complete |
| `stop` | Linkspan is told to exit, before teardown, with the API still up |
| `SIGUSR1`, `SIGUSR2`, `SIGHUP` | The signal arrives |

| Action | Does | `params` |
|---|---|---|
| `shell.exec` | Runs `command` under `sh -c` as a process session; answers when it ends | `command` |
| `vscode.sessions.select`, `jupyter.sessions.select`, `terminal.sessions.select` | Lists sessions | |
| `vscode.sessions.start` | Starts an SSH server | `authorized_key` |
| `jupyter.setup` | Builds the Jupyter environment ahead of the first server | |
| `jupyter.sessions.start`, `.stop` | Starts or stops a Jupyter server | `root_dir`, `addr`, `token`; `id` |
| `terminal.sessions.start`, `.stop` | Starts or stops a web terminal | `cwd`; `id` |
| `filesystem.mount`, `.unmount`, `.copy`, `.sync` | Not implemented | `source`, `target`; `unmount` only `target` |
| `checkpoint.pause` | Snapshots a running process session with CRIU, ending it and the run of the step waiting on it | `id` |
| `checkpoint.resume` | Resumes a snapshot as a process session under the same id; answers when it ends | `id` |

```yaml
name: workspace
tasks:
  - on: start
    steps:
      - name: Build the Jupyter environment
        action: jupyter.setup
  - on: ready
    steps:
      - name: Jupyter
        action: jupyter.sessions.start
        params:
          root_dir: /home/me/project
      - name: Training loop
        ref: trainloop
        action: shell.exec
        params:
          command: python /home/me/train.py
  - on: SIGUSR1
    steps:
      - name: Checkpoint before the time limit
        action: checkpoint.pause
        params:
          id: trainloop
```

A step's `ref`, or a request's `"ref"`, is the id of the session it creates; without one Linkspan assigns it. A
repeated ref replaces the earlier session. An unknown trigger or field, or an action of a disabled subsystem, is
refused before anything starts.

| Example | Shows |
|---|---|
| [`examples/workflow.yml`](examples/workflow.yml) | Each trigger against a local Linkspan |
| [`examples/checkpoint.yml`](examples/checkpoint.yml) | A payload paused on `SIGUSR1` |
| [`examples/restore.yml`](examples/restore.yml) | The next job, resuming it |
| [`examples/checkpoint.sh`](examples/checkpoint.sh) | Runs the pair and checks the count is whole |

## Configuration

| Flag | Default | Description |
|---|---|---|
| `--port` | `8080` | HTTP API port on loopback; `0` picks a free one |
| `--workflow` | | Workflow YAML file |
| `--tunnel-enable` | `false` | Host the tunnel named by `--tunnel-id` |
| `--tunnel-id` | | Id of the client-created tunnel; required with `--tunnel-enable` |
| `--tunnel-cluster` | | Cluster id of `--tunnel-id`; required with `--tunnel-enable` |
| `--tunnel-host-token` | | Host-scoped access token; required with `--tunnel-enable` |
| `--version` | | Print the version and exit |

| Environment | Read by |
|---|---|
| `JUPYTER_TOKEN` | `jupyter.sessions.start`, as the default token |

Each subsystem can be switched off in `main.go`'s `config`; all ship on. An off subsystem has no routes, answering
`404`, and no workflow actions.

| Subsystem | Offers |
|---|---|
| `workflow` | `POST /workflow/shell/exec`; the `shell.exec` action stays available regardless |
| `vscode` | SSH servers for Remote-SSH |
| `jupyter` | Jupyter servers in a `uv`-built environment |
| `terminal` | `ttyd` web terminals, Linux only |
| `filesystem` | Mount, unmount, copy and sync, declared and answering `501` |
| `checkpoint` | Pause and resume of process sessions with CRIU |

## HTTP API

Requests carry no credential; reaching the port is the authorization ([SECURITY.md](SECURITY.md)). Every route
answers errors as `{"error": "<message>"}`: `400` for an unparsable body, `413` over 64KB.

| Method | Path | Answers |
|---|---|---|
| GET | `/api/v1/health` | `{"status":"ok"}` |
| GET | `/api/v1/forward/{port}` | WebSocket carrying one TCP connection, in binary frames, to the loopback port a running task serves; `404` when none does |
| GET | `/api/v1/metrics` | `{"memBytes":<n>,"cpuUsageUsec":<n>,"gpus":[{"index":<n>,"utilPct":<n>,"memUsedMiB":<n>,"memTotalMiB":<n>}]}`, the latest sample, taken every 5s; an unreadable source omits its field |
| POST | `/api/v1/workflow/shell/exec` | Takes `{"command","ref"}`, `400` without `command`; `200` with the process session once it exits 0, `202` once a pause ended it, `500` otherwise |
| GET | `/api/v1/vscode/sessions` | `[{"id":"s-<port>","addr":"127.0.0.1:<port>","state":"<state>","error":""}]` |
| POST | `/api/v1/vscode/sessions` | Takes `{"authorized_key","ref"}`; `201` with `{"id":"s-<port>","bind_port":<port>}` |
| GET | `/api/v1/jupyter/sessions` | `[{"id":"j-<port>","addr":"127.0.0.1:<port>","state":"<state>","error":"","root_dir":"<dir>","token":"<token>"}]` |
| POST | `/api/v1/jupyter/sessions` | Takes `{"root_dir","addr","token","ref"}`; `201` with one session object |
| DELETE | `/api/v1/jupyter/sessions/{id}` | `{"id":"<id>","state":"stopped"}`; `404` for an unknown id |
| POST | `/api/v1/jupyter/setup` | `200` once the environment is built |
| GET | `/api/v1/terminal/sessions` | `[{"id":"t-<port>","addr":"127.0.0.1:<port>","state":"<state>","error":"","cwd":"<dir>"}]` |
| POST | `/api/v1/terminal/sessions` | Takes `{"cwd","ref"}`; `201` with one session object; `501` off Linux |
| DELETE | `/api/v1/terminal/sessions/{id}` | `{"id":"<id>","state":"stopped"}`; `404` for an unknown id |
| POST | `/api/v1/filesystem/{mount,unmount,copy,sync}` | Takes `{"source","target"}`, `unmount` only `target`; `400` for a missing param, else `501` |
| POST | `/api/v1/checkpoint/pause` | Takes `{"id"}`; `200` with `{"id","dir"}` once the snapshot is written; `404` for no such running session, `500` with CRIU's error, `501` without CRIU |
| POST | `/api/v1/checkpoint/resume` | Takes `{"id"}`; as `shell/exec` once the resumed session ends; `404` for no snapshot, `501` without CRIU |

Lists are ordered by id and `[]` when empty. An empty `root_dir` or `cwd` is Linkspan's working directory.

**VS Code.** One SSH server per key, on loopback, accepting only that key and running commands through `sh`. A key
with `authorized_keys` options is refused with `400`. The port accepts before the reply.

**Jupyter and terminals.** A server starts `starting`, becomes `running` once its port accepts, and ends `exited` on
status 0 or `failed` with `error` otherwise; a missing folder fails the server, not the request. It binds loopback
and is reached through `/api/v1/forward`. `addr` pins a Jupyter server's loopback address; `token`
defaults to `JUPYTER_TOKEN`, else a minted one.

**Process sessions.** `shell.exec` runs under `sh -c` on Linkspan's stdout and stderr, with id `ref` or `p-<n>`,
and its object carries `pid`. A pause runs `criu dump` into `~/.cybershuttle/checkpoints/<id>`, replacing an
earlier snapshot only on success; the session ends and its waiting step answers `202`, ending its trigger's run.
A resume runs `criu restore` as a new session under the same id, on the new job's stdout and stderr.

## Architecture

Linkspan is one static Go binary built on three ideas.

- **Everything that runs is a task.** The listener, the relay, the metrics sampler, the workflow, each SSH
  server and each child process are entries of one registry, each a function under a context with an id, an
  address and a state. Starting a task binds its address first, so the port is reserved before the reply.
  Stopping Linkspan cancels every task and waits for it; a child dies with its process group.
- **A command is both a route and a workflow action.** Each subsystem exports a table of commands, plain
  functions over a params map, and a route table naming them, so `POST /api/v1/jupyter/sessions` and a
  `jupyter.sessions.start` step run one function.
- **Building blocks are composed where used.** `internal/` holds primitives; a subsystem composes them into a
  task at the point it starts one.

[CONTRIBUTING.md](CONTRIBUTING.md) has the source layout and checks; [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md)
the client contracts; [SECURITY.md](SECURITY.md) the security model and how to report a vulnerability.

## License

Apache-2.0; see [LICENSE](LICENSE).
