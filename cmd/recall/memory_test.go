package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Polign/recall"
)

// memoryHTTP exercises the production adapter with typed metadata and a
// deliberately small page cap, so a complete history requires several calls.
func memoryHTTP(t *testing.T, db *fakeMemoryDB, pageCap int) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Get("typed") != "true" {
				t.Error("listing omitted typed metadata")
			}
			var filter map[string]any
			if err := json.Unmarshal([]byte(r.URL.Query().Get("filter")), &filter); err != nil {
				t.Error(err)
			}
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
			rows := db.matching(filter, 0)
			total := len(rows)
			start := min(offset, total)
			end := min(start+min(limit, pageCap), total)
			vectors := make([]map[string]any, 0, end-start)
			for _, row := range rows[start:end] {
				vectors = append(vectors, map[string]any{"id": row.ID, "metadata": row.Metadata})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"vectors": vectors, "total": total})
		case http.MethodPut:
			var v struct {
				Values   []float32
				Metadata map[string]any
			}
			if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
				t.Error(err)
			}
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if err := db.Put("memories", id, v.Values, v.Metadata); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		default:
			t.Errorf("unexpected HTTP method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestMemoryAdapterPagesCompleteHistory(t *testing.T) {
	db := newFakeMemoryDB()
	ts := memoryHTTP(t, db, 2)
	source := newMemoryMCP(t).memory.store
	// Use its registry, with deterministic event times supplied directly.
	for i := 0; i < 7; i++ {
		metadata := map[string]any{"subject": "user", "predicate": "prefers_editor", "value": fmt.Sprintf("v%d", i), "kind": "fact", "confidence": 1.0, "source": "user_stated", "observed_at": fmt.Sprintf("2026-01-01T00:00:0%dZ", i)}
		if err := db.Put("memories", fmt.Sprint(i), []float32{1, 0, 0}, metadata); err != nil {
			t.Fatal(err)
		}
	}
	adapter := &memoryDB{api: &api{base: ts.URL}, ctx: t.Context()}
	store := recall.NewStore(adapter, "memories", source.Registry(), func(string) []float32 { return []float32{1, 0, 0} })
	beliefs, err := store.Recall(recall.Query{Subject: "user", Predicate: "prefers_editor"})
	if err != nil || len(beliefs) != 1 || beliefs[0].Value != "v6" {
		t.Fatalf("beliefs = %v, %v", beliefs, err)
	}
	history, err := store.History("user", "prefers_editor")
	if err != nil || len(history) != 7 {
		t.Fatalf("history length = %d, %v", len(history), err)
	}
}

func TestMemoryAdapterRefusesInconsistentPages(t *testing.T) {
	for _, mode := range []string{"total_changed", "duplicate", "empty"} {
		t.Run(mode, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
				total := 3
				rows := []map[string]any{{"id": fmt.Sprint(offset)}}
				if offset > 0 {
					switch mode {
					case "total_changed":
						total = 4
					case "duplicate":
						rows[0]["id"] = "0"
					case "empty":
						rows = nil
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"vectors": rows, "total": total})
			}))
			defer ts.Close()
			db := &memoryDB{api: &api{base: ts.URL}, ctx: t.Context()}
			if _, _, err := db.List("memories", nil, 10); err == nil {
				t.Fatal("inconsistent pages accepted")
			}
		})
	}
}

func TestMCPMemoryReadOnly(t *testing.T) {
	s := newMemoryMCP(t)
	s.write = false
	for _, tool := range s.tools() {
		if tool.Name == "remember" || tool.Name == "forget" {
			t.Fatalf("read-only server advertised %s", tool.Name)
		}
	}
	for _, name := range []string{"remember", "forget"} {
		_, err := s.runTool(t.Context(), name, json.RawMessage(`{"subject":"user","predicate":"likes","value":"go"}`))
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("%s error = %v", name, err)
		}
	}
	for _, name := range []string{"list_predicates", "recall", "memory_history"} {
		_, err := s.runTool(t.Context(), name, json.RawMessage(`{"subject":"user","predicate":"likes"}`))
		if err != nil {
			t.Fatalf("read-only %s: %v", name, err)
		}
	}
}

func TestConcurrentMCPMemoryKeepsEmbeddingErrorsPerRequest(t *testing.T) {
	db := newFakeMemoryDB()
	ts := memoryHTTP(t, db, 1000)
	embedder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Text string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if strings.Contains(in.Text, "fail") {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"embedding unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"values":[1,0,0]}`))
	}))
	defer embedder.Close()
	predicates := filepath.Join(t.TempDir(), "predicates.json")
	if err := os.WriteFile(predicates, []byte(`{"likes":{"cardinality":"multi","value_type":"string"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	apiClient := &api{base: ts.URL}
	memory, err := newMemoryRuntime(apiClient, "memories", predicates, embedder.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpServer{api: apiClient, collection: "memories", write: true, memory: memory}
	requests := make([]string, 0, 32)
	for i := 1; i <= 32; i++ {
		value := "ok"
		if i%2 == 0 {
			value = "fail"
		}
		requests = append(requests, call(i, "remember", fmt.Sprintf(`{"subject":"user%d","predicate":"likes","value":%q}`, i, value)))
	}
	responses := runMCP(t, s, requests...)
	for i := 1; i <= 32; i++ {
		text, isError := toolText(t, responses[float64(i)])
		if isError != (i%2 == 0) {
			t.Errorf("request %d: error=%v, %s", i, isError, text)
		}
	}
	if total := len(db.matching(nil, 0)); total != 16 {
		t.Fatalf("stored %d memories, want 16", total)
	}
}

func TestRemoteEmbedderHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	embedder := newRemoteEmbedder("http://127.0.0.1:1", 0)
	if _, err := embedder.Embed(ctx, "text"); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("error = %v", err)
	}
}
