package engine

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestLexicalQuery(t *testing.T) {
	for q, want := range map[string]string{
		"Where does Maya live now?":     "live lives",
		"What can Maya eat?":            "eat eats",
		"Where does Priya live?":        "priya priyas live lives",
		"which editor is she using?":    "editor editors using us use",
		"Is Maya allergic to anything?": "allergic allergics",
		"where was it?":                 "where was it?", // nothing left: searched as asked
	} {
		if got := lexicalQuery(q, []string{"Maya"}); got != want {
			t.Errorf("lexicalQuery(%q) = %q, want %q", q, got, want)
		}
	}
}

// A person whose facts change, written in the order they were said and asked
// about at once, before any keyword index could have caught up: the hashed
// word vectors are the only ranking. Before the query was cleaned, "Maya"
// ranked her name and her three email addresses (maya@...) above where she
// lives, and "live" matched nothing in "lives in".
func TestWordSearchFindsChangedFacts(t *testing.T) {
	reg, err := LoadRegistry([]byte(`{
		"name":        {"cardinality": "single", "value_type": "string", "description": "name"},
		"lives_in":    {"cardinality": "single", "value_type": "string", "description": "home"},
		"works_at":    {"cardinality": "single", "value_type": "string", "description": "employer"},
		"role":        {"cardinality": "single", "value_type": "string", "description": "job"},
		"email":       {"cardinality": "multi",  "value_type": "string", "description": "email"},
		"diet":        {"cardinality": "single", "value_type": "string", "description": "diet"},
		"allergic_to": {"cardinality": "multi",  "value_type": "string", "description": "allergy"},
		"relationship":{"cardinality": "multi",  "value_type": "string", "description": "people"},
		"plans":       {"cardinality": "multi",  "value_type": "string", "description": "plans"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	db := newFakeDB()
	db.bySimilarity = true
	s := NewStore(db, "memories", reg, func(text string) []float32 {
		v, _ := LexicalEmbedder{}.Embed(context.Background(), text)
		return v
	})
	s.textFirst = true
	clock := t0
	s.now = func() time.Time { return clock }
	for i, f := range [][3]string{
		{"user", "name", "Maya"},
		{"user", "lives_in", "Seattle"},
		{"user", "works_at", "Northwind"},
		{"user", "role", "data analyst"},
		{"user", "email", "maya@northwind.com"},
		{"user", "diet", "vegetarian"},
		{"user", "relationship", "sister Priya"},
		{"priya", "lives_in", "Denver"},
		{"user", "relationship", "friend Sam"},
		{"sam", "lives_in", "Tacoma"},
		{"user", "lives_in", "Tacoma"},
		{"user", "works_at", "Bluefin"},
		{"user", "role", "product manager"},
		{"user", "email", "maya@bluefin.io"},
		{"priya", "lives_in", "Austin"},
		{"user", "lives_in", "Portland"},
		{"user", "relationship", "coworker Dan"},
		{"dan", "lives_in", "Seattle"},
		{"user", "diet", "pescatarian"},
		{"user", "works_at", "Orca Labs"},
		{"user", "email", "maya@orcalabs.com"},
		{"user", "allergic_to", "shellfish"},
		{"user", "plans", "considering moving to Boise in 2027"},
	} {
		clock = t0.Add(time.Duration(i) * time.Hour)
		if _, err := s.Remember("fact", f[0], f[1], f[2], 1, "user_stated"); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		question string
		top      int
		want     []any
	}{
		{"Where does Maya live now?", 3, []any{"Portland"}},
		// Word search cannot tell that "eat" means "diet"; that question
		// needs a model embedder. The words of the fact are enough.
		{"What is Maya's diet?", 3, []any{"pescatarian"}},
		{"Is Maya allergic to anything?", 3, []any{"shellfish"}},
		{"Where does Maya work?", 3, []any{"Orca Labs"}},
		{"Where does her sister Priya live?", 3, []any{"Austin"}},
	} {
		got := recallValues(t, s, Query{Text: tc.question, Limit: 8})
		head := got[:min(tc.top, len(got))]
		for _, w := range tc.want {
			if !slices.Contains(head, w) {
				t.Errorf("%q: top %d = %v, want %v among them", tc.question, tc.top, head, w)
			}
		}
	}
}
