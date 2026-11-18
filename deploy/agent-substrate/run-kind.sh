#!/usr/bin/env bash
# Recall on Agent Substrate, on the kind cluster from Substrate's quickstart.
#
# Needs: Substrate installed as in its quickstart (hack/create-kind-cluster.sh,
# then ate-setup deploy ate-system), docker, kubectl, curl, and kubectl-ate on
# PATH or named by ATE.
#
#   ./run-kind.sh          build, deploy, run the checks
#   SKIP_BASE=1 ./run-kind.sh
#                          only the snapshot checks, 7 to 10
#   ./run-kind.sh clean    remove what the script created
#
# Checks 7 to 10 test Recall state across snapshot restores, clones and
# retries (docs/plans/snapshot-compatibility.md). Where Recall does not yet
# behave as it should they print GAP and carry on, and the script exits 1.
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
  for a in agent-1 agent-2 agent-3 agent-4 agent-5 agent-6; do ate delete actor "$a" -a "$ATESPACE" >/dev/null 2>&1 || true; done
  ate delete tag agent-3-tag -a "$ATESPACE" >/dev/null 2>&1 || true
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

say "Deploy a polign_db for the in-process holder (check 10)"
k apply -f polign.yaml >/dev/null
k -n recall-system rollout status deploy/polign --timeout=180s >/dev/null

say "Worker pool, atespace, actor template"
# Any pool's worker image will do; the install built it.
WORKER_IMAGE="$(k get workerpools -A -o jsonpath='{.items[0].spec.workerImage}')"
[[ -n "$WORKER_IMAGE" ]] || fail "no WorkerPool to copy the worker image from; deploy a Substrate demo first (ate-setup deploy demo counter)"
sed "s|WORKER_IMAGE|$WORKER_IMAGE|" workerpool.yaml | k apply -f - >/dev/null
k -n recall-demo rollout status deploy/recall-demo --timeout=180s >/dev/null 2>&1 || true
ate get atespace "$ATESPACE" >/dev/null 2>&1 || ate create atespace "$ATESPACE" >/dev/null
for a in agent-1 agent-2 agent-3 agent-4 agent-5 agent-6; do ate delete actor "$a" -a "$ATESPACE" >/dev/null 2>&1 || true; done
ate delete tag agent-3-tag -a "$ATESPACE" >/dev/null 2>&1 || true
ate delete actor-template agent -a "$ATESPACE" >/dev/null 2>&1 || true
sed "s|ACTOR_IMAGE|$ACTOR_IMAGE|" actor-template.yaml | ate create actor-template -f - >/dev/null
for _ in $(seq 1 60); do
  [[ -n "$(ate get actor-template agent -a "$ATESPACE" -o json | grep -o '"goldenTag"' || true)" ]] && break
  sleep 2
done

GAPS=0
gap() { printf 'GAP: %s\n' "$*"; GAPS=$((GAPS + 1)); }

if [[ -z "${SKIP_BASE:-}" ]]; then
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
# An actor's writes carry the position its last answer returned.
pos="$(echo "$out" | jq -r '.position | "\(.turns).\(.actions).\(.state)"')"
out="$(call 8101 /v1/agent/update_working_state "{\"agent_id\":\"agent-1\",\"goal\":\"move billing to the v2 API\",\"at\":\"$pos\"}")"
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
# The server still holds the agent for the actor that was deleted; the new
# actor's token is bound to the same agent, so its resume is answered in
# place. A server older than 0.26 answered already_resumed instead.
[[ "$out" != *"already_resumed"* ]] || gap "the recreated actor got already_resumed instead of its context"
echo "new agent-1 resumes: $out"
[[ "$out" == *"move billing to the v2 API"* ]] || fail "the recreated actor did not get the working state back"

fi

