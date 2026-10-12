#!/usr/bin/env bash
# Smoke test for the recall image: start a polign-server on the host, run the
# image as `recall serve` against it, and exercise a key, a write and a read.
#
#   deploy/container/smoke.sh <image> <path to polign-server> [expected version]
#
# Needs docker and curl. The server listens on every interface of the host so
# the container reaches it through host.docker.internal (Docker Desktop has the
# alias; Linux gets it from --add-host), and the container's port is published
# to 127.0.0.1. Everything the test creates lives under a temporary directory.
set -euo pipefail
IMAGE=${1:?image}
POLIGN_SERVER=${2:?polign-server binary}
EXPECT_VERSION=${3:-}
WORK=$(mktemp -d)
cleanup() {
  docker rm -f recall-smoke >/dev/null 2>&1 || true
  if [ -n "${SERVER_PID:-}" ]; then kill "$SERVER_PID" >/dev/null 2>&1 || true; wait "$SERVER_PID" 2>/dev/null || true; fi
  rm -rf "$WORK"
}
trap cleanup EXIT

if [ -n "$EXPECT_VERSION" ]; then
  got=$(docker run --rm "$IMAGE" --version)
  [ "$got" = "$EXPECT_VERSION" ] || { echo "image reports version $got, want $EXPECT_VERSION" >&2; exit 1; }
fi

# polign_db key: "plgn_<id>_<secret>", both hex, the shape recall setup mints
# and polign-server -bootstrap-key-file accepts.
POLIGN_KEY="plgn_$(openssl rand -hex 8)_$(openssl rand -hex 32)"
printf '%s\n' "$POLIGN_KEY" > "$WORK/polign.key"
"$POLIGN_SERVER" -store "fs:$WORK/data" -http 0.0.0.0:23900 -grpc 127.0.0.1:0 \
  -require-data-key -bootstrap-key-file "$WORK/polign.key" -telemetry=false >"$WORK/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 60); do curl -sf http://127.0.0.1:23900/readyz >/dev/null 2>&1 && break; sleep 0.5; done
curl -sf http://127.0.0.1:23900/readyz >/dev/null || { cat "$WORK/server.log" >&2; exit 1; }

# A static Recall key for the test caller, and the image as the server.
RECALL_KEY="rcl_0123456789abcdef_smoke"
mkdir -p "$WORK/keys"; printf '%s smoke\n' "$RECALL_KEY" > "$WORK/keys/keys"; chmod 644 "$WORK/keys/keys"
docker run -d --name recall-smoke --add-host=host.docker.internal:host-gateway -p 127.0.0.1:8900:8080 \
  -e POLIGN_API_KEY="$POLIGN_KEY" -v "$WORK/keys:/keys:ro" "$IMAGE" \
  -url http://host.docker.internal:23900 serve -listen :8080 -keys-file /keys/keys >/dev/null
for _ in $(seq 1 40); do curl -sf http://127.0.0.1:8900/healthz >/dev/null 2>&1 && break; sleep 0.5; done
curl -sf http://127.0.0.1:8900/healthz >/dev/null || { docker logs recall-smoke >&2; exit 1; }

auth="Authorization: Bearer $RECALL_KEY"
code=$(curl -s -o /dev/null -w '%{http_code}' -H 'Content-Type: application/json' -d '{}' http://127.0.0.1:8900/v1/predicates)
[ "$code" = 401 ] || { echo "unauthenticated call answered $code, want 401" >&2; exit 1; }
curl -sf -H "$auth" -H 'Content-Type: application/json' -d '{"subject":"user","predicate":"prefers_editor","value":"vim"}' http://127.0.0.1:8900/v1/remember >/dev/null
curl -sf -H "$auth" -H 'Content-Type: application/json' -d '{"subject":"user","predicate":"prefers_editor","value":"helix"}' http://127.0.0.1:8900/v1/remember >/dev/null
out=$(curl -sf -H "$auth" -H 'Content-Type: application/json' -d '{"subject":"user","predicate":"prefers_editor"}' http://127.0.0.1:8900/v1/recall)
case "$out" in
  *'"value":"helix"'*'"replaced"'*|*'"replaced"'*'"value":"helix"'*) ;;
  *) echo "recall answered: $out" >&2; exit 1 ;;
esac
mcp=$(curl -sf -H "$auth" -H 'Content-Type: application/json' -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' http://127.0.0.1:8900/mcp)
case "$mcp" in *'"name":"memory_history"'*) ;; *) echo "mcp tools/list answered: $mcp" >&2; exit 1 ;; esac
echo "smoke ok: $IMAGE serves memory on polign_db ($(docker run --rm "$IMAGE" --version))"
