package engine

import (
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// maxElsewhere bounds how many beliefs of one answer are checked for the same
// value under other predicates. Each check is one exact listing, so a broad
// read stays a bounded number of round trips.
const maxElsewhere = 10

// attachElsewhere adds to each of the first maxElsewhere typed beliefs the
// beliefs that hold, at asOf, the same value about the same subject under
// another predicate: likes mangoes beside dislikes mangoes. It is the
// read-time safety net for a correction nobody linked: the reader sees both
// and their dates, instead of one predicate's slot alone. Nothing is
// decided here; a belief keeps whatever the fold says.
func (s *Store) attachElsewhere(beliefs []Belief, asOf time.Time) []Belief {
	type key struct{ subject, value string }
	found := map[key][]Belief{}
	var checked []int
	var lookups []Belief
	for i, b := range beliefs {
		if len(checked) == maxElsewhere {
			break
		}
		if b.Predicate == NotePredicate || isReservedPair(pair{b.Subject, b.Predicate}) || b.Value == nil {
			continue
		}
		checked = append(checked, i)
		k := key{b.Subject, ValueKey(b.Value)}
		if _, ok := found[k]; !ok {
			found[k] = nil
			lookups = append(lookups, b)
		}
	}
	// Lookups run side by side, so an uncached read waits for about one
	// listing, not one per belief. A safety net must not break the read it
	// is attached to: a lookup that fails, such as on a backend that refuses
	// filters on unindexed fields, leaves its beliefs without it.
	results := make([][]Belief, len(lookups))
	var wg sync.WaitGroup
	for n, b := range lookups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[n], _ = s.sameValue(b.Subject, b.Value, asOf)
		}()
	}
	wg.Wait()
	for n, b := range lookups {
		found[key{b.Subject, ValueKey(b.Value)}] = results[n]
	}
	for _, i := range checked {
		b := beliefs[i]
		for _, o := range found[key{b.Subject, ValueKey(b.Value)}] {
			if o.Predicate == b.Predicate {
				continue
			}
			p := priorOf(o)
			p.Predicate = o.Predicate
			beliefs[i].Elsewhere = append(beliefs[i].Elsewhere, p)
		}
	}
	return beliefs
}

// sameValue lists the beliefs about subject that hold value at asOf, under
// any typed predicate.
func (s *Store) sameValue(subject string, value any, asOf time.Time) ([]Belief, error) {
	pairs, err := s.pairsHolding(subject, value)
	if err != nil {
		return nil, err
	}
	var out []Belief
	for _, p := range pairs {
		held, err := s.pairBeliefs(p, asOf)
		if err != nil {
			return nil, err
		}
		for _, b := range held {
			if ValueKey(b.Value) == ValueKey(value) {
				out = append(out, b)
			}
		}
	}
	return out, nil
}

// listPairsHolding lists the typed pairs of subject with any event holding
// value, whatever its capitalization. Events record value_key, the value's
// identity; events written before it existed are found by their stored value,
// tried as given, in lower case, and capitalized.
func (s *Store) listPairsHolding(subject string, value any) ([]pair, error) {
	filters := []map[string]any{{"subject": subject, "value_key": ValueKey(value)}}
	if v, ok := value.(string); ok {
		lower := strings.ToLower(v)
		first, size := utf8.DecodeRuneInString(lower)
		variants := []any{v}
		for _, w := range []string{lower, string(unicode.ToUpper(first)) + lower[size:]} {
			if w != v && (len(variants) < 2 || w != variants[1]) {
				variants = append(variants, w)
			}
		}
		filters = append(filters, map[string]any{"subject": subject, "value": map[string]any{"$in": variants}})
	} else {
		filters = append(filters, map[string]any{"subject": subject, "value": value})
	}
	var events []Event
	for _, f := range filters {
		part, err := s.eventsMatching(f, 200)
		if nothingYet(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		events = append(events, part...)
	}
	var out []pair
	for _, p := range dedupePairs(events) {
		if p.predicate != NotePredicate && !isReservedPair(p) {
			out = append(out, p)
		}
	}
	return out, nil
}
