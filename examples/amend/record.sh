#!/usr/bin/env sh
# Plays every beat of the Amend demo with nobody typing, so it can be recorded.
# Run it from the Recall repository:
#
#   asciinema rec --headless --window-size 124x34 -c ./examples/amend/record.sh amend.cast
#   agg --idle-time-limit 6 amend.cast amend.gif
#
# Starts its own server on port 24400 with a fresh store, so a server on the
# usual port cannot change what the recording shows.
set -eu

HTTP_ADDR="127.0.0.1:24400"
GRPC_ADDR="127.0.0.1:24401"
SERVER="${POLIGN_SERVER:-polign-server}"
WORK="$(mktemp -d)"

if curl -sf "http://$HTTP_ADDR/healthz" >/dev/null 2>&1; then
  echo "something is already listening on $HTTP_ADDR; stop it first"
  exit 1
fi

go build -o "$WORK/amend" ./examples/amend

SERVER_PID=""
trap 'kill "$SERVER_PID" 2>/dev/null && wait "$SERVER_PID" 2>/dev/null; rm -rf "$WORK"' EXIT INT TERM

# The server runs inside the work directory so its local files land there.
(cd "$WORK" && exec "$SERVER" -store "fs:$WORK/store" -http "$HTTP_ADDR" -grpc "$GRPC_ADDR") >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
i=0
until curl -sf "http://$HTTP_ADDR/healthz" >/dev/null 2>&1; do
  i=$((i + 1))
  [ "$i" -gt 60 ] && { echo "server did not come up"; cat "$WORK/server.log"; exit 1; }
  sleep 0.25
done

narrate() {
  printf '\n\033[2m# %s\033[0m\n' "$1"
  sleep 2
}

beat() {
  printf '\033[1m$ amend -beat %s\033[0m\n' "$1"
  (cd "$WORK" && ./amend -server "http://$HTTP_ADDR" -collection amend-demo -beat "$1")
  sleep "$2"
}

printf '\033[2J\033[H'
narrate "A support team runs a chat agent and a voice agent on one shared memory."
beat seed 5
narrate "Today a support lead revokes the exception in chat. The voice agent is asked next."
beat correct 7
narrate "The goodwill-credit policy is discontinued."
beat withdraw 6
narrate "The customer disputes the refund they asked for on Sep 20. What did the agents believe then?"
beat timetravel 9
narrate "The dispute file: what was believed on Sep 20, checkable without our server."
beat audit 7