# The side effects the harness reports, tallied outside the actor.
EFFECTS="$(mktemp)"
# job <port>: run what is left of the harness's job, tally its side effects.
job() {
  local out
  out="$(call "$1" /h/job)"
  echo "$out" | jq -c '{from_step, step, action_seq, effects, error}' >&2
  # Not seq: on macOS, seq 1 0 counts down and prints two lines.
  local n
  n="$(echo "$out" | jq -r '.effects // 0')"
  while (( n > 0 )); do echo invoice >>"$EFFECTS"; n=$((n - 1)); done
  [[ "$(echo "$out" | jq -r '.error // empty')" == "" ]] || fail "the job failed: $out"
}
# hcall <port> <verb> [args] [repeat]: a State API call for the agent in the
# harness's memory, sent repeat times.
# The first connection after a revert or a clone can fail, so an empty answer
# is retried. A retry here is safe: nothing reached the actor.
hcall() {
  local out args="${3:-}"
  [[ -n "$args" ]] || args='{}'
  for _ in $(seq 1 15); do
    out="$(call "$1" /h/call "{\"verb\":\"$2\",\"args\":$args,\"repeat\":${4:-1}}")"
    [[ "$out" == *'"answers"'* ]] && break
    sleep 2
  done
  echo "$out"
}
# clock <port>: the actor's wall clock and monotonic time since start.
clock() { call "$1" /clock; }
turns() { hcall "$1" recent_turns '{"limit":200}' | jq '.answers[0].body'; }
wait_state() {
  local s
  for _ in $(seq 1 60); do
    s="$(ate get actor "$1" -a "$ATESPACE" | awk 'NR==2 {print $4}')"
    [[ "$s" == "$2" ]] && return 0
    sleep 1
  done
  fail "$1 is $s, want $2"
}

say "7. Rollback: agent-3 sends an invoice, is reverted to a snapshot from before, and runs again"
new_actor agent-3
proxy agent-3 8103
for _ in $(seq 1 15); do
  out="$(call 8103 /h/resume '{"agent_id":"agent-3"}')"
  [[ "$out" == *'"lease_held":true'* ]] && break
  sleep 2
done
[[ "$out" == *'"lease_held":true'* ]] || fail "agent-3 did not resume: $out"
ate suspend actor agent-3 -a "$ATESPACE" --mode=sync >/dev/null
wait_state agent-3 ACTOR_STATE_SUSPENDED
echo "snapshot taken: the harness has resumed and done nothing"
base="$(turns 8103 | jq length)"
job 8103
ate revert actor agent-3 -a "$ATESPACE" >/dev/null
wait_state agent-3 ACTOR_STATE_SUSPENDED
echo "reverted; what a careful harness would check first, pending_actions:"
hcall 8103 pending_actions | jq -c '.answers[0]'
job 8103
t="$(turns 8103)"
echo "turns: $(echo "$t" | jq -c --argjson b "$base" '.[$b:] | [.[] | {seq, role, message_id}]')"
n_turns="$(( $(echo "$t" | jq length) - base ))"
n_ids="$(echo "$t" | jq --argjson b "$base" '.[$b:] | [.[].message_id] | unique | length')"
n_fx="$(wc -l <"$EFFECTS" | tr -d ' ')"
echo "turns recorded: $n_turns ($n_ids distinct message ids); invoices sent: $n_fx"
[[ "$n_turns" == "2" ]] || gap "the reverted actor recorded its turns again ($n_turns turns, $n_ids distinct ids)"
[[ "$n_fx" == "1" ]] || gap "the reverted actor sent the invoice again ($n_fx invoices)"

say "8. Retries: agent-3 sends each write twice, as it would after losing the first answer"
before="$(turns 8103 | jq length)"
hcall 8103 record_turn '{"role":"user","content":"Also send April.","message_id":"retry-1"}' 2 >/dev/null
after="$(turns 8103 | jq length)"
echo "record_turn twice with message_id retry-1: $before -> $after turns"
[[ "$after" == "$((before + 1))" ]] || gap "a retried record_turn with the same message_id was recorded $((after - before)) times"
out="$(hcall 8103 begin_action '{"tool":"create_ticket","class":"idempotent","args":"{\"id\":\"T-1\"}","call_id":"retry-ticket-1"}' 2)"
echo "begin_action twice: $(echo "$out" | jq -c '[.answers[] | {status, seq: .body.seq, key: .body.key}]')"
s1="$(echo "$out" | jq '.answers[0].body.seq')"
s2="$(echo "$out" | jq '.answers[1].body.seq')"
[[ "$s1" == "$s2" ]] || gap "a retried begin_action began a second action (seq $s1 then $s2), with a new key"
out="$(hcall 8103 end_action "{\"seq\":$s1,\"status\":\"done\",\"result\":\"T-1 created\"}" 2)"
echo "end_action twice: $(echo "$out" | jq -c '[.answers[] | {status, body: (.body.status // .body.error.code // .body)}]')"
[[ "$(echo "$out" | jq '.answers[1].status')" == "200" ]] || gap "a retried end_action with the same outcome was refused"
[[ "$s1" == "$s2" ]] || hcall 8103 end_action "{\"seq\":$s2,\"status\":\"abandoned\"}" >/dev/null

