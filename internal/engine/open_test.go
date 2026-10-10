package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func openClient(t *testing.T, b Backend) *Client {
	t.Helper()
	c, err := NewClient(Config{Backend: b, Collection: "memories", Embedder: LexicalEmbedder{}, Open: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func beliefValues(bs []Belief) []any {
	out := make([]any, len(bs))
	for i, b := range bs {
		out[i] = b.Value
	}
	return out
}

func TestOpenDefinesPredicateOnFirstWrite(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	r, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "Favorite Editor", Value: "helix"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Stored.Predicate != "favorite_editor" {
		t.Fatalf("stored under %q, want favorite_editor", r.Stored.Predicate)
	}
	r, err = c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "favorite-editor", Value: "zed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Superseded) != 1 || r.Superseded[0].Value != "helix" {
		t.Fatalf("superseded %+v, want helix: a new predicate is single-valued", r.Superseded)
	}
	vocab, err := c.Vocabulary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p := vocab["favorite_editor"]; p.Cardinality != "single" || p.valueType() != typeString {
		t.Fatalf("definition %+v", p)
	}
	log, err := c.RegistryLog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || !log[0].Auto || log[0].Name != "favorite_editor" {
		t.Fatalf("registry log %+v, want one automatic definition", log)
	}
}

func TestOpenMultiHintAccumulates(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	for _, v := range []string{"go", "rust"} {
		if _, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "uses_technology", Value: v, Cardinality: Multi, Description: "Technology the subject uses"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.Recall(ctx, Query{Subject: "user", Predicate: "uses_technology"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("beliefs %v, want go and rust", values(got))
	}
}

func TestOpenInfersValueType(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	if _, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "years_coding", Value: float64(12)}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "years_coding", Value: "twelve"}); err == nil {
		t.Fatal("a string was accepted for a predicate first written as a number")
	}
	if _, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "registry change", Value: "x"}); err == nil {
		t.Fatal("the registry's own predicate was defined by a write")
	}
}

