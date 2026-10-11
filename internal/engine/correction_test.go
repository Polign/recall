package engine

import (
	"testing"
	"time"
)

const correctionRegistry = `{
	"likes":    {"cardinality": "multi",  "value_type": "string", "description": "likes"},
	"dislikes": {"cardinality": "multi",  "value_type": "string", "description": "dislikes"},
	"diet":     {"cardinality": "single", "value_type": "string", "description": "diet"}
}`

func newCorrectionStore(t *testing.T) (*Store, *fakeDB, *time.Time) {
	t.Helper()
	reg, err := LoadRegistry([]byte(correctionRegistry))
	if err != nil {
		t.Fatal(err)
	}
	db := newFakeDB()
	clock := t0
	s := NewStore(db, "memories", reg, func(string) []float32 { return []float32{1, 0, 0} })
	s.now = func() time.Time { return clock }
	return s, db, &clock
}

func correct(t *testing.T, s *Store, predicate, value string, replaces ...BeliefRef) RememberResult {
	t.Helper()
	res, err := s.remember("fact", "user", predicate, value, 1, "user_stated", provenance{replaces: replaces})
	if err != nil {
		t.Fatalf("remember %s %s: %v", predicate, value, err)
	}
	return res
}

func held(t *testing.T, s *Store, predicate string, asOf time.Time) []any {
	t.Helper()
	return recallValues(t, s, Query{Subject: "user", Predicate: predicate, AsOf: asOf})
}

func TestCorrectionAcrossPredicates(t *testing.T) {
	s, _, clock := newCorrectionStore(t)
	correct(t, s, "likes", "mangoes")
	correct(t, s, "likes", "bananas")
	*clock = t0.Add(time.Hour)
	res := correct(t, s, "dislikes", "mangoes", BeliefRef{Predicate: "likes", Value: "Mangoes"})

	if len(res.Superseded) != 1 || res.Superseded[0].Predicate != "likes" || res.Superseded[0].Value != "mangoes" {
		t.Fatalf("superseded = %+v, want likes mangoes", res.Superseded)
	}
	if got := held(t, s, "likes", time.Time{}); len(got) != 1 || got[0] != "bananas" {
		t.Fatalf("likes = %v, want bananas only", got)
	}
	dislikes, err := s.Recall(Query{Subject: "user", Predicate: "dislikes"})
	if err != nil || len(dislikes) != 1 {
		t.Fatalf("dislikes = %+v, %v", dislikes, err)
	}
	r := dislikes[0].Replaced
	if len(r) != 1 || r[0].Predicate != "likes" || r[0].Value != "mangoes" || !r[0].ObservedAt.Equal(t0) {
		t.Fatalf("replaced = %+v, want likes mangoes from t0", r)
	}
	// The withdrawal is in the old belief's own history, linked to the
	// statement that made it.
	history, err := s.History("user", "likes")
	if err != nil {
		t.Fatal(err)
	}
	last := history[len(history)-1]
	if !last.Retraction || last.ReplacedBy != dislikes[0].EventID || !last.ObservedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("last likes event = %+v, want a retraction by %s at the correction", last, dislikes[0].EventID)
	}
	// What was believed before the correction is unchanged.
	if got := held(t, s, "likes", t0.Add(time.Minute)); len(got) != 2 {
		t.Fatalf("likes before the correction = %v, want both", got)
	}
}

func TestRepeatingAStatementAppliesItsCorrection(t *testing.T) {
	// The demo's state: both were said, nothing linked them.
	s, _, clock := newCorrectionStore(t)
	correct(t, s, "likes", "mangoes")
	*clock = t0.Add(time.Hour)
	correct(t, s, "dislikes", "mangoes")
	*clock = t0.Add(2 * time.Hour)
	res := correct(t, s, "dislikes", "mangoes", BeliefRef{Predicate: "likes", Value: "mangoes"})
	if !res.Existing || len(res.Superseded) != 1 {
		t.Fatalf("result = %+v, want the existing dislike withdrawing the like", res)
	}
	if got := held(t, s, "likes", time.Time{}); len(got) != 0 {
		t.Fatalf("likes = %v, want none", got)
	}
	// The dislike now says what it corrected, though it was first stated
	// without the correction.
	dislikes, err := s.Recall(Query{Subject: "user", Predicate: "dislikes"})
	if err != nil || len(dislikes) != 1 || len(dislikes[0].Replaced) != 1 || dislikes[0].Replaced[0].Predicate != "likes" {
		t.Fatalf("dislikes = %+v, %v; want one replacing likes mangoes", dislikes, err)
	}
	// Repeating it again finds nothing left to withdraw.
	*clock = t0.Add(3 * time.Hour)
	res = correct(t, s, "dislikes", "mangoes", BeliefRef{Predicate: "likes", Value: "mangoes"})
	if !res.Existing || len(res.Superseded) != 0 || len(res.Unreplaced) != 1 {
		t.Fatalf("second repeat = %+v, want nothing written and the ref reported", res)
	}
	if history, _ := s.History("user", "dislikes"); len(history) != 2 {
		t.Fatalf("dislikes history has %d events, want the statement and its corrected restatement", len(history))
	}
}