say "9. Clone: agent-4 starts from a tag of agent-3 and wakes holding agent-3's id"
ate suspend actor agent-3 -a "$ATESPACE" --mode=sync >/dev/null
wait_state agent-3 ACTOR_STATE_SUSPENDED
ate create tag agent-3-tag -a "$ATESPACE" --actor agent-3 >/dev/null
ate create actor agent-4 -a "$ATESPACE" --template agent --tag agent-3-tag >/dev/null
ate create egress-policy agent-4 -a "$ATESPACE" -f egress-policy.yaml >/dev/null
proxy agent-4 8104
# The gateway caches a missing policy as deny for 10 s.
for _ in $(seq 1 15); do
  out="$(hcall 8104 recent_turns)"
  [[ "$(echo "$out" | jq '.answers[0].status')" =~ ^(200|4..)$ ]] && break
  sleep 2
done
echo "agent-4's in-process counter: $(call 8104 /count) (agent-3's was 0 before the tag)"
echo "agent-4's harness holds: $(echo "$out" | jq -r .agent_id)"
echo "agent-4 reading that agent: $(echo "$out" | jq -c '.answers[0]')"
[[ "$out" == *"namespace_mismatch"* ]] || gap "the clone was not refused the source actor's state"
out="$(call 8104 /h/resume '{"agent_id":"agent-4"}')"
echo "agent-4 resuming itself: fresh=$(echo "$out" | jq '.body.fresh')"

say "10. In process: agent-5 holds an agent through the Go client, against polign_db"
new_actor agent-5
proxy agent-5 8105
for _ in $(seq 1 15); do
  [[ "$(call 8105 /inproc/turns)" == *"not resumed"* ]] && break
  sleep 2
done
RUN="$(date +%s)"
inproc_case() { # <agent id> <ttl seconds> <seconds to wait after the revert>
  local id="$1" out
  out="$(call 8105 /inproc/resume "{\"agent_id\":\"$id\",\"ttl_s\":$2}")"
  echo "resume $id (ttl $2 s): $out"
  [[ "$out" == *'"epoch"'* ]] || fail "agent-5 could not resume $id in process"
  for c in A B; do echo "turn $c: $(call 8105 /inproc/turn "{\"content\":\"$c\"}")"; done
  echo "before the snapshot; clock $(clock 8105)"
  ate suspend actor agent-5 -a "$ATESPACE" --mode=sync >/dev/null
  wait_state agent-5 ACTOR_STATE_SUSPENDED
  echo "snapshot taken after A, B; clock $(clock 8105)"
  for c in C D; do echo "turn $c: $(call 8105 /inproc/turn "{\"content\":\"$c\"}")"; done
  ate revert actor agent-5 -a "$ATESPACE" >/dev/null
  wait_state agent-5 ACTOR_STATE_SUSPENDED
  echo "reverted to the snapshot; waiting $3 s"
  sleep "$3"
  echo "restored; clock $(clock 8105)"
  echo "turn E: $(call 8105 /inproc/turn '{"content":"E"}')"
  echo "turn F: $(call 8105 /inproc/turn '{"content":"F"}')"
  out="$(call 8105 /inproc/turns)"
  echo "turns in polign_db: $(echo "$out" | jq -c '[.[]? | "\(.seq):\(.content)"]')"
  INPROC="$out"
  echo "read by a new client: $(call 8105 /inproc/peek "{\"agent_id\":\"$id\"}")"
}
inproc_case "inproc-a-$RUN" 600 0
[[ "$INPROC" == *'"C"'* ]] || gap "inside the lease TTL, the reverted process overwrote turn C"
inproc_case "inproc-b-$RUN" 15 20
[[ "$INPROC" == *'"C"'* ]] || gap "after the lease TTL, the reverted process overwrote turn C"

if (( GAPS > 0 )); then
  say "$GAPS gaps in checks 7 to 10"
  exit 1
fi
say "All checks passed"