func TestClosedClientReadsButRefusesLogDefinedPredicates(t *testing.T) {
	ctx := context.Background()
	b := newLockedBackend()
	if _, err := openClient(t, b).Remember(ctx, RememberRequest{Subject: "user", Predicate: "favorite_editor", Value: "helix"}); err != nil {
		t.Fatal(err)
	}
	closed, err := NewClient(Config{Backend: b, Collection: "memories", Registry: testRegistry(t), Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := closed.Recall(ctx, Query{Subject: "user", Predicate: "favorite_editor"})
	if err != nil || len(got) != 1 || got[0].Value != "helix" {
		t.Fatalf("closed read = %v, %v", values(got), err)
	}
	if _, err := closed.Remember(ctx, RememberRequest{Subject: "user", Predicate: "favorite_editor", Value: "zed"}); err == nil {
		t.Fatal("a closed client wrote a predicate its registry does not list")
	}
	if _, err := closed.Remember(ctx, RememberRequest{Subject: "user", Predicate: "brand_new", Value: "x"}); err == nil {
		t.Fatal("a closed client defined a predicate")
	}
}

func TestConcurrentAutoDefinitionsFirstWins(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	s, err := c.forContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Two clients that met the same new name at once each record a
	// definition; neither saw the other's.
	first := registryRecord{Name: "lives_in", Auto: true, Predicate: Predicate{Cardinality: "multi", ValueType: typeString}}
	second := registryRecord{Name: "lives_in", Auto: true, Predicate: Predicate{Cardinality: "single", ValueType: typeString}}
	ev, err := s.appendDefinition(nil, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.appendDefinition([]Event{ev}, second); err != nil {
		t.Fatal(err)
	}
	s.reglog.invalidate()
	vocab, err := c.Vocabulary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if vocab["lives_in"].Cardinality != "multi" {
		t.Fatalf("lives_in is %q, want the first definition (multi)", vocab["lives_in"].Cardinality)
	}
}

func TestRedefineRestoresHiddenValues(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	for _, v := range []string{"peanuts", "shellfish"} {
		if _, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "allergy", Value: v}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.Recall(ctx, Query{Subject: "user", Predicate: "allergy"})
	if err != nil || len(got) != 1 {
		t.Fatalf("before redefining: %v, %v; the single-valued guess should hide peanuts", values(got), err)
	}
	beforeFix := time.Now()

	if err := c.Redefine(ctx, "allergy", Predicate{Cardinality: "multi"}); err != nil {
		t.Fatal(err)
	}
	c.reglog.invalidate()
	for name, q := range map[string]Query{
		"now":              {Subject: "user", Predicate: "allergy"},
		"as of before fix": {Subject: "user", Predicate: "allergy", AsOf: beforeFix},
	} {
		got, err := c.Recall(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		vs := values(got)
		if len(vs) != 2 || !slices.Contains(vs, any("peanuts")) || !slices.Contains(vs, any("shellfish")) {
			t.Fatalf("%s: %v, want peanuts and shellfish", name, vs)
		}
	}
	if err := c.Redefine(ctx, "allergy", Predicate{Cardinality: "multi", ValueType: typeNumber}); !errors.Is(err, ErrRegistryMismatch) {
		t.Fatalf("type change = %v, want ErrRegistryMismatch", err)
	}
}

func TestAuditFoldVersionFollowsRegistryRules(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	if _, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "allergy", Value: "peanuts"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "allergy", Value: "shellfish"}); err != nil {
		t.Fatal(err)
	}
	scope := AuditRequest{Scope: AuditScope{Subject: "user", Predicate: "allergy"}}
	b, err := c.ExportAudit(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if b.FoldVersion != foldVersionV2 {
		t.Fatalf("plain open log exported as %q, want %q", b.FoldVersion, foldVersionV2)
	}
	if err := c.Redefine(ctx, "allergy", Predicate{Cardinality: "multi"}); err != nil {
		t.Fatal(err)
	}
	c.reglog.invalidate()
	b, err = c.ExportAudit(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if b.FoldVersion != FoldVersion {
		t.Fatalf("redefined log exported as %q, want %q", b.FoldVersion, FoldVersion)
	}
	got, err := b.Replay()
	if err != nil || len(got) != 2 {
		t.Fatalf("replay = %v, %v; want both allergies", values(got), err)
	}
	b.FoldVersion = foldVersionV2
	if _, err := b.Replay(); !errors.Is(err, ErrInvalidAudit) {
		t.Fatalf("v2 replay of a v3 log = %v, want ErrInvalidAudit", err)
	}
}

// vocabularySpy records the registry each extraction is offered.
type vocabularySpy struct {
	proposals []Proposal
	offered   []Registry
}

func (v *vocabularySpy) Extract(_ context.Context, _ string, r Registry) ([]Proposal, error) {
	v.offered = append(v.offered, r)
	return append([]Proposal(nil), v.proposals...), nil
}

func TestOpenRememberTextDefinesCoinedPredicates(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	text := "I'm allergic to peanuts and shellfish."
	spy := &vocabularySpy{proposals: []Proposal{
		{Subject: "user", Predicate: "Allergic To", Value: "peanuts", Evidence: "allergic to peanuts", Cardinality: "multi", Description: "Something the subject is allergic to"},
		{Subject: "user", Predicate: "allergic_to", Value: "shellfish", Evidence: "shellfish"},
	}}
	out, err := c.RememberText(ctx, text, spy)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Unfiled) != 0 {
		t.Fatalf("unfiled %+v: an open client files coined predicates", out.Unfiled)
	}
	got, err := c.Recall(ctx, Query{Subject: "user", Predicate: "allergic_to"})
	if err != nil || len(got) != 2 {
		t.Fatalf("allergies %v, %v; the multi hint should keep both", beliefValues(got), err)
	}
	if got[0].EvidenceID != out.Episode.Stored.EventID {
		t.Fatalf("statement not linked to its episode")
	}
	if _, err := c.RememberText(ctx, "Also latex.", &vocabularySpy{}); err != nil {
		t.Fatal(err)
	}
	second := &vocabularySpy{}
	if _, err := c.RememberText(ctx, "Nothing new.", second); err != nil {
		t.Fatal(err)
	}
	if p, ok := second.offered[0]["allergic_to"]; !ok || p.Description != "Something the subject is allergic to" {
		t.Fatalf("extractor was offered %v, want the learned allergic_to", second.offered[0].Names())
	}
	bad := &vocabularySpy{proposals: []Proposal{{Subject: "user", Predicate: "x", Value: "y", Evidence: "Bad.", Cardinality: "several"}}}
	if _, err := c.RememberText(ctx, "Bad.", bad); err == nil {
		t.Fatal("an unknown cardinality was accepted")
	}
}
