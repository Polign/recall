package engine

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestInvalidQueriesDoNotReachBackend(t *testing.T) {
	for _, q := range []Query{
		{Limit: int(^uint(0) >> 1)}, {Limit: 1001},
		{MinConfidence: math.NaN()}, {MinConfidence: math.Inf(1)}, {MinConfidence: -0.1}, {MinConfidence: 1.1},
		{ValueMin: floatPtr(math.NaN())}, {ValueMax: floatPtr(math.Inf(-1))},
		{ValueMin: floatPtr(2), ValueMax: floatPtr(1)},
	} {
		t.Run(fmt.Sprint(q), func(t *testing.T) {
			s, db, _ := newStore(t)
			// The exact path previously allocated directly from the untrusted limit.
			q.Subject, q.Predicate = "user", "prefers_editor"
			db.listErr = errors.New("backend must not be called")
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("query panicked: %v", r)
				}
			}()
			if _, err := s.Recall(q); err == nil || errors.Is(err, db.listErr) {
				t.Fatalf("query validation = %v", err)
			}
		})
	}
	s, db, _ := newStore(t)
	db.listErr = errors.New("backend must not be called")
	if _, err := s.ExportEvents(Export{Limit: MaxExport + 1}); err == nil || errors.Is(err, db.listErr) {
		t.Fatalf("export validation = %v", err)
	}
}

func floatPtr(f float64) *float64 { return &f }

func TestNonfiniteInputsCannotWrite(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		s, db, _ := newStore(t)
		if _, err := s.Remember("fact", "user", "daily_step_goal", v, 1, "user_stated"); err == nil {
			t.Errorf("accepted value %v", v)
		}
		if _, err := s.Remember("fact", "user", "prefers_editor", "vim", v, "user_stated"); err == nil {
			t.Errorf("accepted confidence %v", v)
		}
		if _, err := s.Forget("user", "daily_step_goal", fmt.Sprint(v)); err == nil {
			t.Errorf("accepted withdrawal value %v", v)
		}
		if db.puts != 0 {
			t.Fatalf("invalid input wrote %d records", db.puts)
		}
	}
}

func TestMalformedHistoryCannotAnswerOrAuthorizeWrites(t *testing.T) {
	mutations := map[string]func(map[string]any){
		"timestamp":           func(m map[string]any) { m["observed_at"] = "bad" },
		"missing timestamp":   func(m map[string]any) { delete(m, "observed_at") },
		"retraction type":     func(m map[string]any) { m["retraction"] = "true" },
		"null retraction":     func(m map[string]any) { m["retraction"] = nil },
		"kind":                func(m map[string]any) { m["kind"] = "opinion" },
		"source":              func(m map[string]any) { m["source"] = "rumor" },
		"confidence type":     func(m map[string]any) { m["confidence"] = "1" },
		"confidence range":    func(m map[string]any) { m["confidence"] = 2.0 },
		"confidence NaN":      func(m map[string]any) { m["confidence"] = math.NaN() },
		"value type":          func(m map[string]any) { m["value"] = map[string]any{"x": "y"} },
		"registry value type": func(m map[string]any) { m["value"] = true },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s, db, clock := newStore(t)
			mustRemember(t, s, "prefers_editor", "vim")
			*clock = clock.Add(time.Second)
			if _, err := s.Forget("user", "prefers_editor", ""); err != nil {
				t.Fatal(err)
			}
			mutate(db.records[db.order[1]].Metadata)
			before := db.puts
			calls := []func() error{
				func() error { _, err := s.History("user", "prefers_editor"); return err },
				func() error { _, err := s.Recall(Query{Subject: "user", Predicate: "prefers_editor"}); return err },
				func() error {
					_, err := s.Remember("fact", "user", "prefers_editor", "emacs", 1, "user_stated")
					return err
				},
				func() error { _, err := s.Forget("user", "prefers_editor", ""); return err },
				func() error { _, err := s.ExportEvents(Export{}); return err },
			}
			for _, call := range calls {
				if err := call(); !errors.Is(err, ErrIncompleteHistory) {
					t.Errorf("history error = %v", err)
				}
			}
			if db.puts != before {
				t.Fatal("corrupt history authorized a write")
			}
		})
	}
}

