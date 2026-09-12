package recall

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
)

type lockedBackend struct {
	mu sync.Mutex
	db *fakeDB
}

func newLockedBackend() *lockedBackend { return &lockedBackend{db: newFakeDB()} }
func (b *lockedBackend) Put(ctx context.Context, c, id string, v []float32, md map[string]any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.db.Put(c, id, v, md)
}
func (b *lockedBackend) List(ctx context.Context, c string, f map[string]any, n int) ([]StoredVector, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	return b.db.List(c, f, n)
}
func (b *lockedBackend) Search(ctx context.Context, c string, v []float32, n int, f map[string]any) ([]Hit, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.db.Search(c, v, n, f)
}

type backendFuncs struct {
	put    func(context.Context, string, string, []float32, map[string]any) error
	list   func(context.Context, string, map[string]any, int) ([]StoredVector, int, error)
	search func(context.Context, string, []float32, int, map[string]any) ([]Hit, error)
}

func (b backendFuncs) Put(ctx context.Context, c, id string, v []float32, m map[string]any) error {
	return b.put(ctx, c, id, v, m)
}
func (b backendFuncs) List(ctx context.Context, c string, f map[string]any, n int) ([]StoredVector, int, error) {
	return b.list(ctx, c, f, n)
}
func (b backendFuncs) Search(ctx context.Context, c string, v []float32, n int, f map[string]any) ([]Hit, error) {
	return b.search(ctx, c, v, n, f)
}

func clientFor(t *testing.T, b Backend, e Embedder) *Client {
	t.Helper()
	c, err := NewClient(Config{Backend: b, Collection: "memories", Registry: testRegistry(t), Embedder: e})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func testEmbed(context.Context, string) ([]float32, error) { return []float32{1, 0, 0}, nil }

func TestClientTypedRequestsAndHistory(t *testing.T) {
	ctx := t.Context()
	c := clientFor(t, newLockedBackend(), EmbedFunc(testEmbed))
	for _, tc := range []struct {
		predicate string
		value     any
	}{{"uses_dark_mode", false}, {"daily_step_goal", float64(0)}, {"likes", "go"}} {
		result, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: tc.predicate, Value: tc.value})
		if err != nil || result.Stored.Value != tc.value {
			t.Fatalf("remember = %+v, %v", result, err)
		}
		if result.Stored.Kind != "fact" || result.Stored.Confidence != 1 || result.Stored.Source != "user_stated" {
			t.Fatalf("defaults = %+v", result.Stored)
		}
		if n, err := c.Forget(ctx, ForgetRequest{Subject: "user", Predicate: tc.predicate, Value: tc.value}); err != nil || n != 1 {
			t.Fatalf("targeted forget = %d, %v", n, err)
		}
		if got, err := c.Recall(ctx, Query{Subject: "user", Predicate: tc.predicate}); err != nil || len(got) != 0 {
			t.Fatalf("current recall = %+v, %v", got, err)
		}
		if got, err := c.Recall(ctx, Query{Subject: "user", Predicate: tc.predicate, AsOf: result.Stored.ObservedAt}); err != nil || len(got) != 1 || got[0].Value != tc.value {
			t.Fatalf("historical recall = %+v, %v", got, err)
		}
	}
	for _, q := range []ForgetRequest{{Subject: "user", Predicate: "likes"}, {Subject: "user", Predicate: "likes", Value: "go", All: true}, {Subject: "user", Predicate: "uses_dark_mode", Value: "false"}} {
		if _, err := c.Forget(ctx, q); err == nil {
			t.Fatalf("invalid forget accepted: %+v", q)
		}
	}
	for _, v := range []string{"go", "rust"} {
		if _, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "likes", Value: v}); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := c.Forget(ctx, ForgetRequest{Subject: "user", Predicate: "likes", All: true}); err != nil || n != 2 {
		t.Fatalf("blanket forget = %d, %v", n, err)
	}
	zero := 0.0
	r, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "prefers_editor", Value: "vim", Confidence: &zero})
	if err != nil || r.Stored.Confidence != 0 {
		t.Fatalf("explicit zero confidence = %+v, %v", r, err)
	}
	if events, err := c.ExportEvents(ctx, Export{}); err != nil || len(events) != 10 {
		t.Fatalf("export length = %d, %v", len(events), err)
	}
}

