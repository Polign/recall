package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestExtractValidateWholeBatchThenFold(t *testing.T) {
	b := newLockedBackend()
	c, err := NewClient(Config{Backend: b, Collection: "memory", Registry: DefaultRegistry(), Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	text := "I prefer vim. Now I prefer neovim."
	p := ProposedStatements{{Subject: "user", Predicate: "prefers_editor", Value: "vim", Evidence: "I prefer vim."}, {Subject: "user", Predicate: "invented", Value: "neovim", Evidence: "Now I prefer neovim."}}
	p[1].Predicate = "prefers_editor"
	p[1].Evidence = "not in input"
	if _, err := c.RememberText(t.Context(), text, p); err == nil {
		t.Fatal("fabricated evidence accepted")
	}
	p[1].Evidence = "Now I prefer neovim."
	out, err := c.RememberText(t.Context(), text, p)
	if err != nil || len(out.Results) != 2 || len(out.Results[1].Superseded) != 1 {
		t.Fatalf("extract fold: %+v %v", out, err)
	}
	got, err := c.Recall(t.Context(), Query{Subject: "user", Predicate: "prefers_editor"})
	if err != nil || len(got) != 1 || got[0].Value != "neovim" || got[0].Source != "agent_inferred" {
		t.Fatalf("belief: %v %v", got, err)
	}
}

func TestExtractionPartialWriteAndFalseZero(t *testing.T) {
	underlying := newLockedBackend()
	calls := 0
	failed := errors.New("write failed")
	b := backendFuncs{list: underlying.List, search: underlying.Search, put: func(ctx context.Context, c, id string, v []float32, m map[string]any) error {
		calls++
		if calls == 3 { // the episode, then the first statement, then this one
			return failed
		}
		return underlying.Put(ctx, c, id, v, m)
	}}
	r := Registry{"enabled": {Cardinality: "single", ValueType: "boolean"}, "port": {Cardinality: "single", ValueType: "number"}}
	c, err := NewClient(Config{Backend: b, Collection: "m", Registry: r, Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	p := ProposedStatements{{Subject: "project", Predicate: "enabled", Value: false, Evidence: "disabled"}, {Subject: "project", Predicate: "port", Value: float64(0), Evidence: "port 0"}}
	out, err := c.RememberText(t.Context(), "disabled, port 0", p)
	if !errors.Is(err, failed) || len(out.Results) != 1 || out.Results[0].Stored.Value != false {
		t.Fatalf("partial: %+v %v", out, err)
	}
	if out.Episode == nil || out.Episode.Stored.Value != "disabled, port 0" {
		t.Fatalf("the text was not kept before the statements: %+v", out.Episode)
	}
}

func TestExtractUnknownPredicateKeepsNote(t *testing.T) {
	c, err := NewClient(Config{Backend: newLockedBackend(), Collection: "memory", Registry: DefaultRegistry(), Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	text := "I prefer neovim. I use fish as my shell."
	p := ProposedStatements{
		{Subject: "user", Predicate: "prefers_editor", Value: "neovim", Evidence: "I prefer neovim."},
		{Subject: "User", Predicate: "prefers_shell", Value: "fish", Evidence: "I use fish as my shell."},
		{Subject: "user", Predicate: "Shell Choice", Value: "fish", Evidence: "fish"},
	}
	out, err := c.RememberText(t.Context(), text, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Unfiled) != 2 || len(out.Results) != 2 || out.Results[1].Stored.Predicate != NotePredicate {
		t.Fatalf("unfiled proposals must become one note per subject: %+v", out)
	}
	notes, err := c.Recall(t.Context(), Query{Subject: "user", Predicate: NotePredicate})
	if err != nil || len(notes) != 1 || notes[0].Value != text || notes[0].Confidence != noteConfidence || notes[0].Source != "agent_inferred" {
		t.Fatalf("note: %+v %v", notes, err)
	}
	editor, err := c.Recall(t.Context(), Query{Subject: "user", Predicate: "prefers_editor"})
	if err != nil || len(editor) != 1 || editor[0].Value != "neovim" {
		t.Fatalf("a registered proposal beside an unfiled one must still be filed: %+v %v", editor, err)
	}
}

func TestExtractNothingProposedKeepsNote(t *testing.T) {
	c, err := NewClient(Config{Backend: newLockedBackend(), Collection: "memory", Registry: DefaultRegistry(), Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	text := "My daughter's recital is on Friday."
	out, err := c.RememberText(t.Context(), text, ProposedStatements{})
	if err != nil || len(out.Results) != 1 {
		t.Fatalf("empty extraction: %+v %v", out, err)
	}
	got, err := c.Recall(t.Context(), Query{Subject: DefaultSubject, Predicate: NotePredicate})
	if err != nil || len(got) != 1 || got[0].Value != text {
		t.Fatalf("text with no proposals must be kept: %+v %v", got, err)
	}
	// Restating the same text adds nothing: a note folds like any multi value.
	if out, err := c.RememberText(t.Context(), text, ProposedStatements{}); err != nil || !out.Results[0].Existing {
		t.Fatalf("repeat: %+v %v", out, err)
	}
}

func TestExtractBadValueForRegisteredPredicateStillFails(t *testing.T) {
	r := Registry{"port": {Cardinality: "single", ValueType: "number"}}
	b := newLockedBackend()
	c, err := NewClient(Config{Backend: b, Collection: "m", Registry: r, Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	p := ProposedStatements{{Subject: "project", Predicate: "flavor", Value: "x", Evidence: "port"}, {Subject: "project", Predicate: "port", Value: "8000", Evidence: "port"}}
	if _, err := c.RememberText(t.Context(), "port 8000", p); err == nil {
		t.Fatal("a correctable value error must fail the batch, not become a note")
	}
	if got, _ := c.Recall(t.Context(), Query{Subject: "project", Predicate: NotePredicate}); len(got) != 0 {
		t.Fatalf("failed batch wrote a note: %+v", got)
	}
}

func TestAdmitProposalsDropsWhatRememberTextRefuses(t *testing.T) {
	r := Registry{"port": {Cardinality: "single", ValueType: "number"}, "enabled": {Cardinality: "single", ValueType: "boolean"}, "prefers_editor": {Cardinality: "single", ValueType: "string"}}
	text := "I use neovim. The port is 8080 and it is turned on."
	got := r.AdmitProposals(text, []Proposal{
		{Subject: "user", Predicate: "prefers_editor", Value: "neovim", Evidence: "I use neovim."},
		{Subject: "user", Predicate: "prefers_editor", Value: "vim", Evidence: "not in the text"},
		{Subject: "app", Predicate: "port", Value: "8080", Evidence: "The port is 8080"},
		{Subject: "app", Predicate: "port", Value: "eighty", Evidence: "The port is 8080"},
		{Subject: "app", Predicate: "enabled", Value: "true", Evidence: "it is turned on"},
		{Subject: " ", Predicate: "prefers_editor", Value: "neovim", Evidence: "I use neovim."},
		{Subject: "user", Predicate: "invented", Value: "x", Evidence: "I use neovim."},
	})
	if len(got) != 4 || got[1].Value != float64(8080) || got[2].Value != true || got[3].Predicate != "invented" {
		t.Fatalf("admitted: %+v", got)
	}
	c, err := NewClient(Config{Backend: newLockedBackend(), Collection: "m", Registry: r, Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.RememberText(t.Context(), text, ProposedStatements(got))
	if err != nil || len(out.Results) != 4 || len(out.Unfiled) != 1 {
		t.Fatalf("admitted proposals refused: %+v %v", out, err)
	}
}

type datedExtractor struct{ seen time.Time }

func (d *datedExtractor) Extract(ctx context.Context, _ string, _ Registry) ([]Proposal, error) {
	d.seen = ObservedAt(ctx)
	return nil, nil
}

func TestExtractorSeesObservedAt(t *testing.T) {
	c, err := NewClient(Config{Backend: newLockedBackend(), Collection: "m", Registry: DefaultRegistry(), Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	d := &datedExtractor{}
	if _, err := c.RememberText(t.Context(), "hello", d); err != nil || !d.seen.IsZero() {
		t.Fatalf("now: %v %v", d.seen, err)
	}
	at := time.Date(2023, 5, 20, 2, 21, 0, 0, time.UTC)
	if _, err := c.RememberTextAt(t.Context(), "hello again", d, at); err != nil || !d.seen.Equal(at) {
		t.Fatalf("dated: %v %v", d.seen, err)
	}
}
