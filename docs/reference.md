# Recall developer reference

For a first example of shared agent memory, start with the [README](../README.md).
This page covers memory records, the Go API, backend requirements, and limits.

- [Extracting facts from text](#free-text-without-a-second-model)
- [Memory records and corrections](#memory-records-and-corrections)
- [Go client](#context-aware-client) and [Polign adapter](#polign-http-adapter)
- [Validation](#input-and-stored-event-validation) and [complete histories](#complete-histories)
- [Audit bundles](#audit-bundles), [registry](#the-registry), [notes](#notes), and [embeddings](#embeddings)

## Free text, without a second model

The calling agent proposes facts from the text and sends one `remember` call:

```json
{
  "text": "I prefer neovim now.",
  "statements": [{
    "subject": "user",
    "predicate": "prefers_editor",
    "value": "neovim",
    "evidence": "I prefer neovim now."
  }]
}
```

Recall checks the typed value, subject, and verbatim evidence before writing.
An invalid proposal rejects the whole batch before any write. Storage
failures can leave a completed prefix, reported in the error.

Recall first keeps the whole text as a [note](#notes) about `user`: the
episode, returned as `episode`. Each statement is then written with its quoted
excerpt as `evidence` and the episode's event id as `evidence_id`, so what was
said stays readable however it was interpreted. The excerpt is part of the
statement's searchable text, so a fact is also found by the words it came from.
`recall` with `with_sources: true` returns `evidence`, `evidence_id`, and the
whole text as `source_text` for each belief remembered from text; without it
those fields are left out.

Nothing stated is dropped. If a proposal names a predicate that is not in the
registry, Recall also keeps the whole text as a note under that proposal's
subject instead of refusing it. The proposals that became notes come back in
`unfiled`, so you can see which predicates your registry is missing. A wrong
value for a registered predicate, such as the string `"8000"` for a number,
still rejects the batch, because the agent can correct it. Extracted facts
are marked `agent_inferred` with a default confidence of 0.8; this is a convention,
not a calibrated model probability. Validation cannot prove
that an extracted statement is true. Typed `remember` also remains available.

The default search uses local lexical word overlap, not semantic synonyms.
Set `-embed-url` with a dedicated collection to use model embeddings instead.
Never mix embedding spaces in one collection. Exact subject/predicate reads
work without semantic search.

## Go library

Recall is the memory layer for [Polign](https://polign.com) and can use any
backend implementing three operations. The library has no external dependencies.

```sh
go get github.com/Polign/recall
```

Use `recall.DefaultRegistry()` and `recall.LexicalEmbedder{}` to start without
writing a schema or running an embedding service. Add `Materialize: true` to
cache folded beliefs against a backend log watermark. Repeated exact reads
then skip unchanged history. [Materialization contract](materialization.md).

## Memory records and corrections

Each memory has a subject (who or what it describes), a predicate (the kind of
fact), and a typed value. The registry defines the allowed predicates and
whether each accepts one value or several.

```text
kind:        preference
subject:     user
predicate:   prefers_editor
value:       neovim
confidence:  1.0
source:      user_stated
observed_at: 2026-09-12T04:31:07.000000000Z
```

A single-valued predicate, such as `prefers_editor`, keeps the newest assertion.
A multi-valued predicate, such as `uses_technology`, keeps several values. Adding
one technology does not remove another; retract the old value explicitly when
it no longer applies.

Recall appends assertions and retractions to a log. `Fold` derives the current
beliefs from those events using the registry's rules. A correction preserves the
previous event, so history and past-time queries can still show it. This does
not serialize concurrent writers; see the client and audit contracts below.

Forgetting records a retraction rather than deleting data. A targeted retraction
withdraws only that value: vim → emacs → retract vim leaves emacs current.
Retracting the current value never revives one that was superseded. Clearing a
whole subject/predicate pair requires an explicit request; the client APIs below
describe how to express it.

## Using it

```go
registry := recall.DefaultRegistry()
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
```

Semantic recall returns beliefs in search order. A single-valued predicate
answers with its current value, placed where its best-matching event ranked.
A multi-valued predicate, including `note`, answers only with the values whose
own events the search reached, so a query about one note does not bring back
unrelated notes. A query that names both subject and predicate returns every
value of that pair, whatever the text.

When the backend also offers a lexical search (`TextSearchBackend`, which the
Polign backend implements), semantic recall runs it beside the vector search.
With the built-in lexical embedder the text ranking leads, followed by vector
hits it lacks; with a model embedder the two are fused by reciprocal rank.
Polign indexes text as it persists segments, so a statement written in the
last few seconds to a minute is reached only by the vector search until the
index catches up, and a collection with nothing persisted yet uses the vector
search alone.

```go
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

`RememberRequest.ObservedAt` records a statement made earlier, such as a line
from an imported conversation; zero means now. The statement is judged at its
own time: one dated before a later statement for the same subject and
predicate is kept as history, answers `AsOf` queries for its period, and does
not replace what the later one says. Remember refuses a time more than
`MaxObservationSkew` (one minute) past the writer's clock, because a statement
dated in the future would stay hidden from every query about the present.
`RememberTextAt` does the same for text mode, stamping every statement and note
it writes. Forget always records its retraction after everything it withdraws.

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
    BaseURL: "http://127.0.0.1:23000",
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

The [session example](../examples/sessions) runs remember, correction, as-of reads,
and forgetting in separate processes, with instructions for a server restart.
It uses fixture vectors for exact reads and needs no embedding service. Cold
persistence and restart require Polign v0.6.4 or later. The v0.6.3 server
predates that support.

## Resuming an agent

An agent can resume from what it wrote down instead of from a container
snapshot. `Client.Resume` takes the agent's lease, reads its records back,
and assembles the context it should start from, within a token budget. The
same call starts an agent for the first time: with nothing recorded, the
context is marked `Fresh`.

```go
agent, err := client.Resume(ctx, recall.ResumeRequest{AgentID: "coder-1", TokenBudget: 8000})
if err != nil { return err }
defer agent.Release(context.Background())

start := agent.Resumed().Briefing // hand this to the model as its starting context

agent.UpdateWorkingState(ctx, recall.WorkingState{
    Goal: "migrate billing to the v2 API",
    Plan: []string{"find call sites", "migrate", "run tests"},
    Focus: "finding call sites",
})
agent.RecordTurn(ctx, recall.Turn{Role: "assistant", Content: reply})
agent.SetPointer(ctx, recall.Pointer{Name: "wip", Type: recall.PointerGitRef,
    Fields: map[string]string{"repo": "github.com/acme/billing", "branch": "agent/coder-1/wip", "sha": sha}})
agent.Milestone(ctx, "call sites found", "42 call sites listed")
```

An agent keeps four kinds of record:

- **Working state**: one note to its next instance (goal, plan, progress,
  focus, decisions, open questions). Each update supersedes it, and
  `WorkingStateHistory` reads the earlier versions.
- **Pointers** to where its work lives: `git_ref`, `object`, `env`,
  `external` or `process`. Setting a pointer with an existing name replaces
  it. The work stays in the system that already keeps it.
- **Turns**, verbatim. A turn larger than `OutputThreshold` (2,000 estimated
  tokens by default) is stored as an output, and the turn keeps its opening
  and a reference.
- **Outputs**: large tool results, kept whole up to 1 MiB and read back with
  `FetchOutput`.

`ResumeContext` holds the working state and pointers whole, then fills what
is left of the budget with recent turns (newest first), memories that match
the agent's focus and open questions, and references to stored outputs.
`Omitted` counts what did not fit. `Briefing` renders it all as one message
that tells the model to check the world before its first action.

Agent records live in their own collection, the memory collection's name
with `_agents` appended unless `ResumeRequest.Collection` says otherwise.
They cannot share the memory collection, because every record there must
decode as a memory event. Records are embedded with the client's embedder,
or the lexical embedder when it has none.

**One process per agent.** Resume needs a backend that implements
`LeaseBackend`; the Polign adapter does, against a server with the lease API.
A second process resuming the same agent gets `ErrLeaseHeld` (a
`*LeaseHeldError` names the holder). The holder renews in the background every
third of the TTL (60 seconds by default), and every write renews first when a
third of the TTL has passed, so writes land inside an epoch the server
granted. Once another process has taken over, writes fail with
`ErrLeaseLost`. `Release` hands the lease over at once; without it, the next
process waits out the TTL. Set `Unleased` only on a backend without leases,
when something else already guarantees one process per agent.

**Resuming without waiting.** When a process dies without releasing, the
next one normally waits out the TTL before `Resume` succeeds. Where that
silence costs something, as on a phone call, set `Deferred`: `Resume` reads
the records and returns the context at once without taking the lease, since
reading cannot conflict with anything. Writes fail with `ErrLeaseNotHeld`
until `AcquireLease` succeeds, so buffer them. `AcquireLease` tries once and
returns a `*LeaseHeldError` while the old lease is live; retry it until it
succeeds. When it does, it rereads the newest turn and working state, so
anything the old process wrote after the deferred read is continued, not
overwritten. `ResumeContext.LeaseHeld` and `Agent.LeaseHeld` report which
state the agent is in.

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
[audit format and concurrency contract](audit.md) for usage, canonical
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

### Notes

Every registry accepts `note`, even one that does not list it. A note holds a
statement that no other predicate fits, as free text. It keeps information that
would otherwise be refused and lost.

- A note is multi-valued: a new note never replaces an older one, and it never
  conflicts with a typed memory.
- Recall returns typed memories ahead of notes in the same answer.
- A typed `remember` with an unregistered predicate is still refused, so the
  agent gets a chance to pick the right name. The error lists the registered
  predicates and says to use `note` when none of them fits.
- A registry may define `note` with its own description. It must stay
  `multi` and `string`.

To review notes, recall with predicate `note`, or replay an audit bundle with
`recall-audit -notes`. When notes show a recurring kind of fact, add a predicate
for it, then move each note across with `Promote`:

```go
res, err := client.Promote(ctx, recall.PromoteRequest{
    Subject:   "user",
    Note:      "I use fish as my shell.",
    Predicate: "prefers_shell",
    Value:     "fish",
})
```

`Promote` writes the typed memory first and then withdraws the note. If it
fails in between, the fact is stored twice rather than lost. The note stays in
history.

## Embeddings

For local word-overlap search, pass `recall.LexicalEmbedder{}` to `NewClient`.
For semantic search, supply an `Embedder` backed by your chosen model. Legacy
`NewStore` accepts an `embed func(string) []float32`. Stored vectors and query
vectors must use the same embedding method and dimensions. Use a separate
collection when changing methods.

