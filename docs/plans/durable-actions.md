# Durable actions: intents, reconcile, fencing

Status: built 2026-10-17 on branch `at/durable-actions` (polign_db, recall,
polign). Not released. Differences from the design below: the fence is three
headers (`Polign-Lease-Name`, `-Holder`, `-Epoch`), because holder strings
contain colons, and it passes a newer epoch from the same holder, because
renewal moves the epoch on. Reconcile records each settled outcome as a tool
turn, which the kill test showed is needed. The kill test ran 540 trials;
see `polign/python/recall-langgraph/bench/README.md`. The real-model gate
pass has not run.

## Goal

An agent killed at any instruction, including in the middle of a tool call,
resumes with no duplicate external side effect and no completed work lost.
Measured by a kill test, not argued.

Phase 1 (resume, recall 0.6.0) made an agent *persistent*: a new process
picks up the goal, plan and last turns. It is not yet *durable*: nothing
records that an action is about to happen, so a crash between "send the
email" and "the email was sent" leaves the next instance guessing, and
guessing wrong means sending twice. That is the one story a snapshot cannot
fix either, and the one to lead with.

## Where things stand (recall 0.20.2, polign_db 0.14.x)

| Piece | Today | Gap |
|---|---|---|
| Lease | `LeaseBackend` on polign_db, epoch per grant, `guard()` renews at TTL/3 and refuses writes once lost (`internal/engine/agent.go`) | Epoch is written into record metadata but the server never checks it. A process paused past its TTL whose `Put` lands late is not refused. Fencing is a claim, not a fact. |
| Working state | head + versioned copies, `Milestone` | Fine as is. |
| Turns | recorded by `message_id`, paged by 64 | `recall-langgraph` records turns in `pre_model_hook`, so a tool that ran but whose `ToolMessage` never reached the next model call is never recorded. That is exactly the crash window that matters. |
| Outputs, pointers | stored, referenced from the briefing | Fine as is. |
| Tool calls | run by LangGraph's `ToolNode`, unknown to Recall | No record before the call, no class, no idempotency key. |
| Resume | `Client.Resume` -> `load` -> briefing | No step that looks for unfinished actions. |
| MCP (`cmd/recall/mcp_agent.go`) | `agent_resume agent_acquire agent_release update_working_state agent_milestone working_state_history record_turn recent_turns store_output fetch_output set_pointer list_pointers remove_pointer defer_lease` | No action tools. |
| Python (`python/polign_recall/client.py`) | `resume acquire update_working_state milestone record_turn recent_turns store_output fetch_output set_pointer pointers release` | No action methods. |
| Gate (`polign/python/recall-langgraph/bench/gate.py`) | kills at a model call; passed 2026-09-26 (+4.4% tokens) | Never kills inside a tool call. No side-effect counter. |

## Design

### Action record

One record per tool call, kind `action`, id `<agent>:act:<seq>`, paged like
turns. Written twice with the same id, so the second write replaces the
first (the backend `Put` is by id):

```
begin: {seq, tool, args, class, key, status: "started", started_at, epoch}
end:   {... , status: "done" | "failed", result | output_ref, ended_at}
```

`class` is one of:

- `pure`: reading, searching, listing. Safe to run again.
- `idempotent`: the tool takes `key` and the outside system dedupes on it
  (PUT by id, `git push` of a known sha, a payment with an idempotency key).
  Safe to run again with the same key.
- `external`: everything else that touches the world (send, post, charge
  without a key, delete). Never run again blindly.

`key` is `sha256(agent_id ":" seq)`, fixed at `begin`, so a re-run after a
crash presents the same key. The engine passes it to the tool; the tool is
responsible for forwarding it.

Args and results above `OutputThreshold` go through `StoreOutput` and the
record keeps the ref, the same as turns do today.

Trade-off: overwriting the record loses the bare "started" version. The turn
log still holds the model's tool call, and `ended_at` sits beside
`started_at` in the final record, so the history is recoverable. Keeping two
records would make the pending scan a join; one record with a `status`
filter keeps it a single list call.

### Pending actions and reconcile

`load` lists `record=action status=started` for the agent (bounded to the
last `maxRecentTurns` pages). The result goes into `ResumeContext.Pending`
and into the briefing as its own section, ahead of working state:

```
## Unfinished actions
1. send_email(to=..., subject=...) [external] started 2026-10-17T18:02:11Z
   Not known whether this happened. Check before repeating.
2. write_file(path=src/a.py) [idempotent, key 9f3a...] started ...
   Will be re-run with the same key before you continue.
```

`Agent.Reconcile(ctx, r Resolver)` resolves each pending action before the
model takes a step:

| Class | What reconcile does |
|---|---|
| pure | marks `abandoned`; the model will redo it if it still needs it. |
| idempotent | returns it to the harness, which re-runs the tool with the same key and ends the action with the result. |
| external | calls `r.Check(action)` if the harness gave one (the harness can ask the outside system: "is there a sent mail with this subject since started_at?"). If it answers, the action ends `done` or `failed`. If there is no resolver or it cannot tell, the action ends `unknown` and stays in the briefing as a warning, and an open question is appended to the working state. |

Unclassified tools are refused when durable mode is on. The default must be
"this could hurt", not "replay"; one duplicate email on a demo ends the
pitch. (Decision 1 below.)

### Server-side fencing (polign_db)

