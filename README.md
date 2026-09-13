# Recall

**Typed memory for distributed agents.**

Recall lets agents in different processes or services share facts, preferences,
and project context. A memory has a defined type, a current value, and a history.
When something changes, agents can read the updated value and see what came
before it.

Use Recall in your own application through **Go**, **Python**, or **MCP**.
The Claude Code plugin is one example of an agent using it.

## What a memory looks like

A memory describes **who or what**, **which fact**, and **its value**:

| Subject | Fact | Value |
| --- | --- | --- |
| `user` | `prefers_response_style` | `concise` |
| `recall-demo` | `prefers_test_framework` | `pytest` |
| `recall-demo` | `deployment_target` | `kubernetes` |

Suppose one agent records that `recall-demo` prefers pytest. Later, another
records a change to unittest. Agents reading that project's testing preference
get unittest; the earlier pytest statement remains in its history.

Recall comes with 15 memory types for preferences, identity, and project facts.
Each type defines whether it holds one value or several. An editor preference
has one current value; a project's technologies can include both Python and Go.
You can [add types for your application](docs/reference.md#the-registry).

## Where Recall fits

Each agent uses Recall to read and write a shared storage backend. Recall
provides the memory types, validation, correction rules, and history. The backend
provides persistence and access to the data.

[Polign](https://github.com/Polign/polign) is the included backend adapter. You can
also connect your own backend through Recall's `Put`, `List`, and `Search`
interface. Agents share memory by using the same backend, collection, and
namespace, with compatible Recall versions and the same memory definitions.
A collection is a named group of memories; a namespace controls which data a
credential can access.

This supports agents spread across machines without storing memory in each
agent's conversation. Concurrent reads and writes follow the backend's
consistency guarantees. Recall does not add transaction isolation or replication.
See the [backend contract](docs/reference.md#complete-histories) for details.

## Try shared memory with Python

You'll need Python 3.10+ and [Polign v0.6.4 or later](https://github.com/Polign/polign#install).
This example uses a local database; no model or embedding API key is needed.

Start the database in a terminal and leave it running:

```sh
polign-server -store "fs:$HOME/.local/share/recall/data"
```

In another terminal, from a checkout of this repository:

```sh
python3 -m pip install ./python
export POLIGN_URL=http://127.0.0.1:23000
export POLIGN_COLLECTION=recall_demo
```

One agent saves a preference and then updates it:

```python
from polign_recall import Client

with Client() as memory:
    memory.remember("user", "prefers_editor", "vim")
    memory.remember("user", "prefers_editor", "neovim")
```

Another client, using the same connection settings, can read it:

```python
from polign_recall import Client

with Client() as memory:
    current = memory.recall("user", "prefers_editor")
    print(current[0].value)  # neovim
    print(memory.history("user", "prefers_editor"))  # includes both statements
```

Run the second example in a new process to read the same saved memory. For
agents on other machines, use the shared server's URL and the appropriate API
key. Your memories remain in the database after a client exits.

[Python client guide](python/README.md) · [Session example](examples/python/sessions.py)

## Choose an integration

| Integration | How it connects |
| --- | --- |
| [Go library](docs/reference.md#context-aware-client) | Embed Recall directly in your application. Use Polign or supply your own backend. |
| [Python client](python/README.md) | Call Recall from Python through a local `polign mcp` process connected to your database. |
| [MCP](docs/mcp.md) | Give an MCP-compatible agent tools to remember, recall, correct, and forget facts. |
| [Claude Code plugin](https://github.com/Polign/polign/tree/main/plugins/recall) | An example MCP integration for memory across coding sessions. |

To add the Go library:

```sh
go get github.com/Polign/recall
```

The Go library has no external module dependencies. The default memory types
and built-in word-overlap search let you start without a schema file or an
embedding service. Your application can supply a model-based embedder for
semantic search.

## Corrections and forgetting

Corrections preserve the previous statement. You can ask for the current value,
the history of a fact, or what was known at an earlier time.

Forgetting removes a fact from current answers and records that withdrawal in
its history. **It does not permanently delete the record.**

[Developer reference](docs/reference.md) ·
[Caching memory reads](docs/materialization.md) ·
[Exporting and replaying history](docs/audit.md)

## License

[Apache 2.0](LICENSE).
