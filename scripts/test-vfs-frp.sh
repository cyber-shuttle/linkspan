#!/usr/bin/env bash
# Test VFS publish (Mac) + mount (Linux) via FRP.
# FRP server: hub.dev.cybershuttle.org:7000:mysecret
#
# Usage:
#   On Mac:  ./scripts/test-vfs-frp.sh publish
#   On Linux (e.g. ubuntu@hub.dev.cybershuttle.org): ./scripts/test-vfs-frp.sh mount <token> [mountpoint]
#   Or use the API manually (see below).

set -e
LINKSpan_PORT="${LINKSpan_PORT:-8080}"
FRP="${FRP:-hub.dev.cybershuttle.org:7000:mysecret}"
API="http://127.0.0.1:${LINKSpan_PORT}/api/v1"

case "${1:-}" in
  publish)
    if [ -z "${2:-}" ]; then
      echo "Usage: $0 publish <folder>"
      echo "Example: $0 publish /tmp/myshare"
      exit 1
    fi
    FOLDER="$2"
    if [ ! -d "$FOLDER" ]; then
      echo "Folder does not exist: $FOLDER"
      exit 1
    fi
    echo "Publishing $FOLDER with FRP ($FRP)..."
    R=$(curl -s -X POST "$API/fs/publish" -H "Content-Type: application/json" \
      -d "{\"folder\": \"$FOLDER\", \"frp_connection\": \"$FRP\"}")
    echo "$R" | jq .
    TOKEN=$(echo "$R" | jq -r '.token')
    if [ -z "$TOKEN" ] || [ "$TOKEN" = "null" ]; then
      echo "No token in response. Is linkspan running on port $LINKSpan_PORT?"
      exit 1
    fi
    echo ""
    echo "Share this with the Linux mount client:"
    echo "  token=$TOKEN"
    echo "  frp_connection=$FRP"
    echo ""
    echo "On the Linux machine, run linkspan and create a mount:"
    echo "  curl -s -X POST http://127.0.0.1:$LINKSpan_PORT/api/v1/fs/mount -H 'Content-Type: application/json' \\"
    echo "    -d '{\"mountpoint\": \"/tmp/remote\", \"token\": \"$TOKEN\", \"frp_connection\": \"$FRP\"}'"
    ;;
  mount)
    TOKEN="${2:-}"
    MP="${3:-/tmp/vfs-remote}"
    if [ -z "$TOKEN" ]; then
      echo "Usage: $0 mount <token> [mountpoint]"
      echo "Example: $0 mount abc123:secret456 /tmp/remote"
      exit 1
    fi
    echo "Creating FUSE mount at $MP with token (FRP: $FRP)..."
    curl -s -X POST "$API/fs/mount" -H "Content-Type: application/json" \
      -d "{\"mountpoint\": \"$MP\", \"token\": \"$TOKEN\", \"frp_connection\": \"$FRP\"}" | jq .
    echo ""
    echo "Mount created. Try: ls -la $MP"
    echo "To unmount: DELETE $API/fs/mount/<id>  (get id from GET $API/fs/mounts)"
    ;;
  *)
    echo "Usage: $0 publish <folder>  |  $0 mount <token> [mountpoint]"
    echo ""
    echo "1. On Mac: start linkspan, then: $0 publish /path/to/folder"
    echo "2. On Linux: copy linkspan-linux, start it, then: $0 mount <token> /tmp/remote"
    exit 1
    ;;
esac
