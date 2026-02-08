#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  ./create_hpc_vscode_session.sh <base_url> <token> [options]

Required:
  base_url                 e.g. https://example.com
  token                    auth token used in header: X-Tunnel-Authorization: tunnel <token>

Options:
  --tunnel-secret   VALUE  (default: abc)
  --discovery-host  VALUE  (default: hub.dev.cybershuttle.org)
  --discovery-port  VALUE  (default: 7000)
  --discovery-token VALUE  (default: mysecret)
  --password        VALUE  (optional; included in POST body if provided)
  --tunnel-type     VALUE  (default: xtcp)
  --bind-port       VALUE  (optional; override bind port for visitor config)

Examples:
  ./create_hpc_vscode_session.sh https://example.com TOKEN \
    --tunnel-secret abc \
    --discovery-host hub.dev.cybershuttle.org \
    --discovery-port 7000 \
    --discovery-token mysecret \
    --password pass123
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

if [[ $# -lt 2 ]]; then
  usage >&2
  exit 1
fi

BASE_URL="${1%/}"
TOKEN="$2"
shift 2

# Defaults
TUNNEL_SECRET="abc"
DISCOVERY_HOST="hub.dev.cybershuttle.org"
DISCOVERY_PORT="7000"
DISCOVERY_TOKEN="mysecret"
PASSWORD="test"
TUNNEL_TYPE="xtcp"
BIND_PORT=""

# Parse flags
while [[ $# -gt 0 ]]; do
  case "$1" in
    --tunnel-secret)
      TUNNEL_SECRET="${2:-}"; shift 2 ;;
    --discovery-host)
      DISCOVERY_HOST="${2:-}"; shift 2 ;;
    --discovery-port)
      DISCOVERY_PORT="${2:-}"; shift 2 ;;
    --discovery-token)
      DISCOVERY_TOKEN="${2:-}"; shift 2 ;;
    --password)
      PASSWORD="${2:-}"; shift 2 ;;
    --tunnel-type)
      TUNNEL_TYPE="${2:-}"; shift 2 ;;
    --bind-port)
      BIND_PORT="${2:-}"; shift 2 ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ -z "$DISCOVERY_PORT" || ! "$DISCOVERY_PORT" =~ ^[0-9]+$ ]]; then
  echo "Error: --discovery-port must be a number" >&2
  exit 1
fi

# Dependencies: jq preferred; fallback to python3
have_jq=0
if command -v jq >/dev/null 2>&1; then
  have_jq=1
elif ! command -v python3 >/dev/null 2>&1; then
  echo "Error: need either 'jq' or 'python3' installed to parse JSON." >&2
  exit 1
fi

auth_header="X-Tunnel-Authorization: tunnel ${TOKEN}"

# Build session request body
if [[ $have_jq -eq 1 ]]; then
  session_body="$(jq -n --arg password "$PASSWORD" '{password: $password, mount_user_home: false}')"
else
  pw_esc="$(printf '%s' "$PASSWORD" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read())[1:-1])')"
  session_body="{\"password\":\"${pw_esc}\",\"mount_user_home\":false}"
fi

# 1) Create VSCode session -> get bind_port
session_resp="$(
  curl -sS -X POST \
    -H "$auth_header" \
    -H "Content-Type: application/json" \
    "${BASE_URL}/api/v1/vscode/sessions" \
    -d "$session_body"
)"

if [[ $have_jq -eq 1 ]]; then
  bind_port="$(printf '%s' "$session_resp" | jq -r '.bind_port // empty')"
else
  bind_port="$(python3 - <<'PY'
import json,sys
d=json.loads(sys.stdin.read())
print(d.get("bind_port",""))
PY
<<<"$session_resp")"
fi

if [[ -z "${bind_port}" || ! "${bind_port}" =~ ^[0-9]+$ ]]; then
  echo "Error: failed to parse bind_port from response:" >&2
  echo "$session_resp" >&2
  exit 1
fi

# Random tunnel name: frptunnel-<random>
rand_suffix="$(LC_ALL=C tr -dc '0-9' </dev/urandom | head -c 6 || true)"
if [[ -z "$rand_suffix" ]]; then
  rand_suffix="$RANDOM$RANDOM"
fi
tunnel_name="frptunnel-${rand_suffix}"

# Build JSON body (include password only if provided)
json_escape_py='import json,sys; print(json.dumps(sys.stdin.read())[1:-1])'
esc() { python3 -c "$json_escape_py"; }

