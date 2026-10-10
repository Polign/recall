package engine

import (
	"context"
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
