# One way to say where memory lives

Status: decided 2026-10-23 (see the end). Phases 1 and 2 built the same day
on `at/database-config` in Polign/recall and Polign/polign, unreleased.
Phase 3 (the site) waits for 0.23.0 on every channel. `Client.database`
reads the `database` the binary reports in the MCP initialize reply;
recall-vapi uses it, and falls back to `runtime.json` and `local-key` with an
older binary.

## The problem

Running Recall today means learning a different connection story for each
backend and each surface:

| Surface | How it finds the database today |
|---|---|
| `recall mcp` | `-backend NAME` (or `RECALL_BACKEND`), then `-url` (or the backend's own variable: `POLIGN_URL`, `DATABASE_URL`, `MONGODB_URI`, `QDRANT_URL`), then the backend's own key variable (`POLIGN_API_KEY`, `PGPASSWORD`, `MONGODB_PASSWORD`, `QDRANT_API_KEY`). With nothing set it serves the saved `recall setup`, and with no setup it tries `http://localhost:23000` and fails if nothing listens there. |
| `recall ask`, `history`, `diff`, `explain`, `reembed`, `key` | Same as `mcp`, each with its own copy of the "saved setup or URL" logic. |
| `recall serve` | `-backend` and `-url` only; no saved setup, no default. |
| `recall setup`, `recall doctor`, the Claude Code plugin | polign_db only. On any other backend the docs say "add the MCP server yourself". |
| Python `Client` | Three exclusive ways: `local_dir=` (managed local database), `url=` + `api_key=` (a `recall serve`), or `env={"POLIGN_URL": ...}` (a polign_db). Bare `Client()` has the same `localhost:23000` failure as bare `recall mcp`. |
| recall-livekit, recall-langgraph, recall-vapi | `local_dir=` or `url=`/`env=` that become `POLIGN_URL` for the subprocess. Nothing else is reachable through them. |
| Helm chart | `backend.kind` plus `backend.url` plus a Secret. This one is already right. |
| Recall settings that are not about the database | `POLIGN_COLLECTION`, `POLIGN_PREDICATES`, `POLIGN_EMBED_MODEL`, `POLIGN_EMBED_URL`, `POLIGN_EXTRACT_MODEL`, `POLIGN_RECALL_HOME`. They carry the polign_db prefix because Recall used to live inside the polign CLI. |

Two things are wrong with this.

1. **Nothing works until something is configured.** `pip install polign-recall`
   brings the database with it, and `recall setup -local` and
   `Client(local_dir=...)` know how to run it, but bare `recall mcp -write`
   and bare `Client()` do not use that path. A first run fails with a
   connection error to a port nothing listens on.
2. **Each backend has its own vocabulary.** A user who switches from polign_db
   to pgvector learns `DATABASE_URL` and `PGPASSWORD`; to Qdrant, `QDRANT_URL`
   and `QDRANT_API_KEY`. Setup, doctor and the plugin stop working. The
   Python integrations cannot reach those backends at all.

## The rule

Three things describe the database, and they are the same three for every
backend, every command, the Python client, every integration and the chart:

```
-backend NAME     RECALL_BACKEND        which backend: local, polign, pgvector, mongodb, qdrant
-url URL          RECALL_BACKEND_URL    where it is, in that backend's own URL form
-key KEY          RECALL_BACKEND_KEY    the credential, when the backend takes one and the URL does not carry it
```

Anything a backend wants beyond that is a query parameter on the URL (see
"Backend options"). There are no other database variables.

When nothing is given, the backend is `local`: a polign_db that Recall runs
itself, kept in `$RECALL_HOME`. Each value resolves in the same order
everywhere:

1. The flag (`backend=`, `backend_url=`, `backend_key=` in Python).
2. The environment variable.
3. The saved setup in `$RECALL_HOME/config.json`, written by `recall setup`.
4. The default: backend `local`; the URL each backend lists in the table
   below; no key.

One compatibility step sits between 3 and 4 for the backend alone: with no
backend named or saved, a polign_db already answering at
`http://localhost:23000` is used, because that is where 0.22.1 looked. One
stderr line asks for `RECALL_BACKEND=polign` to make it explicit. The attempt
is a loopback connect, refused in under a millisecond when nothing listens,
and it goes away with the other deprecated behavior. `recall serve` had the
same `:23000` default on 0.22.1, so the step applies to it too.

`-backend` decides. A `RECALL_BACKEND_URL` whose scheme does not fit the backend
is an error that names the fix:

```
recall: RECALL_BACKEND_URL is a postgres:// URL but the backend is local (the
default). Set RECALL_BACKEND=pgvector to use it, or unset RECALL_BACKEND_URL.
```

The name carries Recall's prefix so that an application's own `DATABASE_URL`
in the same shell is never mistaken for Recall's database. (pgvector used
`DATABASE_URL` through 0.22.1; it stays as an alias for that backend.)