func TestStoredEventValidationAndLegacyEncoding(t *testing.T) {
	s, db, _ := newStore(t)
	e := assert("legacy", "vim", t0)
	md := e.Metadata()
	// Missing optional retraction means an assertion. RFC3339 and missing
	// observed_ms remain supported; no schema rewrite is required.
	delete(md, "retraction")
	delete(md, "observed_ms")
	md["observed_at"] = t0.Format(time.RFC3339)
	if err := db.Put("memories", e.ID, []float32{1}, md); err != nil {
		t.Fatal(err)
	}
	if events, err := s.History("user", "prefers_editor"); err != nil || len(events) != 1 {
		t.Fatalf("legacy history = %v, %v", events, err)
	}
	for _, id := range []string{"", "  "} {
		if _, err := DecodeEvent(id, md); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("ID %q error = %v", id, err)
		}
	}
	for _, key := range []string{"kind", "subject", "predicate", "source", "confidence", "value"} {
		copy := e.Metadata()
		delete(copy, key)
		if _, err := DecodeEvent(e.ID, copy); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("missing %s error = %v", key, err)
		}
	}
	invalid := e.Metadata()
	invalid["predicate"] = "Invalid Predicate"
	if _, err := DecodeEvent(e.ID, invalid); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("predicate error = %v", err)
	}
	for _, value := range []any{math.NaN(), math.Inf(1), []any{1}, "  "} {
		invalid = e.Metadata()
		invalid["value"] = value
		if _, err := DecodeEvent(e.ID, invalid); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("value %v error = %v", value, err)
		}
	}
	for _, value := range []any{true, float64(0), math.Copysign(0, -1), "vim"} {
		valid := e.Metadata()
		valid["value"] = value
		if _, err := DecodeEvent(e.ID, valid); err != nil {
			t.Errorf("valid value %v error = %v", value, err)
		}
	}
	if _, err := DecodeEvent("blanket", retract("blanket", nil, t0).Metadata()); err != nil {
		t.Fatalf("blanket retraction = %v", err)
	}
}

func TestInvalidEventErrorIdentifiesRecord(t *testing.T) {
	_, err := DecodeEvent("bad-record", map[string]any{})
	if !errors.Is(err, ErrInvalidEvent) || !strings.Contains(err.Error(), "bad-record") {
		t.Fatalf("error = %v", err)
	}
}

func TestMalformedCandidatesAndPartialExportsFail(t *testing.T) {
	s, db, _ := newStore(t)
	mustRemember(t, s, "prefers_editor", "vim")
	db.records[db.order[0]].Metadata["observed_at"] = "invalid"
	for _, q := range []Query{{Subject: "user"}, {Text: "editor"}} {
		if _, err := s.Recall(q); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("candidate error = %v", err)
		}
	}
	if _, err := s.ExportEvents(Export{Limit: 1}); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("partial export error = %v", err)
	}
}

func TestQueryBoundsAndDefaultsRemainUsable(t *testing.T) {
	s, _, _ := newStore(t)
	mustRemember(t, s, "prefers_editor", "vim")
	for _, limit := range []int{-1, 0, 1, MaxRecall} {
		got, err := s.Recall(Query{Subject: "user", Predicate: "prefers_editor", Limit: limit})
		if err != nil || len(got) != 1 {
			t.Errorf("limit %d: %v, %v", limit, got, err)
		}
	}
	if events, err := s.ExportEvents(Export{Limit: MaxExport}); err != nil || len(events) != 1 {
		t.Fatalf("maximum export = %v, %v", events, err)
	}
}
