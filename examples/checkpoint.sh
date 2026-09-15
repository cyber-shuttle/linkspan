#!/bin/sh
set -e
cd "$(dirname "$0")"
log=$(mktemp)

job() {
	linkspan --port 8080 --workflow "$1" </dev/null >>"$log" 2>&1 &
	[ -z "$2" ] || { sleep "$2"; kill -USR1 $!; }
	wait $!
	echo "$1: counted to $(grep -E '^[0-9]+$' "$log" | tail -1)"
}

job checkpoint.yml 5
job restore.yml 5
job restore.yml 5
job restore.yml
[ "$(grep -E '^[0-9]+$' "$log")" = "$(seq 1 30)" ] || { echo "not whole: $log"; exit 1; }
echo "ok: 1 to 30"
