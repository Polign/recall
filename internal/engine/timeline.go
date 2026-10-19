package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Step is one statement in a belief's timeline and what it did to what was
// believed: the correction told as a change, not just as an event.
type Step struct {
	At      time.Time `json:"at"`
	EventID string    `json:"event_id"`
	// Statement tells the event in words. A withdrawal starts "withdrew".
	Statement string `json:"statement"`
	// Predicate is the name the event was written under. It differs from the
	// timeline's own when a statement under an excluding predicate withdrew
	// one of its values: dislikes mangoes ending likes mangoes.
	Predicate string `json:"predicate"`
	Source    string `json:"source"`
	Evidence  string `json:"evidence,omitempty"`
	// Change is "stated" (a value began to be believed), "replaced" (one
	// value took another's place), "withdrawn" (a value stopped being
	// believed with nothing in its place), or "no change" (restated, or
	// dated before a newer statement).
	Change string `json:"change"`
	// Added and Removed are the beliefs this step began and ended, told as
	// sentences: the diff of what was believed just before and just after.
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
}

// Timeline is one subject and predicate's history told as changes, oldest
// first, with what is believed at the end of it.
type Timeline struct {
	Subject   string   `json:"subject"`
	Predicate string   `json:"predicate"`
	Now       []string `json:"now"`
	Steps     []Step   `json:"steps"`
}

// Timeline tells one subject and predicate's history as changes. Each step
// is a recorded statement with the beliefs it began and ended, found by
// folding the log just before and just after it, so the timeline says
// exactly what a read at any instant would have answered. Statements under
// an excluding predicate appear only when they changed this one.
func (s *Store) Timeline(subject, predicate string) (Timeline, error) {
	sc, err := s.view()
	if err != nil {
		return Timeline{}, err
	}
	name := sc.canonical(s.predicateName(predicate))
	own, all, err := s.foldHistory(sc, subject, name)
	if err != nil {
		return Timeline{}, err
	}
	isOwn := make(map[string]bool, len(own))
	for _, e := range own {
		isOwn[e.ID] = true
	}
	out := Timeline{Subject: normalizeSubject(subject), Predicate: name, Now: []string{}, Steps: []Step{}}
	held := map[string]string{}
	for i, e := range all {
		// Events sharing an instant fold together; tell them as one step
		// each, the change landing on the last.
		if i+1 < len(all) && all[i+1].ObservedAt.Equal(e.ObservedAt) {
			if isOwn[e.ID] {
				out.Steps = append(out.Steps, s.step(e, nil, nil))
			}
			continue
		}
		now := sentences(sc.fold(name, all, e.ObservedAt))
		var added, removed []string
		for k, text := range now {
			if _, ok := held[k]; !ok {
				added = append(added, text)
			}
		}
		for k, text := range held {
			if _, ok := now[k]; !ok {
				removed = append(removed, text)
			}
		}
		held = now
		if !isOwn[e.ID] && len(added)+len(removed) == 0 {
			continue
		}
		out.Steps = append(out.Steps, s.step(e, added, removed))
	}
	for _, text := range held {
		out.Now = append(out.Now, text)
	}
	sort.Strings(out.Now)
	return out, nil
}

func (s *Store) step(e Event, added, removed []string) Step {
	sort.Strings(added)
	sort.Strings(removed)
	st := Step{At: e.ObservedAt, EventID: e.ID, Predicate: e.Predicate, Source: e.Source, Evidence: e.Evidence, Added: added, Removed: removed}
	switch {
	case e.Retraction && e.Value == nil:
		st.Statement = "withdrew everything for " + e.Subject + " " + strings.ReplaceAll(e.Predicate, "_", " ")
	case e.Retraction:
		st.Statement = "withdrew " + Sentence(e.Subject, e.Predicate, e.Value)
	default:
		st.Statement = Sentence(e.Subject, e.Predicate, e.Value)
	}
	switch {
	case len(added) > 0 && len(removed) > 0:
		st.Change = "replaced"
	case len(added) > 0:
		st.Change = "stated"
	case len(removed) > 0:
		st.Change = "withdrawn"
	default:
		st.Change = "no change"
	}
	return st
}

// sentences keys beliefs by value identity, ignoring the capitalization the
// fold ignores, and tells each.
func sentences(beliefs []Belief) map[string]string {
	out := make(map[string]string, len(beliefs))
	for _, b := range beliefs {
		out[valueIdentity(b.Value)] = Sentence(b.Subject, b.Predicate, b.Value)
	}
	return out
}

func valueIdentity(v any) string {
	if s, ok := v.(string); ok {
		return "s:" + strings.ToLower(strings.TrimSpace(s))
	}
	return fmt.Sprintf("%T:%v", v, v)
}

// PairChange is how one subject and predicate's beliefs differ between two
// instants.
type PairChange struct {
	Subject   string   `json:"subject"`
	Predicate string   `json:"predicate"`
	Before    []string `json:"before"`
	After     []string `json:"after"`
	Added     []string `json:"added,omitempty"`
	Removed   []string `json:"removed,omitempty"`
}

// DiffRequest names two instants to compare.
type DiffRequest struct {
	// Subject limits the diff to one subject; empty compares every subject.
	Subject string
	// From is the earlier instant; zero means before anything was stated.
	From time.Time
	// To is the later instant; zero means now.
	To time.Time
}

