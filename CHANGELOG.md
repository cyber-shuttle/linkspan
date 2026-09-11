# Changelog

Notable changes to Linkspan. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0/).

## [Unreleased]

### Added

- `/api/v1/jupyter/sessions`: Jupyter sessions created, listed and stopped by Linkspan, in an
  environment it builds with `uv` under `~/.cybershuttle`, with a token it mints. This replaces the
  workflow cs-control shipped for the same purpose.
- `/api/v1/terminal/sessions`: web terminals served by `ttyd`, fetched on first use. The subsystem is off
  as shipped.
- Workflow triggers: a step's `on` is `start`, `ready`, `stop`, or `SIGUSR1`, `SIGUSR2` or `SIGHUP`, so a
  workflow bootstraps a workspace once the tunnel is hosting, checkpoints before Slurm's time limit and
  syncs on exit. A step without `on` runs on `start`, as before.
- Workflow actions beyond `shell.exec`: every subsystem command, `vscode.sessions.select` and `.start`,
  `jupyter.setup`, `jupyter.sessions.select`, `.start` and `.stop`, `terminal.sessions.select`, `.start`
  and `.stop`, and the `filesystem` placeholders, with `params` as the request body, so VS Code and
  Jupyter are bootstrapped from the file. A step's `tasks` is a list of them under one `on`.
- `examples/workflow.yml`, a workflow that exercises each kind of trigger against a local Linkspan.
- Every subsystem command is a route, so `POST /api/v1/jupyter/setup` builds the environment ahead of
  the first server and the `/filesystem` placeholders answer `501` where enabled, `404` as shipped.
- `jupyter.sessions.start` takes `addr` and `token`, and takes the token from `JUPYTER_TOKEN` when given
  none, so cs-control's workflow is one step that reuses Linkspan's server.
- Every listed process is one object: `id`, `addr`, `state`, `error` and its own fields. A VS Code
  session therefore also carries `error`.
- A Jupyter server or terminal created while a tunnel is hosted is added to the tunnel, with the
  `--tunnel-host-token`, for as long as it runs, and answered with its public URL.
- `make check`: format, vet, lint, vulnerability scan and race tests, as CI runs them.
- `TestLayout` enforces the declaration order and outline that `CONTRIBUTING.md` describes.
- `docs/COMPATIBILITY.md` states the exit status an SSH session reports.

### Changed

- A Jupyter server's tunnel port is anonymous, its token being the credential, so a browser reaches it;
  a terminal's port still needs the tunnel owner's sign-in.
- `metrics`, `sshd` and `tunnel` moved to `internal/` as primitives.
- `subsystems/` now holds `workflow`, `vscode`, `terminal`, `jupyter` and `filesystem`.
- A `Config` in `main.go` to turn subsystems on/off.
- `internal/tasks` is one registry: a `Task` is a `Run` under a context with a state, listed until
  stopped even after `Run` ends. The launch gate, published stop and liveness probe are removed; a panic
  in a task is its error. `Start` is the one way in and binds the address a task names; a task's
  `Server` is an in-process server on that listener, and its `Spawn` step an external one.
- `internal/router` is routers only; a `tasks.Task` binds and serves the tree. Each subsystem
  exports `Commands`, one table of the commands behind its routes and the workflow's actions.
- Every server, the relay and the workflow are tasks started by `Start`; every child process is forked
  through `tasks.Exec`.
- A child process runs in its own process group, killed with it; a `setsid` daemon is left alone. Its
  pipes close two seconds after it exits, so an orphan cannot hold an SSH session or the probe open.
- Metrics are sampled whole by the `metrics` task, one `nvidia-smi` at a time with a 5s pause between;
  `/metrics` answers the last sample and never waits on anything.
- Waits have no deadlines: the relay runs until it exits, a server is `starting` until its port accepts,
  and a download runs to completion. Cancellation ends each.
- A malformed workflow is refused before any listener binds.
- Log lines carry a package prefix; a fatal error names its process once.
- An SSH session runs its command, and the commands on its stdin, through `sh`; the stdin path ran
  `$SHELL`.
- `POST /api/v1/vscode/sessions` refuses a key line with `authorized_keys` options, once ignored, and
  answers `413` over 64KB, once `400`.
- `GET /api/v1/vscode/sessions` orders by id.
- `--socket` unlinks a stale socket only; a regular file at the path fails the bind, once deleted.
- The README lists every response body; `SECURITY.md` states which listener assumes an exclusive node.
- The README introduces Linkspan, shows its architecture, and describes the control plane it provides.

