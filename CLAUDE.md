# Linkspan

Go agent that runs inside a compute-node allocation. It hosts the Dev Tunnel its client created, runs the
client's YAML workflow, and serves an SSH server for VS Code Remote-SSH.

Build, test and lint are the Go defaults (`go build ./...`, `go test ./...`, `go vet ./...`). Public
documentation is `README.md`, `CONTRIBUTING.md`, `SECURITY.md` and `docs/COMPATIBILITY.md`; anything a human
reader needs belongs in one of those, not here.

## Rules

- Read `docs/COMPATIBILITY.md` before changing a flag, the `--version` or `--help` output, the release
  archive name, an `/api/v1` route or a response shape. Each is a contract with a client that ships
  separately.
- The surface listed there is the whole surface. A new flag or route is an API change and needs a consumer.
- Restoring PTY allocation or reverse port forwarding to the SSH server is a change, not a fix: both were
  removed in d55b0a1 (#43). `SECURITY.md` states what a session gets.
