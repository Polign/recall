# Recall

**Shared, typed memory for agents, and a way to resume them without snapshots.**

Recall lets agents share facts, preferences, and project context across processes
and sessions. One agent can record a preference, another can update it, and both
can read the current value and its history.

An agent can also keep notes on its own work: its goal, plan, progress, and
where its work lives. When the process crashes or moves to another machine, the
next one [resumes from those records](#resuming-an-agent) instead of starting
over or restoring a container snapshot.

Use it through **Go**, **Python**, or **MCP**, with
[Polign](https://github.com/Polign/polign) as the storage backend. Go applications
can also supply their own backend.

## Choose an integration

| Integration | Get started |
| --- | --- |
| **Python** | Follow the quickstart below, then see the [client guide](python/README.md). |
| **Go** | Run `go get github.com/Polign/recall` and follow the [Go client guide](docs/reference.md#context-aware-client). |
| **MCP** | [Connect an agent](docs/mcp.md) to tools for remembering, recalling, and forgetting facts. |
| **LangGraph** | `pip install recall-langgraph` to [resume LangGraph agents](https://github.com/Polign/polign/tree/main/python/recall-langgraph) from their own records. |
| **LiveKit** | `pip install recall-livekit` for [caller memory in voice agents](https://github.com/Polign/polign/tree/main/python/recall-livekit). |

The [Claude Code plugin](https://github.com/Polign/polign/tree/main/plugins/recall)
is a ready-made MCP integration for memory across coding sessions.

## Quickstart

You'll need Python 3.10+. Install the client:

```sh
pip install polign-recall
```

pip also brings the `polign_db` package, which holds the `polign` and
`polign-server` binaries, so there is nothing else to download or start. This
example needs no model or embedding API key.

One process saves an editor preference and then changes it:

```sh
python - <<'PY'
from polign_recall import Client

with Client(local_dir="./recall-data") as memory:
    memory.remember("user", "prefers_editor", "vim")
    memory.remember("user", "prefers_editor", "neovim")
PY
```

Run a second process in the same terminal. It reads the saved preference and
the earlier statements:

```sh
python - <<'PY'
from polign_recall import Client

with Client(local_dir="./recall-data") as memory:
    current = memory.recall("user", "prefers_editor")
    print(current[0].value)  # neovim
    print([event.value for event in memory.history("user", "prefers_editor")])
    # In a fresh directory: ['vim', 'neovim']
PY
```

The first client to open `./recall-data` starts a local `polign-server` for it
in the background, and later clients share it. The memories stay in that
directory after your program exits. Managed local databases work on Linux and
macOS.

To share memory between machines, run `polign-server` yourself (requires
[Polign v0.8.0 or later](https://github.com/Polign/polign#install)), leave
`local_dir` out, and set `POLIGN_URL`, `POLIGN_COLLECTION`, and
`POLIGN_API_KEY` when authentication is required.

For examples covering historical reads and forgetting, see the
[Python session example](examples/python/sessions.py) or
[Go session example](examples/sessions).

## How memory works

A memory has a **subject**, a **predicate**, and a **typed value**:

| Subject | Predicate | Value |
| --- | --- | --- |
| `user` | `prefers_editor` | `neovim` |
| `recall-demo` | `prefers_test_framework` | `pytest` |
| `recall-demo` | `uses_technology` | `Go` |

The registry defines which predicates an application accepts, their value types,
and whether they hold one value or several. Recall includes 15 predicates for
preferences, identity, and project facts. You can
[extend the registry](docs/reference.md#the-registry) for your application.

For a single-valued predicate such as `prefers_editor`, a new assertion replaces
the current value. A multi-valued predicate such as `uses_technology` can hold
both Python and Go. Corrections preserve earlier statements, so you can read
current values, inspect their history, or query what was known at an earlier time.

Forgetting records a withdrawal and removes the affected fact from current
answers. **It does not permanently delete the record.**

An agent can also [propose facts from text](docs/reference.md#free-text-without-a-second-model).
Recall validates those proposals against the registry before writing them. A
statement that fits no predicate is kept as a [note](docs/reference.md#notes)
instead of being dropped. Once you add a predicate for that kind of fact,
`Promote` files each note under it and keeps the note in history.

## Resuming an agent

`Resume` gives an agent back the context it should start from, built from what
it wrote down while it worked:

- **Working state**: its goal, plan, progress, focus, and open questions.
- **Pointers** to where its work lives, such as a git branch or an object key.
- **Turns**, verbatim, and large tool **outputs** stored by reference.

The next process gets those records, plus memories that match its focus, sized
to a token budget and rendered as one briefing for the model. A lease keeps one
process per agent, so a second copy cannot write over the first. Leases need
Polign v0.8.0 or later.

```go
agent, err := client.Resume(ctx, recall.ResumeRequest{AgentID: "coder-1", TokenBudget: 8000})
if err != nil {
    return err
}
defer agent.Release(context.Background())

briefing := agent.Resumed().Briefing // hand this to the model
```

The [resume example](examples/resume) crashes an agent partway through a task
and continues it in a new process. See [Resuming an agent](docs/reference.md#resuming-an-agent)
for the full API, and the [Python client guide](python/README.md) for `resume`
in Python.

## Storage and search

Recall provides memory rules, validation, and history; the backend provides
persistence and access to the data. Agents share memory by using the same
backend, collection, and namespace, with compatible Recall versions and memory
definitions. Search also requires compatible embedding methods.

Concurrent reads and writes follow the backend's consistency guarantees. Recall
does not add transaction isolation or replication. Go applications can implement
the `Put`, `List`, and `Search` [backend contract](docs/reference.md#complete-histories)
to use another storage system.

The Go library has no external module dependencies. Its built-in lexical
embedder supports word-overlap search; applications can supply a model-based
embedder for semantic search. See the [embedding guide](docs/reference.md#embeddings).

## Repository layout

| Path | Contents |
| --- | --- |
| [`recall.go`](recall.go), [`doc.go`](doc.go) | Public Go API at `github.com/Polign/recall` |
| [`internal/engine/`](internal/engine) | Memory implementation and unit tests |
| [`polign/`](polign) | Go backend adapter |
| [`python/`](python) | Python package and tests |
| [`cmd/`](cmd) | Command-line tools, including `recall-audit` |
| [`examples/`](examples) | Runnable Go and Python examples, including a crash-and-resume demo |
| [`docs/`](docs) | Guides and API reference |
| [`testdata/`](testdata) | Shared audit fixtures |

See the [development guide](docs/development.md) for the implementation map and
test commands.

## Documentation

[Developer reference](docs/reference.md) ·
[Resuming an agent](docs/reference.md#resuming-an-agent) ·
[Connecting through MCP](docs/mcp.md) ·
[Caching memory reads](docs/materialization.md) ·
[Exporting and replaying history](docs/audit.md)

## License

[Apache 2.0](LICENSE).
