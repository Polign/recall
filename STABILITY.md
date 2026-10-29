# Stability

Recall is at 0.x. This file says which parts you can build on today without
being broken by a release, which parts are still moving, and what has to be
true before 1.0.

## Stable

These surfaces do not change in a way that breaks existing users within 0.x,
and 1.0 carries them unchanged. Additions are allowed: new tools, new optional
arguments, new fields, new backends. Removals and renames are not.

| Surface | What is covered |
|---|---|
| Stored records | The event format (`recall-event-v3`), the fold rules (`recall-fold-v4`), and the agent records (working state, turns, pointers, outputs, actions). A store written by any 0.x release is read by every later release. New fields may appear; existing fields keep their meaning. |
| Audit bundles | The bundle format and `recall-audit`, which replays a bundle offline. A bundle exported today verifies with every later verifier. |
| MCP tool contract | The tool names, required arguments and read-only marks of the memory tools in [docs/memory-api-v1.md](docs/memory-api-v1.md). A test in the server fails when a served tool and the document disagree. The State API's tools are beta (below). |
| `recall serve` HTTP | `/mcp`, `/v1/remember`, `/v1/recall`, `/v1/forget`, `/v1/history`, `/v1/keys`, `/v1/auth/kubernetes`, and the response contract in the same document and in [docs/state-api-v1.md](docs/state-api-v1.md). The State API routes under `/v1/agent/` are beta (below). |
| Python client | The memory calls of `polign_recall.Client` (`remember`, `recall`, `forget`, `ask`, `history`, and the result types they return), as documented in [python/README.md](python/README.md). |
| Go memory API | The memory methods of `recall.Client` and the types they take and return: remember, recall, forget, history, events, audit export, the registry. |
| Configuration | The `recall` commands `mcp`, `serve`, `setup` and `doctor`, their documented flags, and the three database settings every backend and every command share: `-backend` / `RECALL_BACKEND`, `-url` / `RECALL_BACKEND_URL` and `-key` / `RECALL_BACKEND_KEY`, with a backend's other options as query keys on the URL. Recall's own settings under the `RECALL_` prefix: `RECALL_HOME`, `RECALL_COLLECTION`, `RECALL_PREDICATES`, `RECALL_EMBED_MODEL`, `RECALL_EMBED_URL`, `RECALL_EXTRACT_MODEL`. The names each backend had before 0.23.0 are not part of this surface; they are listed under Deprecated below. |

## Beta

These may change in a minor release. Every such change is listed in
[CHANGELOG.md](CHANGELOG.md) under a **Breaking** heading with what to do
about it, and the packages that depend on it (recall-langgraph, recall-livekit
and the rest) are released alongside.

| Surface | Why it is still moving |
|---|---|
| State API | The contract in [docs/state-api-v1.md](docs/state-api-v1.md): `Client.Resume`, the `Agent` methods, the `agent_*`, `begin_action`, `end_action` and `pending_actions` MCP tools, the HTTP routes under `/v1/agent/` and the `If-Lease` epoch they carry, `ResumedAgent` in Python, `RecallResume.durable` in recall-langgraph, and the wording of the briefing. Actions shipped in 0.21.0 and changed twice in their first week from what a real model did with them. The stored agent records are stable (above); the calls around them are not yet. |
| Backend interfaces for implementors | `recall.Backend` (Put, List, Search) is stable. The optional capability interfaces (`GetBackend`, `TextSearchBackend`, `WatermarkBackend`, `LeaseBackend`, `FencedBackend`) may gain methods, new ones may appear, and the conformance suite in `backendtest` may add checks a backend has to pass. Using a backend is stable; writing one is not yet. |
| MongoDB backend | First released in 0.21.0. Its on-disk layout may change before 1.0, with a migration note if it does. |
| Text extraction | Which facts the extractor draws from a passage, and the default model behind it, may change as the evaluations improve. What is stored, and how it is read back, does not. |

## Deprecated

These still work, and are going away. Each prints one line on stderr the
first time it is read, naming what replaces it. None of them is carried
into 1.0.

They go because one set of names for every backend means a configuration
written for one backend moves to another by changing one value, the
documentation shows one example instead of one per backend, and a program
opening Recall needs to know three names, not a dozen.

| What | Deprecated in | Use instead | Removed |
|---|---|---|---|
| The URL variable each backend had: `POLIGN_URL`, `DATABASE_URL`, `MONGODB_URI`, `QDRANT_URL` | 0.23.0 | `RECALL_BACKEND_URL` (or `-url`), with `RECALL_BACKEND` naming the backend | No earlier than 0.25.0 |
| The key variable each backend had: `POLIGN_API_KEY`, `PGPASSWORD`, `MONGODB_PASSWORD`, `QDRANT_API_KEY` | 0.23.0 | `RECALL_BACKEND_KEY` (or `-key`), or credentials in the URL where the backend's URL form carries them | No earlier than 0.25.0 |
| The `POLIGN_` spelling of Recall's own settings: `POLIGN_COLLECTION`, `POLIGN_PREDICATES`, `POLIGN_EMBED_MODEL`, `POLIGN_EMBED_URL`, `POLIGN_EXTRACT_MODEL`, and `POLIGN_RECALL_HOME` | 0.23.0 | The same name under `RECALL_`; `RECALL_HOME` for the last | No earlier than 0.25.0 |
| Using a polign_db that answers at `http://localhost:23000` when nothing names a backend | 0.23.0 | `RECALL_BACKEND=polign` to keep it, `RECALL_BACKEND=local` for the local database | No earlier than 0.25.0; after that, nothing named means local |

Not on this list: a setup file saved by a release before 0.23.0 is still
read as it is, and `~/.config/polign/recall` is still the home when it holds
one. If either changes, it is listed here first.

Removal happens in a minor release, under a **Breaking** heading in
[CHANGELOG.md](CHANGELOG.md) with the one-line change to make. Until then,
every release reads the old names and prints the line.

## How changes are made

- A stable surface is deprecated for at least one minor release before it is
  removed, and never removed within 0.x once 1.0 is announced. After 1.0, a
  removal needs 2.0.
- Deprecating something means: it is listed under Deprecated above in the
  release that deprecates it, with its replacement and the earliest release
  that removes it; it warns once per process on stderr for as long as it is
  kept; and the release that removes it says so under a **Breaking**
  heading. Nothing is removed in a patch release.
- A beta surface may change in a minor release. Patch releases never change
  behavior on purpose; they fix bugs and ship security updates.
- Security fixes to the binaries and their dependencies ship as a patch
  release of the current minor.
- Every release is checked before it is tagged: the test suite against
  polign_db, PostgreSQL and MongoDB, `govulncheck` on the source and on the
  built binary, open Dependabot alerts, and a build and smoke test of the
  container image. These run in CI on every change.

## What 1.0 needs

1.0 is tagged when all of these hold, not on a date:

1. Four weeks with no breaking change to a stable surface above.
2. At least one team outside Polign running Recall for real work has upgraded
   across a release on the stable surfaces without changing their code.
3. The State API has either been stable for the same four weeks or is
   explicitly excluded from the 1.0 promise in this file.
4. The release checks above have been blocking CI for the whole period.
5. Everything under Deprecated above has been removed, so 1.0 ships one set
   of names and no deprecation warnings.

Until then, the version number says 0.x and this file says what that means.
