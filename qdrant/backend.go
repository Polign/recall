// Package qdrant implements Recall's context-aware Backend over the Qdrant
// REST API. Each Recall collection is one Qdrant collection, created on the
// first write with the dimension of the first vector written to it.
//
// Qdrant point IDs must be unsigned integers or UUIDs, and Recall's event IDs
// are neither, so each event is stored under a UUIDv5 derived from its ID, and
// the ID itself rides in the payload under IDField. Listings come back in
// point ID order, which is stable, so a longer listing extends a shorter one
// while the data is unchanged, as Recall requires.
//
// Qdrant has no lease or revision API, so this backend implements neither
// recall.LeaseBackend (the agent resume tools need it) nor the watermark that
// enables Recall's materialized reads. Lexical search is not offered either;
// Recall ranks with vector search alone.
package qdrant

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // G505: UUIDv5 is defined over SHA-1; this is an identifier, not a signature
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Polign/recall"
)

// IDField is the payload key that holds a point's Recall event ID.
const IDField = "_recall_id"

// pageSize bounds one scroll request.
const pageSize = 1000

// Config selects a Qdrant endpoint and credential. The default HTTP client has
// a 30-second timeout and does not follow redirects. A supplied HTTPClient keeps
// its own timeout/redirect policy and must be safe for concurrent use.
type Config struct {
	// BaseURL is the REST endpoint, such as http://localhost:6333.
	BaseURL string
	// APIKey is sent as the api-key header when set.
	APIKey     string
	HTTPClient *http.Client
	// Distance is the metric a collection is created with: Cosine (the
	// default), Dot, Euclid, or Manhattan. Existing collections keep theirs.
	Distance string
	// MaxResponseBytes bounds each response; zero defaults to 64 MiB.
	MaxResponseBytes int64
}

// Backend is a reusable Qdrant client, safe for concurrent calls.
type Backend struct {
	baseURL, apiKey  string
	distance         string
	client           *http.Client
	maxResponseBytes int64

	// mu serializes collection creation, so concurrent first writes create a
	// collection once and wait for it rather than racing its activation.
	mu      sync.Mutex
	created map[string]bool
}

var (
	_ recall.Backend    = (*Backend)(nil)
	_ recall.GetBackend = (*Backend)(nil)
)

// New validates the endpoint and creates a backend without making requests.
func New(cfg Config) (*Backend, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("qdrant: base URL must be an HTTP(S) endpoint without credentials, query, or fragment")
	}
	if cfg.MaxResponseBytes < 0 || cfg.MaxResponseBytes == math.MaxInt64 {
		return nil, fmt.Errorf("qdrant: invalid response byte limit")
	}
	distance := cfg.Distance
	switch distance {
	case "":
		distance = "Cosine"
	case "Cosine", "Dot", "Euclid", "Manhattan":
	default:
		return nil, fmt.Errorf("qdrant: distance must be Cosine, Dot, Euclid, or Manhattan")
	}
	limit := cfg.MaxResponseBytes
	if limit == 0 {
		limit = 64 << 20
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Backend{baseURL: strings.TrimRight(cfg.BaseURL, "/"), apiKey: cfg.APIKey, distance: distance, client: client, maxResponseBytes: limit, created: map[string]bool{}}, nil
}

// StatusError reports an HTTP error without losing its machine-readable status.
type StatusError struct {
	StatusCode int
	Message    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("qdrant: HTTP %d: %s", e.StatusCode, e.Message)
}

// NotFound reports a 404, which Recall reads as "nothing written here yet".
func (e *StatusError) NotFound() bool { return e.StatusCode == http.StatusNotFound }

func missing(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.NotFound()
}

func collectionPath(collection string) (string, error) {
	if strings.TrimSpace(collection) == "" || collection == "." || collection == ".." {
		return "", fmt.Errorf("qdrant: collection is required and cannot be a dot path")
	}
	return "/collections/" + url.PathEscape(collection), nil
}

