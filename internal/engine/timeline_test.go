package engine

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"
)

func TestTimelineTellsEachCorrectionAsAChange(t *testing.T) {
	s, _, clock := newStore(t)
	mustRemember(t, s, "prefers_editor", "vim")
	*clock = t0.Add(time.Hour)
	mustRemember(t, s, "prefers_editor", "emacs")
	*clock = t0.Add(2 * time.Hour)
	mustRemember(t, s, "prefers_editor", "emacs") // restating writes nothing
	*clock = t0.Add(3 * time.Hour)
	if _, err := s.Forget("user", "prefers_editor", ""); err != nil {
		t.Fatal(err)
	}
	*clock = t0.Add(4 * time.Hour)
	mustRemember(t, s, "prefers_editor", "zed")

	tl, err := s.Timeline("user", "prefers_editor")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		change         string
		added, removed []string
	}{
		{"stated", []string{"user prefers editor: vim"}, nil},
		{"replaced", []string{"user prefers editor: emacs"}, []string{"user prefers editor: vim"}},
		{"withdrawn", nil, []string{"user prefers editor: emacs"}},
		{"stated", []string{"user prefers editor: zed"}, nil},
	}
	if len(tl.Steps) != len(want) {
		t.Fatalf("steps = %+v, want %d", tl.Steps, len(want))
	}
	for i, w := range want {
		st := tl.Steps[i]
		if st.Change != w.change || !slices.Equal(st.Added, w.added) || !slices.Equal(st.Removed, w.removed) {
			t.Errorf("step %d = %s +%v -%v, want %s +%v -%v", i, st.Change, st.Added, st.Removed, w.change, w.added, w.removed)
		}
	}
	if tl.Steps[2].Statement != "withdrew everything for user prefers editor" {
		t.Errorf("withdrawal statement = %q", tl.Steps[2].Statement)
	}
	if !slices.Equal(tl.Now, []string{"user prefers editor: zed"}) {
		t.Errorf("now = %v", tl.Now)
	}
}

func TestTimelineAgreesWithReadsAtEveryStep(t *testing.T) {
	s, _, clock := newStore(t)
	for i, v := range []string{"go", "rust", "zig"} {
		*clock = t0.Add(time.Duration(i) * time.Hour)
		mustRemember(t, s, "likes", v)
	}
	*clock = t0.Add(5 * time.Hour)
	if _, err := s.Forget("user", "likes", "rust"); err != nil {
		t.Fatal(err)
	}
	tl, err := s.Timeline("user", "likes")
	if err != nil {
		t.Fatal(err)
	}
	held := map[string]bool{}
	for _, st := range tl.Steps {
		for _, a := range st.Added {
			held[a] = true
		}
		for _, r := range st.Removed {
			delete(held, r)
		}
		got := pairValues(t, s, "likes", st.At)
		if len(got) != len(held) {
			t.Fatalf("at %s timeline holds %v, read gives %v", st.At, held, values(got))
		}
		for _, b := range got {
			if !held[Sentence(b.Subject, b.Predicate, b.Value)] {
				t.Fatalf("at %s read gives %v not in timeline %v", st.At, b.Value, held)
			}
		}
	}
}

func TestTimelineShowsAnExcludingStatementThatEndedAValue(t *testing.T) {
	s, clock := newExclusionStore(t, newFakeDB(), exclusionRegistry)
	mustRemember(t, s, "likes", "mangoes")
	*clock = t0.Add(time.Minute)
	mustRemember(t, s, "dislikes", "bananas") // unrelated: not in likes' timeline
	*clock = t0.Add(2 * time.Minute)
	mustRemember(t, s, "dislikes", "mangoes")

	tl, err := s.Timeline("user", "likes")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Steps) != 2 {
		t.Fatalf("steps = %+v, want the like and the dislike that ended it", tl.Steps)
	}
	last := tl.Steps[1]
	if last.Predicate != "dislikes" || last.Change != "withdrawn" || !slices.Equal(last.Removed, []string{"user likes: mangoes"}) {
		t.Fatalf("last step = %+v", last)
	}
	if len(tl.Now) != 0 {
		t.Fatalf("now = %v, want nothing", tl.Now)
	}
}

