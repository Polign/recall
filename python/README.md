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
    print(memory.recall("user", "prefers_editor")[0].value)  # neovim
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
without a `polign_db` wheel it runs `polign` from `PATH` (CLI v0.7.0+), and
`command=[...]` overrides both.

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
