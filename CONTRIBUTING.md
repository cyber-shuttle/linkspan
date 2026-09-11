# Contributing to Linkspan

Issues and pull requests go through [GitHub](https://github.com/cyber-shuttle/linkspan/issues). Branch off
`main`, keep CI passing, cover new behavior with a test, and state in the description what was run.
Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md). A pull request's column on the
group's project board follows its draft, review and merge state through
`.github/workflows/status-sync.yml`, which checks out and runs no repository code.

## Development Setup

Go 1.27+ is the only prerequisite.

```bash
git clone https://github.com/cyber-shuttle/linkspan.git
cd linkspan
go build -o linkspan .
```

The README [Quick Start](README.md#quick-start) follows from there. Without tunnel or workflow flags,
every enabled route answers on that loopback port.

## Source Layout

```
linkspan
├── main.go                    # CLI flags, startup, shutdown
├── main_test.go               # the surface docs/COMPATIBILITY.md freezes
├── layout_test.go             # the file-layout rules below
├── docs/COMPATIBILITY.md      # what clients depend on
├── docs/assets/               # the README's architecture diagram, architecture.mmd rendered to .png
├── examples/workflow.yml      # a workflow that exercises every trigger
├── internal/
│   ├── router/                # a tree of routers; main.go roots it at /api/v1
│   ├── tasks/                 # the task registry; Start is the one way in
│   ├── metrics/               # cgroup v2 + nvidia-smi job metrics
│   ├── sshd/                  # SSH server (gliderlabs/ssh)
│   ├── tunnel/                # relay hosting and tunnel port publishing
│   ├── install/               # ~/.cybershuttle: fetched binaries, uv, Python, the Jupyter environment
│   └── sessions/              # the list and stop commands the session subsystems share
└── subsystems/
    ├── workflow/              # YAML steps on lifecycle triggers; no routes
    ├── vscode/                # /api/v1/vscode/sessions: SSH servers for VS Code Remote-SSH
    ├── jupyter/               # /api/v1/jupyter/sessions: Jupyter sessions in a uv-built environment
    ├── terminal/              # /api/v1/terminal/sessions: ttyd web terminals
    └── filesystem/            # /api/v1/filesystem: mount, unmount, copy and sync, 501 placeholders
```

`internal/` is Linkspan's own infrastructure and its primitives, with no route of their own. `subsystems/`
are the capabilities a client drives: each exports `Commands`, its actions by name, and `Router`, a
`router.Router` at its own prefix whose every route names a `Commands` entry, so one function answers a
request and a workflow step. `TestRoutesCoverCommands` checks that the two tables agree. `main.go` mounts
the ones its `Config` enables into the `/api/v1` root, and is the only file that reads flags. The README's
[Architecture](README.md#architecture) section states the three ideas the code is built on.

Adding a subsystem is one package that exports the two tables and one line in `main.go`'s `subsystems`
map. Adding an action to one is one function and one entry in each table. A subsystem that appears in the
README's diagram is added to `docs/assets/architecture.mmd`, rendered with
`mmdc -i docs/assets/architecture.mmd -o docs/assets/architecture.png -b white -s 2 -w 1600`.

## File Layout

Go fixes no declaration order, so this repository picks one and enforces it in `TestLayout`
(`layout_test.go`):

1. The doc comment attached to the package clause names every top-level declaration, methods included.
   `Name*` covers a family, and a fake's interface methods are covered by its type's entry.
2. A const, var or type used by two or more functions sits above the first function.
3. A method is declared after the type it is on.
4. Every unexported function precedes every exported one, a method taking its receiver's visibility.
5. A function is declared after every function, type, const and var of the file that it names.
6. The doc comment names them in the order the file declares them.
7. Every doc-comment entry names something the file declares.

Files read bottom-up, with primitives first and the surface built on them last. An outline entry carries
only what the code cannot say, which is the reason, contract or client behind a name. A name whose signature
says it all stands alone. Names on an entry line end at the first double space, and comment lines run to
120 columns.

## Checks

`make check` runs what CI runs, fastest gate first. `make tools` installs the two pinned binaries it needs.

| Step | Command | Scope |
|---|---|---|
| Format | `golangci-lint fmt --diff` | gofmt, through the linter so there is one binary to install |
| Vet | `go vet ./...` | the toolchain's own checks |
| Lint | `golangci-lint run ./...` | the standard set plus the linters `.golangci.yml` enables |
| Vulnerabilities | `govulncheck ./...` | symbol-level, so only advisories the code can reach |
| Test | `go test -race ./...` | tests sit beside what they test and need no cluster, network or GPU |

Suppressions live in `.golangci.yml`, never as `//nolint` comments, and each names the codes it answers
and why they cannot be fixed at the root. golangci-lint reports one issue per line, so a gosec taint code
(G702, G703) surfaces only once the code sharing its line is excluded.

### Testing SSH authorization by hand

A rejection test against a session's SSH server passes falsely when `~/.ssh/config` sets
`ControlMaster auto` for `Host *`: the first connection with the good key opens a shared master, and a
second connection with a bad key reuses it without authenticating. Disable sharing and restrict the key:

```bash
ssh -o ControlMaster=no -o ControlPath=none -o IdentitiesOnly=yes -o BatchMode=yes -i badkey -p <bind_port> user@127.0.0.1
```

`IdentitiesOnly=yes` matters on its own, because without it ssh also offers agent keys and every
`~/.ssh/id_*`.

## Releases

Add the version's entry to [CHANGELOG.md](CHANGELOG.md), push the tag `vX.Y.Z`, then publish the GitHub
release for that tag. Publishing triggers `.github/workflows/goreleaser.yml`, which builds and uploads the
archives clients download. The same workflow dry-runs a snapshot on every pull request.

`make` cross-compiles into `bin/` for Linux and macOS on `amd64` and `arm64`. It refuses to build unless
HEAD is tagged `vX.Y.Z`, optionally with a `.<commit>` suffix, because the tag is the version the binary
reports with the leading `v` stripped. Use `go build` for a development binary.

## Compatibility

Flags, `--version` and `--help` output, the release archive name and the `/api/v1` surface are contracts
with clients that ship separately. [docs/COMPATIBILITY.md](docs/COMPATIBILITY.md) states them and is read
before changing any.
