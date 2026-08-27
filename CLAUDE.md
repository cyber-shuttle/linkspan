# Linkspan

Go agent that runs inside a compute-node allocation. It hosts the dev tunnel its client created, runs the
client's YAML workflow, and serves an SSH server for VS Code Remote-SSH.

Build, test and lint are the Go defaults (`go build ./...`, `go test ./...`, `go vet ./...`). Public
documentation is in `README.md`, `CONTRIBUTING.md` and `SECURITY.md`. This file records what the code does
not state directly.

## Consumers

Linkspan has two consumers, and its surface is what they use. Anything not listed here has no caller; a new
entry is an API change and needs a consumer.

- **cs-bridge** (VS Code) launches it with `--port --socket --tunnel-id --tunnel-cluster --tunnel-host-token
  --tunnel-enable`, and calls `GET /health`, `GET /metrics`, `GET /vscode/sessions`, `POST /vscode/sessions`.
- **cs-control** (Jupyter) launches it with `--port --tunnel-enable --tunnel-id --tunnel-cluster
  --tunnel-host-token --workflow`, and makes no HTTP calls.

Both also run `--version`.

## Consumer contracts

Changing any of these breaks a consumer.

- `--version` prints a bare `X.Y.Z[.commit]` as the only line on stdout. cs-control reads the first line;
  cs-bridge matches the whole trimmed output against an anchored regex, so a second line makes it reinstall
  linkspan on every launch.
- `--help` contains the literal `-tunnel-host-token`, one dash, as Go's flag package prints it. cs-control
  runs `--help 2>&1 | grep -q -- '-tunnel-host-token'` and does not submit a job when it is absent, so the
  flag cannot be renamed or removed. Only the printed spelling is matched: Go accepts either form, and the
  consumers pass both (`--tunnel-enable` from cs-control, `-tunnel-enable` from cs-bridge).
- The goreleaser archive name (`linkspan_Linux_${arch}.tar.gz`) and the `linkspan` member inside it. Both
  consumers curl and untar them by those names.
- The session id `s-<port>` and the `addr` field of a session status. cs-bridge takes the port from the last
  `:`-separated field of `addr`, and falls back to stripping the `s-` prefix off the id.
- The response bodies. cs-bridge validates shape rather than status code: `/health` is an object with
  `status: "ok"`, `/vscode/sessions` is an array, `/metrics` is a non-array object, and a created session
  carries `id` and `bind_port`. The tunnel edge answers 200 with an HTML page once hosting stops, so a wrong
  shape is read as a dead host.
- The `sftp` subsystem and the `direct-streamlocal@openssh.com` handler. Neither has a cs-bridge caller;
  their client is VS Code. Remote-SSH's bootstrap fallback uses SFTP, and
  `remote.SSH.remoteServerListenOnSocket` uses streamlocal.

## Runtime behaviour

- A failing workflow step exits the process with status 1, after shutting down the HTTP server, the SSH
  sessions and the tunnel relay. An exhausted tunnel bring-up (`--tunnel-enable`, three failed attempts) does
  the same. These are the only two background failures that are fatal, so any workflow step that can fail is
  a startup dependency.
- The HTTP API binds loopback only and has no authentication. `POST /vscode/sessions` starts an sshd for a
  caller-supplied key, so reaching it is equivalent to a shell as the job owner.
- The client owns the tunnel: it creates it, registers its ports and mints a host-scoped token. Linkspan
  hosts the relay, and never creates, forwards, refreshes or deletes a tunnel.
- `make` refuses to build unless HEAD is tagged `X.Y.Z` or `X.Y.Z.<commit>`; use `go build` for a dev binary.
- The devtunnel CLI is downloaded at runtime to `~/.linkspan/bin/` on first host attempt.

## Reaching a running linkspan in-cluster

Via `--socket`, with no tunnel and no TCP port:

```bash
srun --jobid=<id> --overlap curl --unix-socket /tmp/linkspan.sock http://localhost/api/v1/metrics
```
