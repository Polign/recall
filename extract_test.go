package recall

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
	if _, err := c.RememberText(t.Context(), text, p); err == nil {
		t.Fatal("unknown predicate accepted")
	}
	h, err := c.History(t.Context(), "user", "prefers_editor")
	if err != nil || len(h) != 0 {
		t.Fatalf("invalid batch wrote a prefix: %v %v", h, err)
	}
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
