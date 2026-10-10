# Connect an agent through MCP

Recall exposes memory operations through the Model Context Protocol (MCP).
Any host that supports MCP's stdio transport can launch the server and use its
tools. The [Claude Code plugin](https://github.com/Polign/polign/tree/main/plugins/recall)
is one ready-made integration.

The development version of the Polign CLI also provides `polign recall skill`:
an embedded agent guide covering setup, tool calls, correction rules, and history.
It prints Markdown offline, without requiring a configured database. This command
is pending a Polign release; it is not part of Polign 0.6.6.

## Connect to your memory backend

Install [Polign v0.6.4 or later](https://github.com/Polign/polign#install) and
start or connect to a Polign database. Configure your host to launch:

```sh
polign mcp -memory-only -write
```

Set the connection environment for that process:

| Variable | Purpose | Default |
| --- | --- | --- |
| `POLIGN_URL` | Your Polign server's address | `http://localhost:23000` |
| `POLIGN_COLLECTION` | The collection holding your memories | `recall_lexical_v1` |
| `POLIGN_API_KEY` | Credential for a server that requires authentication | Unset |
| `POLIGN_PREDICATES` | Path to a custom registry of allowed memory types | Built-in registry |

For hosts using the common `mcpServers` configuration format:

```json
{
  "mcpServers": {
    "recall": {
      "command": "/absolute/path/to/polign",
      "args": ["mcp", "-memory-only", "-write"],
      "env": {
        "POLIGN_URL": "http://127.0.0.1:23000",
        "POLIGN_COLLECTION": "recall_demo"
      }
    }
  }
}
```

Replace the executable path and connection settings with yours. Use your host's
credential settings to supply an API key when needed. Host configuration formats
vary; the process command and environment above are the connection requirements.

Each host starts its own MCP process. Hosts share durable memory when those
processes connect to the same backend, collection, and namespace with compatible
Recall versions, registries, and embedding methods. The MCP process's lifetime
does not determine how long memories are kept.

## Memory tools

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

## Text-only tools

`recall mcp -text -extract-model <provider:model>` serves three tools with no
predicates in them:

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

## Turning text into memories

The host agent can propose facts from a user's text in a `remember` call. Recall
checks those proposals against the registry and applies the memory rules. The
MCP server does not require another language model or an embedding service.

See [text extraction](reference.md#free-text-without-a-second-model) for the
request format, evidence handling, and error behavior.
