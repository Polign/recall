# Recall State API, version 1

Recall keeps two things for an agent. **Memory** is what the agent knows: statements about the world, folded into current beliefs, with the record of how they changed. The [Memory API](memory-api-v1.md) is that contract. **State** is where the agent is: one record set per `agent_id`, fenced by a lease so that one process acts for the agent at a time, holding its working state, its turns, its stored outputs, pointers to its work, and the actions it began. This document is the State API contract: the operations, what each carries, how the lease works across transports, and what a backend has to provide.

Before Recall 0.24.0 this contract was section 6 of the Memory API document, under the name Resume API. The tools, their arguments and the records they write are unchanged; the name and the HTTP transport (section 4) are new in 0.24.0.

The MCP tool definitions served by `recall mcp -agent` and `recall serve` are the authoritative form. The tables in this document are checked against them, and against the HTTP routes, by a test in `cmd/recall`, so a tool cannot change without this document changing.

Status: **Beta**. The tools and arguments here may change in a minor release, listed in the changelog as a breaking change with a migration note; the records they write are stable. See [STABILITY.md](../STABILITY.md).

## 1. The model in one paragraph

An agent's state is a set of records under its `agent_id`. The **working state** is the agent's note to its next instance: goal, plan, progress, focus, decisions, open questions, notes. Each update supersedes it whole and the earlier versions are kept. **Turns** are the messages of the run, appended verbatim. **Outputs** are large tool results stored whole, referenced from turns instead of carried in context. **Pointers** are named references to where the work lives: a git ref, an object, an environment, an external id, a process. **Actions** are one record per tool call that touches the world, begun before the call and ended after it, so a crash between the two is visible. Reading the set back is a **resume**: the server rebuilds the context the agent should start from, within a token budget, and renders it as a briefing for the model. A **lease** on the `agent_id` makes writes safe: one holder at a time, each grant with an epoch that only grows, and a write under an older epoch refused.

## 2. Tools

Gated: the tools are served when the backend has leases (section 6) and the server has the agent tools on, which is the default for `recall serve` and `-agent` for `recall mcp`. A client checks for `agent_resume` in the tool list (`agent_enabled` in Python) before relying on them. Over HTTP a server without them answers `501 unsupported`.

| Tool | HTTP verb | Required arguments | Read-only |
| --- | --- | --- | --- |
| agent_resume | resume | agent_id | no |
| agent_acquire | acquire | agent_id | no |
| agent_release | release | agent_id | no |
| update_working_state | update_working_state | agent_id | no |
| agent_milestone | milestone | agent_id, name | no |
| record_turn | record_turn | agent_id, role, content | no |
| recent_turns | recent_turns | agent_id | yes |
| store_output | store_output | agent_id, content | no |
| fetch_output | fetch_output | agent_id, ref | yes |
| set_pointer | set_pointer | agent_id, name, type, fields | no |
| remove_pointer | remove_pointer | agent_id, name | no |
| list_pointers | list_pointers | agent_id | yes |
| working_state_history | working_state_history | agent_id | yes |
| begin_action | begin_action | agent_id, tool, class | no |
| end_action | end_action | agent_id, seq, status | no |
| pending_actions | pending_actions | agent_id | yes |
| agent_context | context | agent_id | yes |
| agent_since | since | agent_id, at | yes |

The HTTP verb is the path under `/v1/agent/` (section 4). The tool names are the MCP names and do not change; the verbs drop the `agent_` prefix where there is one.

Semantics: `agent_resume` takes the agent's lease and rebuilds its context (working state, pointers, relevant memories, recent turns, unfinished actions) within a token budget. It is also how an agent starts for the first time; `fresh` is then true. Its answer carries `epoch`, the lease epoch (section 3). Writes fail while the lease is not held. With `defer_lease`, `agent_resume` returns the context at once without the lease, even while a crashed process's lease is live; writes then fail with `lease_not_held` until `agent_acquire` succeeds, which returns the epoch. `agent_release` hands the lease over at once, so the next process need not wait out the TTL. `agent_id` is 1 to 128 characters from letters, digits, `.`, `_`, `-`.

