# Recall Memory API, version 1

This is the contract an agent, a runtime or a platform can build against. It names the operations, what every call carries, what every answer carries, and what a storage backend has to provide for each part to work. It changes only with a version bump; additions within version 1 are marked by the Recall release that introduced them.

The MCP tool definitions served by `recall mcp` are the authoritative form. The tables in this document are checked against them by a test in `cmd/recall`, so a tool cannot change without this document changing.

Status words used below:

- **0.15**: in recall v0.15.0 and later.
- **0.16**: planned for recall 0.16.0, the release that adds `recall serve`.

## 1. The model in one paragraph

Memory is an append-only log of statements. A statement says that a **subject** stands in a **predicate** relation to a **value**, with a confidence, a source, and the instant it was observed. Predicates are declared in a **registry** that fixes each one's cardinality and value type. Reading folds the log: a single-valued predicate's newest statement is the belief, a multi-valued predicate's statements accumulate, a retraction withdraws, and a statement can name other beliefs it **replaces**. Nothing is deleted, so a read can be asked **as of** any earlier instant.

## 2. Operations

Four verbs carry the model. Two read helpers go with them.

| Verb | What it does | Status |
| --- | --- | --- |
| remember | Append a statement, or statements proposed from text. Single-valued predicates supersede; restating is a no-op; `replaces` records a correction across predicates. | 0.15 |
| recall | Return current beliefs for a subject and predicate, or for a query. Every belief carries what it replaced. `as_of` reads the past. | 0.15 |
| forget | Append a retraction for a subject and predicate, or for one value. Nothing is deleted. | 0.15 |
| memory_history | Every statement ever recorded for a subject and predicate, oldest first. | 0.15 |
| list_predicates | The registry: each predicate's cardinality, type, exclusions, historical flag. | 0.15 |
| explain | Why each belief a question finds is held: source text, definitions, history. | 0.15 |

### 2.1 Typed tools

Served by `recall mcp` by default. A server started without write access serves only the read-only rows.

| Tool | Required arguments | Read-only |
| --- | --- | --- |
| list_predicates | none | yes |
| remember | none at the top level; either `text` (with `statements` unless the server has an extraction model) or `subject`, `predicate`, `value` | no |
| recall | none | yes |
| forget | subject, predicate | no |
| explain | question | yes |
| memory_history | subject, predicate | yes |

### 2.2 Text tools

Served by `recall mcp` (the default; `-typed` serves the typed tools instead). Same log, same fold; the server works out subjects and predicates from the text. Text mode is the on-ramp: a client that starts here loses nothing when it moves to typed statements, because the records are the same kind.

| Tool | Required arguments | Read-only |
| --- | --- | --- |
| remember | none | no |
| recall | none | yes |
| forget | text | no |

Text `remember` takes `text` or `messages`, one of them: messages are `{role, content}` turns with role `user` or `assistant`, and only what the user said or agreed to is kept. Text `recall` takes `question`, `as_of` and `since`, all optional. With a question it searches; without one it reads with no model and no search: everything believed about the user, or with `since`, everything stated after that instant about anyone, newest first, each with what it replaced. With a model embedder, an answer to a question leaves out beliefs scoring under half the best match's similarity.

### 2.3 Argument conventions

- Instants are RFC 3339 strings, UTC preferred: `2026-09-01T00:00:00Z`.
- `as_of`, `since`, `observed_after`, `observed_before` are instants. `observed_at` on a write dates the statement; a statement dated before a newer one for the same subject and predicate becomes history and replaces nothing.
- `value` matches the predicate's declared type: string, number or boolean. Registries may further declare enum, date and reference types; the value is still carried as one of the three JSON scalars.
- Unknown arguments are rejected. Clients must not send fields this document does not name.

## 3. Identity envelope

Every call is made within an identity. Four fields describe it.

| Field | Meaning | Who sets it | Status |
| --- | --- | --- | --- |
| namespace | Who may read and write these records. Enforced by the credential, never by the caller. | The server, from the key or the token exchange. | 0.16 (`recall serve`). Today a polign_db key bound to a namespace gives the same guarantee one layer down. |
| subject | Who or what the statement is about. | The caller. Default `user`. | 0.15 |
| agent | Which agent made or holds the statement. The same value names the agent for the Resume API. | The caller, or the server in Kubernetes mode. | 0.16 |
| session | Which run produced the statement. In Kubernetes mode, the Sandbox name. | The caller, or the server in Kubernetes mode. | 0.16 |

Rules:

- A request that names a `namespace` different from the credential's is rejected with `403 namespace_mismatch`. It is never narrowed to the credential's namespace silently.
- In Kubernetes mode `agent` and `session` are derived by the server from the pod identity. A request that disagrees is rejected the same way.
- Namespace names: letters, digits and `_ - . :`, starting with a letter or digit, at most 128 bytes. The convention for a tenant's agent run is `<tenant>:<run-name>`.
- `agent` and `session` are stored on each event (0.16) and are filterable on recall. A filter constrains only the fields it names.

