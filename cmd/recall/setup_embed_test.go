package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Polign/recall/internal/recallsetup"
)

// fakeOllama answers /api/tags with the given models and records pulls.
func fakeOllama(t *testing.T, models ...map[string]any) (*atomic.Int32, string) {
	t.Helper()
	var pulls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
		case "/api/pull":
			pulls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "success"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &pulls, srv.URL
}

func TestSetupEmbedding(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("VOYAGE_API_KEY", "")
	t.Setenv("OLLAMA_HOST", "127.0.0.1:1") // nothing listens: no Ollama
	for _, tc := range []struct {
		name   string
		cfg    recallsetup.Config
		flag   string
		follow bool
		want   string
		named  bool
	}{
		{"named model", recallsetup.Config{}, "ollama", false, "ollama:nomic-embed-text", true},
		{"named lexical", recallsetup.Config{ExtractModel: "openai:gpt-5-mini"}, "lexical", true, "", true},
		{"openai follows", recallsetup.Config{ExtractModel: "openai:gpt-5-mini"}, "", true, "openai:text-embedding-3-small@512", false},
		{"ollama follows", recallsetup.Config{ExtractModel: "ollama:qwen3:8b"}, "", true, "ollama:nomic-embed-text", false},
		{"typed tools do not", recallsetup.Config{ExtractModel: "openai:gpt-5-mini", Typed: true}, "", true, "", false},
		{"anthropic, no Ollama", recallsetup.Config{ExtractModel: "anthropic:claude-opus-5-5"}, "", true, "", false},
		{"saved setup keeps", recallsetup.Config{ExtractModel: "openai:gpt-5-mini", EmbedModel: "voyage:voyage-3.5"}, "", false, "voyage:voyage-3.5", false},
		{"saved lexical keeps", recallsetup.Config{ExtractModel: "openai:gpt-5-mini"}, "", false, "", false},
	} {
		got, named, err := setupEmbedding(tc.cfg, tc.flag, tc.follow, strings.NewReader(""), false, io.Discard)
		if err != nil || got != tc.want || named != tc.named {
			t.Errorf("%s: %q named=%v, %v; want %q named=%v", tc.name, got, named, err, tc.want, tc.named)
		}
	}
	if _, _, err := setupEmbedding(recallsetup.Config{}, "gemini:x", false, nil, false, io.Discard); err == nil {
		t.Error("unknown provider accepted")
	}
}

func TestSetupEmbeddingAnthropicUsesPulledOllama(t *testing.T) {
	// Without a terminal, an anthropic setup embeds locally only with a model
	// already pulled; it never sends text to a provider nobody chose.
	t.Setenv("VOYAGE_API_KEY", "vo-test")
	_, host := fakeOllama(t, map[string]any{"name": "qwen3:8b", "capabilities": []string{"completion"}})
	t.Setenv("OLLAMA_HOST", host)
	cfg := recallsetup.Config{ExtractModel: "anthropic:claude-opus-5-5"}
	if got, _, _ := setupEmbedding(cfg, "", true, nil, false, io.Discard); got != "" {
		t.Errorf("nothing pulled: %q, want word search", got)
	}
	_, host = fakeOllama(t, map[string]any{"name": "nomic-embed-text:latest", "capabilities": []string{"embedding"}})
	t.Setenv("OLLAMA_HOST", host)
	if got, _, _ := setupEmbedding(cfg, "", true, nil, false, io.Discard); got != "ollama:nomic-embed-text" {
		t.Errorf("pulled: %q", got)
	}
	// At a terminal it asks, offering Voyage only because its key is set,
	// and defaults to the local model.
	var out strings.Builder
	got, _, err := setupEmbedding(cfg, "", true, strings.NewReader("\n"), true, &out)
	if err != nil || got != "ollama:nomic-embed-text" || !strings.Contains(out.String(), "sent to Voyage AI") {
		t.Errorf("asked: %q, %v\n%s", got, err, out.String())
	}
	if got, _, _ := setupEmbedding(cfg, "", true, strings.NewReader("3\n"), true, io.Discard); got != "voyage:voyage-3.5" {
		t.Errorf("chose Voyage: %q", got)
	}
}

func TestAskChoice(t *testing.T) {
	choices := []modelChoice{{"", "words"}, {"ollama:nomic-embed-text", "local"}, {"voyage:voyage-3.5", "hosted"}}
	for in, want := range map[string]string{"\n": "ollama:nomic-embed-text", "3\n": "voyage:voyage-3.5", "9\n1\n": "", "": "ollama:nomic-embed-text"} {
		var out strings.Builder
		if got, err := askChoice(strings.NewReader(in), &out, "Search?", choices, 2); err != nil || got != want {
			t.Errorf("answer %q: %q, %v", in, got, err)
		}
		if !strings.Contains(out.String(), "1. lexical (words)") {
			t.Errorf("choices not listed: %s", out.String())
		}
	}
}

func TestPrepareEmbedding(t *testing.T) {
	pulls, host := fakeOllama(t, map[string]any{"name": "qwen3:8b", "capabilities": []string{"completion"}})
	t.Setenv("OLLAMA_HOST", host)
	var out strings.Builder
	if got, err := prepareEmbedding("ollama:nomic-embed-text", false, &out); err != nil || got != "ollama:nomic-embed-text" || pulls.Load() != 1 {
		t.Errorf("missing model: %q, %v, %d pulls", got, err, pulls.Load())
	}
	if !strings.Contains(out.String(), "Pulling nomic-embed-text") {
		t.Errorf("pull not announced: %s", out.String())
	}

	// Ollama down: a followed embedder gives way to word search; a named one fails.
	t.Setenv("OLLAMA_HOST", "127.0.0.1:1")
	out.Reset()
	if got, err := prepareEmbedding("ollama:nomic-embed-text", false, &out); err != nil || got != "" || !strings.Contains(out.String(), "Using word search") {
		t.Errorf("followed, Ollama down: %q, %v, %s", got, err, out.String())
	}
	if _, err := prepareEmbedding("ollama:nomic-embed-text", true, io.Discard); err == nil {
		t.Error("named, Ollama down: accepted")
	}

	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	if got, err := prepareEmbedding("openai:text-embedding-3-small@512", false, io.Discard); err != nil || got != "" {
		t.Errorf("followed openai without a key: %q, %v", got, err)
	}
}

func TestDescribeSearch(t *testing.T) {
	for spec, want := range map[string]string{
		"":                                  "Search: words only",
		"openai:text-embedding-3-small@512": "sent to OpenAI",
		"voyage:voyage-3.5":                 "sent to Voyage AI",
		"ollama:nomic-embed-text":           "on this machine",
	} {
		if got := describeSearch(recallsetup.Config{EmbedModel: spec}); !strings.Contains(got, want) {
			t.Errorf("%q: %s", spec, got)
		}
	}
}
