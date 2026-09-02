# Compatibility

Clients install and drive Linkspan over its flags, its `--version` and `--help` output, its release archive
name, and its `/api/v1` routes and response shapes. Changing any of it needs a coordinated client release, and
adding to it needs a consumer.

## Consumers

- **cs-bridge** (VS Code) launches Linkspan with `--port --socket --tunnel-id --tunnel-cluster
  --tunnel-host-token --tunnel-enable`, and calls `GET /api/v1/health`, `GET /api/v1/metrics`,
  `GET /api/v1/vscode/sessions`, `POST /api/v1/vscode/sessions`.
- **cs-control** (Jupyter) launches it with `--port --tunnel-enable --tunnel-id --tunnel-cluster
  --tunnel-host-token --workflow`, and makes no HTTP calls.

Both also run `--version`.

## Contracts

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
- The response bodies in the README's [HTTP API](../README.md#http-api) table. cs-bridge requires a GET to
  answer 2xx and to return the documented shape, because the tunnel edge answers 200 with an HTML page once
  hosting stops: `/health` must keep its documented status value, `/vscode/sessions` must stay an array and
  `/metrics` a non-array object. A created session must answer 2xx and carry both of its documented fields.
- The `sftp` subsystem and the `direct-streamlocal@openssh.com` handler. Neither has a cs-bridge caller;
  their client is VS Code. Remote-SSH's bootstrap fallback uses SFTP, and
  `remote.SSH.remoteServerListenOnSocket` uses streamlocal.
