package polign_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Polign/recall"
	"github.com/Polign/recall/polign"
)

func newBackend(t *testing.T, h http.HandlerFunc) *polign.Backend {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	b, err := polign.New(polign.Config{BaseURL: ts.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestListPagesWithExactTotalsAndTypedMetadata(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/proxy/v1/collections/memories/vectors" || r.URL.Query().Get("typed") != "true" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("request = %s, headers %v", r.URL, r.Header)
		}
		var f map[string]any
		if err := json.Unmarshal([]byte(r.URL.Query().Get("filter")), &f); err != nil || f["subject"] != "user & me" {
			t.Errorf("filter = %v, %v", f, err)
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		var rows []recall.StoredVector
		for i := offset; i < min(5, offset+min(limit, 2)); i++ {
			rows = append(rows, recall.StoredVector{ID: fmt.Sprintf("e-%d", i), Values: []float32{1}, Metadata: map[string]any{"value": false, "confidence": 0.5}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"vectors": rows, "total": 5})
	}))
	defer ts.Close()
	b, err := polign.New(polign.Config{BaseURL: ts.URL + "/proxy/", APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	rows, total, err := b.List(t.Context(), "memories", map[string]any{"subject": "user & me"}, 5)
	if err != nil || total != 5 || len(rows) != 5 || calls.Load() != 3 {
		t.Fatalf("list length %d total %d calls %d err %v", len(rows), total, calls.Load(), err)
	}
	if rows[0].Metadata["value"] != false || rows[0].Metadata["confidence"] != 0.5 {
		t.Fatalf("metadata lost types: %+v", rows[0])
	}
	rows, total, err = b.List(t.Context(), "memories", map[string]any{"subject": "user & me"}, 3)
	if err != nil || len(rows) != 3 || total != 5 {
		t.Fatalf("bounded list length %d total %d err %v", len(rows), total, err)
	}
}

func TestPutAndSearchWireContract(t *testing.T) {
	b := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing authentication or content type")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.Method {
		case http.MethodPut:
			if r.URL.EscapedPath() != "/v1/collections/space%20collection/vectors/a%2Fb%20%3F" {
				t.Errorf("escaped path = %s", r.URL.EscapedPath())
			}
			md := body["metadata"].(map[string]any)
			if md["value"] != false || md["confidence"] != 0.5 {
				t.Errorf("put metadata = %v", md)
			}
			w.WriteHeader(http.StatusCreated)
		case http.MethodPost:
			if r.URL.Path != "/v1/collections/space collection/query" || body["typed_metadata"] != true || body["k"] != float64(1) {
				t.Errorf("search = %s %+v", r.URL, body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"hits": []any{map[string]any{"id": "a/b ?", "distance": 0.1, "metadata": map[string]any{"value": false}}}})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	})
	if err := b.Put(t.Context(), "space collection", "a/b ?", []float32{1}, map[string]any{"value": false, "confidence": 0.5}); err != nil {
		t.Fatal(err)
	}
	hits, err := b.Search(t.Context(), "space collection", []float32{1}, 1, map[string]any{"subject": "user"})
	if err != nil || len(hits) != 1 || hits[0].Metadata["value"] != false {
		t.Fatalf("search = %+v, %v", hits, err)
	}
}

func TestListRejectsInconsistentOrMalformedPages(t *testing.T) {
	for _, mode := range []string{"changed_total", "duplicate", "reordered", "empty_page", "missing_total", "null_vectors", "bad_json", "too_many", "empty_id"} {
		t.Run(mode, func(t *testing.T) {
			b := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
				offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
				ids := []string{"a", "b"}
				total := 3
				if offset > 0 {
					ids = []string{"c"}
				}
				switch mode {
				case "changed_total":
					if offset > 0 {
						total = 4
					}
				case "duplicate":
					if offset > 0 {
						ids = []string{"b"}
					}
				case "reordered":
					ids = []string{"b", "a"}
				case "empty_page":
					if offset > 0 {
						ids = []string{}
					}
				case "missing_total":
					_, _ = w.Write([]byte(`{"vectors":[]}`))
					return
				case "null_vectors":
					_, _ = w.Write([]byte(`{"vectors":null,"total":0}`))
					return
				case "bad_json":
					_, _ = w.Write([]byte(`no json`))
					return
				case "too_many":
					ids = []string{"a", "b", "c", "d"}
				case "empty_id":
					ids = []string{""}
				}
				rows := make([]map[string]any, 0, len(ids))
				for _, id := range ids {
					rows = append(rows, map[string]any{"id": id})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"vectors": rows, "total": total})
			})
			if rows, total, err := b.List(t.Context(), "memories", nil, 3); err == nil || rows != nil || total != 0 {
				t.Fatalf("partial listing escaped: %v, %d, %v", rows, total, err)
			}
		})
	}
}

