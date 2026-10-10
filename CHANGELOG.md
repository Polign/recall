# Changelog

## Unreleased

### Added

- Open vocabulary: with `Config.Open`, a write may name any predicate, and the
  first write that names one defines it in the registry log (snake_case name,
  type from the first value, single-valued unless `RememberRequest.Cardinality`
  says multi). The registry may be empty. `Client.Vocabulary` lists what is
  defined, `RememberText` offers it to the extractor and files predicates a
  proposal coins, and proposals may carry `cardinality` and `description`.
  When two clients define one name at once, the first recorded definition
  wins.
- `Client.Redefine` records a retroactive definition that applies to a
  predicate's whole history, so a wrong cardinality is corrected for present
  and as-of answers alike.
- Fold version `recall-fold-v3` for audit bundles whose registry log uses a
  retroactive definition or a second automatic definition of one name. Other
  bundles are still written as `recall-fold-v2`, with unchanged checksums.

## [0.13.0] - 2026-10-09

### Added

- A Qdrant backend, `github.com/Polign/recall/backend/qdrant`, over Qdrant's REST API
  with no new module dependencies. Event IDs are stored as UUIDv5 point IDs
  with the original ID in the payload; each collection is created, with
  payload indexes on the fields Recall filters on, at its first write. It
  supports `Get` but not leases, watermarks, or text search; the README lists
  what each gap means. Agent resume works on it only with `Unleased: true`.
- Backends register by name: `recall.RegisterBackend`, `OpenBackend`,
  `LookupBackend` and `Backends`, in the style of `database/sql` drivers. The
  `polign` and `qdrant` packages register themselves when imported, each with
  its default URL and the environment variables it reads.
- `recall -backend <name>` (or `RECALL_BACKEND`) serves memory from any
  registered backend; `-backend qdrant` reads `QDRANT_URL` and
  `QDRANT_API_KEY`. `-agent` is refused on a backend without leases, which
  includes Qdrant.
- `github.com/Polign/recall/backendtest`, a conformance suite any backend can
  run against a live database: exact totals, a stable ordered prefix,
  filters, search, `Get`, and a full `Client` round trip.

## Python client 0.13.0 - 2026-10-09

- The bundled `recall` binary is 0.13.0, which adds `-backend qdrant`.

## [0.12.0] - 2026-10-08

### Added

- The `recall` binary: `recall mcp` serves the memory tools (and, with
  `-agent`, the agent resume tools) over MCP; `recall setup` configures a
  connection or a managed local polign-server and installs the Claude plugin;
  `recall doctor` checks a saved setup; `recall skill` prints the agent guide.
  It is the server that used to ship inside polign_db as `polign mcp
  -memory-only` and `polign recall`, moved here so a Recall change releases
  with a Recall tag alone. It reaches polign_db only through its HTTP API.
- Release archives for Linux, macOS and Windows on amd64 and arm64, with SBOMs
  and a Sigstore-signed checksum manifest, and `brew install polign/tap/recall`.
- One `vX.Y.Z` tag now releases the Go module, the binary and the Python
  package together. The separate `python-vX.Y.Z` tags are retired.
- The `recall` tool takes `observed_after` and `observed_before`, and every
  belief it returns carries `days_ago`, as polign_db 0.13.0's tool did.
- `recall mcp` without `-url` or `POLIGN_URL` serves the connection `recall
  setup` saved, and without one connects to `http://localhost:23000`. An
  explicit `-config-dir` always means the saved connection.

## Python client 0.12.0 - 2026-10-08

- `polign-recall` ships as one wheel per platform with the `recall` binary
  inside, installed next to `python`, plus a pure wheel for every other
  platform. The client runs that `recall`, then one on `PATH`, and without
  either falls back to `polign mcp -memory-only` from polign_db 0.13 or
  earlier. `polign_db` stays a dependency for `polign-server`, which a managed
  local store runs.
- Published from the `vX.Y.Z` release workflow (release.yml) instead of
  python-publish.yml.
- `recall(observed_after=, observed_before=)` keeps only beliefs stated in
  that span.
- `Belief.days_ago`: whole days between when a belief was stated and `as_of`
  (or today). `None` from an older server that does not send it.

## [0.11.0] - 2026-10-08

### Added

