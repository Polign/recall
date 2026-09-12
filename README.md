# Recall

Typed, durable agent memory. An append-only log of what an agent was told,
and a fold that turns it into what the agent believes, now or at any past
instant.

Recall is the memory layer for [Polign](https://polign.com), but it depends on
no particular database: it needs a `VectorDB` you provide, so it runs against
polign_db, a test fake, or anything else with the same three operations.

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

Recall does not embed anything. You pass an `embed func(string) []float32`, and
whatever produced the stored vectors has to produce the query vectors too.

## License

Apache 2.0. See [LICENSE](LICENSE).
