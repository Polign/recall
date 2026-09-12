# Recall

Typed, durable agent memory. An append-only log of what an agent was told,
and a fold that turns it into what the agent believes, now or at any past
instant.

Recall is the memory layer for [Polign](https://polign.com), but it depends on
no particular database: provide a `Backend` (or legacy `VectorDB`) with three
operations, or use the included Polign HTTP adapter.

```sh
go get github.com/Polign/recall
```

## What it does differently

Most "AI memory" puts text in a vector store and hopes similarity search finds
it again. Recall treats memory as what it actually is: state, with rules.

**Every memory is a typed record.** Predicates come from a closed registry that
declares each one's cardinality and value type. An agent cannot invent
`editor_preference` alongside `prefers_editor` and split one fact across two
names that never meet.

```
kind:        preference
subject:     user
predicate:   prefers_editor
value:       neovim
confidence:  1.0
source:      user_stated
observed_at: 2026-09-12T04:31:07.000000000Z
```

**Supersession is deterministic.** A single-valued predicate holds one value: a
newer statement replaces an older one. A multi-valued predicate accumulates.
Which happens is decided by the registry, never by model judgment.

**Beliefs are derived, never mutated.** The log is the truth, and `Fold` is the
only thing that reads it. Nothing is rewritten when a belief changes, so no
crash between two writes can leave an agent believing two contradictory things
at once.

**History is queryable.** Forgetting is a retraction event, not a deletion, so
asking what the agent believed last Tuesday still works after Wednesday's
correction. With a value supplied, a retraction withdraws only that value,
including for single-valued predicates: vim → emacs → retract vim still leaves
emacs believed. With no value it clears the pair. Retracting the current value
never revives one that was superseded.

## Using it

```go
registry, err := recall.LoadRegistry(predicatesJSON)
store := recall.NewStore(db, "memories", registry, embed)

store.Remember("preference", "user", "prefers_editor", "neovim", 1.0, "user_stated")

// What is believed now.
beliefs, err := store.Recall(recall.Query{Subject: "user", Predicate: "prefers_editor"})

// What was believed on Tuesday. Same call, one more field.
beliefs, err = store.Recall(recall.Query{
    Subject:   "user",
    Predicate: "prefers_editor",
    AsOf:      tuesday,
})

// Semantic recall, folded like every other read, so a superseded statement
// ranking top of the search still cannot come back as a live belief.
beliefs, err = store.Recall(recall.Query{Text: "which editor do I use"})

// Every event behind a belief, oldest first: the audit trail.
events, err := store.History("user", "prefers_editor")
```

`Remember` writes at most one record, and only after folding what is already
there. Restating a belief that already holds writes nothing at all.

## Context-aware client

`NewClient(Config)` adds context-aware operations while `NewStore` and its
existing method signatures remain available. A `Backend` implements Put, List,
and Search with a `context.Context`; an `Embedder` returns both vectors and an
error. `EmbedFunc` adapts a function. Errors preserve `errors.Is`/`errors.As`, and
failed or cancelled embeddings cannot fall through to a write or vector search.

```go
client, err := recall.NewClient(recall.Config{
    Backend: backend,
    Collection: "memories",
    Registry: registry,
    Embedder: recall.EmbedFunc(embedWithContext),
})
if err != nil { return err }

result, err := client.Remember(ctx, recall.RememberRequest{
    Subject: "user", Predicate: "prefers_editor", Value: "vim",
})
beliefs, err := client.Recall(ctx, recall.Query{Subject: "user"})
withdrawn, err := client.Forget(ctx, recall.ForgetRequest{
    Subject: "user", Predicate: "prefers_editor", Value: "vim",
})
```

Typed values are strings, `float64` numbers, or booleans. `ForgetRequest` requires
exactly one value or `All: true`; false and zero are real values, and an omitted
value does not clear the pair. Remember defaults to kind `fact`, source
`user_stated`, and confidence 1. Set its optional confidence pointer to record
an explicit zero. Exact reads, history, and exports work without an embedder.

Client configuration is validated and its registry is copied. Both Client and
legacy Store return a registry copy, so callers cannot mutate their configuration
through `Registry()`. Backends and embedders must support concurrent calls and
honor context cancellation. Each operation has independent request state; this
does not add cross-writer serialization or a database-wide read snapshot.

## Polign HTTP adapter

The `github.com/Polign/recall/polign` package implements `Backend` using the
standard library. It preserves typed metadata and retrieves exact listings
across the server's 1,000-row page limit.

```go
backend, err := polign.New(polign.Config{
    BaseURL: "http://localhost:8080",
    APIKey: os.Getenv("POLIGN_API_KEY"),
})
if err != nil { return err }
client, err := recall.NewClient(recall.Config{
    Backend: backend,
    Collection: "memories",
    Registry: registry,
    Embedder: recall.EmbedFunc(embedWithContext),
})
if err != nil { return err }
```

The API key is sent as a bearer token on every request; namespaced credentials
use the server's namespace isolation. Context cancellation reaches each HTTP
request, including later listing pages. Failed or inconsistent pages return an
error with no partial history. Pagination detects changing totals and repeated
or reordered IDs, but does not establish a snapshot across concurrent writes.

The default HTTP client has a 30-second timeout and does not follow redirects.
Set `HTTPClient` to supply your own timeout and redirect policy. Responses are
bounded at 64 MiB per request; increase `MaxResponseBytes` for large vectors.
HTTP failures expose `*polign.StatusError` through `errors.As`. Writes are not
automatically retried.

The [session example](examples/sessions) runs remember, correction, as-of reads,
and forgetting in separate processes, with instructions for a server restart.
It uses fixture vectors for exact reads and needs no embedding service. Cold
persistence and restart require a server containing
[polign_db #99](https://github.com/Polign/polign_db/pull/99), now on server main;
the published v0.6.3 server predates that support. This API and adapter are
available from the source checkout pending their feature release.

## Input and stored-event validation

Queries return at most `MaxRecall` (1,000) beliefs and exports read at most
`MaxExport` (10,000) events, including explicitly limited exports. Larger limits
return errors before any backend call. Zero or negative limits retain their
default behavior. Numeric values, confidence, and query ranges must be finite;
confidence must be in [0, 1], and a range minimum cannot exceed its maximum.

Store reads validate event IDs, timestamps, typed fields, and the value type of
known predicates. Corrupt histories return `ErrIncompleteHistory` wrapping
`ErrInvalidEvent`, so a malformed correction or retraction cannot revive an old
belief or authorize a write. Candidate reads and limited exports also reject
invalid events. Legacy assertions without a `retraction` field, RFC3339 timestamps,
and records without `observed_ms` remain readable. `DecodeEvent` exposes this
strict decoder; `EventFromMetadata` retains its permissive behavior for existing
callers.

## Complete histories

The `VectorDB.List` implementation must return exact matches and their total
count, paging internally if its backend caps page size. Recall checks that it
has the complete subject-and-predicate log before deriving a belief or writing
a correction or retraction. A partial listing returns `ErrIncompleteHistory`.
Approximate vector search cannot replace this listing: a missed correction or
retraction would change the answer.

Histories are bounded at `MaxHistoryEvents` (10,000 events per subject and
predicate). New events are refused at that bound; existing complete histories
remain readable. A default export likewise fails if it cannot return the
complete log within `MaxExport`. An explicit export limit requests a subset.

With Polign, use a collection that supports exact vector listings. A cold-served
collection without a complete listing index returns an error for memory
operations, including semantic recall when it resolves candidate histories.

## Broad exact queries

A query with only a subject, only a predicate, or neither keeps widening exact
candidate discovery until it fills the requested belief limit or exhausts the
matching events. Withdrawn pairs and beliefs excluded by kind, confidence, time,
or numeric range do not hide later matching pairs. Each discovered pair still
requires its complete history.

Discovery is bounded at `MaxCandidateEvents` (10,000 candidate events). If that
budget cannot fill the requested limit or prove the candidate set exhausted,
Recall returns `ErrIncompleteCandidates` with no partial answer. Narrowing to a
specific subject and predicate bypasses broad discovery. Semantic retrieval
remains approximate; its candidate budget is not an exhaustive listing.

`VectorDB.List` must extend the same ordered prefix when its limit grows and the
data is unchanged. Changing totals, reordered or overwritten prefixes, duplicate
IDs, and short pages fail explicitly. This detects inconsistent discovery reads;
it does not provide a database-wide snapshot across separate history reads.

## Audit bundles

`client.ExportAudit(ctx, request)` captures complete histories with their
registry, fold version, and fixed as-of time. `bundle.Replay()` verifies the
checksum and reproduces exact beliefs offline. The `recall-audit` command
verifies JSON bundles from stdin.

`DigestV2` detects changes to exact typed values, including string case, while
`Digest` and `VerifyDigest` preserve legacy checksum compatibility. See the
[audit format and concurrency contract](docs/audit.md) for usage, canonical
encoding, and the limits of replay and retries.

## The registry

```json
{
  "prefers_editor": {
    "cardinality": "single",
    "value_type": "string",
    "description": "the editor the user works in"
  },
  "likes": {
    "cardinality": "multi",
    "value_type": "string",
    "description": "things the user likes"
  }
}
```

`cardinality` is `single` or `multi`. `value_type` is `string`, `number`, or
`boolean`, and values are stored with that type, so a number compares
numerically in a filter instead of lexically.

`Registry.PromptTable()` renders the registry for an agent's system prompt.

## Embeddings

Recall does not embed anything itself. Pass an `Embedder` to `NewClient`, or an
`embed func(string) []float32` to legacy `NewStore`. Whatever produced the stored
vectors has to produce the query vectors too.

## License

Apache 2.0. See [LICENSE](LICENSE).