// PointID is the UUIDv5 a Recall event ID is stored under. The namespace is
// the RFC 4122 URL namespace, so the mapping is reproducible outside Recall.
func PointID(id string) string {
	ns := [16]byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	h := sha1.New() //nolint:gosec // see import
	h.Write(ns[:])
	h.Write([]byte("recall:" + id))
	s := h.Sum(nil)
	s[6] = s[6]&0x0f | 0x50
	s[8] = s[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", s[0:4], s[4:6], s[6:8], s[8:10], s[10:16])
}

// Put upserts one immutable event ID, creating the collection on first use.
// It waits for the write to be applied, so a following read sees it.
func (b *Backend) Put(ctx context.Context, collection, id string, values []float32, metadata map[string]any) error {
	path, err := collectionPath(collection)
	if err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("qdrant: event ID is required")
	}
	if len(values) == 0 {
		return fmt.Errorf("qdrant: refusing to store an event with no vector")
	}
	payload := make(map[string]any, len(metadata)+1)
	for k, v := range metadata {
		payload[k] = v
	}
	payload[IDField] = id
	body := map[string]any{"points": []map[string]any{{"id": PointID(id), "vector": values, "payload": payload}}}
	err = b.do(ctx, http.MethodPut, path+"/points?wait=true", body, nil)
	if !missing(err) {
		return err
	}
	if err := b.ensureCollection(ctx, path, len(values)); err != nil {
		return err
	}
	// Another process may have created the collection a moment ago, and Qdrant
	// refuses writes until its shards are active. The ID is immutable, so
	// repeating the upsert is safe.
	for attempt := 0; ; attempt++ {
		err = b.do(ctx, http.MethodPut, path+"/points?wait=true", body, nil)
		var se *StatusError
		if attempt == 4 || !errors.As(err, &se) || (se.StatusCode < 500 && !se.NotFound()) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(100<<attempt) * time.Millisecond):
		}
	}
}

// ensureCollection creates a collection unless it exists already.
func (b *Backend) ensureCollection(ctx context.Context, path string, dim int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.created[path] {
		return nil
	}
	err := b.do(ctx, http.MethodGet, path, nil, nil)
	if missing(err) {
		err = b.createCollection(ctx, path, dim)
	}
	if err != nil {
		return err
	}
	b.created[path] = true
	return nil
}

// indexed are the payload fields Recall filters on, indexed when a collection
// is created. Qdrant filters unindexed fields too, but scans to do it, and a
// deployment in strict mode refuses such filters outright.
var indexed = map[string]string{
	"subject":      "keyword",
	"predicate":    "keyword",
	"kind":         "keyword",
	"observed_ms":  "float",
	"recall_agent": "keyword",
	"record":       "keyword",
}

func (b *Backend) createCollection(ctx context.Context, path string, dim int) error {
	err := b.do(ctx, http.MethodPut, path, map[string]any{"vectors": map[string]any{"size": dim, "distance": b.distance}}, nil)
	var se *StatusError
	// A concurrent first write may have created it already.
	if errors.As(err, &se) && (se.StatusCode == http.StatusConflict || se.StatusCode == http.StatusBadRequest && strings.Contains(se.Message, "already exists")) {
		return nil
	}
	if err != nil {
		return err
	}
	for field, schema := range indexed {
		if err := b.do(ctx, http.MethodPut, path+"/index?wait=true", map[string]any{"field_name": field, "field_schema": schema}, nil); err != nil {
			return fmt.Errorf("qdrant: index %s: %w", field, err)
		}
	}
	return nil
}

type point struct {
	ID      any            `json:"id"`
	Score   float32        `json:"score"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload"`
}

// event splits a point into its Recall ID and metadata.
func (p point) event() (string, map[string]any, error) {
	id, _ := p.Payload[IDField].(string)
	if id == "" {
		return "", nil, fmt.Errorf("qdrant: point %v has no %s; the collection holds data Recall did not write", p.ID, IDField)
	}
	md := make(map[string]any, len(p.Payload))
	for k, v := range p.Payload {
		if k != IDField {
			md[k] = v
		}
	}
	return id, md, nil
}

