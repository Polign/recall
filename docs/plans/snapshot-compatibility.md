# State that survives process snapshots

Status: plan, written 2026-11-09 against recall 0.25.0 and Agent Substrate
v1.0.0. Phase 0 ran on kind on 2026-11-16 (results below); Phases 1 to 4 not
started. The table and the in-process design were corrected from the run.

## Goal

An agent whose process is snapshotted and restored by its runtime keeps
correct Recall state in every case the runtime allows:

1. **Suspend and resume.** The same process continues later. Works today
   (`deploy/agent-substrate/run-kind.sh`, checks 4 and 6).
2. **Rollback.** The process restarts from an older snapshot than its last
   write to Recall. On Substrate this is `RevertActor`, and it also happens
   with no one asking: an asynchronous suspend returns before the snapshot is
   uploaded, so a lost node puts the actor back on the previous durable
   snapshot.
3. **Retry across a suspend.** Substrate does not keep open connections, so a
   write in flight at suspend is retried without knowing whether it landed.
4. **Clone.** A new actor starts from another actor's snapshot (a tag, or the
   template's golden snapshot) and wakes up holding the source's identity in
   memory.

Recall's records are never rolled back to match a snapshot. The records are
what happened; the restored process is the one that is behind, and the work
is to tell it so and tell it what it missed.

Measured the way durable actions were: a kind script with a side-effect
counter, not an argument. Done means a reverted actor records no duplicate
turn and repeats no finished external action.

## Where things stand (recall 0.25.0)

| Case | Today | Gap |
|---|---|---|
| Caller behind the records, over HTTP | The server holds the `Agent` with the true `turnSeq`, `actSeq` and working state version (`internal/engine/agent.go`). A write with an identity-bound credential needs no epoch (`checkLease` in `cmd/recall/serve.go`, `p.Agent != ""`). | Nothing tells the server what the caller last saw, so a reverted actor's writes are accepted. It records the same turns again under new seqs and begins an external action that already finished. |
| Turns | `record_turn` stores `message_id` (`RecordTurn`). | The id is stored and never checked. A retried or replayed turn is a second turn. |
| Actions | `begin_action` always takes `actSeq + 1`; the key is derived from the seq (`actionKey`). | A replayed begin is a new action with a new key, so even an `idempotent` tool runs twice. `end_action` on a closed action is an error, so a retried end fails. |
| Process holding the lease itself (Go, stdio MCP inside the actor) | The `Agent` and its seqs are in the snapshot. Writes are `Put` by id `<agent>:turn:<seq>`. | Reproduced (check 10): a reverted process writes seqs it already used and the put replaces the later records, inside the lease TTL and past it. Past the TTL nothing stops it: the restored process's monotonic clock does not count the time it was suspended, so the lease guard never renews, and the polign_db fence does not check expiry, only holder, release and epoch. |
| Suspended actors and the server | `recall serve` keeps every resumed agent, and renews its lease every TTL/3, for the life of the process (`agentRuntime` in `cmd/recall/mcp_agent.go`). | One goroutine and one renew every 20 s for each idle actor, on a runtime built to hold many idle actors. After a server restart a restored actor gets `not_resumed`; a recreated actor gets `already_resumed`. Both make the actor do lease bookkeeping its token already answers. |
| Clone | The token names the new actor, so the namespace is the new actor's and is empty. | A clone that sends the `agent_id` it has in memory gets `403 namespace_mismatch`. There is no way to start a new agent from another agent's state. |

## Design

### Position

A position is three numbers the engine already keeps:

```
{"turns": 41, "actions": 7, "state": 12}
```

Every State API answer carries `position` after the operation. A write may
carry `at`, the position the caller last saw, in the body or as
`If-Position: 41.7.12`. When the records are ahead of `at`, the write is
refused:

```
409 position_behind
{"error": {"code": "position_behind", ...}, "position": {...}}
```

The caller then asks what it missed. One new read-only tool, `agent_since`
(HTTP verb `since`), takes a position and returns the turns, the actions in
every status, and the working state versions written after it, within a
token budget, with a briefing for the model: what you did after the point
you were restored to, and which actions are finished, failed or unknown. The
caller adopts the returned position and carries on.

`at` ahead of the records is refused the same way with `position_ahead`; it
means the backend lost writes, and the caller should not paper over it.

### Writes that are safe to repeat

- `record_turn` with a `message_id` already recorded returns the recorded
  turn. Lookup is the server's set of recent ids (loaded at resume), then a
  metadata filter on `message_id`.
