package engine

import (
	"context"
	"testing"
)

func TestExplainShowsSourceNameRulesAndHistory(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	helix := &vocabularySpy{proposals: []Proposal{{Subject: "user", Predicate: "favorite_editor", Value: "helix", Evidence: "helix"}}}
	if _, err := c.RememberText(ctx, "I use helix.", helix); err != nil {
		t.Fatal(err)
	}
	zed := &vocabularySpy{proposals: []Proposal{{Subject: "user", Predicate: "Editor Favorite", Value: "zed", Evidence: "zed"}}}
	if _, err := c.RememberText(ctx, "Switched to zed this week.", zed); err != nil {
		t.Fatal(err)
	}
	got, err := c.Explain(ctx, "favorite editor")
	if err != nil {
		t.Fatal(err)
	}
	var e *Explanation
	for i := range got {
		if got[i].Predicate == "favorite_editor" {
			e = &got[i]
		}
	}
	if e == nil {
		t.Fatalf("no explanation for the editor in %+v", got)
	}
	if e.Memory != "user favorite editor: zed" || e.WrittenAs != "editor_favorite" || e.Evidence != "zed" || e.SourceText != "Switched to zed this week." {
		t.Fatalf("explanation %+v", *e)
	}
	if len(e.Rules) != 2 || e.Rules[0].Change != "defined on first use" || e.Rules[1].Change != "merged into favorite_editor" {
		t.Fatalf("rules %+v", e.Rules)
	}
	if len(e.History) != 2 {
		t.Fatalf("history has %d events, want 2", len(e.History))
	}
}
