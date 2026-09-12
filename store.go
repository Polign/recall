package recall

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
// holds no state: every answer is folded from the log at read time, so two
// processes sharing a collection cannot disagree about what is believed.
type Store struct {
	db         VectorDB
	collection string
	registry   Registry
	embed      func(string) []float32
	now        func() time.Time
}

// NewStore returns a store over one collection.
func NewStore(db VectorDB, collection string, registry Registry, embed func(string) []float32) *Store {
	return &Store{db: db, collection: collection, registry: registry, embed: embed, now: time.Now}
}

// Registry returns the predicates this store accepts.
func (s *Store) Registry() Registry { return s.registry }

// RememberResult is what a write reports back: the belief that now holds,
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
	if confidence == 0 {
		confidence = 1.0
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
	now := s.now().UTC()
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
	subject = normalizeSubject(subject)
	predicate = strings.TrimSpace(predicate)
	if subject == "" || predicate == "" {
		return 0, fmt.Errorf("forget needs a subject and a predicate")
	}
	spec, ok := s.registry[predicate]
	if !ok {
		return 0, fmt.Errorf("predicate %q is not in the registry", predicate)
	}

	var typed any
	if value = strings.TrimSpace(value); value != "" {
		t, err := s.registry.parseValue(predicate, value)
		if err != nil {
			return 0, err
		}
		typed = t
	}

	events, err := s.History(subject, predicate)
	if err != nil {
		return 0, err
	}
	now := s.now().UTC()
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

	pairs, err := s.candidatePairs(q, limit)
	if err != nil {
		return nil, err
	}

	out := make([]Belief, 0, limit)
	for _, p := range pairs {
		events, err := s.History(p.subject, p.predicate)
		if err != nil {
			return nil, err
		}
		card := Single
		if spec, ok := s.registry[p.predicate]; ok {
			card = spec.Cardinal()
		}
		for _, b := range Fold(events, card, asOf) {
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

// History returns every event recorded for one subject and predicate, oldest
// first. It is the audit primitive: the fold is a pure function of exactly
// this, so anything the store answered can be re-derived from it.
func (s *Store) History(subject, predicate string) ([]Event, error) {
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

// candidatePairs finds the subject-and-predicate pairs a query might answer
// from. A semantic query searches; an exact one lists. Either way the answer
// is folded afterwards, so a candidate that is no longer believed drops out.
func (s *Store) candidatePairs(q Query, limit int) ([]pair, error) {
	if q.Subject != "" && q.Predicate != "" {
		return []pair{{normalizeSubject(q.Subject), strings.TrimSpace(q.Predicate)}}, nil
	}

	filter := map[string]any{}
	if q.Subject != "" {
		filter["subject"] = normalizeSubject(q.Subject)
	}
	if q.Predicate != "" {
		filter["predicate"] = strings.TrimSpace(q.Predicate)
	}
	if q.Kind != "" {
		filter["kind"] = q.Kind
	}

	// Candidates are read wider than the limit: several events can belong to
	// one pair, and a pair can fold to nothing, so a page of events yields
	// fewer beliefs than it holds rows.
	width := limit * 4
	if width < 20 {
		width = 20
	}

	var (
		events []Event
		err    error
	)
	if q.Text != "" {
		events, err = s.searchEvents(q.Text, filter, width)
	} else {
		events, err = s.eventsMatching(filter, width)
	}
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
		p := pair{e.Subject, e.Predicate}
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
	return s.db.Put(s.collection, e.ID, s.embed(e.Text()), e.Metadata())
}

// searchEvents runs a semantic search over the event log.
func (s *Store) searchEvents(query string, filter map[string]any, limit int) ([]Event, error) {
	hits, err := s.db.Search(s.collection, s.embed(query), limit, filter)
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

// eventsMatching lists exact candidates. Approximate search is not an exact
// listing fallback: a missing retraction would turn an old event into a belief.
func (s *Store) eventsMatching(filter map[string]any, limit int) ([]Event, error) {
	vectors, _, err := s.db.List(s.collection, filter, limit)
	if err != nil {
		return nil, err
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
