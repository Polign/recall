package recall

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

// assert builds an assertion event. The id encodes the instant so that ties
// are broken deterministically without the test having to care.
func assert(id string, v any, when time.Time) Event {
	return Event{
		ID: id, Kind: "preference", Subject: "user", Predicate: "prefers_editor",
		Value: v, Confidence: 1, Source: "user_stated", ObservedAt: when,
	}
}

func retract(id string, v any, when time.Time) Event {
	e := assert(id, v, when)
	e.Retraction = true
	return e
}

func values(bs []Belief) []any {
	out := make([]any, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Value)
	}
	return out
}

func TestSingleValuedLatestWins(t *testing.T) {
	got := Fold([]Event{
		assert("e1", "vim", at(0)),
		assert("e2", "neovim", at(time.Hour)),
	}, Single, time.Time{})
	if len(got) != 1 || got[0].Value != "neovim" {
		t.Fatalf("got %v, want neovim", values(got))
	}
}

func TestSingleValuedIgnoresLogOrder(t *testing.T) {
	// The same log shuffled must fold identically: the answer comes from
	// ObservedAt, not from the order rows came back in.
	shuffled := []Event{
		assert("e2", "neovim", at(time.Hour)),
		assert("e1", "vim", at(0)),
	}
	got := Fold(shuffled, Single, time.Time{})
	if len(got) != 1 || got[0].Value != "neovim" {
		t.Fatalf("got %v, want neovim", values(got))
	}
}

// The crash this design exists to survive: the replacement was stored and the
// process died before the old record could be marked superseded, so both
// look live. The fold still answers with one belief.
func TestSupersessionSurvivesAnUnmarkedPredecessor(t *testing.T) {
	got := Fold([]Event{
		assert("e1", "vim", at(0)),
		assert("e2", "neovim", at(time.Hour)),
	}, Single, time.Time{})
	if len(got) != 1 {
		t.Fatalf("got %d beliefs, want exactly 1: %v", len(got), values(got))
	}
	if got[0].Value != "neovim" || got[0].EventID != "e2" {
		t.Fatalf("got %+v, want the newer event", got[0])
	}
}

func TestDuplicateWriteIsIdempotentInTheFold(t *testing.T) {
	// A retried write that landed twice under different ids must not produce
	// two beliefs.
	got := Fold([]Event{
		assert("e1", "neovim", at(0)),
		assert("e2", "neovim", at(0)),
	}, Single, time.Time{})
	if len(got) != 1 || got[0].Value != "neovim" {
		t.Fatalf("got %v, want one neovim", values(got))
	}
}

func TestAsOfSeesThePastBelief(t *testing.T) {
	log := []Event{
		assert("e1", "vim", at(0)),
		assert("e2", "neovim", at(48*time.Hour)),
	}
	if got := Fold(log, Single, at(24*time.Hour)); len(got) != 1 || got[0].Value != "vim" {
		t.Fatalf("as of Tuesday got %v, want vim", values(got))
	}
	if got := Fold(log, Single, at(72*time.Hour)); len(got) != 1 || got[0].Value != "neovim" {
		t.Fatalf("as of Thursday got %v, want neovim", values(got))
	}
}

func TestAsOfBeforeAnythingKnownIsEmpty(t *testing.T) {
	got := Fold([]Event{assert("e1", "vim", at(time.Hour))}, Single, at(0))
	if len(got) != 0 {
		t.Fatalf("got %v, want no belief", values(got))
	}
}

// Forgetting on Wednesday must not change what was believed on Tuesday.
// A design that deleted rows could not answer this at all.
func TestRetractionDoesNotRewriteHistory(t *testing.T) {
	log := []Event{
		assert("e1", "vim", at(0)),
		retract("e2", "vim", at(48*time.Hour)),
	}
	if got := Fold(log, Single, at(24*time.Hour)); len(got) != 1 || got[0].Value != "vim" {
		t.Fatalf("before the retraction got %v, want vim", values(got))
	}
	if got := Fold(log, Single, time.Time{}); len(got) != 0 {
		t.Fatalf("after the retraction got %v, want nothing", values(got))
	}
}

func TestReassertionAfterRetraction(t *testing.T) {
	got := Fold([]Event{
		assert("e1", "vim", at(0)),
		retract("e2", "vim", at(time.Hour)),
		assert("e3", "vim", at(2*time.Hour)),
	}, Single, time.Time{})
	if len(got) != 1 || got[0].Value != "vim" {
		t.Fatalf("got %v, want vim believed again", values(got))
	}
}

