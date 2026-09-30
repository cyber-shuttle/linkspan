# Compatibility

Clients install and drive Linkspan through its flags, `--version` and `--help` output, release archive name, link
protocol, and `/api/v1` routes and response shapes. Changing any of these needs a coordinated client release.
`main_test.go` pins the flag names, version line, archive name, and health and usage bodies.

## Clients

cs-plane launches Linkspan with `--port --workflow --tunnel-enable --tunnel-mode=<the session's transports>` and each
selected transport's args, `--tunnel-link-args="--url $CS_LINK_URL"` and `--tunnel-devtunnel-args="--id … --cluster …"`,
and exports `JUPYTER_TOKEN`, `LINKSPAN_LINK_TOKEN` and `LINKSPAN_TUNNEL_HOST_TOKEN`. It calls `/health`, `/usage` and
`/vscode/sessions`, and reaches the Jupyter server over the link, or over the Dev Tunnel through `/forward`.

cs-bridge attaches the session on cs-plane and launches Linkspan with `--port <attach port> --tunnel-enable
--tunnel-mode link|devtunnel` and that transport's args, its token only in the environment.

Not yet contracts, since no client drives them: `/terminal`, `/filesystem`, `/checkpoint`, `POST /jupyter/setup`.

A cs-plane session runs Linkspan as its Slurm job; what Linkspan runs inside that job is a server or process, though
its routes keep the `/sessions` path.

## Contracts

| Surface | Contract |
|---|---|
| `--version` | A bare `X.Y.Z[.commit]`, the only line on stdout. cs-plane refuses a Linkspan below `0.22.0` by `sort -V`. |
| Archive | `linkspan_Linux_${arch}.tar.gz` holding the member `linkspan`; cs-plane curls and untars it by those names. |
| Link | One WebSocket offering subprotocols `cybershuttle.v1` and `link.<token>`, carrying yamux in binary frames, cs-plane the yamux client. Per stream cs-plane writes a port as two big-endian bytes; Linkspan answers `1` if it connected to that port, else `0`, then carries bytes until either end closes. yamux's keepalive detects a dead link and Linkspan redials. |
| Response bodies | As in the README's [HTTP API](../README.md#http-api), field names included: `/usage` camelCase, `/sessions` snake_case. `/usage` is an object. |
| `POST /vscode/sessions` | `201` with `bind_port` already accepting; a `ref` already serving answers `200` with the same server, so cs-plane and cs-bridge name each key's server `ssh-<key hash>` and keep one per key. |
| Workflow document | `tasks`, each a trigger with `on` and `steps`. cs-plane ships one `start` trigger whose one step is `jupyter.sessions.start` with `root_dir` and `addr`, the port it derived ahead, and the token from `JUPYTER_TOKEN`. The job lives as long as that server. |
| Jupyter token | The object's `token` field, passed to the server as `JUPYTER_TOKEN`, which every Jupyter Server release honors. |
| Dev Tunnel | Linkspan only hosts the Dev Tunnel and publishes no port, so `LINKSPAN_TUNNEL_HOST_TOKEN` needs only the `host` scope. cs-plane declares only the control port and reaches every other port through `/forward`. |
| SSH shell | `sh -c`, for which VS Code's server bootstrap is written. |
| SSH exit status | The child's own code; `255` when signaled; `127` only when the command could not run. A command that exited zero keeps that status even if its output was not delivered. VS Code's bootstrap branches on it. |
| SSH channels | The `sftp` subsystem and `direct-streamlocal@openssh.com` are contracts with VS Code: Remote-SSH's bootstrap falls back to SFTP, and `remote.SSH.remoteServerListenOnSocket` uses streamlocal. |
