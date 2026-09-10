# Linkspan

Go agent that runs as the main process of an HPC batch job: hosts a client-owned Dev Tunnel, runs a
workflow on the job's lifecycle triggers, and serves job metrics and on-demand SSH, Jupyter and terminal
sessions over loopback HTTP. `README.md` is the user document; `CONTRIBUTING.md`, `SECURITY.md`,
`CHANGELOG.md` and `docs/COMPATIBILITY.md` are the others. Anything a human reader needs belongs in one of
them, not here.

## Commands

| Command | Purpose |
|---|---|
| `make check` | format, vet, lint, vulnerability scan, race tests; what CI runs |
| `make tools` | install the two pinned binaries `make check` needs |
| `go build -o linkspan .` | development binary; `make` refuses an untagged HEAD |
| `go test -run TestLayout .` | the declaration-order and doc-outline rules alone |

## Layout

- `main.go` reads flags, validates the workflow and the tunnel, binds the listeners and starts every
  service under tasks, the workflow's triggers included; its `subsystems` table is the one place a
  subsystem's `Router` and `Commands` are named, gated by its `Config`, a literal until a file loader
  replaces it.
- `internal/router` is a tree of routers, and every route is a command. A `Router` is a prefix and its
  `Routes`, `"METHOD path"` relative to the prefix, each naming a command; `New(prefix, routes)` writes
  them behind the prefix and `Routes` is the whole table, so a tree is built leaf first and one function
  is a route and a workflow action alike. The router decodes the body into the params map and adds the
  path id. It imports nothing of Linkspan's and knows no path: `main.go` owns `/api/v1`, health and
  metrics.
- `internal/tasks` is one registry of `Task`: a `Run` function with an id, kind, address, state, error
  and the caller's attrs, under a context. `Start` is its one way in: a task with an address is bound
  first and its `Run` serves the listener it finds on it, and a task stays listed after `Run` ends, as
  failed or exited, until `Stop` removes it; the registry entry changes in place, and `Start` and
  `Select` hand out copies, and a bind that fails is `Start`'s error. A task names its work as a `Run`,
  a `Server` served on the bound listener, or a `Spawn`, a child on the bound port that `exec.go`, the
  one fork path, prepares, probes until it accepts, and whose end `Start` records and never treats as
  fatal; a server or child binds loopback at any port unless an address is given. Kinds are declared by
  whoever starts the task. No restart policy. `tasks.go` is the model, the registry and a task's life;
  `exec.go` is the fork path.
- `internal/{metrics,sshd,tunnel,install}` are primitives: they report data, run SSH servers, host the
  relay, publish ports and say when the relay is hosting, and own `~/.cybershuttle`. `internal/sessions`
  is the list and stop commands over the registry that the session subsystems share.
- `subsystems/` are the capabilities a client drives. `workflow` loads a YAML of steps, each on a trigger,
  `start`, `ready`, `stop` or a signal, and each one action with its params or a `tasks` list of them;
  every task binds its command at load from the table main passes, the workflow's own `shell.exec`
  unprefixed and each enabled subsystem's `Commands` behind its name, called with the params. One document is loaded per
  process; `Run` runs one trigger's tasks, `Start` is the task main starts, the start tasks then the
  ready tasks once `tunnel.Ready` closes, and `WatchSignal` the task of one signal; main mounts no
  workflow router. Every subsystem exports `Commands`, its actions by name, and `Router`, whose every
  route names a `Commands` entry, so each command is a route and a workflow step alike;
  `TestRoutesCoverCommands` checks the two tables agree. Each owns its wire shapes. `jupyter` and `terminal` `Spawn` a task with a step that composes
  `install.Fetch` and `tunnel.Publish` before the command, `vscode` runs `sshd.New` as a task's `Server`,
  the three take their list and stop commands from `internal/sessions`,
  and `filesystem` declares mount, unmount, copy and sync as routes and commands that answer 501 until
  they do something.
- `tasks.Kind` classifies tasks on a separate axis: the HTTP listener is a task that is not a
  capability.

## Rules

- Flags, `--version` and `--help` output, the archive name, every `/api/v1` route and response shape, and
  the workflow document cs-control ships are contracts; read `docs/COMPATIBILITY.md` before changing any.
  `TestRoutesFollowConfig` checks that only an enabled subsystem answers.