func TestDiffBetweenTwoInstants(t *testing.T) {
	s, _, clock := newStore(t)
	mustRemember(t, s, "prefers_editor", "vim")
	mustRemember(t, s, "likes", "go")
	*clock = t0.Add(time.Hour)
	mustRemember(t, s, "prefers_editor", "zed")
	mustRemember(t, s, "likes", "rust")
	*clock = t0.Add(2 * time.Hour)
	mustRemember(t, s, "daily_step_goal", 9000.0)

	got, err := s.Diff(DiffRequest{From: t0.Add(30 * time.Minute), To: t0.Add(90 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("diff = %+v, want likes and prefers_editor", got)
	}
	if got[0].Predicate != "likes" || !slices.Equal(got[0].Added, []string{"user likes: rust"}) || len(got[0].Removed) != 0 ||
		!slices.Equal(got[0].Before, []string{"user likes: go"}) {
		t.Errorf("likes = %+v", got[0])
	}
	if got[1].Predicate != "prefers_editor" || !slices.Equal(got[1].Added, []string{"user prefers editor: zed"}) ||
		!slices.Equal(got[1].Removed, []string{"user prefers editor: vim"}) {
		t.Errorf("prefers_editor = %+v", got[1])
	}

	all, err := s.Diff(DiffRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("diff from the beginning = %+v, want three predicates", all)
	}
	if _, err := s.Diff(DiffRequest{From: t0.Add(time.Hour), To: t0}); err == nil {
		t.Fatal("diff with from after to: want an error")
	}
}

func TestDiffSkipsARestatement(t *testing.T) {
	s, _, clock := newStore(t)
	mustRemember(t, s, "prefers_editor", "vim")
	*clock = t0.Add(time.Hour)
	mustRemember(t, s, "prefers_editor", "vim")
	got, err := s.Diff(DiffRequest{From: t0.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("diff = %+v, want nothing changed", got)
	}
}

func TestTimelineOfALongHistory(t *testing.T) {
	// The longest history a pair may have. One pass of the fold tells it;
	// folding again at every step took about 19 s here.
	s, _ := seedLongHistory(t, MaxHistoryEvents)
	start := time.Now()
	tl, err := s.Timeline("user", "prefers_editor")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("timeline of %d events took %s", MaxHistoryEvents, d)
	}
	last := MaxHistoryEvents - 1
	if len(tl.Steps) != MaxHistoryEvents || tl.Steps[last].Change != "replaced" ||
		!slices.Equal(tl.Now, []string{fmt.Sprintf("user prefers editor: v%d", last)}) {
		t.Fatalf("steps = %d, last = %+v, now = %v", len(tl.Steps), tl.Steps[len(tl.Steps)-1], tl.Now)
	}
}

// TestTimelineMatchesAFoldAtEveryStep checks the one-pass timeline against
// the fold itself on random logs: assertions, targeted and whole
// withdrawals, under a single-valued, a multi-valued, and an excluding
// predicate.
func TestTimelineMatchesAFoldAtEveryStep(t *testing.T) {
	const registry = `{
		"likes":    {"cardinality": "multi",  "value_type": "string", "description": "likes", "excludes": ["dislikes"]},
		"dislikes": {"cardinality": "multi",  "value_type": "string", "description": "dislikes"},
		"editor":   {"cardinality": "single", "value_type": "string", "description": "editor"}
	}`
	values := []any{"go", "rust", "zig", "vim"}
	var steps, crossed int
	for seed := int64(0); seed < 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		db := newFakeDB()
		s, _ := newExclusionStore(t, db, registry)
		for i := 0; i < 30; i++ {
			at := t0.Add(time.Duration(i) * time.Minute)
			pred := []string{"likes", "dislikes", "editor"}[rng.Intn(3)]
			var e Event
			switch r := rng.Intn(10); {
			case r < 6:
				e = assert(fmt.Sprintf("e%02d", i), values[rng.Intn(len(values))], at)
			case r < 9:
				e = retract(fmt.Sprintf("e%02d", i), values[rng.Intn(len(values))], at)
			default:
				e = retract(fmt.Sprintf("e%02d", i), nil, at)
			}
			e.Predicate = pred
			if err := db.Put("memories", e.ID, []float32{1, 0, 0}, e.Metadata()); err != nil {
				t.Fatal(err)
			}
		}
		sc, err := s.view()
		if err != nil {
			t.Fatal(err)
		}
		for _, pred := range []string{"likes", "dislikes", "editor"} {
			tl, err := s.Timeline("user", pred)
			if err != nil {
				t.Fatal(err)
			}
			_, all, err := s.foldHistory(sc, "user", pred)
			if err != nil {
				t.Fatal(err)
			}
			held := map[string]bool{}
			for _, st := range tl.Steps {
				steps++
				if st.Predicate != pred {
					crossed++
				}
				for _, r := range st.Removed {
					if !held[r] {
						t.Fatalf("seed %d %s: step at %s removes %q, not held", seed, pred, st.At, r)
					}
					delete(held, r)
				}
				for _, a := range st.Added {
					held[a] = true
				}
				want := sentences(sc.fold(pred, all, st.At))
				if len(want) != len(held) {
					t.Fatalf("seed %d %s: at %s timeline holds %v, fold gives %v", seed, pred, st.At, held, want)
				}
				for _, w := range want {
					if !held[w] {
						t.Fatalf("seed %d %s: at %s fold gives %q, timeline holds %v", seed, pred, st.At, w, held)
					}
				}
			}
		}
	}
	if steps < 500 || crossed == 0 {
		t.Fatalf("covered %d steps, %d across predicates; the logs are too thin to test anything", steps, crossed)
	}
	t.Logf("%d steps, %d ended by an excluding predicate", steps, crossed)
}
