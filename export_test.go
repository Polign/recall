package recall

import (
	"testing"
	"time"
)

// seedAuditLog writes across two subjects and two predicates, with one
// supersession and one retraction, so an export has every kind of event to
// account for.
func seedAuditLog(t *testing.T) (*Store, *fakeDB) {
	t.Helper()
	s, db, clock := newStore(t)
	step := func(d time.Duration) { *clock = t0.Add(d) }

	step(0)
	mustRemember(t, s, "prefers_editor", "vim")
	step(time.Hour)
	mustRemember(t, s, "prefers_editor", "neovim") // supersedes vim
	step(2 * time.Hour)
	if _, err := s.Remember("fact", "user", "likes", "go", 1, "user_stated"); err != nil {
		t.Fatal(err)
	}
	step(3 * time.Hour)
	if _, err := s.Remember("fact", "alice", "prefers_editor", "emacs", 1, "user_stated"); err != nil {
		t.Fatal(err)
	}
	step(4 * time.Hour)
	if _, err := s.Forget("user", "likes", "go"); err != nil { // retraction
		t.Fatal(err)
	}
	return s, db
}

func TestExportEverythingOldestFirst(t *testing.T) {
	s, _ := seedAuditLog(t)
	events, err := s.ExportEvents(Export{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 {
		t.Fatalf("exported %d events, want 5", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].ObservedAt.Before(events[i-1].ObservedAt) {
			t.Fatalf("event %d (%v) precedes event %d (%v)", i, events[i].ObservedAt, i-1, events[i-1].ObservedAt)
		}
	}
}

// The reason the export exists: what is no longer believed is still in it.
func TestExportIncludesSupersededAndRetracted(t *testing.T) {
	s, _ := seedAuditLog(t)
	events, err := s.ExportEvents(Export{Subject: "user"})
	if err != nil {
		t.Fatal(err)
	}
	var sawVim, sawRetraction bool
	for _, e := range events {
		if e.Predicate == "prefers_editor" && e.Value == "vim" {
			sawVim = true
		}
		if e.Retraction {
			sawRetraction = true
		}
	}
	if !sawVim {
		t.Error("the superseded vim statement is missing from the export")
	}
	if !sawRetraction {
		t.Error("the retraction is missing from the export")
	}
}

func TestExportBySubjectSpansPredicates(t *testing.T) {
	s, _ := seedAuditLog(t)
	events, err := s.ExportEvents(Export{Subject: "user"})
	if err != nil {
		t.Fatal(err)
	}
	// user: vim, neovim, likes go, retract go = 4. alice's emacs is excluded.
	if len(events) != 4 {
		t.Fatalf("exported %d events for user, want 4", len(events))
	}
	for _, e := range events {
		if e.Subject != "user" {
			t.Fatalf("another subject leaked into the export: %q", e.Subject)
		}
	}
}

func TestExportByPredicateSpansSubjects(t *testing.T) {
	s, _ := seedAuditLog(t)
	events, err := s.ExportEvents(Export{Predicate: "prefers_editor"})
	if err != nil {
		t.Fatal(err)
	}
	// user's vim and neovim, alice's emacs = 3.
	if len(events) != 3 {
		t.Fatalf("exported %d prefers_editor events, want 3", len(events))
	}
}

func TestExportLimitIsRespected(t *testing.T) {
	s, _ := seedAuditLog(t)
	events, err := s.ExportEvents(Export{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("exported %d events, want the limit of 2", len(events))
	}
}

func TestDigestIsIndependentOfOrder(t *testing.T) {
	s, _ := seedAuditLog(t)
	events, err := s.ExportEvents(Export{})
	if err != nil {
		t.Fatal(err)
	}
	reversed := make([]Event, len(events))
	for i, e := range events {
		reversed[len(events)-1-i] = e
	}
	if Digest(events) != Digest(reversed) {
		t.Fatal("the same log in a different order produced a different digest")
	}
}

func TestDigestChangesWhenAnyFieldChanges(t *testing.T) {
	s, _ := seedAuditLog(t)
	events, err := s.ExportEvents(Export{})
	if err != nil {
		t.Fatal(err)
	}
	base := Digest(events)

	mutations := map[string]func(*Event){
		"value":      func(e *Event) { e.Value = "tampered" },
		"confidence": func(e *Event) { e.Confidence = 0.5 },
		"source":     func(e *Event) { e.Source = "agent_inferred" },
		"retraction": func(e *Event) { e.Retraction = !e.Retraction },
		"instant":    func(e *Event) { e.ObservedAt = e.ObservedAt.Add(time.Nanosecond) },
		"subject":    func(e *Event) { e.Subject = "mallory" },
	}
	for name, mutate := range mutations {
		altered := make([]Event, len(events))
		copy(altered, events)
		mutate(&altered[2])
		if Digest(altered) == base {
			t.Errorf("changing %s did not change the digest", name)
		}
	}
}

func TestDigestDetectsARemovedEvent(t *testing.T) {
	s, _ := seedAuditLog(t)
	events, err := s.ExportEvents(Export{})
	if err != nil {
		t.Fatal(err)
	}
	// Dropping the retraction would make a forgotten fact look believed.
	var withoutRetraction []Event
	for _, e := range events {
		if !e.Retraction {
			withoutRetraction = append(withoutRetraction, e)
		}
	}
	if Digest(withoutRetraction) == Digest(events) {
		t.Fatal("removing an event did not change the digest")
	}
}

func TestDigestOfEmptyLogIsStable(t *testing.T) {
	if Digest(nil) != Digest([]Event{}) {
		t.Fatal("nil and empty logs digest differently")
	}
	if Digest(nil) == "" {
		t.Fatal("empty digest")
	}
}
