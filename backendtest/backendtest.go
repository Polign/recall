// Package backendtest is a conformance suite for recall.Backend
// implementations. A backend package runs it against a live database:
//
//	func TestConformance(t *testing.T) {
//		backendtest.Run(t, func(t *testing.T) recall.Backend { return newBackend(t) })
//	}
//
// Each subtest uses its own collection, named from the test, so a suite can
// share one database with other runs. The suite checks the storage contract
// Recall's fold depends on (exact totals, a stable ordered prefix, filters,
// read-after-write) and then drives a Client over the backend end to end.
package backendtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Polign/recall"
)

// Run runs every conformance check. newBackend is called once per subtest.
// The checks for optional capabilities (Get, text search, watermarks, leases)
// skip on a backend that does not offer them.
func Run(t *testing.T, newBackend func(t *testing.T) recall.Backend) {
	t.Helper()
	collection := Collection
	t.Run("MissingCollectionIsEmpty", func(t *testing.T) { missingCollection(t, newBackend(t), collection(t)) })
	t.Run("ListExactAndOrdered", func(t *testing.T) { listExact(t, newBackend(t), collection(t)) })
	t.Run("Filters", func(t *testing.T) { filters(t, newBackend(t), collection(t)) })
	t.Run("Search", func(t *testing.T) { search(t, newBackend(t), collection(t)) })
	t.Run("Get", func(t *testing.T) { get(t, newBackend(t), collection(t)) })
	t.Run("Client", func(t *testing.T) { client(t, newBackend(t), collection(t)) })
	t.Run("TextSearch", func(t *testing.T) { textSearch(t, newBackend(t), collection(t)) })
	t.Run("Watermark", func(t *testing.T) { watermark(t, newBackend(t), collection(t)) })
	t.Run("Leases", func(t *testing.T) { leases(t, newBackend(t), collection(t)) })
	t.Run("Agent", func(t *testing.T) { agent(t, newBackend(t), collection(t)) })
}

var (
	run = time.Now().UnixNano()
	seq atomic.Int64
)

// Collection names a fresh collection for one test, from the test's name,
// the time the process started and a counter, so suites can share a
// database with other runs and with repeats of themselves (go test -count).
func Collection(t *testing.T) string {
	name := strings.NewReplacer("/", "_", " ", "_", "#", "_").Replace(t.Name())
	return fmt.Sprintf("recall_conformance_%s_%d_%d", strings.ToLower(name), run, seq.Add(1))
}

func vec(i int) []float32 {
	v := make([]float32, 8)
	v[i%8] = 1
	v[(i+1)%8] = 0.5
	return v
}

