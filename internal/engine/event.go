package engine

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Event is one durable statement about a subject, or the retraction of one.
// Events are immutable once written: a correction is a later event, never an
// edit of an earlier one.
type Event struct {
	// ID is derived from subject, predicate, value identity, retraction, and
	// observation time. It is not a request idempotency key; confidence,
	// source, kind, and string capitalization do not participate in the ID.
	ID string `json:"id"`
	// Kind is "fact" or "preference".
	Kind string `json:"kind"`
	// Subject is who or what the statement is about, lowercased.
	Subject string `json:"subject"`
	// Predicate names the relation, and selects the cardinality that decides
	// how this event interacts with its neighbours.
	Predicate string `json:"predicate"`
	// Value holds the predicate's declared type: string, float64, or bool.
	// It is nil on a retraction that clears every value for the pair.
	Value any `json:"value"`
	// Confidence is in [0, 1].
	Confidence float64 `json:"confidence"`
	// Source is how the statement was come by: user_stated, agent_inferred,
	// or tool_result.
	Source string `json:"source"`
	// Retraction marks this event as withdrawing a belief rather than
	// asserting one. With Value set it withdraws that one value; with Value
	// nil it withdraws every value for the pair.
	Retraction bool `json:"retraction,omitempty"`
	// ObservedAt is the writer's observation time, not database acceptance
	// order. Fold orders by this instant and then ID; skewed writer clocks
	// can place a later accepted write earlier in the log.
	ObservedAt time.Time `json:"observed_at"`
	// Evidence is the exact excerpt a statement was drawn from, when it came
	// from text. EvidenceID is the event that holds that text whole, a note
	// written by RememberText. Both are empty for a statement made directly.
	Evidence   string `json:"evidence,omitempty"`
	EvidenceID string `json:"evidence_id,omitempty"`
	// Replaces, on an assertion, records the beliefs under other predicates
	// (or other values) that this statement made untrue when it was made:
	// "dislikes mangoes" replacing "likes mangoes". Each one was withdrawn by
	// a retraction in its own history whose ReplacedBy names this event, so
	// every history still folds on its own; this snapshot is what lets the
	// new belief say what it corrected.
	Replaces []PriorValue `json:"replaces,omitempty"`
	// ReplacedBy, on a retraction, is the event whose statement withdrew the
	// value: a correction, not a request to forget.
	ReplacedBy string `json:"replaced_by,omitempty"`
}

// Belief is a statement that holds at some instant: the result of folding a
// log, not a stored row.
type Belief struct {
	Subject    string  `json:"subject"`
	Predicate  string  `json:"predicate"`
	Value      any     `json:"value"`
	Confidence float64 `json:"confidence"`
	Source     string  `json:"source"`
	Kind       string  `json:"kind"`
	// ObservedAt is when the surviving event was observed, which is what
	// "since when has this been true" means.
	ObservedAt time.Time `json:"observed_at"`
	// EventID identifies the event this belief came from, so a caller can
	// fetch its full history or cite it.
	EventID string `json:"event_id"`
	// Evidence and EvidenceID are the surviving event's: the excerpt this
	// belief was drawn from and the event holding the whole text.
	Evidence   string `json:"evidence,omitempty"`
	EvidenceID string `json:"evidence_id,omitempty"`
	// Via is set on a belief that Query.FollowRefs added: the event of the
	// belief whose ref value led to this one's subject.
	Via string `json:"via,omitempty"`
	// Replaced lists the values this belief displaced when it was asserted,
	// so a reader sees the correction alongside the answer: what was believed
	// before, when, and on what basis. It goes back one step; the whole chain
	// is in the history. Empty when the belief displaced nothing, which is
	// always the case for a multi-valued predicate.
	Replaced []PriorValue `json:"replaced,omitempty"`
	// Elsewhere lists the subject's current beliefs under other predicates
	// that hold the same value, each with its predicate and when it was
	// stated: dislikes mangoes beside likes mangoes. A correction that was
	// never linked shows up here, so a reader is not answered from one
	// predicate alone. Client reads fill it unless Query.NoElsewhere is set.
	Elsewhere []PriorValue `json:"elsewhere,omitempty"`
}