`recall serve` follows the same rule, local default included. See "serve"
below.

### Backends

| `-backend` | `RECALL_BACKEND_URL` form | Default URL | `RECALL_BACKEND_KEY` is |
|---|---|---|---|
| `local` (the default) | a directory: `./dir`, `/dir`, `~/dir` | `$RECALL_HOME` | not used; the key is generated and kept in the directory |
| `polign` | `http://host:23000`, `https://host` | `http://localhost:23000` | the polign_db API key (`plgn_...`) |
| `pgvector` | `postgres://user:pw@host:5432/db`, `postgresql://` | `postgres://localhost:5432/postgres` | the password, when the URL has none |
| `mongodb` | `mongodb://user:pw@host:27017/db`, `mongodb+srv://` | `mongodb://localhost:27017/recall` | the password, when the URL has none |
| `qdrant` | `http://host:6333`, `https://cluster.cloud.qdrant.io:6333` | `http://localhost:6333` | the Qdrant API key |

Credentials never go in the URL for polign and qdrant (the polign backend
already refuses userinfo, because URLs end up in logs and process lists).
They go in `RECALL_BACKEND_KEY`, `-key`, or `backend_key=` in Python. For
postgres and mongodb the URL may carry the password, since that is their own
convention; `RECALL_BACKEND_KEY` fills it in when it does not.

### Backend options

What a backend takes beyond the URL and the key goes on the URL's query
string. Recall's own keys are documented per backend and removed before the
driver sees the URL; anything else in the query passes through to the driver
(pgx's `sslmode`, MongoDB's `replicaSet`, and so on).

| Backend | Key | Default | Today |
|---|---|---|---|
| `pgvector` | `schema` | `recall` | Go `Config.Schema` |
| `pgvector` | `text_search` | `english` | Go `Config.TextSearchConfig` |
| `qdrant` | `distance` | `Cosine` | Go `Config.Distance` |
| `mongodb` | none; the database is the URL's path | `recall` | |
| `polign` | none | | |
| `local` | none | | |

```sh
RECALL_BACKEND=pgvector RECALL_BACKEND_URL='postgres://recall@db.internal/recall?schema=memory&sslmode=require' RECALL_BACKEND_KEY=... recall mcp -write
```

This is the one mechanism for backend-specific settings. No backend adds a
flag or a variable of its own. A backend that needs a new option adds a
documented query key.

### The local backend

`local` is the machinery `recall setup -local` has today
(`ensureRecallServer`: a detached `polign-server -store fs:DIR/data -http
127.0.0.1:0 -require-data-key -bootstrap-key-file ... -runtime-file ...`),
moved out of `cmd/recall` into a backend package and registered like the
others, so the Go library gets it too:

```go
import _ "github.com/Polign/recall/backend/local"
b, err := recall.OpenBackend("local", recall.BackendOptions{URL: "~/agent-memory"})
```

The directory holds:

```
config.json     what recall setup saved, if anything (only in $RECALL_HOME)
data/           the polign_db store (fs:)
local-key       the key the local server accepts
runtime.json    the running server's URL and pid
server.log
launch          the Claude plugin's launcher (only in $RECALL_HOME)
```

The first process to open a directory starts the server on a free loopback
port; later processes find it through `runtime.json`. The default directory
is `$RECALL_HOME`, so with nothing configured every agent on the machine
shares one memory, which is the point: the Claude plugin and a Python script
remember the same things.

`RECALL_HOME` defaults to `~/.local/share/recall`. `POLIGN_RECALL_HOME` and
the old default `~/.config/polign/recall` keep working: when `RECALL_HOME` is
unset and the old directory holds a `config.json`, that directory is the home,
and `recall doctor` says so. Nothing is moved.

