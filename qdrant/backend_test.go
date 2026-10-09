package qdrant

import (
	"os"
	"reflect"
	"testing"

	"github.com/Polign/recall"
	"github.com/Polign/recall/backendtest"
)

// TestConformance runs the shared backend suite against the Qdrant named by
// RECALL_QDRANT_URL, such as http://localhost:6333, and skips without one.
func TestConformance(t *testing.T) {
	base := os.Getenv("RECALL_QDRANT_URL")
	if base == "" {
		t.Skip("set RECALL_QDRANT_URL to run against a Qdrant server")
	}
	backendtest.Run(t, func(t *testing.T) recall.Backend {
		b, err := New(Config{BaseURL: base, APIKey: os.Getenv("RECALL_QDRANT_API_KEY")})
		if err != nil {
			t.Fatal(err)
		}
		return b
	})
}

func TestPointIDIsStableUUID(t *testing.T) {
	a, b := PointID("m-0123"), PointID("m-0123")
	if a != b || PointID("m-0124") == a {
		t.Fatal("PointID must be deterministic and distinct")
	}
	if len(a) != 36 || a[14] != '5' {
		t.Fatalf("%s is not a UUIDv5", a)
	}
}

func TestTranslateFilter(t *testing.T) {
	got, err := translateFilter(map[string]any{
		"subject":     "user",
		"retraction":  false,
		"confidence":  float64(1),
		"observed_ms": map[string]any{"$gte": float64(10), "$lte": float64(20)},
	})
	if err != nil {
		t.Fatal(err)
	}
	must := got["must"].([]any)
	if len(must) != 4 {
		t.Fatalf("must = %v", must)
	}
	want := map[string]any{
		"subject":     map[string]any{"key": "subject", "match": map[string]any{"value": "user"}},
		"retraction":  map[string]any{"key": "retraction", "match": map[string]any{"value": false}},
		"confidence":  map[string]any{"key": "confidence", "range": map[string]any{"gte": float64(1), "lte": float64(1)}},
		"observed_ms": map[string]any{"key": "observed_ms", "range": map[string]any{"gte": float64(10), "lte": float64(20)}},
	}
	for _, c := range must {
		m := c.(map[string]any)
		if !reflect.DeepEqual(m, want[m["key"].(string)]) {
			t.Fatalf("condition %v, want %v", m, want[m["key"].(string)])
		}
	}
	if f, err := translateFilter(nil); err != nil || f != nil {
		t.Fatalf("empty filter = %v, %v", f, err)
	}
	if _, err := translateFilter(map[string]any{"x": map[string]any{"$regex": "a"}}); err == nil {
		t.Fatal("unknown operator accepted")
	}
	if _, err := translateFilter(map[string]any{IDField: "m-1"}); err == nil {
		t.Fatal("reserved field accepted")
	}
}

func TestNewValidates(t *testing.T) {
	for _, base := range []string{"", "localhost:6333", "http://u:p@h:6333", "http://h:6333?x=1"} {
		if _, err := New(Config{BaseURL: base}); err == nil {
			t.Fatalf("accepted %q", base)
		}
	}
	if _, err := New(Config{BaseURL: "http://h:6333", Distance: "Hamming"}); err == nil {
		t.Fatal("accepted an unknown distance")
	}
}
