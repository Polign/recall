# polign-recall

A Python client for Recall's typed, correctable agent memory. Requires Python
3.10+ and `polign` CLI v0.6.4+ on PATH. No Python runtime dependencies.

```sh
pip install ./python
polign-server -store fs:./recall-data
```

In another terminal:

```python
from polign_recall import Client

with Client() as memory:
    memory.remember("user", "prefers_editor", "vim")
    changed = memory.remember("user", "prefers_editor", "neovim")
    print(memory.recall("user", "prefers_editor")[0].value)  # neovim
    print(memory.history("user", "prefers_editor"))  # both statements
    memory.forget("user", "prefers_editor", "neovim")
```

The client owns a long-lived `polign mcp -memory-only -write` subprocess and
shares the Go implementation's validation and fold. Set `POLIGN_URL`,
`POLIGN_API_KEY`, and `POLIGN_COLLECTION` in the environment, or pass
`env={...}` to Client. Use `write=False` for a read-only connection.

`remember(text=..., statements=[...])` accepts proposals from your agent's model;
the client does not run a second model. Every statement must have subject,
predicate, typed value, and evidence quoting the text exactly. Read `predicates()`
to build your extractor prompt. No separate extraction tool is required.

`recall(..., as_of=...)` accepts an RFC3339 string or timezone-aware datetime.
`forget` requires a typed value or explicit `all=True`; false and zero remain
values. Forgetting retracts a belief and retains its historical events.

Errors raise `RecallError` with a code and, for a failed extraction batch, any
reported partial results. Calls are serialized, timeout after 120 seconds by
default, and never automatically retry writes. A timeout closes the transport;
inspect history using a new client before retrying an uncertain write.
