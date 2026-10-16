package engine

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
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

func TestAskTellsTheUserByName(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	remember(t, c, "name", "Maya", "")
	remember(t, c, "favorite_editor", "helix", "")
	remember(t, c, "favorite_editor", "zed", "")
	texts := func(question string) []string {
		t.Helper()
		got, err := c.Ask(ctx, question)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range got {
			out = append(out, m.Text)
		}
		return out
	}
	got := texts("which editor does Maya like")
	if !slices.Contains(got, "Maya favorite editor: zed") {
		t.Errorf("memories %v, want the editor told under Maya's name", got)
	}
	if slices.Contains(got, "user name: Maya") {
		t.Errorf("memories %v: a question naming Maya got her name back", got)
	}
	if got := texts("what is the user's name"); !slices.Contains(got, "user name: Maya") {
		t.Errorf("memories %v, want the name", got)
	}
}

func TestKeepRelevant(t *testing.T) {
	in := []Belief{
		{Predicate: "diet", Relevance: 0.59},
		{Predicate: "allergic_to", Relevance: 0.36},
		{Predicate: "email", Relevance: 0.06},
		{Predicate: "keyword_only"},
	}
	var got []string
	for _, b := range keepRelevant(in, 0.5) {
		got = append(got, b.Predicate)
	}
	if strings.Join(got, ",") != "diet,allergic_to,keyword_only" {
		t.Fatalf("kept %v", got)
	}
	unscored := []Belief{{Predicate: "a"}, {Predicate: "b"}}
	if len(keepRelevant(unscored, 0.5)) != 2 {
		t.Fatal("beliefs without scores were dropped")
	}
}

func TestAskWithoutAQuestionTellsTheProfile(t *testing.T) {
	ctx := context.Background()
	c := openClient(t, newLockedBackend())
	at := func(day int, subject, predicate string, value any) {
		t.Helper()
		if _, err := c.Remember(ctx, RememberRequest{Subject: subject, Predicate: predicate, Value: value,
			ObservedAt: t0.Add(time.Duration(day) * 24 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	at(0, "user", "name", "Maya")
	at(0, "user", "lives_in", "Seattle")
	at(1, "priya", "lives_in", "Denver")
	at(5, "user", "lives_in", "Portland")
	at(6, "user", "favorite_editor", "zed")

	texts := func(r AskRequest) []string {
		t.Helper()
		got, err := c.AskWith(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range got {
			s := m.Text
			for _, b := range m.Before {
				s += " (was " + b.Text + ")"
			}
			out = append(out, s)
		}
		return out
	}

	// No question: everything believed about the user, current values only.
	profile := texts(AskRequest{})
	for _, want := range []string{"user name: Maya", "Maya lives in: Portland (was Seattle)", "Maya favorite editor: zed"} {
		if !slices.Contains(profile, want) {
			t.Errorf("profile %v lacks %q", profile, want)
		}
	}
	if len(profile) != 3 {
		t.Errorf("profile %v, want the user's three beliefs only", profile)
	}

	// Since: what changed after day 2, about anyone, newest first.
	changed := texts(AskRequest{Since: t0.Add(2 * 24 * time.Hour)})
	if strings.Join(changed, " | ") != "Maya favorite editor: zed | Maya lives in: Portland (was Seattle)" {
		t.Errorf("changed since day 2: %v", changed)
	}
	if got := texts(AskRequest{Since: t0.Add(30 * 24 * time.Hour)}); len(got) != 0 {
		t.Errorf("nothing changed after day 30, got %v", got)
	}
}
