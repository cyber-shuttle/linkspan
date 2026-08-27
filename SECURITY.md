# Security Policy

## Supported Versions

Fixes go into the latest release. Clients check for a newer release on every launch and install it, so
running the latest is the expected state.

## Reporting a Vulnerability

Report vulnerabilities privately through GitHub: open the repository's **Security** tab and choose **Report a
vulnerability**. Please do not open a public issue, pull request or discussion for a security problem.

Please include what an attacker can reach, the steps to reproduce it, and the version `linkspan --version`
prints. We will acknowledge the report and tell you whether we can reproduce it before any fix is published.

## Security Model

Knowing what Linkspan already assumes will tell you whether a finding is a vulnerability or a documented
boundary.

- **The HTTP API has no authentication and binds to loopback only.** `POST /api/v1/vscode/sessions` starts an
  SSH server for a caller-supplied public key, so anything that can reach the API can get a shell as the job's
  user. Access control is the tunnel's and the socket's, not the API's.
- **The unix socket from `--socket` is created mode `0600`**, so only the job's own user can connect to it.
- **The tunnel is the client's.** The client creates it, registers its ports and mints the host-scoped token
  the job is given. That token authorizes hosting and nothing else: Linkspan cannot create, forward, refresh
  or delete a tunnel.
- **SSH servers accept one public key and only that key**, bound on loopback. Password authentication is
  never enabled.
- **Linkspan runs as the submitting user**, with that user's privileges and no more. It needs no root and
  installs nothing outside `~/.linkspan/`.
- **Workflow commands run without a shell**, split on whitespace with no expansion, so a workflow file cannot
  smuggle a glob, a variable or a pipe into the command it names. A workflow file is trusted input: whoever
  writes it can run commands as the job's user by design.

Findings that turn on holding the job's own credentials, or on already having an account on the compute node
as that user, are outside this model.
