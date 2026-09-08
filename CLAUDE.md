# Linkspan

Go agent that runs inside a compute-node allocation. It hosts the Dev Tunnel its client created, runs the
client's YAML workflow, and serves an SSH server for VS Code Remote-SSH.

Build, test and lint are the Go defaults (`go build ./...`, `go test ./...`, `go vet ./...`). Public
documentation is `README.md`, `CONTRIBUTING.md`, `SECURITY.md` and `docs/COMPATIBILITY.md`; anything a human
reader needs belongs in one of those, not here.

## Rules

- Adding to the flag, route or response surface needs a consumer; `docs/COMPATIBILITY.md` names the ones
  there are.