- A search that names a time looks there first. "What did I plant two weeks
  ago", "who gave me jewelry last Saturday" and "what did I watch in January"
  now search the span the phrase points at, read relative to `AsOf`, before
  searching everywhere. Matches from that span take the leading ranks and
  nothing is dropped. On the user-fact questions of LongMemEval_S this took
  accuracy from 77.5% to 80.5%, and temporal questions from 68.9% to 76.7%,
  with no change in latency. `Query.NoTimeHint` turns it off.
- `Query.ObservedAfter` and `Query.ObservedBefore` keep only beliefs stated in
  a span, both inclusive.

## Python client 0.10.0 - 2026-10-05

- `Belief.replaced`, a tuple of `PriorValue`: what each belief replaced.
- Requires `polign_db` 0.12.0 or later, the first whose `recall` tool returns
  `replaced`.

## [0.10.0] - 2026-10-05

### Added

- Reads deliver corrections. Every `Belief` now carries `Replaced`, the values
  it displaced when it was asserted, each with its source, the time it was
  stated, and its event id. An agent that asks what is believed gets the
  current value and the correction behind it in one call, without a separate
  `History` read. It goes back one step, a multi-valued predicate never
  replaces anything, and restating the current value keeps the correction it
  made. The MCP `recall` tool returns it as `replaced`.

### Compatibility

- polign-recall 0.5.0 and earlier fail on a field they do not know, so they
  cannot read beliefs from a server built on this release. 0.6.0 and later
  ignore it.

## Python client 0.9.0 - 2026-10-04

The Python client now shares the Go library's version number.

- `remember(text=...)` no longer needs `statements`. Without them, the server's
  extraction model proposes the statements in the text. Statements your agent
  passes are still used as given, and the model is not called.
- `Client(extract_model="provider:model")` names that model, for example
  `"anthropic:claude-opus-5-5"`, `"openai:<model>"`, or `"ollama:<model>"`.
  `POLIGN_EXTRACT_MODEL` in the environment does the same.
- Requires `polign_db` 0.11.0 or later, the first whose `polign mcp` takes
  `-extract-model`.

## [0.9.0] - 2026-10-04

### Added

- Extraction by a language model. The new `model` package is an `Extractor`
  that proposes the statements in a text, so `RememberText` can be given text
  alone. `model.Parse("anthropic:claude-opus-5-5")` and `model.NewExtractor`
  build it. Anthropic models are called through the Anthropic Go SDK; OpenAI,
  Ollama, and other endpoints that speak the OpenAI chat completions API are
  called over HTTP. The model can name only registered predicates and must
  quote its evidence from the text; the registry and the fold decide what is
  stored, as for any other extractor.
- `Registry.AdmitProposals` keeps the proposals `RememberText` would accept and
  drops the rest, reading numbers and booleans written as strings. An extractor
  whose model cannot be asked to correct a refused batch uses it, so one bad
  proposal does not cost the others.
- `ObservedAt(ctx)` gives an extractor the time `RememberTextAt` was given, so
  relative dates such as "last week" resolve against when the text was said.
- Starter registries. `StarterRegistry(name)` returns a ready-made registry for
  `coding`, `support`, `sales`, or `voice` agents, and `StarterNames()` lists
  them. `DefaultRegistry()` is the coding one, unchanged. The same registries
  are in `registries/` as JSON.
- Three value types. `enum` accepts one of the predicate's `allowed` values and
  names them when it refuses another. `date` accepts a day or an RFC3339 time
  and can be filtered with `Query.ValueAfter` and `Query.ValueBefore`. `ref`
  holds the name of another subject.
- Reads across refs. `Query.RefersTo` returns the current beliefs whose ref
  value is a given subject. `Query.FollowRefs` adds what is believed about each
  subject a returned ref names, one step, with `Belief.Via` naming the ref.
- Registry changes are recorded in the log. `Client.SyncRegistry` writes one
  event for each predicate whose definition changed, and `Client.RegistryLog`
  reads them back. A cardinality change applies from the moment it is recorded,
  so `AsOf` reads from before it answer as they did then.
- Renames. A predicate lists its former names in `aliases`. Events under an old
  name fold with the new one, and a write to an old name is stored under the
  new one.
