# polign-recall

<!-- mcp-name: io.github.polign/recall -->

A Python client for Recall, memory and state for agents. Requires Python
3.10+.

```sh
pip install polign-recall
```

That is the whole install. pip also brings the
[`polign_db`](https://pypi.org/project/polign-db/) package, which holds the
`polign` CLI and `polign-server` binaries for Linux, macOS and Windows, so
there is nothing else to download.

The client has two halves, matching Recall's two contracts. [Memory](#memory)
is what the agent knows: `remember`, `ask`, `forget_text` and the typed calls,
the [Memory API](../docs/memory-api-v1.md). [State](#state) is where the agent
is: `resume` and the `ResumedAgent` it returns, the
[State API](../docs/state-api-v1.md).

## Memory

```python
from polign_recall import Client

with Client(local_dir="./recall-data", extract_model="anthropic:claude-haiku-4-5-20251001") as memory:
    memory.remember(text="I switched from helix to zed last week.")
    for m in memory.ask("Which editor do I use?"):
        print(m.text, m.since, [e.text for e in m.before])  # user prefers editor: zed ... ['helix']
    memory.forget_text("my editor")
```

`remember(text=...)` hands the server a passage; the model named by
`extract_model` works out the facts in it, and a newer fact replaces an older
one. It returns a `TextResult` listing what was filed and what each statement
replaced. `ask` returns `Memory` sentences, each with `since`, `days_ago`, and
what it replaced under `before`; it accepts `as_of`. `forget_text` withdraws
the facts a description names and returns them; the texts they came from stay
as the record. Without `extract_model`, remembered text is kept as written:
`ask` finds it by its words, but it replaces nothing, and `forget_text` needs
a model.

Nothing is deleted, so the record of corrections can be read back:

```python
for t in memory.history_text("my editor"):       # every statement, oldest first
    for step in t.changes:
        print(step.when, step.change, step.ended, step.began)
memory.ask("Which editor do I use?", as_of="2026-10-01T00:00:00Z")  # believed then
memory.diff(since="2026-10-01T00:00:00Z")           # what ended and began since
```

`history_text` returns a `Timeline` per thing the question reaches, including
things since forgotten: `now` is what is believed, and each `Step` says whether
it `stated`, `replaced` or `withdrawn` something, with the sentences it `ended`
and `began`. `diff(since, until=None)` returns a `Change` per thing whose
beliefs differ between the two instants. Both need a recall server newer than
0.21; an older one raises `RecallError` with code `unsupported`.

`local_dir` keeps the database on this machine. The first client to open the
directory starts a `polign-server` for it in the background, listening on
localhost only and protected by a key stored in the directory. Later clients,
including ones in other processes, share that server. It keeps running after
your program exits; its process id is in `runtime.json` and its log in
`server.log`, both inside the directory. Managed local databases work on Linux
and macOS.

`backend`, `backend_url` and `backend_key` name any database, the same three
names as `recall -backend`, `-url` and `-key`: `backend` is `"local"`,
`"polign"`, `"pgvector"`, `"mongodb"` or `"qdrant"`, the URL is in that
backend's own form, and the key is the credential when the URL does not carry
one. `local_dir=X` is the short spelling of `backend="local", backend_url=X`.

```python
Client(backend="polign", backend_url="http://memory.internal:23000", backend_key="plgn_...")
Client(backend="pgvector", backend_url="postgres://recall:pw@db.internal/recall?schema=memory")
Client(backend="qdrant", backend_url="https://x.cloud.qdrant.io:6333", backend_key="...")
```

Given any of the three, the client hands the binary exactly those and nothing
the environment says about a database. Given none, the `recall` binary
resolves it: `RECALL_BACKEND`, `RECALL_BACKEND_URL` and `RECALL_BACKEND_KEY`
in the environment, then what `recall setup` saved, then a local database in
`~/.local/share/recall` that it runs itself and that every agent on the
machine shares. So a bare `Client()` works on a fresh machine with nothing
configured. The names used before recall 0.23 (`POLIGN_URL` and the rest)
still work in the environment, and a client given `backend=` against an older
binary says the same thing in the names that binary knows. `Client.database`
says where memory ended up, as the binary resolved it: a `Database` with the
`backend`, the `url` (a directory for local) and, for local, the loopback
`server` address; `None` from a binary older than 0.23 or a `recall serve`.

The client owns a long-lived `recall mcp -write -text` subprocess and shares
the Go implementation's validation and fold. Use `write=False` for a read-only
connection. It runs the `recall` binary that this package's platform wheel
installs next to `python`; on a platform without one it runs `recall` from
`PATH`, and without `recall` it falls back to `polign mcp -memory-only`
from polign_db 0.13 or earlier. `command=[...]` overrides all of these.

The model is `provider:model`: `anthropic:<model>`, `openai:<model>`, or
`ollama:<model>`, with keys from `ANTHROPIC_API_KEY` or `OPENAI_API_KEY`.
`POLIGN_EXTRACT_MODEL` in the environment does the same.

### Typed memory

With `typed=True`, the client uses the typed tools (`recall mcp -typed`,
recall 0.18 or later): your code names the subject, predicate and value, over
a fixed vocabulary.

```python
with Client(local_dir="./recall-data", typed=True) as memory:
    memory.remember("user", "prefers_editor", "vim")
    changed = memory.remember("user", "prefers_editor", "neovim")
    current = memory.recall("user", "prefers_editor")[0]
    print(current.value, current.replaced[0].value)  # neovim vim
    print(memory.history("user", "prefers_editor"))  # both statements
    for step in memory.timeline("user", "prefers_editor").changes:
        print(step.when, step.change, step.ended, step.began)  # ... replaced vim -> neovim
    memory.forget("user", "prefers_editor", "neovim")
```

`history` returns the raw events; `timeline` tells the same record as
changes, in the same `Timeline` and `Step` types `history_text` returns, with
`subject` and `predicate` set. `diff(since, until=None, subject=None)` works
here too, and each `Change` also carries its `subject`, `predicate`, and the
beliefs `before` and `after`.

Read `predicates()` for the vocabulary: the starter registry, or the file
named by `POLIGN_PREDICATES`. `remember(text=..., statements=[...])` accepts
proposals from your agent's model; every statement must have subject,
predicate, typed value, and evidence quoting the text exactly. A proposal
that names an unlisted predicate is dropped, and the whole text is kept as a
note either way. With `extract_model` and no statements, the server's model
proposes them over the same vocabulary.

The text calls (`ask`, `forget_text`, `remember(text=...)` alone) raise
`ValueError` on a typed client, and the typed calls (`recall`, `forget`,
`history`, `predicates`) raise on a text client. With `command=[...]` or
`url=...`, the client follows whichever tools the server lists; `typed=True`
or `text=True` against the other surface is an error at open.

`recall(..., as_of=...)` accepts an RFC3339 string or timezone-aware datetime.
`forget` requires a typed value or explicit `all=True`; false and zero remain
values. Forgetting retracts a belief and retains its historical events.

Errors raise `RecallError` with a code and, for a failed extraction batch, any
reported partial results. Calls are serialized, timeout after 120 seconds by
default, and never automatically retry writes. A timeout closes the transport;
inspect history using a new client before retrying an uncertain write.

## Connecting to a server

When memory is served by `recall serve` rather than a local process, give the
client the server and one credential and it speaks HTTP instead:

```python
from polign_recall import Client

client = Client(url="http://recall.recall.svc:8080", api_key="rcl_...")
```

Inside a Kubernetes sandbox the credential is the pod's projected service
account token, which the server exchanges for a short-lived key and the
client renews:

```python
client = Client(url="http://recall.recall.svc:8080", token_file="/var/run/secrets/recall/token")
```

`Client.from_env()` reads `RECALL_URL`, `RECALL_API_KEY` and
`RECALL_TOKEN_FILE` (default `/var/run/secrets/recall/token` when it exists)
and falls back to a local process when `RECALL_URL` is unset, so the same
program runs on a laptop and in a sandbox. Over HTTP the server decides
whether it serves the text or the typed tools and whether the resume tools
exist; the client finds out when it connects.

## State

Memory is what the agent knows. State is where the agent is: one set of
records per agent id, holding its working state, its turns, large tool
outputs, pointers to its work, and the tool calls it began. A lease on the id
keeps one process acting for the agent at a time. The contract is
[docs/state-api-v1.md](../docs/state-api-v1.md); it is a beta surface, so
the calls here may change in a minor release with a note in the changelog,
while the records they write are stable.

### Resuming an agent

An agent that crashes or moves to another machine can pick up from its own
records instead of a snapshot of its process. Open the client with
`agent=True` and call `resume`:

```python
from polign_recall import Client

with Client(local_dir="./recall-data", agent=True) as client:
    with client.resume("billing-migrator") as agent:
        print(agent.context.briefing)  # hand this to the model as its starting context
        agent.update_working_state(goal="Move billing to the v2 API",
                                   plan=["find call sites", "migrate them", "run the tests"])
        agent.record_turn("user", "Start with the charge endpoint.")
        agent.set_pointer("wip", "git_ref", {"repo": "github.com/acme/billing", "branch": "agent/wip"})
        agent.milestone("call sites found", progress="12 call sites listed")
```

The first `resume` of an id starts the agent fresh (`context.fresh` is true).
Every later one rebuilds `context` from what the agent wrote: its working
state, pointers to its work, relevant memories, and its most recent turns,
trimmed to `token_budget`. `context.briefing` is all of that as one text for
the model; the same parts are also there as fields.

`resume` takes a lease on the agent id, so only one process acts for it at a
time. While another process holds it, `resume` raises `RecallError` with code
`"lease_held"`. The lease is renewed in the background and handed over when
you call `release()`, leave the `with` block, or close the client.

If the process before crashed without releasing, `resume` has to wait for its
lease to expire. When you cannot wait, as on a live call, pass
`defer_lease=True`: `resume` returns the context at once without the lease,
because reading cannot conflict with anyone. Writes then raise `RecallError`
with code `"lease_not_held"`, so keep them in a buffer and call
`agent.acquire()` until it returns True:

```python
agent = client.resume("call-42", defer_lease=True, lease_ttl=6)
greet_with(agent.context.briefing)  # no waiting
while not agent.acquire():          # False while the crashed process's lease is live
    time.sleep(1)
agent.record_turn("assistant", "Sorry, we got cut off.")
```

Once it holds the lease, anything the crashed process wrote after the
resume is picked up, and new turns continue after it.

The other methods write what the next resume reads: `update_working_state`
(fields you leave out are kept), `milestone`, `record_turn` (a turn longer
than the output threshold is stored whole and the turn keeps a reference),
`store_output` and `fetch_output` for large tool results, `set_pointer`,
`remove_pointer` and `pointers`, and `recent_turns` and
`working_state_history` to read back.

### Tool calls that touch the world

The records above say what the agent was doing, not whether its last tool
call happened. Record each call that changes something before it runs, and
close it when it returns:

```python
action = agent.begin_action("send_email", "external", {"to": to, "subject": subject})
result = mailer.send(to, subject)
agent.end_action(action.seq, "done", result=result)
```

The class says what running the call again would do: `"pure"` (reads only),
`"idempotent"` (repeating it with `action.key` does nothing more, so pass
the key on) or `"external"` (it could happen twice). If the process dies
between `begin_action` and `end_action`, the call is still open on the next
resume. Interrupted pure calls are closed for you and external ones are
marked unknown. Every call still open is in `context.pending_actions` and at
the top of the briefing, as one to check before repeating.

`agent.reconcile(resolver)` settles them. The resolver gets each open
action: for an idempotent one it runs the call again with `action.key`; for
an external one it asks the outside system whether it happened. It returns
`ActionEnd("done")` or `ActionEnd("failed")`, or None when it cannot tell.
Each settled outcome is written to the agent's history, so the model sees
it, and `context` is rebuilt. Recall never runs a tool itself.

### State over HTTP

`resume` and everything on `ResumedAgent` work the same against a
`recall serve`, through `Client(url=..., api_key=..., agent=True)`. The
server holds the agent, and its lease, between requests; `agent.epoch` is the
lease epoch it issued, and the client sends it as `If-Lease` on every call so
that a client the server no longer holds the agent for is refused with
`RecallError` code `"lease_lost"` rather than writing over its successor.
After a server restart the held agents are gone: calls raise with the
message `not resumed`, and `client.resume` starts over from the records.

Other languages reach the same operations at `POST /v1/agent/<verb>` with
the tool's arguments as the body and the epoch in `If-Lease` or an `epoch`
field; see the [State API](../docs/state-api-v1.md#4-http-transport).
