// Package recall is the typed agent-memory layer: a closed registry of
// predicates, an append-only event log per subject, and the fold that turns
// that log into what the agent believes, now or at any past instant.
//
// The central decision is that a belief is derived, never mutated. An earlier
// design flipped a record's status to "superseded" in a second write after
// storing its replacement, which left two live beliefs for a single-valued
// predicate whenever a process died between the two. Here the log is the
// truth and Fold deterministically interprets the events visible to a reader.
// This does not serialize concurrent writers or establish a read snapshot.
// Current and historical beliefs use the same fold with a different ceiling.
//
// Retraction is likewise an event, not a deletion. Forgetting a fact on
// Wednesday must not change what the agent believed on Tuesday, and a log
// that erases its own history cannot answer that.
package recall

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

	switch card {
	case Multi:
		return foldMulti(ordered)
	default:
		return foldSingle(ordered)
	}
}

// foldSingle tracks the currently held value. A targeted retraction clears
// only that value; withdrawing a superseded value leaves the newer assertion
// intact. Clearing the current value never revives a superseded assertion.
func foldSingle(ordered []Event) []Belief {
	var current Event
	held := false
	for _, e := range ordered {
		if !e.Retraction {
			current, held = e, true
		} else if held && (e.Value == nil || ValueKey(e.Value) == ValueKey(current.Value)) {
			held = false
		}
	}
	if held {
		return []Belief{beliefOf(current)}
	}
	return nil
}

// foldMulti accumulates values, where a retraction removes one value or, with
// no value, all of them.
func foldMulti(ordered []Event) []Belief {
	held := map[string]Belief{}
	var order []string
	for _, e := range ordered {
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
		if _, seen := held[k]; !seen {
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
		held[k] = beliefOf(e)
	}
	out := make([]Belief, 0, len(held))
	for _, k := range order {
		if b, ok := held[k]; ok {
			out = append(out, b)
		}
	}
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
	}
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
