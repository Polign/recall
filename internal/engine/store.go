package engine

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// StoredVector is one record as the database returns it.
type StoredVector struct {
	ID       string
	Values   []float32
	Metadata map[string]any
}

// Hit is one search result.
type Hit struct {
	ID       string
	Distance float32
	Score    float32
	Metadata map[string]any
}

// VectorDB is the database surface the store needs. Keeping it an interface
// is what lets the memory layer live outside the database it usually runs
// against, and lets tests exercise the semantics without one.
type VectorDB interface {
	Put(collection, id string, values []float32, metadata map[string]any) error
	// List returns up to limit exact matches and the total number of matches.
	// Implementations must page internally when their backend caps a page.
	// Increasing limit must extend the same ordered prefix while data is unchanged.
	List(collection string, filter map[string]any, limit int) ([]StoredVector, int, error)
	Search(collection string, values []float32, k int, filter map[string]any) ([]Hit, error)
}

// MaxHistoryEvents bounds a complete subject-and-predicate history. Histories
// beyond this bound fail explicitly; a partial fold could revive a retraction.
const MaxHistoryEvents = 10000

// ErrIncompleteHistory means the backend could not supply a complete exact log.
// No belief or write may be derived from such a log.
var ErrIncompleteHistory = errors.New("recall: complete exact history unavailable")

// defaultLimit is how many beliefs a recall returns when the caller
// does not say.
const defaultLimit = 20

// Store turns writes into durable, typed events and reads into beliefs. It
// derives each answer from the log returned by its backend at read time.
// Consistency across concurrent operations depends on that backend.
type Store struct {
	db           VectorDB
	collection   string
	registry     Registry
	embed        func(string) ([]float32, error)
	now          func() time.Time
	materialized *Materialization
	registryErr  error
}

// NewStore returns a store over one collection.
//
// An invalid registry is recorded rather than returned, because this
// constructor has never had an error to return. Every operation reports it, so
// a bad cardinality surfaces at the first call instead of silently folding a
// multi-valued predicate as single-valued and appearing much later as a
// malformed audit.
func NewStore(db VectorDB, collection string, registry Registry, embed func(string) []float32) *Store {
	return &Store{db: db, collection: collection, registry: registry.Clone(), now: time.Now,
		registryErr: registry.Validate(),
		embed: func(text string) ([]float32, error) {
			if embed == nil {
				return nil, ErrEmbedderRequired
			}
			return embed(text), nil
		},
	}
}

// Registry returns a copy of the predicates this store accepts.
func (s *Store) Registry() Registry { return s.registry.Clone() }

// RememberResult is what a write reports back: the assertion stored or found,
// whether it already held, and anything it displaced.
type RememberResult struct {
	Stored     Belief   `json:"stored"`
	Existing   bool     `json:"already_known,omitempty"`
	Superseded []Belief `json:"superseded,omitempty"`
}

// Remember records one statement.
//
// It writes at most one event, and only after folding the existing log: if the
// statement already holds, nothing is written at all. Nothing is ever
// rewritten, so there is no window in which a crash can leave two live beliefs
// for a single-valued predicate.
func (s *Store) Remember(kind, subject, predicate string, value any, confidence float64, source string) (RememberResult, error) {
	if s.registryErr != nil {
		return RememberResult{}, s.registryErr
	}

	if confidence == 0 {
		confidence = 1
	}
	return s.remember(kind, subject, predicate, value, confidence, source)
}

