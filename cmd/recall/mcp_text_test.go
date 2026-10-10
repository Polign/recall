package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Polign/recall"
)

// fakeBackend serves a fakeMemoryDB through the context-aware Backend, which
// the text tools need.
type fakeBackend struct{ db *fakeMemoryDB }

func (b fakeBackend) Put(_ context.Context, c, id string, v []float32, md map[string]any) error {
	return b.db.Put(c, id, v, md)
}

func (b fakeBackend) List(_ context.Context, c string, f map[string]any, n int) ([]recall.StoredVector, int, error) {
	return b.db.List(c, f, n)
}

func (b fakeBackend) Search(_ context.Context, c string, v []float32, k int, f map[string]any) ([]recall.Hit, error) {
	return b.db.Search(c, v, k, f)
}

// wordModel stands in for an extraction model: it files "I use X." as the
// user's favorite editor, and picks every candidate naming a word the request
// shares with it.
type wordModel struct{}

func (wordModel) Extract(_ context.Context, text string, _ recall.Registry) ([]recall.Proposal, error) {
	for _, prefix := range []string{"I use ", "I switched to "} {
		if rest, ok := strings.CutPrefix(text, prefix); ok {
			editor := strings.TrimSuffix(rest, ".")
			return []recall.Proposal{{Subject: "user", Predicate: "favorite_editor", Value: editor, Evidence: editor}}, nil
		}
	}
	return nil, nil
}

func (wordModel) Select(_ context.Context, request string, candidates []recall.Belief) ([]int, error) {
	var out []int
	for i, b := range candidates {
		if strings.Contains(request, "editor") && b.Predicate == "favorite_editor" {
			out = append(out, i)
		}
	}
	return out, nil
}

func newTextMCP(t *testing.T, extractor recall.Extractor) *mcpServer {
	t.Helper()
	client, err := recall.NewClient(recall.Config{Backend: fakeBackend{newFakeMemoryDB()}, Collection: "memories", Embedder: recall.LexicalEmbedder{}, Open: true})
	if err != nil {
		t.Fatal(err)
	}
	mem := &memoryRuntime{client: client, ctx: context.Background(), extractor: extractor, open: true, text: true}
	return &mcpServer{api: &api{base: "http://unused"}, write: true, collection: "memories", memory: mem}
}

func TestTextSurfaceNamesNoPredicates(t *testing.T) {
	s := newTextMCP(t, wordModel{})
	resps := runMCP(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if names := toolNames(t, resps[2]); strings.Join(names, ",") != "remember,recall,forget" {
		t.Fatalf("tools %v", names)
	}
	for id, resp := range resps {
		raw, _ := json.Marshal(resp.Result)
		if strings.Contains(strings.ToLower(string(raw)), "predicate") {
			t.Fatalf("response %v mentions predicates: %s", id, raw)
		}
	}
	s.write = false
	if names := toolNames(t, runMCP(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)[3]); strings.Join(names, ",") != "recall" {
		t.Fatalf("read-only tools %v", names)
	}
}

func TestTextRememberRecallForget(t *testing.T) {
	s := newTextMCP(t, wordModel{})
	if text, bad := step(t, s, "remember", `{"text":"I use helix."}`); bad || !strings.Contains(text, "user favorite editor: helix") {
		t.Fatalf("remember: %s", text)
	}
	text, bad := step(t, s, "remember", `{"text":"I switched to zed."}`)
	if bad || !strings.Contains(text, `"user favorite editor: zed"`) || !strings.Contains(text, `"replaces":["user favorite editor: helix"]`) {
		t.Fatalf("correction: %s", text)
	}
	text, bad = step(t, s, "recall", `{"question":"which editor do I use"}`)
	if bad || !strings.Contains(text, `"text":"user favorite editor: zed"`) || !strings.Contains(text, `"before":[{"text":"helix"`) || !strings.Contains(text, `"days_ago":0`) {
		t.Fatalf("recall: %s", text)
	}
	if text, bad := step(t, s, "forget", `{"text":"my editor"}`); bad || !strings.Contains(text, `"forgotten":["user favorite editor: zed"]`) {
		t.Fatalf("forget: %s", text)
	}
	if text, _ := step(t, s, "recall", `{"question":"which editor do I use"}`); strings.Contains(text, "editor: zed") {
		t.Fatalf("zed still recalled: %s", text)
	}
	if text, bad := step(t, s, "remember", `{"subject":"user","predicate":"x","value":"y"}`); !bad {
		t.Fatalf("typed arguments accepted by the text surface: %s", text)
	}
	if _, bad := step(t, s, "list_predicates", `{}`); !bad {
		t.Fatal("list_predicates is reachable on the text surface")
	}
}

func TestTextSurfaceWithoutAModelKeepsText(t *testing.T) {
	s := newTextMCP(t, nil)
	if text, bad := step(t, s, "remember", `{"text":"I use helix."}`); bad || !strings.Contains(text, `"kept_as_text":true`) {
		t.Fatalf("remember: %s", text)
	}
	if text, bad := step(t, s, "recall", `{"question":"helix"}`); bad || !strings.Contains(text, `"text":"I use helix."`) {
		t.Fatalf("recall: %s", text)
	}
	if text, bad := step(t, s, "forget", `{"text":"helix"}`); !bad || !strings.Contains(text, "-extract-model") {
		t.Fatalf("forget without a model: %s", text)
	}
}

func TestOpenTypedSurfaceDefinesPredicates(t *testing.T) {
	s := newTextMCP(t, nil)
	s.memory.text = false
	if text, bad := step(t, s, "remember", `{"subject":"user","predicate":"allergic_to","value":"peanuts","cardinality":"multi","description":"Something the user is allergic to"}`); bad {
		t.Fatalf("open remember: %s", text)
	}
	if text, bad := step(t, s, "remember", `{"subject":"user","predicate":"allergic_to","value":"shellfish"}`); bad || strings.Contains(text, "superseded") {
		t.Fatalf("second allergy: %s", text)
	}
	if text, bad := step(t, s, "list_predicates", `{}`); bad || !strings.Contains(text, `"predicate":"allergic_to","cardinality":"multi"`) {
		t.Fatalf("list_predicates: %s", text)
	}
	if instr := s.memoryInstructions(); strings.Contains(instr, "closed set") || !strings.Contains(instr, "allergic_to") {
		t.Fatalf("instructions:\n%s", instr)
	}
}
