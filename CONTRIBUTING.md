# Contributing to Linkspan

Thank you for your interest in contributing to Linkspan. This document covers the source layout, how to get a
development build running, and the contracts a change must not break. Bug reports, features and documentation
fixes are all welcome.

## Development Setup

### Prerequisites

- [Git](https://git-scm.com/)
- [Go](https://go.dev/dl/) 1.24+

Nothing else. Linkspan has no code generation step, no C dependencies and no service to run locally.

### Getting Started

```bash
git clone https://github.com/cyber-shuttle/linkspan.git
cd linkspan

go build -o linkspan .
./linkspan --port 8080
```

With no flags beyond `--port`, Linkspan serves its HTTP API on loopback and starts neither a tunnel nor a
workflow, which is enough to exercise every endpoint:

```bash
curl http://127.0.0.1:8080/api/v1/health
curl http://127.0.0.1:8080/api/v1/metrics
curl -X POST http://127.0.0.1:8080/api/v1/vscode/sessions \
  -H 'Content-Type: application/json' \
  -d "{\"authorized_key\": \"$(cat ~/.ssh/id_ed25519.pub)\"}"
```

The metrics endpoint reports whichever sources exist on the machine: off a Linux compute node, cgroup-v2 and
`nvidia-smi` are usually both absent and their fields are omitted.

## Source Layout

```
linkspan
├── main.go                    # CLI flags, startup, shutdown
├── internal/
│   ├── httpapi/               # every route, handler and listener
│   └── workflow/              # YAML workflow: load and run shell.exec steps in order
└── subsystems/
    ├── metrics/               # cgroup-v2 + nvidia-smi job metrics
    ├── sshd/                  # supervised SSH server (gliderlabs/ssh) with PTY support
    └── tunnel/                # devtunnel CLI download + relay hosting
```

The subsystems report data and run processes; they know nothing about HTTP. `internal/httpapi` is the only
package that handles requests, and `main.go` is the only one that reads flags.

## Testing

```bash
go build ./...
go test ./...
go vet ./...
```

CI runs all three on every pull request, with `-race` on the tests. Tests sit beside what they test as
`*_test.go` and need no cluster, no network and no GPU.

## Releases

`make` cross-compiles into `bin/` for Linux and macOS on `amd64`/`arm64`. It refuses to build unless HEAD is
tagged `X.Y.Z` or `X.Y.Z.<commit>`, because the tag is the version the binary reports; use `go build` for a
development binary.

Publishing a GitHub release triggers GoReleaser, which builds and uploads the archives that clients download.
`goreleaser release --snapshot --clean` produces the same archives locally.

## Compatibility

CyberShuttle clients install and drive Linkspan over the following, so a change to any of them needs a
coordinated release of the clients too:

- `--version` prints a bare `X.Y.Z[.commit]` as the only line on stdout.
- `--help` lists `-tunnel-host-token`; a client greps for that literal before submitting a job.
- The release archive `linkspan_Linux_<arch>.tar.gz` contains a binary named `linkspan`.
- The four `/api/v1` paths, their response shapes, and the session id `s-<port>`.

The `sftp` subsystem and the `direct-streamlocal@openssh.com` channel handler have no caller in the
CyberShuttle clients, but they are not unused: VS Code Remote-SSH's bootstrap fallback uses SFTP, and its
`remote.SSH.remoteServerListenOnSocket` setting uses streamlocal.

## Pull Requests

- Branch off `main`; `main` takes changes only through pull requests.
- Keep the build, tests and vet green — CI runs them on every pull request.
- Add a test for behaviour that a reader could otherwise change by accident, and say in the description what
  you ran.
- Report security issues privately instead of opening a pull request. See [SECURITY.md](SECURITY.md).
