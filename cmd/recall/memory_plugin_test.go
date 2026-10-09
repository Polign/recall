package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMemoryPluginDefaultsExtractionAndMaterialization(t *testing.T) {
	target, err := url.Parse(polignServer(t))
	if err != nil {
		t.Fatal(err)
	}
	handler := httputil.NewSingleHostReverseProxy(target)
	var lists atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/vectors") {
			lists.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	defer ts.Close()
	connect := func() *mcpServer {
		t.Helper()
		api := &api{base: ts.URL}
		m, err := newMemoryRuntime(api, "recall_lexical_v1", "", "")
		if err != nil {
			t.Fatal(err)
		}
		return &mcpServer{api: api, memory: m, write: true, collection: "recall_lexical_v1"}
	}
	s := connect()
	if len(s.memory.registry()) != 16 {
		t.Fatal("default registry missing")
	}
	text, bad := step(t, s, "remember", `{"text":"I prefer vim.","statements":[{"subject":"user","predicate":"prefers_editor","value":"vim","evidence":"I prefer vim."}]}`)
	if bad || !strings.Contains(text, `"agent_inferred"`) {
		t.Fatalf("extract: %s", text)
	}
	selector := `{"subject":"user","predicate":"prefers_editor"}`
	if _, bad := step(t, s, "recall", selector); bad {
		t.Fatal("initial recall failed")
	}
	before := lists.Load()
	if _, bad := step(t, s, "recall", selector); bad || lists.Load() != before {
		t.Fatal("unchanged recall replayed history")
	}
	other := connect()
	if text, bad := step(t, other, "remember", `{"subject":"user","predicate":"prefers_editor","value":"neovim","confidence":0}`); bad || !strings.Contains(text, `"confidence":0`) {
		t.Fatalf("zero confidence: %s", text)
	}
	text, bad = step(t, s, "recall", selector)
	if bad || !strings.Contains(text, `"neovim"`) || lists.Load() == before {
		t.Fatalf("external writer invalidation: %s", text)
	}
	if text, bad := step(t, s, "recall", `{"query":"neovim editor"}`); bad || !strings.Contains(text, `"neovim"`) {
		t.Fatalf("lexical retrieval: %s", text)
	}
	if _, bad := step(t, s, "polign_search", `{"text":"neovim"}`); !bad {
		t.Fatal("raw tool reachable in memory-only mode")
	}
	for _, args := range []string{`{"text":"x"}`, `{"text":"x","statements":[],"subject":"user"}`, `{"statements":[]}`} {
		if _, bad := step(t, s, "remember", args); !bad {
			t.Fatalf("accepted invalid extraction: %s", args)
		}
	}
	if _, bad := step(t, s, "remember", `{"text":"nothing to remember","statements":[]}`); bad {
		t.Fatal("empty extraction rejected")
	}
	// A statement no predicate fits is kept as a note, not refused.
	if text, bad := step(t, s, "remember", `{"text":"x","statements":[{"subject":"user","predicate":"invented","value":"x","evidence":"x"}]}`); bad || !strings.Contains(text, `"unfiled"`) {
		t.Fatalf("unregistered proposal: %s", text)
	}
	if text, bad := step(t, s, "recall", `{"subject":"user","predicate":"note"}`); bad || !strings.Contains(text, `"nothing to remember"`) || !strings.Contains(text, `"x"`) {
		t.Fatalf("notes: %s", text)
	}
	s.write = false
	if _, bad := step(t, s, "remember", `{"text":"x","statements":[]}`); !bad {
		t.Fatal("read-only extraction accepted")
	}
}

func TestMemoryPluginTypedForget(t *testing.T) {
	base := polignServer(t)
	p := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(p, []byte(`{"enabled":{"cardinality":"single","value_type":"boolean"},"port":{"cardinality":"single","value_type":"number"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	api := &api{base: base}
	mem, err := newMemoryRuntime(api, "test", p, "")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{api: api, memory: mem, write: true}
	for _, args := range []string{`{"subject":"project","predicate":"enabled","value":false}`, `{"subject":"project","predicate":"port","value":0}`} {
		if text, bad := step(t, s, "remember", args); bad {
			t.Fatal(text)
		}
		if text, bad := step(t, s, "forget", args); bad || !strings.Contains(text, `"withdrawn":1`) {
			t.Fatalf("typed forget: %s", text)
		}
	}
	text, bad := step(t, s, "list_predicates", `{}`)
	var entries []any
	if bad || json.Unmarshal([]byte(text), &entries) != nil || len(entries) != 3 { // the two registered, plus note
		t.Fatalf("custom registry: %s", text)
	}
}
