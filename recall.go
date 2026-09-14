package recall

import (
	"time"

	"github.com/Polign/recall/internal/engine"
)

// The public package keeps the supported import path. Implementation and its
// tests live in internal/engine; aliases retain the underlying method sets.

// Backend is the context-aware storage contract used by Client. Implementations
// must be safe for concurrent calls. List must obey VectorDB's exact totals,
// internal pagination, and stable ordered-prefix contract.
type Backend = engine.Backend

// Embedder supplies compatible document and query vectors. Implementations must
// honor cancellation and be safe for concurrent calls.
type Embedder = engine.Embedder

// EmbedFunc adapts a function to Embedder.
type EmbedFunc = engine.EmbedFunc

// ErrEmbedderRequired means an operation needs vectors but no embedder was configured.
var ErrEmbedderRequired = engine.ErrEmbedderRequired

// Config configures an immutable Client. Embedder is optional for exact reads,
// histories, and exports; writes and semantic recall require it.
type Config = engine.Config

// Client is a context-aware memory client. Configuration is immutable, and
// operations have independent request state. Concurrent writes still follow the
// backend's consistency guarantees; Client does not promise serializable writes.
type Client = engine.Client

// NewClient validates configuration and copies the registry.
func NewClient(cfg Config) (*Client, error) {
	return engine.NewClient(cfg)
}

// RememberRequest records a typed value (string, float64, or bool). Kind defaults
// to fact and Source to user_stated. Nil Confidence defaults to 1; a pointer to
// zero records zero confidence rather than selecting the default.
type RememberRequest = engine.RememberRequest

// ForgetRequest selects exactly one typed Value or All=true. False and numeric
// zero are values; an omitted value never implicitly withdraws everything.
type ForgetRequest = engine.ForgetRequest

// StoredVector is one record as the database returns it.
type StoredVector = engine.StoredVector

// Hit is one search result.
type Hit = engine.Hit

// VectorDB is the database surface the store needs. Keeping it an interface
// is what lets the memory layer live outside the database it usually runs
// against, and lets tests exercise the semantics without one.
type VectorDB = engine.VectorDB

// MaxHistoryEvents bounds a complete subject-and-predicate history. Histories
// beyond this bound fail explicitly; a partial fold could revive a retraction.
const MaxHistoryEvents = engine.MaxHistoryEvents

// ErrIncompleteHistory means the backend could not supply a complete exact log.
// No belief or write may be derived from such a log.
var ErrIncompleteHistory = engine.ErrIncompleteHistory

// Store turns writes into durable, typed events and reads into beliefs. It
// derives each answer from the log returned by its backend at read time.
// Consistency across concurrent operations depends on that backend.
type Store = engine.Store

// NewStore returns a store over one collection.
//
// An invalid registry is recorded rather than returned, because this
// constructor has never had an error to return. Every operation reports it, so
// a bad cardinality surfaces at the first call instead of silently folding a
// multi-valued predicate as single-valued and appearing much later as a
// malformed audit.
func NewStore(db VectorDB, collection string, registry Registry, embed func(string) []float32) *Store {
	return engine.NewStore(db, collection, registry, embed)
}

// RememberResult is what a write reports back: the assertion stored or found,
// whether it already held, and anything it displaced.
type RememberResult = engine.RememberResult

// Query is one read. With Text set the read is semantic (the text is embedded
// and searched); without it it is an exact filtered read. Both modes answer
// with beliefs folded from the same log, so semantic recall can never return
// something exact recall would say is no longer believed.
type Query = engine.Query

// Event is one durable statement about a subject, or the retraction of one.
// Events are immutable once written: a correction is a later event, never an
// edit of an earlier one.
type Event = engine.Event

// Belief is a statement that holds at some instant: the result of folding a
// log, not a stored row.
type Belief = engine.Belief

// Cardinality decides what a second value for the same subject and predicate
// means.
type Cardinality = engine.Cardinality

// Single means a newer value replaces the older one: an agent has one
// preferred editor.
const Single = engine.Single

// Multi means values accumulate: an agent may like many languages.
const Multi = engine.Multi