func TestClientRegistryIsImmutable(t *testing.T) {
	reg := testRegistry(t)
	b := newLockedBackend()
	c, err := NewClient(Config{Backend: b, Collection: "memories", Registry: reg, Embedder: EmbedFunc(testEmbed)})
	if err != nil {
		t.Fatal(err)
	}
	reg["likes"] = Predicate{Cardinality: "single", ValueType: "number"}
	copy := c.Registry()
	delete(copy, "likes")
	if _, err := c.Remember(t.Context(), RememberRequest{Subject: "user", Predicate: "likes", Value: "go"}); err != nil {
		t.Fatal(err)
	}
	if c.Registry()["likes"].Cardinality != "multi" {
		t.Fatal("registry mutated")
	}
	// Legacy construction and Registry also avoid sharing mutable maps.
	legacyReg := testRegistry(t)
	s := NewStore(newFakeDB(), "memories", legacyReg, func(string) []float32 { return []float32{1} })
	delete(legacyReg, "likes")
	delete(s.Registry(), "likes")
	if _, err := s.Remember("fact", "user", "likes", "go", 1, "user_stated"); err != nil {
		t.Fatal(err)
	}
}

func TestClientConfigurationAndReadOnlyUse(t *testing.T) {
	var nilBackend *lockedBackend
	for _, cfg := range []Config{
		{Backend: nilBackend, Collection: "memories", Registry: testRegistry(t)},
		{Backend: newLockedBackend(), Registry: testRegistry(t)},
		{Backend: newLockedBackend(), Collection: "memories"},
		{Backend: newLockedBackend(), Collection: "memories", Registry: Registry{"likes": {Cardinality: "invalid"}}},
	} {
		if _, err := NewClient(cfg); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	c := clientFor(t, newLockedBackend(), EmbedFunc(nil))
	if events, err := c.History(t.Context(), "user", "likes"); err != nil || len(events) != 0 {
		t.Fatalf("read-only history = %v, %v", events, err)
	}
	if _, err := c.Remember(t.Context(), RememberRequest{Subject: "user", Predicate: "likes", Value: "go"}); !errors.Is(err, ErrEmbedderRequired) {
		t.Fatalf("missing embedder = %v", err)
	}
	if _, err := c.Recall(t.Context(), Query{Text: "languages"}); !errors.Is(err, ErrEmbedderRequired) {
		t.Fatalf("semantic missing embedder = %v", err)
	}
}

func TestClientPreservesEmbeddingErrorsWithoutWrites(t *testing.T) {
	boom := errors.New("embedding unavailable")
	b := newLockedBackend()
	c := clientFor(t, b, EmbedFunc(func(context.Context, string) ([]float32, error) { return nil, boom }))
	if _, err := c.Remember(t.Context(), RememberRequest{Subject: "user", Predicate: "likes", Value: "go"}); !errors.Is(err, boom) {
		t.Fatalf("write embedding error = %v", err)
	}
	if _, err := c.Recall(t.Context(), Query{Text: "languages"}); !errors.Is(err, boom) {
		t.Fatalf("query embedding error = %v", err)
	}
	if b.db.puts != 0 {
		t.Fatal("embedding failure wrote a record")
	}
	for _, values := range [][]float32{nil, {}, {float32(math.NaN())}, {float32(math.Inf(1))}} {
		c := clientFor(t, b, EmbedFunc(func(context.Context, string) ([]float32, error) { return values, nil }))
		if _, err := c.Remember(t.Context(), RememberRequest{Subject: "user", Predicate: "likes", Value: "go"}); err == nil {
			t.Fatal("invalid embedding accepted")
		}
	}
	if b.db.puts != 0 {
		t.Fatal("invalid embedding wrote a record")
	}
}

func TestClientCancellationBeforeEveryOperation(t *testing.T) {
	c := clientFor(t, backendFuncs{}, EmbedFunc(testEmbed)) // Any backend call panics.
	for _, ctx := range []context.Context{nil, func() context.Context { ctx, cancel := context.WithCancel(t.Context()); cancel(); return ctx }()} {
		for _, call := range []func() error{
			func() error { _, err := c.Remember(ctx, RememberRequest{}); return err },
			func() error { _, err := c.Forget(ctx, ForgetRequest{}); return err },
			func() error { _, err := c.Recall(ctx, Query{}); return err },
			func() error { _, err := c.History(ctx, "user", "likes"); return err },
			func() error { _, err := c.ExportEvents(ctx, Export{}); return err },
		} {
			if err := call(); err == nil || ctx != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled operation = %v", err)
			}
		}
	}
}

