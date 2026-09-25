package engine

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// MaxCandidateEvents bounds event discovery for broad exact queries. A query
// for one explicit subject/predicate uses MaxHistoryEvents instead.
const MaxCandidateEvents = 10000

// ErrIncompleteCandidates means a broad exact query could not discover enough
// matching beliefs or prove it exhausted the candidate set. No partial answer
// is returned; callers can narrow the query to an explicit subject/predicate.
var ErrIncompleteCandidates = errors.New("recall: complete exact candidate discovery unavailable")

func candidateFilter(q Query) map[string]any {
	f := map[string]any{}
	if q.Subject != "" {
		f["subject"] = normalizeSubject(q.Subject)
	}
	if q.Predicate != "" {
		f["predicate"] = strings.TrimSpace(q.Predicate)
	}
	if q.Kind != "" {
		f["kind"] = q.Kind
	}
	return f
}

func (s *Store) recallExact(q Query, limit int, asOf time.Time) ([]Belief, error) {
	filter := candidateFilter(q)
	width := min(max(limit*4, 20), MaxCandidateEvents)
	total := -1
	var previous []Event
	seen := make(map[pair]bool)
	var out ranked
	for {
		events, count, err := s.exactCandidates(filter, width)
		if err != nil {
			return nil, err
		}
		if total >= 0 && count != total {
			return nil, fmt.Errorf("%w: total changed during discovery; retry", ErrIncompleteCandidates)
		}
		total = count
		// Widening List must extend the same ordered prefix. Check decoded
		// content as well as IDs: an overwritten event invalidates prior folds.
		if len(previous) > 0 && (len(events) < len(previous) || !reflect.DeepEqual(events[:len(previous)], previous)) {
			return nil, fmt.Errorf("%w: events changed during discovery; retry", ErrIncompleteCandidates)
		}
		for _, p := range dedupePairs(events[len(previous):]) {
			if seen[p] {
				continue
			}
			seen[p] = true
			beliefs, err := s.pairBeliefs(p, asOf)
			if err != nil {
				return nil, err
			}
			for _, b := range beliefs {
				if matchesBelief(b, q) && out.add(b) == limit {
					return out.beliefs(), nil
				}
			}
		}
		if len(events) == total {
			return out.beliefs(), nil
		}
		if width == MaxCandidateEvents {
			return nil, fmt.Errorf("%w: examined %d of %d events; narrow the query", ErrIncompleteCandidates, width, total)
		}
		previous = events
		width = min(width*2, MaxCandidateEvents)
	}
}

func (s *Store) exactCandidates(filter map[string]any, limit int) ([]Event, int, error) {
	rows, total, err := s.db.List(s.collection, filter, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", ErrIncompleteCandidates, err)
	}
	if total < 0 || len(rows) != min(limit, total) {
		return nil, 0, fmt.Errorf("%w: read %d of %d events with limit %d", ErrIncompleteCandidates, len(rows), total, limit)
	}
	seen := make(map[string]bool, len(rows))
	events := make([]Event, 0, len(rows))
	for _, row := range rows {
		if seen[row.ID] {
			return nil, 0, fmt.Errorf("%w: duplicate event %q", ErrIncompleteCandidates, row.ID)
		}
		seen[row.ID] = true
		e, err := s.decodeEvent(row.ID, row.Metadata)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: %w", ErrIncompleteCandidates, err)
		}
		events = append(events, e)
	}
	return events, total, nil
}