func TestCorrectionNeverWithdrawsWhatItCannot(t *testing.T) {
	s, _, clock := newCorrectionStore(t)
	if _, err := s.Remember("fact", "user", NotePredicate, "I love mangoes", 1, "user_stated"); err != nil {
		t.Fatal(err)
	}
	*clock = t0.Add(time.Hour)
	correct(t, s, "likes", "papaya")
	*clock = t0.Add(2 * time.Hour)
	res := correct(t, s, "diet", "fruit-free",
		BeliefRef{Predicate: "likes", Value: "kiwi"},                 // never believed
		BeliefRef{Predicate: NotePredicate, Value: "I love mangoes"}, // a record, not an interpretation
		BeliefRef{Predicate: "missing", Value: "x"},                  // no such predicate
		BeliefRef{Predicate: "likes", Value: "papaya"},
	)
	if len(res.Unreplaced) != 3 || len(res.Superseded) != 1 {
		t.Fatalf("result = %+v, want papaya withdrawn and three refs unreplaced", res)
	}
	if got := recallValues(t, s, Query{Subject: "user", Predicate: NotePredicate}); len(got) != 1 {
		t.Fatalf("notes = %v, want the note kept", got)
	}

	// A statement dated before a belief cannot correct it.
	*clock = t0.Add(3 * time.Hour)
	correct(t, s, "likes", "plums")
	res, err := s.remember("fact", "user", "dislikes", "plums", 1, "user_stated",
		provenance{observedAt: t0.Add(30 * time.Minute), replaces: []BeliefRef{{Predicate: "likes", Value: "plums"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Unreplaced) != 1 || len(res.Superseded) != 0 {
		t.Fatalf("backdated correction = %+v, want plums kept", res)
	}
}

func TestRelatedFindsTheSameValueUnderOtherPredicates(t *testing.T) {
	s, _, clock := newCorrectionStore(t)
	correct(t, s, "likes", "mangoes")
	correct(t, s, "likes", "bananas")
	*clock = t0.Add(time.Hour)
	res := correct(t, s, "dislikes", "mangoes")
	related, err := s.related(res.Stored, relatedLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(related) == 0 || related[0].Predicate != "likes" || related[0].Value != "mangoes" {
		t.Fatalf("related = %+v, want likes mangoes first", related)
	}
	for _, b := range related {
		if b.Predicate == "dislikes" {
			t.Fatalf("related lists the statement's own predicate: %+v", b)
		}
	}
}

func TestCorrectionLinksSurviveStorageAndAudit(t *testing.T) {
	s, _, clock := newCorrectionStore(t)
	correct(t, s, "likes", "mangoes")
	*clock = t0.Add(time.Hour)
	res := correct(t, s, "dislikes", "mangoes", BeliefRef{Predicate: "likes", Value: "mangoes"})

	history, err := s.History("user", "dislikes")
	if err != nil || len(history) != 1 || len(history[0].Replaces) != 1 || history[0].Replaces[0].EventID == "" {
		t.Fatalf("dislikes history = %+v, %v", history, err)
	}
	if _, err := DecodeEvent("x", map[string]any{"kind": "fact", "subject": "user", "predicate": "likes", "value": "a",
		"confidence": 1.0, "source": "user_stated", "observed_at": formatTime(t0), "replaces": "[]"}); err == nil {
		t.Fatal("an empty replaces list decoded")
	}

	*clock = t0.Add(2 * time.Hour)
	b, err := s.ExportAudit(AuditRequest{Scope: AuditScope{Subject: "user"}})
	if err != nil {
		t.Fatal(err)
	}
	if b.EventVersion != EventVersion {
		t.Fatalf("bundle event version %q, want %q", b.EventVersion, EventVersion)
	}
	got, err := b.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].EventID != res.Stored.EventID || len(got[0].Replaced) != 1 {
		t.Fatalf("replay = %+v, want only the dislike, replacing the like", got)
	}
	// An older event version cannot record the links.
	b.EventVersion = eventVersionV2
	if _, err := b.Replay(); err == nil {
		t.Fatal("a v2 bundle carrying corrections replayed")
	}

	plain, _, _ := newCorrectionStore(t)
	correct(t, plain, "likes", "mangoes")
	pb, err := plain.ExportAudit(AuditRequest{})
	if err != nil || pb.EventVersion != eventVersionV2 {
		t.Fatalf("plain bundle version %q, %v; want %q", pb.EventVersion, err, eventVersionV2)
	}
}
