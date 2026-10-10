package engine

import (
	"context"
	"strings"
	"testing"
)

func TestAskTellsBeliefsAsSentences(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	remember(t, c, "favorite_editor", "helix", "")
	remember(t, c, "favorite_editor", "zed", "")
	remember(t, c, "years_coding", float64(12), "")
	got, err := c.Ask(ctx, "which editor is my favorite")
	if err != nil {
		t.Fatal(err)
	}
	var editor *Memory
	for i := range got {
		if strings.Contains(got[i].Text, "editor") {
			editor = &got[i]
		}
	}
	if editor == nil || editor.Text != "user favorite editor: zed" || len(editor.Before) != 1 || editor.Before[0].Text != "helix" || editor.Since.IsZero() {
		t.Fatalf("memories %+v", got)
	}
	if s := Sentence("user", "years_coding", float64(12)); s != "user years coding: 12" {
		t.Fatalf("number sentence %q", s)
	}
	if s := Sentence("user", NotePredicate, "I like tea."); s != "I like tea." {
		t.Fatalf("note sentence %q", s)
	}
	if _, err := c.Ask(ctx, " "); err == nil {
		t.Fatal("an empty question was accepted")
	}
}
