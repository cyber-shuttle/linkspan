# Contributing to Linkspan

Issues and pull requests go through [GitHub](https://github.com/cyber-shuttle/linkspan/issues); branch off
`main`, keep CI green, and cover new behaviour with a test, stating in the description what you ran.
Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md).

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

Add the version's entry to [CHANGELOG.md](CHANGELOG.md), push the tag `vX.Y.Z`, then publish the GitHub
release for that tag. Publishing triggers `.github/workflows/goreleaser.yml`, which builds and uploads the
archives clients download; the same workflow dry-runs a snapshot build on every pull request.

`make` cross-compiles into `bin/` for Linux and macOS on `amd64`/`arm64`. It refuses to build unless HEAD is
tagged `vX.Y.Z`, optionally with a `.<commit>` suffix for a build between releases, because the tag is the
version the binary reports; the leading `v` is stripped, so `v0.17.4` reports `0.17.4`. Use `go build` for a
development binary.

## Compatibility

Flags, `--version`/`--help` output, the release archive name and the `/api/v1` surface are contracts with
clients that ship separately; see [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md) before changing any of them.
