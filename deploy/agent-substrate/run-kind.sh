#!/usr/bin/env bash
# Recall on Agent Substrate, on the kind cluster from Substrate's quickstart.
#
# Needs: Substrate installed as in its quickstart (hack/create-kind-cluster.sh,
# then ate-setup deploy ate-system), docker, kubectl, curl, and kubectl-ate on
# PATH or named by ATE.
#
#   ./run-kind.sh          build, deploy, run the checks
#   ./run-kind.sh clean    remove what the script created
set -o errexit -o nounset -o pipefail

cd "$(dirname "$0")"
ROOT="$(cd ../.. && pwd)"
CONTEXT="${CONTEXT:-kind-kind}"
REGISTRY="${REGISTRY:-localhost:5001}"
ATE="${ATE:-kubectl-ate}"
ATESPACE=recall-demo
RECALL_HOST=recall.recall-system.svc.cluster.local

k() { kubectl --context "$CONTEXT" "$@"; }
# kubectl-ate warns on every call that it acts as the shared identity.
ERR="$(mktemp)"
ate() {
  local rc=0
  "$ATE" --context "$CONTEXT" "$@" 2>"$ERR" || rc=$?
  grep -v '^Warning: no --token-file' "$ERR" >&2 || true
  return "$rc"
}
say() { printf '\n== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

PIDS=()
stop_proxies() {
  for p in "${PIDS[@]:-}"; do
    kill "$p" 2>/dev/null || true
    wait "$p" 2>/dev/null || true
  done
  PIDS=()
}
trap 'stop_proxies; rm -f "$ERR"' EXIT

clean() {
  for a in agent-1 agent-2; do ate delete actor "$a" -a "$ATESPACE" >/dev/null 2>&1 || true; done
  ate delete actor-template agent -a "$ATESPACE" >/dev/null 2>&1 || true
  k delete namespace recall-demo recall-system --ignore-not-found --wait=true
}

if [[ "${1:-}" == "clean" ]]; then
  clean
  exit 0
fi

# proxy <actor> <local port>: a local port that reaches the actor through the
# router, resuming the actor when it is suspended.
proxy() {
  "$ATE" --context "$CONTEXT" proxy actor "$1" "$2:80" -a "$ATESPACE" >/dev/null 2>&1 &
  PIDS+=("$!")
  for _ in $(seq 1 60); do
    curl -s -o /dev/null -m 2 "localhost:$2/readyz" && return 0
    sleep 0.5
  done
  fail "no proxy to $1 on port $2"
}

# A failed call prints nothing, so the check after it fails with a message
# instead of the script stopping silently.
call() { curl -s -m 60 -X POST "localhost:$1$2" -d "${3:-}" || true; }

new_actor() {
  ate create actor "$1" -a "$ATESPACE" --template agent >/dev/null
  ate create egress-policy "$1" -a "$ATESPACE" -f egress-policy.yaml >/dev/null
}

say "Build the recall image and the demo actor, push them to $REGISTRY"
docker build -q -f "$ROOT/deploy/container/Dockerfile" --build-arg VERSION="$(git -C "$ROOT" describe --tags --always)" -t "$REGISTRY/recall:dev" "$ROOT" >/dev/null
docker build -q -f actor/Dockerfile -t "$REGISTRY/recall-demo-actor:dev" "$ROOT" >/dev/null
docker push -q "$REGISTRY/recall:dev" >/dev/null
docker push -q "$REGISTRY/recall-demo-actor:dev" >/dev/null
# Substrate takes images pinned by digest only: a changed image would
# invalidate snapshots.
RECALL_IMAGE="$(docker inspect --format '{{index .RepoDigests 0}}' "$REGISTRY/recall:dev")"
ACTOR_IMAGE="$(docker inspect --format '{{index .RepoDigests 0}}' "$REGISTRY/recall-demo-actor:dev")"

say "Deploy recall serve, trusting the install's actor JWT issuer"
sed "s|RECALL_IMAGE|$RECALL_IMAGE|" recall.yaml | k apply -f - >/dev/null
k -n recall-system rollout status deploy/recall --timeout=180s >/dev/null
# The first start downloads polign-server for the local database.
for _ in $(seq 1 60); do
  k -n recall-system logs deploy/recall 2>/dev/null | grep 'Agent Substrate actor JWTs' && break
  sleep 2
done

say "Worker pool, atespace, actor template"
# Any pool's worker image will do; the install built it.
WORKER_IMAGE="$(k get workerpools -A -o jsonpath='{.items[0].spec.workerImage}')"
[[ -n "$WORKER_IMAGE" ]] || fail "no WorkerPool to copy the worker image from; deploy a Substrate demo first (ate-setup deploy demo counter)"
sed "s|WORKER_IMAGE|$WORKER_IMAGE|" workerpool.yaml | k apply -f - >/dev/null
k -n recall-demo rollout status deploy/recall-demo --timeout=180s >/dev/null 2>&1 || true
ate get atespace "$ATESPACE" >/dev/null 2>&1 || ate create atespace "$ATESPACE" >/dev/null
for a in agent-1 agent-2; do ate delete actor "$a" -a "$ATESPACE" >/dev/null 2>&1 || true; done
ate delete actor-template agent -a "$ATESPACE" >/dev/null 2>&1 || true
sed "s|ACTOR_IMAGE|$ACTOR_IMAGE|" actor-template.yaml | ate create actor-template -f - >/dev/null
for _ in $(seq 1 60); do
  [[ -n "$(ate get actor-template agent -a "$ATESPACE" -o json | grep -o '"goldenTag"' || true)" ]] && break
  sleep 2
done

say "Two actors. Each may reach Recall and nothing else, and holds no key"
new_actor agent-1
new_actor agent-2
proxy agent-1 8101
proxy agent-2 8102

say "1. agent-1 remembers, with a placeholder in its Authorization header"
[[ "$(call 8101 /count)" == "1" ]] || fail "agent-1's counter did not start at 1"
# After a redeploy the gateway can hold the Recall Service's old address for a
# short while, so wait until a call gets through.
for _ in $(seq 1 30); do
  [[ "$(call 8101 /v1/recall '{"question":"anything"}')" == *'"memories"'* ]] && break
  sleep 3
done
call 8101 /v1/remember '{"text":"The deploy window for the billing service is Tuesday 14:00 UTC."}' >/dev/null
out="$(call 8101 /v1/recall '{"question":"when is the billing deploy window"}')"
echo "agent-1 recalls: $out"
[[ "$out" == *"Tuesday 14:00"* ]] || fail "agent-1 did not recall what it remembered"

say "2. agent-2 asks the same question and gets nothing: its token names another actor"
out="$(call 8102 /v1/recall '{"question":"when is the billing deploy window"}')"
echo "agent-2 recalls: $out"
[[ "$out" != *"Tuesday"* ]] || fail "agent-2 read agent-1's memory"

say "3. A caller with no actor JWT is refused"
k -n recall-system port-forward svc/recall 8180:80 >/dev/null 2>&1 &
PIDS+=("$!")
# curl prints 000 until the port-forward is listening.
code=000
for _ in $(seq 1 30); do
  code="$(curl -s -o /dev/null -w '%{http_code}' -m 10 -X POST localhost:8180/v1/recall -H 'Authorization: Bearer placeholder' -d '{"question":"billing"}' || true)"
  [[ "$code" != "000" ]] && break
  sleep 1
done
echo "direct call with the placeholder: HTTP $code"
[[ "$code" == "401" ]] || fail "a call without an actor JWT got HTTP $code, want 401"

say "4. Suspend agent-1. The snapshot keeps its process memory; Recall answers on the first call after resume"
ate suspend actor agent-1 -a "$ATESPACE" --mode=sync >/dev/null
state="$(ate get actor agent-1 -a "$ATESPACE" | awk 'NR==2 {print $4}')"
echo "agent-1 is $state"
[[ "$state" == "ACTOR_STATE_SUSPENDED" ]] || fail "agent-1 did not suspend"
start="$(date +%s.%N)"
out="$(call 8101 /v1/recall '{"question":"when is the billing deploy window"}')"
end="$(date +%s.%N)"
printf 'resume and recall: %.2f s\n' "$(echo "$end - $start" | bc)"
[[ "$out" == *"Tuesday 14:00"* ]] || fail "agent-1 did not recall after resume: $out"
n="$(call 8101 /count)"
echo "counter after resume: $n"
[[ "$n" == "2" ]] || fail "the counter in process memory did not survive the suspend"

say "5. Delete agent-1 and create it again. The snapshot is gone; the memory is not"
stop_proxies
ate delete actor agent-1 -a "$ATESPACE" >/dev/null
new_actor agent-1
proxy agent-1 8101
n="$(call 8101 /count)"
echo "counter in the new actor: $n"
[[ "$n" == "1" ]] || fail "the new actor kept the old actor's process memory"
# The gateway caches a missing policy as deny for 10 s.
for _ in $(seq 1 15); do
  out="$(call 8101 /v1/recall '{"question":"when is the billing deploy window"}')"
  [[ "$out" == *"Tuesday 14:00"* ]] && break
  sleep 2
done
echo "new agent-1 recalls: $out"
[[ "$out" == *"Tuesday 14:00"* ]] || fail "the recreated actor did not reach the old actor's memory"

say "6. The State API: an actor resumes only itself, and picks up where the deleted actor stopped"
proxy agent-2 8102
# A run before this one may have left the agent resumed.
call 8101 /v1/agent/release '{"agent_id":"agent-1"}' >/dev/null
out="$(call 8101 /v1/agent/resume '{"agent_id":"agent-1"}')"
[[ "$out" == *'"lease_held":true'* ]] || fail "agent-1 did not resume itself: $out"
out="$(call 8101 /v1/agent/update_working_state '{"agent_id":"agent-1","goal":"move billing to the v2 API"}')"
[[ "$out" != *'"error"'* ]] || fail "agent-1 could not write its working state: $out"
out="$(call 8102 /v1/agent/resume '{"agent_id":"agent-1"}')"
echo "agent-2 resuming agent-1: $out"
[[ "$out" == *"namespace_mismatch"* ]] || fail "agent-2 was not refused agent-1's state"
stop_proxies
ate delete actor agent-1 -a "$ATESPACE" >/dev/null
new_actor agent-1
proxy agent-1 8101
for _ in $(seq 1 15); do
  out="$(call 8101 /v1/agent/resume '{"agent_id":"agent-1"}')"
  [[ "$out" == *"already_resumed"* || "$out" == *"briefing"* ]] && break
  sleep 2
done
# The server still holds the agent for the actor that was deleted. The new
# actor releases it and resumes.
if [[ "$out" == *"already_resumed"* ]]; then
  call 8101 /v1/agent/release '{"agent_id":"agent-1"}' >/dev/null
  out="$(call 8101 /v1/agent/resume '{"agent_id":"agent-1"}')"
fi
echo "new agent-1 resumes: $out"
[[ "$out" == *"move billing to the v2 API"* ]] || fail "the recreated actor did not get the working state back"

say "All checks passed"
