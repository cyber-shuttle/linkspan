# Security Policy

## Supported Versions

Fixes go into the latest release. Earlier releases are not patched.

## Reporting a Vulnerability

Reports go privately through the repository's **Security** tab, under **Report a vulnerability**, rather
than through a public issue, pull request or discussion. A report includes what an attacker can reach, the
steps to reproduce it, and the version `linkspan --version` prints. The maintainers acknowledge the report
and state whether it reproduces before any fix is published.

## Security Model

A report is most useful when it shows one of these boundaries failing.

- **Access control is at the transport.** The HTTP listener binds loopback only, the `--socket` listener
  is set to mode `0600` immediately after bind, and remote callers arrive over the tunnel the client
  created and controls. Requests carry no credential because those three boundaries are the check. The
  port admits every process on the node, so Linkspan assumes the job holds its node exclusively, as the
  CyberShuttle clients request. The socket admits the job's user alone. The port is always bound, so on a
  shared node another user can start an SSH server for their own key that runs commands as the job's
  user; Linkspan is not for a shared node.
- **The tunnel is client-owned.** The client creates it, registers its own ports and mints the host-scoped
  token. Linkspan passes that token to `devtunnel host`, adds a port for each Jupyter server or
  terminal it starts and removes it when the server ends, and never creates, refreshes or deletes a
  tunnel. A terminal's port is non-anonymous, so reaching it means signing in as the tunnel's owner. A
  Jupyter server's port is anonymous, its token being the credential, because a browser client cannot
  present a tunnel token. The token is a command-line argument, the only form the CLI documents, so the
  process list shows it to every user on the node. A host-scoped token allows hosting only. The tunnel
  terminates at Microsoft's Dev Tunnels service, so HTTP API traffic is not end-to-end encrypted between
  client and job. SSH sessions carry their own encryption inside it.
- **Each SSH server has one key.** Each server accepts one public key, binds on loopback and never enables
  password authentication. A key line with `authorized_keys` options is refused rather than accepted with
  the options ignored. An authenticated session has what the job's user has: command execution through `sh`,
  SFTP, and TCP and unix-socket forwarding from the node. PTY allocation is refused and reverse port
  forwarding is not offered.
- **Linkspan holds no privilege.** It runs as the submitting user, requires no root privilege and installs
  under `~/.cybershuttle/`.
- **Fetched binaries are executed.** Linkspan downloads Microsoft's `devtunnel` from
  `tunnelsassetsprod.blob.core.windows.net`, `ttyd` from its GitHub release at a pinned version, and
  `uv` through Astral's installer script, all over HTTPS into `~/.cybershuttle/bin/`, and runs them as the
  job's user, and `uv` in turn fetches a Python and packages from PyPI. There is no checksum or signature
  check, so the transport is the only integrity guarantee.
- **A Jupyter server and a terminal are shells.** A Jupyter server accepts its token in the query and
  runs kernels and terminals as the job's user, and a web terminal is a login shell. Both bind loopback and
  are reachable only over the tunnel port, a Jupyter server's with its token and a terminal's after the
  owner's sign-in, or by a process on the node.
- **Workflow commands run without a shell.** They are split on whitespace with no expansion, so a workflow
  file cannot smuggle a glob, a variable or a pipe into the command it names. The file is trusted input from
  the client that submitted the job and runs commands as the job's user by design.

A finding that depends on already holding the job's credentials, an account on the compute node as
that user, or another account on a node the job was meant to hold exclusively, describes one of these
boundaries rather than a way through it.
