# Compatibility

Clients install and drive Linkspan over its flags, its `--version` and `--help` output, its release archive
name, and its `/api/v1` routes and response shapes. Changing any of it needs a coordinated client release;
adding to it needs a client.

## Clients

- **cs-bridge** (VS Code): launches Linkspan with `--port --socket --tunnel-id --tunnel-cluster
  --tunnel-host-token --tunnel-enable` and calls all four `/api/v1` routes.
- **cs-control** (Jupyter): launches it with `--port --tunnel-enable --tunnel-id --tunnel-cluster
  --tunnel-host-token --workflow` and makes no HTTP calls.

Both run `--version`.

## Contracts

- `--version` prints a bare `X.Y.Z[.commit]` as the only line on stdout. cs-control reads the first line;
  cs-bridge matches the whole trimmed output against an anchored regex, so a second line makes it reinstall
  Linkspan on every launch.
- `--help` contains the literal `-tunnel-host-token`, one dash, as Go's flag package prints it. cs-control
  runs `--help 2>&1 | grep -q -- '-tunnel-host-token'` and does not submit a job when it is absent, so the
  flag cannot be renamed or removed.
- The archive name `linkspan_Linux_${arch}.tar.gz` and the `linkspan` member inside it. Both clients curl
  and untar them by those names.
- The session id `s-<port>` and the `addr` field of a listed session. cs-bridge takes the port from the
  last `:`-separated field of `addr`, falling back to the id without its `s-` prefix.
- The response bodies in the README's [HTTP API](../README.md#http-api) table, field names included:
  metrics camelCase, sessions snake_case. cs-bridge requires a GET to answer 2xx with the documented
  shape, because the tunnel edge answers 200 with an HTML page once hosting stops: `/health` keeps its
  status value, `/vscode/sessions` stays an array of `id`, `addr` and the literal `state` `running`, and
  `/metrics` a non-array object. A created session answers 2xx with both documented fields.
- `sh -c` as the session shell; VS Code's bootstrap is written for it.
- The exit status an SSH session reports: the child's own code, `255` when it was signalled, and `127`
  only when the command could not run at all. VS Code's server bootstrap runs over these sessions and
  branches on the status. A command that ran and exited zero keeps that status even when its output could
  not be delivered.
- The `sftp` subsystem and the `direct-streamlocal@openssh.com` handler. Their client is VS Code, not
  cs-bridge: Remote-SSH's bootstrap fallback uses SFTP and `remote.SSH.remoteServerListenOnSocket` uses
  streamlocal.