// List returns up to limit exact matches and the exact total, paging
// internally. The total is counted before and after the pages are read, and a
// change between the two is reported as an error rather than a partial history.
func (b *Backend) List(ctx context.Context, collection string, filter map[string]any, limit int) ([]recall.StoredVector, int, error) {
	path, err := collectionPath(collection)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > recall.MaxHistoryEvents+1 {
		return nil, 0, fmt.Errorf("qdrant: list limit must be in [1, %d]", recall.MaxHistoryEvents+1)
	}
	qf, err := translateFilter(filter)
	if err != nil {
		return nil, 0, err
	}
	total, err := b.count(ctx, path, qf)
	if missing(err) {
		return []recall.StoredVector{}, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	want := min(limit, total)
	out := make([]recall.StoredVector, 0, want)
	var offset any
	for len(out) < want {
		body := map[string]any{"limit": min(want-len(out), pageSize), "with_payload": true, "with_vector": true}
		if qf != nil {
			body["filter"] = qf
		}
		if offset != nil {
			body["offset"] = offset
		}
		var page struct {
			Points []point `json:"points"`
			Next   any     `json:"next_page_offset"`
		}
		if err := b.do(ctx, http.MethodPost, path+"/points/scroll", body, &page); err != nil {
			return nil, 0, err
		}
		if len(page.Points) > want-len(out) {
			return nil, 0, fmt.Errorf("qdrant: listing exceeded its requested bound; retry")
		}
		for _, p := range page.Points {
			id, md, err := p.event()
			if err != nil {
				return nil, 0, err
			}
			out = append(out, recall.StoredVector{ID: id, Values: p.Vector, Metadata: md})
		}
		if len(out) < want && (page.Next == nil || len(page.Points) == 0) {
			return nil, 0, fmt.Errorf("qdrant: listing ended before its counted total; retry")
		}
		offset = page.Next
	}
	again, err := b.count(ctx, path, qf)
	if err != nil {
		return nil, 0, err
	}
	if again != total {
		return nil, 0, fmt.Errorf("qdrant: listing changed while it was read; retry")
	}
	return out, total, nil
}

func (b *Backend) count(ctx context.Context, path string, qf map[string]any) (int, error) {
	body := map[string]any{"exact": true}
	if qf != nil {
		body["filter"] = qf
	}
	var result struct {
		Count *int `json:"count"`
	}
	if err := b.do(ctx, http.MethodPost, path+"/points/count", body, &result); err != nil {
		return 0, err
	}
	if result.Count == nil || *result.Count < 0 {
		return 0, fmt.Errorf("qdrant: count omitted a valid total")
	}
	return *result.Count, nil
}

// Search retrieves semantic candidates using the supplied query vector. Score
// is Qdrant's similarity; Distance is 1 - Score for the Cosine and Dot metrics
// and the score itself for Euclid and Manhattan, whose scores are distances.
func (b *Backend) Search(ctx context.Context, collection string, values []float32, k int, filter map[string]any) ([]recall.Hit, error) {
	path, err := collectionPath(collection)
	if err != nil {
		return nil, err
	}
	if k <= 0 || k > 10000 {
		return nil, fmt.Errorf("qdrant: search limit must be in [1, 10000]")
	}
	qf, err := translateFilter(filter)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"query": values, "limit": k, "with_payload": true}
	if qf != nil {
		body["filter"] = qf
	}
	var result struct {
		Points []point `json:"points"`
	}
	if err := b.do(ctx, http.MethodPost, path+"/points/query", body, &result); err != nil {
		if missing(err) {
			return []recall.Hit{}, nil
		}
		return nil, err
	}
	if len(result.Points) > k {
		return nil, fmt.Errorf("qdrant: invalid search response")
	}
	hits := make([]recall.Hit, 0, len(result.Points))
	for _, p := range result.Points {
		id, md, err := p.event()
		if err != nil {
			return nil, err
		}
		dist := p.Score
		if b.distance == "Cosine" || b.distance == "Dot" {
			dist = 1 - p.Score
		}
		hits = append(hits, recall.Hit{ID: id, Score: p.Score, Distance: dist, Metadata: md})
	}
	return hits, nil
}

// Get reads events by id in request order; unknown ids are omitted. It lets
// Recall follow a belief's EvidenceID.
func (b *Backend) Get(ctx context.Context, collection string, ids []string) ([]recall.StoredVector, error) {
	path, err := collectionPath(collection)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 || len(ids) > recall.MaxGetEvents {
		return nil, fmt.Errorf("qdrant: get takes 1 to %d ids", recall.MaxGetEvents)
	}
	pids := make([]string, len(ids))
	for i, id := range ids {
		pids[i] = PointID(id)
	}
	var points []point
	if err := b.do(ctx, http.MethodPost, path+"/points", map[string]any{"ids": pids, "with_payload": true, "with_vector": true}, &points); err != nil {
		if missing(err) {
			return []recall.StoredVector{}, nil
		}
		return nil, err
	}
	byID := make(map[string]recall.StoredVector, len(points))
	for _, p := range points {
		id, md, err := p.event()
		if err != nil {
			return nil, err
		}
		byID[id] = recall.StoredVector{ID: id, Values: p.Vector, Metadata: md}
	}
	out := make([]recall.StoredVector, 0, len(points))
	for _, id := range ids {
		if v, ok := byID[id]; ok {
			out = append(out, v)
			delete(byID, id)
		}
	}
	return out, nil
}

