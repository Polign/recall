// Package polign implements Recall's context-aware Backend over the Polign HTTP
// API. It uses typed metadata and exact, internally paginated vector listings.
package polign

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Polign/recall"
)

// Config selects a Polign endpoint and credential. The default HTTP client has
// a 30-second timeout and does not follow redirects. A supplied HTTPClient keeps
// its own timeout/redirect policy and must be safe for concurrent use.
type Config struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	// MaxResponseBytes bounds each response; zero defaults to 64 MiB. Large
	// vector dimensions may require a larger bound even for metadata-only uses.
	MaxResponseBytes int64
}

// Backend is a reusable Polign client, safe for concurrent calls.
type Backend struct {
	baseURL, apiKey  string
	client           *http.Client
	maxResponseBytes int64
}

var (
	_ recall.Backend      = (*Backend)(nil)
	_ recall.LeaseBackend = (*Backend)(nil)
)

// New validates the endpoint and creates a backend without making requests.
func New(cfg Config) (*Backend, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("polign: base URL must be an HTTP(S) endpoint without credentials, query, or fragment")
	}
	if cfg.MaxResponseBytes < 0 || cfg.MaxResponseBytes == math.MaxInt64 {
		return nil, fmt.Errorf("polign: invalid response byte limit")
	}
	limit := cfg.MaxResponseBytes
	if limit == 0 {
		limit = 64 << 20
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Backend{baseURL: strings.TrimRight(cfg.BaseURL, "/"), apiKey: cfg.APIKey, client: client, maxResponseBytes: limit}, nil
}

// StatusError reports an HTTP error without losing its machine-readable status.
type StatusError struct {
	StatusCode int
	Message    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("polign: HTTP %d: %s", e.StatusCode, e.Message)
}

// NotFound reports a 404, which Recall reads as "nothing written here yet"
// when an agent lists a collection it has not written to.
func (e *StatusError) NotFound() bool { return e.StatusCode == http.StatusNotFound }

func collectionPath(collection string) (string, error) {
	if strings.TrimSpace(collection) == "" || collection == "." || collection == ".." {
		return "", fmt.Errorf("polign: collection is required and cannot be a dot path")
	}
	return "/v1/collections/" + url.PathEscape(collection), nil
}

// Put upserts one immutable event ID. It does not automatically retry writes.
func (b *Backend) Put(ctx context.Context, collection, id string, values []float32, metadata map[string]any) error {
	path, err := collectionPath(collection)
	if err != nil {
		return err
	}
	if strings.TrimSpace(id) == "" || id == "." || id == ".." {
		return fmt.Errorf("polign: event ID is required and cannot be a dot path")
	}
	return b.do(ctx, http.MethodPut, path+"/vectors/"+url.PathEscape(id), map[string]any{"values": values, "metadata": metadata}, nil)
}

// List returns up to limit exact matches and the exact total, paging internally.
// Limits up to MaxHistoryEvents+1 support every bounded Recall read, including
// its overflow probe. Changing totals, duplicate/reordered IDs, or incomplete
// pages return errors without exposing a partial history.
func (b *Backend) List(ctx context.Context, collection string, filter map[string]any, limit int) ([]recall.StoredVector, int, error) {
	path, err := collectionPath(collection)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > recall.MaxHistoryEvents+1 {
		return nil, 0, fmt.Errorf("polign: list limit must be in [1, %d]", recall.MaxHistoryEvents+1)
	}
	filterJSON, err := json.Marshal(filter)
	if err != nil {
		return nil, 0, err
	}
	total := -1
	var out []recall.StoredVector
	for len(out) < limit {
		q := url.Values{"typed": {"true"}, "limit": {strconv.Itoa(min(limit-len(out), 1000))}, "offset": {strconv.Itoa(len(out))}}
		if len(filter) > 0 {
			q.Set("filter", string(filterJSON))
		}
		var page struct {
			Vectors *[]recall.StoredVector `json:"vectors"`
			Total   *int                   `json:"total"`
		}
		if err := b.do(ctx, http.MethodGet, path+"/vectors?"+q.Encode(), nil, &page); err != nil {
			return nil, 0, err
		}
		if page.Total == nil || page.Vectors == nil || *page.Total < 0 {
			return nil, 0, fmt.Errorf("polign: listing omitted valid vectors or total")
		}
		if total < 0 {
			total = *page.Total
		}
		rows := *page.Vectors
		if *page.Total != total || len(rows) > min(limit-len(out), 1000) || len(rows) > total-len(out) {
			return nil, 0, fmt.Errorf("polign: listing changed or exceeded its reported bounds; retry")
		}
		for _, row := range rows {
			if row.ID == "" || len(out) > 0 && row.ID <= out[len(out)-1].ID {
				return nil, 0, fmt.Errorf("polign: listing repeated or reordered an event; retry")
			}
			out = append(out, row)
		}
		if len(out) == total {
			break
		}
		if len(rows) == 0 {
			return nil, 0, fmt.Errorf("polign: listing ended before its reported total; retry")
		}
	}
	return out, total, nil
}

