package engine

import (
	"strings"
	"testing"
	"time"
)

const exclusionRegistry = `{
	"likes":    {"cardinality": "multi", "value_type": "string", "description": "likes", "excludes": ["dislikes"]},
	"dislikes": {"cardinality": "multi", "value_type": "string", "description": "dislikes"}
}`

func newExclusionStore(t *testing.T, db *fakeDB, registry string) (*Store, *time.Time) {
	t.Helper()
	reg, err := LoadRegistry([]byte(registry))
	if err != nil {
		t.Fatal(err)
	}
	clock := t0
	s := NewStore(db, "memories", reg, func(string) []float32 { return []float32{1, 0, 0} })
	s.now = func() time.Time { return clock }
	return s, &clock
}

func pairValues(t *testing.T, s *Store, predicate string, asOf time.Time) []Belief {
	t.Helper()
	got, err := s.Recall(Query{Subject: "user", Predicate: predicate, AsOf: asOf})
	if err != nil {
		t.Fatalf("Recall(%s): %v", predicate, err)
	}
	return got
}

func TestExcludingAssertionWithdrawsTheSameValue(t *testing.T) {
	s, clock := newExclusionStore(t, newFakeDB(), exclusionRegistry)
	mustRemember(t, s, "likes", "mangoes")
	*clock = t0.Add(time.Minute)
	mustRemember(t, s, "likes", "bananas")
	*clock = t0.Add(2 * time.Minute)
	res := mustRemember(t, s, "dislikes", "Mangoes")

	if len(res.Superseded) != 1 || res.Superseded[0].Predicate != "likes" || res.Superseded[0].Value != "mangoes" {
		t.Fatalf("superseded = %+v, want likes mangoes", res.Superseded)
	}
	likes := pairValues(t, s, "likes", time.Time{})
	if len(likes) != 1 || likes[0].Value != "bananas" {
		t.Fatalf("likes = %v, want only bananas", values(likes))
	}
	dislikes := pairValues(t, s, "dislikes", time.Time{})
	if len(dislikes) != 1 || dislikes[0].Value != "Mangoes" {
		t.Fatalf("dislikes = %v, want Mangoes", values(dislikes))
	}
	r := dislikes[0].Replaced
	if len(r) != 1 || r[0].Predicate != "likes" || r[0].Value != "mangoes" || !r[0].ObservedAt.Equal(t0) {
		t.Fatalf("replaced = %+v, want likes mangoes from t0", r)
	}
	// Time travel still answers what was believed before the change.
	before := pairValues(t, s, "likes", t0.Add(90*time.Second))
	if len(before) != 2 {
		t.Fatalf("likes before the dislike = %v, want mangoes and bananas", values(before))
	}
}

func TestExclusionDeclaredLaterResolvesExistingLog(t *testing.T) {
	// The demo's case: both statements were written before the registry
	// said they contradict each other.
	db := newFakeDB()
	plain := strings.Replace(exclusionRegistry, `, "excludes": ["dislikes"]`, "", 1)
	old, clock := newExclusionStore(t, db, plain)
	mustRemember(t, old, "likes", "mangoes")
	*clock = t0.Add(time.Minute)
	mustRemember(t, old, "dislikes", "mangoes")
	if got := pairValues(t, old, "likes", time.Time{}); len(got) != 1 {
		t.Fatalf("without an exclusion, likes = %v, want mangoes kept", values(got))
	}

	s, clock := newExclusionStore(t, db, exclusionRegistry)
	*clock = t0.Add(time.Hour)
	if got := pairValues(t, s, "likes", time.Time{}); len(got) != 0 {
		t.Fatalf("likes = %v, want mangoes withdrawn by the later dislike", values(got))
	}
	// Restating the dislike is still idempotent.
	if res := mustRemember(t, s, "dislikes", "mangoes"); !res.Existing {
		t.Fatalf("restating the dislike wrote a new event: %+v", res)
	}
}