### Fixed

- A relay that exited with status zero was reported with a nil cause.
- A relay killed for not reporting ready was reported as exited.
- A relay printing more than 64KB before its ready line was killed at the timeout.
- The router and `sshd` packages had no package documentation, a blank line detaching the comment;
  `TestLayout` now checks attachment.

## [0.17.5] - 2026-09-07

### Added

- `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `CHANGELOG.md` and `docs/COMPATIBILITY.md`, which
  states the flags, routes, response shapes and `--version` output the CyberShuttle clients depend on.

### Changed

- The README describes the routes that exist and the binaries Linkspan execs. It advertised endpoints for
  running scripts, stopping subprocesses and opening tunnels, none of which are routes, and claimed no runtime
  dependencies although the `devtunnel` CLI is fetched on first use and `nvidia-smi` is executed.
- The README names the cluster, network and home-directory requirements a site needs in order to run Linkspan.

### Removed

- The interactive PTY shell. An SSH session runs commands and serves SFTP; PTY allocation is now refused.
- Reverse port forwarding. Local TCP and unix-socket forwarding are unchanged.
- The TCP keep-alive set on accepted SSH connections.

### Fixed

- The `LICENSE` copyright line, which still carried the Apache-2.0 placeholder text.

## [0.17.4] - 2026-08-25

Documentation only. Source comments and project notes were corrected; nothing the binary does changed.

## [0.17.3] - 2026-08-24

### Changed

- `make` rebuilds when the sources change, so it no longer ships a binary built at an older tag, and release
  artifacts are built with the Go version `go.mod` names.

### Fixed

- A workflow step that daemonizes no longer stalls the workflow, and step output is written as it happens
  rather than buffered until the step ends.
- The devtunnel relay is no longer left running when Linkspan exits during bring-up or after a failed
  attempt.
- Relay failure is decided by the process exiting rather than by the word `Warning` in its output.
- `GET /api/v1/metrics` can no longer hang on a wedged GPU; the 3s bound announced in 0.17.2 did not hold,
  and only one probe may now be outstanding.
- `ssh -tt <host> <command>` no longer wedges on the first terminal resize.
- Interactive shells receive the client's `TERM`, a shell's last output is no longer truncated, and a session
  that ends now ends its shell.

### Security

- The `--socket` unix socket is set to mode `0600` after bind instead of taking the umask.

## [0.17.2] - 2026-08-24

### Fixed

- PTY sessions report the command's real exit status; they previously always reported 0.
- `GET /api/v1/metrics` bounds the `nvidia-smi` probe at 3s and cancels it when the client disconnects.
- The SSH port returned in a `201` is bound before it is returned, so it is already accepting.

### Security

- The HTTP API binds `127.0.0.1` rather than `0.0.0.0`. Any host able to route to the compute node could
  previously post a public key to `POST /api/v1/vscode/sessions` and obtain a shell as the job's user. A
  process owned by another user on the same node still can; closing that needs a credential on the request.

## [0.17.1] - 2026-08-24

### Changed

- `POST /api/v1/vscode/sessions` caps the request body at 64KB.

### Fixed

- Remote commands report their real exit status; every command previously returned 0 with the error written
  into its own stdout.
- A finished command no longer hangs the session when the client keeps its stdin open.
- A dropped connection no longer leaves the shell or command it spawned running for the rest of the
  allocation.
- A failed devtunnel attempt kills its relay, so a retry no longer starts a second one on the same tunnel.
- Shutdown runs on every exit path, so a failing workflow or an exhausted tunnel still stops the relay and
  the SSH sessions.
- The devtunnel CLI download is bounded and cancelable.
- A dead HTTP server exits 1 rather than 0.
- `--help` prints `-socket string` instead of mis-rendering the flag's usage text.

## [0.17.0] - 2026-08-21

### Removed

- The FUSE overlay filesystem and the VFS subsystem, with `--vfs-mode` and `--vfs-session-id`.
- The Jupyter kernel manager and its `/api/v1/jupyter/kernels` routes.
- The FRP tunnel backend, the Dev Tunnels SDK and the tunnel-provider abstraction over them.
- The `/api/v1/tunnels`, `/api/v1/metadata` and `/api/v1/status` routes, and the log broadcaster.
- Eleven of the twelve workflow actions; `shell.exec` is the only one left.
- `--tunnel-api`, `--host`, `--tunnel-retries`, `--tunnel-retry-delay`, `--tunnel-attempt-timeout` and
  `--verbose-version`.
- `mount_user_home` on `POST /api/v1/vscode/sessions`, and `active`, `restarts` and `last_error` from a
  session status.

## [0.16.0] - 2026-08-21

### Added

- `--tunnel-host-token`. `--tunnel-enable` now requires `--tunnel-id`, `--tunnel-cluster` and
  `--tunnel-host-token` together.

### Changed

- `POST /api/v1/vscode/sessions` requires the caller's `authorized_key` and returns neither a password nor a
  private key. This breaks clients that do not send one.
- `make` refuses to build an untagged commit.

### Removed

- `--tunnel-auth-token`, so no Microsoft Entra bearer token reaches the compute node.
- Password authentication on the SSH server.

### Security

- Access tokens are kept out of the CLI logs and out of `GET /api/v1/tunnels/devtunnels`.

## [0.15.12] - 2026-07-15

### Added

- `GET /api/v1/metrics`, reporting live job resource metrics.

## [0.15.11] - 2026-07-15

### Added

- `--socket <path>` serves the HTTP API on a unix domain socket alongside the TCP port, reachable in-cluster
  with `srun --jobid --overlap`.

## [0.15.10] - 2026-07-06

### Added

- `direct-streamlocal@openssh.com` channels, so VS Code Remote-SSH works with
  `remote.SSH.remoteServerListenOnSocket`.

## [0.15.9] - 2026-06-25

### Fixed

- Panics in the SSH session and SFTP handlers are isolated. The recover added in 0.15.8 could not reach the
  goroutine those handlers run on.

## [0.15.8] - 2026-06-25

### Added

- The VS Code SSH listener restarts after a fatal exit, per-connection panics are isolated, and session
  status reports real liveness.

## [0.15.7] - 2026-06-16

### Fixed

- Hosting a client-created tunnel uses its cluster-qualified id, which fixes a `Login required` failure.

## [0.15.6] - 2026-06-15

### Added

- `--tunnel-cluster`, which resolves a client-created tunnel at its own cluster endpoint rather than the
  global one and fixes an HTTP 404 when hosting it.

## [0.15.5] - 2026-06-15

### Added

- `--tunnel-id` hosts a dev tunnel the client created, and never deletes it. Without the flag Linkspan
  creates and hosts its own as before.

## [0.15.4] - 2026-06-15

### Changed

- The API, log and SSH ports share one dev tunnel; a new port is forwarded onto the running host instead of
  creating a second tunnel. Needs the matching client change.

## [0.15.3] - 2026-06-11

### Added

- OS TCP keepalive on the VS Code SSH daemon, so quiet sessions survive at the transport layer.

## [0.15.2] - 2026-04-11

### Added

- `POST /api/v1/vscode/sessions` returns a private key for joining the session.

## [0.15.1] - 2026-03-30

### Changed

- `POST /api/v1/tunnels/devtunnels/forward` answers `201` instead of `200`.

## [0.15.0] - 2026-03-30

### Added

- `POST /api/v1/tunnels/devtunnels/forward`, which forwards a port on an existing dev tunnel.

## [0.14.13] - 2026-03-28

### Added

- The dev tunnel's forwarded ports and cluster id are logged once the tunnel is up.

## [0.14.12] - 2026-03-28

### Added

- Password authentication on the VS Code SSH session, restored.

## [0.14.11] - 2026-03-28

### Changed

- Dev Tunnels operations go through the service's HTTP API instead of the SDK calls they replaced.

## [0.14.10] - 2026-03-25

### Added

- `POST /api/v1/tunnels/devtunnels/auth-token`, which refreshes the Dev Tunnels auth token.

## [0.14.9] - 2026-03-24

No change. The tag points at the same commit as 0.14.8.

## [0.14.8] - 2026-03-24

### Changed

- A failing workflow exits Linkspan instead of leaving it running.

## [0.14.7] - 2026-03-23

### Changed

- `--version` prints the bare version, without the `linkspan ` prefix.

## [0.14.6] - 2026-03-23

### Added

- `--version` and `--verbose-version`. Release builds stamp the tag, commit and build date into the binary.

## [0.14.5] - 2026-03-10

### Fixed

- FUSE attributes report a Unix `mode_t` rather than Go's `FileMode`.

## [0.14.4] - 2026-03-08

### Fixed

- SSH exec runs the client's raw command string instead of one reassembled from split arguments.

## [0.14.3] - 2026-03-08

### Fixed

- SSH exec joins command arguments the way OpenSSH does.

## [0.14.2] - 2026-03-08

### Fixed

- Overlay SSH port mapping, connection retry and error reporting.

## [0.14.1] - 2026-03-08

### Added

- A tunnel-provider registry behind generic `/api/v1/tunnels` routes, with the dev tunnel and FRP backends
  registered as providers.

## [0.14.0] - 2026-03-08

### Added

- The overlay filesystem reconnects its SFTP session automatically.

## [0.13.2] - 2026-03-08

### Fixed

- FUSE mount cleanup falls back to a forced unmount when the normal one fails.

## [0.13.1] - 2026-03-08

### Fixed

- A stale FUSE mount is cleared before the overlay is set up.

## [0.13.0] - 2026-03-08

### Changed

- The overlay is a pure-Go userspace FUSE filesystem, so sshfs and fuse-overlayfs are no longer needed.

### Removed

- Windows release archives.

## [0.12.1] - 2026-03-08

### Changed

- The overlay runs on sshfs and fuse-overlayfs, downloaded on first use when absent.

## [0.12.0] - 2026-03-07

### Added

- A TCP listener that streams Linkspan's log output to connected clients; its port is exposed to workflows as
  `LogPort`.

## [0.11.0] - 2026-03-07

### Added

- `GET /api/v1/metadata` and `GET`/`PUT /api/v1/metadata/{key}`, an in-memory key-value store.
- An overlay mount whose lower layer is the local Linkspan origin.

## [0.10.0] - 2026-03-07

### Added

- `GET /api/v1/health`, and `GET /api/v1/status` reporting workflow progress.

## [0.9.2] - 2026-03-07

### Fixed

- The devtunnel CLI hosts with the host token and without `-p` flags.

## [0.9.1] - 2026-03-07

### Fixed

- The devtunnel CLI hosts with a token carrying the manage scope.

## [0.9.0] - 2026-03-07

### Changed

- Creating and hosting a dev tunnel is one operation, taking the server port.

## [0.8.0] - 2026-03-07

### Changed

- The dev tunnel lifecycle is split into create, host and forward phases.

## [0.7.0] - 2026-03-07

### Added

- A VFS subsystem with sync and mount providers, selected by `--vfs-mode` and `--vfs-session-id` or the
  `CS_VFS_MODE` and `CS_SESSION_ID` environment variables.

### Removed

- The FUSE subsystem the VFS replaces, and the `/api/v1/fuse` and `/api/v1/fs` routes.

## [0.6.2] - 2026-03-04

### Fixed

- `tunnel.devtunnel_connect` uses the cluster-qualified tunnel id.

## [0.6.1] - 2026-03-03

### Added

- The `tunnel.devtunnel_connect` workflow action.

## [0.6.0] - 2026-03-02

### Added

- `--mount-remote`, with `--session-id` and `--server-addr`, mounts a published folder locally and blocks
  until interrupted: over NFS on macOS, FUSE on Linux.

## [0.5.1] - 2026-03-01

### Added

- `POST /api/v1/fuse/start-server`, which starts the FUSE server on demand.

### Fixed

- NFS readdir returned empty listings on macOS.

## [0.5.0] - 2026-03-01

### Added

- A FUSE subsystem serving a filesystem over TCP, with `POST /api/v1/fuse/mount-remote` and
  `GET /api/v1/fuse/status`.

## [0.4.0] - 2026-02-27

### Added

- `--workflow`, running an ordered list of steps given in YAML at startup.
- `--tunnel-auth-token`, and dev tunnel operations backed by the Dev Tunnels SDK.

### Removed

- Password authentication on the VS Code SSH server.

## [0.3.0] - 2026-02-07

### Added

- `--host` and `--port` control where the HTTP server binds; it was fixed at `:8080`.
- SSH port forwarding on the VS Code SSH server.

### Changed

- `--tunnel-enable` defaults to off.

## [0.2.0] - 2026-02-07

### Added

- An SSH server for VS Code sessions, and `GET /api/v1/vscode/sessions/{id}/status`.
- An FRP tunnel backend, with `/api/v1/tunnels/frp` routes.

### Changed

- The project is named Linkspan; it was previously Conduit.

## [0.1.0] - 2026-01-06

### Added

- First release. Serves `/api/v1` on port 8080 with Jupyter kernel, VS Code session, filesystem and dev
  tunnel routes, and opens a dev tunnel at startup unless `--tunnel-enable=false`.
- GoReleaser archives for Linux, macOS and Windows.

[Unreleased]: https://github.com/cyber-shuttle/linkspan/compare/v0.17.5...HEAD
[0.17.5]: https://github.com/cyber-shuttle/linkspan/compare/v0.17.4...v0.17.5
[0.17.4]: https://github.com/cyber-shuttle/linkspan/compare/v0.17.3...v0.17.4
[0.17.3]: https://github.com/cyber-shuttle/linkspan/compare/v0.17.2...v0.17.3
[0.17.2]: https://github.com/cyber-shuttle/linkspan/compare/v0.17.1...v0.17.2
[0.17.1]: https://github.com/cyber-shuttle/linkspan/compare/v0.17.0...v0.17.1
[0.17.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.16.0...v0.17.0
[0.16.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.15.12...v0.16.0
[0.15.12]: https://github.com/cyber-shuttle/linkspan/compare/v0.15.11...v0.15.12
[0.15.11]: https://github.com/cyber-shuttle/linkspan/compare/v0.15.10...v0.15.11
[0.15.10]: https://github.com/cyber-shuttle/linkspan/compare/v0.15.9...v0.15.10
[0.15.9]: https://github.com/cyber-shuttle/linkspan/compare/v0.15.8...v0.15.9
[0.15.8]: https://github.com/cyber-shuttle/linkspan/compare/v0.15.7...v0.15.8
[0.15.7]: https://github.com/cyber-shuttle/linkspan/compare/v0.15.6...v0.15.7
[0.15.6]: https://github.com/cyber-shuttle/linkspan/compare/v0.15.5...v0.15.6
[0.15.5]: https://github.com/cyber-shuttle/linkspan/compare/v0.15.4...v0.15.5
[0.15.4]: https://github.com/cyber-shuttle/linkspan/compare/v0.15.3...v0.15.4
[0.15.3]: https://github.com/cyber-shuttle/linkspan/compare/v0.15.2...v0.15.3
[0.15.2]: https://github.com/cyber-shuttle/linkspan/compare/v0.15.1...v0.15.2
[0.15.1]: https://github.com/cyber-shuttle/linkspan/compare/v0.15.0...v0.15.1
[0.15.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.14.13...v0.15.0
[0.14.13]: https://github.com/cyber-shuttle/linkspan/compare/v0.14.12...v0.14.13
[0.14.12]: https://github.com/cyber-shuttle/linkspan/compare/v0.14.11...v0.14.12
[0.14.11]: https://github.com/cyber-shuttle/linkspan/compare/v0.14.10...v0.14.11
[0.14.10]: https://github.com/cyber-shuttle/linkspan/compare/v0.14.9...v0.14.10
[0.14.9]: https://github.com/cyber-shuttle/linkspan/compare/v0.14.8...v0.14.9
[0.14.8]: https://github.com/cyber-shuttle/linkspan/compare/v0.14.7...v0.14.8
[0.14.7]: https://github.com/cyber-shuttle/linkspan/compare/v0.14.6...v0.14.7
[0.14.6]: https://github.com/cyber-shuttle/linkspan/compare/v0.14.5...v0.14.6
[0.14.5]: https://github.com/cyber-shuttle/linkspan/compare/v0.14.4...v0.14.5
[0.14.4]: https://github.com/cyber-shuttle/linkspan/compare/v0.14.3...v0.14.4
[0.14.3]: https://github.com/cyber-shuttle/linkspan/compare/v0.14.2...v0.14.3
[0.14.2]: https://github.com/cyber-shuttle/linkspan/compare/v0.14.1...v0.14.2
[0.14.1]: https://github.com/cyber-shuttle/linkspan/compare/v0.14.0...v0.14.1
[0.14.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.13.2...v0.14.0
[0.13.2]: https://github.com/cyber-shuttle/linkspan/compare/v0.13.1...v0.13.2
[0.13.1]: https://github.com/cyber-shuttle/linkspan/compare/v0.13.0...v0.13.1
[0.13.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.12.1...v0.13.0
[0.12.1]: https://github.com/cyber-shuttle/linkspan/compare/v0.12.0...v0.12.1
[0.12.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.9.2...v0.10.0
[0.9.2]: https://github.com/cyber-shuttle/linkspan/compare/v0.9.1...v0.9.2
[0.9.1]: https://github.com/cyber-shuttle/linkspan/compare/v0.9.0...v0.9.1
[0.9.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.6.2...v0.7.0
[0.6.2]: https://github.com/cyber-shuttle/linkspan/compare/v0.6.1...v0.6.2
[0.6.1]: https://github.com/cyber-shuttle/linkspan/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.5.1...v0.6.0
[0.5.1]: https://github.com/cyber-shuttle/linkspan/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/cyber-shuttle/linkspan/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/cyber-shuttle/linkspan/releases/tag/v0.1.0