// Fold reduces one subject-and-predicate's events to the beliefs that hold at
// asOf. A zero asOf means now.
//
// Events may arrive in any order and may repeat; the fold is defined by the
// log's content alone, so a partially applied write, a retried write, or a
// clock that stepped backwards changes the answer only in so far as it
// changed the log.
func Fold(events []Event, card Cardinality, asOf time.Time) []Belief {
	return engine.Fold(events, card, asOf)
}

// ValueKey renders a value in the canonical form used to tell two values
// apart. Strings fold case, so "Neovim" stated twice with different capitals
// is one belief rather than two; the event keeps whatever form was written,
// and only identity is case-insensitive. Numbers render without exponent
// drift so that 8000 and 8000.0 are one value rather than two.
func ValueKey(v any) string {
	return engine.ValueKey(v)
}

// Predicate is one registry entry. Cardinality decides what a second value
// for the same subject means: "single" makes a newer value supersede the old
// one, "multi" makes it an additional fact. ValueType decides what a value
// IS: a string, a number, or a boolean. Values are stored with that type, so
// a number compares numerically in recall filters instead of lexically.
type Predicate = engine.Predicate

// Registry is the closed set of predicates the store accepts. A write with an
// unregistered predicate is rejected, so an agent cannot invent near-duplicate
// predicates ("editor_preference" beside "prefers_editor") and split one fact
// across two names that never meet.
type Registry = engine.Registry

// LoadRegistry parses and validates a registry document.
func LoadRegistry(raw []byte) (Registry, error) {
	return engine.LoadRegistry(raw)
}

// DefaultRegistry returns an independent starter vocabulary. Applications may
// extend the returned map before constructing a Client.
func DefaultRegistry() Registry {
	return engine.DefaultRegistry()
}

// LexicalEmbedder is a dependency-free signed feature-hashing fallback. It
// retrieves overlapping words, not semantic synonyms. Use a dedicated collection
// for this versioned 256-dimensional space; never mix it with model embeddings.
type LexicalEmbedder = engine.LexicalEmbedder

const LexicalSpace = engine.LexicalSpace

// Proposal is an untrusted model suggestion, never a belief until Remember
// validates and folds it. Evidence must be an exact excerpt from the input.
type Proposal = engine.Proposal

type Extractor = engine.Extractor

type ExtractionResult = engine.ExtractionResult

// ProposedStatements adapts proposals made by the calling agent. This lets an
// MCP host use its own model for extraction without another model service.
// RememberText still treats every proposal as untrusted and validates the batch.
type ProposedStatements = engine.ProposedStatements

// WatermarkBackend optionally supplies an opaque revision of the complete
// collection visible to this reader. It must change on every visible mutation,
// including overwritten IDs, deletes, late arrivals, restores, and route changes.
// It is not an observation timestamp or a maximum event ID. Equality is the only
// meaningful operation; replicas need not expose the same revision.
//
// A revision must never be reused once it has changed. A counter that resets on
// restart lets a revision repeat, and a repeat silently revalidates a cached
// fold that the log has since moved past. Entries also expire after
// 30 seconds so that a backend which under-reports bounds, rather than
// keeps, the staleness it causes.
type WatermarkBackend = engine.WatermarkBackend

var ErrWatermarkUnsupported = engine.ErrWatermarkUnsupported

// Materialization is a bounded, process-local derived belief view owned by one
// client. The durable event log remains authoritative. Restart rebuilds the view.
type Materialization = engine.Materialization

// Export selects part of the log for audit. An empty field means "every one of
// these": zero Export exports everything the store holds.
type Export = engine.Export

// MaxExport bounds an unbounded export, so that asking for everything cannot
// turn into an unbounded scan by accident.
const MaxExport = engine.MaxExport

// SortEvents orders a log oldest first, breaking ties on id so that two
// exports of the same events are byte-identical.
func SortEvents(events []Event) {
	engine.SortEvents(events)
}