func TestCancellationDuringLaterPageReturnsNoPartialHistory(t *testing.T) {
	entered := make(chan struct{})
	finished := make(chan struct{})
	b := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") == "0" {
			_, _ = w.Write([]byte(`{"vectors":[{"id":"a"}],"total":2}`))
			return
		}
		close(entered)
		<-r.Context().Done()
		close(finished)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		rows, total, err := b.List(ctx, "memories", nil, 2)
		if rows != nil || total != 0 {
			t.Error("cancelled scan returned partial data")
		}
		done <- err
	}()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	<-finished
}

func TestStatusErrorsAndResponseBounds(t *testing.T) {
	for _, code := range []int{401, 403, 429, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			b := newBackend(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"error":"backend unavailable"}`))
			})
			_, _, err := b.List(t.Context(), "memories", nil, 1)
			var status *polign.StatusError
			if !errors.As(err, &status) || status.StatusCode != code || status.Message != "backend unavailable" {
				t.Fatalf("status error = %v", err)
			}
		})
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(strings.Repeat("x", 65))) }))
	defer ts.Close()
	b, err := polign.New(polign.Config{BaseURL: ts.URL, MaxResponseBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.List(t.Context(), "memories", nil, 1); err == nil || !strings.Contains(err.Error(), "exceeds 64") {
		t.Fatalf("response bound = %v", err)
	}
}

func TestDefaultClientDoesNotFollowRedirects(t *testing.T) {
	var forwarded atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded.Add(1)
		_, _ = w.Write([]byte(`{"vectors":[],"total":0}`))
	}))
	defer sink.Close()
	b := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	})
	_, _, err := b.List(t.Context(), "memories", nil, 1)
	var status *polign.StatusError
	if !errors.As(err, &status) || status.StatusCode != 307 || forwarded.Load() != 0 {
		t.Fatalf("redirect result = %v, forwarded %d", err, forwarded.Load())
	}
}

func TestInvalidConfigurationAndLimits(t *testing.T) {
	for _, url := range []string{"", "://bad", "file:///tmp/db", "http://", "http://u:p@example.com", "http://example.com?q=1", "http://example.com#frag"} {
		if _, err := polign.New(polign.Config{BaseURL: url}); err == nil {
			t.Errorf("accepted URL %q", url)
		}
	}
	b := newBackend(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached server") })
	for _, limit := range []int{0, -1, recall.MaxHistoryEvents + 2} {
		if _, _, err := b.List(t.Context(), "memories", nil, limit); err == nil {
			t.Errorf("accepted limit %d", limit)
		}
	}
	if err := b.Put(t.Context(), "memories", "..", []float32{1}, nil); err == nil {
		t.Error("dot ID accepted")
	}
	if _, _, err := b.List(t.Context(), "..", nil, 1); err == nil {
		t.Error("dot collection accepted")
	}
	if _, err := b.Search(t.Context(), "memories", []float32{1}, 0, nil); err == nil {
		t.Error("zero search limit accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := b.List(ctx, "memories", nil, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled list = %v", err)
	}
	if _, _, err := b.List(nil, "memories", nil, 1); err == nil {
		t.Error("nil context accepted")
	}
}
