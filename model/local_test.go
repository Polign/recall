package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseLocal(t *testing.T) {
	for spec, want := range map[string]string{"local": "local:all-minilm-l6-v2", "local:All-MiniLM-L6-v2": "local:all-minilm-l6-v2"} {
		cfg, err := ParseEmbed(spec)
		if err != nil || cfg.Spec() != want || cfg.KnownDim() != 384 {
			t.Errorf("ParseEmbed(%q) = %q dim %d, %v", spec, cfg.Spec(), cfg.KnownDim(), err)
		}
	}
	if _, err := ParseEmbed("local:bert-huge"); err == nil {
		t.Error("an unknown local model was accepted")
	}
	if _, err := NewLocalEmbedder(EmbedConfig{Provider: "local", Model: DefaultLocalEmbedModel, Dim: 512}, nil); err == nil {
		t.Error("a local model was given a size it does not make")
	}
}

func TestCutText(t *testing.T) {
	if got := cutText("héllo", 2); got != "h" {
		t.Errorf("cut inside é = %q, want %q", got, "h")
	}
	if got := cutText("hello", 10); got != "hello" {
		t.Errorf("short text changed: %q", got)
	}
}

// A fake model served like Hugging Face: each file's bytes are its path.
func fakeHub(t *testing.T, corrupt string) (*atomic.Int32, func()) {
	t.Helper()
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gets.Add(1)
		body := r.URL.Path[strings.LastIndex(r.URL.Path, "/resolve/"):]
		if corrupt != "" && strings.HasSuffix(r.URL.Path, corrupt) {
			body += "!"
		}
		_, _ = w.Write([]byte(body))
	}))
	saved, savedHub := localModels["test-model"], huggingFace
	m := localModel{repo: "org/test", revision: "abc", dim: 4, onnx: "model.onnx"}
	for _, f := range []string{"onnx/model.onnx", "tokenizer.json"} {
		sum := sha256.Sum256([]byte("/resolve/abc/" + f))
		m.files = append(m.files, localFile{path: f, sha256: hex.EncodeToString(sum[:])})
	}
	localModels["test-model"], huggingFace = m, srv.URL
	return &gets, func() {
		srv.Close()
		huggingFace = savedHub
		if saved.repo == "" {
			delete(localModels, "test-model")
		} else {
			localModels["test-model"] = saved
		}
	}
}

func TestFetchLocalModel(t *testing.T) {
	gets, done := fakeHub(t, "")
	defer done()
	dir := t.TempDir()
	if LocalModelReady("test-model", dir) {
		t.Fatal("ready before any download")
	}
	if err := FetchLocalModel(context.Background(), "test-model", dir, nil); err != nil {
		t.Fatal(err)
	}
	if !LocalModelReady("test-model", dir) || gets.Load() != 2 {
		t.Fatalf("after download: ready=%v, %d requests", LocalModelReady("test-model", dir), gets.Load())
	}
	// Files already present are not fetched again.
	if err := FetchLocalModel(context.Background(), "test-model", dir, nil); err != nil || gets.Load() != 2 {
		t.Fatalf("second fetch: %v, %d requests", err, gets.Load())
	}
}

func TestFetchLocalModelRefusesABadChecksum(t *testing.T) {
	_, done := fakeHub(t, "tokenizer.json")
	defer done()
	dir := t.TempDir()
	err := FetchLocalModel(context.Background(), "test-model", dir, nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("corrupt download: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "test-model", "tokenizer.json")); err == nil {
		t.Fatal("a file that failed its checksum was kept")
	}
}

// With RECALL_TEST_LOCAL_MODEL_DIR naming a directory that holds (or may
// download into) all-minilm-l6-v2, the real model runs: vectors are 384
// numbers of length 1, and meaning outranks unrelated text.
func TestLocalEmbedderRuns(t *testing.T) {
	dir := os.Getenv("RECALL_TEST_LOCAL_MODEL_DIR")
	if dir == "" {
		t.Skip("set RECALL_TEST_LOCAL_MODEL_DIR to run the local model")
	}
	e, err := NewLocalEmbedder(EmbedConfig{Provider: "local", Model: DefaultLocalEmbedModel, BaseURL: dir}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ctx := context.Background()
	embed := func(s string) []float32 {
		v, err := e.Embed(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cos := func(a, b []float32) (d float64) {
		for i := range a {
			d += float64(a[i] * b[i])
		}
		return d
	}
	// Recall searches "the user" for the user's name (engine asUser).
	q := embed("What can the user eat?")
	if len(q) != 384 || math.Abs(cos(q, q)-1) > 1e-3 {
		t.Fatalf("vector: %d values, length² %.4f", len(q), cos(q, q))
	}
	diet, email := embed("user diet: pescatarian"), embed("user email: maya@orcalabs.com")
	if cos(q, diet) <= cos(q, email) {
		t.Errorf("diet %.3f should outrank email %.3f for a question about food", cos(q, diet), cos(q, email))
	}
	if _, err := e.Embed(ctx, strings.Repeat("long text ", 2000)); err != nil {
		t.Errorf("long text: %v", err)
	}
	// Pasted terminal output runs near one token per character, so the
	// first cut is still over the model's 512 tokens.
	dense := strings.Repeat("npm warn publish \"bin[recall]\" script name was cleaned; ~/.npm/_logs/2026-10-12T17_02_11Z-debug-0.log ", 40)
	if v, err := e.Embed(ctx, dense); err != nil || len(v) != 384 {
		t.Errorf("dense text: %d values, %v", len(v), err)
	}
}

// The model loads inside the first Embed, usually under a request's context.
// Ending that request must not end the model: later embeddings run under
// their own contexts.
func TestLocalEmbedderOutlivesTheFirstCallersContext(t *testing.T) {
	dir := os.Getenv("RECALL_TEST_LOCAL_MODEL_DIR")
	if dir == "" {
		t.Skip("set RECALL_TEST_LOCAL_MODEL_DIR to run the local model")
	}
	e, err := NewLocalEmbedder(EmbedConfig{Provider: "local", Model: DefaultLocalEmbedModel, BaseURL: dir}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	first, cancel := context.WithCancel(context.Background())
	if _, err := e.Embed(first, "the first request"); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := e.Embed(context.Background(), "a later request"); err != nil {
		t.Fatalf("after the first caller's context ended: %v", err)
	}
}
