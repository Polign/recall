package telemetry

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// clearEnv removes every variable that turns telemetry off, so a test run in
// CI still exercises the enabled path.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"RECALL_TELEMETRY", "DO_NOT_TRACK", "POLIGN_CONFIDENTIAL", "CI"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

func TestDisabled(t *testing.T) {
	clearEnv(t)
	home := t.TempDir()
	if got := Disabled("v1.2.3", home); got != "" {
		t.Fatalf("release build with a home: disabled by %q", got)
	}
	for _, version := range []string{"", "dev"} {
		if Disabled(version, home) == "" {
			t.Errorf("version %q must not report", version)
		}
	}
	if Disabled("v1.2.3", "") == "" {
		t.Error("no home must not report")
	}
	for name, values := range map[string][]string{
		"RECALL_TELEMETRY":    {"0", "false", "off", "no"},
		"DO_NOT_TRACK":        {"1", "true", "yes"},
		"POLIGN_CONFIDENTIAL": {"1", "true"},
		"CI":                  {"true", "1"},
	} {
		for _, v := range values {
			t.Setenv(name, v)
			if Disabled("v1.2.3", home) == "" {
				t.Errorf("%s=%s must turn telemetry off", name, v)
			}
		}
		os.Unsetenv(name)
	}
	for name, v := range map[string]string{"RECALL_TELEMETRY": "1", "DO_NOT_TRACK": "0", "CI": "false"} {
		t.Setenv(name, v)
		if got := Disabled("v1.2.3", home); got != "" {
			t.Errorf("%s=%s turned telemetry off: %s", name, v, got)
		}
		os.Unsetenv(name)
	}

	if err := SetOff(home, true); err != nil {
		t.Fatal(err)
	}
	if Disabled("v1.2.3", home) == "" {
		t.Error("recall telemetry off must turn telemetry off")
	}
	if err := SetOff(home, false); err != nil {
		t.Fatal(err)
	}
	if got := Disabled("v1.2.3", home); got != "" {
		t.Errorf("recall telemetry on left it off: %s", got)
	}
}

func TestOffCreatesNothingAndSendsNothing(t *testing.T) {
	clearEnv(t)
	t.Setenv("RECALL_TELEMETRY", "0")
	home := t.TempDir()
	if r := Start(Config{Version: "v1.2.3", Home: home, Mode: "mcp"}, func(string) { t.Error("notice while off") }); r != nil {
		t.Fatal("a reporter was started while off")
	}
	var r *Reporter
	r.SetClient("x")
	r.Tool("remember", true)
	r.Close()
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("files written while off: %v", entries)
	}
}

func TestPayloadCarriesOnlyLabelsAndCounts(t *testing.T) {
	clearEnv(t)
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	home := t.TempDir()
	notices := 0
	cfg := Config{Version: "v1.2.3", Home: home, Mode: "mcp", Backend: "local", Embedder: "ollama:my-private-model@512", Extractor: "anthropic:claude-secret", Text: true}
	r := newReporter(cfg, func(string) { notices++ })
	if r == nil {
		t.Fatal("no reporter")
	}
	r.endpoint = srv.URL
	r.SetClient("Claude Code/2.0 (private build)")
	r.Tool("remember", true)
	r.Tool("remember", false)
	r.Tool("recall", false)
	r.Tool("forget", false)
	r.Tool("history", false)
	r.Tool("diff", false)
	r.Tool("list_predicates", false)
	r.Tool("agent_resume", false)
	r.Close()
	r.Close() // idempotent

	if len(bodies) != 1 {
		t.Fatalf("sent %d heartbeats, want the final one", len(bodies))
	}
	var got map[string]any
	if err := json.Unmarshal(bodies[0], &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"version": "v1.2.3", "mode": "mcp", "backend": "local", "embedder": "ollama", "extractor": "anthropic",
		"surface": "text", "client": "claude-code-2.0-private-build",
		"remembers": 2.0, "replacements": 1.0, "recalls": 1.0, "forgets": 1.0, "inspections": 2.0, "state_calls": 1.0,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if len(got) != 18 {
		t.Errorf("payload has %d fields, want 18; a new field needs TELEMETRY.md and the collector: %v", len(got), got)
	}
	if s := string(bodies[0]); strings.Contains(s, "private-model") || strings.Contains(s, "secret") || strings.Contains(s, home) {
		t.Errorf("payload leaks a model name or a path: %s", s)
	}

	// The ID is created once, announced once, and kept.
	id, _ := os.ReadFile(filepath.Join(home, idFile))
	if !validID(strings.TrimSpace(string(id))) || got["install_id"] != strings.TrimSpace(string(id)) {
		t.Errorf("install id %q does not match the file %q", got["install_id"], id)
	}
	again := newReporter(cfg, func(string) { notices++ })
	if again.base.InstallID != r.base.InstallID {
		t.Error("the install id changed between runs")
	}
	if again.base.ProcessID == r.base.ProcessID {
		t.Error("two runs share a process id")
	}
	if notices != 1 {
		t.Errorf("the notice was shown %d times, want once", notices)
	}
	if p := Preview(cfg); p.InstallID != r.base.InstallID || p.ProcessID != "" || p.Remembers != 0 {
		t.Errorf("preview = %+v", p)
	}
}

func TestServeNamesNoClient(t *testing.T) {
	clearEnv(t)
	r := newReporter(Config{Version: "v1.2.3", Home: t.TempDir(), Mode: "serve"}, nil)
	r.SetClient("cursor")
	if got := r.Snapshot(); got.Client != "" || got.Embedder != "lexical" || got.Extractor != "none" || got.Surface != "typed" {
		t.Errorf("snapshot = %+v", got)
	}
}

func TestLabel(t *testing.T) {
	for in, want := range map[string]string{
		"Claude Code": "claude-code", "cursor-vscode": "cursor-vscode", "  ": "", "../../etc": "etc",
		"名前": "", strings.Repeat("a", 80): strings.Repeat("a", 48),
	} {
		if got := label(in); got != want {
			t.Errorf("label(%q) = %q, want %q", in, got, want)
		}
	}
}