func (s *Store) remember(kind, subject, predicate string, value any, confidence float64, source string) (RememberResult, error) {
	var zero RememberResult

	if !validKinds[kind] {
		return zero, fmt.Errorf(`kind must be "fact" or "preference", got %q`, kind)
	}
	subject = normalizeSubject(subject)
	if subject == "" {
		return zero, fmt.Errorf("subject must not be empty")
	}
	predicate = strings.TrimSpace(predicate)
	if !predicateName.MatchString(predicate) {
		return zero, fmt.Errorf("predicate must be snake_case (e.g. prefers_editor), got %q", predicate)
	}
	spec, ok := s.registry[predicate]
	if !ok {
		return zero, fmt.Errorf("predicate %q is not in the registry; registered predicates are: %s",
			predicate, strings.Join(s.registry.Names(), ", "))
	}
	value, err := normalizeValue(predicate, spec, value)
	if err != nil {
		return zero, err
	}
	if !finite(confidence) || confidence < 0 || confidence > 1 {
		return zero, fmt.Errorf("confidence must be in [0, 1], got %g", confidence)
	}
	if source == "" {
		source = "user_stated"
	}
	if !validSources[source] {
		return zero, fmt.Errorf(`source must be "user_stated", "agent_inferred", or "tool_result", got %q`, source)
	}

	events, err := s.History(subject, predicate)
	if err != nil {
		return zero, err
	}
	// Remember keeps the writer's own instant. Observation time, not write
	// acceptance order, decides what is believed, so a statement observed
	// earlier stays earlier: TestObservationTimeOverridesWriteAcceptanceOrder
	// pins that, and Superseded below is only meaningful at this same ceiling.
	now := disambiguateInstant(events, s.now())
	held := Fold(events, spec.Cardinal(), now)

	// Idempotence lives here rather than in the id: stating what is already
	// believed is not new information, and appending it would add an event
	// that changes no answer.
	for _, b := range held {
		if ValueKey(b.Value) == ValueKey(value) {
			return RememberResult{Stored: b, Existing: true}, nil
		}
	}

	if len(events) >= MaxHistoryEvents {
		return zero, fmt.Errorf("%w: maximum %d events reached", ErrIncompleteHistory, MaxHistoryEvents)
	}

	ev := Event{
		ID:         eventID(subject, predicate, value, false, now),
		Kind:       kind,
		Subject:    subject,
		Predicate:  predicate,
		Value:      value,
		Confidence: confidence,
		Source:     source,
		ObservedAt: now,
	}
	if err := s.append(ev); err != nil {
		return zero, err
	}

	// What this statement displaced, reported for the agent's benefit. It is
	// derived from the fold, not from any field written on the old events.
	var superseded []Belief
	if spec.Cardinal() == Single {
		superseded = held
	}
	return RememberResult{Stored: beliefOf(ev), Superseded: superseded}, nil
}

// Forget withdraws a belief by appending a retraction.
//
// It is a write, never a delete. Forgetting on Wednesday must not change what
// the agent believed on Tuesday, and a log that erased its own history could
// not answer that. With value set only that value is withdrawn; without it
// every value for the pair is.
func (s *Store) Forget(subject, predicate, value string) (int, error) {
	if s.registryErr != nil {
		return 0, s.registryErr
	}

	predicate = strings.TrimSpace(predicate)
	var typed any
	if value = strings.TrimSpace(value); value != "" {
		t, err := s.registry.parseValue(predicate, value)
		if err != nil {
			return 0, err
		}
		typed = t
	}
	return s.forgetValue(subject, predicate, typed)
}

func (s *Store) forgetValue(subject, predicate string, typed any) (int, error) {
	subject = normalizeSubject(subject)
	predicate = strings.TrimSpace(predicate)
	if subject == "" || predicate == "" {
		return 0, fmt.Errorf("forget needs a subject and a predicate")
	}
	spec, ok := s.registry[predicate]
	if !ok {
		return 0, fmt.Errorf("predicate %q is not in the registry", predicate)
	}
	if typed != nil {
		value, err := normalizeValue(predicate, spec, typed)
		if err != nil {
			return 0, err
		}
		typed = value
	}

	events, err := s.History(subject, predicate)
	if err != nil {
		return 0, err
	}
	// A retraction has to land after everything it withdraws. Folding and
	// stamping at the writer's own clock makes Forget a silent no-op whenever
	// the log already holds a later instant: it would report success, write
	// nothing, and let the belief reappear. Remember keeps the writer's clock
	// for the reason given there; Forget cannot.
	now := writeInstant(events, s.now())
	held := Fold(events, spec.Cardinal(), now)
	if len(held) == 0 {
		return 0, nil
	}

	withdrawn := 0
	for _, b := range held {
		if typed == nil || ValueKey(b.Value) == ValueKey(typed) {
			withdrawn++
		}
	}
	if withdrawn == 0 {
		// Nothing believed matches, so a retraction would record a withdrawal
		// that never happened.
		return 0, nil
	}

	if len(events) >= MaxHistoryEvents {
		return 0, fmt.Errorf("%w: maximum %d events reached", ErrIncompleteHistory, MaxHistoryEvents)
	}

	ev := Event{
		ID:         eventID(subject, predicate, typed, true, now),
		Kind:       "fact",
		Subject:    subject,
		Predicate:  predicate,
		Value:      typed,
		Confidence: 1,
		Source:     "user_stated",
		Retraction: true,
		ObservedAt: now,
	}
	if err := s.append(ev); err != nil {
		return 0, err
	}
	return withdrawn, nil
}

