# Compatibility

Clients install and drive Linkspan through its flags, `--version` and `--help` output, release archive name, link
protocol, and `/api/v1` routes and response shapes. Changing any of these needs a coordinated client release.
`main_test.go` pins the flag names, version line, archive name, and health and metrics bodies.

## Clients

cs-plane launches Linkspan with `--port --link-url --workflow`, adding `--tunnel-enable
--tunnel-id --tunnel-cluster --tunnel-host-token` when the owner delegated a Dev Tunnels account, and exports
`JUPYTER_TOKEN` and `LINKSPAN_LINK_TOKEN`. It calls `/metrics` and `/vscode/sessions`, and reaches the Jupyter
server over the link, or over the tunnel through `/forward`.

Not yet contracts, since no client drives them: `/terminal`, `/filesystem`, `/checkpoint`, `POST /jupyter/setup`.

A "session" differs by layer: to cs-plane and cs-jupyter it is a Linkspan job; to Linkspan it is a server or process
inside that job.

## Contracts

| Surface | Contract |
|---|---|
| `--version` | A bare `X.Y.Z[.commit]`, the only line on stdout. cs-plane refuses a Linkspan below `0.20.0` by `sort -V`. |
| Archive | `linkspan_Linux_${arch}.tar.gz` holding the member `linkspan`; cs-plane curls and untars it by those names. |
| Link | One WebSocket offering subprotocols `cybershuttle.v1` and `link.<token>`, carrying yamux in binary frames, cs-plane the yamux client. Per stream cs-plane writes a port as two big-endian bytes; Linkspan answers `1` if it connected to that port, else `0`, then carries bytes until either end closes. yamux's keepalive detects a dead link and Linkspan redials. |
| Response bodies | As in the README's [HTTP API](../README.md#http-api), field names included: metrics camelCase, sessions snake_case. `/metrics` is an object. |
| `POST /vscode/sessions` | `201` with `bind_port` already accepting; a `ref` already serving answers `200` with the same server, so cs-plane names each key's server `ssh-<key hash>` and reuses it. |
| Workflow document | `tasks`, each with `on` and `steps`. cs-plane ships one `start` task whose one step is `jupyter.sessions.start` with `root_dir` and `addr`, the port it derived ahead, and the token from `JUPYTER_TOKEN`. The job lives as long as that server. |
| Jupyter token | The object's `token` field, passed to the server as `JUPYTER_TOKEN`, which every Jupyter Server release honors. |
| Delegated tunnel | Linkspan only hosts the tunnel and publishes no port, so `--tunnel-host-token` needs only the `host` scope. cs-plane declares only the control port and reaches every other port through `/forward`. |
| SSH session shell | `sh -c`, for which VS Code's server bootstrap is written. |
| SSH exit status | The child's own code; `255` when signaled; `127` only when the command could not run. A command that exited zero keeps that status even if its output was not delivered. VS Code's bootstrap branches on it. |
| SSH channels | The `sftp` subsystem and `direct-streamlocal@openssh.com` are contracts with VS Code: Remote-SSH's bootstrap falls back to SFTP, and `remote.SSH.remoteServerListenOnSocket` uses streamlocal. |