Every agent record write already carries the epoch. The server should check
it. `PUT /v1/collections/{c}/vectors/{id}` and the batch form accept a
header `If-Lease: <lease name>:<epoch>`; the server reads the chain at
`.agent-lease/<key>/.lease/<ns/name>/` and refuses with 412 when the chain's
epoch is newer or the holder differs. Writes without the header behave as
today. The chain read is cached per request burst; the lease code already
has `Peek`.

pgvector backend: same check inside the write transaction.
Qdrant: no compare-and-swap, so no fencing. Document it in the backend
matrix; `-agent` is already refused there.

Without this, the zombie window is: `guard()` passes (epoch younger than
TTL/3), the process stalls for longer than the TTL, the `Put` lands after a
new holder has written. Narrow, but it is the difference between "fencing
token" meaning something and not.

### Harness (recall-langgraph)

`recall_langgraph.durable(tools, classes={"send_email": "external", ...})`
wraps each `BaseTool`:

1. `begin_action` with the call's args and class.
2. Invoke the tool. For `idempotent` tools, pass `key` as the
   `idempotency_key` argument when the tool declares one.
3. `end_action` with the result (or the error), and `record_turn` for the
   `ToolMessage` right there, closing the lost-ToolMessage gap.

`RecallResume.open()` runs `Reconcile` after the lease is held and before
the first model call. Idempotent re-runs happen inside `open()`; their
results are injected as `ToolMessage`s so the model sees them as ordinary
history. A `resolver=` callable handles `external`.

## Phases

### 1. Engine (recall)

`internal/engine/agent.go`, `agent_context.go`: `Action`, `BeginAction`,
`EndAction`, `Pending`, `Reconcile`, `Resolver`; `ResumeContext.Pending`;
briefing section; `recAction` kind. Tests in the `agent_test.go` /
`replay_test.go` style on the fake backend: begin without end survives
resume; end replaces; pending scan ignores done; reconcile per class;
briefing text. About 3 days.

### 2. Fencing (polign_db, then recall adapter)

`If-Lease` on vector PUT and batch PUT; 412 on a stale epoch; test with two
holders. Recall's polign adapter sends the header when an agent write
carries an epoch; `ErrLeaseLost` on 412. pgvector transaction check. Matrix
row. About 3 days. Can ship one release after Phase 1 if time is short, but
the launch post should not say "fenced" until it has.

### 3. MCP and Python client (recall, same tag as Phase 1)

Tools `begin_action`, `end_action`, `pending_actions`; `agent_resume`
output gains `pending`. Python `ResumedAgent.begin_action / end_action /
pending / reconcile`. Memory API spec test updated
(`memory_api_spec_test.go`). About 1 day.

### 4. recall-langgraph 0.2.0

`durable()` wrapper, `classes=` and `resolver=` on `RecallResume`, refuse
unclassified tools, reconcile in `open()`, `ToolMessage` recorded in the
wrapper. README section with the three classes and one worked
`send_email` example. About 2 days.

### 5. Kill test

New `bench/kill_actions.py` beside `gate.py`. A scripted agent (no model,
like `examples/resume`) drives one `pure`, one `idempotent` and one
`external` tool against a local "mail server" that counts calls per key and
raw. Kill points: after `begin` before the tool runs; after the tool runs
before `end`; after `end` before the next turn. 20 runs per cell, 180 runs,
free and deterministic. Pass: zero duplicate `external` effects, exactly one
effective `idempotent` effect per action, every run finishes.

Then one real-model pass of `gate.py` with the tools wrapped, to confirm the
token overhead is still under 10%. Claim-bearing numbers come from the
current frontier model; estimate the dollars and ask before running
(Decision 3). About 2 days.

### 6. Docs and site

`docs/architecture.md` gains an "Actions" section and the fencing
statement; mirror to polign.com/docs/architecture. recall-langgraph README.
One launch post in the technology style (problem, introducing, how it
works, limitations, availability), led by the duplicate-email case, with
the kill-test table. No negatives about Recall; limitations stated as
scope. About 1 day.

### 7. Release order

1. polign_db with `If-Lease` (tag by Anup).
2. recall: CHANGELOG heading, `python/pyproject.toml`, `__init__` bumped
   before the tag; one tag ships module, binary, cask, wheels.
3. recall-langgraph 0.2.0 on PyPI.
4. polign_web push (autodeploys).

About 12 to 13 working days in all.

## Decisions for Anup

1. Unclassified tool in durable mode: refuse at construction (recommended)
   or treat as `external`. Refusing makes the author think once per tool;
   treating as external is quieter but hides mistakes until a resume.
2. Fencing in the same release as Phase 1 (recommended) or the next one.
3. Which model and budget for the real-model gate pass.

## Out of scope

WIP-branch push and the Kubernetes recipe from the Phase 2 list (files
belong in git either way, and agent-sandbox already shows the pod dying).
Qdrant fencing. An OpenAI Agents SDK adapter. Any model-judged resolution of
`external` actions: the resolver is code or a human, never a guess.

## Risks

- Tool authors misclassify. The kill test only covers the three sample
  tools; the README has to make the classes impossible to misread.
- Pending scan cost on an agent with thousands of actions: bounded by
  pages, same as turns, but worth measuring in Phase 5.
- `If-Lease` adds a chain read per agent write on polign_db. Cache it; if it
  shows up in the warm-path numbers, batch the agent writes instead.
