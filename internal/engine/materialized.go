package engine

import (
	"context"
	"errors"
	"sync"
	"time"
)

// WatermarkBackend optionally supplies an opaque revision of the complete
// collection visible to this reader. It must change on every visible mutation,
// including overwritten IDs, deletes, late arrivals, restores, and route changes.
// It is not an observation timestamp or a maximum event ID. Equality is the only
// meaningful operation; replicas need not expose the same revision.
//
// A revision must never be reused once it has changed. A counter that resets on
// restart lets a revision repeat, and a repeat silently revalidates a cached
// fold that the log has since moved past. Entries also expire after
// materializeTTL so that a backend which under-reports bounds, rather than
// keeps, the staleness it causes.
type WatermarkBackend interface {
	Watermark(context.Context, string) (string, error)
}

const (
	// materializeTTL bounds how long one revision may vouch for a cached fold.
	materializeTTL = 30 * time.Second
	// materializeAttempts bounds the retries for a revision that keeps moving
	// while one pair's history is read.
	materializeAttempts = 3
)

var ErrWatermarkUnsupported = errors.New("recall: backend does not support watermarks")

type materializedEntry struct {
	watermark string
	beliefs   []Belief
	asOf      time.Time
	nextEvent time.Time
	filledAt  time.Time
}

// Materialization is a bounded, process-local derived belief view owned by one
// client. The durable event log remains authoritative. Restart rebuilds the view.
type Materialization struct {
	mu      sync.Mutex
	entries map[pair]materializedEntry
	// values caches which pairs of a subject have ever held a value, for
	// Belief.Elsewhere, under the same revision rules as entries.
	values map[valueKey]valueEntry
}

// valueKey is a subject and a value's identity.
type valueKey struct{ subject, value string }

type valueEntry struct {
	watermark string
	pairs     []pair
	filledAt  time.Time
}

// invalidateValue drops the cached pairs for one subject and value, which a
// write of that value changes.
func (m *Materialization) invalidateValue(subject string, value any) {
	if m == nil || value == nil {
		return
	}
	m.mu.Lock()
	delete(m.values, valueKey{subject, ValueKey(value)})
	m.mu.Unlock()
}

// invalidate drops one pair's cached fold. A write knows exactly which pair it
// changed, so the cache must not wait for the backend's revision to tell it:
// a client has to read its own writes even when the revision lags.
func (m *Materialization) invalidate(p pair) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.entries, p)
	m.mu.Unlock()
}

func (s *Store) materializedBeliefs(p pair, asOf time.Time) ([]Belief, error) {
	w, ok := s.db.(interface{ Watermark(string) (string, error) })
	if !ok || s.materialized == nil {
		return s.foldPair(p, asOf)
	}
	// The revision is an optimisation, never a source of truth. Every problem
	// reading it degrades to the uncached fold, which is always correct, rather
	// than failing a read that the uncached path would have served. An older
	// server, a proxy answering 501, a body without the field, and a transient
	// network error are therefore all the same case.
	for attempt := 0; attempt < materializeAttempts; attempt++ {
		rev, err := w.Watermark(s.collection)
		if err != nil || rev == "" {
			return s.foldPair(p, asOf)
		}
		m := s.materialized
		m.mu.Lock()
		cached, exists := m.entries[p]
		m.mu.Unlock()
		if exists && cached.watermark == rev && s.now().Sub(cached.filledAt) < materializeTTL &&
			!asOf.Before(cached.asOf) && (cached.nextEvent.IsZero() || asOf.Before(cached.nextEvent)) {
			return append([]Belief(nil), cached.beliefs...), nil
		}
		sc, err := s.view()
		if err != nil {
			return nil, err
		}
		_, events, err := s.foldHistory(sc, p.subject, p.predicate)
		if err != nil {
			return nil, err
		}
		after, err := w.Watermark(s.collection)
		if err != nil || after == "" {
			return s.foldEvents(p, events, asOf)
		}
		if rev != after {
			// The revision covers the whole collection, so a write to an
			// unrelated pair moves it. This history may well be complete, and
			// reporting a torn read for someone else's write would fail an
			// answer that is correct. Retry for a quiet window, then serve the
			// fold uncached.
			continue
		}
		beliefs, err := s.foldEvents(p, events, asOf)
		if err != nil {
			return nil, err
		}
		entry := materializedEntry{watermark: rev, beliefs: append([]Belief(nil), beliefs...), asOf: asOf, filledAt: s.now()}
		for _, event := range events {
			if event.ObservedAt.After(asOf) && (entry.nextEvent.IsZero() || event.ObservedAt.Before(entry.nextEvent)) {
				entry.nextEvent = event.ObservedAt
			}
		}
		m.mu.Lock()
		if m.entries == nil || len(m.entries) >= 1024 {
			m.entries = make(map[pair]materializedEntry)
		}
		m.entries[p] = entry
		m.mu.Unlock()
		return beliefs, nil
	}
	return s.foldPair(p, asOf)
}

func (b requestBackend) Watermark(collection string) (string, error) {
	if err := b.ctx.Err(); err != nil {
		return "", err
	}
	w, ok := b.backend.(WatermarkBackend)
	if !ok {
		return "", ErrWatermarkUnsupported
	}
	return w.Watermark(b.ctx, collection)
}

// pairsHolding lists the pairs of subject with an event holding value, read
// through the materialized view when the backend reports revisions, so a
// repeated read costs no listing.
func (s *Store) pairsHolding(subject string, value any) ([]pair, error) {
	w, ok := s.db.(interface{ Watermark(string) (string, error) })
	if !ok || s.materialized == nil {
		return s.listPairsHolding(subject, value)
	}
	k := valueKey{subject, ValueKey(value)}
	rev, err := w.Watermark(s.collection)
	if err != nil || rev == "" {
		return s.listPairsHolding(subject, value)
	}
	m := s.materialized
	m.mu.Lock()
	cached, exists := m.values[k]
	m.mu.Unlock()
	if exists && cached.watermark == rev && s.now().Sub(cached.filledAt) < materializeTTL {
		return append([]pair(nil), cached.pairs...), nil
	}
	pairs, err := s.listPairsHolding(subject, value)
	if err != nil {
		return nil, err
	}
	if after, err := w.Watermark(s.collection); err == nil && after == rev {
		m.mu.Lock()
		if m.values == nil || len(m.values) >= 1024 {
			m.values = make(map[valueKey]valueEntry)
		}
		m.values[k] = valueEntry{watermark: rev, pairs: append([]pair(nil), pairs...), filledAt: s.now()}
		m.mu.Unlock()
	}
	return pairs, nil
}
