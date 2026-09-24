# Security Policy

## Supported Versions

Fixes go into the latest release only.

## Reporting a Vulnerability

Report privately through the repository's **Security** tab, **Report a vulnerability**, never in a public issue,
pull request or discussion. Include what an attacker can reach, steps to reproduce, and the output of
`linkspan --version`. The maintainers acknowledge the report and state whether it reproduces before any fix is
published.

## Security Model

A useful report shows one of these boundaries failing.

- **Access control is at the transport.** Requests carry no credential. The HTTP listener binds loopback, and remote
  callers arrive over the link or the client-owned tunnel. The port admits every user on the node, any of whom could
  start an SSH server for their own key running as the job's user, and cs-plane does not request `--exclusive`.
- **The link trusts cs-plane.** Linkspan presents the token from `LINKSPAN_LINK_TOKEN`, kept off the process list,
  as a WebSocket subprotocol, in clear over `ws`. cs-plane, like any caller of `/api/v1/forward/{port}`, may open a
  stream to any loopback port a running task serves, the API's included, and nothing else on the node.
- **The tunnel is client-owned.** The client creates it and mints the host-scoped token. Linkspan passes the token
  to `devtunnel host` as a command-line argument, the only form the CLI documents, so the process list shows it to
  every user on the node. Linkspan only hosts it: it publishes no port and never creates, refreshes or deletes a
  tunnel. The client declares the API port, and every server is reached through `/api/v1/forward` behind it. The
  tunnel terminates at Microsoft's Dev Tunnels service, so HTTP traffic is not end-to-end encrypted; SSH carries its
  own encryption.
- **Each SSH server admits one key.** It binds loopback and never offers password authentication. A key with
  `authorized_keys` options is refused, not accepted with the options ignored. A session has what the job's user
  has: commands through `sh`, SFTP, and TCP and unix-socket forwarding from the node. PTYs and reverse forwarding
  are refused.
- **Linkspan holds no privilege.** It runs as the submitting user and writes only under `~/.cybershuttle/`.
- **Fetched binaries run unverified.** `devtunnel` from `tunnelsassetsprod.blob.core.windows.net`, `ttyd` from its
  GitHub release at a pinned version, and `uv` through Astral's installer script are fetched over HTTPS into
  `~/.cybershuttle/bin/`; `uv` in turn fetches Python from GitHub and packages from PyPI. HTTPS is the only integrity
  check.
- **Other programs run as the job's user.** `nvidia-smi` and `criu` from `PATH`, `sh` for SSH sessions and
  `shell.exec`, and `$SHELL` for terminals. A Jupyter server runs kernels and terminals for whoever holds its token;
  a web terminal is a PTY. A workflow file is trusted input from the client that submitted the job.

A finding that requires already holding the job's credentials or the job user's account describes one of these
boundaries rather than a way through it.