Actions: a harness calls `begin_action` before each tool call that touches the world and `end_action` after it. `class` is `pure` (reads only), `idempotent` (repeating with the returned `key` does nothing more) or `external` (could happen twice). A crash between the two leaves the action open. On resume, open pure actions are closed as `abandoned`, open external actions are marked `unknown`, and open idempotent actions wait to be run again with the same key; every action still open is listed in `pending_actions` and at the top of the briefing until `end_action` closes it. Repeating `end_action` with the outcome already recorded returns the action; a different outcome is an error, since the first one stands. `agent_context` rebuilds the briefing after a harness has settled them. Recall never runs a tool itself. `begin_action` with `call_id`, the harness's id for the tool call, returns the action already begun under that id, in whatever status it is, instead of beginning another: a harness restored from an earlier snapshot sees `done` and skips the call. `record_turn` with a `message_id` already recorded returns the recorded turn and writes nothing.

Argument conventions are the Memory API's: instants are RFC 3339 strings, and unknown arguments are rejected (over HTTP, `epoch` is the one field this document adds, section 4). `at` is a position (section 3.1).

## 3. The lease

Every agent has one lease, named by its `agent_id`, in the backend. Taking it returns an **epoch**, a number that only grows: each grant is a higher epoch than the last, and a holder presenting an old one has been superseded. Renewal may advance the epoch too; what a caller holds is the epoch the server issued to it, and the server keeps that association for as long as it holds the agent. On a backend with the Fence capability (section 6) the backend checks the holder's epoch on every write, so a process that stalled past its lease is refused even when its own clock says the lease is fresh; elsewhere the client checks it before each write.

Who holds the lease depends on the transport:

- **In process** (Go, Python over stdio, MCP over stdio): the `Agent` holds it, renews it in the background, and releases it on `Release`, on `agent_release`, or when the session ends. Nothing is carried per call.
- **HTTP** (`recall serve`, both `/mcp` and `/v1/agent/*`): requests have no session of their own, so the server holds the resumed agent, and its lease, in the caller's namespace for the life of the process. `agent_resume` returns the epoch; a caller presents it on every write, and the server refuses an epoch it did not issue for the agent it holds with `409 lease_lost`. A second `agent_resume` for an agent the server already holds answers `409 already_resumed`; `agent_release` with the epoch hands it over, and the next `agent_resume` issues a new epoch. If the server restarts, the agents it held are gone and their leases lapse at their TTL: a write then answers `409 not_resumed`, and the caller resumes again.
- **Bound credentials** (Kubernetes mode, section 5, and Agent Substrate actors): the credential is bound to one `agent_id`, so the server holds the lease for it and a write need not carry the epoch. A call for an agent the server does not hold resumes it in place, so a server restart is invisible to the caller, and `agent_resume` for an agent already held rebuilds its context and answers instead of `already_resumed`. An agent with no call for `-agent-idle` (default 5 minutes) is released, and its next call takes the lease again. In Kubernetes mode the key also names the pod as its session: a new pod carrying a key for the same agent takes the agent over from the pod it replaced, and the old pod's writes are refused.

### 3.1 Position

A process can come back behind its own records: its runtime restored it from an earlier snapshot, or it retried a write without knowing whether the first one landed. Recall never rolls the records back. It tells the caller it is behind instead.

The agent's **position** is three numbers: the newest turn seq, the newest action seq and the working state version, written `turns.actions.state` (`41.7.12`) or as an object `{"turns": 41, "actions": 7, "state": 12}`. Every State API answer carries the position after the call: a `position` field in an object answer, and over HTTP the `Recall-Position` header on every answer.

`record_turn`, `begin_action`, `end_action`, `update_working_state` and `agent_milestone` take an optional `at`, the position the caller last saw, as the body field or the `If-Position` header. When the records are ahead of it the write is refused with `409 position_behind`, and the answer carries the records' `position` and the `at` it refused. The caller calls `agent_since` with its own position: the turns, the actions in every status, and the working state if it changed, with a briefing that tells the model those things happened and are not to be done again. It then continues from the position `agent_since` returns. A position ahead of the records is `409 position_ahead`: the backend lost writes the caller saw acknowledged.

A write the ids show was already made (a `message_id`, a `call_id`, an `end_action` with the outcome already recorded) returns what was recorded before `at` is checked, so a retry carrying the position from before its first attempt is not refused.