## 4. Response contract

Every belief returned by recall carries these fields. They are the reason to use Recall rather than a search index, so a client may rely on them.

| Field | Meaning | Status |
| --- | --- | --- |
| subject, predicate, value | The belief. | 0.15 |
| confidence, source, observed_at | How sure, how come by, when stated. | 0.15 |
| days_ago | Days between `observed_at` and `as_of` (or now). | 0.15 |
| replaced | The value this belief replaced, when that was stated, and its source. Empty when nothing was replaced. | 0.15 |
| elsewhere | The subject's current beliefs under other predicates holding the same value, with when each was stated, so a contradiction arrives with the answer. | 0.15 |
| evidence, source_text | With `with_sources`: the excerpt a text-derived belief was drawn from, and the whole text. Evidence is an exact substring of the source. | 0.15 |

Text-mode recall returns sentences; the same information appears as `before` (what the sentence replaced) and `elsewhere`.

A remember answer carries `replaced` for what the write superseded, `related` for the subject's current beliefs under other predicates that may now conflict, and `already_known` when the write changed nothing.

## 5. Registry contract

- The registry is a closed set per namespace unless the server was started open. An unregistered predicate is refused, never invented. The `note` predicate takes anything that fits nothing else, so nothing has to be dropped.
- Each predicate declares cardinality (single or multi), a value type, optional exclusions (stating a value under this predicate replaces the same value under the excluded ones) and whether it is historical (never replaced).
- Registry changes are events in the same log: define, redefine, merge, split. A read as of an earlier instant uses the registry as it was then.
- Open mode (`-open`, and text mode unless `-predicates` is given) lets a write define a predicate by first use. The definition is still a logged event.

## 6. Resume API, version 1

Separate from the memory verbs, and gated: it needs a backend with leases (section 7). A client checks `agent_enabled` before relying on it.

| Tool | Required arguments | Read-only |
| --- | --- | --- |
| agent_resume | agent_id | no |
| agent_acquire | agent_id | no |
| agent_release | agent_id | no |
| update_working_state | agent_id | no |
| agent_milestone | agent_id, name | no |
| record_turn | agent_id, role, content | no |
| recent_turns | agent_id | yes |
| store_output | agent_id, content | no |
| fetch_output | agent_id, ref | yes |
| set_pointer | agent_id, name, type, fields | no |
| remove_pointer | agent_id, name | no |
| list_pointers | agent_id | yes |
| working_state_history | agent_id | yes |

Semantics: `agent_resume` takes the agent's lease and rebuilds its context (working state, pointers, relevant memories, recent turns) within a token budget. Writes fail while the lease is not held. The lease epoch is a fencing token: a process that lost its lease cannot overwrite the holder's writes. `agent_id` is 1 to 128 characters from letters, digits, `.`, `_`, `-`. In Kubernetes mode the server fixes `agent_id` to the pod's sandbox name, so a sandbox can only resume itself.

## 7. Backend capability matrix

Recall runs on a vector store that implements a small interface. The core needs Put, List and Search. Each further capability lights up one part of the API. Pages describing a backend state its column of this table.

| Capability | What it enables | polign_db | Qdrant | Postgres + pgvector (planned) |
| --- | --- | --- | --- | --- |
| Core (Put, List, Search) | remember, recall, forget, memory_history, as_of, replaced, elsewhere, registry, audit export | yes | yes | yes |
| Get by id | evidence links, explain | yes | yes | yes |
| Lexical search | hybrid ranking (vector + BM25), lexical-only servers | yes | no | yes |
| Watermark | materialized (cached) reads; without it every read refolds the log | yes | no | yes |
| Leases | the Resume API | yes | no | yes |
| Front door (`recall serve` key store) | namespace-scoped keys, Kubernetes token exchange. Needs Get only; keys are write-once with permanent tombstones, so no compare-and-swap is required. | yes (0.16) | yes (0.16) | yes |
| Cost | bytes per idle namespace, cost per thousand cold tenants, scale to zero, warm RSS | measured on polign.com | from vendor sizing guidance | measured when the backend ships |

## 8. Migration between backends

The event log is portable. Copying a namespace's events from one backend to another is a supported, idempotent operation: event IDs are derived from content, so importing the same event twice stores it once.

Export format (one JSON object per line, oldest first):

| Field | Meaning | Status |
| --- | --- | --- |
| id, kind, subject, predicate, value, confidence, source, retraction, observed_at | The event, as `memory_history` returns it. | 0.15 (`ExportEvents`) |
| agent, session | Identity fields, when present. | 0.16 |
| embedding_model | Identifier of the model that produced the vector, as the exporting server was configured. | 0.16 |
| embedding | The vector. Optional. | 0.16 |

