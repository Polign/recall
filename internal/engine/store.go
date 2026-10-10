package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
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

// TextSearcher is an optional VectorDB capability: a lexical (BM25) search
// over each event's text metadata field. Semantic recall uses it beside the
// vector search when the database offers it, because a hashed lexical vector
// ranks words far worse than an inverted index does.
type TextSearcher interface {
	// SearchText returns ErrTextSearchUnsupported when this collection has no
	// text index to search yet; recall then uses the vector search alone.
	SearchText(collection, text string, k int, filter map[string]any) ([]Hit, error)
}

// ErrTextSearchUnsupported reports that a lexical search is not available for
// a collection, such as before its first index is published.
var ErrTextSearchUnsupported = errors.New("recall: text search unsupported")

// TextField is the metadata key that holds an event's searchable text.
const TextField = "text"

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
	// reglog caches the registry recorded in the store; see schema.go.
	reglog *registryLog
	// textFirst ranks every lexical hit ahead of the vector hits instead of
	// fusing the two rankings; see textFirstFor.
	textFirst bool
	// open lets a write define a predicate; see Config.Open.
	open bool
}

// NewStore returns a store over one collection.
//
// An invalid registry is recorded rather than returned, because this
// constructor has never had an error to return. Every operation reports it, so
// a bad cardinality surfaces at the first call instead of silently folding a
// multi-valued predicate as single-valued and appearing much later as a
// malformed audit.
func NewStore(db VectorDB, collection string, registry Registry, embed func(string) []float32) *Store {
	registryErr := registry.Validate()
	withNote, err := registry.withNote()
	if err != nil {
		withNote = registry.Clone()
		registryErr = errors.Join(registryErr, err)
	}
	return &Store{db: db, collection: collection, registry: withNote, now: time.Now,
		registryErr: registryErr, reglog: &registryLog{},
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
	return s.remember(kind, subject, predicate, value, confidence, source, provenance{})
}

// provenance is where a statement came from, beyond its source kind: when it
// was made, and the text it was drawn from. The zero value means now, stated
// directly.
type provenance struct {
	observedAt time.Time
	evidence   string
	evidenceID string
	// cardinality and description define a predicate an open write meets
	// for the first time.
	cardinality Cardinality
	description string
}

// MaxObservationSkew is how far past the writer's clock an observation time
// may be. A statement observed in the future would stay hidden from every
// query about the present until its time came, so a later one is refused.
const MaxObservationSkew = time.Minute

// MaxEvidenceBytes bounds the excerpt a statement keeps as its evidence. The
// whole text lives in the evidence event; the excerpt is the quoted part.
const MaxEvidenceBytes = 2048

func (s *Store) remember(kind, subject, predicate string, value any, confidence float64, source string, from provenance) (RememberResult, error) {
	var zero RememberResult

	now := s.now()
	if !from.observedAt.IsZero() {
		if from.observedAt.After(now.Add(MaxObservationSkew)) {
			return zero, fmt.Errorf("observed_at %s is in the future; a statement can only be recorded as made now or earlier",
				from.observedAt.UTC().Format(time.RFC3339))
		}
		now = from.observedAt
	}
	from.evidence = strings.TrimSpace(from.evidence)
	from.evidenceID = strings.TrimSpace(from.evidenceID)
	if !utf8.ValidString(from.evidence) || len(from.evidence) > MaxEvidenceBytes {
		return zero, fmt.Errorf("evidence must be UTF-8 of at most %d bytes", MaxEvidenceBytes)
	}

	if !validKinds[kind] {
		return zero, fmt.Errorf(`kind must be "fact" or "preference", got %q`, kind)
	}
	subject = normalizeSubject(subject)
	if subject == "" {
		return zero, fmt.Errorf("subject must not be empty")
	}
	predicate = s.predicateName(predicate)
	if !predicateName.MatchString(predicate) || len(predicate) > maxPredicateName {
		return zero, fmt.Errorf("predicate must be snake_case (e.g. prefers_editor) of at most %d bytes, got %q", maxPredicateName, predicate)
	}
	sc, err := s.view()
	if err != nil {
		return zero, err
	}
	// A former name is accepted and stored under the name that owns it now.
	predicate = sc.canonical(predicate)
	spec, ok := sc.writable[predicate]
	if !ok && s.open {
		spec, ok = sc.reg[predicate]
	}
	if !ok && s.open {
		predicate, spec, sc, err = s.define(predicate, value, from)
		if err != nil {
			return zero, err
		}
		ok = true
	}
	if !ok {
		return zero, fmt.Errorf("predicate %q is not in the registry; registered predicates are: %s. "+
			"If none of them fits, remember it with predicate %q and the statement as the value",
			predicate, strings.Join(sc.writable.Names(), ", "), NotePredicate)
	}
	value, err = normalizeValue(predicate, spec, value)
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
	now = disambiguateInstant(events, now)
	held := sc.fold(predicate, events, now)

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
		Evidence:   from.evidence,
		EvidenceID: from.evidenceID,
	}
	if err := s.append(ev); err != nil {
		return zero, err
	}

	// What this statement displaced, reported for the agent's benefit. It is
	// derived from the fold, not from any field written on the old events.
	var superseded []Belief
	if sc.cardAt(predicate, now) == Single {
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

	sc, err := s.view()
	if err != nil {
		return 0, err
	}
	predicate = sc.canonical(s.predicateName(predicate))
	var typed any
	if value = strings.TrimSpace(value); value != "" {
		t, err := sc.reg.parseValue(predicate, value)
		if err != nil {
			return 0, err
		}
		typed = t
	}
	return s.forgetValue(subject, predicate, typed)
}

func (s *Store) forgetValue(subject, predicate string, typed any) (int, error) {
	subject = normalizeSubject(subject)
	predicate = s.predicateName(predicate)
	if subject == "" || predicate == "" {
		return 0, fmt.Errorf("forget needs a subject and a predicate")
	}
	sc, err := s.view()
	if err != nil {
		return 0, err
	}
	predicate = sc.canonical(predicate)
	spec, ok := sc.reg[predicate]
	if !ok {
		return 0, fmt.Errorf("predicate %q is not in the registry", predicate)
	}
	if typed != nil {
		value, err := lenientValue(predicate, spec, typed)
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
	held := sc.fold(predicate, events, now)
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
	// ValueAfter and ValueBefore bound date-typed values, both inclusive. A
	// calendar day counts as its first instant in UTC.
	ValueAfter  time.Time
	ValueBefore time.Time
	// RefersTo answers "who points at this subject": the beliefs of
	// ref-typed predicates whose value is the subject named here. It is an
	// exact read and cannot be combined with Text. With Predicate set, only
	// that predicate is searched.
	RefersTo string
	// FollowRefs adds, after each ref-typed belief in the answer, what is
	// believed about the subject it names, one hop and within Limit. The
	// added beliefs carry Via.
	FollowRefs bool
	// ObservedAfter and ObservedBefore keep only beliefs whose surviving
	// event was observed in that span, both inclusive. Either may be zero.
	ObservedAfter  time.Time
	ObservedBefore time.Time
	// NoTimeHint stops a search from reading a time phrase in Text ("two
	// weeks ago", "last Saturday", "in March") as a hint to look at that
	// span first. The hint only reorders candidates; it never drops any.
	NoTimeHint bool
}

// Recall returns the beliefs that hold at the query's instant.
func (s *Store) Recall(q Query) ([]Belief, error) {
	if s.registryErr != nil {
		return nil, s.registryErr
	}

	if err := validateQuery(q); err != nil {
		return nil, err
	}
	sc, err := s.view()
	if err != nil {
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
	q.Predicate = sc.canonical(s.predicateName(q.Predicate))

	var beliefs []Belief
	switch {
	case q.RefersTo != "":
		beliefs, err = s.recallReferrers(sc, q, limit, asOf)
	case q.Text == "" && (q.Subject == "" || q.Predicate == ""):
		beliefs, err = s.discover(sc, s.filters(sc, q), func(b Belief) bool { return matchesBelief(b, q) }, q.Predicate == RegistryPredicate || q.Predicate == ExtractionPredicate, limit, asOf)
	default:
		beliefs, err = s.recallRanked(sc, q, limit, asOf)
	}
	if err != nil {
		return nil, err
	}
	if q.FollowRefs {
		return s.followRefs(sc, beliefs, limit, asOf)
	}
	return beliefs, nil
}

// recallRanked answers a query that names its pair or carries search text.
func (s *Store) recallRanked(sc *schema, q Query, limit int, asOf time.Time) ([]Belief, error) {
	candidates, err := s.candidatePairs(sc, q, limit, asOf)
	if err != nil {
		return nil, err
	}

	var hits []hit
	for _, c := range candidates {
		beliefs, err := s.pairBeliefs(c.pair, asOf)
		if err != nil {
			return nil, err
		}
		multi := sc.cardAt(c.pair.predicate, asOf) == Multi
		for _, b := range beliefs {
			if !matchesBelief(b, q) {
				continue
			}
			rank := c.rank
			if c.values != nil && multi {
				// A multi-valued pair holds values that have nothing to do
				// with each other, so each one answers the query only when
				// its own event matched. Returning the rest in fold order is
				// how a query for one note came back with the oldest ten.
				r, ok := c.values[ValueKey(b.Value)]
				if !ok {
					continue
				}
				rank = r
			}
			// A single-valued pair ranks by its best-matching event: a hit on
			// a superseded value still means the question is about this pair,
			// and the answer is the value that holds now.
			hits = append(hits, hit{b, rank})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].rank < hits[j].rank })

	var out ranked
	for _, h := range hits {
		if out.add(h.belief) >= limit {
			break
		}
	}
	return out.beliefs(), nil
}

// recallReferrers finds the beliefs whose ref value is one subject. The
// value is stored the way subjects are, so the read is an exact filter on it.
func (s *Store) recallReferrers(sc *schema, q Query, limit int, asOf time.Time) ([]Belief, error) {
	if q.Text != "" {
		return nil, fmt.Errorf("recall: refers_to is an exact read and cannot be combined with search text")
	}
	target := normalizeSubject(q.RefersTo)
	if target == "" {
		return nil, fmt.Errorf("recall: refers_to must name a subject")
	}
	var predicates []string
	if q.Predicate != "" {
		if sc.reg[q.Predicate].valueType() != typeRef {
			return nil, fmt.Errorf("recall: refers_to needs a ref-typed predicate, and %q is not one", q.Predicate)
		}
		predicates = []string{q.Predicate}
	} else {
		for _, name := range sc.reg.Names() {
			if sc.reg[name].valueType() == typeRef {
				predicates = append(predicates, name)
			}
		}
	}
	var filters []map[string]any
	for _, predicate := range predicates {
		for _, name := range sc.storedNames(predicate) {
			f := map[string]any{"predicate": name, "value": target}
			if q.Subject != "" {
				f["subject"] = normalizeSubject(q.Subject)
			}
			filters = append(filters, f)
		}
	}
	return s.discover(sc, filters, func(b Belief) bool {
		return matchesBelief(b, q) && ValueKey(b.Value) == ValueKey(target)
	}, false, limit, asOf)
}

// followRefs appends what is believed about each subject a ref-typed belief
// names. It goes one hop: a belief it adds is never followed in turn, so the
// answer stays bounded by the question rather than by the shape of the data.
func (s *Store) followRefs(sc *schema, beliefs []Belief, limit int, asOf time.Time) ([]Belief, error) {
	seen := make(map[string]bool, len(beliefs))
	for _, b := range beliefs {
		seen[b.EventID] = true
	}
	out := beliefs
	followed := map[string]bool{}
	for _, b := range beliefs {
		if len(out) >= limit {
			break
		}
		target, ok := b.Value.(string)
		if !ok || sc.reg[b.Predicate].valueType() != typeRef || followed[target] {
			continue
		}
		followed[target] = true
		about, err := s.discover(sc, []map[string]any{{"subject": target}}, func(Belief) bool { return true }, false, limit, asOf)
		if err != nil {
			return nil, err
		}
		for _, a := range about {
			if len(out) >= limit {
				break
			}
			if seen[a.EventID] {
				continue
			}
			seen[a.EventID] = true
			a.Via = b.EventID
			out = append(out, a)
		}
	}
	return out, nil
}

// hit is one belief a semantic query found, with the search rank of the event
// that earned it.
type hit struct {
	belief Belief
	rank   int
}

// ranked collects one page of beliefs and returns typed beliefs ahead of
// notes. A note is kept so nothing is lost, but it is the store's weakest
// claim: when a typed belief and a note answer the same query, the typed one
// comes first. Which beliefs make the page is unchanged; only their order is.
type ranked struct{ typed, notes []Belief }

func (r *ranked) add(b Belief) int {
	if b.Predicate == NotePredicate {
		r.notes = append(r.notes, b)
	} else {
		r.typed = append(r.typed, b)
	}
	return len(r.typed) + len(r.notes)
}

func (r *ranked) beliefs() []Belief {
	return append(append(make([]Belief, 0, len(r.typed)+len(r.notes)), r.typed...), r.notes...)
}

func (s *Store) pairBeliefs(p pair, asOf time.Time) ([]Belief, error) {
	return s.materializedBeliefs(p, asOf)
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
	return s.foldEvents(p, events, asOf)
}

// foldEvents folds one pair's complete history under the store's schema.
func (s *Store) foldEvents(p pair, events []Event, asOf time.Time) ([]Belief, error) {
	sc, err := s.view()
	if err != nil {
		return nil, err
	}
	return sc.fold(sc.canonical(p.predicate), events, asOf), nil
}

// History returns every event recorded for one subject and predicate, oldest
// first. It is the audit primitive: the fold is a pure function of exactly
// this, so anything the store answered can be re-derived from it.
//
// A predicate that was renamed has events under each name it has had. They
// are one history, returned together, each event under the name it was
// written with.
func (s *Store) History(subject, predicate string) ([]Event, error) {
	if s.registryErr != nil {
		return nil, s.registryErr
	}
	sc, err := s.view()
	if err != nil {
		return nil, err
	}

	var events []Event
	for _, name := range sc.storedNames(sc.canonical(s.predicateName(predicate))) {
		filter := map[string]any{
			"subject":   normalizeSubject(subject),
			"predicate": name,
		}
		part, err := s.completeEvents(filter, MaxHistoryEvents)
		if err != nil {
			return nil, err
		}
		events = append(events, part...)
	}
	if len(events) > MaxHistoryEvents {
		return nil, fmt.Errorf("%w: read %d events (maximum %d)", ErrIncompleteHistory, len(events), MaxHistoryEvents)
	}
	SortEvents(events)
	return events, nil
}

// pair identifies one belief slot.
type pair struct{ subject, predicate string }

// candidatePairs selects an explicitly requested pair or semantic candidates.
// Broad exact queries use recallExact to keep discovering past withdrawn or
// filtered pairs. Semantic candidates are approximate and folded afterwards.
// candidate is a pair a semantic query reached, with the search rank of its
// best event and, per value, the rank of the best event asserting it.
type candidate struct {
	pair pair
	rank int
	// values is nil when the query named the pair outright, which asks for
	// all of it rather than for the values the search happened to match.
	values map[string]int
}

func (s *Store) candidatePairs(sc *schema, q Query, limit int, asOf time.Time) ([]candidate, error) {
	if q.Subject != "" && q.Predicate != "" {
		return []candidate{{pair: pair{normalizeSubject(q.Subject), q.Predicate}}}, nil
	}

	// Candidates are read wider than the limit: several events can belong to
	// one pair, and a pair can fold to nothing, so a page of events yields
	// fewer beliefs than it holds rows.
	width := limit * 4
	if width < 20 {
		width = 20
	}

	// A renamed predicate is searched under each of its names, the current
	// one first.
	//
	// A time phrase in the text searches its span first, for at most limit
	// events, and then everywhere; the span's events take the leading ranks.
	text := q.Text
	hint, hinted := timeHint{}, false
	if !q.NoTimeHint {
		hint, hinted = queryWindow(q.Text, asOf)
		if hinted && strings.TrimSpace(hint.rest) != "" {
			text = hint.rest
		}
	}
	var first, rest []Event
	for _, filter := range s.filters(sc, q) {
		filter = withObserved(filter, q.ObservedAfter, q.ObservedBefore)
		if hinted {
			from, to := hint.from, hint.to
			if from.Before(q.ObservedAfter) {
				from = q.ObservedAfter
			}
			if !q.ObservedBefore.IsZero() && to.After(q.ObservedBefore) {
				to = q.ObservedBefore
			}
			if from.Before(to) {
				found, err := s.searchEvents(text, withObserved(filter, from, to), limit)
				if err != nil {
					return nil, err
				}
				first = append(first, found...)
			}
		}
		found, err := s.searchEvents(text, filter, width)
		if err != nil {
			return nil, err
		}
		rest = append(rest, found...)
	}
	events := append(first, rest...)
	return rankCandidates(sc.candidates(events, q.Predicate == RegistryPredicate || q.Predicate == ExtractionPredicate)), nil
}

// candidates prepares discovered events for grouping into pairs: each takes
// the name of the predicate that owns it now, and the registry's own history
// is left out unless the query asked for it.
func (sc *schema) candidates(events []Event, registry bool) []Event {
	out := make([]Event, 0, len(events))
	for _, e := range events {
		if !registry && isReservedPair(pair{normalizeSubject(e.Subject), e.Predicate}) {
			continue
		}
		e.Predicate = sc.canonical(strings.TrimSpace(e.Predicate))
		out = append(out, e)
	}
	return out
}

// filters is the candidate filter for a query, once per name its predicate's
// events may be stored under.
func (s *Store) filters(sc *schema, q Query) []map[string]any {
	if q.Predicate == "" {
		return []map[string]any{candidateFilter(q)}
	}
	var out []map[string]any
	for _, name := range sc.storedNames(q.Predicate) {
		f := candidateFilter(q)
		f["predicate"] = name
		out = append(out, f)
	}
	return out
}

// rankCandidates groups search hits by pair, first hit first, and keeps the
// rank of each pair's best event and of the best event asserting each value.
// A retraction matching the query says the pair is relevant but asserts no
// value, so it ranks the pair and nothing inside it.
func rankCandidates(events []Event) []candidate {
	index := map[pair]int{}
	var out []candidate
	for rank, e := range events {
		p := pair{normalizeSubject(e.Subject), strings.TrimSpace(e.Predicate)}
		if p.subject == "" || p.predicate == "" {
			continue
		}
		i, seen := index[p]
		if !seen {
			i = len(out)
			index[p] = i
			out = append(out, candidate{pair: p, rank: rank, values: map[string]int{}})
		}
		if e.Retraction {
			continue
		}
		if _, ok := out[i].values[ValueKey(e.Value)]; !ok {
			out[i].values[ValueKey(e.Value)] = rank
		}
	}
	return out
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
	if !q.ObservedAfter.IsZero() && b.ObservedAt.Before(q.ObservedAfter) ||
		!q.ObservedBefore.IsZero() && b.ObservedAt.After(q.ObservedBefore) {
		return false
	}
	if !q.ValueAfter.IsZero() || !q.ValueBefore.IsZero() {
		v, _ := b.Value.(string)
		d, ok := parseDate(v)
		if !ok || !q.ValueAfter.IsZero() && d.Before(q.ValueAfter) || !q.ValueBefore.IsZero() && d.After(q.ValueBefore) {
			return false
		}
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

// searchEvents runs a semantic search over the event log: the vector search,
// fused with a lexical search when the database has one. The vector search
// stays in because it also reaches events too new for the text index.
func (s *Store) searchEvents(query string, filter map[string]any, limit int) ([]Event, error) {
	values, err := s.embed(query)
	if err != nil {
		return nil, err
	}
	hits, err := s.db.Search(s.collection, values, limit, filter)
	if err != nil {
		return nil, err
	}
	if ts, ok := s.db.(TextSearcher); ok {
		text, err := ts.SearchText(s.collection, query, limit, filter)
		switch {
		case errors.Is(err, ErrTextSearchUnsupported):
		case err != nil:
			return nil, err
		default:
			hits = fuseHits(hits, text, s.textFirst, limit)
		}
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

// rrfK is the reciprocal rank fusion constant, the usual 60.
const rrfK = 60

// fuseHits merges a vector and a lexical ranking and keeps the best limit.
// Fusion is reciprocal rank fusion, which rewards a hit both rankings found.
// With textFirst the lexical ranking leads and the vector hits it lacks follow
// in their own order: those are mostly events written since the text index
// last caught up, which only the vector search can reach.
func fuseHits(vector, text []Hit, textFirst bool, limit int) []Hit {
	byID := map[string]Hit{}
	var order []string
	if textFirst {
		for _, hits := range [][]Hit{text, vector} {
			for _, h := range hits {
				if _, seen := byID[h.ID]; !seen {
					byID[h.ID] = h
					order = append(order, h.ID)
				}
			}
		}
	} else {
		score := map[string]float64{}
		for _, hits := range [][]Hit{vector, text} {
			for rank, h := range hits {
				if _, seen := byID[h.ID]; !seen {
					byID[h.ID] = h
					order = append(order, h.ID)
				}
				score[h.ID] += 1 / float64(rrfK+rank+1)
			}
		}
		sort.SliceStable(order, func(i, j int) bool { return score[order[i]] > score[order[j]] })
	}
	if len(order) > limit {
		order = order[:limit]
	}
	out := make([]Hit, len(order))
	for i, id := range order {
		out[i] = byID[id]
	}
	return out
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
