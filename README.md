# Polign Recall

Memory for agents whose facts change. When a user corrects something, Recall
replaces the old fact, tells the agent what it replaced, and keeps the old
value in a history you can read back. Nothing is deleted, and no model decides
on its own what to forget.

![Recall, mem0 and Zep answering questions about a user whose facts changed over nine months](docs/demo.gif)

The demo is a real run: one user over nine months, with three homes, three
employers, and three work emails, then six questions about her now. Opus 5.5 extracted, answered, and judged for
all three systems, three runs each. Right answers: Recall 18/18, mem0 18/18,
Zep 12/18. Tokens to save one message: Recall 3.0k, mem0 12.7k, Zep 14.7k.

## Install

```sh
pip install polign-recall          # Python client; brings the database with it
go get github.com/Polign/recall    # Go
recall mcp -write                  # MCP over stdio: remember(text), recall(question), forget(text)
```

Nothing else to configure: with no database named, `recall` keeps memory in a
local polign_db it runs itself, in `~/.local/share/recall`, shared by every
agent on the machine. The [Quickstart](#quickstart) runs without a model or an
API key. To keep memory somewhere else, see [Backends](#backends).

## What a correction looks like

Every change is kept. Three commands read that record:

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

- **Corrections are records.** A new value replaces the old one by a rule you
  can read, and the answer arrives with what it replaced.
- **Ask about any moment.** `as_of`, `history`, and `diff` answer what the
  agent believed then, and when that changed.
- **Every answer has a source.** `recall explain` shows the message a fact came
  from, the rule that decided it, and its history.
- **Shared and resumable.** Agents in different processes read and correct the
  same memory, and a crashed agent [resumes from its records](#resuming-an-agent).
- **Your storage.** polign_db, PostgreSQL with pgvector, MongoDB, or Qdrant.
  Apache 2.0.

## Benchmark

LongMemEval user-facts subset (267 questions about a user's own facts,
preferences, updates, and dates), Recall 0.19.0, Sonnet 5.5 as reader and
judge, upstream prompts:

| What the reader is given | Score |
| --- | --- |
| Nothing | 6.0 |
| The full chat history | 88.0 |
| BM25 over sessions, top 10 | 89.9 |
| Recall, built-in word search | 88.8 |
| Recall with a model embedder (OpenAI text-embedding-3-small, 512 dims) | 92.9 |
| Only the sessions that hold the answer | 96.6 |

With its built-in word search, Recall is about even with BM25 overall and
behind it on preference questions (70.0 against 83.3). A model embedder puts it
ahead, and it is the default since 0.20.0 when the extractor is an OpenAI
model. With word search, Recall handed the reader about 28k tokens per
question, against about 44k for BM25 and 168k for the full history.

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

Run a second process in the same terminal. It reads the current preference,
the full history, and what was believed before the correction:

```sh
python - <<'PY'
from polign_recall import Client

with Client(local_dir="./recall-data") as memory:
    print(memory.recall("user", "prefers_editor")[0].value)  # neovim

    events = memory.history("user", "prefers_editor")
    print([event.value for event in events])  # ['vim', 'neovim'] in a fresh directory

    # Ask what was believed at the moment of the first statement.
    before = memory.recall("user", "prefers_editor", as_of=events[0].observed_at)
    print(before[0].value)  # vim
PY
```

The correction did not erase `vim`. It is still there to read, which is how you
answer "what did the agent believe yesterday, and when did that change?"

The first client to open `./recall-data` starts a local `polign-server` for it
in the background, and later clients share it. The memories stay in that
directory after your program exits. A bare `Client()` does the same in
`~/.local/share/recall`, the memory every agent on the machine shares, the
Claude plugin included. Managed local databases work on Linux and macOS.

To share memory between machines, run `polign-server` yourself (requires
[Polign v0.8.0 or later](https://github.com/Polign/polign#install)), leave
`local_dir` out, and set `RECALL_BACKEND=polign` and `RECALL_BACKEND_URL` to
its address, with `RECALL_BACKEND_KEY` when authentication is required, and
`RECALL_COLLECTION` for the collection.

For examples covering forgetting as well, see the
[Python session example](examples/python/sessions.py) or
[Go session example](examples/sessions).

## How memory works

A memory has a **subject**, a **predicate**, and a **typed value**:

| Subject | Predicate | Value |
| --- | --- | --- |
| `user` | `prefers_editor` | `neovim` |
| `recall-demo` | `prefers_test_framework` | `pytest` |
| `recall-demo` | `uses_technology` | `Go` |

Predicates live in a registry that declares each one's value type, whether it
holds one value or several, and which predicates it contradicts (`dislikes`
excludes `likes`, `gave_away` excludes `owns`). Recall includes
[starter registries](docs/reference.md#starter-registries) for coding, personal
assistant, support, sales, and voice agents. Add your own predicates
([the registry](docs/reference.md#the-registry)), and the extractor files
statements under them; you can
[change the registry later](docs/reference.md#changing-the-registry) without
changing what the agent remembered before. A value can be text, a number, a boolean, an enum,
a date, or a [reference to another subject](docs/reference.md#references-between-subjects).

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

### Without a registry

You don't have to define predicates at all. With an
[open vocabulary](docs/reference.md#open-vocabulary), a write may name any
predicate and its first use defines it, starting from the coding and personal
starters. `recall explain "<question>"` shows, for each answer, the text it
came from, the predicate it was filed under, the rules that decide it, and its
history.

With a model configured, the MCP server goes one step further and takes plain
text, with no predicates in any tool or answer:

```sh
recall setup -extract-model anthropic:claude-haiku-4-5-20251001
```

```
remember("I switched from helix to zed last week")
recall("which editor do I use?")  ->  "user prefers editor: zed" (before: helix)
forget("my editor")
```

The model decides how each statement is filed: which predicate, and whether a
new value replaces the old one or adds to it. Once filed, replacement follows
the predicate's rule as before, every filing is recorded, and a wrong one is
corrected with one event (`Redefine`, `Merge`, `Split`) that every answer
follows. The model is called on every `remember` and `forget`, and the text is
sent to its provider. See [text-only tools](docs/mcp.md#text-only-tools).

The [benchmark](#benchmark) above uses these text tools with an open
vocabulary seeded from the personal starter.

### Typed tools

`recall mcp -write -typed` serves tools over a fixed vocabulary instead, such
as `remember(subject, predicate, value)`. A session with the personal starter
registry:

```text
remember(user, likes, mangoes)
remember(user, dislikes, mangoes)
  -> superseded: [likes: mangoes]          # the registry says dislikes excludes likes

recall(user, dislikes)
  -> mangoes   replaced: [likes: mangoes]  # the correction arrives with the answer

remember(user, goal, "run a marathon")
remember(user, did,  "run a marathon")
recall(user, goal)
  -> run a marathon   elsewhere: [did: run a marathon]   # no rule joins goal and did,
                                                         # so the read shows both

remember(user, did, "run a marathon",
         replaces=[goal: "run a marathon", goal: "learn piano"])
  -> replaced:   [goal: run a marathon]
  -> unreplaced: [goal: learn piano]       # never believed, so reported, not dropped
```

No model reconciles memory on its own. Recall applies the rules the registry
declares, shows the candidates that may conflict, and lets your agent confirm
the rest, so every correction can be read back to the rule or the statement
that made it.

### Reading the record

Each step of a history is the log folded just before and just after that
statement, so it agrees with a read at that instant. Agents get the same
record through the `history` and `diff` MCP tools (`timeline` and `diff` with
`-typed`), `recall serve` answers them over HTTP at `/v1/history`,
`/v1/timeline` and `/v1/diff`, and the Python client has `history_text`,
`timeline` and `diff`.

## Resuming an agent

`Resume` gives an agent back the context it should start from, built from what
it wrote down while it worked. The new process resumes from records, not from a
process or container snapshot:

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

Resume and actions are a beta surface: the calls around the records may still
change in a minor release, with the change and what to do about it in the
changelog. The records themselves are stable. See [STABILITY.md](STABILITY.md).

The [resume example](examples/resume) crashes an agent partway through a task
and continues it in a new process. See [Resuming an agent](docs/reference.md#resuming-an-agent)
for the full API, and the [Python client guide](python/README.md) for `resume`
in Python.

## Serving many agents

`recall mcp` is one agent, one process, one store. `recall serve` is the same
memory for many agents at once, over HTTP, each inside the namespace its key
names. The server holds the store's credentials; agents hold Recall keys.

```sh
recall serve -backend polign -url http://polign:23000 -bootstrap-key-file admin.key   # admin.key holds one rcl_... line
recall key create -backend polign -url http://polign:23000 -namespace acme:agent-7 -expires 168h
```

With no database named, `recall serve` runs on the local database like every
other command, and `recall key create -namespace team:agent-1` against the
same default mints a key for it: a shared memory server for several agents on
one machine, with nothing configured. Its first log line says where memory
is, so a container that meant to name a database and did not is told at once.

Each namespace gets its own collection, so nothing a caller sends can reach
another namespace's records. Keys are stored hashed, expire if asked, renew at
`POST /v1/keys/renew`, and are revoked by a tombstone that nothing can
overwrite. `POST /mcp` is MCP over streamable HTTP; `POST /v1/remember`,
`/v1/recall`, `/v1/forget` and `/v1/history` take a tool's arguments as the
body. Any MCP client that speaks stdio reaches the server through
`recall connect`, with `RECALL_URL` and `RECALL_API_KEY` set.

On Kubernetes a pod needs no key at all. With `recall serve -kubernetes`, it
posts its projected service account token (audience `recall`) and gets a
short-lived key for `<tenant>:<pod name>`, fixed to that agent, which `recall
connect` and the Python client obtain and renew by themselves from
`RECALL_TOKEN_FILE`. The Helm chart is `charts/recall` in
[Polign/polign](https://github.com/Polign/polign); the
[agent-sandbox demo](deploy/agent-sandbox) runs it on kind. The contract every
client is written against is [docs/memory-api-v1.md](docs/memory-api-v1.md).

## Storage and search

Three things say where memory lives, the same three for every backend and
every command:

| Flag | Variable | What it is |
| --- | --- | --- |
| `-backend` | `RECALL_BACKEND` | `local` (the default), `polign`, `pgvector`, `mongodb` or `qdrant` |
| `-url` | `RECALL_BACKEND_URL` | Where the database is, in the backend's own form: a directory, `http://host:23000`, `postgres://user@host/db`, `mongodb://host/db`, `http://host:6333` |
| `-key` | `RECALL_BACKEND_KEY` | The credential, when the backend takes one and the URL does not carry it: the API key of polign and qdrant, the password of pgvector and mongodb |

Each resolves in the same order: the flag, the variable, the setup `recall
setup` saved, the default. The flags go before the command or after it. A
backend's other options are query keys on the URL, listed in the table below;
anything else in the query goes to the driver as it is
(`postgres://host/db?schema=memory&sslmode=require`).

Nothing else about the database has a variable. The names each backend had
before 0.23.0 (`POLIGN_URL`, `POLIGN_API_KEY`, `DATABASE_URL`, `PGPASSWORD`,
`MONGODB_URI`, `MONGODB_PASSWORD`, `QDRANT_URL`, `QDRANT_API_KEY`) keep
working as aliases, with one line on stderr naming the new one, and so does a
polign_db already answering at `http://localhost:23000` when nothing names a
backend, which is where 0.22 looked. See [STABILITY.md](STABILITY.md) for
when they go away.

Recall provides memory rules, validation, and history; the backend provides
persistence and access to the data. Agents share memory by using the same
backend, collection, and namespace, with compatible Recall versions and memory
definitions. Search also requires compatible embedding methods.

Concurrent reads and writes follow the backend's consistency guarantees. Recall
does not add transaction isolation or replication.

### Backends

| | local (default) | polign_db | PostgreSQL + pgvector | MongoDB | Qdrant |
| --- | --- | --- | --- | --- | --- |
| Package | [`backend/local/`](backend/local) | [`polign/`](polign) | [`backend/pgvector/`](backend/pgvector) | [`backend/mongodb/`](backend/mongodb) | [`backend/qdrant/`](backend/qdrant) |
| `RECALL_BACKEND` | `local` | `polign` | `pgvector` | `mongodb` | `qdrant` |
| `RECALL_BACKEND_URL` | a directory (default `~/.local/share/recall`) | `http://host:23000` | `postgres://user:pw@host:5432/db` | `mongodb://user:pw@host:27017/db` | `http://host:6333` |
| `RECALL_BACKEND_KEY` | not used; a key is generated in the directory | the polign_db API key | the password, when the URL has none | the password, when the URL has none | the Qdrant API key |
| Options on the URL | `?server=/path/to/polign-server` | none | `?schema=recall&text_search=english` | `?database=recall` | `?distance=Cosine` |
| Server version | the `polign-server` beside `recall`, which pip and Homebrew install; started on a loopback port and left running | 0.8.0 or later | PostgreSQL 13 or later with pgvector 0.6.0 or later; 0.8.0 or later recommended | 6.0 or later, Atlas or self-hosted | 1.10 or later |
| Remember, recall, forget, history, as-of | yes | yes | yes | yes | yes |
| Evidence links, audit export | yes | yes | yes | yes | yes |
| Keyword search | as polign_db | yes, BM25 | yes, PostgreSQL full-text search | yes, MongoDB text search | no, vector search only |
| Cached reads | yes | yes | yes | yes | no, every read refolds the history |
| Agent leases (`recall mcp -agent`) | yes | yes | yes | yes | no; Go `Resume` with `Unleased: true` only |
| Agent writes fenced on the server | yes | yes | yes, in the write's transaction | yes on a replica set or Atlas, in the write's transaction; checked just before the write on a standalone server | no |
| `recall setup` and the Claude plugin | yes | yes | yes | yes | yes |

To run Recall on a PostgreSQL database:

```sh
export RECALL_BACKEND=pgvector RECALL_BACKEND_URL=postgres://user:password@localhost:5432/mydb
recall mcp -write
```

The first write creates a `recall` schema with one table per collection and
two bookkeeping tables. The role needs `CREATE` on the database, and the
`vector` extension must be installed or creatable by that role.

Pitfalls on PostgreSQL:

- A collection takes its vector size from the first write. Changing embedders
  needs a new collection.
- Every write to a collection takes a row lock on that collection's revision,
  so writes to one collection are serialized. That is fine at conversation
  pace but slows a bulk import.
- Keyword search uses the `english` text search configuration unless
  `?text_search=` on the URL (Go: `Config.TextSearchConfig`) says otherwise.
  It is fixed when a collection is created.
- On pgvector older than 0.8.0, a filtered vector search can return fewer
  hits than asked for, because the index stops scanning at `ef_search`
  candidates before the filter applies.

To run Recall on MongoDB:

```sh
export RECALL_BACKEND=mongodb RECALL_BACKEND_URL=mongodb://user:password@localhost:27017/recall
recall mcp -write
```

The database is the connection string's path, `recall` when it has none. The
first write to a collection creates a MongoDB collection named `rc_` and the
collection's name, and two bookkeeping collections, `recall_collections` and
`recall_leases`. The user needs `readWrite` on the database.

Pitfalls on MongoDB:

- Vector search is exact: the similarity is computed in the server over the
  events a filter keeps. No Atlas Search index is needed and the answer is
  the true nearest events, but each search reads the whole filtered
  collection. Agent memory collections hold thousands of events, which is
  fast; a collection of millions is not what this backend is for.
- Run a replica set (Atlas always is) for agents. On a standalone server
  there are no transactions, so a fenced agent write is checked just before
  it lands rather than with it, and listings are not read from one snapshot.
  A single-node replica set is enough.
- A collection takes its vector size from the first write. Changing embedders
  needs a new collection.
- Keyword search uses MongoDB's `english` text index, fixed when a collection
  is created.

Pitfalls on Qdrant:

- Use a collection only Recall writes to. A point without the `_recall_id`
  payload field makes reads fail.
- A collection takes its vector size from the first write. Changing embedders
  needs a new collection.
- `Unleased: true` is safe only when something else keeps one process per
  agent. Without a lease, a crashed process that comes back can overwrite its
  replacement.
- With the built-in lexical embedder, exact-word queries rank worse than on
  polign_db. A model embedder (`-embed-model`) helps paraphrases, not keywords.

To add a backend, implement `Put`, `List` and `Search`
([contract](docs/reference.md#complete-histories)), register it with
`recall.RegisterBackend` in `init`, pass [`backendtest`](backendtest), and import
it in [`cmd/recall/backends.go`](cmd/recall/backends.go).

The Go library has no external module dependencies. The pgvector backend uses
[pgx](https://github.com/jackc/pgx) and the MongoDB backend the
[official MongoDB driver](https://github.com/mongodb/mongo-go-driver), which a
program pulls in only by importing that package. Its built-in lexical
embedder supports word-overlap search; applications can supply a model-based
embedder for semantic search. See the [embedding guide](docs/reference.md#embeddings).

## Repository layout

| Path | Contents |
| --- | --- |
| [`recall.go`](recall.go), [`doc.go`](doc.go) | Public Go API at `github.com/Polign/recall` |
| [`internal/engine/`](internal/engine) | Memory implementation and unit tests |
| [`backend/local/`](backend/local) | The default backend: a polign_db recall runs itself |
| [`polign/`](polign) | polign_db backend |
| [`backend/pgvector/`](backend/pgvector) | PostgreSQL + pgvector backend |
| [`backend/mongodb/`](backend/mongodb) | MongoDB backend |
| [`backend/qdrant/`](backend/qdrant) | Qdrant backend |
| [`backendtest/`](backendtest) | Conformance suite for backends |
| [`python/`](python) | Python package and tests |
| [`cmd/`](cmd) | Command-line tools, including `recall-audit` |
| [`examples/`](examples) | Runnable Go and Python examples, including a crash-and-resume demo |
| [`docs/`](docs) | Guides and API reference |
| [`testdata/`](testdata) | Shared audit fixtures |

See the [development guide](docs/development.md) for the implementation map and
test commands.

## Documentation

[What is stable and what is beta](STABILITY.md) ·
[Developer reference](docs/reference.md) ·
[Resuming an agent](docs/reference.md#resuming-an-agent) ·
[Connecting through MCP](docs/mcp.md) ·
[Caching memory reads](docs/materialization.md) ·
[Exporting and replaying history](docs/audit.md)

## Support the project

If Recall kept your agent from acting on a stale fact, a star on this
repository helps other people find it. Bugs, questions, and ideas go in
[issues](https://github.com/Polign/recall/issues).

## License

[Apache 2.0](LICENSE).