The local backend needs `polign-server` beside the `recall` binary or on
PATH. pip and Homebrew install both; a `go get` user without it gets one
error naming the two install commands. Windows: the local backend does not
need the `/bin/sh` launcher, only process detachment, which
`detach_windows.go` already does, so `recall mcp` with nothing configured
should work on Windows. `recall setup` (the plugin installer) stays Unix-only
until the launcher is a batch file.

### One prefix for Recall's own settings

Settings that are about Recall, not about the database, take the `RECALL_`
prefix. The old names stay as aliases (see "Compatibility with 0.22.1").

| Today | After |
|---|---|
| `POLIGN_COLLECTION` | `RECALL_COLLECTION` |
| `POLIGN_PREDICATES` | `RECALL_PREDICATES` |
| `POLIGN_EMBED_MODEL` | `RECALL_EMBED_MODEL` |
| `POLIGN_EMBED_URL` | `RECALL_EMBED_URL` |
| `POLIGN_EXTRACT_MODEL` | `RECALL_EXTRACT_MODEL` |
| `POLIGN_RECALL_HOME` | `RECALL_HOME` |

`RECALL_BACKEND`, `RECALL_TYPED`, `RECALL_OPEN`, `RECALL_LISTEN` and the
`RECALL_KUBERNETES_*` family already have the right prefix and do not change.
`RECALL_URL`, `RECALL_API_KEY` and `RECALL_TOKEN_FILE` keep their meaning: a
running `recall serve`, which `recall connect`, the Python `Client(url=...)`
and the OpenClaw plugin talk to. That is a Recall server, not a database, and
this plan leaves it alone.

### What each surface looks like after

**First run, nothing configured.** Works, keeps memory in `~/.local/share/recall`:

```sh
pip install polign-recall
recall mcp -write
```

```python
from polign_recall import Client
with Client() as memory:          # the same local memory the Claude plugin uses
    memory.remember("I switched from vim to neovim")
```

**Another backend.** Two variables, the same two for every backend:

```sh
RECALL_BACKEND=qdrant RECALL_BACKEND_URL=http://localhost:6333 recall mcp -write
RECALL_BACKEND=pgvector RECALL_BACKEND_URL=postgres://recall:pw@db/recall recall mcp -write
RECALL_BACKEND=mongodb RECALL_BACKEND_URL=mongodb://db/recall RECALL_BACKEND_KEY=pw recall mcp -write
```

**Claude Code.** `recall setup` with no flags saves the local backend and
installs the plugin, as `-local` does today. `recall setup -backend qdrant
-url http://localhost:6333` does the same on Qdrant. Setup and doctor work on
every backend; the "add the MCP server yourself" paragraphs on the Qdrant,
pgvector and MongoDB pages go away.

**Any MCP host.**

```json
{"mcpServers": {"recall": {"command": "recall", "args": ["mcp", "-write"],
  "env": {"RECALL_BACKEND": "pgvector", "RECALL_BACKEND_URL": "postgres://recall:pw@db.internal/recall"}}}}
```

**Python.** `Client(backend=, backend_url=, backend_key=)` mirror the three
names. Bare `Client()` follows the rule. `url=` and `api_key=` keep meaning a
`recall serve`, as today.

```python
Client()                                                        # local, or what the environment says
Client(backend="local", backend_url="./recall-data")           # local, in this directory (local_dir= still works)
Client(backend="polign", backend_url="http://memory.internal:23000", backend_key=k)
Client(backend="qdrant", backend_url="https://x.cloud.qdrant.io:6333", backend_key=k)
Client(url="http://memory.internal:8080", api_key=k)            # a recall serve, unchanged
```

**Integrations.** `RecallMemory.open(...)`, `RecallResume(name, ...)` and the
Vapi and Pydantic AI examples take `backend=`, `backend_url=` and
`backend_key=` and pass them through. They reach every backend for the first
time.

**Helm chart.** Already `backend.kind`, `backend.url` and a Secret for the
key. The Secret becomes `RECALL_BACKEND_KEY` in the pod instead of the backend's
own variable; `backend.kind` grows the two values it lacks (`pgvector`,
`mongodb`). Nothing a current `values.yaml` says changes meaning.

**`recall serve`.** The same three names and the same default. With nothing
configured, `recall serve` starts on the local database in `$RECALL_HOME`, and
`recall key create -namespace team:agent-1` against the same default mints a
key for it, so a shared memory server for several agents on one machine is
two commands with no configuration. This reverses the 2026-10-22 call that a
zero-config `serve` was not worth having; the dropped `at/serve-local`
branch (commit 732674c) built the fallback once and is the starting point.
`serve` prints the resolved backend and URL at startup, so a container that
meant to name a database and did not is told on its first log line that it is
writing to a local directory.

