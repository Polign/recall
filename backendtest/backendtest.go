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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Polign/recall"
)

// Run runs every conformance check. newBackend is called once per subtest.
func Run(t *testing.T, newBackend func(t *testing.T) recall.Backend) {
	t.Helper()
	run := time.Now().UnixNano()
	collection := func(t *testing.T) string {
		name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
		return fmt.Sprintf("recall_conformance_%s_%d", strings.ToLower(name), run)
	}
	t.Run("MissingCollectionIsEmpty", func(t *testing.T) { missingCollection(t, newBackend(t), collection(t)) })
	t.Run("ListExactAndOrdered", func(t *testing.T) { listExact(t, newBackend(t), collection(t)) })
	t.Run("Filters", func(t *testing.T) { filters(t, newBackend(t), collection(t)) })
	t.Run("Search", func(t *testing.T) { search(t, newBackend(t), collection(t)) })
	t.Run("Get", func(t *testing.T) { get(t, newBackend(t), collection(t)) })
	t.Run("Client", func(t *testing.T) { client(t, newBackend(t), collection(t)) })
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
