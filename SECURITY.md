# Security Policy

## Supported Versions

Fixes go into the latest release; earlier releases are not patched.

## Reporting a Vulnerability

Report privately through the repository's **Security** tab, **Report a vulnerability**; not through a
public issue, pull request or discussion. Include what an attacker can reach, the steps to reproduce it,
and the version `linkspan --version` prints. We acknowledge the report and say whether we can reproduce it
before any fix is published.

## Security Model

A report is most useful when it shows one of these boundaries failing.

- **Access control at the transport**: the HTTP listener binds loopback only; the `--socket` listener is
  set to mode `0600` immediately after bind; remote callers arrive over the tunnel the client created and
  controls. Requests carry no credential because those three boundaries are the check. The port admits
  every process on the node, so Linkspan assumes the job holds its node exclusively, as the CyberShuttle
  clients request; the socket admits the job's user alone. On a shared node, bind only the socket: over
  the port another user can start an SSH server for their own key that runs commands as the job's user.
- **A client-owned tunnel**: the client creates it, registers its ports and mints the host-scoped token.
  Linkspan passes that token to `devtunnel host` and never creates, forwards, refreshes or deletes a
  tunnel. The token is a command-line argument, the only form the CLI documents, so the process list
  shows it to every user on the node; a host-scoped token allows hosting and nothing else. The tunnel
  terminates at Microsoft's Dev Tunnels service, so HTTP API traffic is not end-to-end encrypted between
  client and job; SSH sessions carry their own encryption inside it.
- **One key per SSH server**: each server accepts one public key, binds on loopback and never enables
  password authentication; a key line with `authorized_keys` options is refused, not accepted with the
  options ignored. An authenticated session has what the job's user has: command execution through `sh`,
  SFTP, and TCP and unix-socket forwarding from the node. PTY allocation is refused and reverse port
  forwarding is not offered.
- **No privilege**: Linkspan runs as the submitting user, requires no root privilege and installs nothing
  outside `~/.linkspan/`.
- **A fetched binary is executed**: with `--tunnel-enable`, Linkspan downloads Microsoft's binary over
  HTTPS from `tunnelsassetsprod.blob.core.windows.net` into `~/.linkspan/bin/` and runs it as the job's
  user. There is no checksum or signature check; the transport is the only integrity guarantee.
- **No shell for workflow commands**: they are split on whitespace with no expansion, so a workflow file
  cannot smuggle a glob, a variable or a pipe into the command it names. The file is trusted input from
  the client that submitted the job and runs commands as the job's user by design.

A finding that depends on already holding the job's credentials, an account on the compute node as
that user, or another account on a node the job was meant to hold exclusively, describes one of these
boundaries rather than a way through it.