**Errors.** An explicit URL that cannot be reached says what it tried, which
variable it came from, and what the default would have been:

```
recall: nothing is listening at http://localhost:23000 (RECALL_BACKEND_URL, backend
polign from RECALL_BACKEND). Start polign-server there, or unset both to keep
memory in ~/.local/share/recall.
```

**`recall doctor`.** Prints the resolved backend, URL and where each came from
(flag, environment, saved setup, default), then checks the connection, on
every backend.

## Compatibility with 0.22.1

The promise: a 0.22.1 installation upgraded to 0.23.0, with its configuration
untouched, reaches the same database, the same collection and the same tools.
The only behavior that changes is in the one case where 0.22.1 failed: nothing
configured, nothing listening, which now gets the local backend instead of a
connection error. Stored records do not change; a database written by 0.22.1
is read by 0.23.0 as it is.

How each 0.22.1 configuration is honored:

| 0.22.1 configuration | 0.23.0 behavior |
|---|---|
| Nothing set, `polign-server` on `:23000` | The compatibility step finds it. Same database. One stderr line per run suggests `RECALL_BACKEND=polign`. |
| Nothing set, nothing listening | 0.22.1 failed. 0.23.0 uses the local backend. |
| `recall setup` done (`config.json` under `~/.config/polign/recall`) | That directory is `RECALL_HOME` when the variable is unset and the old directory holds a `config.json`. The saved `launch` file runs `recall mcp -config-dir DIR`, and `-config-dir` stays a flag on every command that had it. A `config.json` with `url`/`server` and no `backend` is read as polign or local, as it is today. |
| `POLIGN_RECALL_HOME=DIR` | Honored as `RECALL_HOME`. |
| `POLIGN_URL`, `POLIGN_API_KEY` | Aliases of `RECALL_BACKEND_URL` and `RECALL_BACKEND_KEY` when the backend is polign. |
| `RECALL_BACKEND=pgvector` with `DATABASE_URL`/`PGPASSWORD` | Aliases of `RECALL_BACKEND_URL` and `RECALL_BACKEND_KEY` for this backend. |
| `RECALL_BACKEND=mongodb` with `MONGODB_URI`/`MONGODB_PASSWORD` | Aliases of `RECALL_BACKEND_URL` and `RECALL_BACKEND_KEY` for this backend. |
| `RECALL_BACKEND=qdrant` with `QDRANT_URL`/`QDRANT_API_KEY` | Aliases of `RECALL_BACKEND_URL` and `RECALL_BACKEND_KEY` for this backend. |
| `RECALL_BACKEND=X` with no URL | The backend's default URL, as today. |
| `recall -backend X -url Y -key K mcp` | Unchanged. The flags are already the right ones; they are also accepted after the subcommand now. |
| `recall setup -local`, `recall setup -url X` | `-local` is `-backend local`; `-url X` with no `-backend` is `-backend polign -url X`, as it meant before. |
| `recall connect` with `RECALL_URL`, `RECALL_API_KEY`, `RECALL_TOKEN_FILE` | Unchanged. |
| Python `Client(local_dir=X)` | Unchanged result: `backend="local", backend_url=X`. `local_dir=` stays as the short spelling and is not deprecated. |
| Python `Client(url=X, api_key=K)` | Unchanged: a `recall serve`. |
| Python `Client(env={"POLIGN_URL": ...})`, `Client.from_env()` | Unchanged results; the old variables alias as above. |
| recall-livekit, recall-langgraph, recall-vapi, recall-pydantic-ai on their current releases, against recall 0.23.0 | Work unchanged: they pass `POLIGN_URL` or run `recall setup -local -no-plugin -config-dir DIR`, both of which 0.23.0 accepts. `runtime.json` and `local-key` stay in the directory with the same contents, because recall-vapi 0.2.0 reads them. |
| polign-recall 0.22.1 against recall 0.23.0 | Works unchanged, for the same reasons. |
| polign-recall 0.23.0 against recall 0.22.1 (a Homebrew `recall` the pip wheel did not replace, or `RECALL_BIN`) | The client runs `recall -version` once and, below 0.23, translates: `backend="local"` runs `recall setup -local -config-dir` as today; the others set `RECALL_BACKEND` and the backend's 0.22.1 variables. A `?schema=` style option 0.22.1 cannot take gets a `RecallError` naming the upgrade. |
| `polign recall mcp`, `polign mcp -memory-only` (polign_db hand-off) | Unchanged: the hand-off sets `POLIGN_URL`, which aliases. |
| Helm chart `backend.kind`, `backend.url`, `backend.existingSecret` | Same values, same meaning. The Secret is mounted as `RECALL_BACKEND_KEY`; the chart on an older image mounts it under the old name too, for one chart minor. |
| OpenClaw plugin `url`/`apiKey` and `RECALL_URL`/`RECALL_API_KEY` | Unchanged. |
| Go: `recall.BackendDriver{URLEnv, KeyEnv, DefaultURL}` | Fields stay. `URLEnv` and `KeyEnv` are the alias names the fold reads; `DefaultURL` is still the default. Nothing a custom backend does today stops compiling. |

