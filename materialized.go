package recall

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// WatermarkBackend optionally supplies an opaque revision of the complete
// collection visible to this reader. It must change on every visible mutation,
// including overwritten IDs, deletes, late arrivals, restores, and route changes.
// It is not an observation timestamp or a maximum event ID. Equality is the only
// meaningful operation; replicas need not expose the same revision.
type WatermarkBackend interface {
	Watermark(context.Context, string) (string, error)
}

var ErrWatermarkUnsupported = errors.New("recall: backend does not support watermarks")

type materializedEntry struct {
	watermark string
	beliefs   []Belief
	asOf      time.Time
	nextEvent time.Time
}

// Materialization is a bounded, process-local derived belief view owned by one
// client. The durable event log remains authoritative. Restart rebuilds the view.
type Materialization struct {
	mu      sync.Mutex
	entries map[pair]materializedEntry
}

func (s *Store) materializedBeliefs(p pair, asOf time.Time) ([]Belief, error) {
	w, ok := s.db.(interface{ Watermark(string) (string, error) })
	if !ok || s.materialized == nil {
		return s.foldPair(p, asOf)
	}
	rev, err := w.Watermark(s.collection)
	if errors.Is(err, ErrWatermarkUnsupported) {
		return s.foldPair(p, asOf)
	}
	if err != nil {
		return nil, err
	}
	if rev == "" {
		return nil, fmt.Errorf("recall: empty backend watermark")
	}
	m := s.materialized
	m.mu.Lock()
	cached, exists := m.entries[p]
	m.mu.Unlock()
	if exists && cached.watermark == rev && !asOf.Before(cached.asOf) && (cached.nextEvent.IsZero() || asOf.Before(cached.nextEvent)) {
		return append([]Belief(nil), cached.beliefs...), nil
	}
	events, err := s.History(p.subject, p.predicate)
	if err != nil {
		return nil, err
	}
	after, err := w.Watermark(s.collection)
	if err != nil {
		return nil, err
	}
	if rev != after {
		return nil, fmt.Errorf("%w: log changed while materializing; retry", ErrIncompleteHistory)
	}
	card := Single
	if spec, ok := s.registry[p.predicate]; ok {
		card = spec.Cardinal()
	}
	beliefs := Fold(events, card, asOf)
	entry := materializedEntry{watermark: rev, beliefs: append([]Belief(nil), beliefs...), asOf: asOf}
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
