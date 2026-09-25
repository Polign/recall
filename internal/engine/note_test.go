package engine

import (
	"strings"
	"testing"
	"time"
)

func TestNoteIsAlwaysRegistered(t *testing.T) {
	custom := Registry{"likes": {Cardinality: "multi", ValueType: "string"}}
	c, err := NewClient(Config{Backend: newLockedBackend(), Collection: "m", Registry: custom, Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Registry()[NotePredicate]; !ok {
		t.Fatal("a custom registry must still accept notes")
	}
	if _, ok := custom[NotePredicate]; ok {
		t.Fatal("registering note mutated the caller's registry")
	}
	s := NewStore(newFakeDB(), "m", custom, func(string) []float32 { return []float32{1} })
	if _, err := s.Remember("fact", "user", NotePredicate, "met Priya at the offsite", 1, ""); err != nil {
		t.Fatalf("store note: %v", err)
	}

	// A registry may describe note in its own words, but not change what it is.
	own := Registry{NotePredicate: {Cardinality: "multi", Description: "anything else"}}
	c, err = NewClient(Config{Backend: newLockedBackend(), Collection: "m", Registry: own})
	if err != nil || c.Registry()[NotePredicate].Description != "anything else" {
		t.Fatalf("own note description: %v", err)
	}
	for _, bad := range []Predicate{{Cardinality: "single", ValueType: "string"}, {Cardinality: "multi", ValueType: "number"}} {
		r := Registry{NotePredicate: bad}
		if _, err := NewClient(Config{Backend: newLockedBackend(), Collection: "m", Registry: r}); err == nil {
			t.Fatalf("client accepted note as %+v", bad)
		}
		s := NewStore(newFakeDB(), "m", r, func(string) []float32 { return []float32{1} })
		if _, err := s.Remember("fact", "user", NotePredicate, "x", 1, ""); err == nil {
			t.Fatalf("store accepted note as %+v", bad)
		}
	}
}

func TestUnknownPredicateErrorPointsAtNote(t *testing.T) {
	s, _, _ := newStore(t)
	_, err := s.Remember("fact", "user", "prefers_shell", "fish", 1, "")
	if err == nil || !strings.Contains(err.Error(), `predicate "note"`) {
		t.Fatalf("error must name the fallback: %v", err)
	}
}

func TestNotesRankAfterTypedBeliefs(t *testing.T) {
	s, _, clock := newStore(t)
	if _, err := s.Remember("fact", "user", NotePredicate, "prefers editor neovim for go work", 0.5, "agent_inferred"); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Second)
	mustRemember(t, s, "prefers_editor", "neovim")
	for _, q := range []Query{{Text: "editor neovim", Subject: "user"}, {Subject: "user"}} {
		got, err := s.Recall(q)
		if err != nil || len(got) != 2 {
			t.Fatalf("%+v: %+v %v", q, got, err)
		}
		if got[0].Predicate != "prefers_editor" || got[1].Predicate != NotePredicate {
			t.Fatalf("%+v: a note ranked ahead of a typed belief: %+v", q, got)
		}
	}
}

func TestPromoteFilesNoteAndWithdrawsIt(t *testing.T) {
	r := DefaultRegistry()
	c, err := NewClient(Config{Backend: newLockedBackend(), Collection: "m", Registry: r, Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	note := "I use fish as my shell."
	if _, err := c.Remember(t.Context(), RememberRequest{Subject: "user", Predicate: NotePredicate, Value: note}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Promote(t.Context(), PromoteRequest{Subject: "user", Note: note, Predicate: NotePredicate, Value: "x"}); err == nil {
		t.Fatal("promoting a note into note accepted")
	}
	if _, err := c.Promote(t.Context(), PromoteRequest{Subject: "user", Note: "never said", Predicate: "uses_technology", Value: "fish"}); err == nil {
		t.Fatal("promoted a note that does not exist")
	}
	res, err := c.Promote(t.Context(), PromoteRequest{Subject: "User", Note: " " + note, Predicate: "uses_technology", Value: "fish"})
	if err != nil || res.Stored.Value != "fish" {
		t.Fatalf("promote: %+v %v", res, err)
	}
	if got, _ := c.Recall(t.Context(), Query{Subject: "user", Predicate: NotePredicate}); len(got) != 0 {
		t.Fatalf("note still held after promotion: %+v", got)
	}
	if got, _ := c.Recall(t.Context(), Query{Subject: "user", Predicate: "uses_technology"}); len(got) != 1 {
		t.Fatalf("typed fact missing: %+v", got)
	}
	h, err := c.History(t.Context(), "user", NotePredicate)
	if err != nil || len(h) != 2 || !h[1].Retraction {
		t.Fatalf("the note must stay in history: %+v %v", h, err)
	}
}