// translateFilter turns Recall's filter language into a Qdrant filter. Recall
// filters are a conjunction of fields, each either a scalar that must match
// exactly or an object of comparison operators ($eq, $ne, $gt, $gte, $lt,
// $lte, $in, $nin). A nil result means no filter.
func translateFilter(filter map[string]any) (map[string]any, error) {
	var must, mustNot []any
	for key, v := range filter {
		if key == IDField {
			return nil, fmt.Errorf("qdrant: %s is reserved", IDField)
		}
		ops, isOps := v.(map[string]any)
		if !isOps {
			c, err := equal(key, v)
			if err != nil {
				return nil, err
			}
			must = append(must, c)
			continue
		}
		rng := map[string]any{}
		for op, arg := range ops {
			switch op {
			case "$eq":
				c, err := equal(key, arg)
				if err != nil {
					return nil, err
				}
				must = append(must, c)
			case "$ne":
				c, err := equal(key, arg)
				if err != nil {
					return nil, err
				}
				mustNot = append(mustNot, c)
			case "$gt", "$gte", "$lt", "$lte":
				n, ok := number(arg)
				if !ok {
					return nil, fmt.Errorf("qdrant: filter %s %s needs a number", key, op)
				}
				rng[op[1:]] = n
			case "$in", "$nin":
				list, ok := arg.([]any)
				if !ok || len(list) == 0 {
					return nil, fmt.Errorf("qdrant: filter %s %s needs a non-empty list", key, op)
				}
				var alts []any
				for _, x := range list {
					c, err := equal(key, x)
					if err != nil {
						return nil, err
					}
					alts = append(alts, c)
				}
				if op == "$in" {
					must = append(must, map[string]any{"should": alts})
				} else {
					mustNot = append(mustNot, alts...)
				}
			default:
				return nil, fmt.Errorf("qdrant: unsupported filter operator %s on %s", op, key)
			}
		}
		if len(rng) > 0 {
			must = append(must, map[string]any{"key": key, "range": rng})
		}
	}
	if len(must) == 0 && len(mustNot) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	if len(must) > 0 {
		out["must"] = must
	}
	if len(mustNot) > 0 {
		out["must_not"] = mustNot
	}
	return out, nil
}

// equal is one field-equals-value condition. Qdrant matches strings and
// booleans exactly but numbers only as integers, so a number is matched as a
// closed range, which compares integers and floats alike.
func equal(key string, v any) (map[string]any, error) {
	if n, ok := number(v); ok {
		return map[string]any{"key": key, "range": map[string]any{"gte": n, "lte": n}}, nil
	}
	switch v.(type) {
	case string, bool:
		return map[string]any{"key": key, "match": map[string]any{"value": v}}, nil
	}
	return nil, fmt.Errorf("qdrant: filter %s needs a string, number, or bool, not %T", key, v)
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func (b *Backend) do(ctx context.Context, method, path string, body, out any) error {
	if ctx == nil {
		return fmt.Errorf("qdrant: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.apiKey != "" {
		req.Header.Set("api-key", b.apiKey)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("qdrant: request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, b.maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("qdrant: response: %w", err)
	}
	if int64(len(raw)) > b.maxResponseBytes {
		return fmt.Errorf("qdrant: response exceeds %d bytes", b.maxResponseBytes)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Status json.RawMessage `json:"status"`
	}
	_ = json.Unmarshal(raw, &envelope)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var detail struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(envelope.Status, &detail)
		message := detail.Error
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		if len(message) > 4096 {
			message = message[:4096]
		}
		return &StatusError{StatusCode: resp.StatusCode, Message: message}
	}
	if out != nil {
		if len(envelope.Result) == 0 {
			return fmt.Errorf("qdrant: response has no result")
		}
		if err := json.Unmarshal(envelope.Result, out); err != nil {
			return fmt.Errorf("qdrant: decode response: %w", err)
		}
	}
	return nil
}
