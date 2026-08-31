# Contributing to Linkspan

This document covers the source layout, how to get a development build running, and the contracts a change
must not break. Issues and pull requests go through GitHub; branch off `main`, keep CI green, and cover new
behaviour with a test, stating in the description what you ran.

## Development Setup

### Prerequisites

- [Git](https://git-scm.com/)
- [Go](https://go.dev/dl/) 1.24+

Linkspan has no code generation step, no C dependencies and no service to run locally.

### Getting Started

```bash
git clone https://github.com/cyber-shuttle/linkspan.git
cd linkspan

go build -o linkspan .
```

Then follow the README [Quick Start](README.md#quick-start); with no tunnel or workflow flags every route in
the [HTTP API](README.md#http-api) table answers on that loopback port.

## Source Layout

```
linkspan
├── main.go                    # CLI flags, startup, shutdown
├── internal/
│   ├── httpapi/               # every route, handler and listener
│   └── workflow/              # YAML workflow: load and run shell.exec steps in order
└── subsystems/
    ├── metrics/               # cgroup-v2 + nvidia-smi job metrics
    ├── sshd/                  # supervised SSH server (gliderlabs/ssh)
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

CyberShuttle clients install and drive Linkspan over the flags, `--version`/`--help` output, release archive
name, `/api/v1` routes and response shapes listed under **Consumer contracts** in [CLAUDE.md](CLAUDE.md);
changing any of them needs a coordinated client release.