// PriorValue is a value that a newer assertion displaced.
type PriorValue struct {
	// Predicate is set when the displaced value was held under another
	// predicate than this one: likes mangoes, replaced by dislikes mangoes.
	Predicate  string    `json:"predicate,omitempty"`
	Value      any       `json:"value"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
	EventID    string    `json:"event_id"`
}

// Cardinality decides what a second value for the same subject and predicate
// means.
type Cardinality string

const (
	// Single means a newer value replaces the older one: an agent has one
	// preferred editor.
	Single Cardinality = "single"
	// Multi means values accumulate: an agent may like many languages.
	Multi Cardinality = "multi"
)

// Fold reduces one subject-and-predicate's events to the beliefs that hold at
// asOf. A zero asOf means now.
//
// Events may arrive in any order and may repeat; the fold is defined by the
// log's content alone, so a partially applied write, a retried write, or a
// clock that stepped backwards changes the answer only in so far as it
// changed the log.
func Fold(events []Event, card Cardinality, asOf time.Time) []Belief {
	return foldWith(events, func(time.Time) Cardinality { return card }, asOf)
}

// foldWith is Fold for a predicate whose cardinality has changed: cardAt
// reports the cardinality in effect at an instant, and each assertion is
// applied under the one in effect when it was observed. A question about an
// instant before a change is therefore answered the way it was at the time.
//
// Under single an assertion replaces whatever is held; under multi it is
// added. A predicate that went from multi to single keeps the values it held
// until the next assertion replaces them, and one that went from single to
// multi keeps its value and accumulates from there.
func foldWith(events []Event, cardAt func(time.Time) Cardinality, asOf time.Time) []Belief {
	return foldOpposed(events, cardAt, nil, asOf)
}

// opposed describes the predicates that exclude the one being folded, whose
// events are mixed into the log handed to foldOpposed.
type opposed struct {
	// by names the excluding predicate an event belongs to, or "" for an
	// event of the predicate being folded.
	by func(Event) string
	// cardAt is an excluding predicate's cardinality at an instant.
	cardAt func(name string, at time.Time) Cardinality
	// mutual reports whether assertions here withdraw the excluding
	// predicate's values in turn. They do not when it is historical: owning
	// the piano again does not unwrite having given it away.
	mutual func(name string) bool
}

// foldOpposed is foldWith for a predicate that other predicates exclude. An
// assertion of a value under an excluding predicate withdraws that value
// here, exactly like a targeted retraction, and an assertion here reports the
// excluded value it displaced in Replaced. Each excluding predicate is folded
// alongside, under its own cardinality and retractions, only to know what it
// held at each instant; its beliefs are not returned. Withdrawing a value on
// the excluding side never revives one here: what was displaced stays
// displaced until it is stated again.
func foldOpposed(events []Event, cardAt func(time.Time) Cardinality, opp *opposed, asOf time.Time) []Belief {
	if asOf.IsZero() {
		asOf = time.Now()
	}
	ordered := make([]Event, 0, len(events))
	for _, e := range events {
		// An event with no instant cannot be placed in the log at all. Keeping
		// it would sort it ahead of every real event and let one malformed
		// record invent a belief.
		if e.ObservedAt.IsZero() {
			continue
		}
		// An event observed after the ceiling has not happened yet from this
		// query's point of view, which is the whole of time travel.
		if e.ObservedAt.After(asOf) {
			continue
		}
		ordered = append(ordered, e)
	}
	// Ties are broken so that two events sharing an instant fold the same way
	// on every node and every replay.
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].ObservedAt.Equal(ordered[j].ObservedAt) {
			return ordered[i].ObservedAt.Before(ordered[j].ObservedAt)
		}
		// A retraction cannot precede the assertion it withdraws. Ordering on
		// id alone is a hash coin flip, and stored instants are not always
		// fine-grained: parseTime accepts whole-second RFC3339 for imported and
		// hand-written logs, which makes a tie ordinary rather than exotic.
		// Losing that coin flip drops the retraction and revives the belief.
		if ordered[i].Retraction != ordered[j].Retraction {
			return ordered[j].Retraction
		}
		return ordered[i].ID < ordered[j].ID
	})

	held := map[string]Belief{}
	var order []string
	// against is what each excluding predicate holds, by name and value key.
	against := map[string]map[string]Belief{}
	for _, e := range ordered {
		if name := opposedName(opp, e); name != "" {
			theirs := against[name]
			if theirs == nil {
				theirs = map[string]Belief{}
				against[name] = theirs
			}
			switch {
			case e.Retraction && e.Value == nil:
				clear(theirs)
			case e.Retraction:
				delete(theirs, ValueKey(e.Value))
			default:
				k := ValueKey(e.Value)
				if _, ok := held[k]; ok {
					delete(held, k)
					order = slices.DeleteFunc(order, func(v string) bool { return v == k })
				}
				if opp.cardAt(name, e.ObservedAt) != Multi {
					clear(theirs)
				}
				b := beliefOf(e)
				b.Predicate = name
				theirs[k] = b
			}
			continue
		}
		// A retraction removes one value or, with no value, all of them. A
		// targeted retraction of a value that is not held changes nothing, so
		// withdrawing a superseded value leaves the newer assertion intact,
		// and clearing the current value never revives a superseded one.
		if e.Retraction && e.Value == nil {
			held = map[string]Belief{}
			order = order[:0]
			continue
		}
		k := ValueKey(e.Value)
		if e.Retraction {
			delete(held, k)
			order = slices.DeleteFunc(order, func(v string) bool { return v == k })
			continue
		}
		b := beliefOf(e)
		// Restating a held value keeps what that value had replaced: hearing
		// it again does not undo the correction it was.
		prev, seen := held[k]
		if seen {
			b.Replaced = prev.Replaced
		}
		if cardAt(e.ObservedAt) != Multi {
			var replaced []PriorValue
			for _, hk := range order {
				if hk != k {
					replaced = append(replaced, priorOf(held[hk]))
				}
			}
			if len(replaced) > 0 {
				b.Replaced = replaced
			}
			held = map[string]Belief{}
			order = order[:0]
			seen = false
		}
		for _, name := range sortedKeys(against) {
			if was, ok := against[name][k]; ok && opp.mutual(name) {
				prior := priorOf(was)
				prior.Predicate = name
				b.Replaced = append(append([]PriorValue(nil), b.Replaced...), prior)
				delete(against[name], k)
			}
		}
		// A correction across predicates says what it corrected, beside what
		// it displaced under its own.
		if len(e.Replaces) > 0 {
			b.Replaced = append(append([]PriorValue(nil), b.Replaced...), e.Replaces...)
		}
		if !seen {
			order = append(order, k)
		}
		// A repeated assertion refreshes confidence, source, and instant:
		// hearing the same fact again is new evidence for it.
		//
		// Remember will not produce one: it folds first and returns early when
		// the value is already held, so restating a belief writes nothing and
		// cannot raise its confidence. This branch therefore serves imported
		// logs, a re-assertion after a retraction, and any writer appending
		// events directly. Raising confidence through Remember would be a
		// change to that idempotence rule, not to this fold.
		held[k] = b
	}
	out := make([]Belief, 0, len(held))
	for _, k := range order {
		if b, ok := held[k]; ok {
			out = append(out, b)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func opposedName(opp *opposed, e Event) string {
	if opp == nil {
		return ""
	}
	return opp.by(e)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func beliefOf(e Event) Belief {
	return Belief{
		Subject:    e.Subject,
		Predicate:  e.Predicate,
		Value:      e.Value,
		Confidence: e.Confidence,
		Source:     e.Source,
		Kind:       e.Kind,
		ObservedAt: e.ObservedAt,
		EventID:    e.ID,
		Evidence:   e.Evidence,
		EvidenceID: e.EvidenceID,
	}
}

func priorOf(b Belief) PriorValue {
	return PriorValue{Value: b.Value, Source: b.Source, ObservedAt: b.ObservedAt, EventID: b.EventID}
}

// ValueKey renders a value in the canonical form used to tell two values
// apart. Strings fold case, so "Neovim" stated twice with different capitals
// is one belief rather than two; the event keeps whatever form was written,
// and only identity is case-insensitive. Numbers render without exponent
// drift so that 8000 and 8000.0 are one value rather than two.
func ValueKey(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return "s:" + strings.ToLower(t)
	case bool:
		return "b:" + strconv.FormatBool(t)
	case float64:
		return "n:" + strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return "n:" + strconv.FormatFloat(float64(t), 'f', -1, 64)
	default:
		return "x:" + fmt.Sprint(t)
	}
}
