package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Polign/recall"
)

// collections is a context-aware backend holding one fakeMemoryDB per
// collection, so tests can tell collections apart.
type collections struct {
	mu  sync.Mutex
	dbs map[string]*fakeMemoryDB
	err error
}

type notFound struct{}

func (notFound) Error() string  { return "collection not found" }
func (notFound) NotFound() bool { return true }

func newCollections() *collections { return &collections{dbs: map[string]*fakeMemoryDB{}} }

func (c *collections) db(name string, create bool) *fakeMemoryDB {
	c.mu.Lock()
	defer c.mu.Unlock()
	db, ok := c.dbs[name]
	if !ok && create {
		db = newFakeMemoryDB()
		c.dbs[name] = db
	}
	return db
}

func (c *collections) Put(_ context.Context, collection, id string, values []float32, md map[string]any) error {
	return c.db(collection, true).Put(collection, id, values, md)
}

func (c *collections) List(_ context.Context, collection string, filter map[string]any, limit int) ([]recall.StoredVector, int, error) {
	if c.err != nil {
		return nil, 0, c.err
	}
	db := c.db(collection, false)
	if db == nil {
		return []recall.StoredVector{}, 0, nil // as Qdrant and polign_db answer for a collection never written
	}
	return db.List(collection, filter, limit)
}

func (c *collections) Search(_ context.Context, collection string, values []float32, k int, filter map[string]any) ([]recall.Hit, error) {
	db := c.db(collection, false)
	if db == nil {
		return nil, notFound{}
	}
	return db.Search(collection, values, k, filter)
}

// fixed embeds every text as a vector of dim ones, counting the calls.
func fixed(spec string, dim int, derived bool, calls *int) embedding {
	return embedding{Embedder: recall.EmbedFunc(func(context.Context, string) ([]float32, error) {
		if calls != nil {
			*calls++
		}
		v := make([]float32, dim)
		for i := range v {
			v[i] = 1
		}
		return v, nil
	}), spec: spec, dim: dim, derived: derived}
}

