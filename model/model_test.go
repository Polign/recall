package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Polign/recall"
)

func TestParse(t *testing.T) {
	for spec, want := range map[string]Config{
		"anthropic":                  {Provider: "anthropic", Model: DefaultAnthropicModel},
		"anthropic:claude-haiku-4-5": {Provider: "anthropic", Model: "claude-haiku-4-5"},
		"openai:gpt-5-mini":          {Provider: "openai", Model: "gpt-5-mini"},
		" Ollama:llama3.1 ":          {Provider: "ollama", Model: "llama3.1"},
	} {
		got, err := Parse(spec)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %+v, %v", spec, got, err)
		}
	}
	for _, spec := range []string{"", "openai", "ollama:", "gemini:pro"} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) accepted", spec)
		}
	}
}

func TestOllamaBase(t *testing.T) {
	for host, want := range map[string]string{
		"":                        "http://localhost:11434/v1",
		"0.0.0.0:11434":           "http://0.0.0.0:11434/v1",
		"https://ollama.example/": "https://ollama.example/v1",
	} {
		if got := ollamaBase(host); got != want {
			t.Errorf("ollamaBase(%q) = %q", host, got)
		}
	}
}

const said = "I moved to Lisbon last week. My editor is neovim."

// reply is what a model might return: one good statement, one that
// misquotes the text, and one with a relative date resolved.
const reply = `{"statements":[
 {"subject":"user","predicate":"prefers_editor","value":"neovim","evidence":"My editor is neovim."},
 {"subject":"user","predicate":"prefers_editor","value":"vim","evidence":"I love vim"},
 {"subject":"user","predicate":"location","value":"Lisbon","evidence":"I moved to Lisbon last week."}]}`

func registry() recall.Registry {
	return recall.Registry{
		"prefers_editor":     {Cardinality: "single", ValueType: "string", Description: "Preferred text editor"},
		"location":           {Cardinality: "single", ValueType: "string", Description: "Where the subject lives"},
		recall.NotePredicate: {Cardinality: "multi", ValueType: "string", Description: "Anything else"},
	}
}

func checkProposals(t *testing.T, got []recall.Proposal) {
	t.Helper()
	if len(got) != 2 || got[0].Value != "neovim" || got[1].Predicate != "location" {
		t.Fatalf("proposals: %+v", got)
	}
}

// checkPrompt confirms the request carries the date, the registered
// predicates without note, and the schema's predicate enum.
func checkPrompt(t *testing.T, system string, schema map[string]any) {
	t.Helper()
	if !strings.Contains(system, "Today's date: 2023-05-20") || !strings.Contains(system, "- location (single-valued string)") || strings.Contains(system, "- note ") {
		t.Fatalf("system prompt:\n%s", system)
	}
	items := schema["properties"].(map[string]any)["statements"].(map[string]any)["items"].(map[string]any)
	enum := items["properties"].(map[string]any)["predicate"].(map[string]any)["enum"].([]any)
	if len(enum) != 2 || enum[0] != "location" || enum[1] != "prefers_editor" {
		t.Fatalf("predicate enum: %v", enum)
	}
}

func dated() context.Context {
	return recallCtx(time.Date(2023, 5, 20, 2, 21, 0, 0, time.UTC))
}

// recallCtx sets the observation time the way RememberTextAt does, by
// running a throwaway extraction through a client.
func recallCtx(at time.Time) context.Context {
	var got context.Context
	c, err := recall.NewClient(recall.Config{Backend: nopBackend{}, Collection: "m", Registry: registry(), Embedder: recall.LexicalEmbedder{}})
	if err != nil {
		panic(err)
	}
	_, _ = c.RememberTextAt(context.Background(), "x", ctxGrabber(func(ctx context.Context) { got = ctx }), at)
	return got
}

type ctxGrabber func(context.Context)

func (g ctxGrabber) Extract(ctx context.Context, _ string, _ recall.Registry) ([]recall.Proposal, error) {
	g(ctx)
	return nil, context.Canceled
}

type nopBackend struct{ recall.Backend }

