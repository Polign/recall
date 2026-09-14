# Recall

**Shared, typed memory for agents.**

Recall lets agents share facts, preferences, and project context across processes
and sessions. One agent can record a preference, another can update it, and both
can read the current value and its history.

Use it through **Go**, **Python**, or **MCP**, with
[Polign](https://github.com/Polign/polign) as the storage backend. Go applications
can also supply their own backend.

## Choose an integration

| Integration | Get started |
| --- | --- |
| **Python** | Follow the quickstart below, then see the [client guide](python/README.md). |
| **Go** | Run `go get github.com/Polign/recall` and follow the [Go client guide](docs/reference.md#context-aware-client). |
| **MCP** | [Connect an agent](docs/mcp.md) to tools for remembering, recalling, and forgetting facts. |

The [Claude Code plugin](https://github.com/Polign/polign/tree/main/plugins/recall)
is a ready-made MCP integration for memory across coding sessions.

## Quickstart

You'll need Python 3.10+ and [Polign v0.6.4 or later](https://github.com/Polign/polign#install),
with both `polign` and `polign-server` on your `PATH`. The Python client launches
a local `polign mcp` process that connects to your database. This example needs
no model or embedding API key.

Start the database in a terminal and leave it running:

```sh
polign-server -store "fs:$HOME/.local/share/recall/data"
```

In another terminal, from a checkout of this repository, install the Python
client and select a collection for the example:

```sh
python3 -m venv .venv
source .venv/bin/activate
python -m pip install ./python
export POLIGN_URL=http://127.0.0.1:23000
export POLIGN_COLLECTION=recall_demo
```

One process saves an editor preference and then changes it:

```sh
python - <<'PY'
from polign_recall import Client

with Client() as memory:
    memory.remember("user", "prefers_editor", "vim")
    memory.remember("user", "prefers_editor", "neovim")
PY
```

Run a second process in the same terminal. It reads the saved preference and
the earlier statements:

```sh
python - <<'PY'
from polign_recall import Client

with Client() as memory:
    current = memory.recall("user", "prefers_editor")
    print(current[0].value)  # neovim
    print([event.value for event in memory.history("user", "prefers_editor")])
    # On a fresh collection: ['vim', 'neovim']
PY
```

Each process closes its client when it exits; the memories remain in the
database. Agents on other machines can use the same server URL and collection,
with `POLIGN_API_KEY` set when authentication is required.

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
Recall validates those proposals against the registry before writing them.

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
| [`examples/`](examples) | Runnable Go and Python examples |
| [`docs/`](docs) | Guides and API reference |
| [`testdata/`](testdata) | Shared audit fixtures |

See the [development guide](docs/development.md) for the implementation map and
test commands.

## Documentation

[Developer reference](docs/reference.md) ·
[Caching memory reads](docs/materialization.md) ·
[Exporting and replaying history](docs/audit.md)

## License

[Apache 2.0](LICENSE).
