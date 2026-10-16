package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseEmbed(t *testing.T) {
	for spec, want := range map[string]string{
		"openai":                               "openai:text-embedding-3-small@512",
		"openai:@256":                          "openai:text-embedding-3-small@256",
		"openai:text-embedding-3-small":        "openai:text-embedding-3-small",
		" OpenAI:text-embedding-3-large@1024 ": "openai:text-embedding-3-large@1024",
		"ollama":                               "ollama:nomic-embed-text",
		"ollama:mxbai-embed-large":             "ollama:mxbai-embed-large",
		"voyage":                               "voyage:voyage-3.5",
		"voyage:voyage-3.5-lite@512":           "voyage:voyage-3.5-lite@512",
	} {
		got, err := ParseEmbed(spec)
		if err != nil || got.Spec() != want {
			t.Errorf("ParseEmbed(%q) = %q, %v; want %q", spec, got.Spec(), err, want)
		}
	}
	for _, spec := range []string{"", "anthropic", "gemini:x", "openai:x@0", "openai:x@big"} {
		if _, err := ParseEmbed(spec); err == nil {
			t.Errorf("ParseEmbed(%q) accepted", spec)
		}
	}
}

func TestKnownDim(t *testing.T) {
	for spec, want := range map[string]int{
		"openai":                        512,
		"openai:text-embedding-3-small": 1536,
		"ollama":                        768,
		"voyage":                        1024,
		"ollama:mystery":                0,
	} {
		cfg, _ := ParseEmbed(spec)
		if got := cfg.KnownDim(); got != want {
			t.Errorf("%s: KnownDim = %d, want %d", spec, got, want)
		}
	}
}

func TestEmbedFor(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "")
	for extract, want := range map[string]string{
		"openai:gpt-5-mini":    "openai:text-embedding-3-small@512",
		"ollama:qwen3:8b":      "ollama:nomic-embed-text",
		"anthropic:claude-x":   "",
		"":                     "",
		"not-a-provider:model": "",
	} {
		if got := EmbedFor(extract); got != want {
			t.Errorf("EmbedFor(%q) = %q, want %q", extract, got, want)
		}
	}
	// An openai spec pointed at another endpoint (vLLM, Groq) may have no
	// embeddings, so it does not pick one.
	t.Setenv("OPENAI_BASE_URL", "http://localhost:8000/v1")
	if got := EmbedFor("openai:llama"); got != "" {
		t.Errorf("EmbedFor with a non-OpenAI base = %q, want none", got)
	}
}

func TestEmbedRequests(t *testing.T) {
	for _, tc := range []struct {
		spec, dimField string
		dim            int
		auth           string
	}{
		{"openai", "dimensions", 512, "Bearer sk-test"},
		{"voyage:voyage-3.5@256", "output_dimension", 256, "Bearer vo-test"},
		{"ollama:nomic-embed-text", "", 768, ""},
	} {
		var got map[string]any
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/embeddings" {
				t.Errorf("%s: path %s", tc.spec, r.URL.Path)
			}
			gotAuth = r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&got)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": make([]float32, tc.dim)}}})
		}))
		cfg, err := ParseEmbed(tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		cfg.BaseURL = srv.URL + "/v1"
		cfg.APIKey = strings.TrimPrefix(tc.auth, "Bearer ")
		e, err := NewEmbedder(cfg)
		if err != nil {
			t.Fatal(err)
		}
		v, err := e.Embed(context.Background(), strings.Repeat("x", maxEmbedChars+10))
		srv.Close()
		if err != nil || len(v) != tc.dim {
			t.Fatalf("%s: Embed = %d values, %v", tc.spec, len(v), err)
		}
		if got["model"] != cfg.Model {
			t.Errorf("%s: model %v", tc.spec, got["model"])
		}
		if in, _ := got["input"].([]any); len(in) != 1 || len(in[0].(string)) != maxEmbedChars {
			t.Errorf("%s: input not one text cut to %d chars", tc.spec, maxEmbedChars)
		}
		if tc.dimField != "" && got[tc.dimField] != float64(tc.dim) {
			t.Errorf("%s: %s = %v", tc.spec, tc.dimField, got[tc.dimField])
		}
		for _, f := range []string{"dimensions", "output_dimension"} {
			if f != tc.dimField && got[f] != nil {
				t.Errorf("%s: sent %s", tc.spec, f)
			}
		}
		if gotAuth != tc.auth {
			t.Errorf("%s: Authorization %q", tc.spec, gotAuth)
		}
	}
}

func TestEmbedErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req["model"] {
		case "missing":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": `model "missing" not found`}})
		case "short":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"embedding": []float32{1, 2}}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		}
	}))
	defer srv.Close()
	for _, tc := range []struct {
		model string
		dim   int
		want  string
	}{
		{"missing", 0, "ollama pull missing"},
		{"short", 768, "returned 2 dimensions, expected 768"},
		{"empty", 0, "no vector"},
	} {
		e, err := NewEmbedder(EmbedConfig{Provider: "ollama", Model: tc.model, Dim: tc.dim, BaseURL: srv.URL + "/v1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Embed(context.Background(), "hello"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.model, err, tc.want)
		}
	}
}

func TestEmbedNeedsKeys(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("VOYAGE_API_KEY", "")
	for _, spec := range []string{"openai", "voyage"} {
		cfg, _ := ParseEmbed(spec)
		if _, err := NewEmbedder(cfg); err == nil || !strings.Contains(err.Error(), "_API_KEY") {
			t.Errorf("%s without a key: %v", spec, err)
		}
	}
}
