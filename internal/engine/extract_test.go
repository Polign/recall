package engine

import (
	"context"
	"errors"
	"testing"
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
		if calls == 2 {
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
