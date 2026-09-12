package recall

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

type discoveryDB struct {
	*fakeDB
	widths    []int
	histories int
	searches  int
	corrupt   func(int, []StoredVector, int) ([]StoredVector, int)
}

func (d *discoveryDB) List(collection string, f map[string]any, limit int) ([]StoredVector, int, error) {
	rows, total, err := d.fakeDB.List(collection, f, limit)
	if _, ok := f["predicate"]; !ok {
		d.widths = append(d.widths, limit)
		if d.corrupt != nil {
			rows, total = d.corrupt(len(d.widths), rows, total)
		}
	} else {
		d.histories++
	}
	return rows, total, err
}
func (d *discoveryDB) Search(collection string, v []float32, k int, f map[string]any) ([]Hit, error) {
	d.searches++
	return d.fakeDB.Search(collection, v, k, f)
}

func seedDiscovery(t *testing.T, n int, withdraw bool) (*Store, *discoveryDB) {
	t.Helper()
	s, base, clock := newStore(t)
	for i := 0; i < n; i++ {
		e := assert(fmt.Sprintf("a-%05d", i), fmt.Sprint(i), at(time.Duration(i)*time.Second))
		if err := base.Put("memories", e.ID, []float32{1}, e.Metadata()); err != nil {
			t.Fatal(err)
		}
	}
	if withdraw {
		e := retract("withdraw", nil, at(time.Duration(n)*time.Second))
		if err := base.Put("memories", e.ID, []float32{1}, e.Metadata()); err != nil {
			t.Fatal(err)
		}
	}
	e := assert("live", "go", at(time.Duration(n+1)*time.Second))
	e.Predicate = "likes"
	if err := base.Put("memories", e.ID, []float32{1}, e.Metadata()); err != nil {
		t.Fatal(err)
	}
	*clock = at(24 * time.Hour)
	db := &discoveryDB{fakeDB: base}
	s.db = db
	return s, db
}

func TestExactRecallDiscoversPastWithdrawnHistory(t *testing.T) {
	s, db := seedDiscovery(t, 80, true)
	got := recallValues(t, s, Query{Subject: "user"})
	if !reflect.DeepEqual(got, []any{"go"}) {
		t.Fatalf("broad exact recall = %v, want [go]", got)
	}
	if !reflect.DeepEqual(db.widths, []int{80, 160}) {
		t.Fatalf("discovery widths = %v", db.widths)
	}
	if db.histories != 2 {
		t.Fatalf("read %d histories, want one per pair", db.histories)
	}
	if db.searches != 0 {
		t.Fatal("exact discovery used approximate search")
	}
}

func TestExactRecallContinuesAfterBeliefFilters(t *testing.T) {
	for _, mode := range []string{"confidence", "kind", "as_of", "number_range"} {
		t.Run(mode, func(t *testing.T) {
			s, db := seedDiscovery(t, 100, false)
			q := Query{Subject: "user"}
			latest := db.records["a-00099"]
			switch mode {
			case "confidence":
				latest.Metadata["confidence"] = 0.1
				q.MinConfidence = 0.5
			case "kind":
				latest.Metadata["kind"] = "fact"
				q.Kind = "preference"
			case "as_of":
				for _, id := range db.order[:100] {
					db.records[id].Metadata["observed_at"] = formatTime(at(2 * time.Hour))
				}
				q.AsOf = at(time.Hour)
			case "number_range":
				s.registry["likes"] = Predicate{Cardinality: "multi", ValueType: "number"}
				db.records["live"].Metadata["value"] = 42.0
				q.ValueMin = floatPtr(40)
				q.ValueMax = floatPtr(50)
			}
			got := recallValues(t, s, q)
			want := any("go")
			if mode == "number_range" {
				want = 42.0
			}
			if !reflect.DeepEqual(got, []any{want}) {
				t.Fatalf("filtered recall = %v, want %v", got, want)
			}
		})
	}
}

func TestExactRecallStopsOnceLimitIsFilled(t *testing.T) {
	s, db := seedDiscovery(t, 80, false)
	got := recallValues(t, s, Query{Subject: "user", Limit: 1})
	if !reflect.DeepEqual(got, []any{"79"}) || !reflect.DeepEqual(db.widths, []int{20}) || db.histories != 1 {
		t.Fatalf("got %v, widths %v, histories %d", got, db.widths, db.histories)
	}
}

