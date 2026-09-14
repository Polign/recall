package engine

import (
	"fmt"
	"math"
)

// MaxRecall bounds the number of beliefs a query can request. Candidate
// discovery and result allocations are bounded independently of caller input.
const MaxRecall = 1000

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func validateQuery(q Query) error {
	if q.Limit > MaxRecall {
		return fmt.Errorf("recall: limit %d exceeds maximum %d", q.Limit, MaxRecall)
	}
	if !finite(q.MinConfidence) || q.MinConfidence < 0 || q.MinConfidence > 1 {
		return fmt.Errorf("recall: min confidence must be finite and in [0, 1]")
	}
	if q.ValueMin != nil && !finite(*q.ValueMin) || q.ValueMax != nil && !finite(*q.ValueMax) {
		return fmt.Errorf("recall: value bounds must be finite")
	}
	if q.ValueMin != nil && q.ValueMax != nil && *q.ValueMin > *q.ValueMax {
		return fmt.Errorf("recall: value minimum exceeds maximum")
	}
	return nil
}

// decodeEvent also enforces a known predicate's value type. Unknown historical
// predicates remain readable: removing a registry entry must not erase its log.
func (s *Store) decodeEvent(id string, md map[string]any) (Event, error) {
	e, err := DecodeEvent(id, md)
	if err != nil {
		return Event{}, err
	}
	if spec, ok := s.registry[e.Predicate]; ok && e.Value != nil {
		if _, err := normalizeValue(e.Predicate, spec, e.Value); err != nil {
			return Event{}, fmt.Errorf("%w: event %q: %w", ErrInvalidEvent, id, err)
		}
	}
	return e, nil
}
