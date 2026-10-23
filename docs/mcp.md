# Connect an agent through MCP

Recall exposes memory operations through the Model Context Protocol (MCP).
Any host that supports MCP's stdio transport can launch the server and use its
tools. The [Claude Code plugin](https://github.com/Polign/polign/tree/main/plugins/recall)
is one ready-made integration.

`recall skill` prints an agent guide covering setup, tool calls, correction
rules, and history, as Markdown, offline, without a configured database.

## Connect to a shared server

When a `recall serve` holds the memory, an MCP client that speaks stdio starts
`recall connect` instead of `recall mcp`:

```json
{"mcpServers": {"recall": {"command": "recall", "args": ["connect"],
  "env": {"RECALL_URL": "http://recall.recall.svc:8080", "RECALL_API_KEY": "rcl_..."}}}}
```

A client that speaks MCP over streamable HTTP connects to `RECALL_URL/mcp`
directly with `Authorization: Bearer rcl_...`. Inside a Kubernetes pod,
`RECALL_TOKEN_FILE` in place of the key exchanges the pod's projected token
for one. See [memory-api-v1.md](memory-api-v1.md).

## Connect to your memory backend

Configure your host to launch:

```sh
recall mcp -write
```

With nothing else set, memory is kept in a local database `recall` runs itself
in `~/.local/share/recall`, the same one `recall setup` and the Claude plugin
use. To keep it somewhere else, set the environment for that process. Three
variables name the database, the same three for every backend:

| Variable | Purpose | Default |
| --- | --- | --- |
| `RECALL_BACKEND` | `local`, `polign`, `pgvector`, `mongodb` or `qdrant` | `local`, or what `recall setup` saved |
| `RECALL_BACKEND_URL` | Where it is, in the backend's own form: a directory, `http://host:23000`, `postgres://user@host/db`, `mongodb://host/db`, `http://host:6333`. Options go on the query string, such as `?schema=memory` | The backend's default |
| `RECALL_BACKEND_KEY` | The credential, when the backend takes one and the URL does not carry it | Unset |

And these configure the memory itself:

| Variable | Purpose | Default |
| --- | --- | --- |
| `RECALL_COLLECTION` | The collection holding your memories | One per embedder: `recall_lexical_v1` for word search |
| `RECALL_EMBED_MODEL` | Embedder for search by meaning, as `provider:model[@dim]` or `lexical` | Follows `RECALL_EXTRACT_MODEL`: OpenAI and Ollama embed with their own models, Anthropic with the model built into recall |
| `RECALL_EXTRACT_MODEL` | Model that works out the statements in remembered text, as `provider:model` | Unset: text is kept as written |
| `RECALL_PREDICATES` | Path to a custom registry of allowed memory types | Built-in registry |

The names used before 0.23.0 (`POLIGN_URL`, `POLIGN_API_KEY`,
`POLIGN_COLLECTION` and the rest, and each backend's own URL variable) keep
working, with one line on stderr naming the new one.

For hosts using the common `mcpServers` configuration format:

```json
{
  "mcpServers": {
    "recall": {
      "command": "/absolute/path/to/recall",
      "args": ["mcp", "-write"],
      "env": {
        "RECALL_BACKEND": "polign",
        "RECALL_BACKEND_URL": "http://127.0.0.1:23000",
        "RECALL_COLLECTION": "recall_demo"
      }
    }
  }
}
```

Replace the executable path and connection settings with yours. Use your host's
credential settings to supply `RECALL_BACKEND_KEY` when needed. Host
configuration formats vary; the process command and environment above are the
connection requirements.

Each host starts its own MCP process. Hosts share durable memory when those
processes connect to the same backend, collection, and namespace with compatible
Recall versions, registries, and embedding methods. The MCP process's lifetime
does not determine how long memories are kept.

## Memory tools

`recall mcp` serves three tools with no predicates in them. Give
`-extract-model <provider:model>` so the server works out the facts:

| Tool | Takes | What it does |
| --- | --- | --- |
| `remember` | `text` | The server works out the statements in the text, files them, and keeps the text as the record. A newer statement replaces an older one. |
| `recall` | `question` | Answers with current memories as sentences, each with what it replaced and how many days ago it was stated. |
| `forget` | `text` | Withdraws the facts a description names, such as "my editor". The texts they came from stay. |

The model is called on every `remember` and `forget`, so pick one that answers
in a second or two: a local thinking model such as `qwen3:8b` took 24 to 82
seconds per write in testing. Restating the same text replays the first
extraction instead of calling the model again. Without `-extract-model`,
`remember` keeps the text as written (searchable, but it replaces nothing)
and `forget` reports that it needs a model. `recall setup -extract-model`
saves the model, and the saved setup then serves these tools.

Writes define predicates as they go: a predicate nothing has defined is defined
by its first use, seeded from a starter registry. `-predicates FILE` fixes the
vocabulary instead, and the model files into it.

## Typed tools

`recall mcp -typed` (or `RECALL_TYPED=1`) serves the typed tools instead, over
the starter registry or `-predicates FILE`:

| Tool | What it does |
| --- | --- |
| `list_predicates` | Lists the kinds of memory the application accepts. |
| `remember` | Saves a fact or preference. A new value replaces an old one when its type allows only one current value. |
| `recall` | Reads current memories or answers a query about an earlier time. Each memory lists the value it replaced, so a correction arrives with the answer. |
| `memory_history` | Shows the statements and withdrawals behind a memory. |
| `explain` | Says why each memory a question finds is held: the text it came from, the rules that fold it, and its history. |
| `forget` | Withdraws a memory from current answers while preserving its history. |

Start without `-write` to expose only the read tools. There is no separate
correction tool: use `remember` with the same subject and predicate and the new
value. Types that accept several values need an explicit `forget` to remove an
old value.

With `-open`, the agent is not limited to the listed kinds of memory: a
predicate nothing has defined is defined by its first use, so no predicates
file is needed. `remember` then also takes `cardinality` (`single` or `multi`)
and `description` for a new one.

## Turning text into memories

The host agent can propose facts from a user's text in a `remember` call. Recall
checks those proposals against the registry and applies the memory rules. The
MCP server does not require another language model or an embedding service.

See [text extraction](reference.md#free-text-without-a-second-model) for the
request format, evidence handling, and error behavior.
