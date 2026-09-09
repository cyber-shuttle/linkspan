# Linkspan

Go agent that runs as the main process of an HPC batch job: hosts a client-owned Dev Tunnel, runs a
startup workflow, and serves job metrics and on-demand SSH servers over loopback HTTP. `README.md` is the
user document; `CONTRIBUTING.md`, `SECURITY.md` and `docs/COMPATIBILITY.md` are the others. Anything a
human reader needs belongs in one of them, not here.

## Commands

| Command | Purpose |
|---|---|
| `make check` | format, vet, lint, vulnerability scan, race tests; what CI runs |
| `make tools` | install the two pinned binaries `make check` needs |
| `go build -o linkspan .` | development binary; `make` refuses an untagged HEAD |
| `go test -run TestLayout .` | the declaration-order and doc-outline rules alone |

## Layout

- `main.go` reads flags, constructs the listeners, the workflow and the tunnel, then starts each.
- `internal/httpapi` is the only package that handles requests; it owns every wire shape.
- `internal/procmgr` starts, stops and queries every process; no restart policy.
- `internal/workflow` loads and runs `shell.exec` steps in order.
- `subsystems/{metrics,sshd,tunnel}` are capabilities hosted for the client; none handles a request.
- `procmgr.Kind` classifies processes on a separate axis: the HTTP listener is a process that is not a
  capability.

## Rules

- Flags, `--version` and `--help` output, the archive name, and every `/api/v1` route and response shape
  are contracts; read `docs/COMPATIBILITY.md` before changing any.
- `TestLayout` enforces the file-layout rules in `CONTRIBUTING.md`, including the doc-comment outline, so
  every rename and every new declaration updates the outline.
- Every subprocess, server and goroutine that outlives a request runs under `procmgr.Start`, except the
  short-lived metrics probe; every fork, the probe included, goes through `procmgr.Exec`.
- A package exposes a few primitives and callers compose them at the point of use. A function whose body
  delegates to one primitive is a wrapper: delete it and name the primitive at the call site. The one
  exception is a package's own `Start`, which owns its procmgr kind and id. A loop over a query belongs at
  the call site.
- An error that leaves a package before procmgr is involved carries that package's prefix once, as in
  `workflow: read: ...`. An error from a running task carries none, because `Failed` prefixes the
  process id. main returns errors unwrapped.
- Suppressions live in `.golangci.yml`, never as `//nolint`; `CONTRIBUTING.md` has the rule.
- Tests name coreutils by absolute path and the shell as `sh`.

## Load-bearing constructs

Each reads as redundancy and fixes a race that a test pins. Before removing one, run
`git log -S'<symbol>' --oneline` and read the commit that added it.

- **`StopAll` waits for the task** (procmgr): a process is a context; `StopAll` cancels it and blocks on
  `done`. `Exec` waits for the child it killed, so the relay has exited before main `os.Exit`s. A task
  that ignores its context hangs shutdown. `TestStopAllWaitsForTheTask`,
  `TestStopAllWaitsForTheChild`, `TestStopAllKillsTheRelay`.
- **`Exec` kills the process group and bounds the pipe wait** (procmgr): `Setpgid` lets cancellation
  reach a child's helpers; a `setsid` daemon, as cs-control's `setsid --fork` step and VS Code's server
  are, survives by design. `WaitDelay` closes the pipes `stdioGrace` after exit, so an orphan on stdout
  cannot hold a session or the probe open. `TestExecKillsTheGroup`, `TestExecOutlivesAnOrphanedPipe`.
- **The process context ends with the task** (procmgr `Start`): the `cancel()` after the task returns is
  why `Serve`'s `AfterFunc` always fires.
- **The probe runs outside procmgr behind a flag, and its error is ignored** (metrics `Collect`): a probe
  wedged in an uninterruptible driver call ignores SIGKILL, so `Wait` blocks regardless of context. The
  `probing` flag is held until `Wait` returns, so a stuck probe blocks no later call and starts no second
  one, and `StopAll` does not wait for it. A missing `nvidia-smi` is an omitted field. The test fakes the
  wedge with a stdout holder forked into a new session.
- **`Serve` closes the listener, not only the server** (procmgr): gliderlabs resets its done channel
  while it holds no listener, so a `Close` in that window is discarded and `Serve` accepts forever. A
  closed listener fails `Accept` regardless. `TestServeReturnsOnStopAll`.
- **A repeated id cancels its predecessor, and the entry leaves by identity** (procmgr `Start`):
  `s-<port>` ids repeat when a later session takes a freed port. Deleting by id evicted live sessions;
  overwriting without cancelling left a running process no `StopAll` could reach. `TestRepeatedIDReplaces`.
- **`guard` at every handler-table entry, the key callback and every goroutine** (sshd): gliderlabs
  spawns one goroutine per channel and another inside `DefaultSessionHandler` that a channel-level
  recover cannot reach. The pty and forwarding callbacks return constants and stay bare.
- **One fork path, four stdio wirings** (procmgr `Exec`): workflow passes `*os.File` descriptors through
  unchanged, tunnel a capped concurrently-readable buffer, sshd an `ssh.Session`, metrics a buffer read
  only after the probe's goroutine ends. `Exec` owns start, kill and wait; the wiring stays at each call site.
