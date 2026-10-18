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
| MCP tool contract | The tool names, required arguments and read-only marks in [docs/memory-api-v1.md](docs/memory-api-v1.md). A test in the server fails when a served tool and the document disagree. |
| `recall serve` HTTP | `/mcp`, `/v1/remember`, `/v1/recall`, `/v1/forget`, `/v1/history`, `/v1/keys`, `/v1/auth/kubernetes`, and the response contract in the same document. |
| Python client | The memory calls of `polign_recall.Client` (`remember`, `recall`, `forget`, `ask`, `history`, and the result types they return), as documented in [python/README.md](python/README.md). |
| Go memory API | The memory methods of `recall.Client` and the types they take and return: remember, recall, forget, history, events, audit export, the registry. |
| Configuration | The `recall` commands `mcp`, `serve`, `setup` and `doctor`, their documented flags, and the environment variables `RECALL_BACKEND`, `POLIGN_URL`, `POLIGN_API_KEY`, `DATABASE_URL`, `PGPASSWORD`, `MONGODB_URI`, `MONGODB_PASSWORD`, `QDRANT_URL`, `QDRANT_API_KEY`. |

## Beta

These may change in a minor release. Every such change is listed in
[CHANGELOG.md](CHANGELOG.md) under a **Breaking** heading with what to do
about it, and the packages that depend on it (recall-langgraph, recall-livekit
and the rest) are released alongside.

| Surface | Why it is still moving |
|---|---|
| Agent resume and actions | `Client.Resume`, the `Agent` methods, the `agent_*`, `begin_action`, `end_action` and `pending_actions` MCP tools, `ResumedAgent` in Python, `RecallResume.durable` in recall-langgraph, and the wording of the briefing. Actions shipped in 0.21.0 and changed twice in their first week from what a real model did with them. The stored agent records are stable (above); the calls around them are not yet. |
| Backend interfaces for implementors | `recall.Backend` (Put, List, Search) is stable. The optional capability interfaces (`GetBackend`, `TextSearchBackend`, `WatermarkBackend`, `LeaseBackend`, `FencedBackend`) may gain methods, new ones may appear, and the conformance suite in `backendtest` may add checks a backend has to pass. Using a backend is stable; writing one is not yet. |
| MongoDB backend | First released in 0.21.0. Its on-disk layout may change before 1.0, with a migration note if it does. |
| Text extraction | Which facts the extractor draws from a passage, and the default model behind it, may change as the evaluations improve. What is stored, and how it is read back, does not. |

## How changes are made

- A stable surface is deprecated for at least one minor release before it is
  removed, and never removed within 0.x once 1.0 is announced. After 1.0, a
  removal needs 2.0.
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
3. Agent resume and actions have either been stable for the same four weeks
   or are explicitly excluded from the 1.0 promise in this file.
4. The release checks above have been blocking CI for the whole period.

Until then, the version number says 0.x and this file says what that means.
