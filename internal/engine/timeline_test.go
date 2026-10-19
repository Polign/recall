package engine

import (
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
	s, _ := seedLongHistory(t, 1001)
	tl, err := s.Timeline("user", "prefers_editor")
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Steps) != 1001 || tl.Steps[1000].Change != "replaced" || !slices.Equal(tl.Now, []string{"user prefers editor: v1000"}) {
		t.Fatalf("steps = %d, last = %+v, now = %v", len(tl.Steps), tl.Steps[len(tl.Steps)-1], tl.Now)
	}
}
