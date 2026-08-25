# Linkspan

Go agent that runs inside a compute-node allocation: hosts the dev tunnel its client created, runs the
client's YAML workflow, and serves an SSH server for VS Code Remote-SSH.

Build, test and lint are the Go defaults (`go build ./...`, `go test ./...`, `go vet ./...`). What follows
is only what reading the code does not tell you — the rest lives in the code and its comments.

## The consumer contract

Linkspan has exactly two consumers, and its surface is exactly what they use. Anything not on this list has
no caller — do not add to it speculatively, and treat a new entry as an API change that needs a consumer.

- **cs-bridge** (VS Code) launches it with `--port --socket --tunnel-id --tunnel-cluster --tunnel-host-token
  --tunnel-enable`, and calls `GET /health`, `GET /metrics`, `GET /vscode/sessions`, `POST /vscode/sessions`.
- **cs-control** (Jupyter) launches it with `--port --tunnel-enable --tunnel-id --tunnel-cluster
  --tunnel-host-token --workflow`, and makes no HTTP calls at all.

Both also run `--version`.

## Changing these breaks a consumer

- `--version` must print a bare `X.Y.Z[.commit]` as the **only** line on stdout. cs-control tolerates
  trailing lines; cs-bridge does not, and a second line makes it reinstall linkspan on every launch.
- `--help` must contain the literal `-tunnel-host-token` — one dash, as Go's flag package prints it.
  cs-control greps for exactly that (`--help 2>&1 | grep -q -- '-tunnel-host-token'`) and refuses to submit
  a job without it, so the flag cannot be renamed or removed. Flags are written `--name` everywhere else,
  which is how they are passed; only the printed spelling is matched.
- The goreleaser archive name (`linkspan_Linux_${arch}.tar.gz`) and the `linkspan` member inside it — both
  consumers curl and untar them by those exact names.
- The session id shape `s-<port>` — cs-bridge strips the prefix to recover the port.
- The `sftp` subsystem and the `direct-streamlocal@openssh.com` handler have no cs-bridge caller and look
  dead. They are not: VS Code Remote-SSH's bootstrap fallback uses SFTP, and
  `remote.SSH.remoteServerListenOnSocket` uses streamlocal. The client is VS Code, not cs-bridge.

## Behaviour you would not guess

- **A failing workflow step exits the whole process with status 1**, after shutting down the HTTP server,
  the SSH sessions and the tunnel relay. So does an exhausted tunnel bring-up (`--tunnel-enable`, three
  failed attempts). Those are the only two background failures that are fatal — a workflow is not a
  background nicety, so any step that can fail is a startup dependency.
- The HTTP API binds loopback only and has **no authentication**. `POST /vscode/sessions` starts an sshd
  for a caller-supplied key, so reaching it is equivalent to a shell as the job owner.
- The client owns the tunnel: it creates it, registers its ports and mints a host-scoped token. Linkspan
  only hosts the relay, and never creates, forwards, refreshes or deletes a tunnel.
- `make` refuses to build unless HEAD is tagged `X.Y.Z` or `X.Y.Z.<commit>`; use `go build` for a dev binary.
- The devtunnel CLI is downloaded at runtime to `~/.linkspan/bin/` on first host attempt.

## Reaching a running linkspan in-cluster

Via `--socket`, with no tunnel and no TCP port:

```bash
srun --jobid=<id> --overlap curl --unix-socket /tmp/linkspan.sock http://localhost/api/v1/metrics
```