- `ErrRegistryMismatch`. Once a store has a recorded registry, a client whose
  registry disagrees on a predicate's cardinality or value type, or still uses
  a renamed name, is refused until it is corrected or calls `SyncRegistry`.
- Audit bundles carry the recorded registry changes in `registry_log` and
  replay under them. Bundles without one verify and replay as before.

### Changed

- `Predicate` has two new fields, `Allowed` and `Aliases`. Code that builds a
  `Predicate` without field names, or compares two with `==`, no longer
  compiles. Use field names and `reflect.DeepEqual`.
- Each client reads the store's registry events once, then at most every 30
  seconds. This is one extra listing per client, not per call.
- A stored value is checked against its predicate's stored type (text, number,
  or boolean) when read, no longer against the full rule for new writes. This
  keeps a value readable after its enum drops it.

## Python client 0.6.0 - 2026-09-30

- `recall(with_sources=True)` returns each belief's `evidence`, `evidence_id`
  and `source_text`, the excerpt and the whole text it was drawn from;
  `remember(text=...)` returns the episode note as `episode`.
- Beliefs and events ignore fields they do not know, so a newer server cannot
  break decoding.
- Requires `polign_db` 0.10.0 or later, the first whose `recall` tool accepts
  `with_sources`.

## [0.8.0] - 2026-09-30

### Added

- Statements remembered from text keep what they were drawn from.
  `RememberText` first keeps the whole text as a note (the episode, returned as
  `ExtractionResult.Episode`), then writes each statement with its quoted
  excerpt as `Evidence` and the episode as `EvidenceID`. Beliefs carry both.
  The excerpt is part of the statement's searchable text, so a fact is also
  found by the words it came from.
- `Client.Events` reads events by id, through the optional `GetBackend`
  (implemented by the Polign backend with its batch read), so a caller can
  follow a belief's `EvidenceID` to the whole text.
- Audit bundles record events as `recall-event-v2` and checksum them with
  `DigestV3`, which also covers `Evidence` and `EvidenceID`. `recall-event-v1`
  bundles and `sha256:v2:` digests still verify.

### Changed

- `RememberText` always keeps the text as a note about `DefaultSubject`, even
  when every proposal is filed. Before, the text was kept only when nothing
  could be filed. `Results` keep their order and contents.

## Python client 0.5.0 - 2026-09-29

- `remember` takes `observed_at` (an RFC3339 string or an aware `datetime`)
  to record statements made earlier, such as an imported conversation.
- Requires `polign_db` 0.9.0 or later, the first whose `remember` tool
  accepts `observed_at` and whose recall ranks by the query with BM25.

## [0.7.0] - 2026-09-29

### Added

- Semantic recall uses the backend's lexical (BM25) search when it has one,
  through the optional `TextSearchBackend` (or `TextSearcher` for `NewStore`).
  The Polign backend implements it with Polign's segment text index. With the
  built-in lexical embedder, the text ranking leads and vector hits it lacks
  follow; with a model embedder the two rankings are fused evenly. On a
  LongMemEval-S subset, recall over stored conversation turns found an answer
  session in its top ten for 98% of questions, up from 62% with the hashed
  vectors alone.
- `RememberRequest.ObservedAt` and `Client.RememberTextAt` record statements
  made earlier, such as an imported conversation. Observation time decides
  what holds, so statements written out of order still supersede in the order
  they were made, and `AsOf` answers for their period. Times more than a
  minute in the future are refused.
- Every event stores its searchable text under the `text` metadata key
  (`TextField`). Events written before this release have none, so only the
  vector search reaches them.

### Fixed

- Semantic recall ranks beliefs by the query. It used to rank subject and
  predicate pairs, then return each pair's values in the order they were
  written, so a query that reached a multi-valued pair came back with that
  pair's oldest values whatever it asked. Every note shares one pair, which
  made `recall(query=...)` over notes return the same oldest notes for any
  question. A multi-valued pair now returns only the values whose own events
  the search matched, best first. A single-valued pair still answers with its
  current value, ranked by its best-matching event. Naming both subject and
  predicate still returns the whole pair.

## [0.6.1] - 2026-09-26

### Added

- `Turn.Brief`, a shorter form of a turn for the resume briefing. A harness
  that records a tool call can keep the call whole in `Content` and give the
  briefing a form with long arguments elided.

