package engine

import (
	"strings"
	"testing"
	"time"
)

func newElsewhereClient(t *testing.T) *Client {
	t.Helper()
	reg, err := LoadRegistry([]byte(correctionRegistry))
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(Config{Backend: newLockedBackend(), Collection: "m", Registry: reg, Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func say(t *testing.T, c *Client, predicate, value string, at time.Time, replaces ...BeliefRef) {
	t.Helper()
	if _, err := c.Remember(t.Context(), RememberRequest{Subject: "user", Predicate: predicate, Value: value, ObservedAt: at, Replaces: replaces}); err != nil {
		t.Fatal(err)
	}
}

func TestReadsShowTheSameValueUnderOtherPredicates(t *testing.T) {
	c := newElsewhereClient(t)
	say(t, c, "likes", "Mangoes", t0)
	say(t, c, "likes", "bananas", t0)
	say(t, c, "dislikes", "mangoes", t0.Add(time.Hour)) // never linked to the like

	likes, err := c.Recall(t.Context(), Query{Subject: "user", Predicate: "likes"})
	if err != nil || len(likes) != 2 {
		t.Fatalf("likes = %+v, %v", likes, err)
	}
	for _, b := range likes {
		switch b.Value {
		case "Mangoes":
			if len(b.Elsewhere) != 1 || b.Elsewhere[0].Predicate != "dislikes" || !b.Elsewhere[0].ObservedAt.Equal(t0.Add(time.Hour)) {
				t.Fatalf("mangoes elsewhere = %+v, want the later dislike", b.Elsewhere)
			}
		case "bananas":
			if len(b.Elsewhere) != 0 {
				t.Fatalf("bananas elsewhere = %+v, want none", b.Elsewhere)
			}
		}
	}
	// Either side shows the other.
	dislikes, err := c.Recall(t.Context(), Query{Subject: "user", Predicate: "dislikes"})
	if err != nil || len(dislikes) != 1 || len(dislikes[0].Elsewhere) != 1 || dislikes[0].Elsewhere[0].Predicate != "likes" {
		t.Fatalf("dislikes = %+v, %v", dislikes, err)
	}
	// Before the dislike was said there was nothing elsewhere.
	before, err := c.Recall(t.Context(), Query{Subject: "user", Predicate: "likes", AsOf: t0.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range before {
		if len(b.Elsewhere) != 0 {
			t.Fatalf("as of before the dislike, %v elsewhere = %+v", b.Value, b.Elsewhere)
		}
	}
	// A caller can opt out.
	plain, err := c.Recall(t.Context(), Query{Subject: "user", Predicate: "dislikes", NoElsewhere: true})
	if err != nil || len(plain) != 1 || len(plain[0].Elsewhere) != 0 {
		t.Fatalf("NoElsewhere = %+v, %v", plain, err)
	}
}

func TestACorrectionClearsElsewhere(t *testing.T) {
	c := newElsewhereClient(t)
	say(t, c, "likes", "mangoes", t0)
	say(t, c, "dislikes", "mangoes", t0.Add(time.Hour), BeliefRef{Predicate: "likes", Value: "mangoes"})
	got, err := c.Recall(t.Context(), Query{Subject: "user", Predicate: "dislikes"})
	if err != nil || len(got) != 1 || len(got[0].Elsewhere) != 0 || len(got[0].Replaced) != 1 {
		t.Fatalf("dislikes = %+v, %v; want the correction under replaced and nothing elsewhere", got, err)
	}
}

func TestAskSaysWhatElseIsBelieved(t *testing.T) {
	c := newElsewhereClient(t)
	say(t, c, "likes", "mangoes", t0)
	say(t, c, "dislikes", "mangoes", t0.Add(time.Hour))
	memories, err := c.Ask(t.Context(), "mangoes")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range memories {
		if strings.Contains(m.Text, "likes: mangoes") && !strings.Contains(m.Text, "dislikes") {
			if len(m.Elsewhere) != 1 || m.Elsewhere[0].Text != "user dislikes: mangoes" {
				t.Fatalf("like's elsewhere = %+v", m.Elsewhere)
			}
			return
		}
	}
	t.Fatalf("ask = %+v, want the like", memories)
}

func TestElsewhereMatchesAcrossCapitalization(t *testing.T) {
	c := newElsewhereClient(t)
	say(t, c, "likes", "état civil", t0)
	say(t, c, "dislikes", "État civil", t0.Add(time.Hour))
	got, err := c.Recall(t.Context(), Query{Subject: "user", Predicate: "likes"})
	if err != nil || len(got) != 1 || len(got[0].Elsewhere) != 1 {
		t.Fatalf("likes = %+v, %v; want the dislike elsewhere", got, err)
	}
}

func TestElsewhereFindsEventsWrittenBeforeValueKey(t *testing.T) {
	b := newLockedBackend()
	reg, err := LoadRegistry([]byte(correctionRegistry))
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(Config{Backend: b, Collection: "m", Registry: reg, Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	// A like stored by an older release, with no value_key.
	old := Event{ID: "m-old", Kind: "fact", Subject: "user", Predicate: "likes", Value: "Mangoes", Confidence: 1, Source: "user_stated", ObservedAt: t0}
	md := old.Metadata()
	delete(md, "value_key")
	if err := b.Put(t.Context(), "m", old.ID, []float32{1}, md); err != nil {
		t.Fatal(err)
	}
	// An unrelated older like that must not be matched.
	other := Event{ID: "m-other", Kind: "fact", Subject: "user", Predicate: "likes", Value: "kiwi", Confidence: 1, Source: "user_stated", ObservedAt: t0}
	md = other.Metadata()
	delete(md, "value_key")
	if err := b.Put(t.Context(), "m", other.ID, []float32{1}, md); err != nil {
		t.Fatal(err)
	}
	say(t, c, "dislikes", "mangoes", t0.Add(time.Hour))
	got, err := c.Recall(t.Context(), Query{Subject: "user", Predicate: "dislikes"})
	if err != nil || len(got) != 1 || len(got[0].Elsewhere) != 1 || got[0].Elsewhere[0].Value != "Mangoes" {
		t.Fatalf("dislikes = %+v, %v; want the old like elsewhere", got, err)
	}
	s, err := c.forContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	pairs, err := s.listPairsHolding("user", "mangoes")
	if err != nil || len(pairs) != 2 {
		t.Fatalf("pairs holding mangoes = %v, %v; want likes and dislikes only", pairs, err)
	}
}
