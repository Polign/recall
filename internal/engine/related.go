package engine

import "errors"

// relatedLimit bounds the related beliefs one write or one extraction is
// shown. It is a short list for a model to read, not a search result.
const relatedLimit = 8

// related lists the current beliefs about b's subject, under other
// predicates, that may contradict it: first those holding the same value
// (likes mangoes for dislikes mangoes), then those closest in meaning (likes
// steak for is vegetarian). Notes are left out: they are the record of what
// was said, which a correction never withdraws.
func (s *Store) related(b Belief, limit int) ([]Belief, error) {
	var out []Belief
	seen := map[string]bool{}
	add := func(c Belief) {
		if len(out) >= limit || seen[c.EventID] || c.Predicate == NotePredicate || isReservedPair(pair{c.Subject, c.Predicate}) {
			return
		}
		if c.Subject == b.Subject && c.Predicate == b.Predicate {
			return
		}
		seen[c.EventID] = true
		out = append(out, c)
	}

	same, err := s.sameValue(b.Subject, b.Value, s.now())
	if err != nil {
		return nil, err
	}
	for _, c := range same {
		add(c)
	}

	near, err := s.Recall(Query{Text: (Event{Subject: b.Subject, Predicate: b.Predicate, Value: b.Value}).Text(),
		Subject: b.Subject, Limit: limit * 2, NoTimeHint: true})
	if nothingYet(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, c := range near {
		add(c)
	}
	return out, nil
}

// nothingYet reports a backend saying the collection does not exist, which
// for a lookup of related beliefs means there are none.
func nothingYet(err error) bool {
	var nf interface{ NotFound() bool }
	return errors.As(err, &nf) && nf.NotFound()
}
