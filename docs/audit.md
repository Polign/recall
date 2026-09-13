# Reproducible audit bundles

`ExportAudit` captures complete event histories, the predicate registry used to
interpret them, an explicit as-of time, and the event and fold versions. A bundle
can reproduce exact beliefs without a database or embedder.

```go
bundle, err := client.ExportAudit(ctx, recall.AuditRequest{
    Scope: recall.AuditScope{Subject: "user"},
    AsOf: tuesday,
})
if err != nil { return err }
if err := json.NewEncoder(file).Encode(bundle); err != nil { return err }

// Elsewhere, after decoding the bundle:
beliefs, err := bundle.Replay() // verifies before folding
```

An empty scope selects the entire collection visible to the backend credential.
A zero `AsOf` captures the writer's current time once before reading. Exports
retain events after that instant, but replay excludes them. Results are ordered
by subject and predicate, then by the fold's value order, without semantic
ranking or a query result limit.

Export requires a complete listing within `MaxExport` (10,000 events); it has no
option to truncate. Missing registry entries, invalid typed events, duplicate
IDs, incomplete reads, and unsupported versions fail explicitly. Registry
configuration is copied into the bundle. Verification does not mutate it.

## Offline verifier

From this source checkout:

```sh
go run ./cmd/recall-audit < testdata/audit-v1.json
go run ./cmd/recall-audit -digest "$TRUSTED_DIGEST" < bundle.json
```

The command reads at most 64 MiB, rejects unknown JSON fields and trailing JSON,
verifies the bundle, and prints replayed beliefs. Invalid input produces an
error and nonzero exit status. It uses no database or embedding service. These
APIs are available from source pending their feature release.

A checksum detects changes relative to a trusted digest. Someone who can
replace both the events and the digest can construct a new valid bundle; this
format does not sign data or authenticate its author. The optional `-digest`
checks an expected bundle checksum obtained through a separate trusted channel.

## What replay establishes

Replay answers what the supplied events imply under the supplied registry and
fold rules at the recorded time. It cannot prove that the exporter included
every event. Complete export depends on the backend's consistency guarantees;
pagination checks do not establish a snapshot across writers.

A bundle contains the exporter's current registry, not a history of registry
changes. Changing a predicate from single to multi changes how the same events
fold; the registry is therefore included in the checksum. Preserve bundles when
you need to retain prior interpretations. A later export also cannot reconstruct
which events an earlier reader had seen. A backdated event accepted today can
change today's reconstruction of yesterday's beliefs.

## Version and checksum contract

The first bundle declares `recall-audit-v1`, `recall-event-v1`, and
`recall-fold-v1`. These identify the envelope, the existing typed Event fields,
and the current fold semantics (including targeted single-value retractions).
Future incompatible semantics require a new version and an explicit verifier
implementation; unknown versions are refused. Existing stored metadata is not
rewritten or assigned a new event ID by this feature.

`Digest(events)` retains the legacy `sha256:<hex>` format and case-insensitive
string identity. It intentionally does not detect `Neovim` changing to `neovim`.
`DigestV2(events)` returns `sha256:v2:<hex>` and covers exact typed event fields.
`VerifyDigest(events, expected)` dispatches between the two formats, preserving
legacy verification. The golden fixtures in `testdata` were computed separately
using Python SHA-256 and IEEE-754 packing.

Both new hashes use a stream of length-prefixed UTF-8 fields: decimal byte
length, a colon, then that many bytes, without separators. SHA-256 produces
lowercase hexadecimal output. Event order is canonicalized by UTC observation
time, then event ID in ascending byte order. Duplicate IDs are rejected even
when their content is identical. Invalid UTF-8 and non-finite numbers are
rejected so that JSON serialization cannot silently change checksummed values.

The event digest stream starts with `recall-events-digest-v2` and the decimal
event count. Each sorted event contributes these fields:

| Order | Field | Encoding |
| --- | --- | --- |
| 1 to 4 | ID, kind, subject, predicate | Exact strings |
| 5 | Value type | `null`, `string`, `number`, or `boolean` |
| 6 | Value | Empty for null; exact string; 16 lowercase hex digits of IEEE-754 binary64 bits for a number; `true` or `false` for a boolean |
| 7 | Confidence | Same 16-digit IEEE-754 encoding |
| 8 | Source | Exact string |
| 9 | Retraction | `true` or `false` |
| 10 | Observed at | UTC RFC3339, exactly nine fractional digits and `Z` |

This distinguishes positive and negative zero and preserves string case and
whitespace. It hashes logical Event fields, not JSON source bytes: equivalent
time zones and JSON property orders produce the same checksum. Derived
`observed_ms`, vectors, and unrelated backend metadata are not Event fields.

The bundle checksum stream contains, in order: bundle version, event version,
fold version, scope subject, scope predicate, as-of time in the same canonical
UTC format, the full `sha256:v2:` event digest, and the decimal registry entry
count. Registry entries follow sorted by name, contributing name, cardinality,
value type, and description as exact strings. An omitted/default value type
(`""`) remains distinct from an explicit `"string"`. The checksum is prefixed
`sha256:audit-v1:`. The bundle's own Digest field is excluded.

## Ordering, concurrent writers, and retries

Fold orders events by the writer's `ObservedAt`, breaking ties by ID. It does
not use database acceptance order. A writer with a slow clock can append an
event that sorts before a correction already stored; a fast clock can create
an event that stays invisible until a reader's as-of ceiling reaches it.

Remember and Forget read history and then append at most one event. Those two
operations are not a transaction or compare-and-append. A returned Remember
result describes the assertion written or found by that call, not a guarantee
that it wins after other writers complete. Targeted retractions only withdraw
the matching current value, while blanket retractions clear all values held at
their position in the ordered log. A Forget that sees nothing held is a no-op;
it cannot withdraw a concurrent assertion it never saw.

After a lost write acknowledgement, the event may already be committed. A
Remember retry does not write again if the same belief still holds. If a
correction or retraction intervenes, the retry can append a new event. A Forget
retry after a reassertion can likewise withdraw that new belief. Neither Client
nor the HTTP adapter promises exactly-once requests or automatically retries
writes.

Event IDs use subject, predicate, case-insensitive value identity, retraction,
and observation time. They omit source, confidence, and kind. Two writers
producing the same identity at the same instant can upsert the same ID; the
backend decides which record survives. The ID is not a checksum of every field
and is not a caller-supplied idempotency key. Applications needing serializable
updates or exactly-once request handling need coordination and an appropriate
backend primitive; a process-local lock cannot provide that across writers.

Regression tests exercise skewed clocks, equal-time tie ordering, simultaneous
Remember/Forget after the same history read, and retries after a committed write
loses its acknowledgement. They define these existing guarantees without
changing write ordering or adding a new concurrency protocol.
