package engine

import (
	"context"
	"errors"
	"testing"
)

// scriptedExtractor answers each call with the next batch, and counts calls,
// standing in for a model that would file the same text differently twice.
type scriptedExtractor struct {
	calls   int
	batches [][]Proposal
	open    []bool
}

func (x *scriptedExtractor) Extract(ctx context.Context, _ string, _ Registry) ([]Proposal, error) {
	x.open = append(x.open, OpenVocabulary(ctx))
	b := x.batches[min(x.calls, len(x.batches)-1)]
	x.calls++
	return append([]Proposal(nil), b...), nil
}

func editorClient(t *testing.T, b Backend) *Client {
	t.Helper()
	c, err := NewClient(Config{Backend: b, Collection: "memory", Registry: DefaultRegistry(), Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func editor(t *testing.T, c *Client) any {
	t.Helper()
	got, err := c.Recall(context.Background(), Query{Subject: "user", Predicate: "prefers_editor"})
	if err != nil || len(got) != 1 {
		t.Fatalf("prefers_editor = %v, %v", beliefValues(got), err)
	}
	return got[0].Value
}

func TestRetriedTextReplaysTheFirstExtraction(t *testing.T) {
	ctx := context.Background()
	underlying := newLockedBackend()
	puts, failAt := 0, 3 // the episode, its extraction record, then the statement
	b := backendFuncs{list: underlying.List, search: underlying.Search, put: func(ctx context.Context, c, id string, v []float32, m map[string]any) error {
		puts++
		if puts == failAt {
			return errors.New("crashed")
		}
		return underlying.Put(ctx, c, id, v, m)
	}}
	c := editorClient(t, b)
	text := "I use helix these days."
	x := &scriptedExtractor{batches: [][]Proposal{
		{{Subject: "user", Predicate: "prefers_editor", Value: "helix", Evidence: "helix"}},
		// What a second model call would have said instead.
		{{Subject: "user", Predicate: "uses_technology", Value: "helix", Evidence: "helix"}},
	}}
	if _, err := c.RememberText(ctx, text, x); err == nil {
		t.Fatal("the simulated crash did not surface")
	}
	if _, err := c.RememberText(ctx, text, x); err != nil {
		t.Fatal(err)
	}
	if x.calls != 1 {
		t.Fatalf("the model was asked %d times; a retry must replay the first answer", x.calls)
	}
	if got := editor(t, c); got != "helix" {
		t.Fatalf("editor %v, want helix", got)
	}
	tech, err := c.Recall(ctx, Query{Subject: "user", Predicate: "uses_technology"})
	if err != nil || len(tech) != 0 {
		t.Fatalf("uses_technology %v, %v; the second answer must not be filed", beliefValues(tech), err)
	}
}

func TestRestatedTextIsCurrentAgain(t *testing.T) {
	ctx := context.Background()
	c := editorClient(t, newLockedBackend())
	vim := &scriptedExtractor{batches: [][]Proposal{{{Subject: "user", Predicate: "prefers_editor", Value: "vim", Evidence: "vim"}}}}
	zed := &scriptedExtractor{batches: [][]Proposal{{{Subject: "user", Predicate: "prefers_editor", Value: "zed", Evidence: "zed"}}}}
	for _, step := range []struct {
		text string
		x    *scriptedExtractor
		want string
	}{{"I prefer vim.", vim, "vim"}, {"I prefer zed.", zed, "zed"}, {"I prefer vim.", vim, "vim"}} {
		if _, err := c.RememberText(ctx, step.text, step.x); err != nil {
			t.Fatal(err)
		}
		if got := editor(t, c); got != step.want {
			t.Fatalf("after %q: editor %v, want %s", step.text, got, step.want)
		}
	}
	if vim.calls != 1 {
		t.Fatalf("restated text asked the model %d times, want 1", vim.calls)
	}
	if vim.open[0] {
		t.Fatal("a closed client told the extractor it may coin predicates")
	}
}

func TestExtractionRecordsStayOutOfAnswers(t *testing.T) {
	ctx := context.Background()
	b := newLockedBackend()
	c := editorClient(t, b)
	x := &scriptedExtractor{batches: [][]Proposal{{{Subject: "user", Predicate: "prefers_editor", Value: "vim", Evidence: "vim"}}}}
	if _, err := c.RememberText(ctx, "I prefer vim for extraction work.", x); err != nil {
		t.Fatal(err)
	}
	for _, q := range []Query{{Text: "recall extraction vim"}, {Subject: ExtractionSubject}} {
		got, err := c.Recall(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, bl := range got {
			if bl.Subject == ExtractionSubject {
				t.Fatalf("query %+v answered with an extraction record", q)
			}
		}
	}
	// Statements the caller proposed are not recorded.
	if _, err := c.RememberText(ctx, "I prefer zed.", ProposedStatements{{Subject: "user", Predicate: "prefers_editor", Value: "zed", Evidence: "zed"}}); err != nil {
		t.Fatal(err)
	}
	_, total, err := b.List(ctx, "memory", map[string]any{"subject": ExtractionSubject}, 10)
	if err != nil || total != 1 {
		t.Fatalf("%d extraction records, %v; want only the model's", total, err)
	}
}