### Changed

- The resume briefing's header is one sentence and no longer tells the agent
  to update its working state. Every model call after a resume carries the
  header, and that standing instruction led agents resumed early in a run to
  record a note after every action, doubling the cost of those runs. When to
  take notes is left to the tool descriptions and the harness's own prompt.

## [0.6.0] - 2026-09-26

### Added

- `Client.Resume` resumes an agent from its own records instead of a
  snapshot. It takes the agent's lease and returns an `Agent` whose
  `Resumed()` context holds the working state, pointers, recent turns,
  relevant memories and stored-output references within a token budget, and
  a `Briefing` to hand the model.
- `Agent` writes those records: `UpdateWorkingState`, `Milestone`,
  `RecordTurn`, `StoreOutput`, `SetPointer` and `RemovePointer`, with
  `WorkingStateHistory`, `RecentTurns`, `FetchOutput` and `Pointers` to read
  them back. Records go in a collection of their own.
- `LeaseBackend`, with `ErrLeaseHeld`, `ErrLeaseLost` and
  `ErrLeaseUnsupported`, keeps one process per agent. The Polign adapter
  implements it over the server's lease API.
- `polign.StatusError.NotFound` reports a 404.
- `Turn.MessageID` keeps a harness's own id for a message, so a harness that
  restores its message history after a resume can tell recorded messages
  from new ones.
- `ResumeRequest.Deferred` returns the context without taking the lease, so
  a process replacing a crashed one can start at once. `Agent.AcquireLease`
  takes the lease later and picks up anything written in between; writes
  before it fail with `ErrLeaseNotHeld`.

Leases need Polign 0.8.0 or later; an older server answers
`ErrLeaseUnsupported`. Everything else works with the servers 0.5.0 did.

## [0.5.0] - 2026-09-25

### Added

- Statements that fit no predicate are kept instead of dropped. Every registry
  now accepts `note`, a multi-valued text predicate, even when the registry
  does not list it. A registry that defines `note` itself must keep it `multi`
  and `string`.
- `RememberText` keeps the whole text as a note, once per subject, when a
  proposal names an unregistered predicate or when there are no proposals (the
  subject is then `user`). Those proposals come back in the new `Unfiled` field.
  Before, an unregistered predicate rejected the whole batch.
- `Client.Promote` files a note under a typed predicate: it writes the typed
  memory, then withdraws the note.
- `recall-audit -notes` prints only the notes in a bundle.

### Changed

- Recall returns typed memories ahead of notes on the same page.
- The error for an unregistered predicate in typed `remember` now says to use
  `note` when nothing fits.
- The Go implementation now lives in `internal/engine`, with public aliases
  preserving the supported API and method sets. Reflection reports the
  internal package as the defining path of those types.

## Python client 0.4.1 - 2026-09-26

- `record_turn(brief=...)` keeps a shorter form of a turn for the resume
  briefing, and `Turn.brief` reads it back.

## Python client 0.4.0 - 2026-09-26

- Requires `polign_db` 0.8.0 or later, the first with `polign mcp -agent`.
- `Client(agent=True)` runs `polign mcp -agent` and adds `resume`, which
  takes an agent's lease and returns a `ResumedAgent` with its rebuilt
  `ResumeContext`. The agent writes its working state, turns, outputs and
  pointers through it, and releases the lease on `release()`, on leaving a
  `with` block, or when the client closes.
- A resume refused because another process holds the agent raises
  `RecallError` with code `"lease_held"`; a write after another process took
  the agent over raises code `"lease_lost"`.
- `resume(defer_lease=True)` returns the context without waiting for a
  crashed process's lease. `ResumedAgent.acquire()` takes it later (False
  while it is still held), and writes before that raise code
  `"lease_not_held"`. `ResumeContext.lease_held` and
  `ResumedAgent.lease_held` say which state the agent is in.
- `record_turn(message_id=...)` keeps the harness's id for a message with
  the turn, and `Turn.message_id` reads it back.
- New types: `ResumeContext`, `ResumedAgent`, `WorkingState`, `Turn`,
  `Output`, `OutputRef`, `Pointer`. `Client.agent_enabled` says whether a
  client can resume.
