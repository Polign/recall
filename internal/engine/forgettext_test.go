package engine

import (
	"context"
	"strings"
	"testing"
)

type selectFunc func(request string, candidates []Belief) []int

func (f selectFunc) Select(_ context.Context, request string, candidates []Belief) ([]int, error) {
	return f(request, candidates), nil
}

func TestForgetTextWithdrawsWhatTheSelectorNames(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	for _, r := range []RememberRequest{
		{Subject: "user", Predicate: "favorite_editor", Value: "zed"},
		{Subject: "user", Predicate: "lives_in", Value: "Lisbon"},
	} {
		if _, err := c.Remember(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	var shown []Belief
	pickEditor := selectFunc(func(_ string, cands []Belief) []int {
		shown = cands
		for i, b := range cands {
			if b.Predicate == "favorite_editor" {
				return []int{i, i}
			}
		}
		return nil
	})
	out, err := c.ForgetText(ctx, "forget my favorite editor", pickEditor)
	if err != nil {
		t.Fatal(err)
	}
	if len(shown) == 0 || len(out.Withdrawn) != 1 || out.Withdrawn[0].Value != "zed" {
		t.Fatalf("withdrawn %+v from %d candidates", out.Withdrawn, len(shown))
	}
	if got, _ := c.Recall(ctx, Query{Subject: "user", Predicate: "favorite_editor"}); len(got) != 0 {
		t.Fatalf("editor still believed: %v", beliefValues(got))
	}
	if got, _ := c.Recall(ctx, Query{Subject: "user", Predicate: "lives_in"}); len(got) != 1 {
		t.Fatal("a statement the selector did not name was withdrawn")
	}
	history, err := c.History(ctx, "user", "favorite_editor")
	if err != nil || len(history) != 2 {
		t.Fatalf("history %d events, %v; forgetting records a retraction", len(history), err)
	}
	if _, err := c.ForgetText(ctx, "forget where I live", selectFunc(func(string, []Belief) []int { return []int{99} })); err == nil {
		t.Fatal("an out-of-range choice was accepted")
	}
}

// TestForgetTextWithdrawsFactsNotTheirSource reproduces a live run in which
// the model, asked to forget "my editor", also chose the texts that mention
// an editor, withdrawing a record that held where the user lives.
func TestForgetTextWithdrawsFactsNotTheirSource(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	text := "I use helix as my editor. I live in Lisbon."
	x := &vocabularySpy{proposals: []Proposal{
		{Subject: "user", Predicate: "prefers_editor", Value: "helix", Evidence: "helix as my editor"},
		{Subject: "user", Predicate: "lives_in", Value: "Lisbon", Evidence: "I live in Lisbon"},
	}}
	if _, err := c.RememberText(ctx, text, x); err != nil {
		t.Fatal(err)
	}
	greedy := selectFunc(func(_ string, cands []Belief) []int {
		var out []int
		for i, b := range cands {
			if strings.Contains(Sentence(b.Subject, b.Predicate, b.Value), "editor") {
				out = append(out, i)
			}
		}
		return out
	})
	out, err := c.ForgetText(ctx, "forget my editor", greedy)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Withdrawn) != 1 || out.Withdrawn[0].Predicate != "prefers_editor" {
		t.Fatalf("withdrawn %+v, want only the editor fact", out.Withdrawn)
	}
	notes, err := c.Recall(ctx, Query{Subject: "user", Predicate: NotePredicate})
	if err != nil || len(notes) != 1 || notes[0].Value != text {
		t.Fatalf("the source text was withdrawn: %v, %v", beliefValues(notes), err)
	}
}

func TestAskHidesTextsItFiledStatementsFrom(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	filed := &vocabularySpy{proposals: []Proposal{{Subject: "user", Predicate: "lives_in", Value: "Lisbon", Evidence: "Lisbon"}}}
	if _, err := c.RememberText(ctx, "I live in Lisbon.", filed); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RememberText(ctx, "Lisbon trams are lovely in spring.", &vocabularySpy{}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Ask(ctx, "Lisbon")
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, m := range got {
		texts = append(texts, m.Text)
	}
	joined := strings.Join(texts, " | ")
	if strings.Contains(joined, "I live in Lisbon.") || !strings.Contains(joined, "user lives in: Lisbon") || !strings.Contains(joined, "Lisbon trams are lovely in spring.") {
		t.Fatalf("memories: %s", joined)
	}
}