// Search retrieves semantic candidates using the supplied query vector.
func (b *Backend) Search(ctx context.Context, collection string, values []float32, k int, filter map[string]any) ([]recall.Hit, error) {
	path, err := collectionPath(collection)
	if err != nil {
		return nil, err
	}
	if k <= 0 || k > 10000 {
		return nil, fmt.Errorf("polign: search limit must be in [1, 10000]")
	}
	var result struct {
		Hits *[]recall.Hit `json:"hits"`
	}
	if err := b.do(ctx, http.MethodPost, path+"/query", map[string]any{"values": values, "k": k, "filter": filter, "typed_metadata": true}, &result); err != nil {
		return nil, err
	}
	if result.Hits == nil || len(*result.Hits) > k {
		return nil, fmt.Errorf("polign: invalid search response")
	}
	return *result.Hits, nil
}

// SearchText runs Polign's BM25 search over each event's text field. Polign
// indexes text as it persists segments, so a collection with nothing persisted
// yet, or a server without a segment store, has no text index; both come back
// as recall.ErrTextSearchUnsupported and Recall falls back to vector search.
// Text written since the last persist is not in the index either, which is
// why Recall keeps the vector search beside this one.
func (b *Backend) SearchText(ctx context.Context, collection, text string, k int, filter map[string]any) ([]recall.Hit, error) {
	path, err := collectionPath(collection)
	if err != nil {
		return nil, err
	}
	if k <= 0 || k > 10000 {
		return nil, fmt.Errorf("polign: search limit must be in [1, 10000]")
	}
	if strings.TrimSpace(text) == "" {
		return nil, recall.ErrTextSearchUnsupported
	}
	var result struct {
		Hits *[]recall.Hit `json:"hits"`
	}
	err = b.do(ctx, http.MethodPost, path+"/query", map[string]any{"text": text, "k": k, "filter": filter, "typed_metadata": true}, &result)
	var status *StatusError
	if errors.As(err, &status) && textIndexMissing(status) {
		return nil, fmt.Errorf("%w: %s", recall.ErrTextSearchUnsupported, status.Message)
	}
	if err != nil {
		return nil, err
	}
	if result.Hits == nil || len(*result.Hits) > k {
		return nil, fmt.Errorf("polign: invalid search response")
	}
	return *result.Hits, nil
}

// textIndexMissing recognizes the two ways Polign reports that it has no text
// index to search: nothing persisted for the collection yet (404), or no
// segment store on the server (400).
func textIndexMissing(e *StatusError) bool {
	switch e.StatusCode {
	case http.StatusNotFound:
		return strings.Contains(e.Message, "no segments")
	case http.StatusBadRequest:
		return strings.Contains(e.Message, "require a configured segment store")
	}
	return false
}

type leaseGrant struct {
	Epoch     uint64    `json:"epoch"`
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expires_at"`
	TTLMillis int64     `json:"ttl_ms"`
}

