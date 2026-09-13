package recall

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

type revisionBackend struct {
	*lockedBackend
	revision     int
	lists        int
	changeOnList bool
	frozen       bool // a backend whose revision does not advance on a write
}

func (b *revisionBackend) Watermark(context.Context, string) (string, error) {
	return fmt.Sprint(b.revision), nil
}
func (b *revisionBackend) Put(ctx context.Context, c, id string, v []float32, m map[string]any) error {
	err := b.lockedBackend.Put(ctx, c, id, v, m)
	if err == nil && !b.frozen {
		b.revision++
	}
	return err
}
func (b *revisionBackend) List(ctx context.Context, c string, f map[string]any, n int) ([]StoredVector, int, error) {
	b.lists++
	if b.changeOnList {
		b.revision++
	}
	return b.lockedBackend.List(ctx, c, f, n)
}

func TestMaterializationReusesAndInvalidatesVisibleLog(t *testing.T) {
	b := &revisionBackend{lockedBackend: newLockedBackend()}
	c, err := NewClient(Config{Backend: b, Collection: "memory", Registry: DefaultRegistry(), Embedder: LexicalEmbedder{}, Materialize: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	q := Query{Subject: "user", Predicate: "prefers_editor"}
	first, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "prefers_editor", Value: "vim"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Recall(ctx, q)
	if err != nil || len(got) != 1 {
		t.Fatalf("first read: %v %v", got, err)
	}
	reads := b.lists
	got[0].Value = "caller mutation"
	got, err = c.Recall(ctx, q)
	if err != nil || got[0].Value != "vim" || b.lists != reads {
		t.Fatalf("cache miss or alias: %v %v reads=%d", got, err, b.lists)
	}
	// Another client changes the log. The reader must discover this itself.
	writer, err := NewClient(Config{Backend: b, Collection: "memory", Registry: DefaultRegistry(), Embedder: LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Remember(ctx, RememberRequest{Subject: "user", Predicate: "prefers_editor", Value: "neovim"}); err != nil {
		t.Fatal(err)
	}
	got, err = c.Recall(ctx, q)
	if err != nil || got[0].Value != "neovim" {
		t.Fatalf("stale cache %v %v", got, err)
	}
	q.AsOf = first.Stored.ObservedAt
	got, err = c.Recall(ctx, q)
	if err != nil || got[0].Value != "vim" {
		t.Fatalf("historical cache %v %v", got, err)
	}
	q.AsOf = time.Time{}
	if _, err := writer.Forget(ctx, ForgetRequest{Subject: "user", Predicate: "prefers_editor", All: true}); err != nil {
		t.Fatal(err)
	}
	got, err = c.Recall(ctx, q)
	if err != nil || len(got) != 0 {
		t.Fatalf("retraction cache %v %v", got, err)
	}
	reads = b.lists
	_, err = c.Recall(ctx, q)
	if err != nil || b.lists != reads {
		t.Fatal("empty beliefs were not cached")
	}
}

func TestMaterializationFutureEventsLateArrivalsAndTornHistory(t *testing.T) {
	b := &revisionBackend{lockedBackend: newLockedBackend()}
	s := NewStore(requestBackend{ctx: t.Context(), backend: b}, "memory", DefaultRegistry(), func(string) []float32 { return []float32{1} })
	s.materialized = &Materialization{}
	e := Event{ID: "future", Subject: "user", Predicate: "prefers_editor", Value: "neovim", Kind: "fact", Source: "user_stated", Confidence: 1, ObservedAt: at(2 * time.Hour)}
	if err := b.Put(t.Context(), "memory", e.ID, []float32{1}, e.Metadata()); err != nil {
		t.Fatal(err)
	}
	p := pair{"user", "prefers_editor"}
	got, err := s.pairBeliefs(p, at(time.Hour))
	if err != nil || len(got) != 0 {
		t.Fatalf("future: %v %v", got, err)
	}
	got, err = s.pairBeliefs(p, at(3*time.Hour))
	if err != nil || len(got) != 1 {
		t.Fatalf("future activation: %v %v", got, err)
	}
	// A late event with a lower observation timestamp changes historical reads.
	e.ID = "late"
	e.Value = "vim"
	e.ObservedAt = at(time.Hour)
	if err := b.Put(t.Context(), "memory", e.ID, []float32{1}, e.Metadata()); err != nil {
		t.Fatal(err)
	}
	got, err = s.pairBeliefs(p, at(90*time.Minute))
	if err != nil || len(got) != 1 || got[0].Value != "vim" {
		t.Fatalf("late: %v %v", got, err)
	}
	// Same ID, different contents must also invalidate, even though row count is unchanged.
	e.Value = "emacs"
	if err := b.Put(t.Context(), "memory", e.ID, []float32{1}, e.Metadata()); err != nil {
		t.Fatal(err)
	}
	got, err = s.pairBeliefs(p, at(90*time.Minute))
	if err != nil || got[0].Value != "emacs" {
		t.Fatalf("overwrite: %v %v", got, err)
	}
	// A revision that never settles must not fail the read. It covers the whole
	// collection, so it also moves for a write to an unrelated pair, and
	// completeEvents is what actually establishes this history's completeness.
	// The answer is still served, uncached.
	b.revision++
	b.changeOnList = true
	// At this ceiling both events are visible, so the later one holds.
	got, err = s.pairBeliefs(p, at(3*time.Hour))
	if err != nil || len(got) != 1 || got[0].Value != "neovim" {
		t.Fatalf("unsettled revision did not degrade to an uncached fold: %v %v", got, err)
	}
	reads := b.lists
	if _, err := s.pairBeliefs(p, at(3*time.Hour)); err != nil || b.lists == reads {
		t.Fatalf("an unsettled revision must not populate the cache: %v", err)
	}
}

func TestDefaultRegistryAndLexicalSpace(t *testing.T) {
	r := DefaultRegistry()
	if len(r) != 15 || r.Validate() != nil {
		t.Fatal("invalid defaults")
	}
	delete(r, "name")
	if len(DefaultRegistry()) != 15 {
		t.Fatal("registry aliases global state")
	}
	e := LexicalEmbedder{}
	a, err := e.Embed(t.Context(), "Neovim, editor!")
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Embed(t.Context(), "editor neovim")
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("lexical space is not stable")
	}
	c, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.Embed(c, "x"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// Correctness must not rest on the backend advancing its revision. A lagging
// replica, a caching proxy in front of the endpoint, or a counter that resets
// all leave the revision equal across a write this client made itself, and a
// client has to read its own writes regardless.
func TestMaterializationInvalidatesOnItsOwnWrite(t *testing.T) {
	ctx := t.Context()
	b := &revisionBackend{lockedBackend: newLockedBackend(), frozen: true}
	c, err := NewClient(Config{Backend: b, Collection: "memory", Registry: DefaultRegistry(), Embedder: LexicalEmbedder{}, Materialize: true})
	if err != nil {
		t.Fatal(err)
	}
	q := Query{Subject: "user", Predicate: "prefers_editor"}
	if _, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "prefers_editor", Value: "vim"}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Recall(ctx, q); err != nil || got[0].Value != "vim" {
		t.Fatalf("first read: %v %v", got, err)
	}
	if _, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "prefers_editor", Value: "neovim"}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Recall(ctx, q); err != nil || len(got) != 1 || got[0].Value != "neovim" {
		t.Fatalf("client did not read its own correction back: %v %v", got, err)
	}
	if _, err := c.Forget(ctx, ForgetRequest{Subject: "user", Predicate: "prefers_editor", All: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Recall(ctx, q); err != nil || len(got) != 0 {
		t.Fatalf("retracted belief served from cache: %v %v", got, err)
	}
}

// A revision that cannot be read at all is not a reason to fail a read the
// uncached path would have served.
func TestMaterializationDegradesWhenTheRevisionIsUnreadable(t *testing.T) {
	ctx := t.Context()
	b := &brokenWatermarkBackend{lockedBackend: newLockedBackend()}
	c, err := NewClient(Config{Backend: b, Collection: "memory", Registry: DefaultRegistry(), Embedder: LexicalEmbedder{}, Materialize: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Remember(ctx, RememberRequest{Subject: "user", Predicate: "prefers_editor", Value: "vim"}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Recall(ctx, Query{Subject: "user", Predicate: "prefers_editor"})
	if err != nil || len(got) != 1 || got[0].Value != "vim" {
		t.Fatalf("unreadable revision failed the read: %v %v", got, err)
	}
}

type brokenWatermarkBackend struct{ *lockedBackend }

func (b *brokenWatermarkBackend) Watermark(context.Context, string) (string, error) {
	return "", errors.New("watermark: connection reset")
}