// seed remembers one typed fact into collection with emb.
func seed(t *testing.T, b recall.Backend, collection string, emb embedding, value string) {
	t.Helper()
	c, err := recall.NewClient(recall.Config{Backend: b, Collection: collection, Embedder: emb, Open: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remember(context.Background(), recall.RememberRequest{Subject: "user", Predicate: "timezone", Value: value}); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultCollection(t *testing.T) {
	for spec, want := range map[string]string{
		"":                                  "recall_lexical_v1",
		"openai:text-embedding-3-small@512": "recall_openai_text_embedding_3_small_512_v1",
		"ollama:nomic-embed-text":           "recall_ollama_nomic_embed_text_v1",
		"voyage:voyage-3.5":                 "recall_voyage_voyage_3_5_v1",
	} {
		if got := defaultCollection(spec); got != want {
			t.Errorf("defaultCollection(%q) = %q, want %q", spec, got, want)
		}
	}
}

func TestChooseEmbedding(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_BASE_URL", "")
	for _, tc := range []struct {
		url, model, extract string
		spec                string
		derived             bool
	}{
		{"", "", "", "", false},
		{"", "", "anthropic:claude-opus-5-5", "", false},
		{"", "", "openai:gpt-5-mini", "openai:text-embedding-3-small@512", true},
		{"", "", "ollama:qwen3:8b", "ollama:nomic-embed-text", true},
		{"", "lexical", "openai:gpt-5-mini", "", false},
		{"", "ollama:mxbai-embed-large", "openai:gpt-5-mini", "ollama:mxbai-embed-large", false},
		{"http://127.0.0.1:8765", "", "openai:gpt-5-mini", "url:http://127.0.0.1:8765", false},
	} {
		got, err := chooseEmbedding(tc.url, tc.model, tc.extract)
		if err != nil || got.spec != tc.spec || got.derived != tc.derived {
			t.Errorf("chooseEmbedding(%q, %q, %q) = %q derived=%v, %v; want %q derived=%v",
				tc.url, tc.model, tc.extract, got.spec, got.derived, err, tc.spec, tc.derived)
		}
	}
	if _, err := chooseEmbedding("http://x", "openai", ""); err == nil {
		t.Error("-embed-url with -embed-model accepted")
	}
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := chooseEmbedding("", "", "openai:gpt-5-mini"); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("openai without a key: %v", err)
	}
}

func TestFitEmbedding(t *testing.T) {
	ctx := context.Background()
	b := newCollections()
	seed(t, b, "words", lexical, "America/Los_Angeles")
	seed(t, b, "meaning", fixed("openai:x@512", 512, false, nil), "America/Los_Angeles")

	// An empty or missing collection takes any embedder.
	if got, note, err := fitEmbedding(ctx, b, "new", fixed("openai:x@512", 512, true, nil)); err != nil || note != "" || got.spec != "openai:x@512" {
		t.Errorf("missing collection: %q %q %v", got.spec, note, err)
	}
	// Matching vectors fit.
	if _, _, err := fitEmbedding(ctx, b, "meaning", fixed("openai:x@512", 512, false, nil)); err != nil {
		t.Errorf("matching: %v", err)
	}
	// A followed embedder gives way to lexical vectors, with a note naming the copy.
	got, note, err := fitEmbedding(ctx, b, "words", fixed("openai:x@512", 512, true, nil))
	if err != nil || got.spec != "" || !strings.Contains(note, "recall reembed -from words") {
		t.Errorf("followed over lexical: %q %q %v", got.spec, note, err)
	}
	// A named embedder that does not fit is an error, either way round.
	if _, _, err := fitEmbedding(ctx, b, "words", fixed("openai:x@512", 512, false, nil)); err == nil || !strings.Contains(err.Error(), "holds lexical vectors") {
		t.Errorf("named over lexical: %v", err)
	}
	if _, _, err := fitEmbedding(ctx, b, "meaning", lexical); err == nil || !strings.Contains(err.Error(), "512-dimensional") {
		t.Errorf("lexical over model vectors: %v", err)
	}
	// polign_db answers 404 for a collection it has never seen.
	b.err = notFound{}
	if got, _, err := fitEmbedding(ctx, b, "words", fixed("openai:x@512", 512, false, nil)); err != nil || got.spec != "openai:x@512" {
		t.Errorf("404: %q, %v", got.spec, err)
	}
	// A database that cannot be read yet skips the check.
	b.err = errors.New("connection refused")
	if _, _, err := fitEmbedding(ctx, b, "words", fixed("openai:x@512", 512, false, nil)); err != nil {
		t.Errorf("unreadable database: %v", err)
	}
}

func TestFitMCPCollection(t *testing.T) {
	model := fixed("openai:text-embedding-3-small@512", 512, true, nil)

	// A new install writes the embedder's own collection.
	b := newCollections()
	collection := ""
	got, err := fitMCPCollection(b, &collection, model)
	if err != nil || got.spec != model.spec || collection != "recall_openai_text_embedding_3_small_512_v1" {
		t.Errorf("new install: %q in %q, %v", got.spec, collection, err)
	}

	// An upgrade with memory in the lexical default keeps reading it.
	b = newCollections()
	seed(t, b, defaultLexicalCollection, lexical, "America/Los_Angeles")
	collection = ""
	got, err = fitMCPCollection(b, &collection, model)
	if err != nil || got.spec != "" || collection != defaultLexicalCollection {
		t.Errorf("upgrade: %q in %q, %v", got.spec, collection, err)
	}

	// Once copied, the model collection is used.
	seed(t, b, "recall_openai_text_embedding_3_small_512_v1", model, "America/Denver")
	collection = ""
	got, err = fitMCPCollection(b, &collection, model)
	if err != nil || got.spec != model.spec || collection != "recall_openai_text_embedding_3_small_512_v1" {
		t.Errorf("after the copy: %q in %q, %v", got.spec, collection, err)
	}

	// A named embedder does not give way.
	collection = ""
	named := model
	named.derived = false
	if got, err = fitMCPCollection(newCollections(), &collection, named); err != nil || got.spec != named.spec {
		t.Errorf("named: %q, %v", got.spec, err)
	}

	collection = ""
	if _, err := fitMCPCollection(newCollections(), &collection, embedding{spec: "url:http://x"}); err == nil {
		t.Error("-embed-url without -collection accepted")
	}
}

func TestReembed(t *testing.T) {
	ctx := context.Background()
	b := newCollections()
	seed(t, b, "old", lexical, "America/Los_Angeles")
	seed(t, b, "old", lexical, "America/Denver")
	calls := 0
	dst := fixed("openai:x@512", 512, false, &calls)
	n, err := reembed(ctx, b, "old", lexical, "new", dst)
	if err != nil || n == 0 {
		t.Fatalf("reembed = %d, %v", n, err)
	}
	if calls != n+1 { // one check, then one per event
		t.Errorf("embedded %d times for %d events", calls, n)
	}
	// The copy holds the same beliefs, with the correction, under new vectors.
	c, err := recall.NewClient(recall.Config{Backend: b, Collection: "new", Embedder: dst, Open: true})
	if err != nil {
		t.Fatal(err)
	}
	beliefs, err := c.Recall(ctx, recall.Query{Subject: "user", Predicate: "timezone"})
	if err != nil || len(beliefs) != 1 || beliefs[0].Value != "America/Denver" || len(beliefs[0].Replaced) != 1 {
		t.Errorf("copied beliefs = %+v, %v", beliefs, err)
	}
	if dim, _ := storedDim(ctx, b, "new"); dim != 512 {
		t.Errorf("copy holds %d-dimensional vectors", dim)
	}
	if dim, _ := storedDim(ctx, b, "old"); dim != lexicalDim {
		t.Errorf("source changed to %d dimensions", dim)
	}
	// Copying again stores each event once.
	if _, err := reembed(ctx, b, "old", lexical, "new", dst); err != nil {
		t.Errorf("second copy: %v", err)
	}
	if again, _, _ := b.List(ctx, "new", nil, 1000); len(again) != n {
		t.Errorf("second copy left %d events, want %d", len(again), n)
	}
	// A target of another embedder, and an empty source, are refused.
	if _, err := reembed(ctx, b, "old", lexical, "new", fixed("ollama:y", 768, false, nil)); err == nil {
		t.Error("copy into a 512-dimensional collection with a 768-dimensional embedder accepted")
	}
	if _, err := reembed(ctx, b, "nothing", lexical, "other", dst); err == nil || !strings.Contains(err.Error(), "no memory") {
		t.Errorf("empty source: %v", err)
	}
}
