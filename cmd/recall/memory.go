package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Polign/recall"
)

// This file adapts the CLI's HTTP client to the memory layer. recall owns the
// semantics (typed predicates, deterministic supersession, belief history);
// everything here is transport.

// remoteEmbedder speaks the small HTTP contract polign_demo's BGE sidecar
// serves: POST {"text": ...} and read back {"values": [...]}. The sidecar owns
// any model-specific query prefix, so callers send raw text.
//
// This optional adapter uses the model that produced the collection's vectors.
// Memory defaults to Recall's built-in lexical embedder when no URL is set.
type remoteEmbedder struct {
	url    string
	dim    int
	client *http.Client
}

func newRemoteEmbedder(addr string, dim int) *remoteEmbedder {
	return &remoteEmbedder{
		url:    strings.TrimRight(addr, "/") + "/embed",
		dim:    dim,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (r *remoteEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	payload, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(payload)) // #nosec G704 -- The operator selects the endpoint at startup; tool calls cannot choose it.
	if err != nil {
		return nil, fmt.Errorf("embedder: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req) // #nosec G704 -- The operator configures this embedding endpoint at startup; tool arguments supply only the text.
	if err != nil {
		return nil, fmt.Errorf("embedder: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Values []float32 `json:"values"`
		Error  string    `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embedder: decode status %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedder: status %d: %s", resp.StatusCode, out.Error)
	}
	if r.dim > 0 && len(out.Values) != r.dim {
		return nil, fmt.Errorf("embedder returned dimension %d, collection requires %d", len(out.Values), r.dim)
	}
	return out.Values, nil
}

// memoryDB adapts the CLI's api to recall.VectorDB.
//
// Every read asks for typed metadata. recall stores numbers and booleans as
// themselves so that a confidence floor or a value range compares numerically,
// and the CLI's other commands decode metadata as strings, which would turn
// every number back into text on the way in.
type memoryDB struct {
	api *api
	ctx context.Context
	// embedErr records a failure from the embedding function, which recall's
	// signature cannot return. The store reports a zero vector as some other
	// error, so the real cause is kept here and surfaced instead.
	embedErr error
}

func (m *memoryDB) Put(collection, id string, values []float32, metadata map[string]any) error {
	if m.embedErr != nil {
		return m.embedErr
	}
	if len(values) == 0 {
		return fmt.Errorf("refusing to store a memory with no vector")
	}
	body := map[string]any{"values": values, "metadata": metadata}
	path := "/v1/collections/" + url.PathEscape(collection) + "/vectors/" + url.PathEscape(id)
	return m.api.doCtx(m.ctx, http.MethodPut, path, body, nil)
}

// List honors Recall's requested bound across the server's capped pages.
// A changing total or repeated ID means the pages do not describe a complete
// stable log, so the caller must retry rather than fold a partial result.
func (m *memoryDB) List(collection string, filter map[string]any, limit int) ([]recall.StoredVector, int, error) {
	if limit <= 0 {
		return nil, 0, fmt.Errorf("memory listing requires a positive limit")
	}
	var out []recall.StoredVector
	seen := make(map[string]bool)
	total := -1
	for len(out) < limit {
		page, count, err := m.listPage(collection, filter, limit-len(out), len(out))
		if err != nil {
			return nil, 0, err
		}
		if total < 0 {
			total = count
		}
		if count != total || count < len(out)+len(page) {
			return nil, 0, fmt.Errorf("memory history changed while paging; retry")
		}
		for _, v := range page {
			if seen[v.ID] {
				return nil, 0, fmt.Errorf("memory history repeated an event while paging; retry")
			}
			seen[v.ID] = true
			out = append(out, v)
		}
		if len(out) >= total {
			break
		}
		if len(page) == 0 {
			return nil, 0, fmt.Errorf("memory history ended before its reported total; retry")
		}
	}
	return out, total, nil
}

func (m *memoryDB) listPage(collection string, filter map[string]any, limit, offset int) ([]recall.StoredVector, int, error) {
	q := url.Values{}
	q.Set("typed", "true")
	q.Set("offset", strconv.Itoa(offset))
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if len(filter) > 0 {
		raw, err := json.Marshal(filter)
		if err != nil {
			return nil, 0, err
		}
		q.Set("filter", string(raw))
	}
	path := "/v1/collections/" + url.PathEscape(collection) + "/vectors?" + q.Encode()
	var out struct {
		Vectors []struct {
			ID       string         `json:"id"`
			Metadata map[string]any `json:"metadata"`
		} `json:"vectors"`
		Total int `json:"total"`
	}
	if err := m.api.doCtx(m.ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, 0, err
	}
	vs := make([]recall.StoredVector, 0, len(out.Vectors))
	for _, v := range out.Vectors {
		vs = append(vs, recall.StoredVector{ID: v.ID, Metadata: v.Metadata})
	}
	return vs, out.Total, nil
}

func (m *memoryDB) Search(collection string, values []float32, k int, filter map[string]any) ([]recall.Hit, error) {
	if m.embedErr != nil {
		return nil, m.embedErr
	}
	body := map[string]any{"k": k, "typed_metadata": true}
	if len(values) > 0 {
		body["values"] = values
	}
	if len(filter) > 0 {
		body["filter"] = filter
	}
	path := "/v1/collections/" + url.PathEscape(collection) + "/query"
	var out struct {
		Hits []struct {
			ID       string         `json:"id"`
			Distance float32        `json:"distance"`
			Score    float32        `json:"score"`
			Metadata map[string]any `json:"metadata"`
		} `json:"hits"`
	}
	if err := m.api.doCtx(m.ctx, http.MethodPost, path, body, &out); err != nil {
		return nil, err
	}
	hits := make([]recall.Hit, 0, len(out.Hits))
	for _, h := range out.Hits {
		hits = append(hits, recall.Hit{ID: h.ID, Distance: h.Distance, Score: h.Score, Metadata: h.Metadata})
	}
	return hits, nil
}
