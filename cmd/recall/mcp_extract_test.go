package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Polign/recall/model"
)

// rememberTextMode returns the fields the remember schema requires in text
// mode.
func rememberTextMode(t *testing.T, s *mcpServer) []string {
	t.Helper()
	for _, tool := range s.memoryTools() {
		if tool.Name == "remember" {
			return tool.InputSchema["oneOf"].([]any)[0].(map[string]any)["required"].([]string)
		}
	}
	t.Fatal("no remember tool")
	return nil
}

// With -extract-model, remember takes text alone and the model proposes the
// statements; without it, text alone is refused with a pointer to the flag.
func TestMCPRememberTextWithModel(t *testing.T) {
	polignURL := polignServer(t)
	s := newAgentMCP(t, polignURL)

	text := "I've switched to emacs for everything now."
	if got := rememberTextMode(t, s); len(got) != 2 {
		t.Fatalf("text mode without a model requires %v; want text and statements", got)
	}
	if _, err := callTool(t, s, "remember", map[string]any{"text": text}); err == nil || !strings.Contains(err.Error(), "-extract-model") {
		t.Fatalf("text alone without a model: %v", err)
	}

	var calls atomic.Int32
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !strings.Contains(body.Messages[0].Content, "Today's date: 2023-05-01") {
			t.Errorf("prompt is not dated by observed_at:\n%s", body.Messages[0].Content)
		}
		reply := `{"statements":[{"subject":"user","predicate":"prefers_editor","value":"emacs","evidence":"switched to emacs"}]}`
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": reply}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(llm.Close)
	extractor, err := model.NewExtractor(model.Config{Provider: "ollama", Model: "test", BaseURL: llm.URL})
	if err != nil {
		t.Fatal(err)
	}
	s.memory.extractor = extractor

	if got := rememberTextMode(t, s); len(got) != 1 || got[0] != "text" {
		t.Fatalf("text mode with a model requires %v; want text only", got)
	}
	out := mustTool(t, s, "remember", map[string]any{"text": text, "observed_at": "2023-05-01T09:00:00Z"})
	if results, _ := out["results"].([]any); len(results) != 1 || !strings.Contains(mustJSON(t, results[0]), `"value":"emacs"`) {
		t.Fatalf("remember text alone = %v", out)
	}
	now, err := callTool(t, s, "recall", map[string]any{"subject": "user", "predicate": "prefers_editor"})
	if err != nil || !strings.Contains(now, `"value":"emacs"`) {
		t.Fatalf("recall = %s, %v", now, err)
	}

	// Statements the agent proposes still win over the model.
	mustTool(t, s, "remember", map[string]any{
		"text":       "Actually I use vim.",
		"statements": []map[string]any{{"subject": "user", "predicate": "prefers_editor", "value": "vim", "evidence": "I use vim"}},
	})
	if calls.Load() != 1 {
		t.Fatalf("model called %d times; want once", calls.Load())
	}
}
