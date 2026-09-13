package recall

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeDB is an in-memory VectorDB. It keeps writes in arrival order so a test
// can assert how many happened, which is how the "one write per statement"
// guarantee is checked.
type fakeDB struct {
	records map[string]StoredVector
	order   []string
	puts    int
	// listErr, when set, is returned by List, standing in for a cold-served
	// collection that has no listing index.
	listErr error
	// searchReversed serves hits oldest-last, to prove recall does not depend
	// on a superseded event ranking low.
	searchReversed bool
}

func newFakeDB() *fakeDB { return &fakeDB{records: map[string]StoredVector{}} }

func (f *fakeDB) Put(_, id string, values []float32, md map[string]any) error {
	if _, exists := f.records[id]; !exists {
		f.order = append(f.order, id)
	}
	f.records[id] = StoredVector{ID: id, Values: values, Metadata: md}
	f.puts++
	return nil
}

func (f *fakeDB) matching(filter map[string]any, limit int) []StoredVector {
	out := []StoredVector{}
	ids := f.order
	if f.searchReversed {
		ids = make([]string, len(f.order))
		for i, id := range f.order {
			ids[len(f.order)-1-i] = id
		}
	}
	for _, id := range ids {
		rec := f.records[id]
		ok := true
		for k, want := range filter {
			if fmt.Sprint(rec.Metadata[k]) != fmt.Sprint(want) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, rec)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (f *fakeDB) List(_ string, filter map[string]any, limit int) ([]StoredVector, int, error) {
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	got := f.matching(filter, limit)
	return got, len(f.matching(filter, 0)), nil
}

func (f *fakeDB) Search(_ string, _ []float32, k int, filter map[string]any) ([]Hit, error) {
	out := []Hit{}
	for _, rec := range f.matching(filter, k) {
		out = append(out, Hit{ID: rec.ID, Metadata: rec.Metadata})
	}
	return out, nil
}

func testRegistry(t *testing.T) Registry {
	t.Helper()
	reg, err := LoadRegistry([]byte(`{
		"prefers_editor":  {"cardinality": "single", "value_type": "string",  "description": "editor"},
		"likes":           {"cardinality": "multi",  "value_type": "string",  "description": "likes"},
		"daily_step_goal": {"cardinality": "single", "value_type": "number",  "description": "steps"},
		"uses_dark_mode":  {"cardinality": "single", "value_type": "boolean", "description": "dark mode"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// newStore returns a store whose clock the test drives, so that ordering is
// explicit rather than dependent on how fast the test runs.
func newStore(t *testing.T) (*Store, *fakeDB, *time.Time) {
	t.Helper()
	db := newFakeDB()
	clock := t0
	s := NewStore(db, "memories", testRegistry(t), func(string) []float32 { return []float32{1, 0, 0} })
	s.now = func() time.Time { return clock }
	return s, db, &clock
}

func mustRemember(t *testing.T, s *Store, predicate string, value any) RememberResult {
	t.Helper()
	res, err := s.Remember("preference", "user", predicate, value, 1, "user_stated")
	if err != nil {
		t.Fatalf("Remember(%s, %v): %v", predicate, value, err)
	}
	return res
}

func recallValues(t *testing.T, s *Store, q Query) []any {
	t.Helper()
	got, err := s.Recall(q)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	out := make([]any, 0, len(got))
	for _, b := range got {
		out = append(out, b.Value)
	}
	return out
}

func TestRememberThenRecall(t *testing.T) {
	s, _, _ := newStore(t)
	mustRemember(t, s, "prefers_editor", "neovim")
	if got := recallValues(t, s, Query{Subject: "user", Predicate: "prefers_editor"}); len(got) != 1 || got[0] != "neovim" {
		t.Fatalf("got %v", got)
	}
}

// One statement is one write. The old design wrote the new record and then
// rewrote every record it superseded, which is the window this removes.
func TestSupersessionCostsOneWrite(t *testing.T) {
	s, db, clock := newStore(t)
	mustRemember(t, s, "prefers_editor", "vim")
	before := db.puts
	*clock = t0.Add(time.Hour)
	res := mustRemember(t, s, "prefers_editor", "neovim")

	if db.puts-before != 1 {
		t.Fatalf("supersession made %d writes, want exactly 1", db.puts-before)
	}
	if len(res.Superseded) != 1 || res.Superseded[0].Value != "vim" {
		t.Fatalf("superseded = %+v, want vim", res.Superseded)
	}
	if got := recallValues(t, s, Query{Subject: "user", Predicate: "prefers_editor"}); len(got) != 1 || got[0] != "neovim" {
		t.Fatalf("got %v, want only neovim", got)
	}
}

// The superseded event is still there: nothing was rewritten, so history and
// the audit trail survive.
func TestSupersededEventSurvivesInHistory(t *testing.T) {
	s, _, clock := newStore(t)
	mustRemember(t, s, "prefers_editor", "vim")
	*clock = t0.Add(time.Hour)
	mustRemember(t, s, "prefers_editor", "neovim")

	events, err := s.History("user", "prefers_editor")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("history has %d events, want 2", len(events))
	}
	if events[0].Value != "vim" || events[1].Value != "neovim" {
		t.Fatalf("history out of order: %v, %v", events[0].Value, events[1].Value)
	}
}

func TestRestatingABeliefWritesNothing(t *testing.T) {
	s, db, clock := newStore(t)
	mustRemember(t, s, "prefers_editor", "neovim")
	before := db.puts
	*clock = t0.Add(time.Hour)
	res := mustRemember(t, s, "prefers_editor", "neovim")

	if !res.Existing {
		t.Error("restating a held belief was not reported as already known")
	}
	if db.puts != before {
		t.Fatalf("restating wrote %d times, want 0", db.puts-before)
	}
}

func TestRecallAsOfSeesThePastBelief(t *testing.T) {
	s, _, clock := newStore(t)
	mustRemember(t, s, "prefers_editor", "vim")
	*clock = t0.Add(48 * time.Hour)
	mustRemember(t, s, "prefers_editor", "neovim")

	q := Query{Subject: "user", Predicate: "prefers_editor", AsOf: t0.Add(24 * time.Hour)}
	if got := recallValues(t, s, q); len(got) != 1 || got[0] != "vim" {
		t.Fatalf("as of Tuesday got %v, want vim", got)
	}
	if got := recallValues(t, s, Query{Subject: "user", Predicate: "prefers_editor"}); got[0] != "neovim" {
		t.Fatalf("now got %v, want neovim", got)
	}
}

func TestForgetIsARetractionNotADeletion(t *testing.T) {
	s, db, clock := newStore(t)
	mustRemember(t, s, "prefers_editor", "vim")
	stored := db.puts

	*clock = t0.Add(48 * time.Hour)
	n, err := s.Forget("user", "prefers_editor", "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("withdrew %d, want 1", n)
	}
	if db.puts != stored+1 {
		t.Fatalf("forget made %d writes, want 1 appended retraction", db.puts-stored)
	}
	if got := recallValues(t, s, Query{Subject: "user", Predicate: "prefers_editor"}); len(got) != 0 {
		t.Fatalf("still believed %v after forgetting", got)
	}
	// The point of retracting rather than deleting: Tuesday is unchanged.
	past := Query{Subject: "user", Predicate: "prefers_editor", AsOf: t0.Add(24 * time.Hour)}
	if got := recallValues(t, s, past); len(got) != 1 || got[0] != "vim" {
		t.Fatalf("forgetting rewrote history: as of Tuesday got %v, want vim", got)
	}
}

func TestForgettingWhatIsNotBelievedWritesNothing(t *testing.T) {
	s, db, _ := newStore(t)
	mustRemember(t, s, "prefers_editor", "vim")
	before := db.puts
	n, err := s.Forget("user", "prefers_editor", "emacs")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || db.puts != before {
		t.Fatalf("withdrew %d and wrote %d, want 0 and 0", n, db.puts-before)
	}
}

func TestReassertionAfterForgetting(t *testing.T) {
	s, _, clock := newStore(t)
	mustRemember(t, s, "prefers_editor", "vim")
	*clock = t0.Add(time.Hour)
	if _, err := s.Forget("user", "prefers_editor", ""); err != nil {
		t.Fatal(err)
	}
	*clock = t0.Add(2 * time.Hour)
	mustRemember(t, s, "prefers_editor", "vim")

	if got := recallValues(t, s, Query{Subject: "user", Predicate: "prefers_editor"}); len(got) != 1 || got[0] != "vim" {
		t.Fatalf("got %v, want vim believed again", got)
	}
	events, _ := s.History("user", "prefers_editor")
	if len(events) != 3 {
		t.Fatalf("history has %d events, want all 3 preserved", len(events))
	}
}

func TestMultiValuedAccumulatesAndRetractsOne(t *testing.T) {
	s, _, clock := newStore(t)
	for i, v := range []string{"go", "rust", "zig"} {
		*clock = t0.Add(time.Duration(i) * time.Hour)
		if _, err := s.Remember("fact", "user", "likes", v, 1, "user_stated"); err != nil {
			t.Fatal(err)
		}
	}
	if got := recallValues(t, s, Query{Subject: "user", Predicate: "likes"}); len(got) != 3 {
		t.Fatalf("got %v, want three", got)
	}
	*clock = t0.Add(10 * time.Hour)
	if _, err := s.Forget("user", "likes", "rust"); err != nil {
		t.Fatal(err)
	}
	got := recallValues(t, s, Query{Subject: "user", Predicate: "likes"})
	if len(got) != 2 || got[0] != "go" || got[1] != "zig" {
		t.Fatalf("got %v, want go and zig", got)
	}
}

// Semantic recall folds like every other read, so a superseded event ranking
// top of the search cannot come back as a live belief.
func TestSemanticRecallNeverReturnsASupersededBelief(t *testing.T) {
	s, db, clock := newStore(t)
	mustRemember(t, s, "prefers_editor", "vim")
	*clock = t0.Add(time.Hour)
	mustRemember(t, s, "prefers_editor", "neovim")

	db.searchReversed = true // the stale event now ranks first
	got := recallValues(t, s, Query{Text: "which editor"})
	if len(got) != 1 || got[0] != "neovim" {
		t.Fatalf("got %v, want only neovim", got)
	}
}

func TestColdCollectionRefusesApproximateHistory(t *testing.T) {
	s, db, _ := newStore(t)
	mustRemember(t, s, "prefers_editor", "neovim")
	db.listErr = fmt.Errorf("listing is not supported for a cold-served resource")
	if _, err := s.Recall(Query{Subject: "user", Predicate: "prefers_editor"}); !errors.Is(err, ErrIncompleteHistory) {
		t.Fatalf("cold read error = %v, want incomplete history", err)
	}
}

func TestListErrorsOtherThanColdPropagate(t *testing.T) {
	s, db, _ := newStore(t)
	mustRemember(t, s, "prefers_editor", "neovim")
	db.listErr = fmt.Errorf("bucket on fire")
	if _, err := s.Recall(Query{Subject: "user", Predicate: "prefers_editor"}); err == nil {
		t.Fatal("a real failure was swallowed as a cold-collection fallback")
	}
}

func TestValidationRejectsBadWrites(t *testing.T) {
	s, _, _ := newStore(t)
	cases := []struct {
		name                     string
		kind, subject, predicate string
		value                    any
		confidence               float64
		source, want             string
	}{
		{"unknown predicate", "fact", "user", "invented_thing", "x", 1, "user_stated", "not in the registry"},
		{"bad kind", "opinion", "user", "prefers_editor", "vim", 1, "user_stated", "kind must be"},
		{"empty subject", "fact", "  ", "prefers_editor", "vim", 1, "user_stated", "subject must not be empty"},
		{"not snake case", "fact", "user", "PrefersEditor", "vim", 1, "user_stated", "snake_case"},
		{"wrong value type", "fact", "user", "daily_step_goal", "8000", 1, "user_stated", "expects a number"},
		{"confidence out of range", "fact", "user", "prefers_editor", "vim", 1.5, "user_stated", "confidence must be"},
		{"bad source", "fact", "user", "prefers_editor", "vim", 1, "rumour", "source must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Remember(tc.kind, tc.subject, tc.predicate, tc.value, tc.confidence, tc.source)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestTypedValuesRoundTrip(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.Remember("fact", "user", "daily_step_goal", float64(8000), 1, "user_stated"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remember("fact", "user", "uses_dark_mode", true, 1, "user_stated"); err != nil {
		t.Fatal(err)
	}
	got := recallValues(t, s, Query{Subject: "user", Predicate: "daily_step_goal"})
	if len(got) != 1 || got[0] != float64(8000) {
		t.Fatalf("got %#v, want the number 8000", got)
	}
	got = recallValues(t, s, Query{Subject: "user", Predicate: "uses_dark_mode"})
	if len(got) != 1 || got[0] != true {
		t.Fatalf("got %#v, want true", got)
	}
}

func TestSubjectIsNormalized(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.Remember("fact", "  User  ", "prefers_editor", "neovim", 1, "user_stated"); err != nil {
		t.Fatal(err)
	}
	if got := recallValues(t, s, Query{Subject: "user", Predicate: "prefers_editor"}); len(got) != 1 {
		t.Fatalf("got %v, want the record addressable as \"user\"", got)
	}
}

func TestMinConfidenceFiltersAfterFolding(t *testing.T) {
	s, _, _ := newStore(t)
	if _, err := s.Remember("fact", "user", "prefers_editor", "vim", 0.4, "agent_inferred"); err != nil {
		t.Fatal(err)
	}
	q := Query{Subject: "user", Predicate: "prefers_editor", MinConfidence: 0.8}
	if got := recallValues(t, s, q); len(got) != 0 {
		t.Fatalf("got %v, want nothing above the confidence floor", got)
	}
}

// Every other path keys a pair on the normalized subject, so a record written
// by an import or another tool with a different case must not become a second
// pair. Both would resolve to the same normalized history and the caller would
// receive one belief twice, spending its limit on a duplicate.
func TestBroadRecallDoesNotDuplicateAnUnnormalizedSubject(t *testing.T) {
	s, db, clock := newStore(t)
	*clock = at(time.Hour)
	mustRemember(t, s, "prefers_editor", "vim")

	stray := Event{ID: "stray", Kind: "preference", Subject: "User", Predicate: "prefers_editor",
		Value: "emacs", Confidence: 1, Source: "user_stated", ObservedAt: at(30 * time.Minute)}
	if err := db.Put("memories", stray.ID, []float32{1, 0, 0}, stray.Metadata()); err != nil {
		t.Fatal(err)
	}
	got, err := s.Recall(Query{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Value != "vim" {
		t.Fatalf("unnormalized subject produced a duplicate pair: %+v", got)
	}
}
