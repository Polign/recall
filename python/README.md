# polign-recall

A Python client for Recall's typed, correctable agent memory. Requires Python
3.10+.

```sh
pip install polign-recall
```

That is the whole install. pip also brings the
[`polign_db`](https://pypi.org/project/polign-db/) package, which holds the
`polign` CLI and `polign-server` binaries for Linux, macOS and Windows, so
there is nothing else to download.

```python
from polign_recall import Client

with Client(local_dir="./recall-data") as memory:
    memory.remember("user", "prefers_editor", "vim")
    changed = memory.remember("user", "prefers_editor", "neovim")
    current = memory.recall("user", "prefers_editor")[0]
    print(current.value, current.replaced[0].value)  # neovim vim
    print(memory.history("user", "prefers_editor"))  # both statements
    memory.forget("user", "prefers_editor", "neovim")
```

`local_dir` keeps the database on this machine. The first client to open the
directory starts a `polign-server` for it in the background, listening on
localhost only and protected by a key stored in the directory. Later clients,
including ones in other processes, share that server. It keeps running after
your program exits; its process id is in `runtime.json` and its log in
`server.log`, both inside the directory. Managed local databases work on Linux
and macOS.

To use a server you run yourself, leave `local_dir` out and set `POLIGN_URL`,
`POLIGN_API_KEY`, and `POLIGN_COLLECTION` in the environment, or pass
`env={...}` to Client. With neither, the client connects to
`http://localhost:23000`, where `polign-server -store fs:./recall-data` listens
by default.

The client owns a long-lived `polign mcp -memory-only -write` subprocess and
shares the Go implementation's validation and fold. Use `write=False` for a
read-only connection. It runs the `polign` binary pip installed; on a platform
without a `polign_db` wheel it runs `polign` from `PATH` (CLI v0.8.0+), and
`command=[...]` overrides both.

`remember(text=..., statements=[...])` accepts proposals from your agent's model.
Every statement must have subject, predicate, typed value, and evidence quoting
the text exactly. Read `predicates()` to build your extractor prompt.

To have the server work out the statements instead, name a model and pass the
text alone:

```python
with Client(extract_model="anthropic:claude-opus-5-5") as memory:
    memory.remember(text="I moved to Lisbon and I edit in Zed now.")
```

The model is `provider:model`: `anthropic:<model>`, `openai:<model>`, or
`ollama:<model>`, with keys from `ANTHROPIC_API_KEY` or `OPENAI_API_KEY`. It may
name only registered predicates and must quote the text; a proposal that does
not is dropped, and the whole text is kept as a note either way. Statements you
pass are used as given, and the model is not called.

`recall(..., as_of=...)` accepts an RFC3339 string or timezone-aware datetime.
`forget` requires a typed value or explicit `all=True`; false and zero remain
values. Forgetting retracts a belief and retains its historical events.

Errors raise `RecallError` with a code and, for a failed extraction batch, any
reported partial results. Calls are serialized, timeout after 120 seconds by
default, and never automatically retry writes. A timeout closes the transport;
inspect history using a new client before retrying an uncertain write.

## Resuming an agent

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
`working_state_history` to read back. Needs a `polign` CLI 0.8.0 or later,
the first with `polign mcp -agent`.