func put(t *testing.T, b recall.Backend, c string, n int, md func(i int) map[string]any) {
	t.Helper()
	for i := range n {
		if err := b.Put(context.Background(), c, fmt.Sprintf("m-%04d", i), vec(i), md(i)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
}

func missingCollection(t *testing.T, b recall.Backend, c string) {
	rows, total, err := b.List(context.Background(), c, map[string]any{"subject": "user"}, 10)
	if err != nil {
		var nf interface{ NotFound() bool }
		if !asNotFound(err, &nf) {
			t.Fatalf("list of an unwritten collection: %v (want empty, or an error reporting NotFound)", err)
		}
		return
	}
	if len(rows) != 0 || total != 0 {
		t.Fatalf("unwritten collection listed %d rows, total %d", len(rows), total)
	}
}

func asNotFound(err error, nf *interface{ NotFound() bool }) bool {
	for e := err; e != nil; {
		if v, ok := e.(interface{ NotFound() bool }); ok && v.NotFound() {
			*nf = v
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

func listExact(t *testing.T, b recall.Backend, c string) {
	ctx := context.Background()
	const n = 25
	put(t, b, c, n, func(i int) map[string]any {
		return map[string]any{"subject": "user", "predicate": "p", "n": float64(i), "flag": i%2 == 0, "text": fmt.Sprintf("row %d", i)}
	})
	all, total, err := b.List(ctx, c, map[string]any{"subject": "user"}, recall.MaxHistoryEvents+1)
	if err != nil {
		t.Fatal(err)
	}
	if total != n || len(all) != n {
		t.Fatalf("listed %d rows, total %d; want %d of %d", len(all), total, n, n)
	}
	seen := map[string]bool{}
	for _, r := range all {
		if seen[r.ID] {
			t.Fatalf("listing repeated %s", r.ID)
		}
		seen[r.ID] = true
		if _, ok := r.Metadata["n"].(float64); !ok {
			t.Fatalf("number metadata came back as %T, want float64", r.Metadata["n"])
		}
		if _, ok := r.Metadata["flag"].(bool); !ok {
			t.Fatalf("bool metadata came back as %T, want bool", r.Metadata["flag"])
		}
	}
	for _, limit := range []int{1, 7, n - 1} {
		prefix, total, err := b.List(ctx, c, map[string]any{"subject": "user"}, limit)
		if err != nil {
			t.Fatal(err)
		}
		if total != n || len(prefix) != limit {
			t.Fatalf("limit %d: listed %d, total %d", limit, len(prefix), total)
		}
		for i := range prefix {
			if prefix[i].ID != all[i].ID {
				t.Fatalf("limit %d: row %d is %s, the full listing has %s; a longer listing must extend a shorter one", limit, i, prefix[i].ID, all[i].ID)
			}
		}
	}
	// Overwriting an ID is an upsert, not a second row.
	if err := b.Put(ctx, c, "m-0000", vec(0), map[string]any{"subject": "user", "predicate": "p", "n": float64(0)}); err != nil {
		t.Fatal(err)
	}
	if _, total, err := b.List(ctx, c, map[string]any{"subject": "user"}, 1); err != nil || total != n {
		t.Fatalf("after re-put: total %d, err %v; want %d", total, err, n)
	}
}

func filters(t *testing.T, b recall.Backend, c string) {
	ctx := context.Background()
	put(t, b, c, 10, func(i int) map[string]any {
		subject := "alice"
		if i >= 6 {
			subject = "bob"
		}
		return map[string]any{"subject": subject, "predicate": "p", "observed_ms": float64(1000 * i), "retraction": i == 3}
	})
	cases := []struct {
		name   string
		filter map[string]any
		want   int
	}{
		{"none", nil, 10},
		{"string", map[string]any{"subject": "alice"}, 6},
		{"conjunction", map[string]any{"subject": "bob", "predicate": "p"}, 4},
		{"bool", map[string]any{"retraction": true}, 1},
		{"number", map[string]any{"observed_ms": float64(2000)}, 1},
		{"range", map[string]any{"observed_ms": map[string]any{"$gte": float64(2000), "$lte": float64(5000)}}, 4},
		{"range and string", map[string]any{"subject": "bob", "observed_ms": map[string]any{"$gte": float64(7000)}}, 3},
		{"no match", map[string]any{"subject": "carol"}, 0},
	}
	for _, tc := range cases {
		rows, total, err := b.List(ctx, c, tc.filter, 100)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if total != tc.want || len(rows) != tc.want {
			t.Fatalf("%s: listed %d, total %d; want %d", tc.name, len(rows), total, tc.want)
		}
	}
}

func search(t *testing.T, b recall.Backend, c string) {
	ctx := context.Background()
	put(t, b, c, 8, func(i int) map[string]any {
		return map[string]any{"subject": map[bool]string{true: "alice", false: "bob"}[i < 4], "predicate": "p"}
	})
	hits, err := b.Search(ctx, c, vec(2), 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || len(hits) > 3 {
		t.Fatalf("search returned %d hits, want 1 to 3", len(hits))
	}
	if hits[0].ID != "m-0002" {
		t.Fatalf("nearest hit is %s, want m-0002", hits[0].ID)
	}
	if hits[0].Metadata["subject"] != "alice" {
		t.Fatalf("hit metadata %v", hits[0].Metadata)
	}
	hits, err = b.Search(ctx, c, vec(2), 8, map[string]any{"subject": "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 4 {
		t.Fatalf("filtered search returned %d hits, want 4", len(hits))
	}
	for _, h := range hits {
		if h.Metadata["subject"] != "bob" {
			t.Fatalf("filtered search returned %s for %v", h.ID, h.Metadata["subject"])
		}
	}
}

func get(t *testing.T, b recall.Backend, c string) {
	g, ok := b.(recall.GetBackend)
	if !ok {
		t.Skip("backend does not implement recall.GetBackend")
	}
	ctx := context.Background()
	put(t, b, c, 3, func(i int) map[string]any { return map[string]any{"subject": "user", "i": float64(i)} })
	rows, err := g.Get(ctx, c, []string{"m-0002", "m-9999", "m-0000"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != "m-0002" || rows[1].ID != "m-0000" {
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
		}
		t.Fatalf("get returned %v, want [m-0002 m-0000]", ids)
	}
	if rows[0].Metadata["i"] != float64(2) {
		t.Fatalf("get metadata %v", rows[0].Metadata)
	}
}

func client(t *testing.T, b recall.Backend, c string) {
	ctx := context.Background()
	cl, err := recall.NewClient(recall.Config{Backend: b, Collection: c, Registry: recall.DefaultRegistry(), Embedder: recall.LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	remember := func(pred, value string) {
		t.Helper()
		if _, err := cl.Remember(ctx, recall.RememberRequest{Subject: "user", Predicate: pred, Value: value}); err != nil {
			t.Fatalf("remember %s=%s: %v", pred, value, err)
		}
	}
	remember("prefers_editor", "vim")
	remember("uses_technology", "go")
	remember("uses_technology", "qdrant")
	remember("prefers_editor", "neovim")

	beliefs, err := cl.Recall(ctx, recall.Query{Subject: "user", Predicate: "prefers_editor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(beliefs) != 1 || beliefs[0].Value != "neovim" {
		t.Fatalf("prefers_editor beliefs %+v, want neovim alone", beliefs)
	}
	history, err := cl.History(ctx, "user", "prefers_editor")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("history has %d events, want 2", len(history))
	}
	beliefs, err = cl.Recall(ctx, recall.Query{Subject: "user", Predicate: "uses_technology"})
	if err != nil {
		t.Fatal(err)
	}
	if len(beliefs) != 2 {
		t.Fatalf("uses_technology beliefs %+v, want two", beliefs)
	}
	if _, err := cl.Forget(ctx, recall.ForgetRequest{Subject: "user", Predicate: "uses_technology", Value: "go"}); err != nil {
		t.Fatal(err)
	}
	beliefs, err = cl.Recall(ctx, recall.Query{Subject: "user", Predicate: "uses_technology"})
	if err != nil {
		t.Fatal(err)
	}
	if len(beliefs) != 1 || beliefs[0].Value != "qdrant" {
		t.Fatalf("after forget: %+v, want qdrant alone", beliefs)
	}
	beliefs, err = cl.Recall(ctx, recall.Query{Text: "which editor does the user prefer"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, bl := range beliefs {
		if bl.Predicate == "prefers_editor" {
			found = true
			if bl.Value != "neovim" {
				t.Fatalf("semantic recall returned superseded %v", bl.Value)
			}
		}
	}
	if !found {
		t.Fatalf("semantic recall missed prefers_editor: %+v", beliefs)
	}
}

func textSearch(t *testing.T, b recall.Backend, c string) {
	ts, ok := b.(recall.TextSearchBackend)
	if !ok {
		t.Skip("backend does not implement recall.TextSearchBackend")
	}
	ctx := context.Background()
	texts := []string{"the user prefers the vim editor", "the user works in Seattle", "the project is written in go", "the user dislikes emacs"}
	put(t, b, c, len(texts), func(i int) map[string]any {
		return map[string]any{"subject": map[bool]string{true: "user", false: "project"}[i != 2], recall.TextField: texts[i]}
	})
	hits, err := ts.SearchText(ctx, c, "which editor does the user prefer", 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].ID != "m-0000" {
		ids := make([]string, len(hits))
		for i, h := range hits {
			ids[i] = h.ID
		}
		t.Fatalf("text search ranked %v, want m-0000 first", ids)
	}
	if hits[0].Metadata["subject"] != "user" {
		t.Fatalf("hit metadata %v", hits[0].Metadata)
	}
	hits, err = ts.SearchText(ctx, c, "user", 10, map[string]any{"subject": "project"})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Metadata["subject"] != "project" {
			t.Fatalf("filtered text search returned %s for %v", h.ID, h.Metadata["subject"])
		}
	}
	if _, err := ts.SearchText(ctx, c, "zzzz qqqq", 10, nil); err != nil {
		t.Fatalf("text search with no match: %v", err)
	}
}

func watermark(t *testing.T, b recall.Backend, c string) {
	wb, ok := b.(recall.WatermarkBackend)
	if !ok {
		t.Skip("backend does not implement recall.WatermarkBackend")
	}
	ctx := context.Background()
	w0, err := wb.Watermark(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	put(t, b, c, 1, func(int) map[string]any { return map[string]any{"subject": "user"} })
	w1, err := wb.Watermark(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if w1 == w0 {
		t.Fatalf("watermark %q did not change on the first write", w1)
	}
	again, err := wb.Watermark(ctx, c)
	if err != nil || again != w1 {
		t.Fatalf("watermark moved without a write: %q then %q (%v)", w1, again, err)
	}
	// An overwrite of an existing ID is a visible mutation too.
	if err := b.Put(ctx, c, "m-0000", vec(0), map[string]any{"subject": "user", "n": float64(1)}); err != nil {
		t.Fatal(err)
	}
	w2, err := wb.Watermark(ctx, c)
	if err != nil || w2 == w1 {
		t.Fatalf("watermark %q did not change on an overwrite (%v)", w2, err)
	}
	// With watermarks, a materializing client serves the same answers.
	cl, err := recall.NewClient(recall.Config{Backend: b, Collection: c + "_m", Registry: recall.DefaultRegistry(), Embedder: recall.LexicalEmbedder{}, Materialize: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"vim", "neovim"} {
		if _, err := cl.Remember(ctx, recall.RememberRequest{Subject: "user", Predicate: "prefers_editor", Value: v}); err != nil {
			t.Fatal(err)
		}
		beliefs, err := cl.Recall(ctx, recall.Query{Subject: "user", Predicate: "prefers_editor"})
		if err != nil || len(beliefs) != 1 || beliefs[0].Value != v {
			t.Fatalf("materialized recall after %s: %+v, %v", v, beliefs, err)
		}
	}
}

func leases(t *testing.T, b recall.Backend, c string) {
	lb, ok := b.(recall.LeaseBackend)
	if !ok {
		t.Skip("backend does not implement recall.LeaseBackend")
	}
	ctx := context.Background()
	g1, err := lb.AcquireLease(ctx, c, "agent-1", "pod-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if g1.Epoch != 1 || g1.Holder != "pod-a" {
		t.Fatalf("first grant %+v", g1)
	}
	_, err = lb.AcquireLease(ctx, c, "agent-1", "pod-b", time.Minute)
	var held *recall.LeaseHeldError
	if !errors.As(err, &held) || held.Holder != "pod-a" || held.Expires.Before(time.Now()) {
		t.Fatalf("second acquire: %v, want LeaseHeldError for pod-a", err)
	}
	// Another lease name is independent.
	if _, err := lb.AcquireLease(ctx, c, "agent-2", "pod-b", time.Minute); err != nil {
		t.Fatalf("other agent: %v", err)
	}
	g2, err := lb.RenewLease(ctx, c, "agent-1", "pod-a", g1.Epoch, time.Minute)
	if err != nil || g2.Epoch <= g1.Epoch {
		t.Fatalf("renew: %+v, %v", g2, err)
	}
	if _, err := lb.RenewLease(ctx, c, "agent-1", "pod-a", g1.Epoch, time.Minute); !errors.Is(err, recall.ErrLeaseLost) {
		t.Fatalf("renew with a stale epoch: %v, want ErrLeaseLost", err)
	}
	// Release with a stale epoch does nothing; with the current one it frees the lease.
	if err := lb.ReleaseLease(ctx, c, "agent-1", "pod-a", g1.Epoch); err != nil {
		t.Fatal(err)
	}
	if _, err := lb.AcquireLease(ctx, c, "agent-1", "pod-b", time.Minute); !errors.As(err, &held) {
		t.Fatalf("acquire after a stale release: %v, want held", err)
	}
	if err := lb.ReleaseLease(ctx, c, "agent-1", "pod-a", g2.Epoch); err != nil {
		t.Fatal(err)
	}
	g3, err := lb.AcquireLease(ctx, c, "agent-1", "pod-b", time.Minute)
	if err != nil || g3.Epoch <= g2.Epoch || g3.Holder != "pod-b" {
		t.Fatalf("acquire after release: %+v, %v", g3, err)
	}
	if _, err := lb.RenewLease(ctx, c, "agent-1", "pod-a", g2.Epoch, time.Minute); !errors.Is(err, recall.ErrLeaseLost) {
		t.Fatalf("old holder renew: %v, want ErrLeaseLost", err)
	}
	// A lapsed lease can be taken, and the epoch keeps growing.
	if _, err := lb.AcquireLease(ctx, c, "agent-3", "pod-a", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	g4, err := lb.AcquireLease(ctx, c, "agent-3", "pod-b", time.Minute)
	if err != nil || g4.Epoch != 2 {
		t.Fatalf("acquire after lapse: %+v, %v", g4, err)
	}
}

// agent drives Client.Resume end to end: with a lease where the backend
// grants them, and unleased otherwise.
func agent(t *testing.T, b recall.Backend, c string) {
	ctx := context.Background()
	_, leased := b.(recall.LeaseBackend)
	cl, err := recall.NewClient(recall.Config{Backend: b, Collection: c, Registry: recall.DefaultRegistry(), Embedder: recall.LexicalEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	req := recall.ResumeRequest{AgentID: "a1", Unleased: !leased}
	if leased {
		if _, err := cl.Resume(ctx, recall.ResumeRequest{AgentID: "a1", Unleased: true}); err != nil {
			t.Fatalf("unleased resume on a leasing backend: %v", err)
		}
	} else if _, err := cl.Resume(ctx, recall.ResumeRequest{AgentID: "a1"}); !errors.Is(err, recall.ErrLeaseUnsupported) {
		t.Fatalf("leased resume: %v, want ErrLeaseUnsupported", err)
	}
	a, err := cl.Resume(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.UpdateWorkingState(ctx, recall.WorkingState{Goal: "ship it"}); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := a.RecordTurn(ctx, recall.Turn{Role: "user", Content: fmt.Sprintf("turn %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.SetPointer(ctx, recall.Pointer{Name: "repo", Type: recall.PointerGitRef, Fields: map[string]string{"branch": "main"}}); err != nil {
		t.Fatal(err)
	}
	if leased {
		if _, err := cl.Resume(ctx, req); !errors.Is(err, recall.ErrLeaseHeld) {
			t.Fatalf("second leased resume: %v, want ErrLeaseHeld", err)
		}
		if err := a.Release(ctx); err != nil {
			t.Fatal(err)
		}
	}
	again, err := cl.Resume(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.WorkingState().Goal; got != "ship it" {
		t.Fatalf("goal %q after resume", got)
	}
	if turns, err := again.RecentTurns(ctx, 10); err != nil || len(turns) != 3 {
		t.Fatalf("turns %d, %v; want 3", len(turns), err)
	}
	if ps, err := again.Pointers(ctx); err != nil || len(ps) != 1 {
		t.Fatalf("pointers %d, %v; want 1", len(ps), err)
	}
	if leased {
		if err := again.Release(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