- Closing a client opened with `agent=True` ends the MCP session by closing
  its input first, so the server hands leases over instead of leaving them to
  expire.

## Python client 0.3.1 - 2026-09-25

- `ExtractionResult.unfiled` lists proposals kept as notes because no
  registered predicate fit them.
- Requires `polign_db>=0.7.2` on supported wheel platforms so an install
  includes the matching note-aware CLI.

## Python client 0.3.0 - 2026-09-20

- `pip install polign-recall` is now the whole install. It depends on the new
  `polign_db` package, which ships the `polign` CLI and `polign-server` as
  platform wheels (Linux, macOS and Windows on x86_64 and arm64). The client
  runs that binary, then falls back to `polign` on `PATH`.
- `Client(local_dir=...)` keeps the database on the same machine: it starts a
  background `polign-server` for the directory through `polign recall setup
  -local`, shares it between processes, and ignores any `POLIGN_URL` or
  `POLIGN_API_KEY` in the environment. Linux and macOS.

## [0.4.0] - 2026-09-13

Correctness release from a full review of 0.3.0. **The fold version is now
`recall-fold-v2`**: a retraction sharing an instant with the assertion it
withdraws now sorts after it, where v1 ordered that tie on the event id. The
same log can fold differently under the two, so `Replay` refuses a v1 bundle
rather than answering from it, and now names which version moved.

### Fixed

- Materialized beliefs no longer fail a read they cannot verify. The revision
  covers the whole collection, so a write to an unrelated pair moved it and
  aborted a correct read with an incomplete-history error. Every problem reading
  the revision, including an unsettled one, an unreadable one and an empty one,
  now degrades to the uncached fold, which is always correct.
- A write invalidates its own pair, so a client reads its own writes back even
  when the backend revision lags or repeats. Cached entries also expire after 30
  seconds, bounding the staleness a revision that under-reports can cause.
- The Polign adapter treats 501, and a 200 carrying no watermark, as "endpoint
  not supported" rather than failing the read.
- `Forget` withdraws a belief whose event is stamped later than the writer's own
  clock. It previously folded at that clock, saw nothing held, reported success
  and wrote nothing, so the belief reappeared as the clock caught up. `Remember`
  deliberately keeps the writer's instant, because observation time decides what
  is believed, but it no longer shares an instant with an existing event.
- Broad recall no longer returns one belief twice when the log holds a subject
  that is not normalized. Audit replay groups on the same normalized pair, so it
  reproduces what the store answers.
- `NewStore` records its registry validation and every operation returns it. An
  invalid cardinality previously folded a multi-valued predicate as single and
  surfaced much later as a malformed audit.
- The cold-collection fallback is back in bounded exports, so a failover node
  reading from S3, GCS or Azure degrades to filtered search instead of failing.
  It is deliberately absent from complete-history reads, which exist to refuse a
  truncated log.
- Python client: a write to a server that has stopped reading is now bounded. It
  could block forever while holding the lock `close()` needs, so no other thread
  could recover the client. Server notifications no longer end the session, and
  a missing binary raises `RecallError` as documented rather than `OSError`.

## [0.3.0] - 2026-09-12

- Default registry with 15 predicates for preferences, identity, and project facts.
- `RememberText` proposal pipeline: the calling agent supplies extracted facts
  and exact evidence quotes; the registry validates the entire batch before the
  fold resolves each write. Partial storage failures report completed results.
- Built-in, versioned lexical feature hashing for memory without an embedding
  service or a second model runtime. Exact typed memory requires no model.
- Optional bounded materialized belief view keyed by a backend-visible log
  watermark, with late-arrival, overwrite, and future-event invalidation.
- Polign HTTP watermark adapter; older servers fall back to uncached reads.
- Python client `polign-recall` v0.1.0 over the existing MCP stdio transport,
  with typed results, timeout handling, and no automatic write retries.
- Claude Code Recall plugin v0.1.0 in `Polign/polign`, using the memory-only
  MCP surface in Polign v0.6.4. That server release also contains complete
  cold-history support.

The Go library's prior v0.1.0, v0.2.0, and v0.2.1 tags are preserved. Python and
the plugin have independent version lines. This release also includes the
context-aware client, public HTTP adapter, and audit bundles previously on main.