func TestStatingTheOriginalAgainWinsBack(t *testing.T) {
	s, clock := newExclusionStore(t, newFakeDB(), exclusionRegistry)
	mustRemember(t, s, "likes", "mangoes")
	*clock = t0.Add(time.Minute)
	mustRemember(t, s, "dislikes", "mangoes")
	*clock = t0.Add(2 * time.Minute)
	res := mustRemember(t, s, "likes", "mangoes")
	if res.Existing {
		t.Fatal("liking a value the dislike displaced was treated as already known")
	}
	if got := pairValues(t, s, "dislikes", time.Time{}); len(got) != 0 {
		t.Fatalf("dislikes = %v, want none", values(got))
	}
	likes := pairValues(t, s, "likes", time.Time{})
	if len(likes) != 1 || len(likes[0].Replaced) != 1 || likes[0].Replaced[0].Predicate != "dislikes" {
		t.Fatalf("likes = %+v, want mangoes replacing dislikes mangoes", likes)
	}
}

func TestForgettingTheExcluderRevivesNothing(t *testing.T) {
	s, clock := newExclusionStore(t, newFakeDB(), exclusionRegistry)
	mustRemember(t, s, "likes", "mangoes")
	*clock = t0.Add(time.Minute)
	mustRemember(t, s, "dislikes", "mangoes")
	*clock = t0.Add(2 * time.Minute)
	if n, err := s.Forget("user", "dislikes", "mangoes"); err != nil || n != 1 {
		t.Fatalf("Forget = %d, %v", n, err)
	}
	if got := pairValues(t, s, "likes", time.Time{}); len(got) != 0 {
		t.Fatalf("likes = %v, want nothing: withdrawing the dislike is not liking it", values(got))
	}
}

func TestSingleValuedExcluderKeepsOnlyItsLatest(t *testing.T) {
	// The excluding side is folded under its own cardinality: after
	// favorite moves from tea to coffee, a later avoid of tea displaces
	// nothing, because tea was no longer held.
	s, clock := newExclusionStore(t, newFakeDB(), `{
		"favorite_drink": {"cardinality": "single", "value_type": "string", "description": "favorite"},
		"avoids_drink":   {"cardinality": "multi",  "value_type": "string", "description": "avoids", "excludes": ["favorite_drink"]}
	}`)
	mustRemember(t, s, "favorite_drink", "tea")
	*clock = t0.Add(time.Minute)
	mustRemember(t, s, "favorite_drink", "coffee")
	*clock = t0.Add(2 * time.Minute)
	res := mustRemember(t, s, "avoids_drink", "tea")
	if len(res.Superseded) != 0 {
		t.Fatalf("superseded = %+v, want nothing", res.Superseded)
	}
	avoid := pairValues(t, s, "avoids_drink", time.Time{})
	if len(avoid) != 1 || len(avoid[0].Replaced) != 0 {
		t.Fatalf("avoids = %+v, want tea replacing nothing", avoid)
	}
	if fav := pairValues(t, s, "favorite_drink", time.Time{}); len(fav) != 1 || fav[0].Value != "coffee" {
		t.Fatalf("favorite = %v, want coffee", values(fav))
	}
}