Over HTTP an Agent Substrate actor's writes must carry `at`; elsewhere it is optional. A process that holds the agent itself (Go, MCP over stdio) needs no `at`: on a backend with Create (section 6) its turns, action begins and working state versions are create-only, so a restored process that writes a seq it already used is refused with `position_behind` and continues after the newest record.

## 4. HTTP transport

`recall serve` serves every tool in section 2 at `POST /v1/agent/<verb>`, behind the same Recall key as the memory verbs. The body is the tool's arguments as a JSON object; the answer is the tool's result as JSON. The identity envelope of the Memory API applies: a request that names a `namespace`, `agent_id` or `session` the credential does not carry is refused with `403 namespace_mismatch`.

A write carries the lease epoch in one of two places, which must agree when both are given:

- the header `If-Lease: <agent_id>:<epoch>`, the form polign_db's own fence headers use;
- the body field `"epoch": <epoch>`, removed before the tool sees the arguments.

A write without either is refused with `400 invalid_argument`, unless the key is bound to the agent (Kubernetes mode), when the server holds the lease for the key. A read may carry the epoch, and it is checked when it does. `agent_resume` and `agent_acquire` take no epoch; they issue one.

```sh
R=$(curl -s -X POST $RECALL_URL/v1/agent/resume -H "Authorization: Bearer $KEY" \
  -d '{"agent_id":"billing-migrator"}')
EPOCH=$(echo "$R" | jq .epoch)      # the briefing is in .briefing
curl -s -X POST $RECALL_URL/v1/agent/update_working_state \
  -H "Authorization: Bearer $KEY" -H "If-Lease: billing-migrator:$EPOCH" \
  -d '{"agent_id":"billing-migrator","goal":"move billing to the v2 API"}'
curl -s -X POST $RECALL_URL/v1/agent/begin_action -H "Authorization: Bearer $KEY" \
  -d '{"agent_id":"billing-migrator","tool":"send_email","class":"external","epoch":'$EPOCH'}'
curl -s -X POST $RECALL_URL/v1/agent/release -H "Authorization: Bearer $KEY" \
  -d '{"agent_id":"billing-migrator","epoch":'$EPOCH'}'
```

`POST /mcp` serves the same tools as MCP over streamable HTTP. The MCP contract does not require the epoch, so a write without it is accepted there; an `If-Lease` header or `epoch` field that is given is checked the same way, and a refusal is a tool result, as every tool failure is over MCP. The Python client sends `If-Lease` on every call of a resumed agent over HTTP.

## 5. Kubernetes workload identity

On Kubernetes a pod needs no key of its own. With `recall serve -kubernetes`, `POST /v1/auth/kubernetes` with body `{"token": "<projected service account token>"}` is the one route without a bearer key. The server asks the API server to review the token (TokenReview, audience `recall` by default) and mints a short-lived key for the workload. Nothing in the key's identity comes from the client: the tenant is the service account's Kubernetes namespace, or a label on the service account named by `-kubernetes-tenant-label`; the agent is the pod's name, or the pod's label named by `-kubernetes-agent-label`, or with `-kubernetes-sandbox-claims` the SandboxClaim that owns the pod's Sandbox, so an agent keeps its identity across the pods a pool hands out. The key's namespace is `<tenant>:<agent>`, its `agent` the agent and its `session` the pod. A pod can therefore only ever get a key for itself, and with it only ever resume itself: a request that names another `agent_id` is refused with `403 namespace_mismatch`.

Kubernetes-mode keys default to a 15 minute TTL (`-kubernetes-key-ttl`) and are never renewed: `POST /v1/keys/renew` answers `409 renew_with_token`, and the workload exchanges its projected token again, so a key copied out of a pod dies at its TTL and a deleted pod's key cannot be extended. The answer to the exchange carries `key`, `ttl` and `expires_at`.

Clients: `RECALL_TOKEN_FILE` names the projected token (audience `recall`); `recall connect` and the Python client (`Client(url=..., token_file=...)` or `Client.from_env()`, which defaults to `/var/run/secrets/recall/token` when it exists) exchange and re-exchange it by themselves. `RECALL_AGENT` and `RECALL_SESSION` are informational in this mode; the server derives both.

### Agent Substrate actor identity