// Digest is the legacy stable checksum over a log. It uses case-insensitive
// value identity, so it does not detect changes only to string capitalization.
// Use DigestV2 for exact typed values or ExportAudit for reproducible bundles.
//
// A separately trusted digest lets two parties compare logs without exchanging
// them. A checksum alone does not authenticate an exporter or prove completeness.
// The input is each event's canonical fields in a fixed order, so the digest
// depends on the content of the log and not on how it was serialised, paged,
// or which node answered.
func Digest(events []Event) string {
	return engine.Digest(events)
}

// AuditVersion identifies the bundle envelope and its canonical checksum.
const AuditVersion = engine.AuditVersion

// EventVersion identifies the typed Event fields recorded in a bundle.
const EventVersion = engine.EventVersion

// FoldVersion identifies observation-time ordering and the current single
// and multi-value fold rules. Incompatible changes need a new replay path.
//
// v2 orders a retraction after the assertion it withdraws when the two
// share an instant. v1 ordered that tie on the event id, so the same log
// can fold differently under the two, and a v1 bundle must not be replayed
// here as though nothing had changed.
const FoldVersion = engine.FoldVersion

// ErrInvalidAudit marks unsupported versions or invalid bundle inputs.
var ErrInvalidAudit = engine.ErrInvalidAudit

// ErrDigestMismatch means validated content differs from its checksum.
var ErrDigestMismatch = engine.ErrDigestMismatch

// AuditScope selects complete histories. Empty fields match all subjects or
// predicates. Unlike Export, this scope cannot request a truncated event subset.
type AuditScope = engine.AuditScope

// AuditRequest selects histories and the instant at which to replay them.
// A zero AsOf captures the store's current time once, before reading events.
type AuditRequest = engine.AuditRequest

// AuditBundle records the inputs needed to reproduce exact beliefs offline.
// The digest covers every field except Digest itself, independent of event or
// registry iteration order. It is a checksum, not a signature or proof that the
// exporter supplied every event. Retain its digest through a trusted channel.
type AuditBundle = engine.AuditBundle

// DigestV2 hashes exact typed event fields, including case-sensitive strings
// and the IEEE-754 bits of finite numbers (so -0 and +0 differ). Instants are
// canonical UTC timestamps. It rejects invalid events and duplicate IDs. Digest
// remains the legacy, case-insensitive checksum for existing exports.
func DigestV2(events []Event) (string, error) {
	return engine.DigestV2(events)
}

// VerifyDigest verifies either a legacy sha256: digest or a sha256:v2: digest.
// Legacy verification intentionally retains its original identity semantics.
func VerifyDigest(events []Event, expected string) error {
	return engine.VerifyDigest(events, expected)
}

// EventFromMetadata decodes without validation, retaining its legacy behavior.
// Invalid fields become their zero values. Use DecodeEvent before deriving
// beliefs from stored metadata; Store uses that strict decoder internally.
func EventFromMetadata(id string, m map[string]any) Event {
	return engine.EventFromMetadata(id, m)
}

// ErrInvalidEvent means a stored record cannot safely participate in a fold.
var ErrInvalidEvent = engine.ErrInvalidEvent

// DecodeEvent validates typed event metadata before returning it. Store reads
// use this strict decoder so a malformed correction or retraction cannot be
// silently skipped. EventFromMetadata remains available as a permissive decoder.
// Missing retraction denotes a legacy assertion; observed_ms is optional because
// observed_at is the authoritative timestamp.
func DecodeEvent(id string, m map[string]any) (Event, error) {
	return engine.DecodeEvent(id, m)
}

// MaxCandidateEvents bounds event discovery for broad exact queries. A query
// for one explicit subject/predicate uses MaxHistoryEvents instead.
const MaxCandidateEvents = engine.MaxCandidateEvents

// ErrIncompleteCandidates means a broad exact query could not discover enough
// matching beliefs or prove it exhausted the candidate set. No partial answer
// is returned; callers can narrow the query to an explicit subject/predicate.
var ErrIncompleteCandidates = engine.ErrIncompleteCandidates

// MaxRecall bounds the number of beliefs a query can request. Candidate
// discovery and result allocations are bounded independently of caller input.
const MaxRecall = engine.MaxRecall
