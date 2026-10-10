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
	// Open lets writes define predicates. A write naming a predicate nothing
	// has defined records its definition in the store's registry log, and a
	// predicate defined there by any client is writable. Registry then only
	// seeds the vocabulary and may be empty. Without Open the registry is a
	// closed set and an unregistered predicate is refused.
	Open bool
	// MatchThreshold is how similar, from 0 to 1, a new predicate name must
	// be to an existing one for an open write to use the existing one
	// instead of defining it ("favorite_editor" for "editor_favorite").
	// Similarity is the cosine of the embedder's vectors for the two names,
	// and their descriptions when both have one. Zero means 0.9; a negative
	// value turns matching off. Only a predicate with the same stored type,
	// and the same cardinality when the write names one, can match.
	MatchThreshold float64
}

// defaultMatchThreshold is deliberately strict. A missed match leaves two
// predicates for one relation, which costs completeness; a wrong match
// can make one value replace an unrelated one. Client.Split undoes a match.
const defaultMatchThreshold = 0.9

// Client is a context-aware memory client. Configuration is immutable, and
// operations have independent request state. Concurrent writes still follow the
// backend's consistency guarantees; Client does not promise serializable writes.
type Client struct {
	backend      Backend
	collection   string
	registry     Registry
	embedder     Embedder
	materialized *Materialization
	reglog       *registryLog
	open         bool
	matchAt      float64
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
	if !finite(cfg.MatchThreshold) || cfg.MatchThreshold > 1 {
		return nil, fmt.Errorf("recall: match threshold must be at most 1")
	}
	if len(cfg.Registry) == 0 && !cfg.Open {
		return nil, fmt.Errorf("recall: registry must not be empty")
	}
	if err := cfg.Registry.Validate(); err != nil {
		return nil, err
	}
	registry, err := cfg.Registry.withNote()
	if err != nil {
		return nil, err
	}
	embedder := cfg.Embedder
	if nilInterface(embedder) {
		embedder = nil
	}
	c := &Client{backend: cfg.Backend, collection: collection, registry: registry, embedder: embedder, reglog: &registryLog{}, open: cfg.Open}
	switch {
	case cfg.MatchThreshold == 0:
		c.matchAt = defaultMatchThreshold
	case cfg.MatchThreshold > 0:
		c.matchAt = cfg.MatchThreshold
	}
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

// Vocabulary returns every predicate the store can read: the configured
// registry plus everything its registry log defines. For an open client this
// is also everything it can write.
func (c *Client) Vocabulary(ctx context.Context) (Registry, error) {
	s, err := c.forContext(ctx)
	if err != nil {
		return nil, err
	}
	sc, err := s.view()
	if err != nil {
		return nil, err
	}
	return sc.reg.Clone(), nil
}

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
	// ObservedAt is when the statement was made, for statements recorded
	// after the fact, such as an imported conversation. Zero means now. A
	// statement observed before a later one for the same subject and
	// predicate is history: it answers as_of queries for its time and does
	// not replace what the later one says.
	ObservedAt time.Time
	// Evidence is the exact excerpt the statement was drawn from, at most
	// MaxEvidenceBytes, and EvidenceID the event holding the whole text.
	// RememberText sets both; a caller recording a quote may set Evidence.
	Evidence   string
	EvidenceID string
	// Cardinality and Description define the predicate when an open client
	// meets it for the first time. Cardinality defaults to Single, because a
	// stated fact is more often an update than an addition; a wrong guess is
	// corrected by recording a new definition, and every answer follows it.
	// Both are ignored for a predicate that is already defined.
	Cardinality Cardinality
	Description string
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
	return s.remember(kind, q.Subject, q.Predicate, q.Value, confidence, q.Source,
		provenance{observedAt: q.ObservedAt, evidence: q.Evidence, evidenceID: q.EvidenceID, cardinality: q.Cardinality, description: q.Description})
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

// PromoteRequest files a note under a typed predicate. Note is the note's
// value exactly as recall returned it; the remaining fields are the typed
// statement it becomes, with RememberRequest's defaults.
type PromoteRequest struct {
	Subject    string
	Note       string
	Predicate  string
	Value      any
	Kind       string
	Confidence *float64
	Source     string
}

// Promote records a typed statement and then withdraws the note it came from,
// in that order: a failure between the two writes leaves the fact stated
// twice, never not at all. The note stays in history, so the promotion is
// auditable and can be reversed.
func (c *Client) Promote(ctx context.Context, q PromoteRequest) (RememberResult, error) {
	if strings.TrimSpace(q.Predicate) == NotePredicate {
		return RememberResult{}, fmt.Errorf("recall: promote files a note under another predicate, not %q", NotePredicate)
	}
	s, err := c.forContext(ctx)
	if err != nil {
		return RememberResult{}, err
	}
	events, err := s.History(q.Subject, NotePredicate)
	if err != nil {
		return RememberResult{}, err
	}
	held := false
	for _, b := range Fold(events, Multi, s.now()) {
		held = held || ValueKey(b.Value) == ValueKey(strings.TrimSpace(q.Note))
	}
	if !held {
		return RememberResult{}, fmt.Errorf("recall: %q holds no current note matching %q", normalizeSubject(q.Subject), q.Note)
	}
	r, err := c.Remember(ctx, RememberRequest{Subject: q.Subject, Predicate: q.Predicate, Value: q.Value, Kind: q.Kind, Confidence: q.Confidence, Source: q.Source})
	if err != nil {
		return RememberResult{}, err
	}
	if _, err := s.forgetValue(q.Subject, NotePredicate, strings.TrimSpace(q.Note)); err != nil {
		return r, fmt.Errorf("recall: stored %s but could not withdraw the note: %w", q.Predicate, err)
	}
	return r, nil
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
	return &Store{db: requestBackend{ctx: ctx, backend: c.backend}, collection: c.collection, registry: c.registry, reglog: c.reglog, now: time.Now, materialized: c.materialized, textFirst: textFirstFor(c.embedder), open: c.open, matchAt: c.matchAt,
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

// GetBackend is an optional Backend capability: reading records by id. It
// is how a caller follows a belief's EvidenceID to the text it came from.
// Unknown ids are omitted from the result, not reported as errors.
type GetBackend interface {
	Get(ctx context.Context, collection string, ids []string) ([]StoredVector, error)
}

// ErrGetUnsupported reports a backend that cannot read records by id.
var ErrGetUnsupported = errors.New("recall: this backend cannot read events by id")

// MaxGetEvents bounds one Events call.
const MaxGetEvents = 1000

// Events reads events by id, in the order asked, skipping ids that are not
// stored. It is how a caller reads the text behind a belief's EvidenceID.
func (c *Client) Events(ctx context.Context, ids []string) ([]Event, error) {
	if ctx == nil {
		return nil, fmt.Errorf("recall: context must not be nil")
	}
	var want []string
	seen := map[string]bool{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return []Event{}, nil
	}
	if len(want) > MaxGetEvents {
		return nil, fmt.Errorf("recall: at most %d events per call, got %d", MaxGetEvents, len(want))
	}
	g, ok := c.backend.(GetBackend)
	if !ok {
		return nil, ErrGetUnsupported
	}
	rows, err := g.Get(ctx, c.collection, want)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	s := &Store{collection: c.collection, registry: c.registry}
	byID := make(map[string]Event, len(rows))
	for _, r := range rows {
		e, err := s.decodeEvent(r.ID, r.Metadata)
		if err != nil {
			return nil, err
		}
		byID[e.ID] = e
	}
	out := make([]Event, 0, len(byID))
	for _, id := range want {
		if e, ok := byID[id]; ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// textFirstFor puts lexical hits ahead of vector hits when the vectors are
// themselves only hashed words. Fusing the two rewards hits both searches
// found, and two word-overlap rankings mostly agree on filler that shares
// common words with the query; on LongMemEval-S, recall over the conversation
// found an answer session in its top ten for 85% of questions fused (text
// counted twice) against 98% with the text ranking first. A model embedder
// adds meaning that BM25 lacks, so its ranking is fused evenly.
func textFirstFor(e Embedder) bool {
	switch e.(type) {
	case LexicalEmbedder, *LexicalEmbedder:
		return true
	}
	return false
}

// TextSearchBackend is an optional Backend capability, the context-aware form
// of TextSearcher.
type TextSearchBackend interface {
	SearchText(ctx context.Context, collection, text string, k int, filter map[string]any) ([]Hit, error)
}

func (b requestBackend) SearchText(collection, text string, k int, f map[string]any) ([]Hit, error) {
	ts, ok := b.backend.(TextSearchBackend)
	if !ok {
		return nil, ErrTextSearchUnsupported
	}
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	hits, err := ts.SearchText(b.ctx, collection, text, k, f)
	if err == nil {
		err = b.ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	return hits, nil
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
