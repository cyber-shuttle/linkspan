#!/bin/sh
# Runs checkpoint.yml, pausing it with SIGUSR1 after 5s, then restore.yml three times, paused twice more and then
# run to the end; the payload's count, over the four jobs, must be whole. Linux, with linkspan and criu on PATH
# and an init that reaps orphans; from macOS, with Docker:
#
#   GOOS=linux CGO_ENABLED=0 go build -o /tmp/linkspan . && docker run --rm --init --privileged \
#     -v /tmp/linkspan:/usr/local/bin/linkspan -v "$PWD/examples":/examples debian:bookworm \
#     sh -c 'apt-get update -qq && apt-get install -y -qq criu >/dev/null && /examples/checkpoint.sh'
set -e
cd "$(dirname "$0")"
log=$(mktemp)

# job <document> [seconds before SIGUSR1]
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