On Agent Substrate an actor holds no credential at all. With `recall serve -substrate-issuer <issuer>`, the server accepts an actor JWT as the bearer on every request; the actor's egress policy has Substrate's gateway put the token in the `Authorization` header (`actorJwt` with the audience `recall`, or the one `-substrate-audience` names). There is no exchange and no Recall key. The server checks the token against the issuer's discovery document and key set, and refuses `worker-for-actor` tokens. The subject `actor/<atespace>/<name>` gives the namespace `<atespace>:<name>`, or the atespace alone with `-substrate-scope atespace`, and the `agent` is the actor's name: a request that names another `agent_id` is refused with `403 namespace_mismatch`, and a write need not carry the epoch. The token carries no session, so an actor created again under a deleted actor's name reaches the agent the old actor left, resumed in place. An actor can be reverted to an earlier snapshot at any time, so its writes must carry `at` (section 3.1). Added in 0.25; resume in place and the required `at` in 0.26.

## 6. Backend requirements

The State API needs two capabilities beyond the Memory API's core, from the [backend capability matrix](memory-api-v1.md#7-backend-capability-matrix):

| Capability | What it enables | polign_db | Qdrant | Postgres + pgvector | MongoDB |
| --- | --- | --- | --- | --- | --- |
| Leases | the State API | yes | no | yes | yes |
| Fence | the backend refuses an agent write whose epoch the lease no longer backs | yes (lease fence headers) | no | yes (in the write's transaction) | yes (in the write's transaction on a replica set; checked just before the write on a standalone server) |
| Create | the backend writes an agent record only when its id holds none, so a restored in-process holder cannot replace later records | 0.16.0 and later (`If-None-Match: *`) | no | yes (`ON CONFLICT DO NOTHING`) | yes (insert) |

Without leases the tools are not served: `recall serve` logs it at start, `recall mcp -agent` refuses to start, and `POST /v1/agent/*` answers `501 unsupported`. Agent records live in their own collection, the memory collection's name with `_agents` appended.

## 7. Transports

| Transport | Status | Notes |
| --- | --- | --- |
| Go (`github.com/Polign/recall`) | 0.15 | `Client.Resume` returns an `Agent`; its methods are the tools above. [Resuming an agent](reference.md#resuming-an-agent). |
| MCP over stdio (`recall mcp -agent`) | 0.15 | Authoritative tool definitions. The session holds the lease. |
| Python (`polign-recall`) | 0.15 | `Client(agent=True).resume` returns a `ResumedAgent`; see [State](../python/README.md#state). Over stdio and over HTTP; over HTTP it sends `If-Lease`. |
| MCP over streamable HTTP (`recall serve`, `/mcp`) | 0.16 | Same tools; the server holds the lease; `If-Lease` checked when given. |
| HTTP (`recall serve`, `/v1/agent/*`) | 0.24 | Section 4. |
| recall-langgraph | 0.21 | `RecallResume` wraps resume and actions for LangGraph. |

## 8. Errors

Over HTTP these are the status and `error.code`; over MCP the same codes appear in the tool error text.

| Code | HTTP | Meaning |
| --- | --- | --- |
| unsupported | 501 | The backend has no leases, or the server was started without the agent tools. |
| lease_held | 409 | Another process holds the agent. Retry, or resume with `defer_lease`. |
| lease_lost | 409 | The epoch presented is not the one the server issued for the agent it holds, or the backend fenced the write: a later resume took the agent over. Resume again. |
| lease_not_held | 409 | A write after a deferred resume, before `agent_acquire` succeeded. |
| not_resumed | 409 | The server holds no such agent; call `agent_resume` first. After a server restart, every held agent is in this state. |
| already_resumed | 409 | `agent_resume` for an agent the server already holds for this caller, with a key not bound to the agent. Release it, or carry on with the epoch already issued. |
| position_behind | 409 | The records are ahead of the `at` the write carried: the process was restored from an earlier point, or another writer moved them. Nothing was written. The answer carries `position`; call `agent_since` with your own position. |
| position_ahead | 409 | The `at` the write carried is ahead of the records: the backend lost writes. Nothing was written. |
| namespace_mismatch | 403 | The request names a namespace, agent or session the credential does not carry. |
| invalid_argument | 400 | Unknown field, wrong type, bad instant, a write without the epoch, or an Agent Substrate actor's write without `at`. |
| tool_error | 400 | The operation refused the request for a reason in its message. |
