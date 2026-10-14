package pgvector

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Polign/recall"
	"github.com/Polign/recall/backendtest"
)

// live opens a backend on the database RECALL_PGVECTOR_URL names, such as
// postgres://postgres:recall@localhost:5433/postgres, and skips without one.
func live(t *testing.T) *Backend {
	t.Helper()
	url := os.Getenv("RECALL_PGVECTOR_URL")
	if url == "" {
		t.Skip("set RECALL_PGVECTOR_URL to run against PostgreSQL with pgvector")
	}
	b, err := New(Config{URL: url, Schema: "recall_test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return b
}

// TestConformance runs the shared backend suite, including its lease,
// watermark and text search checks, which this backend supports.
func TestConformance(t *testing.T) {
	live(t)
	backendtest.Run(t, func(t *testing.T) recall.Backend { return live(t) })
}

func TestDimensionIsFixedByFirstWrite(t *testing.T) {
	b := live(t)
	ctx := context.Background()
	c := backendtest.Collection(t)
	if err := b.Put(ctx, c, "a", []float32{1, 0, 0}, map[string]any{"subject": "u"}); err != nil {
		t.Fatal(err)
	}
	err := b.Put(ctx, c, "b", []float32{1, 0}, map[string]any{"subject": "u"})
	if err == nil || !strings.Contains(err.Error(), "3-dimensional") {
		t.Fatalf("second dimension accepted: %v", err)
	}
	if _, err := b.Search(ctx, c, []float32{1, 0}, 1, nil); err == nil {
		t.Fatal("search with the wrong dimension accepted")
	}
	// The table registered for the collection is the one TableName predicts.
	var tbl string
	if err := b.Pool().QueryRow(ctx, "SELECT tbl FROM recall_test.recall_collections WHERE name = $1", c).Scan(&tbl); err != nil {
		t.Fatal(err)
	}
	if tbl != TableName(c) {
		t.Fatalf("table %q, TableName gives %q", tbl, TableName(c))
	}
}

func TestCloseLeavesSuppliedPool(t *testing.T) {
	owner := live(t)
	b, err := New(Config{Pool: owner.Pool(), Schema: "recall_test"})
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	if err := owner.Pool().Ping(context.Background()); err != nil {
		t.Fatalf("Close closed a pool it was given: %v", err)
	}
}

func TestTableName(t *testing.T) {
	cases := map[string]string{
		"memory":            "memory",
		"recall_lexical_v1": "recall_lexical_v1",
	}
	for in, want := range cases {
		if got := TableName(in); got != want {
			t.Errorf("TableName(%q) = %q, want %q", in, got, want)
		}
	}
	// Names that are not plain identifiers get a hash so they cannot collide.
	a, b := TableName("Memory"), TableName("memory")
	if a == b || !strings.HasPrefix(a, "memory_") || len(a) != len("memory_")+8 {
		t.Fatalf("TableName(Memory) = %q, TableName(memory) = %q", a, b)
	}
	if x, y := TableName("a-b"), TableName("a_b"); x == y {
		t.Fatalf("a-b and a_b share table %q", x)
	}
	if got := TableName("1st"); !strings.HasPrefix(got, "c_1st_") {
		t.Fatalf("TableName(1st) = %q", got)
	}
	if got := TableName(strings.Repeat("x", 100)); len(got) > 63 {
		t.Fatalf("TableName of a long name is %d bytes", len(got))
	}
	if got := TableName("recall_collections"); got == "recall_collections" {
		t.Fatal("a collection may not take the bookkeeping table's name")
	}
	if got := TableName("日本語"); !strings.HasPrefix(got, "c_") {
		t.Fatalf("TableName(日本語) = %q", got)
	}
}

func TestTranslateFilter(t *testing.T) {
	where, args, err := translateFilter(map[string]any{
		"subject":     "user",
		"retraction":  false,
		"confidence":  float64(1),
		"observed_ms": map[string]any{"$gte": float64(10), "$lte": float64(20)},
		"kind":        map[string]any{"$in": []any{"a", "b"}, "$ne": "c"},
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := " WHERE metadata @> $3::jsonb AND (metadata @> $4::jsonb OR metadata @> $5::jsonb) AND NOT (metadata @> $6::jsonb)" +
		" AND " + numberExpr("observed_ms") + " >= $7::float8 AND " + numberExpr("observed_ms") + " <= $8::float8" +
		" AND metadata @> $9::jsonb AND metadata @> $10::jsonb"
	if where != want {
		t.Fatalf("where:\n%s\nwant:\n%s", where, want)
	}
	wantArgs := []any{`{"confidence":1}`, `{"kind":"a"}`, `{"kind":"b"}`, `{"kind":"c"}`, float64(10), float64(20), `{"retraction":false}`, `{"subject":"user"}`}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args %#v, want %#v", args, wantArgs)
	}
	if w, a, err := translateFilter(nil, 1); err != nil || w != "" || a != nil {
		t.Fatalf("empty filter = %q, %v, %v", w, a, err)
	}
	for name, f := range map[string]map[string]any{
		"unknown operator": {"x": map[string]any{"$regex": "a"}},
		"bad field":        {"x'); DROP TABLE t; --": "a"},
		"range non-number": {"x": map[string]any{"$gt": "a"}},
		"empty in":         {"x": map[string]any{"$in": []any{}}},
		"object value":     {"x": []int{1}},
	} {
		if _, _, err := translateFilter(f, 1); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestTextQuery(t *testing.T) {
	if got := textQuery(`which "editor" does -the user prefer or not`); got != "which or editor or does or the or user or prefer or not" {
		t.Fatalf("textQuery = %q", got)
	}
	if got := textQuery("  "); got != "" {
		t.Fatalf("textQuery of blanks = %q", got)
	}
}

func TestVectorRoundTrip(t *testing.T) {
	in := []float32{1, 0.5, -0.25, 1e-7}
	s := formatVector(in)
	if s != "[1,0.5,-0.25,1e-07]" {
		t.Fatalf("formatVector = %s", s)
	}
	out, err := parseVector(s)
	if err != nil || !reflect.DeepEqual(out, in) {
		t.Fatalf("parseVector(%s) = %v, %v", s, out, err)
	}
	for _, bad := range []string{"", "1,2", "[1,x]"} {
		if _, err := parseVector(bad); err == nil {
			t.Errorf("parseVector(%q) accepted", bad)
		}
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("accepted an empty connection string")
	}
	if _, err := New(Config{URL: "postgres://localhost/db", Schema: "Bad Schema"}); err == nil {
		t.Fatal("accepted a schema that is not an identifier")
	}
	if _, err := New(Config{URL: "postgres://localhost/db", TextSearchConfig: "eng'lish"}); err == nil {
		t.Fatal("accepted a text search configuration that is not an identifier")
	}
	b, err := New(Config{URL: "postgres://nobody:nothing@localhost:1/none"})
	if err != nil {
		t.Fatalf("New made a request or rejected a well-formed URL: %v", err)
	}
	b.Close()
	if !atLeast("0.8.1", 0, 8) || atLeast("0.7.4", 0, 8) || !atLeast("1.0.0", 0, 8) || atLeast("x", 0, 8) {
		t.Fatal("atLeast")
	}
}