Precedence when old and new names are both set: the flag, then
`RECALL_BACKEND_URL`/`RECALL_BACKEND_KEY`, then the backend's old variable. So adding the
new variable never silently loses to an old one left in a shell profile, and
not adding it changes nothing.

Deprecation follows `STABILITY.md`, which lists the old names as stable:

- 0.23.0 adds the new names and prints one stderr line when an old name
  decided the URL, the key or a setting. `STABILITY.md`'s Configuration row
  changes to `RECALL_BACKEND`, `RECALL_BACKEND_URL`, `RECALL_BACKEND_KEY`, `RECALL_HOME`
  and the `RECALL_`-prefixed settings, and lists the old names as deprecated
  with the release they go away in.
- The old names, the `:23000` compatibility step and the `URLEnv` and
  `KeyEnv` fields are removed no earlier than 0.25.0, in a release whose
  changelog has a **Breaking** heading. If 1.0 is announced before then they
  stay until 2.0, per the stability rules.

## Phases

One release: recall 0.23.0 and polign-recall 0.23.0 on the same tag, with
patch releases of recall-langgraph, recall-livekit, recall-vapi and
recall-pydantic-ai and chart 0.4.0 right after. Nobody outside us runs the
Python client against an older binary, so the client guards the mismatch (as
the 0.18.0 text-default release did) instead of a two-step rollout.

### 1. The binary

- `backends.go`: `BackendOptions` gains `Params map[string]string` (the
  Recall query keys, already stripped from `URL`); `BackendDriver` gains
  `Params []string`, the keys it accepts, so an unknown key is an error
  before any connection. `URLEnv`, `KeyEnv` and `DefaultURL` stay; the alias
  fold in `cmd/recall` reads the first two until 0.25.
- Each backend's `init` maps its params onto its `Config` (`schema` and
  `text_search` for pgvector, `distance` for qdrant). The polign backend
  stops refusing a query string; it refuses unknown keys instead.
- New package `backend/local`: the managed local server, moved from
  `cmd/recall/recall_setup.go` (`ensureRecallServer`, `localServerPath`, the
  key helpers) and `internal/recallsetup`. Registers as `local`, URL a
  directory, default `$RECALL_HOME`. Opens a `polign.Backend` against
  `runtime.json`'s URL after ensuring the server.
- `cmd/recall`: one `resolveDatabase(flags, env, home)` returning backend,
  URL, key and the source of each, used by every subcommand. It replaces the
  four copies of "saved setup or URL" in `mcp.go`, `explain.go`
  (`openCLIMemory`), `reembed.go` and `recall_setup.go`, and the per-driver
  environment lookup in `main.go`. `-backend`, `-url` and `-key` are accepted
  after the subcommand as well as before it. The `:23000` compatibility step
  lives here.
- `recall setup`: saves any backend, not only polign. `config.json` gains a
  `backend` field; a config without one is read as today. `-local` is
  `-backend local`.
- `recall doctor`: resolved backend, URL and sources, any backend.
- `recall serve`: the shared resolver, local default included, and the
  startup line naming the resolved backend and URL.
- `RECALL_HOME` with the old-directory fallback; `RECALL_` names for the
  settings; aliases with the stderr note.
