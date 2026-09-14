package engine

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRememberAfterValueRetractionDoesNotDuplicate(t *testing.T) {
	s, _, clock := newStore(t)
	mustRemember(t, s, "likes", "rust")
	for i := 0; i < 3; i++ {
		*clock = clock.Add(time.Second)
		mustRemember(t, s, "likes", "go")
		got := recallValues(t, s, Query{Subject: "user", Predicate: "likes"})
		if len(got) != 2 || got[0] != "rust" || got[1] != "go" {
			t.Fatalf("cycle %d: %v", i, got)
		}
		*clock = clock.Add(time.Second)
		n, err := s.Forget("user", "likes", "go")
		if err != nil || n != 1 {
			t.Fatalf("withdrawn = %d, %v", n, err)
		}
	}
}

func seedLongHistory(t *testing.T, n int) (*Store, *fakeDB) {
	t.Helper()
	s, db, clock := newStore(t)
	for i := 0; i < n; i++ {
		e := assert(fmt.Sprintf("e-%05d", i), fmt.Sprintf("v%d", i), t0.Add(time.Duration(i)*time.Second))
		if err := db.Put("memories", e.ID, []float32{1, 0, 0}, e.Metadata()); err != nil {
			t.Fatal(err)
		}
	}
	*clock = t0.Add(24 * time.Hour)
	return s, db
}

func TestHistoryBeyondOldLimitIncludesCorrectionAndRetraction(t *testing.T) {
	s, _ := seedLongHistory(t, 1001)
	got := recallValues(t, s, Query{Subject: "user", Predicate: "prefers_editor"})
	if len(got) != 1 || got[0] != "v1000" {
		t.Fatalf("got %v, want latest correction", got)
	}
	if _, err := s.Forget("user", "prefers_editor", ""); err != nil {
		t.Fatal(err)
	}
	if got := recallValues(t, s, Query{Subject: "user", Predicate: "prefers_editor"}); len(got) != 0 {
		t.Fatalf("retracted value revived: %v", got)
	}
	got = recallValues(t, s, Query{Subject: "user", Predicate: "prefers_editor", AsOf: t0.Add(600 * time.Second)})
	if len(got) != 1 || got[0] != "v600" {
		t.Fatalf("historical value = %v", got)
	}
	history, err := s.History("user", "prefers_editor")
	if err != nil || len(history) != 1002 {
		t.Fatalf("history length = %d, %v", len(history), err)
	}
}

type incompleteDB struct {
	*fakeDB
	duplicate bool
}

func (d incompleteDB) List(collection string, filter map[string]any, limit int) ([]StoredVector, int, error) {
	vs, total, err := d.fakeDB.List(collection, filter, limit)
	if len(vs) > 1 {
		if d.duplicate {
			vs[1] = vs[0]
		} else {
			vs = vs[:1]
		}
	}
	return vs, total, err
}

func TestIncompleteHistoryCannotAnswerOrAuthorizeWrites(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprint(duplicate), func(t *testing.T) {
			s, db := seedLongHistory(t, 2)
			s.db = incompleteDB{db, duplicate}
			calls := []func() error{
				func() error { _, err := s.History("user", "prefers_editor"); return err },
				func() error { _, err := s.Recall(Query{Subject: "user", Predicate: "prefers_editor"}); return err },
				func() error {
					_, err := s.Remember("fact", "user", "prefers_editor", "new", 1, "user_stated")
					return err
				},
				func() error { _, err := s.Forget("user", "prefers_editor", ""); return err },
				func() error { _, err := s.ExportEvents(Export{}); return err },
			}
			before := db.puts
			for _, call := range calls {
				if err := call(); !errors.Is(err, ErrIncompleteHistory) {
					t.Fatalf("error = %v", err)
				}
			}
			if db.puts != before {
				t.Fatal("incomplete history permitted a write")
			}
		})
	}
}

func TestHistoryBoundIsExplicitAndDoesNotPermitOverflow(t *testing.T) {
	s, db := seedLongHistory(t, MaxHistoryEvents)
	if _, err := s.History("user", "prefers_editor"); err != nil {
		t.Fatal(err)
	}
	before := db.puts
	if _, err := s.Remember("fact", "user", "prefers_editor", "new", 1, "user_stated"); !errors.Is(err, ErrIncompleteHistory) {
		t.Fatalf("error = %v", err)
	}
	if _, err := s.Forget("user", "prefers_editor", ""); !errors.Is(err, ErrIncompleteHistory) {
		t.Fatalf("error = %v", err)
	}
	if db.puts != before {
		t.Fatal("wrote beyond history bound")
	}
	s, _ = seedLongHistory(t, MaxHistoryEvents+1)
	if _, err := s.History("user", "prefers_editor"); !errors.Is(err, ErrIncompleteHistory) {
		t.Fatalf("overflow error = %v", err)
	}
}