- `begin_action` takes an optional `call_id`, the harness's id for the tool
  call (the model's tool call id). A `call_id` already begun returns that
  action with its current status and result, so a restored harness sees
  `done` and skips the call, or sees `started` and settles it as it would
  after a crash.
- `end_action` repeated with the same status and result returns the action.
  A different outcome is still an error: the first outcome stands.

These two parts cover each other. The position check catches a rollback of
any size; the ids make the common small replay, and every blind retry, a
no-op without a round trip.

### Identity-bound callers stop managing the lease

For a credential bound to one agent (Substrate actor JWT, Kubernetes key):

- A call for an agent the server does not hold resumes it in place, so
  `not_resumed` after a server restart goes away.
- `agent_resume` for an agent already held with no session rebuilds the
  context and answers, instead of `already_resumed`.
- An agent with no call for `-agent-idle` (default 5 minutes) is released.
  The next call takes the lease again. The lease is still what stops two
  holders; the server just stops holding it for an actor that is asleep.

Callers with a plain key keep today's behaviour.

### In-process holders

Phase 0 reproduced the overwrite, and showed the wall-clock check alone is
not enough: in check 10's first case the revert and the next write came 1.2 s
of wall time after the snapshot, well inside any renew interval. So:

- Turn and action-begin records become create-only writes. A collision means
  the process is behind: the `Agent` rereads its seqs from the head and
  returns `position_behind`. pgvector and MongoDB can do this today (insert
  instead of upsert, in the fenced transaction). polign_db cannot: its vector
  PUT has no create-only form, so it needs one (`If-None-Match: *` beside the
  lease fence headers). Its object store already has `PutIfAbsent`.
- As a second, cheap signal, `guard` compares wall-clock readings
  (`time.Now().Round(0)`, which drops the monotonic reading): a jump of more
  than TTL/3 since the last grant means the process was suspended, so it
  renews before the write. Go's `time.Since` alone cannot see it; check 10
  measured 6 ms of monotonic time across a 21 s suspend.
- Whether the polign_db fence should refuse an expired lease nobody took over
  is a separate question. With create-only writes it no longer matters for
  this case, so it stays out of scope.

### Fork (designed, not built)

`agent_fork` would copy a source agent's state as of an instant into a new
agent and record the lineage, with a rule for who may fork whom. It adds a
tool and an authorization rule, and nobody has asked for clones with Recall.
For now the clone case gets a clear error and a documented pattern: read the
actor name from `/run/ate/actor-name` at time of use, and do not call
`agent_resume` before the golden snapshot is taken.

## Phases

Sizes are estimates.

### 0. Reproduce on kind (about 1 day)

Add failing checks to `deploy/agent-substrate/run-kind.sh`, with the demo
actor counting side effects in a file outside the actor:

- 7: record turns and finish one `external` action, `kubectl ate revert
  actor`, run the same steps. Count turns and side effects.
- 8: kill the connection during `record_turn` and `begin_action`, retry.
- 9: tag the actor, create a second actor from the tag, call the State API.
- 10: `recall mcp -agent` over stdio inside the actor against a Fence
  backend; revert inside the lease TTL and after it.

Write what each one actually did into this file. If 7 or 10 behaves
differently from the table above, fix the design before Phase 1.

#### Phase 0 results (2026-11-16, kind, Substrate v1.0.0, gVisor)

`SKIP_BASE=1 ./run-kind.sh` runs checks 7 to 10. Checks 1 to 6 still pass.
The demo actor now runs a harness whose progress and agent id are process
memory, and an in-process Go holder against a polign_db 0.15.0 the actor
reaches directly (`polign.yaml`). The script counts side effects outside the
actor. Seven gaps, all predicted except where noted:

- **7, rollback.** After `kubectl ate revert` to a sync snapshot taken just
  after resume, the harness ran its job again: 4 turns for 2 message ids, 2
  invoices sent, action seq 2 begun for the same call. `pending_actions` right
  after the revert was empty, since the first action had ended `done`, so a
  harness that checks pending work first still gets no signal. Only the
  position check can catch this.
- **8, retries** (a lost answer, simulated by sending each request twice; the
  first lands): `record_turn` with the same `message_id` gave 2 turns;
  `begin_action` gave seqs 3 and 4 with different keys, so an idempotent
  tool would run twice; a repeated `end_action` with the same outcome got
  `400 tool_error`.
- **9, clone.** As the table says: the clone wakes holding `agent-3`, and its
  calls get `403 namespace_mismatch` ("the credential does not carry agent_id
  agent-3"). Clear enough to stop it, but it does not say why. The clone
  resumes itself as `fresh`. Not counted as a gap.
- **10, in process.** Turns A, B, snapshot, C, D, revert, E, F. A new client
  reading the records sees `1:A 2:B 3:E 4:F`: C and D are gone. This happened
  with a 600 s TTL and no wait, and also with a 15 s TTL and a 20 s wait after
  the revert, which the table had expected to be safe. The epoch stayed 1
  throughout. The restored process's monotonic clock read 3755 ms before and
  3761 ms after a 21 s suspend, so `guard` never renewed, and the backend
  fence accepted the expired epoch because nobody else had taken the lease.
  This changes the design (In-process holders, above) and means a polign_db
  change is needed.

Script notes: the first connection through `kubectl ate proxy` after a
revert or a clone can fail, so the harness calls retry an empty answer. Recall
keeps an actor's records across runs of the script, so check 7 counts turns
from where they stood when the check started.

### 1. Engine (about 2 days)

`internal/engine/agent.go`, `agent_action.go`, `recall.go`:

- `Position`, `Agent.Position()`, `Agent.Since(ctx, Position, budget)`.
- `RecordTurn` dedupe on `MessageID`; `message_id` added to the turn
  record's metadata.
- `BeginAction` with `CallID`; `EndAction` repeat.
- The in-process fix chosen in Phase 0.
- Unit tests for each, plus a rollback test that restores an `Agent` value
  copied earlier and writes through it.

### 2. Server and wire (about 2 days)

`cmd/recall/mcp_agent.go`, `serve.go`:

- `position` in every State API answer; `at` and `If-Position`;
  `position_behind` and `position_ahead` in `stateError`.
- `agent_since` tool and `since` verb; `call_id` on `begin_action`.
- Resume in place, idle release and `-agent-idle` for bound callers.
- `docs/state-api-v1.md`: tool table, a Position section, the two error
  codes, the Substrate paragraph. The spec test in `cmd/recall` fails until
  the table matches.

### 3. Clients (about 2 days)

- Python (`python/polign_recall/client.py`): `ResumedAgent` keeps the
  position from every answer and sends `If-Position`; `position_behind`
  raises `RolledBack`; `agent.catch_up()` calls `since` and adopts the
  position; `begin_action(call_id=...)`.
- Polign/polign, after the recall release: recall-langgraph and
  recall-openai-agents pass message ids and tool call ids; the Hermes
  plugin's turn save passes a message id.
- The demo actor in `deploy/agent-substrate/actor` uses all of it.

### 4. Prove it and document it (about 1 day)

- Checks 7 to 10 pass; check 9 shows the clear error.
- `deploy/agent-substrate/README.md`: revert and clone under "What to know".
- CHANGELOG, then the site's Agent Substrate page once released.

### Release

One recall minor (0.26.0) for Phases 1 to 4, merged to main as each phase
is done and tagged on a Friday. The integrations follow in Polign/polign.
Phase 0 showed polign_db needs a create-only vector put. That ships first,
and recall's polign backend uses it when the server has it.

## Decisions for Anup

1. **Is `at` required?** Recommended: required for Substrate callers, where
   rollback is a runtime feature and the mode shipped today with no users to
   break; optional everywhere else, with the Python client always sending it.
2. **Resume in place for bound callers.** It changes the `already_resumed`
   behaviour documented in 0.25.0. Recommended: yes.
3. **Fork.** Recommended: leave out until someone uses tag clones with
   Recall.
4. **Names.** `position`, `at`, `If-Position`, `position_behind`,
   `agent_since`, `call_id`.

## Out of scope

- Rolling Recall's records back to a snapshot's point in time.
- Snapshotting `recall serve` itself, or running more than one replica of
  it; the held agents live in one process.
- Backends without leases (Qdrant), which do not serve the State API.
- Memory (`remember`, `recall`): a replayed `remember` of the same statement
  already folds to nothing.

## Risks

- **Models do not carry positions.** Over MCP driven by a model, `at` stays
  optional, so those callers get the id dedupe and not the rollback check.
  The harness integrations are where the check is real.
- **Filtered listings can lag** (the note in `PendingActions`). The recent-id
  set covers the window that matters; a replay older than the set and newer
  than the index is caught by the position check when `at` is sent.
- **Idle release widens a takeover window.** Between release and the next
  call another holder can take the agent. For a bound caller only the same
  identity can, and the backend fence still refuses the stale one.
- **Phase 0 may show Substrate behaving differently** from its documents, as
  the DNS policy matching did. The table is to be corrected from the run.
