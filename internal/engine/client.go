package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// Backend is the context-aware storage contract used by Client. Implementations
// must be safe for concurrent calls. List must obey VectorDB's exact totals,
// internal pagination, and stable ordered-prefix contract.
type Backend interface {
	Put(ctx context.Context, collection, id string, values []float32, metadata map[string]any) error
	List(ctx context.Context, collection string, filter map[string]any, limit int) ([]StoredVector, int, error)
	Search(ctx context.Context, collection string, values []float32, k int, filter map[string]any) ([]Hit, error)
}

// Embedder supplies compatible document and query vectors. Implementations must
// honor cancellation and be safe for concurrent calls.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// EmbedFunc adapts a function to Embedder.
type EmbedFunc func(context.Context, string) ([]float32, error)

func (f EmbedFunc) Embed(ctx context.Context, text string) ([]float32, error) { return f(ctx, text) }

// ErrEmbedderRequired means an operation needs vectors but no embedder was configured.
var ErrEmbedderRequired = errors.New("recall: an embedder is required for this operation")

// Config configures an immutable Client. Embedder is optional for exact reads,
// histories, and exports; writes and semantic recall require it.
type Config struct {
	Backend    Backend
	Collection string
	Registry   Registry
	Embedder   Embedder
	// Materialize caches current pair beliefs when the backend supplies watermarks.
	Materialize bool
}

// Client is a context-aware memory client. Configuration is immutable, and
// operations have independent request state. Concurrent writes still follow the
// backend's consistency guarantees; Client does not promise serializable writes.
type Client struct {
	backend      Backend
	collection   string
	registry     Registry
	embedder     Embedder
	materialized *Materialization
}

// NewClient validates configuration and copies the registry.
func NewClient(cfg Config) (*Client, error) {
	if nilInterface(cfg.Backend) {
		return nil, fmt.Errorf("recall: backend is required")
	}
	collection := strings.TrimSpace(cfg.Collection)
	if collection == "" {
		return nil, fmt.Errorf("recall: collection is required")
	}
	if len(cfg.Registry) == 0 {
		return nil, fmt.Errorf("recall: registry must not be empty")
	}
	if err := cfg.Registry.Validate(); err != nil {
		return nil, err
	}
	embedder := cfg.Embedder
	if nilInterface(embedder) {
		embedder = nil
	}
	c := &Client{backend: cfg.Backend, collection: collection, registry: cfg.Registry.Clone(), embedder: embedder}
	if cfg.Materialize {
		c.materialized = &Materialization{}
	}
	return c, nil
}

func nilInterface(v any) bool {
	if v == nil {
		return true
	}
	switch reflect.ValueOf(v).Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflect.ValueOf(v).IsNil()
	}
	return false
}

// Registry returns an independent copy of the client's predicate registry.
func (c *Client) Registry() Registry { return c.registry.Clone() }

// RememberRequest records a typed value (string, float64, or bool). Kind defaults
// to fact and Source to user_stated. Nil Confidence defaults to 1; a pointer to
// zero records zero confidence rather than selecting the default.
type RememberRequest struct {
	Subject    string
	Predicate  string
	Value      any
	Kind       string
	Confidence *float64
	Source     string
}

// ForgetRequest selects exactly one typed Value or All=true. False and numeric
// zero are values; an omitted value never implicitly withdraws everything.
type ForgetRequest struct {
	Subject   string
	Predicate string
	Value     any
	All       bool
}

// Remember records one statement, preserving the embedding or backend error.
func (c *Client) Remember(ctx context.Context, q RememberRequest) (RememberResult, error) {
	s, err := c.forContext(ctx)
	if err != nil {
		return RememberResult{}, err
	}
	confidence := 1.0
	if q.Confidence != nil {
		confidence = *q.Confidence
	}
	kind := q.Kind
	if kind == "" {
		kind = "fact"
	}
	return s.remember(kind, q.Subject, q.Predicate, q.Value, confidence, q.Source)
}

// Forget appends a targeted or blanket retraction.
func (c *Client) Forget(ctx context.Context, q ForgetRequest) (int, error) {
	s, err := c.forContext(ctx)
	if err != nil {
		return 0, err
	}
	if q.All == (q.Value != nil) {
		return 0, fmt.Errorf("recall: forget requires exactly one value or all=true")
	}
	return s.forgetValue(q.Subject, q.Predicate, q.Value)
}

// Recall returns current or historical beliefs.
func (c *Client) Recall(ctx context.Context, q Query) ([]Belief, error) {
	s, err := c.forContext(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.Recall(q)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

// History returns a complete subject-and-predicate event history.
func (c *Client) History(ctx context.Context, subject, predicate string) ([]Event, error) {
	s, err := c.forContext(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.History(subject, predicate)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ExportEvents returns a validated event export.
func (c *Client) ExportEvents(ctx context.Context, q Export) ([]Event, error) {
	s, err := c.forContext(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.ExportEvents(q)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

// This adapter exists only for one operation; no context or embedding error is
// ever written into shared client configuration.
func (c *Client) forContext(ctx context.Context) (*Store, error) {
	if ctx == nil {
		return nil, fmt.Errorf("recall: context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &Store{db: requestBackend{ctx: ctx, backend: c.backend}, collection: c.collection, registry: c.registry, now: time.Now, materialized: c.materialized,
		embed: func(text string) ([]float32, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if c.embedder == nil {
				return nil, ErrEmbedderRequired
			}
			values, err := c.embedder.Embed(ctx, text)
			if err != nil {
				return nil, fmt.Errorf("recall: embed: %w", err)
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(values) == 0 {
				return nil, fmt.Errorf("recall: embedder returned an empty vector")
			}
			for _, v := range values {
				if !finite(float64(v)) {
					return nil, fmt.Errorf("recall: embedder returned a non-finite vector")
				}
			}
			return values, nil
		},
	}, nil
}

type requestBackend struct {
	ctx     context.Context
	backend Backend
}

func (b requestBackend) Put(collection, id string, v []float32, md map[string]any) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	return b.backend.Put(b.ctx, collection, id, v, md)
}
func (b requestBackend) List(collection string, f map[string]any, limit int) ([]StoredVector, int, error) {
	if err := b.ctx.Err(); err != nil {
		return nil, 0, err
	}
	rows, total, err := b.backend.List(b.ctx, collection, f, limit)
	if err == nil {
		err = b.ctx.Err()
	}
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}
func (b requestBackend) Search(collection string, v []float32, k int, f map[string]any) ([]Hit, error) {
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	hits, err := b.backend.Search(b.ctx, collection, v, k, f)
	if err == nil {
		err = b.ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return hits, nil
}
