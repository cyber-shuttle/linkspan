# Security Policy

## Supported Versions

Fixes go into the latest release. Clients check for a newer release on every launch and install it, so
running the latest is the expected state.

## Reporting a Vulnerability

Report vulnerabilities privately through GitHub: open the repository's **Security** tab and choose **Report a
vulnerability**. Please do not use a public issue, pull request or discussion for a security problem.

Include what an attacker can reach, the steps to reproduce it, and the version `linkspan --version` prints.
We will acknowledge the report and say whether we can reproduce it before any fix is published.

## Security Model

These are the boundaries Linkspan is designed around. A report is most useful when it shows one of them
failing.

- **Access control is at the transport, not in the API.** The HTTP listener binds loopback only, so nothing
  off the node can reach it. The `--socket` listener is created mode `0600`, so only the job's own user can
  connect. Remote callers reach the API over the tunnel the client created and controls. Requests carry no
  separate credential, because those three boundaries are the check.
- **The tunnel belongs to the client.** The client creates it, registers its ports and mints the host-scoped
  token the job is given. That token authorizes hosting and nothing else: Linkspan cannot create, forward,
  refresh or delete a tunnel, and a job that leaked its token could not use it to reach anything.
- **SSH servers accept one public key and only that key**, and bind on loopback. Password authentication is
  never enabled.
- **Linkspan runs as the submitting user**, with that user's privileges and no more. It needs no root, and
  installs nothing outside `~/.linkspan/`.
- **Workflow commands run without a shell**, split on whitespace with no expansion, so a workflow file cannot
  smuggle a glob, a variable or a pipe into the command it names. The file itself is trusted input: it comes
  from the client that submitted the job, and by design it runs commands as the job's user.

A finding that depends on already holding the job's credentials, or on already having an account on the
compute node as that user, describes one of these boundaries rather than a way through it.
