# Polign Recall

Recall gives an agent memory and state that survive the conversation and the
process. Memory is what the agent knows: facts a user stated, with every
correction kept, so the agent can say what it believes now, what it believed
before, and why. State is where the agent is: its progress, its notes to its
next instance, and the tool calls it had in flight, so a run that crashes or
moves machines resumes from its own records.

Recall is a Go library, a `recall` binary that serves the same operations as
MCP tools and as HTTP, and a Python client. It stores everything in a
database you run: polign_db, PostgreSQL, MongoDB or Qdrant. Apache 2.0.

![Recall, mem0 and Zep answering questions about a user whose facts changed over nine months](docs/demo.gif)

One user over nine months, three homes, three employers, three work emails,
six questions, Claude Opus 5.5 extracting, answering and judging for all three
systems: Recall 18/18, mem0 18/18, Zep 12/18. Tokens to save one message:
Recall 3.0k, mem0 12.7k, Zep 14.7k.

## Install

```sh
pip install polign-recall          # the Python client, the recall binary, and the database it runs
brew install polign/tap/recall     # the same for Homebrew
go install github.com/Polign/recall/cmd/recall@latest   # the binary alone; it fetches the database on first use
```

Then give it to the agent you use:

```sh
claude mcp add recall -- recall mcp -write      # Claude Code
```

