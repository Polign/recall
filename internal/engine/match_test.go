package engine

import (
	"context"
	"testing"
)

func remember(t *testing.T, c *Client, predicate string, value any, card Cardinality) RememberResult {
	t.Helper()
	r, err := c.Remember(context.Background(), RememberRequest{Subject: "user", Predicate: predicate, Value: value, Cardinality: card})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func current(t *testing.T, c *Client, predicate string) []any {
	t.Helper()
	got, err := c.Recall(context.Background(), Query{Subject: "user", Predicate: predicate})
	if err != nil {
		t.Fatal(err)
	}
	return beliefValues(got)
}

func TestMatchedNameFoldsWithItsOwner(t *testing.T) {
	ctx := context.Background()
	b := newLockedBackend()
	c := openClient(t, b)
	remember(t, c, "favorite_editor", "helix", "")
	r := remember(t, c, "Editor Favorite", "zed", "")
	if r.Stored.Predicate != "favorite_editor" || len(r.Superseded) != 1 {
		t.Fatalf("stored %+v; editor_favorite should be read as favorite_editor and replace helix", r)
	}
	if got := current(t, c, "favorite_editor"); len(got) != 1 || got[0] != "zed" {
		t.Fatalf("favorite_editor %v, want zed", got)
	}
	vocab, err := c.Vocabulary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, own := vocab["editor_favorite"]; own {
		t.Fatal("a matched name became its own predicate")
	}
	// The event keeps the name it was written under.
	rows, _, err := b.List(ctx, "memories", map[string]any{"predicate": "editor_favorite"}, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("%d events stored under editor_favorite, %v", len(rows), err)
	}

	if err := c.Split(ctx, "editor_favorite"); err != nil {
		t.Fatal(err)
	}
	c.reglog.invalidate()
	if got := current(t, c, "favorite_editor"); len(got) != 1 || got[0] != "helix" {
		t.Fatalf("after split favorite_editor %v, want helix back", got)
	}
	if got := current(t, c, "editor_favorite"); len(got) != 1 || got[0] != "zed" {
		t.Fatalf("after split editor_favorite %v, want zed", got)
	}
}

func TestNoMatchAcrossTypeCardinalityOrLowSimilarity(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	remember(t, c, "favorite_editor", "helix", "")
	remember(t, c, "editor_favorite", float64(3), "") // number, not string
	remember(t, c, "favorite editor list", "vim", "") // similar, below 0.9
	vocab, err := c.Vocabulary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"editor_favorite", "favorite_editor_list"} {
		if _, ok := vocab[name]; !ok {
			t.Fatalf("%s was merged; want its own predicate (vocabulary %v)", name, vocab.Names())
		}
	}
	if got := current(t, c, "favorite_editor"); len(got) != 1 || got[0] != "helix" {
		t.Fatalf("favorite_editor %v, want helix untouched", got)
	}

	// A single-valued owner cannot take a write that asks for many values.
	c = openClient(t, newLockedBackend())
	remember(t, c, "favorite_editor", "helix", "")
	remember(t, c, "editor favorite", "nano", Multi)
	if got := current(t, c, "favorite_editor"); len(got) != 1 || got[0] != "helix" {
		t.Fatalf("favorite_editor %v; a multi write must not merge into it", got)
	}
}

func TestMatchingCanBeTurnedOff(t *testing.T) {
	c, err := NewClient(Config{Backend: newLockedBackend(), Collection: "memories", Embedder: LexicalEmbedder{}, Open: true, MatchThreshold: -1})
	if err != nil {
		t.Fatal(err)
	}
	remember(t, c, "favorite_editor", "helix", "")
	remember(t, c, "editor_favorite", "zed", "")
	if got := current(t, c, "favorite_editor"); len(got) != 1 || got[0] != "helix" {
		t.Fatalf("favorite_editor %v; with matching off the names stay apart", got)
	}
	if _, err := NewClient(Config{Backend: newLockedBackend(), Collection: "m", Open: true, MatchThreshold: 1.5}); err == nil {
		t.Fatal("a threshold above 1 was accepted")
	}
}

func TestManualMergeAndConcurrentMerges(t *testing.T) {
	ctx := context.Background()
	c, err := NewClient(Config{Backend: newLockedBackend(), Collection: "memories", Embedder: LexicalEmbedder{}, Open: true, MatchThreshold: -1})
	if err != nil {
		t.Fatal(err)
	}
	remember(t, c, "lives_in", "Lisbon", "")
	remember(t, c, "home_city", "Porto", "")
	remember(t, c, "residence", "Porto", "")
	if err := c.Merge(ctx, "home_city", "lives_in"); err != nil {
		t.Fatal(err)
	}
	if err := c.Merge(ctx, "residence", "lives_in"); err != nil {
		t.Fatal(err)
	}
	c.reglog.invalidate()
	if got := current(t, c, "lives_in"); len(got) != 1 || got[0] != "Porto" {
		t.Fatalf("lives_in %v, want the newest of the merged values", got)
	}
	s, err := c.forContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := s.view()
	if err != nil {
		t.Fatal(err)
	}
	if names := sc.storedNames("lives_in"); len(names) != 3 {
		t.Fatalf("lives_in reads %v; both merges must survive", names)
	}
	if err := c.Merge(ctx, "lives_in", "lives_in"); err == nil {
		t.Fatal("a predicate was merged into itself")
	}

	b, err := c.ExportAudit(ctx, AuditRequest{Scope: AuditScope{Subject: "user", Predicate: "lives_in"}})
	if err != nil {
		t.Fatal(err)
	}
	if b.FoldVersion != FoldVersion {
		t.Fatalf("merged log exported as %q", b.FoldVersion)
	}
	got, err := b.Replay()
	if err != nil || len(got) != 1 || got[0].Value != "Porto" {
		t.Fatalf("replay %v, %v", beliefValues(got), err)
	}
}
