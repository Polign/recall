#!/usr/bin/env sh
# Drives the demo from inside the sandbox with nothing but curl: exchange the
# projected token for a key, remember, recall, and show the key's identity.
# Run it as: kubectl -n agents exec deploy-or-pod -- sh -s < demo.sh
set -eu
: "${RECALL_URL:?}" "${RECALL_TOKEN_FILE:?}"
TOKEN=$(cat "$RECALL_TOKEN_FILE")
echo "# exchanging the pod's token for a key"
RESP=$(curl -sS -H 'Content-Type: application/json' -d "{\"token\":\"$TOKEN\"}" "$RECALL_URL/v1/auth/kubernetes")
echo "$RESP"
KEY=$(printf '%s' "$RESP" | sed -n 's/.*"key":"\([^"]*\)".*/\1/p')
[ -n "$KEY" ] || { echo "no key in the answer" >&2; exit 1; }
auth="Authorization: Bearer $KEY"
echo "# remembering"
curl -sS -H "$auth" -H 'Content-Type: application/json' -d '{"subject":"user","predicate":"prefers_editor","value":"vim"}' "$RECALL_URL/v1/remember"; echo
curl -sS -H "$auth" -H 'Content-Type: application/json' -d '{"subject":"user","predicate":"prefers_editor","value":"helix"}' "$RECALL_URL/v1/remember"; echo
echo "# recalling: the current value, with what it replaced"
curl -sS -H "$auth" -H 'Content-Type: application/json' -d '{"subject":"user","predicate":"prefers_editor"}' "$RECALL_URL/v1/recall"; echo
echo "# another namespace is out of reach"
curl -sS -o /dev/null -w 'HTTP %{http_code}\n' -H "$auth" -H 'Content-Type: application/json' -d '{"namespace":"agents:someone-else","subject":"user","predicate":"prefers_editor"}' "$RECALL_URL/v1/recall"