// AcquireLease takes an agent's lease. A live holder is reported as a
// recall.LeaseHeldError; a server without the lease API as
// recall.ErrLeaseUnsupported.
func (b *Backend) AcquireLease(ctx context.Context, collection, name, holder string, ttl time.Duration) (recall.LeaseGrant, error) {
	var g leaseGrant
	raw, err := b.lease(ctx, collection, name, "acquire", map[string]any{"holder": holder, "ttl_ms": ttl.Milliseconds()}, &g)
	var se *StatusError
	if errors.As(err, &se) && se.StatusCode == http.StatusConflict {
		var held struct {
			Holder    string    `json:"holder"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		_ = json.Unmarshal(raw, &held)
		return recall.LeaseGrant{}, &recall.LeaseHeldError{Holder: held.Holder, Expires: held.ExpiresAt}
	}
	if err != nil {
		return recall.LeaseGrant{}, err
	}
	return recall.LeaseGrant{Epoch: g.Epoch, Holder: g.Holder, TTL: time.Duration(g.TTLMillis) * time.Millisecond}, nil
}

// RenewLease extends a held lease; a 409 means another holder took over and
// is reported as recall.ErrLeaseLost.
func (b *Backend) RenewLease(ctx context.Context, collection, name, holder string, epoch uint64, ttl time.Duration) (recall.LeaseGrant, error) {
	var g leaseGrant
	_, err := b.lease(ctx, collection, name, "renew", map[string]any{"holder": holder, "epoch": epoch, "ttl_ms": ttl.Milliseconds()}, &g)
	var se *StatusError
	if errors.As(err, &se) && se.StatusCode == http.StatusConflict {
		return recall.LeaseGrant{}, fmt.Errorf("%w: %s", recall.ErrLeaseLost, se.Message)
	}
	if err != nil {
		return recall.LeaseGrant{}, err
	}
	return recall.LeaseGrant{Epoch: g.Epoch, Holder: g.Holder, TTL: time.Duration(g.TTLMillis) * time.Millisecond}, nil
}

// ReleaseLease hands a held lease over at once.
func (b *Backend) ReleaseLease(ctx context.Context, collection, name, holder string, epoch uint64) error {
	_, err := b.lease(ctx, collection, name, "release", map[string]any{"holder": holder, "epoch": epoch}, nil)
	return err
}

func (b *Backend) lease(ctx context.Context, collection, name, action string, body map[string]any, out any) ([]byte, error) {
	path, err := collectionPath(collection)
	if err != nil {
		return nil, err
	}
	raw, err := b.doRaw(ctx, http.MethodPost, path+"/leases/"+url.PathEscape(name)+"/"+action, body, out)
	var se *StatusError
	// A server older than the lease API has no such route.
	if errors.As(err, &se) && (se.StatusCode == http.StatusNotFound || se.StatusCode == http.StatusMethodNotAllowed || se.StatusCode == http.StatusNotImplemented) {
		return raw, fmt.Errorf("%w: %s", recall.ErrLeaseUnsupported, se.Error())
	}
	return raw, err
}

func (b *Backend) do(ctx context.Context, method, path string, body, out any) error {
	_, err := b.doRaw(ctx, method, path, body, out)
	return err
}

// doRaw is do that also returns the response body, so a caller can read the
// details an error response carries.
func (b *Backend) doRaw(ctx context.Context, method, path string, body, out any) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("polign: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("polign: request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, b.maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("polign: response: %w", err)
	}
	if int64(len(raw)) > b.maxResponseBytes {
		return nil, fmt.Errorf("polign: response exceeds %d bytes", b.maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var detail struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &detail)
		message := detail.Error
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		if len(message) > 4096 {
			message = message[:4096]
		}
		return raw, &StatusError{StatusCode: resp.StatusCode, Message: message}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return raw, fmt.Errorf("polign: decode response: %w", err)
		}
	}
	return raw, nil
}