- Tests: a table test over `resolveDatabase` (every combination of flag,
  environment, saved setup, old names, and the mismatch error); one test per
  subcommand that it calls the shared resolver; `backend/local` started
  twice and written through with a real `polign-server` when one is on PATH
  (not the conformance suite: the local server runs cold-first, as setup
  always started it, and answers text search only once an index is
  published; the polign backend's own tests cover the rest);
  query params reaching each backend's `Config`; a Windows build of the local
  backend; a compatibility test that replays each row of the table above (a
  0.22.1 `config.json` and `launch` file, each old variable set, a server on
  `:23000` with nothing set) and asserts the same database is reached.
- Docs in this repo: README install and quickstart (bare `recall mcp -write`
  and bare `Client()` are the first thing shown; `local_dir` leaves the
  quickstart), the Backends table (one row for `RECALL_BACKEND`,
  `RECALL_BACKEND_URL` and `RECALL_BACKEND_KEY` instead of "URL and key variables", plus a
  row for the query keys), `docs/mcp.md` (the three names), `STABILITY.md`,
  `CHANGELOG.md`.

### 2. Python, the integrations, the chart, the plugins

- `Client(backend=None, backend_url=None, backend_key=None, ...)`. Bare
  `Client()` spawns `recall mcp` with nothing, so the binary resolves it. The
  three become `RECALL_BACKEND`, `RECALL_BACKEND_URL` and `RECALL_BACKEND_KEY` in the
  subprocess environment, and the old connection variables are stripped from
  the inherited environment when any of the three is given, as `_CONNECTION`
  does today for `local_dir`. `local_dir=X` stays as `backend="local",
  backend_url=X`.
- `_local_server` stays as the path for a binary older than 0.23 (see the
  compatibility table); against 0.23.0 the binary starts the local server
  itself. The `local` backend keeps writing `runtime.json` and `local-key`
  in the directory, unchanged, so recall-vapi 0.2.0 and polign-recall 0.22.1
  keep reading them. A `Client.database` property (the resolved backend and
  URL, reported by the binary in the MCP `initialize` reply) gives
  recall-vapi a cleaner way to get the address in its next release.
- The version probe: `recall -version` once per process, cached. Below 0.23,
  the three names are translated into the 0.22.1 variables.
- `RecallMemory.open(backend=, backend_url=, backend_key=)`,
  `RecallResume(..., backend=, backend_url=, backend_key=)`; the Vapi and
  Pydantic AI READMEs show the same.
- Chart: `backend.kind` accepts `pgvector` and `mongodb`; the Secret is
  mounted as `RECALL_BACKEND_KEY` (and the old name for one minor); README rows.
- Claude plugin: `start.sh` fallback becomes `exec recall mcp -write`;
  `docs/setup.md` on the three names.

### 3. The site (polign_web, once 0.23.0 is on every channel)

- Quickstart: bare `Client()`; `local_dir` gone; "Any MCP host" shows the
  three names.
- Qdrant, pgvector, MongoDB pages: `-e RECALL_BACKEND=... -e RECALL_BACKEND_URL=...`;
  drop the "setup and the plugin are polign_db
  only" paragraphs; `recall setup -backend ... -url ...` for Claude Code; the
  query keys under "Good to know".
- Claude Code page: `recall setup` unchanged on the surface; add the one line
  for another backend.
- Architecture page and `docs/architecture.md` in this repo: the
  configuration paragraph, kept in sync as the memory note requires.

## Not in this plan

- Inferring the backend from the URL's scheme. `-backend` decides, and a
  scheme that does not fit is an error naming the fix.
- Folding `recall connect` and the `recall serve` client variables
  (`RECALL_URL`, `RECALL_API_KEY`) into the database names. A Recall server
  is a different layer from a database, and the names are already Recall's.
- Starting a local `polign-server` against a bucket (`RECALL_BACKEND_URL=s3://...`
  with the local backend). It is what the demos do by hand and would be a
  natural option later, but it drags bucket credentials and the server's
  object-store flags into Recall's surface. Refused with a clear message.
- A SQLite or in-process backend. The local backend is polign_db in a child
  process, which already exists and is what every other path is tested
  against.
- Changing the default collection name or the per-embedder collection rule.

## Decided 2026-10-23

1. `RECALL_HOME` defaults to `~/.local/share/recall`; the old
   `~/.config/polign/recall` is honored when it already holds a setup.
2. `recall serve` gets the local default like every other command.
3. The connection variable is `RECALL_BACKEND_URL`, not `DATABASE_URL`, with
   `RECALL_BACKEND_KEY` beside it. `DATABASE_URL` stays as pgvector's alias
   until the other old names go.