func TestMultiValuedAccumulatesInOrder(t *testing.T) {
	got := Fold([]Event{
		assert("e1", "go", at(0)),
		assert("e2", "rust", at(time.Hour)),
		assert("e3", "zig", at(2*time.Hour)),
	}, Multi, time.Time{})
	if len(got) != 3 {
		t.Fatalf("got %v, want three", values(got))
	}
	for i, want := range []any{"go", "rust", "zig"} {
		if got[i].Value != want {
			t.Fatalf("position %d = %v, want %v", i, got[i].Value, want)
		}
	}
}

func TestMultiValuedRepeatRefreshesRatherThanDuplicates(t *testing.T) {
	got := Fold([]Event{
		assert("e1", "go", at(0)),
		assert("e2", "go", at(time.Hour)),
	}, Multi, time.Time{})
	if len(got) != 1 {
		t.Fatalf("got %v, want one", values(got))
	}
	if !got[0].ObservedAt.Equal(at(time.Hour)) {
		t.Fatalf("observed at %v, want the later instant", got[0].ObservedAt)
	}
}

func TestMultiValuedRetractionRemovesOneValue(t *testing.T) {
	got := Fold([]Event{
		assert("e1", "go", at(0)),
		assert("e2", "rust", at(time.Hour)),
		retract("e3", "go", at(2*time.Hour)),
	}, Multi, time.Time{})
	if len(got) != 1 || got[0].Value != "rust" {
		t.Fatalf("got %v, want only rust", values(got))
	}
}

func TestMultiValuedBlanketRetractionClearsAll(t *testing.T) {
	got := Fold([]Event{
		assert("e1", "go", at(0)),
		assert("e2", "rust", at(time.Hour)),
		retract("e3", nil, at(2*time.Hour)),
	}, Multi, time.Time{})
	if len(got) != 0 {
		t.Fatalf("got %v, want nothing", values(got))
	}
}

func TestBlanketRetractionThenReassertion(t *testing.T) {
	got := Fold([]Event{
		assert("e1", "go", at(0)),
		retract("e2", nil, at(time.Hour)),
		assert("e3", "rust", at(2*time.Hour)),
	}, Multi, time.Time{})
	if len(got) != 1 || got[0].Value != "rust" {
		t.Fatalf("got %v, want only rust", values(got))
	}
}

func TestTiesBreakDeterministically(t *testing.T) {
	// Two events at the same instant: whichever the fold picks, it must pick
	// the same one every time and regardless of input order.
	a := []Event{assert("e1", "vim", at(0)), assert("e2", "neovim", at(0))}
	b := []Event{assert("e2", "neovim", at(0)), assert("e1", "vim", at(0))}
	first, second := Fold(a, Single, time.Time{}), Fold(b, Single, time.Time{})
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("got %v and %v", values(first), values(second))
	}
	if first[0].Value != second[0].Value {
		t.Fatalf("order changed the answer: %v vs %v", first[0].Value, second[0].Value)
	}
}

func TestEmptyLogIsNoBelief(t *testing.T) {
	if got := Fold(nil, Single, time.Time{}); len(got) != 0 {
		t.Fatalf("got %v", values(got))
	}
	if got := Fold(nil, Multi, time.Time{}); len(got) != 0 {
		t.Fatalf("got %v", values(got))
	}
}

func TestValueKeyDistinguishesTypesAndUnifiesNumbers(t *testing.T) {
	if ValueKey("8000") == ValueKey(float64(8000)) {
		t.Error("a string and a number collided")
	}
	if ValueKey(float64(8000)) != ValueKey(8000) {
		t.Error("int and float forms of one number disagreed")
	}
	if ValueKey(true) == ValueKey("true") {
		t.Error("a bool and a string collided")
	}
}

func TestValueKeyFoldsCaseSoOneBeliefNotTwo(t *testing.T) {
	if ValueKey("Neovim") != ValueKey("neovim") {
		t.Fatal("the same statement in different capitals split into two values")
	}
	got := Fold([]Event{
		assert("e1", "Neovim", at(0)),
		assert("e2", "neovim", at(time.Hour)),
	}, Multi, time.Time{})
	if len(got) != 1 {
		t.Fatalf("got %v, want one belief", values(got))
	}
	// The event's own spelling survives: identity folds case, display does not.
	if got[0].Value != "neovim" {
		t.Fatalf("value = %v, want the later spelling", got[0].Value)
	}
}

func TestUndatedEventCannotInventABelief(t *testing.T) {
	// A record whose observed_at failed to parse has a zero instant. It must
	// not sort ahead of the log and become the answer.
	undated := assert("e0", "emacs", time.Time{})
	got := Fold([]Event{undated, assert("e1", "neovim", at(0))}, Single, time.Time{})
	if len(got) != 1 || got[0].Value != "neovim" {
		t.Fatalf("got %v, want neovim", values(got))
	}
	if got := Fold([]Event{undated}, Multi, time.Time{}); len(got) != 0 {
		t.Fatalf("got %v, want nothing", values(got))
	}
}