// Query is one read. With Text set the read is semantic (the text is embedded
// and searched); without it it is an exact filtered read. Both modes answer
// with beliefs folded from the same log, so semantic recall can never return
// something exact recall would say is no longer believed.
type Query struct {
	// Text runs a semantic search. Empty means an exact, filtered read.
	Text      string
	Subject   string
	Predicate string
	Kind      string
	// AsOf answers as of a past instant. Zero means now, so "what is believed"
	// and "what was believed on Tuesday" are the same call.
	AsOf          time.Time
	MinConfidence float64
	Limit         int
	// ValueMin and ValueMax bound number-typed values.
	ValueMin *float64
	ValueMax *float64
}

// Recall returns the beliefs that hold at the query's instant.
func (s *Store) Recall(q Query) ([]Belief, error) {
	if s.registryErr != nil {
		return nil, s.registryErr
	}

	if err := validateQuery(q); err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	asOf := q.AsOf
	if asOf.IsZero() {
		asOf = s.now().UTC()
	}
	if q.Text == "" && (q.Subject == "" || q.Predicate == "") {
		return s.recallExact(q, limit, asOf)
	}

	pairs, err := s.candidatePairs(q, limit)
	if err != nil {
		return nil, err
	}

	out := make([]Belief, 0, limit)
	for _, p := range pairs {
		beliefs, err := s.pairBeliefs(p, asOf)
		if err != nil {
			return nil, err
		}
		for _, b := range beliefs {
			if !matchesBelief(b, q) {
				continue
			}
			out = append(out, b)
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

func (s *Store) pairBeliefs(p pair, asOf time.Time) ([]Belief, error) {
	return s.materializedBeliefs(p, asOf)
}

// cardinality reports how a predicate folds. An unregistered predicate folds
// as single-valued, which is the safer default: it supersedes rather than
// accumulates.
func (s *Store) cardinality(predicate string) Cardinality {
	if spec, ok := s.registry[predicate]; ok {
		return spec.Cardinal()
	}
	return Single
}

// disambiguateInstant keeps a new event off an instant this pair's log already
// occupies. Two events sharing an instant are ordered by their id hash, which
// is deterministic but uncorrelated with the order they were stated, so a
// stopped clock or a coarse one can silently invert a correction. Unlike
// writeInstant this never moves past a later event, so a deliberately backdated
// statement stays backdated.
func disambiguateInstant(events []Event, now time.Time) time.Time {
	now = now.UTC()
	for again := true; again; {
		again = false
		for _, e := range events {
			if e.ObservedAt.Equal(now) {
				now = now.Add(time.Nanosecond)
				again = true
			}
		}
	}
	return now
}

// writeInstant places a new event after everything already in this pair's log.
//
// Folding at the writer's own clock is wrong whenever the log already holds a
// later instant, which a peer whose clock runs ahead or a local step backwards
// both produce. The decision would not see that event, and the event written
// at the writer's instant would sort before it: Forget would report success
// having withdrawn nothing, and a correction through Remember would revert as
// soon as the clock caught up. Neither reports an error, which is the worst
// available outcome for a memory that is asked to be correctable.
func writeInstant(events []Event, now time.Time) time.Time {
	now = now.UTC()
	for _, e := range events {
		if !e.ObservedAt.Before(now) {
			now = e.ObservedAt.UTC().Add(time.Nanosecond)
		}
	}
	return now
}

func (s *Store) foldPair(p pair, asOf time.Time) ([]Belief, error) {
	events, err := s.History(p.subject, p.predicate)
	if err != nil {
		return nil, err
	}
	return Fold(events, s.cardinality(p.predicate), asOf), nil
}

// History returns every event recorded for one subject and predicate, oldest
// first. It is the audit primitive: the fold is a pure function of exactly
// this, so anything the store answered can be re-derived from it.
func (s *Store) History(subject, predicate string) ([]Event, error) {
	if s.registryErr != nil {
		return nil, s.registryErr
	}

	filter := map[string]any{
		"subject":   normalizeSubject(subject),
		"predicate": strings.TrimSpace(predicate),
	}
	events, err := s.completeEvents(filter, MaxHistoryEvents)
	if err != nil {
		return nil, err
	}
	SortEvents(events)
	return events, nil
}

// pair identifies one belief slot.
type pair struct{ subject, predicate string }

// candidatePairs selects an explicitly requested pair or semantic candidates.
// Broad exact queries use recallExact to keep discovering past withdrawn or
// filtered pairs. Semantic candidates are approximate and folded afterwards.
func (s *Store) candidatePairs(q Query, limit int) ([]pair, error) {
	if q.Subject != "" && q.Predicate != "" {
		return []pair{{normalizeSubject(q.Subject), strings.TrimSpace(q.Predicate)}}, nil
	}

	filter := candidateFilter(q)

	// Candidates are read wider than the limit: several events can belong to
	// one pair, and a pair can fold to nothing, so a page of events yields
	// fewer beliefs than it holds rows.
	width := limit * 4
	if width < 20 {
		width = 20
	}

	events, err := s.searchEvents(q.Text, filter, width)
	if err != nil {
		return nil, err
	}
	return dedupePairs(events), nil
}

// dedupePairs keeps each pair once, in the order the events arrived, so that
// search relevance survives into the answer.
func dedupePairs(events []Event) []pair {
	seen := map[pair]bool{}
	out := make([]pair, 0, len(events))
	for _, e := range events {
		p := pair{normalizeSubject(e.Subject), strings.TrimSpace(e.Predicate)}
		if p.subject == "" || p.predicate == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// matchesBelief applies the filters that describe a belief rather than the
// events behind it, so they are checked after folding.
func matchesBelief(b Belief, q Query) bool {
	if q.Kind != "" && b.Kind != q.Kind {
		return false
	}
	if q.MinConfidence > 0 && b.Confidence < q.MinConfidence {
		return false
	}
	if q.ValueMin != nil || q.ValueMax != nil {
		f, ok := b.Value.(float64)
		if !ok {
			return false
		}
		if q.ValueMin != nil && f < *q.ValueMin {
			return false
		}
		if q.ValueMax != nil && f > *q.ValueMax {
			return false
		}
	}
	return true
}

// append writes one event with a freshly embedded vector.
func (s *Store) append(e Event) error {
	values, err := s.embed(e.Text())
	if err != nil {
		return err
	}
	if err := s.db.Put(s.collection, e.ID, values, e.Metadata()); err != nil {
		return err
	}
	s.materialized.invalidate(pair{normalizeSubject(e.Subject), strings.TrimSpace(e.Predicate)})
	return nil
}

// searchEvents runs a semantic search over the event log.
func (s *Store) searchEvents(query string, filter map[string]any, limit int) ([]Event, error) {
	values, err := s.embed(query)
	if err != nil {
		return nil, err
	}
	hits, err := s.db.Search(s.collection, values, limit, filter)
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(hits))
	for _, h := range hits {
		e, err := s.decodeEvent(h.ID, h.Metadata)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// completeEvents refuses to fold a truncated log. One extra row detects an
// overflow even when a backend reports only the size of the returned page.
func (s *Store) completeEvents(filter map[string]any, limit int) ([]Event, error) {
	vectors, total, err := s.db.List(s.collection, filter, limit+1)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrIncompleteHistory, err)
	}
	if total != len(vectors) || len(vectors) > limit {
		return nil, fmt.Errorf("%w: read %d of %d events (maximum %d)", ErrIncompleteHistory, len(vectors), total, limit)
	}
	seen := make(map[string]bool, len(vectors))
	out := make([]Event, 0, len(vectors))
	for _, v := range vectors {
		if seen[v.ID] {
			return nil, fmt.Errorf("%w: duplicate event %q in listing", ErrIncompleteHistory, v.ID)
		}
		seen[v.ID] = true
		e, err := s.decodeEvent(v.ID, v.Metadata)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrIncompleteHistory, err)
		}
		out = append(out, e)
	}
	return out, nil
}

// coldListUnsupported is the server's way of saying this collection is served
// from object storage and has no complete in-memory listing index.
const coldListUnsupported = "listing is not supported for a cold-served resource"

// eventsMatching reads a bounded exact subset for an explicitly limited export,
// falling back to filtered vector search on a cold-served collection.
// It cannot establish a complete history or exhaustive candidate discovery.
//
// A server cold-started from an object store deliberately has no listing index,
// so List is refused. Filtered search scans the same segments and merges the
// write-ahead tail, which makes it the exact-filter fallback on a failover node
// reading from S3, GCS, or Azure. The fallback belongs here and nowhere else:
// search is bounded and approximate, so it can serve an export that already
// declares itself bounded, and must never stand in for completeEvents, which
// exists to refuse a truncated log.
func (s *Store) eventsMatching(filter map[string]any, limit int) ([]Event, error) {
	vectors, _, err := s.db.List(s.collection, filter, limit)
	if err != nil {
		if !strings.Contains(err.Error(), coldListUnsupported) {
			return nil, err
		}
		return s.searchEvents("typed durable memory record", filter, limit)
	}
	out := make([]Event, 0, len(vectors))
	for _, v := range vectors {
		e, err := s.decodeEvent(v.ID, v.Metadata)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// normalizeSubject folds a subject to its stored form, so that "User" and
// " user " address one subject rather than three.
func normalizeSubject(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
