package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// textDB adds a lexical index to fakeDB: SearchText ranks records by how many
// query words their text field holds, the way BM25 would for these tests.
type textDB struct {
	*fakeDB
	err      error
	searched int
}

func (d *textDB) SearchText(_, text string, k int, filter map[string]any) ([]Hit, error) {
	d.searched++
	if d.err != nil {
		return nil, d.err
	}
	type scored struct {
		rec   StoredVector
		score int
	}
	var ranked []scored
	for _, rec := range d.matching(filter, 0) {
		body, _ := rec.Metadata[TextField].(string)
		n := 0
		for _, w := range strings.Fields(strings.ToLower(text)) {
			if strings.Contains(strings.ToLower(body), w) {
				n++
			}
		}
		if n > 0 {
			ranked = append(ranked, scored{rec, n})
		}
	}
	for i := 1; i < len(ranked); i++ {
		for j := i; j > 0 && ranked[j].score > ranked[j-1].score; j-- {
			ranked[j], ranked[j-1] = ranked[j-1], ranked[j]
		}
	}
	out := []Hit{}
	for _, r := range ranked[:min(k, len(ranked))] {
		out = append(out, Hit{ID: r.rec.ID, Metadata: r.rec.Metadata})
	}
	return out, nil
}

func newTextStore(t *testing.T) (*Store, *textDB, *time.Time) {
	t.Helper()
	s, db, clock := newStore(t) // constant vectors: the vector search ranks by arrival only
	tdb := &textDB{fakeDB: db}
	s.db = tdb
	return s, tdb, clock
}

func TestEventsCarryTheirSearchableText(t *testing.T) {
	s, db, _ := newTextStore(t)
	mustRemember(t, s, "prefers_editor", "neovim")
	for _, rec := range db.records {
		if rec.Metadata[TextField] != "user prefers editor neovim" {
			t.Fatalf("text metadata = %q", rec.Metadata[TextField])
		}
	}
}

// With a vector search that cannot rank (every vector is the same), only the
// lexical search can put the one relevant note first. The ranking is the one
// a client with the lexical embedder uses; fused evenly, the note ties with
// the vector search's first hit.
func TestSemanticRecallFusesTheTextSearch(t *testing.T) {
	s, db, clock := newTextStore(t)
	s.textFirst = textFirstFor(LexicalEmbedder{})
	for i := range 40 {
		rememberAt(t, s, clock, time.Duration(i)*time.Minute, NotePredicate, fmt.Sprintf("filler conversation %d about cooking", i))
	}
	rememberAt(t, s, clock, time.Hour, NotePredicate, "reached level 100 in apex legends")

	got := recallValues(t, s, Query{Text: "apex legends level", Limit: 3})
	if len(got) == 0 || got[0] != "reached level 100 in apex legends" {
		t.Fatalf("got %v, want the apex legends note first", got)
	}
	if db.searched == 0 {
		t.Fatal("recall never used the text search")
	}
}

// A collection with no text index yet still answers from the vector search.
func TestSemanticRecallFallsBackWhenTextSearchIsUnsupported(t *testing.T) {
	s, db, _ := newTextStore(t)
	db.err = fmt.Errorf("%w: no segments yet", ErrTextSearchUnsupported)
	mustRemember(t, s, "prefers_editor", "neovim")
	if got := recallValues(t, s, Query{Text: "editor"}); len(got) != 1 || got[0] != "neovim" {
		t.Fatalf("got %v, want neovim from the vector search", got)
	}

	db.err = fmt.Errorf("index corrupt")
	if _, err := s.Recall(Query{Text: "editor"}); err == nil {
		t.Fatal("a text search failure other than unsupported must surface")
	}
}

func TestFuseHitsOrdersAndKeepsFreshVectorHits(t *testing.T) {
	vector := []Hit{{ID: "fresh"}, {ID: "a"}, {ID: "b"}}
	text := []Hit{{ID: "c"}, {ID: "b"}}
	ids := func(hs []Hit) string {
		var out []string
		for _, h := range hs {
			out = append(out, h.ID)
		}
		return strings.Join(out, ",")
	}
	// Text first: the lexical ranking as it is, then the vector hits it
	// lacks, so "fresh", which the text index has not seen yet, still comes.
	if got := ids(fuseHits(vector, text, true, 4)); got != "c,b,fresh,a" {
		t.Fatalf("text first: %s", got)
	}
	// Fused: b, found by both, leads; the rest follow by rank.
	if got := ids(fuseHits(vector, text, false, 3)); got != "b,fresh,c" {
		t.Fatalf("fused: %s", got)
	}
}

func TestTextLeadsOnlyForTheLexicalEmbedder(t *testing.T) {
	if !textFirstFor(LexicalEmbedder{}) || !textFirstFor(&LexicalEmbedder{}) {
		t.Fatal("hashed lexical vectors should follow the text ranking")
	}
	if textFirstFor(nil) {
		t.Fatal("a model embedder is fused evenly")
	}
}