# Use jq to build JSON safely if available; else build carefully with python escaping for strings
if [[ $have_jq -eq 1 ]]; then
  tunnel_body="$(
    jq -n \
      --arg tunnelName "$tunnel_name" \
      --arg tunnelType "$TUNNEL_TYPE" \
      --arg tunnelSecret "$TUNNEL_SECRET" \
      --arg discoveryHost "$DISCOVERY_HOST" \
      --arg discoveryToken "$DISCOVERY_TOKEN" \
      --argjson port "$bind_port" \
      --argjson discoveryPort "$DISCOVERY_PORT" \
      '{
        tunnelName: $tunnelName,
        port: $port,
        tunnelType: $tunnelType,
        tunnelSecret: $tunnelSecret,
        discoveryHost: $discoveryHost,
        discoveryPort: $discoveryPort,
        discoveryToken: $discoveryToken
      }'
  )"
else
  # Escape strings using python3, keep numbers numeric
  tn_esc="$(printf '%s' "$tunnel_name" | esc)"
  tt_esc="$(printf '%s' "$TUNNEL_TYPE" | esc)"
  ts_esc="$(printf '%s' "$TUNNEL_SECRET" | esc)"
  dh_esc="$(printf '%s' "$DISCOVERY_HOST" | esc)"
  dt_esc="$(printf '%s' "$DISCOVERY_TOKEN" | esc)"
  tunnel_body="$(cat <<JSON
{
  "tunnelName": "${tn_esc}",
  "port": ${bind_port},
  "tunnelType": "${tt_esc}",
  "tunnelSecret": "${ts_esc}",
  "discoveryHost": "${dh_esc}",
  "discoveryPort": ${DISCOVERY_PORT},
  "discoveryToken": "${dt_esc}"
}
JSON
)"
fi

# 2) Create FRP tunnel
tunnel_resp="$(
  curl -sS -X POST \
    -H "$auth_header" \
    -H "Content-Type: application/json" \
    "${BASE_URL}/api/v1/tunnels/frp" \
    -d "$tunnel_body"
)"

if [[ $have_jq -eq 1 ]]; then
  created_name="$(printf '%s' "$tunnel_resp" | jq -r '.tunnelName // empty')"
else
  created_name="$(python3 - <<'PY'
import json,sys
d=json.loads(sys.stdin.read())
print(d.get("tunnelName",""))
PY
<<<"$tunnel_resp")"
fi

if [[ -z "${created_name}" ]]; then
  echo "Error: failed to parse tunnelName from response:" >&2
  echo "$tunnel_resp" >&2
  exit 1
fi

echo "$created_name"

# Determine the bindPort to use (optional override or from session response)
if [[ -n "$BIND_PORT" ]]; then
  visitor_bind_port="$BIND_PORT"
else
  visitor_bind_port="8032"
fi

# Write FRP client visitor configuration to frpc.toml
cat > frpc.toml <<EOF
serverAddr = "${DISCOVERY_HOST}"
serverPort = ${DISCOVERY_PORT}
auth.token = "${DISCOVERY_TOKEN}"
loginFailExit = false

[[visitors]]
name = "xtcp_client"
type = "xtcp"
serverName = "${created_name}"
secretKey = "${TUNNEL_SECRET}"
bindPort = ${visitor_bind_port}
bindAddr = "0.0.0.0"
keepTunnelOpen = true
EOF

echo "FRP client configuration written to frpc.toml"

# Remove any existing known_hosts entry for this localhost port
if [[ -f ~/.ssh/known_hosts ]]; then
  sed -i.bak "/\[localhost\]:${visitor_bind_port}/d" ~/.ssh/known_hosts
  echo "Removed known_hosts entries for [localhost]:${visitor_bind_port}"
fi

# Print connection instructions in bold
echo ""
echo -e "\033[1;32m╔════════════════════════════════════════════════════════════╗\033[0m"
echo -e "\033[1;32m║                                                            ║\033[0m"
echo -e "\033[1;32m║  \033[1;37mConnect to job using: ssh -p ${visitor_bind_port} localhost\033[1;32m              ║\033[0m"
echo -e "\033[1;32m║                                                            ║\033[0m"
echo -e "\033[1;32m╚════════════════════════════════════════════════════════════╝\033[0m"
echo ""

# Check if frpc binary is available
if ! command -v ./frpc >/dev/null 2>&1 && ! command -v frpc >/dev/null 2>&1; then
  echo "Error: frpc binary not found." >&2
  echo "Please download it from: https://github.com/fatedier/frp/releases/tag/v0.67.0" >&2
  echo "Extract the archive and place 'frpc' in the current directory or in your PATH." >&2
  exit 1
fi

# Determine which frpc to use
if [[ -x ./frpc ]]; then
  FRPC_BIN="./frpc"
else
  FRPC_BIN="frpc"
fi

echo "Starting frpc client..."
exec "$FRPC_BIN" -c frpc.toml
