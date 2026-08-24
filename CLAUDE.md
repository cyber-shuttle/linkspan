# Linkspan

Go agent that runs inside a compute-node allocation: hosts the dev tunnel its client created, runs the
client's YAML workflow, and serves an SSH server for VS Code Remote-SSH.

## Prerequisites

- Go 1.24+ (toolchain go1.24.13)
- Optional: goreleaser for release builds

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
./linkspan --workflow /path/to/workflow.yaml    # run a workflow
./linkspan --port 0                             # OS-assigned random port
./linkspan --socket /tmp/linkspan.sock          # also serve on a unix socket
```

Reach `--socket` in-cluster (no tunnel/TCP port): `srun --jobid=<id> --overlap curl --unix-socket /tmp/linkspan.sock http://localhost/api/v1/metrics`

## Architecture

```
main.go                         # CLI flags, startup, shutdown -- no request handling
internal/
  httpapi/                      # every route, handler and listener
  workflow/                     # YAML workflow: load, then run shell.exec steps in order
  process/                      # background process tracking, process.Global singleton
subsystems/
  metrics/                      # cgroup-v2 memory/cpu + per-GPU nvidia-smi, as a Snapshot
  sshd/                         # Supervised SSH server (gliderlabs/ssh) with PTY support
  tunnel/                       # devtunnel CLI download + relay hosting, retried
```

## The consumer contract

Linkspan has exactly two consumers, and its surface is exactly what they use. Anything not on this list has
no caller — do not add to it speculatively, and treat a new entry as an API change that needs a consumer.

- **cs-bridge** (VS Code) launches it with `--port --socket --tunnel-id --tunnel-cluster --tunnel-host-token
  --tunnel-enable`, and calls `GET /health`, `GET /metrics`, `GET /vscode/sessions`, `POST /vscode/sessions`.
- **cs-control** (Jupyter) launches it with `--port --tunnel-enable --tunnel-id --tunnel-cluster
  --tunnel-host-token --workflow`, and makes no HTTP calls at all.

Both also run `--version`, and cs-control greps `--help` for `-tunnel-host-token`. That single dash is not
a typo: Go's flag package prints flags as `-name`, and cs-control matches that literal
(`--help 2>&1 | grep -q -- '-tunnel-host-token'`). Flags are written `--name` everywhere else here, which is
how they are passed.

## REST API (`/api/v1/`)

- `GET /health` — liveness, `{"status":"ok"}`
- `GET /metrics` — cgroup-v2 memory/CPU + per-GPU `nvidia-smi`; each source omits its field when absent,
  and the nvidia-smi probe is bounded so a wedged driver cannot hold the handler open
- `GET /vscode/sessions` — list SSH servers and supervisor state
- `POST /vscode/sessions` — start an SSH server for one authorized key

## Workflow Engine

A workflow is a `name` and a list of `steps`, run in order, stopping at the first failure. `shell.exec` is
the only action: `params.command` is split on whitespace and run without a shell, so nothing is expanded.
Cancellation is checked between steps; the current step is not preempted.

A step's output goes straight to linkspan's stdout as it happens, not captured and logged afterwards: a
step that daemonises (cs-control ends with `setsid --fork python -m jupyter_server`) keeps the descriptors
it inherited, and waiting on a pipe would block until that server exited.

**A failing step exits the whole process with status 1**, after shutting the HTTP server, the SSH sessions
and the tunnel relay down. That is deliberate — an allocation whose setup failed has nothing left to serve —
but it means a workflow is not a background nicety: any step that can fail is a startup dependency. An
exhausted tunnel bring-up (`--tunnel-enable`, three failed attempts) exits the same way, through the same
path; those are the only two background failures that are fatal.

## Key Patterns

- **process.Global** singleton tracks long-running processes (the devtunnel host CLI); its output
  buffers are mutex-guarded because callers read them while the process is still writing
- The client owns the tunnel: it creates it, registers its ports, and mints a host-scoped token. Linkspan
  hosts the relay and never creates, forwards, refreshes or deletes a tunnel.
- **internal/httpapi is the only package that serves HTTP.** Subsystems report data — `metrics.Read(ctx)`
  returns a Snapshot, `sshd.Start` returns an id and port — and know nothing about requests or JSON
- **The HTTP API binds loopback only, and has no authentication.** `POST /vscode/sessions` starts an sshd
  for a caller-supplied key, so reaching it is equivalent to a shell as the job owner. Both consumers get
  there through the devtunnel relay (a child process on this node, which dials localhost) or `--socket`;
  binding the wildcard would offer that endpoint to anything that can route to the compute node
- The SSH server accepts exactly one public key, supplied at create time, and binds loopback itself.
  `sshd.Start` binds before it returns, so the port it reports is already accepting and a bind failure
  is a 500 rather than a session that never works. The session id embeds that port (`s-<port>`)
- The SSH server is supervised: it restarts on non-graceful exit, bounded by consecutive failures; each
  restart rebinds the same address
- With `--port 0` the OS assigns the port; it is read back off the listener and appears only in the startup log line

## Gotchas

- `--version` must print a bare `X.Y.Z[.commit]` as the **only** line on stdout. cs-control tolerates
  trailing lines; cs-bridge does not, and a second line makes it reinstall linkspan on every launch.
- `--help` must contain the literal `-tunnel-host-token` — one dash, as Go's flag package prints it.
  cs-control greps for exactly that and refuses to submit a job without it, so the flag cannot be renamed
  or removed. It is passed as `--tunnel-host-token`; both spellings work, only the printed one is matched.
- The goreleaser archive name (`linkspan_Linux_${arch}.tar.gz`) and the `linkspan` member inside it are a
  contract — both consumers curl and untar them by those exact names.
- The devtunnel CLI is downloaded at runtime to `~/.linkspan/bin/` on first host attempt
- The SSH server keeps the `sftp` subsystem and the `direct-streamlocal@openssh.com` handler: VS Code
  Remote-SSH's bootstrap fallback uses SFTP, and `remote.SSH.remoteServerListenOnSocket` uses streamlocal.
  Neither shows up in a cs-bridge grep because the client is VS Code, not cs-bridge.
- SSH server spawns a shell via PTY (creack/pty) — resize handled via the SSH window-change channel, and
  the pty request's terminal type is passed through as TERM, which a batch job's environment does not have
- `s.Context()` is gliderlabs' **connection** context, not the session's, so it cannot end one session's
  shell. What does is closing the pty master when the copy to the client stops — the shell exiting or the
  session going away. Stdin reaching EOF is not that signal: a client may send no more input and stay
- gliderlabs sends exit-status 0 for any session whose handler just returns, so both the exec and PTY
  paths call `s.Exit` with the command's real status; without it every failure looks like success
- `make` refuses to build unless HEAD is tagged `X.Y.Z` or `X.Y.Z.<commit>`; use `go build` for a dev binary