- `TestLayout` enforces the file-layout rules in `CONTRIBUTING.md`, including the doc-comment outline, so
  every rename and every new declaration updates the outline.
- Every subprocess, server and goroutine that outlives a request is a `tasks.Task` started by its
  `Start`; every fork goes through `tasks.Exec`.
- A package exposes a few primitives and callers compose them at the point of use. A function whose body
  delegates to one primitive is a wrapper: delete it and name the primitive at the call site. A
  primitive's long-running work is a `Run`, `func(context.Context) error`: `metrics.Poll`, `Tunnel.Relay`,
  `workflow.Start`, or a function of its arguments returning one, `workflow.WatchSignal(name)`; a server
  is composed where it is started, `(&tasks.Task{Kind: kind, Server: sshd.New(key)}).Start()`, and a
  child server is a `Spawn` step in the same literal. Every subsystem declares `var Commands`, a map
  from a name such as `sessions.start` to a `router.Command`; every route of its `Router` names an entry
  of that map, and a workflow step calls the same command with the step's params, so one function
  answers both. A loop over a query belongs at the call site.
- An error that leaves a package before tasks is involved carries that package's prefix once, as in
  `workflow: read: ...`. An error from a running task carries none, because `Failed` prefixes the
  process id. `startAll` returns them unwrapped.
- Suppressions live in `.golangci.yml`, never as `//nolint`; `CONTRIBUTING.md` has the rule.
- Tests name coreutils by absolute path and the shell as `sh`.

## Load-bearing constructs

Each reads as redundancy and fixes a race that a test pins. Before removing one, run
`git log -S'<symbol>' --oneline` and read the commit that added it.

- **`StopAll` waits for the task** (tasks): a task is a context; `StopAll` cancels it and blocks on
  `done`. `Exec` waits for the child it killed, so the relay has exited before main `os.Exit`s. A task
  that ignores its context hangs shutdown. `TestStopAllWaitsForTheTask`,
  `TestStopAllWaitsForTheChild`, `TestStopAllKillsTheRelay`.
- **`Exec` kills the process group and bounds the pipe wait** (tasks): `Setpgid` lets cancellation
  reach a child's helpers; a `setsid` daemon, as cs-control's `setsid --fork` step and VS Code's server
  are, survives by design. `WaitDelay` closes the pipes `stdioGrace` after exit, so an orphan on stdout
  cannot hold a session or the probe open. `TestExecKillsTheGroup`, `TestExecOutlivesAnOrphanedPipe`.
- **The task's context ends with its `Run`** (tasks `Start`): the `cancel()` after `Run` returns is why
  every `AfterFunc` on it fires.
- **The metrics task waits for each probe** (metrics `Poll`): one nvidia-smi at a time, however long it
  takes, then the fixed pause. Cancellation kills a slow probe through `Exec`; a probe wedged in an
  uninterruptible driver call ignores SIGKILL and holds the task, and `StopAll` with it, by choice over
  leaving probes behind. `Latest` answers from the last sample at once. A missing `nvidia-smi` is an
  omitted field.
- **`Start` closes the listener, not only the server** (tasks): gliderlabs resets its done channel
  while it holds no listener, so a `Close` in that window is discarded and `Serve` accepts forever. A
  closed listener fails `Accept` regardless. `TestStartClosesTheListener`.
- **A repeated id cancels its predecessor, and only `Stop` removes an entry** (tasks `Start`):
  `s-<port>` ids repeat when a later session takes a freed port. Overwriting without cancelling left a
  running task no `StopAll` could reach; an ended task keeps its entry so its state can be read.
  `TestRepeatedIDReplaces`, `TestExitStaysListed`.
- **`guard` at every handler-table entry, the key callback and every goroutine** (sshd): gliderlabs
  spawns one goroutine per channel and another inside `DefaultSessionHandler` that a channel-level
  recover cannot reach. The pty and forwarding callbacks return constants and stay bare.
- **One fork path, four stdio wirings** (tasks `Exec`): workflow, the Jupyter build steps and server and
  ttyd pass Linkspan's `*os.File` descriptors through unchanged, tunnel a capped concurrently-readable
  buffer, sshd an `ssh.Session`, metrics a buffer read only after the probe's goroutine ends. `Exec`
  owns start, kill and wait; the wiring stays at each call site.
