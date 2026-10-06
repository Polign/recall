package engine

import (
	"testing"
	"time"
)

func TestCurrentBeliefCarriesWhatItReplaced(t *testing.T) {
	got := Fold([]Event{
		assert("e1", "vim", at(0)),
		assert("e2", "emacs", at(time.Hour)),
		assert("e3", "neovim", at(2*time.Hour)),
	}, Single, time.Time{})
	if len(got) != 1 || got[0].Value != "neovim" {
		t.Fatalf("got %v, want neovim", values(got))
	}
	// One step back only: emacs, not vim. The rest is in the history.
	r := got[0].Replaced
	if len(r) != 1 || r[0].Value != "emacs" || r[0].EventID != "e2" ||
		!r[0].ObservedAt.Equal(at(time.Hour)) || r[0].Source != "user_stated" {
		t.Fatalf("replaced = %+v, want emacs from e2", r)
	}
}

func TestFirstBeliefReplacedNothing(t *testing.T) {
	got := Fold([]Event{assert("e1", "vim", at(0))}, Single, time.Time{})
	if len(got[0].Replaced) != 0 {
		t.Fatalf("replaced = %+v, want none", got[0].Replaced)
	}
}

func TestAsOfShowsTheReplacementThenInForce(t *testing.T) {
	log := []Event{
		assert("e1", "vim", at(0)),
		assert("e2", "emacs", at(time.Hour)),
		assert("e3", "neovim", at(2*time.Hour)),
	}
	got := Fold(log, Single, at(90*time.Minute))
	if len(got) != 1 || got[0].Value != "emacs" || len(got[0].Replaced) != 1 || got[0].Replaced[0].Value != "vim" {
		t.Fatalf("got %+v, want emacs replacing vim", got)
	}
}

func TestRestatingKeepsTheCorrection(t *testing.T) {
	// Hearing the current value again is not a new correction, and must not
	// hide the one it was.
	got := Fold([]Event{
		assert("e1", "vim", at(0)),
		assert("e2", "neovim", at(time.Hour)),
		assert("e3", "neovim", at(2*time.Hour)),
	}, Single, time.Time{})
	if len(got) != 1 || len(got[0].Replaced) != 1 || got[0].Replaced[0].Value != "vim" {
		t.Fatalf("got %+v, want neovim still replacing vim", got)
	}
}

func TestReassertionAfterRetractionReplacedNothing(t *testing.T) {
	// A withdrawn value was not displaced by the next assertion; the
	// retraction had already removed it.
	got := Fold([]Event{
		assert("e1", "vim", at(0)),
		retract("e2", nil, at(time.Hour)),
		assert("e3", "neovim", at(2*time.Hour)),
	}, Single, time.Time{})
	if len(got) != 1 || len(got[0].Replaced) != 0 {
		t.Fatalf("got %+v, want neovim replacing nothing", got)
	}
}

func TestMultiValuedReplacesNothing(t *testing.T) {
	got := Fold([]Event{
		assert("e1", "go", at(0)),
		assert("e2", "rust", at(time.Hour)),
	}, Multi, time.Time{})
	for _, b := range got {
		if len(b.Replaced) != 0 {
			t.Fatalf("%v replaced %+v, want nothing", b.Value, b.Replaced)
		}
	}
}