Import rules: a server whose embedding model differs from `embedding_model` recomputes vectors and never stores a mixed collection. Registry events travel in the same stream and are imported first. An audit bundle (`ExportAudit`) is the complete form, with registry, fold rules and as-of; it refuses partial exports.

## 9. Transports

| Transport | Status | Notes |
| --- | --- | --- |
| MCP over stdio (`recall mcp`) | 0.15 | Authoritative tool definitions. |
| Go (`github.com/Polign/recall`) | 0.15 | `Client.Remember`, `Recall`, `Forget`, `History`, `Explain`, `Ask`, `Resume`, `ExportEvents`, `ExportAudit`. |
| Python (`polign-recall`) | 0.15 | `Client.remember`, `recall`, `forget`, `history`, `predicates`, `ask`, `resume`. Speaks the MCP protocol to a `recall` process. |
| MCP over streamable HTTP (`recall serve`, `/mcp`) | 0.16 | Same tools, same dispatch, behind a Recall key. |
| HTTP (`recall serve`, `/v1/remember`, `/v1/recall`, `/v1/forget`, `/v1/history`, `/v1/predicates`, `/v1/explain`) | 0.16 | JSON bodies with the tool arguments; JSON answers with the tool results. |
| Python over HTTP | 0.16 | `Client(url=..., api_key=...)` or `Client.from_env()`. Same decoders. |

## 10. Credentials and environment

A client needs a URL and one credential. Nothing else is configuration: the server resolves the collection from the credential's namespace, and holds the backend credentials, the embedding model and the extraction model itself.

| Variable | Meaning | Status |
| --- | --- | --- |
| RECALL_URL | The `recall serve` endpoint. | 0.16 |
| RECALL_API_KEY | A Recall key, `rcl_<id>_<secret>`. | 0.16 |
| RECALL_TOKEN_FILE | Path to a projected Kubernetes service account token with audience `recall`, exchanged for a key at `POST /v1/auth/kubernetes` (body `{"token": ...}`) and renewed at `POST /v1/keys/renew`. The server derives the tenant from the service account's namespace or a label on it, and the agent and session from the pod the token is bound to. | 0.16 |
| RECALL_AGENT, RECALL_SESSION | Informational in Kubernetes mode (the server derives them); authoritative otherwise. | 0.16 |
| POLIGN_URL, POLIGN_API_KEY, POLIGN_COLLECTION, POLIGN_PREDICATES, POLIGN_EMBED_URL, POLIGN_EXTRACT_MODEL | Direct-to-backend configuration for `recall mcp` without a server. Kept through 0.16 as aliases. | 0.15 |

Keys: `rcl_<id>_<secret>`. The store holds the SHA-256 of the key, the namespace, the optional agent and session, and an optional expiry, as write-once records in a control collection beside the memory. Renewal rewrites the expiry. Revocation writes a tombstone that is never overwritten and that every check reads, so a late renewal cannot resurrect a revoked key. A revoked or expired key is refused within the server's auth cache TTL (`-auth-cache-ttl`, default 30 seconds), which the server prints at start. Kubernetes-mode keys default to a 15 minute TTL and are never renewed: `POST /v1/keys/renew` answers `409 renew_with_token`, and the workload exchanges its projected token again, so a key copied out of a pod dies at its TTL and a deleted pod's key cannot be extended.

Key management (0.16): `recall key create | list | revoke` against the backend, or over HTTP with an admin key: `POST /v1/keys` (body: `namespace`, `agent`, `session`, `note`, `expires_in`, `admin`), `GET /v1/keys`, `DELETE /v1/keys/<id>`. Any key renews itself with `POST /v1/keys/renew`. An admin key reaches no namespace. `-bootstrap-key-file` registers one admin key at start if the store does not know it, and never rebinds a key already on record.

## 11. Errors

| Code | HTTP | Meaning |
| --- | --- | --- |
| unauthenticated | 401 | No credential, or one the server does not know. |
| namespace_mismatch | 403 | The request names a namespace, agent or session the credential does not carry. |
| unregistered_predicate | 400 | Closed registry, unknown predicate. Use `note`. |
| invalid_argument | 400 | Unknown field, wrong type, bad instant. |
| tool_error | 400 | The operation refused the request for a reason in its message. |
| forbidden | 403 | A non-admin key called key management. |
| lease_held | 409 | Another process holds the agent. Retry or `defer_lease`. |
| lease_lost | 409 | This process lost the lease; its write was refused. |
| renew_with_token | 409 | The key was minted from a workload token and is not renewed; exchange the token again. |
| unsupported | 501 | The backend lacks the capability (for example the Resume API without leases). |

MCP carries the same codes in the tool error text.
