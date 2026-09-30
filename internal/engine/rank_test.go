package engine

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// newRankedStore is newStore with a lexical embedder and a fake index that
// ranks by similarity, so a test can tell which events a query matched.
func newRankedStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s, db, clock := newStore(t)
	db.bySimilarity = true
	s.embed = func(text string) ([]float32, error) {
		return LexicalEmbedder{}.Embed(context.Background(), text)
	}
	return s, clock
}

func rememberAt(t *testing.T, s *Store, clock *time.Time, at time.Duration, predicate, value string) {
	t.Helper()
	*clock = t0.Add(at)
	if _, err := s.Remember("fact", "user", predicate, value, 1, "user_stated"); err != nil {
		t.Fatalf("Remember(%s, %q): %v", predicate, value, err)
	}
}

// Every note shares one subject and predicate. Recall used to rank pairs and
// then return the pair's values in the order they were written, so any query
// that touched a note came back with the oldest notes whatever it asked.
func TestSemanticRecallRanksNotesByTheQuery(t *testing.T) {
	s, clock := newRankedStore(t)
	for i := range 40 {
		rememberAt(t, s, clock, time.Duration(i)*time.Minute, NotePredicate,
			fmt.Sprintf("filler conversation %d about cooking weather and travel plans", i))
	}
	rememberAt(t, s, clock, time.Hour, NotePredicate, "my apex legends goal is reaching diamond rank")
	for i := range 10 {
		rememberAt(t, s, clock, 2*time.Hour+time.Duration(i)*time.Minute, NotePredicate,
			fmt.Sprintf("later filler %d about gardening", i))
	}

	for _, q := range []Query{
		{Text: "what was my apex legends rank goal", Limit: 5},
		{Text: "what was my apex legends rank goal", Subject: "user", Limit: 5},
	} {
		got := recallValues(t, s, q)
		if len(got) == 0 || got[0] != "my apex legends goal is reaching diamond rank" {
			t.Fatalf("%+v: got %v, want the apex legends note first", q, got)
		}
		if len(got) > 5 {
			t.Fatalf("%+v: got %d beliefs, want at most 5", q, len(got))
		}
	}

	// Two different questions must not get the same answer.
	a := recallValues(t, s, Query{Text: "gardening", Limit: 3})
	b := recallValues(t, s, Query{Text: "cooking weather", Limit: 3})
	if fmt.Sprint(a) == fmt.Sprint(b) {
		t.Fatalf("unrelated queries returned the same beliefs: %v", a)
	}
}

// A multi-valued pair returns the values the query matched, best first, and
// leaves out values whose events it never reached.
func TestSemanticRecallReturnsOnlyMatchedValuesOfAMultiPair(t *testing.T) {
	s, clock := newRankedStore(t)
	for i := range 30 {
		rememberAt(t, s, clock, time.Duration(i)*time.Minute, "likes", fmt.Sprintf("board game number %d", i))
	}
	rememberAt(t, s, clock, time.Hour, "likes", "sourdough bread baking")

	// Sourdough was written last, so returning the pair in fold order would
	// have left it off a page of three.
	got := recallValues(t, s, Query{Text: "sourdough bread", Limit: 3})
	if len(got) == 0 || got[0] != "sourdough bread baking" {
		t.Fatalf("got %v, want sourdough first", got)
	}

	// Naming the pair asks for all of it; the query text does not filter it.
	all := recallValues(t, s, Query{Text: "sourdough bread", Subject: "user", Predicate: "likes", Limit: 100})
	if len(all) != 31 {
		t.Fatalf("explicit pair returned %d values, want all 31", len(all))
	}
}

// A single-valued pair ranks by its best event, and a hit on a superseded
// value still answers with the value that holds now.
func TestSemanticRecallAnswersASupersededHitWithTheCurrentValue(t *testing.T) {
	s, clock := newRankedStore(t)
	rememberAt(t, s, clock, 0, "prefers_editor", "vim")
	rememberAt(t, s, clock, time.Hour, "prefers_editor", "emacs")
	for i := range 30 {
		rememberAt(t, s, clock, 2*time.Hour+time.Duration(i)*time.Minute, NotePredicate, fmt.Sprintf("unrelated note %d", i))
	}

	got, err := s.Recall(Query{Text: "vim", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Predicate != "prefers_editor" || got[0].Value != "emacs" {
		t.Fatalf("got %+v, want emacs first", got)
	}
	for _, b := range got {
		if b.Value == "vim" {
			t.Fatalf("superseded vim came back: %+v", got)
		}
	}
}
