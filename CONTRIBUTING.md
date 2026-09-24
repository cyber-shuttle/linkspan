# Contributing to Linkspan

Issues and pull requests go through [GitHub](https://github.com/cyber-shuttle/linkspan/issues). Branch off
`main`, keep CI passing, cover new behavior with a test, and state in the description what was run. Participation
follows the [Code of Conduct](CODE_OF_CONDUCT.md).

## Development Setup

Go 1.27+ is the only prerequisite.

```bash
git clone https://github.com/cyber-shuttle/linkspan.git
cd linkspan
go build -o linkspan .
```

Then follow the README [Quick Start](README.md#quick-start); without tunnel or workflow flags, every enabled route
answers on the loopback port.

## Source Layout

```
linkspan
├── main.go                    # CLI flags, startup, shutdown
├── main_test.go               # the surface docs/COMPATIBILITY.md freezes
├── layout_test.go             # the file-layout rules below
├── docs/COMPATIBILITY.md      # what clients depend on
├── docs/assets/               # architecture.mmd, rendered to the README's architecture.png
├── examples/                  # workflow.yml; checkpoint.yml, restore.yml and checkpoint.sh, which runs them
├── Makefile                   # make check, make tools, and the tagged cross-compile
├── .golangci.yml              # the linters and the one place suppressions live
├── .goreleaser.yaml           # the release archives
├── internal/
│   ├── router/                # a tree of routers; main.go roots it at /api/v1
│   ├── tasks/                 # the task registry and the one fork path
│   ├── sessions/              # what the session subsystems share, and a process session's life
│   ├── metrics/               # cgroup v2 + nvidia-smi job metrics
│   ├── sshd/                  # SSH server (gliderlabs/ssh)
│   ├── tunnel/                # hosting the delegated Dev Tunnel
│   ├── forward/               # /api/v1/forward/{port}
│   └── install/               # ~/.cybershuttle: fetched binaries, uv, Python, the Jupyter environment
└── subsystems/
    ├── workflow/              # /api/v1/workflow/shell/exec, and the --workflow document
    ├── vscode/                # /api/v1/vscode/sessions
    ├── jupyter/               # /api/v1/jupyter/sessions and /setup
    ├── terminal/              # /api/v1/terminal/sessions
    ├── checkpoint/            # /api/v1/checkpoint/pause and /resume
    └── filesystem/            # /api/v1/filesystem/{mount,unmount,copy,sync}, answering 501
```

`internal/` holds primitives with no routes of their own. Each package under `subsystems/` exports `Commands`, its
actions by name, and `Router`, a `router.Router` at its own prefix whose every route names a `Commands` entry;
`TestRoutesCoverCommands` checks that every command is routed. `main.go` mounts the subsystems its `config` enables
under `/api/v1` and is the only file that reads flags.

| To add | Change |
|---|---|
| A subsystem | One package exporting both tables, and one line in `main.go`'s `subsystems` map |
| An action | One function, and one entry in each table |
| A subsystem to the diagram | `docs/assets/architecture.mmd`, then `mmdc -i docs/assets/architecture.mmd -o docs/assets/architecture.png -b white -s 2 -w 1600` |

## File Layout

`TestLayout` (`layout_test.go`) enforces one declaration order on every Go file:

1. The doc comment attached to the package clause names every top-level type, function and method. A
   const or var may be named.
   `Name*` covers a family, and a fake's interface methods are covered by its type's entry.
2. A const, var or type used by two or more functions sits above the first function.
3. A method is declared after the type it is on.
4. Every unexported function precedes every exported one, a method taking its receiver's visibility.
5. A function is declared after every function, type, const and var of the file that it names.
6. The doc comment names them in the order the file declares them.
7. Every doc-comment entry names something the file declares.

Files read bottom-up: primitives first, the surface last. An outline entry carries only what the code cannot say,
the reason, contract or client behind a name; a name whose signature says it all stands alone. Names on an entry
line end at the first double space, and comment lines run to 120 columns.

## Checks

`make check` runs what CI runs, fastest gate first; `make tools` installs its two pinned binaries.

| Step | Command | Scope |
|---|---|---|
| Format | `golangci-lint fmt --diff` | gofmt |
| Vet | `go vet ./...` | the toolchain's checks |
| Lint | `golangci-lint run ./...` | the standard set plus the linters `.golangci.yml` enables |
| Vulnerabilities | `govulncheck ./...` | advisories the code can reach |
| Test | `go test -race ./...` | no cluster, network or GPU needed |

Suppressions live in `.golangci.yml`, never as `//nolint`, each naming its codes and why the root cannot be fixed.
golangci-lint reports one issue per line, so a gosec taint code (G702, G703) surfaces only once the code sharing
its line is excluded.

### Testing SSH authorization by hand

With `ControlMaster auto` in `~/.ssh/config`, a bad key reuses the good key's master connection and a rejection test
passes falsely. Without `IdentitiesOnly=yes`, ssh also offers agent keys and every `~/.ssh/id_*`. Use:

```bash
ssh -o ControlMaster=no -o ControlPath=none -o IdentitiesOnly=yes -o BatchMode=yes -i badkey -p <bind_port> user@127.0.0.1
```

## Releases

1. Add the version's entry to [CHANGELOG.md](CHANGELOG.md).
2. Push the tag `vX.Y.Z`.
3. Publish the GitHub release for that tag; `.github/workflows/on-release.yml` then builds and uploads the archives.

`.github/workflows/on-pr-and-main.yml` runs `make check` on every pull request and push to `main`. `make`
cross-compiles into `bin/` for Linux and macOS on `amd64` and `arm64`, and refuses unless HEAD is tagged `vX.Y.Z`
or `vX.Y.Z.<commit>`, since the tag without `v` is the version the binary reports. Use `go build` for a
development binary.