func TestChatCompletions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("request: %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role, Content string
			} `json:"messages"`
			ResponseFormat struct {
				Type       string `json:"type"`
				JSONSchema struct {
					Strict bool           `json:"strict"`
					Schema map[string]any `json:"schema"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "m1" || body.Messages[1].Content != said || body.ResponseFormat.Type != "json_schema" || !body.ResponseFormat.JSONSchema.Strict {
			t.Errorf("body: %+v", body)
		}
		checkPrompt(t, body.Messages[0].Content, body.ResponseFormat.JSONSchema.Schema)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "```json\n" + reply + "\n```"}, "finish_reason": "stop"}}})
	}))
	defer srv.Close()
	e, err := NewExtractor(Config{Provider: "openai", Model: "m1", BaseURL: srv.URL + "/v1", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.Extract(dated(), said, registry())
	if err != nil {
		t.Fatal(err)
	}
	checkProposals(t, got)
}

func TestChatCompletionsErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"api error": {401, `{"error":{"message":"bad key"}}`, "bad key"},
		"refusal":   {200, `{"choices":[{"message":{"refusal":"no"},"finish_reason":"stop"}]}`, "declined"},
		"cut off":   {200, `{"choices":[{"message":{"content":"{"},"finish_reason":"length"}]}`, "cut off"},
		"not json":  {200, `{"choices":[{"message":{"content":"sorry"},"finish_reason":"stop"}]}`, "not the statements JSON"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		e, _ := NewExtractor(Config{Provider: "ollama", Model: "m", BaseURL: srv.URL})
		_, err := e.Extract(context.Background(), said, registry())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestOpenAINeedsKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	if _, err := NewExtractor(Config{Provider: "openai", Model: "m"}); err == nil {
		t.Fatal("openai without a key or endpoint accepted")
	}
}

func anthropicServer(t *testing.T, stop string, check func(map[string]any)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "k" {
			t.Errorf("request: %s %q", r.URL.Path, r.Header.Get("X-Api-Key"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		check(body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": body["model"],
			"content":     []any{map[string]any{"type": "text", "text": reply}},
			"stop_reason": stop, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	}))
}

func TestAnthropic(t *testing.T) {
	srv := anthropicServer(t, "end_turn", func(body map[string]any) {
		if body["model"] != "claude-opus-5-5" || body["fallbacks"] != nil {
			t.Errorf("body: model %v fallbacks %v", body["model"], body["fallbacks"])
		}
		system := body["system"].([]any)[0].(map[string]any)["text"].(string)
		format := body["output_config"].(map[string]any)["format"].(map[string]any)
		if format["type"] != "json_schema" {
			t.Errorf("format: %v", format)
		}
		checkPrompt(t, system, format["schema"].(map[string]any))
	})
	defer srv.Close()
	e, err := NewExtractor(Config{Provider: "anthropic", Model: "claude-opus-5-5", BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.Extract(dated(), said, registry())
	if err != nil {
		t.Fatal(err)
	}
	checkProposals(t, got)
}

func TestAnthropicRefusal(t *testing.T) {
	srv := anthropicServer(t, "refusal", func(map[string]any) {})
	defer srv.Close()
	e, _ := NewExtractor(Config{Provider: "anthropic", Model: "claude-opus-5-5", BaseURL: srv.URL, APIKey: "k"})
	if _, err := e.Extract(context.Background(), said, registry()); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("refusal: %v", err)
	}
}

func TestAnthropicFallbacksOnlyDirect(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "")
	if m := newAnthropic(Config{Model: "claude-opus-5-5", HTTPClient: http.DefaultClient}); !m.fallbacks {
		t.Error("direct Claude API call without fallbacks")
	}
	if m := newAnthropic(Config{Model: "claude-haiku-4-5", HTTPClient: http.DefaultClient}); m.fallbacks {
		t.Error("fallbacks on a model that does not take them")
	}
	if m := newAnthropic(Config{Model: "claude-opus-5-5", BaseURL: "http://proxy", HTTPClient: http.DefaultClient}); m.fallbacks {
		t.Error("fallbacks through a proxy")
	}
}

func TestOnlyNotesMakesNoRequest(t *testing.T) {
	e := &Extractor{c: failing{}}
	got, err := e.Extract(context.Background(), said, recall.Registry{recall.NotePredicate: {Cardinality: "multi"}})
	if err != nil || got != nil {
		t.Fatalf("%v %v", got, err)
	}
}

type failing struct{}

func (failing) complete(context.Context, string, string, map[string]any) (string, error) {
	panic("no request expected")
}