// Diff says what was believed differently at To than at From: for every
// subject and predicate a statement touched in between, the beliefs at each
// end and which began and ended. Notes filed into statements are left out,
// as answers leave them out.
func (s *Store) Diff(r DiffRequest) ([]PairChange, error) {
	to := r.To
	if to.IsZero() {
		to = s.now().UTC()
	}
	if !r.From.IsZero() && !r.From.Before(to) {
		return nil, fmt.Errorf("recall: diff: from (%s) must be before to (%s)", r.From.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	sc, err := s.view()
	if err != nil {
		return nil, err
	}
	events, err := s.ExportEvents(Export{Subject: r.Subject})
	if err != nil {
		return nil, err
	}
	touched := map[pair]bool{}
	for _, e := range events {
		if e.ObservedAt.After(to) || !r.From.IsZero() && !e.ObservedAt.After(r.From) {
			continue
		}
		p := pair{normalizeSubject(e.Subject), sc.canonical(e.Predicate)}
		if isReservedPair(p) {
			continue
		}
		touched[p] = true
		// A statement under one predicate can withdraw the same value
		// under another that it excludes.
		for _, name := range sc.withdraws(p.predicate) {
			touched[pair{p.subject, name}] = true
		}
	}
	pairs := make([]pair, 0, len(touched))
	for p := range touched {
		pairs = append(pairs, p)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].subject != pairs[j].subject {
			return pairs[i].subject < pairs[j].subject
		}
		return pairs[i].predicate < pairs[j].predicate
	})
	var out []PairChange
	for _, p := range pairs {
		_, all, err := s.foldHistory(sc, p.subject, p.predicate)
		if err != nil {
			return nil, err
		}
		var before []Belief
		if !r.From.IsZero() {
			before = sc.fold(p.predicate, all, r.From)
		}
		after := sc.fold(p.predicate, all, to)
		if p.predicate == NotePredicate {
			if before, err = s.unfiled(before); err != nil {
				return nil, err
			}
			if after, err = s.unfiled(after); err != nil {
				return nil, err
			}
		}
		b, a := sentences(before), sentences(after)
		c := PairChange{Subject: p.subject, Predicate: p.predicate, Before: sortedValues(b), After: sortedValues(a)}
		for k, text := range a {
			if _, ok := b[k]; !ok {
				c.Added = append(c.Added, text)
			}
		}
		for k, text := range b {
			if _, ok := a[k]; !ok {
				c.Removed = append(c.Removed, text)
			}
		}
		if len(c.Added)+len(c.Removed) == 0 {
			continue
		}
		sort.Strings(c.Added)
		sort.Strings(c.Removed)
		out = append(out, c)
	}
	return out, nil
}

// unfiled drops notes whose statements were filed, which answers tell by
// those statements instead.
func (s *Store) unfiled(notes []Belief) ([]Belief, error) {
	out := notes[:0:0]
	for _, b := range notes {
		filed, err := s.episodeFiled(b.EventID)
		if err != nil {
			return nil, err
		}
		if !filed {
			out = append(out, b)
		}
	}
	return out, nil
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Timeline tells one subject and predicate's history as changes.
func (c *Client) Timeline(ctx context.Context, subject, predicate string) (Timeline, error) {
	s, err := c.forContext(ctx)
	if err != nil {
		return Timeline{}, err
	}
	out, err := s.Timeline(subject, predicate)
	if err == nil {
		err = ctx.Err()
	}
	return out, err
}

// TimelinesFor tells the timeline of each subject and predicate a question
// reaches, including ones whose every value has since been withdrawn, so
// "what happened to my editor" finds a forgotten editor too.
func (c *Client) TimelinesFor(ctx context.Context, question string) ([]Timeline, error) {
	if strings.TrimSpace(question) == "" {
		return nil, fmt.Errorf("recall: a question is required")
	}
	s, err := c.forContext(ctx)
	if err != nil {
		return nil, err
	}
	sc, err := s.view()
	if err != nil {
		return nil, err
	}
	cands, err := s.candidatePairs(sc, Query{Text: question}, timelineCandidates, time.Time{})
	if err != nil {
		return nil, err
	}
	var out []Timeline
	seen := map[pair]bool{}
	for _, cand := range cands {
		p := pair{normalizeSubject(cand.pair.subject), sc.canonical(cand.pair.predicate)}
		if seen[p] || isReservedPair(p) {
			continue
		}
		seen[p] = true
		t, err := s.Timeline(p.subject, p.predicate)
		if err != nil {
			return out, err
		}
		if p.predicate == NotePredicate || len(t.Steps) == 0 {
			continue
		}
		out = append(out, t)
	}
	return out, ctx.Err()
}

// timelineCandidates bounds how many subjects and predicates one question's
// timelines cover.
const timelineCandidates = 5

// Diff says what was believed differently at r.To than at r.From.
func (c *Client) Diff(ctx context.Context, r DiffRequest) ([]PairChange, error) {
	s, err := c.forContext(ctx)
	if err != nil {
		return nil, err
	}
	out, err := s.Diff(r)
	if err == nil {
		err = ctx.Err()
	}
	return out, err
}
