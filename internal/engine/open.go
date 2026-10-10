package engine

import (
	"fmt"
	"strings"
)

// maxPredicateName bounds a predicate name.
const maxPredicateName = 64

// predicateName is how this store reads a predicate a caller named. A closed
// store takes the name as given, so a misspelling is refused rather than
// guessed at. An open store defines whatever it is given, so it first brings
// the name to snake_case: "Works For" and "works-for" are works_for.
func (s *Store) predicateName(name string) string {
	name = strings.TrimSpace(name)
	if !s.open {
		return name
	}
	return normalizePredicateName(name)
}

// normalizePredicateName lowercases a name and turns every run of characters
// other than ASCII letters and digits into one underscore.
func normalizePredicateName(name string) string {
	var b strings.Builder
	gap := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if gap && b.Len() > 0 {
				b.WriteByte('_')
			}
			gap = false
			b.WriteRune(r)
			continue
		}
		gap = true
	}
	return b.String()
}

// inferValueType is the value type a predicate defined by its first value
// gets. Enums, dates, and refs are only ever declared, never inferred.
func inferValueType(v any) (string, error) {
	switch v.(type) {
	case string:
		return typeString, nil
	case float64:
		return typeNumber, nil
	case bool:
		return typeBoolean, nil
	}
	return "", fmt.Errorf("value must be a string, number, or boolean, got %s", describeValue(v))
}

// define records the definition of a predicate an open write met for the
// first time, and returns the schema that includes it. It reads the registry
// log afresh first, because another client may have defined the name since
// this one last looked; if two define it at once, the first definition
// recorded is the one everyone folds under.
func (s *Store) define(predicate string, value any, hint provenance) (string, Predicate, *schema, error) {
	if predicate == RegistryPredicate {
		return "", Predicate{}, nil, fmt.Errorf("predicate %q is reserved for the registry's own history", predicate)
	}
	s.reglog.invalidate()
	sc, err := s.view()
	if err != nil {
		return "", Predicate{}, nil, err
	}
	predicate = sc.canonical(predicate)
	if spec, ok := sc.reg[predicate]; ok {
		return predicate, spec, sc, nil
	}
	valueType, err := inferValueType(value)
	if err != nil {
		return "", Predicate{}, nil, err
	}
	card := hint.cardinality
	if card == "" {
		card = Single
	}
	spec := Predicate{Cardinality: string(card), ValueType: valueType, Description: strings.TrimSpace(hint.description)}
	if err := (Registry{predicate: spec}).Validate(); err != nil {
		return "", Predicate{}, nil, err
	}
	if len(sc.log) >= MaxHistoryEvents {
		return "", Predicate{}, nil, fmt.Errorf("%w: registry log is full at %d events", ErrIncompleteHistory, MaxHistoryEvents)
	}
	if _, err := s.appendDefinition(sc.log, registryRecord{Name: predicate, Auto: true, Predicate: spec}); err != nil {
		return "", Predicate{}, nil, err
	}
	s.reglog.invalidate()
	if sc, err = s.view(); err != nil {
		return "", Predicate{}, nil, err
	}
	predicate = sc.canonical(predicate)
	spec, ok := sc.reg[predicate]
	if !ok {
		return "", Predicate{}, nil, fmt.Errorf("recall: predicate %q was defined but is not in the registry log; retry", predicate)
	}
	return predicate, spec, sc, nil
}
