# Conduit — simple REST scaffold

This repository provides a small Go REST API skeleton exposing endpoints for:

- Jupyter kernel management
- VS Code remote session management
- Remote filesystem management
- Tunnel management

Run locally:

```bash
cd /Users/dwannipurage3/code/conduit
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
# conduit