func TestClientCancellationDuringHistoryPreventsWrite(t *testing.T) {
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := backendFuncs{list: func(ctx context.Context, _ string, _ map[string]any, _ int) ([]StoredVector, int, error) {
		close(entered)
		<-ctx.Done()
		return nil, 0, ctx.Err()
	}}
	c := clientFor(t, b, EmbedFunc(func(context.Context, string) ([]float32, error) {
		t.Error("embedding started after cancellation")
		return []float32{1}, nil
	}))
	done := make(chan error, 1)
	go func() {
		_, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "likes", Value: "go"})
		done <- err
	}()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestClientConcurrentCallsKeepContextsAndErrorsSeparate(t *testing.T) {
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := clientFor(t, newLockedBackend(), EmbedFunc(func(ctx context.Context, text string) ([]float32, error) {
		if strings.HasPrefix(text, "blocked ") {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []float32{1, 0, 0}, nil
	}))
	done := make(chan error, 1)
	go func() {
		_, err := c.Remember(ctx, RememberRequest{Subject: "blocked", Predicate: "likes", Value: "go"})
		done <- err
	}()
	<-entered
	if _, err := c.Remember(t.Context(), RememberRequest{Subject: "healthy", Predicate: "likes", Value: "rust"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("blocked call = %v", err)
	}
	if got, err := c.History(t.Context(), "healthy", "likes"); err != nil || len(got) != 1 || got[0].Value != "rust" {
		t.Fatalf("healthy history = %v, %v", got, err)
	}
	if got, err := c.History(t.Context(), "blocked", "likes"); err != nil || len(got) != 0 {
		t.Fatalf("cancelled history = %v, %v", got, err)
	}
}

func TestClientPropagatesContextAndBackendErrors(t *testing.T) {
	type traceKey struct{}
	ctx := context.WithValue(t.Context(), traceKey{}, "trace")
	check := func(ctx context.Context) {
		t.Helper()
		if ctx.Value(traceKey{}) != "trace" {
			t.Fatal("request context lost")
		}
	}
	boom := errors.New("backend unavailable")
	b := backendFuncs{
		list: func(ctx context.Context, _ string, _ map[string]any, _ int) ([]StoredVector, int, error) {
			check(ctx)
			return nil, 0, nil
		},
		put: func(ctx context.Context, _, _ string, _ []float32, _ map[string]any) error { check(ctx); return boom },
		search: func(ctx context.Context, _ string, _ []float32, _ int, _ map[string]any) ([]Hit, error) {
			check(ctx)
			return nil, boom
		},
	}
	c := clientFor(t, b, EmbedFunc(func(ctx context.Context, _ string) ([]float32, error) { check(ctx); return []float32{1}, nil }))
	if _, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "likes", Value: "go"}); !errors.Is(err, boom) {
		t.Fatalf("write error = %v", err)
	}
	if _, err := c.Recall(ctx, Query{Text: "languages"}); !errors.Is(err, boom) {
		t.Fatalf("search error = %v", err)
	}
	b.list = func(ctx context.Context, _ string, _ map[string]any, _ int) ([]StoredVector, int, error) {
		check(ctx)
		return nil, 0, boom
	}
	c = clientFor(t, b, nil)
	if _, err := c.History(ctx, "user", "likes"); !errors.Is(err, boom) {
		t.Fatalf("history error = %v", err)
	}
}

func TestClientRejectsReadReturnedAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := backendFuncs{list: func(context.Context, string, map[string]any, int) ([]StoredVector, int, error) {
		cancel()
		return nil, 0, nil
	}}
	c := clientFor(t, b, EmbedFunc(func(context.Context, string) ([]float32, error) {
		t.Error("embedding started after cancelled read")
		return []float32{1}, nil
	}))
	if _, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "likes", Value: "go"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("late read = %v", err)
	}
}
