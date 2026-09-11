# Compatibility

Clients install and drive Linkspan over its flags, its `--version` and `--help` output, its release archive
name, and its `/api/v1` routes and response shapes. Changing any of it needs a coordinated client release.
Adding to it needs a client.

## Clients

- cs-bridge, the VS Code extension, launches Linkspan with `--port --socket --tunnel-id --tunnel-cluster
  --tunnel-host-token -tunnel-enable`, the last with one dash as Go's flag package also accepts, and calls
  the health, metrics and `/vscode/sessions` routes, over the tunnel and over the socket. It reads the
  `state` of a listed session only to skip `failed` ones.
- cs-control, the Jupyter runtime service, launches it with `--port --tunnel-enable --tunnel-id --tunnel-cluster
  --tunnel-host-token --workflow`, exports `JUPYTER_TOKEN`, and makes no HTTP calls. Its document is one
  `jupyter.sessions.start` step naming `root_dir` and `addr`, the port it declared on the tunnel ahead.
- A browser opens `/terminal/sessions` URLs after the tunnel owner signs in at the Dev Tunnels page.
- The `/filesystem` routes and commands answer `501` and have no client yet, so they are not contracts.
  Nor is `POST /jupyter/setup`, which is a route because every command is one.

Both clients run `--version`.

## Contracts

- `--version` prints a bare `X.Y.Z[.commit]` as the only line on stdout. cs-control reads the first line.
  cs-bridge matches the whole trimmed output against an anchored regex, so a second line makes it reinstall
  Linkspan on every launch.
- `--help` contains the literal `-tunnel-host-token`, one dash, as Go's flag package prints it. cs-control
  runs `--help 2>&1 | grep -q -- '-tunnel-host-token'` and does not submit a job when it is absent, so the
  flag cannot be renamed or removed.
- The archive is named `linkspan_Linux_${arch}.tar.gz` and holds the `linkspan` member. Both clients curl
  and untar them by those names.
- The session id is `s-<port>` and a listed session carries `addr`. cs-bridge takes the port from the last
  `:`-separated field of `addr`, falling back to the id without its `s-` prefix.
- The response bodies are those in the README's [HTTP API](../README.md#http-api) table, field names
  included, with metrics in camelCase and sessions in snake_case. cs-bridge requires a GET to answer 2xx
  with the documented shape, because the tunnel edge answers 200 with an HTML page once hosting stops. So
  `/health` keeps its status value, `/vscode/sessions` stays an array of `id`, `addr` and the literal
  `state` `running`, and `/metrics` stays a non-array object. A created session answers 2xx with both
  documented fields.
- The session shell is `sh -c`, for which VS Code's bootstrap is written.
- The workflow document cs-control ships has `name` and `steps`, each `action: shell.exec` with `name` and
  `params.command` or `action: jupyter.sessions.start` with `params.root_dir` and `params.addr`, run in
  order at startup, with the Jupyter token taken from `JUPYTER_TOKEN` in Linkspan's environment. A step's `on`
  and `tasks` and every other action are additions, and a document without them loads and runs as it did.
- The tunnel port a Jupyter server or terminal is published on is added with the token from
  `--tunnel-host-token`, so that token must carry port rights. cs-control mints `host manage:ports`, and
  cs-bridge mints `host`, which the Dev Tunnels contract states includes port updates. A Jupyter server's
  port is anonymous, so cs-jupyter reaches it with the Jupyter token alone, as it did the port cs-control
  declared. A terminal's port is not anonymous. Linkspan republishes a port cs-control declared ahead
  without its description, so cs-control finds that port by number.
- A Jupyter server's token is the `token` field of its object, minted per server. The server reads it
  from `JUPYTER_TOKEN`, so any Jupyter Server release honors it.
- An SSH session reports the child's own exit code, `255` when it was signaled, and `127` only when the
  command could not run at all. VS Code's server bootstrap runs over these sessions and
  branches on the status. A command that ran and exited zero keeps that status even when its output could
  not be delivered.
- The `sftp` subsystem and the `direct-streamlocal@openssh.com` handler are contracts with VS Code itself,
  since Remote-SSH's bootstrap fallback uses SFTP and `remote.SSH.remoteServerListenOnSocket` uses
  streamlocal.
