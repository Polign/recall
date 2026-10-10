package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Polign/recall/internal/recallsetup"
)

func TestRecallConnectionCheckRejectsCredentialsWithoutWriting(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			reads := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("setup attempted a write: %s %s", r.Method, r.URL.Path)
				}
				if r.URL.Path == "/readyz" && legacy {
					http.NotFound(w, r)
					return
				}
				if r.URL.Path == "/readyz" || r.URL.Path == "/healthz" {
					fmt.Fprint(w, "ok")
					return
				}
				reads++
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"error":"bad credential secret-test-key"}`)
			}))
			defer srv.Close()
			err := checkRecall(&api{base: srv.URL, key: "secret-test-key"}, recallsetup.Config{Collection: "test"})
			if err == nil || !strings.Contains(err.Error(), "memory read failed") || strings.Contains(err.Error(), "secret-test-key") || reads == 0 {
				t.Fatalf("check = %v, reads = %d", err, reads)
			}
		})
	}
}

func TestRecallURLRejectsEmbeddedSecrets(t *testing.T) {
	for _, raw := range []string{"http://user:password@localhost:23000", "https://host?key=secret", "file:///tmp/data", "http://", "https://host/#secret"} {
		if err := validateRecallURL(raw); err == nil || strings.Contains(err.Error(), "password") {
			t.Fatalf("unexpected validation for %q: %v", raw, err)
		}
	}
}

func TestClaudeSetupVersion(t *testing.T) {
	for raw, want := range map[string]bool{"2.1.233": false, "2.1.251": true, "2.1.270": true, "2.2.0": true, "3.0.0": true, "unknown": false} {
		if got := versionAtLeast(raw, 2, 1, 251); got != want {
			t.Errorf("%s = %v, want %v", raw, got, want)
		}
	}
}

func TestLocalKeyShape(t *testing.T) {
	key, err := generateLocalKey()
	if err != nil || !validLocalKey(key) || !strings.HasPrefix(key, "plgn_") || len(key) != len("plgn_")+16+1+64 {
		t.Fatalf("generated key %q (err %v) does not have the server's shape", key, err)
	}
	other, _ := generateLocalKey()
	if other == key {
		t.Fatal("two generated keys are identical")
	}
	for _, bad := range []string{"", "plgn_", key[:len(key)-1], "PLGN" + key[4:], strings.ToUpper(key), key + "x", `{"key":"` + key + `"}`} {
		if validLocalKey(bad) {
			t.Errorf("%q accepted as a local key", bad)
		}
	}
}

func TestDescribeSurface(t *testing.T) {
	for cfg, want := range map[recallsetup.Config]string{
		{ExtractModel: "ollama:qwen3:8b"}:   "text-only tools",
		{Predicates: "/p.json", Open: true}: "predicates from /p.json",
		{Open: true}:                        "defined on first use",
		{}:                                  "starter predicates",
	} {
		if got := describeSurface(cfg); !strings.Contains(got, want) {
			t.Errorf("%+v: %q, want it to mention %q", cfg, got, want)
		}
	}
}

func TestModelHintListsLocalOllamaModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"models":[{"name":"qwen3:8b","capabilities":["completion","tools"]},{"name":"nomic-embed-text","capabilities":["embedding"]}]}`)
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", strings.TrimPrefix(srv.URL, "http://"))
	hint := modelHint()
	if !strings.Contains(hint, "Local Ollama models found: qwen3:8b ") || strings.Contains(hint, "nomic-embed-text") {
		t.Fatalf("hint: %s", hint)
	}
	t.Setenv("OLLAMA_HOST", "127.0.0.1:1")
	if strings.Contains(modelHint(), "Local Ollama") {
		t.Fatal("hint names models with no Ollama answering")
	}
}
