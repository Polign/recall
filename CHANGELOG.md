# Changelog

## [Unreleased]

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

## Python client 0.3.1 - unreleased

- `ExtractionResult.unfiled` lists proposals kept as notes because no
  registered predicate fit them.

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
  MCP surface in Polign v0.6.4. That server release also contains #99's complete
  cold-history support.

The Go library's prior v0.1.0, v0.2.0, and v0.2.1 tags are preserved. Python and
the plugin have independent version lines. This release also includes the
context-aware client, public HTTP adapter, and audit bundles previously on main.