func TestExclusionRegistryValidation(t *testing.T) {
	for name, raw := range map[string]string{
		"self":    `{"a": {"cardinality": "multi", "excludes": ["a"]}}`,
		"unknown": `{"a": {"cardinality": "multi", "excludes": ["b"]}}`,
		"types":   `{"a": {"cardinality": "multi", "excludes": ["b"]}, "b": {"cardinality": "multi", "value_type": "number"}}`,
		"note":    `{"a": {"cardinality": "multi", "excludes": ["note"]}, "note": {"cardinality": "multi"}}`,
	} {
		if _, err := LoadRegistry([]byte(raw)); err == nil {
			t.Errorf("%s: registry accepted", name)
		}
	}
	// Several exclusions, declared on either side, each held both ways.
	reg, err := LoadRegistry([]byte(`{"owns": {"cardinality": "multi"}, "gave_away": {"cardinality": "multi", "excludes": ["owns"]},
		"sold": {"cardinality": "multi", "excludes": ["owns"]}, "wants": {"cardinality": "multi"}, "likes": {"cardinality": "multi", "excludes": ["wants"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.ExcludedBy("owns"); len(got) != 2 || got[0] != "gave_away" || got[1] != "sold" {
		t.Fatalf("ExcludedBy(owns) = %v", got)
	}
	if got := reg.ExcludedBy("wants"); len(got) != 1 || got[0] != "likes" {
		t.Fatalf("ExcludedBy(wants) = %v", got)
	}
}

func TestSeveralExcludersAndHistoricalOnes(t *testing.T) {
	s, clock := newExclusionStore(t, newFakeDB(), `{
		"owns":      {"cardinality": "multi", "value_type": "string", "description": "owns"},
		"gave_away": {"cardinality": "multi", "value_type": "string", "description": "gave away", "excludes": ["owns"], "historical": true},
		"sold":      {"cardinality": "multi", "value_type": "string", "description": "sold", "excludes": ["owns"]}
	}`)
	mustRemember(t, s, "owns", "bike")
	mustRemember(t, s, "owns", "piano")
	*clock = t0.Add(time.Minute)
	res := mustRemember(t, s, "gave_away", "piano")
	if len(res.Superseded) != 1 || res.Superseded[0].Predicate != "owns" {
		t.Fatalf("superseded = %+v, want owns piano", res.Superseded)
	}
	*clock = t0.Add(2 * time.Minute)
	mustRemember(t, s, "sold", "bike")
	if got := pairValues(t, s, "owns", time.Time{}); len(got) != 0 {
		t.Fatalf("owns = %v, want nothing: each excluder withdrew one", values(got))
	}
	// Owning the piano again does not unwrite the giving away: what happened
	// is historical, whatever excludes it.
	*clock = t0.Add(3 * time.Minute)
	res = mustRemember(t, s, "owns", "piano")
	if len(res.Superseded) != 0 {
		t.Fatalf("superseded = %+v, want nothing: gave_away is historical", res.Superseded)
	}
	if got := pairValues(t, s, "gave_away", time.Time{}); len(got) != 1 {
		t.Fatalf("gave_away = %v, want piano kept", values(got))
	}
	owns := pairValues(t, s, "owns", time.Time{})
	if len(owns) != 1 || owns[0].Value != "piano" || len(owns[0].Replaced) != 0 {
		t.Fatalf("owns = %+v, want piano replacing nothing", owns)
	}
}

func TestPromptTableNamesExclusionsBothWays(t *testing.T) {
	reg, err := LoadRegistry([]byte(exclusionRegistry))
	if err != nil {
		t.Fatal(err)
	}
	table := reg.PromptTable()
	for _, want := range []string{"Contradicts dislikes", "Contradicts likes"} {
		if !strings.Contains(table, want) {
			t.Errorf("prompt table missing %q:\n%s", want, table)
		}
	}
}

func TestAuditOfOnePredicateCarriesItsExcluders(t *testing.T) {
	s, clock := newExclusionStore(t, newFakeDB(), exclusionRegistry)
	mustRemember(t, s, "likes", "mangoes")
	*clock = t0.Add(time.Minute)
	mustRemember(t, s, "dislikes", "mangoes")
	*clock = t0.Add(time.Hour)

	b, err := s.ExportAudit(AuditRequest{Scope: AuditScope{Subject: "user", Predicate: "likes"}})
	if err != nil {
		t.Fatal(err)
	}
	if b.FoldVersion != FoldVersion || len(b.Events) != 2 {
		t.Fatalf("bundle fold %q with %d events, want %q with both", b.FoldVersion, len(b.Events), FoldVersion)
	}
	got, err := b.Replay()
	if err != nil {
		t.Fatal(err)
	}
	for _, belief := range got {
		if belief.Predicate == "likes" {
			t.Fatalf("replay believes %+v, which the dislike withdrew", belief)
		}
	}

	b.FoldVersion = foldVersionV3
	if _, err := b.Replay(); err == nil {
		t.Fatal("a v3 bundle with exclusions replayed")
	}
}