[Add to Cursor](https://cursor.com/en/install-mcp?name=recall&config=eyJjb21tYW5kIjoicmVjYWxsIiwiYXJncyI6WyJtY3AiLCItd3JpdGUiXX0%3D) ·
[Add to VS Code](https://vscode.dev/redirect/mcp/install?name=recall&config=%7B%22command%22%3A%22recall%22%2C%22args%22%3A%5B%22mcp%22%2C%22-write%22%5D%7D) ·
any other MCP host: command `recall`, arguments `mcp -write` ([details](docs/mcp.md)).

With nothing configured, memory lives in a local database in
`~/.local/share/recall`, started on first use and shared by every agent on the
machine. No model and no API key are needed to start. To keep memory in a
database you already run, see [Backends](#backends).

## What a correction looks like

Every change is kept. Three commands read the record:

```text
$ recall history "which editor do I use"
user preferred editor
  believed now: user preferred editor: zed
  2026-09-01 10:00  stated, user_stated
      + user preferred editor: vim
  2026-09-20 10:00  replaced, user_stated
      - user preferred editor: vim
      + user preferred editor: emacs
  2026-10-05 10:00  replaced, user_stated
      - user preferred editor: emacs
      + user preferred editor: zed

$ recall ask -as-of 2026-09-25 "which editor do I use"
Believed as of 2026-09-25 00:00:
user preferred editor: emacs  (since 2026-09-20)
  replaced: vim  (since 2026-09-01)

$ recall diff -from 2026-09-15 -to 2026-10-31
Changed between 2026-09-15 00:00 and 2026-10-31 00:00:
- user home city: Lisbon
+ user home city: Porto
- user preferred editor: vim
+ user preferred editor: zed
```

## Why Recall

- **Corrections are records, not overwrites.** A new value replaces the old
  one by a rule you can read, and every answer arrives with what it replaced.
- **Any moment is readable.** `as_of`, `history` and `diff` say what the agent
  believed then and when that changed.
- **Every answer has a source.** `recall explain` shows the message a fact came
  from, the rule that decided it, and its history.
- **Agents resume from records.** A crashed or moved agent gets its working
  state, recent turns and unfinished tool calls back, not a snapshot.
- **Your storage, your rules.** Memory lives in a database you already run, and
  the vocabulary of what gets remembered is yours to define or leave open.

## Quickstart

Python 3.10 or later. Save a preference, then correct it:

```sh
pip install polign-recall
python - <<'PY'
from polign_recall import Client

with Client(local_dir="./recall-data") as memory:
    memory.remember("user", "prefers_editor", "vim")
    memory.remember("user", "prefers_editor", "neovim")
PY
```

A second process reads the current value, the history, and what was believed
before the correction:

```sh
python - <<'PY'
from polign_recall import Client

with Client(local_dir="./recall-data") as memory:
    print(memory.recall("user", "prefers_editor")[0].value)   # neovim

    events = memory.history("user", "prefers_editor")
    print([e.value for e in events])                           # ['vim', 'neovim']

    before = memory.recall("user", "prefers_editor", as_of=events[0].observed_at)
    print(before[0].value)                                      # vim
PY
```

The first client to open `./recall-data` starts a local database for it and
later clients share it; the memories stay there after the program exits. A
bare `Client()` uses `~/.local/share/recall`, the memory every agent on the
machine shares. The [Python client guide](python/README.md) covers the rest:
plain-text memory with a model, forgetting, history, and resuming an agent.

## How memory works

A memory is a **subject**, a **predicate** and a **typed value**: `user`,
`prefers_editor`, `neovim`. Predicates live in a registry that says what type
a value has, whether the predicate holds one value or many, and which
predicates it contradicts: `dislikes` excludes `likes`, `gave_away` excludes
`owns`. A single-valued predicate replaces its value on a new statement; a
multi-valued one accumulates. Forgetting records a withdrawal; nothing is
deleted. Recall ships [starter registries](docs/reference.md#starter-registries)
for coding, personal assistant, support, sales and voice agents, and you can
[change a registry later](docs/reference.md#changing-the-registry) without
touching what was already remembered.

You do not have to define predicates. With an
[open vocabulary](docs/reference.md#open-vocabulary) the first use of a
predicate defines it, and with a model configured the MCP tools take plain
text and no predicates appear anywhere:

```sh
recall setup -extract-model anthropic:claude-haiku-4-5-20251001
```

```text
remember("I switched from helix to zed last week")
recall("which editor do I use?")  ->  "user prefers editor: zed" (before: helix)
forget("my editor")
```

The model decides how each statement is filed. Once filed, replacement follows
the predicate's rule, every filing is recorded, and a wrong filing is corrected
with one event that every later answer follows. No model reconciles memory on
its own: Recall applies the registry's rules, shows what may conflict, and
lets the agent confirm the rest, so every correction reads back to the rule or
statement that made it. `recall mcp -typed` serves the typed tools
(`remember(subject, predicate, value)`) instead. The
[developer reference](docs/reference.md) has the details and the
[Memory API](docs/memory-api-v1.md) is the contract.

## State

The second half of Recall, with its own contract,
[docs/state-api-v1.md](docs/state-api-v1.md). An agent writes down where it is
as it works, and a new process resumes from those records instead of from a
snapshot:

- **Working state**: goal, plan, progress, focus, decisions, open questions.
- **Turns**, verbatim, and large tool **outputs** stored by reference.
- **Pointers** to where the work lives: a branch, an object key, a process.
- **Actions**: each tool call that touches the world, recorded before it runs
  and closed with its result, so a crash between the two is visible and the
  next resume lists the call as unconfirmed instead of repeating it.

`Resume` takes a lease on the agent id, so one process acts for an agent at a
time, rebuilds the context within a token budget, and renders it as one
briefing for the model:

```go
agent, err := client.Resume(ctx, recall.ResumeRequest{AgentID: "coder-1", TokenBudget: 8000})
if err != nil {
    return err
}
defer agent.Release(context.Background())

briefing := agent.Resumed().Briefing // hand this to the model
```

The same operations are MCP tools (`agent_resume`, `update_working_state`,
`record_turn`, `begin_action`, ...) and, on `recall serve`, HTTP routes at
`POST /v1/agent/<verb>`. Over HTTP the server holds the lease between
requests: `resume` returns an epoch and every write carries it, so a stale
process cannot write over its successor. The State API is beta; the records it
writes are stable ([STABILITY.md](STABILITY.md)). The
[resume example](examples/resume) kills an agent mid-task and continues it in
a new process.

## Serving many agents

`recall mcp` is one agent, one process. `recall serve` is the same memory and
state for many agents over HTTP, each inside the namespace its key names. The
server holds the database credentials; agents hold Recall keys.

```sh
recall serve -backend polign -url http://polign:23000 -bootstrap-key-file admin.key
recall key create -backend polign -url http://polign:23000 -namespace acme:agent-7 -expires 168h
```

Each namespace gets its own collection, so nothing a caller sends can reach
another namespace's records. `POST /mcp` is MCP over streamable HTTP;
`POST /v1/<tool>` takes a memory tool's arguments as the body, and
`POST /v1/agent/<verb>` a state tool's. Any stdio MCP client reaches the
server through `recall connect` with `RECALL_URL` and `RECALL_API_KEY` set.

On Kubernetes a pod needs no key at all. With `recall serve -kubernetes`, it
posts its projected service account token and gets a short-lived key fixed to
its own agent id, which `recall connect` and the Python client obtain and
renew by themselves. The Helm chart is `charts/recall` in
[Polign/polign](https://github.com/Polign/polign); the
[agent-sandbox demo](deploy/agent-sandbox) runs it on kind.

## Backends

| | local (default) | polign_db | PostgreSQL + pgvector | MongoDB | Qdrant |
| --- | --- | --- | --- | --- | --- |
| `RECALL_BACKEND` | `local` | `polign` | `pgvector` | `mongodb` | `qdrant` |
| `RECALL_BACKEND_URL` | a directory | `http://host:23000` | `postgres://user:pw@host/db` | `mongodb://user:pw@host/db` | `http://host:6333` |
| Memory, history, as-of | yes | yes | yes | yes | yes |
| Keyword search | yes | yes | yes | yes | no |
| Cached reads | yes | yes | yes | yes | no |
| State API (leases) | yes | yes | yes | yes | no |

```sh
export RECALL_BACKEND=pgvector RECALL_BACKEND_URL=postgres://user:password@localhost:5432/mydb
recall mcp -write
```

Three settings name the database for every command: `-backend`, `-url` and
`-key`, or the variables above. Everything else, including what each backend
needs and how to add one, is in [docs/backends.md](docs/backends.md).

## Benchmark

LongMemEval user-facts subset, 267 questions about a user's own facts,
preferences, updates and dates, with Claude Sonnet 5.5 as reader and judge
and the upstream prompts:

| What the reader is given | Score |
| --- | --- |
| Nothing | 6.0 |
| The full chat history | 88.0 |
| BM25 over sessions, top 10 | 89.9 |
| Recall, built-in word search | 88.8 |
| Recall with a model embedder (OpenAI text-embedding-3-small, 512 dims) | 92.9 |
| Only the sessions that hold the answer | 96.6 |

With word search Recall is even with BM25 overall and behind it on preference
questions (70.0 against 83.3); a model embedder puts it ahead. Recall handed
the reader about 28k tokens per question, against 44k for BM25 and 168k for
the full history. The harness is in [`eval/longmemeval`](eval/longmemeval).

## Integrations

| | |
| --- | --- |
| **Python** | `pip install polign-recall`, then the [client guide](python/README.md). |
| **Go** | `go get github.com/Polign/recall`, then the [Go library guide](docs/reference.md#go-library). |
| **MCP** | [Connect any agent](docs/mcp.md) to remember, recall and forget. |
| **Claude Code** | The [Recall plugin](https://github.com/Polign/polign/tree/main/plugins/recall): memory across coding sessions. |
| **LangGraph** | `pip install recall-langgraph` to [resume LangGraph agents](https://github.com/Polign/polign/tree/main/python/recall-langgraph). |
| **LiveKit**, **Vapi** | `recall-livekit` and `recall-vapi` for [caller memory in voice agents](https://github.com/Polign/polign/tree/main/python). |
| **Pydantic AI** | `pip install recall-pydantic-ai` for [memory tools in Pydantic AI agents](https://github.com/Polign/polign/tree/main/python/recall-pydantic-ai). |
| **Vercel AI SDK** | The [Next.js chat example](examples/vercel-ai-sdk), one click to deploy. |
| **OpenClaw** | `@polign/openclaw-recall` on npm, the [OpenClaw plugin](https://github.com/Polign/polign/tree/main/plugins/openclaw). |

## Documentation

[Developer reference](docs/reference.md) ·
[Memory API contract](docs/memory-api-v1.md) ·
[State API contract](docs/state-api-v1.md) ·
[Backends](docs/backends.md) ·
[Connecting through MCP](docs/mcp.md) ·
[Caching memory reads](docs/materialization.md) ·
[Exporting and replaying history](docs/audit.md) ·
[What is stable and what is beta](STABILITY.md) ·
[Development guide](docs/development.md)

## Support the project

If Recall kept your agent from acting on a stale fact, a star on this
repository helps other people find it. Bugs, questions and ideas go in
[issues](https://github.com/Polign/recall/issues).

## License

[Apache 2.0](LICENSE).
