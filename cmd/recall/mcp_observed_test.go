package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Polign/recall"
)

// An imported conversation says vim in March and emacs in May, written out
// of order. observed_at, not write order, decides what holds when.
func TestMCPRememberObservedAt(t *testing.T) {
	polignURL := polignServer(t)
	s := newAgentMCP(t, polignURL)

	may := mustTool(t, s, "remember", map[string]any{
		"text":        "I've switched to emacs for everything now.",
		"statements":  []map[string]any{{"subject": "user", "predicate": "prefers_editor", "value": "emacs", "evidence": "switched to emacs"}},
		"observed_at": "2023-05-01T09:00:00Z",
	})
	results, _ := may["results"].([]any)
	if len(results) != 1 || !strings.Contains(mustJSON(t, results[0]), `"observed_at":"2023-05-01T09:00:00Z"`) {
		t.Fatalf("text-mode remember = %v", may)
	}
	mustTool(t, s, "remember", map[string]any{"subject": "user", "predicate": "prefers_editor", "value": "vim", "observed_at": "2023-03-01T09:00:00Z"})

	now, err := callTool(t, s, "recall", map[string]any{"subject": "user", "predicate": "prefers_editor"})
	// The vim dated March is history, not the answer; it shows only as what
	// emacs replaced.
	if err != nil {
		t.Fatal(err)
	}
	var beliefs []recall.Belief
	if err := json.Unmarshal([]byte(now), &beliefs); err != nil {
		t.Fatal(err)
	}
	if len(beliefs) != 1 || beliefs[0].Value != "emacs" {
		t.Fatalf("recall now = %s; want emacs only", now)
	}
	if r := beliefs[0].Replaced; len(r) != 1 || r[0].Value != "vim" {
		t.Fatalf("emacs replaced %+v, want vim", r)
	}
	april, err := callTool(t, s, "recall", map[string]any{"subject": "user", "predicate": "prefers_editor", "as_of": "2023-04-01T00:00:00Z"})
	if err != nil || !strings.Contains(april, `"value":"vim"`) || strings.Contains(april, `"emacs"`) {
		t.Fatalf("recall as of April = %s, %v; want vim only", april, err)
	}

	for _, bad := range []string{"May 1st", "2999-01-01T00:00:00Z"} {
		if _, err := callTool(t, s, "remember", map[string]any{"subject": "user", "predicate": "prefers_editor", "value": "nano", "observed_at": bad}); err == nil {
			t.Errorf("observed_at %q was accepted", bad)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Facts remembered from text point at the text. A caller that asks with
// with_sources gets the excerpt and the whole text; every other output leaves
// the evidence fields out, because released Python clients reject fields they
// do not know.
func TestMCPRecallWithSources(t *testing.T) {
	polignURL := polignServer(t)
	s := newAgentMCP(t, polignURL)

	text := "user: I bought the new release from my favorite author, originally $30, on sale for $24."
	stored, err := callTool(t, s, "remember", map[string]any{
		"text":       text,
		"statements": []map[string]any{{"subject": "user", "predicate": "prefers_editor", "value": "neovim", "evidence": "originally $30, on sale for $24"}},
	})
	if err != nil || strings.Contains(stored, `"evidence_id"`) || !strings.Contains(stored, `"episode"`) {
		t.Fatalf("remember = %s, %v; want an episode and no evidence_id", stored, err)
	}

	plain, err := callTool(t, s, "recall", map[string]any{"subject": "user", "predicate": "prefers_editor"})
	if err != nil || strings.Contains(plain, "evidence") || strings.Contains(plain, "source_text") {
		t.Fatalf("plain recall = %s, %v; want no evidence fields", plain, err)
	}
	history, err := callTool(t, s, "memory_history", map[string]any{"subject": "user", "predicate": "prefers_editor"})
	if err != nil || strings.Contains(history, "evidence") {
		t.Fatalf("history = %s, %v; want no evidence fields", history, err)
	}

	sourced, err := callTool(t, s, "recall", map[string]any{"subject": "user", "predicate": "prefers_editor", "with_sources": true})
	if err != nil {
		t.Fatal(err)
	}
	var beliefs []map[string]any
	if err := json.Unmarshal([]byte(sourced), &beliefs); err != nil || len(beliefs) != 1 {
		t.Fatalf("with_sources recall = %s, %v", sourced, err)
	}
	if beliefs[0]["evidence"] != "originally $30, on sale for $24" || beliefs[0]["source_text"] != text || beliefs[0]["evidence_id"] == "" {
		t.Fatalf("with_sources belief = %v", beliefs[0])
	}
	// The belief's own source, how it was come by, must survive beside it.
	if beliefs[0]["source"] != "agent_inferred" {
		t.Fatalf("with_sources belief = %v", beliefs[0])
	}
}

func TestMCPRecallObservedWindow(t *testing.T) {
	polignURL := polignServer(t)
	s := newAgentMCP(t, polignURL)
	for _, w := range []struct{ value, at string }{{"vim", "2023-01-10T00:00:00Z"}, {"helix", "2023-03-10T00:00:00Z"}} {
		if _, err := callTool(t, s, "remember", map[string]any{"subject": "user", "predicate": "uses_technology", "value": w.value, "observed_at": w.at}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := callTool(t, s, "recall", map[string]any{"query": "technology", "observed_after": "2023-03-01T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "helix") || strings.Contains(got, "vim") {
		t.Fatalf("observed_after kept the wrong beliefs: %s", got)
	}
	aged, err := callTool(t, s, "recall", map[string]any{"query": "technology", "as_of": "2023-03-20T12:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(aged, `"days_ago":10`) || !strings.Contains(aged, `"days_ago":69`) {
		t.Fatalf("days_ago missing or wrong as of March 20: %s", aged)
	}
	if _, err := callTool(t, s, "recall", map[string]any{"query": "technology", "observed_before": "march"}); err == nil {
		t.Fatal("a bad observed_before was accepted")
	}
}
