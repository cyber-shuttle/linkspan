# LinkSpan — simple REST scaffold

This repository provides a small Go REST API skeleton exposing endpoints for:

- Jupyter kernel management
- VS Code remote session management
- Remote filesystem management
- Tunnel management

Run locally:

```bash
cd /Users/dwannipurage3/code/linkspan
go test ./...    # runs tests and fetches modules
go run .         # starts server on :8080
```

Example curl calls:

List kernels
```bash
curl -s http://localhost:8080/api/v1/jupyter/kernels | jq
```

Create a VS Code session (placeholder)
```bash
curl -X POST http://localhost:8080/api/v1/vscode/sessions
```

Read a remote file (placeholder)
```bash
curl -s "http://localhost:8080/api/v1/fs/read?path=/tmp/test.txt" | jq
```

### VFS: Publish and FUSE mount (with FRP)

- **Publish** (any OS): expose a local folder over gRPC. Use `frp_connection` (e.g. `hub.dev.cybershuttle.org:7000:mysecret`) to register with an FRP server; the API returns a **token** (`id:secret`) for the mount client.
- **Mount** (Linux only): FUSE-mount a remote filesystem. Use either `server_addr` (direct gRPC) or `token` + `frp_connection` (FRP tunnel).

Example — publish with FRP (Mac or Linux):

```bash
curl -s -X POST http://localhost:8080/api/v1/fs/publish \
  -H "Content-Type: application/json" \
  -d '{"folder": "/tmp/myshare", "frp_connection": "hub.dev.cybershuttle.org:7000:mysecret"}' | jq
# => {"id":"publish-1","token":"<id>:<secret>"}
```

Example — mount on Linux (with token from publish):

```bash
curl -s -X POST http://localhost:8080/api/v1/fs/mount \
  -H "Content-Type: application/json" \
  -d '{"mountpoint": "/tmp/remote", "token": "<id>:<secret>", "frp_connection": "hub.dev.cybershuttle.org:7000:mysecret"}' | jq
```

Helper script: `scripts/test-vfs-frp.sh publish <folder>` and `scripts/test-vfs-frp.sh mount <token> [mountpoint]`. Use the Linux binary `linkspan-linux` (build with `GOOS=linux GOARCH=amd64 go build -o linkspan-linux .`) on the Ubuntu mount host.

# linkspan

Build with gorelease 
```
goreleaser release --snapshot --clean
```