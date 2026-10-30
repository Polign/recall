# Backends

Recall keeps memory in a database you already run. Three settings say which
one, the same three for every backend and every command:

| Flag | Variable | What it is |
| --- | --- | --- |
| `-backend` | `RECALL_BACKEND` | `local` (the default), `polign`, `pgvector`, `mongodb` or `qdrant` |
| `-url` | `RECALL_BACKEND_URL` | Where the database is, in the backend's own form: a directory, `http://host:23000`, `postgres://user@host/db`, `mongodb://host/db`, `http://host:6333` |
| `-key` | `RECALL_BACKEND_KEY` | The credential, when the backend takes one and the URL does not carry it: the API key of polign and qdrant, the password of pgvector and mongodb |

Each resolves in this order: the flag, the variable, what `recall setup`
saved, the default. The flags go before the command or after it. A backend's
own options are query keys on the URL, listed below; anything else in the
query goes to the driver as it is
(`postgres://host/db?schema=memory&sslmode=require`). `RECALL_COLLECTION`
names the collection.

Recall provides the memory rules, validation and history; the backend
provides persistence. Agents share memory by using the same backend,
collection and namespace. Concurrent reads and writes follow the backend's
own consistency guarantees; Recall adds no transaction isolation or
replication of its own.

## What each backend provides

| | local (default) | polign_db | PostgreSQL + pgvector | MongoDB | Qdrant |
| --- | --- | --- | --- | --- | --- |
| Package | [`backend/local/`](../backend/local) | [`polign/`](../polign) | [`backend/pgvector/`](../backend/pgvector) | [`backend/mongodb/`](../backend/mongodb) | [`backend/qdrant/`](../backend/qdrant) |
| `RECALL_BACKEND` | `local` | `polign` | `pgvector` | `mongodb` | `qdrant` |
| `RECALL_BACKEND_URL` | a directory (default `~/.local/share/recall`) | `http://host:23000` | `postgres://user:pw@host:5432/db` | `mongodb://user:pw@host:27017/db` | `http://host:6333` |
| `RECALL_BACKEND_KEY` | not used; a key is generated in the directory | the polign_db API key | the password, when the URL has none | the password, when the URL has none | the Qdrant API key |
| Options on the URL | `?server=/path/to/polign-server` | none | `?schema=recall&text_search=english` | `?database=recall` | `?distance=Cosine` |
| Server version | the `polign-server` beside `recall` or on `PATH`, else polign_db v0.15.0 downloaded once into `~/.local/share/recall/bin`; started on a loopback port and left running | 0.8.0 or later | PostgreSQL 13 or later with pgvector 0.6.0 or later; 0.8.0 or later recommended | 6.0 or later, Atlas or self-hosted | 1.10 or later |
| Remember, recall, forget, history, as-of | yes | yes | yes | yes | yes |
| Evidence links, audit export | yes | yes | yes | yes | yes |
| Keyword search | as polign_db | yes, BM25 | yes, PostgreSQL full-text search | yes, MongoDB text search | no, vector search only |
| Cached reads | yes | yes | yes | yes | no, every read refolds the history |
| Agent leases (the State API) | yes | yes | yes | yes | no; Go `Resume` with `Unleased: true` only |
| Agent writes fenced on the server | yes | yes | yes, in the write's transaction | yes on a replica set or Atlas, in the write's transaction; checked just before the write on a standalone server | no |
| `recall setup` and the Claude plugin | yes | yes | yes | yes | yes |

## The local database

With nothing named, every command keeps memory in `~/.local/share/recall`:
a polign_db that `recall` starts on first use on a loopback port, protected by
a key generated in that directory, and shared by every agent on the machine.
It keeps running after your program exits; its process id is in
`runtime.json` and its log in `server.log`, both inside the directory.

The server is the `polign-server` beside the `recall` binary or on `PATH`,
which pip and Homebrew install. When there is none, as after `go install`,
`recall` downloads the polign_db release it was tested with (about 80 MB)
from dl.polign.com, checks the archive against a SHA-256 built into the
binary, keeps `polign-server` and its license in `~/.local/share/recall/bin`,
and starts it. That happens once per machine and is reported on stderr.
`RECALL_NO_DOWNLOAD=1` refuses the download; the error then says how to
install polign_db by hand.

## PostgreSQL with pgvector

```sh
export RECALL_BACKEND=pgvector RECALL_BACKEND_URL=postgres://user:password@localhost:5432/mydb
recall mcp -write
```

The first write creates a `recall` schema with one table per collection and
two bookkeeping tables. The role needs `CREATE` on the database, and the
`vector` extension must be installed or creatable by that role.

Things to know:

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

## MongoDB

```sh
export RECALL_BACKEND=mongodb RECALL_BACKEND_URL=mongodb://user:password@localhost:27017/recall
recall mcp -write
```

The database is the connection string's path, `recall` when it has none. The
first write to a collection creates a MongoDB collection named `rc_` plus the
collection's name, and two bookkeeping collections, `recall_collections` and
`recall_leases`. The user needs `readWrite` on the database.

Things to know:

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

## Qdrant

```sh
export RECALL_BACKEND=qdrant RECALL_BACKEND_URL=http://localhost:6333
recall mcp -write
```

Things to know:

- Use a collection only Recall writes to. A point without the `_recall_id`
  payload field makes reads fail.
- A collection takes its vector size from the first write. Changing embedders
  needs a new collection.
- Qdrant grants no leases, so the State API is not served on it. Go `Resume`
  with `Unleased: true` works only when something else keeps one process per
  agent; without a lease, a crashed process that comes back can overwrite its
  replacement.
- With the built-in lexical embedder, exact-word queries rank worse than on
  polign_db. A model embedder (`-embed-model`) helps paraphrases, not keywords.

## Embedders

Two ship with Recall: lexical word search, which needs nothing, and
`local:all-minilm-l6-v2`, a model that runs inside the binary with no key and
no service (about 90 MB, downloaded once on first use). `-embed-model` names an
OpenAI, Ollama or Voyage model instead. A collection is written with one
embedder; search needs the same one. See the
[embedding guide](reference.md#embeddings).

## Adding a backend

Implement `Put`, `List` and `Search`
([contract](reference.md#complete-histories)), register it with
`recall.RegisterBackend` in `init`, pass [`backendtest`](../backendtest), and
import it in [`cmd/recall/backends.go`](../cmd/recall/backends.go). The core
package, `github.com/Polign/recall`, imports nothing outside the standard
library; each backend pulls in its own driver only when imported.