func TestExactRecallReportsDiscoveryBudgetExhaustion(t *testing.T) {
	s, db := seedDiscovery(t, MaxCandidateEvents-1, true)
	got, err := s.Recall(Query{Subject: "user"})
	if !errors.Is(err, ErrIncompleteCandidates) || len(got) != 0 {
		t.Fatalf("budget result = %v, %v", got, err)
	}
	if db.widths[len(db.widths)-1] != MaxCandidateEvents {
		t.Fatalf("widths = %v", db.widths)
	}
	// Explicit pair reads are independent of how large the rest of the corpus is.
	if got := recallValues(t, s, Query{Subject: "user", Predicate: "likes"}); !reflect.DeepEqual(got, []any{"go"}) {
		t.Fatalf("explicit pair = %v", got)
	}
}

func TestExactRecallAcceptsCompleteCandidateBudget(t *testing.T) {
	s, db := seedDiscovery(t, MaxCandidateEvents-2, true)
	got := recallValues(t, s, Query{Subject: "user"})
	if !reflect.DeepEqual(got, []any{"go"}) || db.widths[len(db.widths)-1] != MaxCandidateEvents {
		t.Fatalf("got %v, widths %v", got, db.widths)
	}
}

func TestExactRecallRejectsInconsistentDiscoveryPages(t *testing.T) {
	for _, mode := range []string{"changed_total", "changed_prefix", "changed_event", "duplicate", "short_page", "negative_total", "excess_rows"} {
		t.Run(mode, func(t *testing.T) {
			s, db := seedDiscovery(t, 80, true)
			db.corrupt = func(call int, rows []StoredVector, total int) ([]StoredVector, int) {
				switch mode {
				case "changed_total":
					if call == 2 {
						total++
					}
				case "changed_prefix":
					if call == 2 {
						rows[0], rows[1] = rows[1], rows[0]
					}
				case "changed_event":
					if call == 2 {
						md := assert(rows[0].ID, "changed", t0).Metadata()
						rows[0].Metadata = md
					}
				case "duplicate":
					rows[1] = rows[0]
				case "short_page":
					rows = rows[:len(rows)-1]
				case "negative_total":
					total = -1
				case "excess_rows":
					rows = append(rows, rows[0])
				}
				return rows, total
			}
			if got, err := s.Recall(Query{Subject: "user"}); !errors.Is(err, ErrIncompleteCandidates) || len(got) != 0 {
				t.Fatalf("inconsistent result = %v, %v", got, err)
			}
		})
	}
}

func TestExactRecallEmptyAndPredicateOnly(t *testing.T) {
	s, _, _ := newStore(t)
	if got := recallValues(t, s, Query{Subject: "nobody"}); len(got) != 0 {
		t.Fatal(got)
	}
	mustRemember(t, s, "likes", "go")
	if _, err := s.Remember("fact", "alice", "likes", "rust", 1, "user_stated"); err != nil {
		t.Fatal(err)
	}
	if got := recallValues(t, s, Query{Predicate: "likes"}); !reflect.DeepEqual(got, []any{"go", "rust"}) {
		t.Fatalf("predicate query = %v", got)
	}
}

func TestExactRecallDoesNotReturnPartialAnswerAtBudget(t *testing.T) {
	s, _ := seedDiscovery(t, MaxCandidateEvents, false)
	// One live belief is found immediately, but another pair lies past the
	// work bound. A successful one-element result would hide that second pair.
	got, err := s.Recall(Query{Subject: "user"})
	if !errors.Is(err, ErrIncompleteCandidates) || got != nil {
		t.Fatalf("partial result = %v, %v", got, err)
	}
}

func TestExactRecallCanFillLimitFromOneMultiValuedPair(t *testing.T) {
	s, _, clock := newStore(t)
	for _, v := range []string{"go", "rust", "python"} {
		mustRemember(t, s, "likes", v)
		*clock = clock.Add(time.Second)
	}
	got := recallValues(t, s, Query{Subject: "user", Limit: 2})
	if !reflect.DeepEqual(got, []any{"go", "rust"}) {
		t.Fatalf("multi-value limit = %v", got)
	}
}
