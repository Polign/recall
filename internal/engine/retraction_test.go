package engine

import (
	"reflect"
	"testing"
	"time"
)

func TestSingleRetractionTargetsOnlyCurrentValue(t *testing.T) {
	initial := []Event{assert("a", "vim", at(0)), assert("b", "emacs", at(time.Second))}
	for _, tc := range []struct {
		name   string
		target any
		want   []any
	}{
		{"superseded value", "vim", []any{"emacs"}},
		{"unknown value", "nano", []any{"emacs"}},
		{"current value", "emacs", []any{}},
		{"current value different case", "EMACS", []any{}},
		{"all values", nil, []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := append(append([]Event(nil), initial...), retract("c", tc.target, at(2*time.Second)))
			// Arrival order must not affect the fold, including a duplicated withdrawal.
			shuffled := []Event{events[2], events[0], events[2], events[1]}
			got := values(Fold(shuffled, Single, at(3*time.Second)))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			if got := values(Fold(shuffled, Single, at(1500*time.Millisecond))); !reflect.DeepEqual(got, []any{"emacs"}) {
				t.Fatalf("history changed: %v", got)
			}
		})
	}
}

func TestSingleRetractionNeverResurrectsSupersededValues(t *testing.T) {
	events := []Event{
		assert("a", "vim", at(0)), assert("b", "emacs", at(time.Second)),
		retract("c", "emacs", at(2*time.Second)), retract("d", "nano", at(3*time.Second)),
	}
	if got := Fold(events, Single, at(4*time.Second)); len(got) != 0 {
		t.Fatalf("old assertion revived: %v", got)
	}
	events = append(events, assert("e", "vim", at(5*time.Second)))
	if got := values(Fold(events, Single, at(6*time.Second))); !reflect.DeepEqual(got, []any{"vim"}) {
		t.Fatalf("reassertion = %v", got)
	}
}

func TestSingleRetractionUsesDeterministicEqualTimeOrder(t *testing.T) {
	when := at(time.Second)
	events := []Event{retract("c", "vim", when), assert("b", "emacs", when), assert("a", "vim", when)}
	if got := values(Fold(events, Single, when)); !reflect.DeepEqual(got, []any{"emacs"}) {
		t.Fatalf("tie order = %v", got)
	}
}

type interleavedHistoryDB struct {
	*fakeDB
	afterList func()
}

func (d *interleavedHistoryDB) List(collection string, filter map[string]any, limit int) ([]StoredVector, int, error) {
	rows, total, err := d.fakeDB.List(collection, filter, limit)
	if fn := d.afterList; fn != nil {
		d.afterList = nil
		fn()
	}
	return rows, total, err
}

func TestForgetDoesNotEraseCorrectionWrittenAfterItsHistoryRead(t *testing.T) {
	s, db, clock := newStore(t)
	mustRemember(t, s, "prefers_editor", "vim")
	writer := NewStore(db, "memories", s.Registry(), func(string) []float32 { return []float32{1, 0, 0} })
	writer.now = func() time.Time { return at(time.Second) }
	// The other writer commits after Forget has read vim but before it appends
	// the targeted withdrawal. A deterministic hook models that interleaving.
	s.db = &interleavedHistoryDB{fakeDB: db, afterList: func() { mustRemember(t, writer, "prefers_editor", "emacs") }}
	*clock = at(2 * time.Second)
	if withdrawn, err := s.Forget("user", "prefers_editor", "vim"); err != nil || withdrawn != 1 {
		t.Fatalf("Forget = %d, %v", withdrawn, err)
	}
	for _, tc := range []struct {
		when time.Time
		want string
	}{
		{at(500 * time.Millisecond), "vim"}, {at(1500 * time.Millisecond), "emacs"}, {at(3 * time.Second), "emacs"},
	} {
		got := recallValues(t, s, Query{Subject: "user", Predicate: "prefers_editor", AsOf: tc.when})
		if !reflect.DeepEqual(got, []any{tc.want}) {
			t.Fatalf("as of %v: %v, want %s", tc.when, got, tc.want)
		}
	}
	history, err := s.History("user", "prefers_editor")
	if err != nil || len(history) != 3 || !history[2].Retraction || history[2].Value != "vim" {
		t.Fatalf("history = %v, %v", history, err)
	}
}

// Stored instants are not always fine-grained: parseTime accepts whole-second
// RFC3339 for imported and hand-written logs. Ordering a tie on the id alone is
// a hash coin flip, and losing it drops the retraction and revives the belief.
func TestRetractionOrdersAfterItsAssertionAtOneInstant(t *testing.T) {
	when := at(time.Hour)
	assertion := Event{ID: eventID("user", "prefers_editor", "vim", false, when), Kind: "fact",
		Subject: "user", Predicate: "prefers_editor", Value: "vim", Confidence: 1,
		Source: "user_stated", ObservedAt: when}
	retraction := Event{ID: eventID("user", "prefers_editor", "vim", true, when), Kind: "fact",
		Subject: "user", Predicate: "prefers_editor", Value: "vim", Confidence: 1,
		Source: "user_stated", Retraction: true, ObservedAt: when}
	if !(retraction.ID < assertion.ID) {
		t.Skipf("ids %s/%s do not exercise the tie-break", assertion.ID, retraction.ID)
	}
	for _, order := range [][]Event{{assertion, retraction}, {retraction, assertion}} {
		if got := Fold(order, Single, when); len(got) != 0 {
			t.Fatalf("retraction lost its tie-break: %+v", got)
		}
	}
}
