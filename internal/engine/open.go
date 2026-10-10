package engine

import (
	"fmt"
	"math"
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
	owner, err := s.match(sc, predicate, valueType, hint)
	if err != nil {
		return "", Predicate{}, nil, err
	}
	if owner != "" {
		return s.recordMerge(sc, predicate, owner)
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

// recordMerge records name as an alias of owner and returns the schema that
// includes it.
func (s *Store) recordMerge(sc *schema, name, owner string) (string, Predicate, *schema, error) {
	if len(sc.log) >= MaxHistoryEvents {
		return "", Predicate{}, nil, fmt.Errorf("%w: registry log is full at %d events", ErrIncompleteHistory, MaxHistoryEvents)
	}
	if _, err := s.appendDefinition(sc.log, registryRecord{Name: name, AliasOf: owner}); err != nil {
		return "", Predicate{}, nil, err
	}
	s.reglog.invalidate()
	sc, err := s.view()
	if err != nil {
		return "", Predicate{}, nil, err
	}
	owner = sc.canonical(name)
	spec, ok := sc.reg[owner]
	if !ok {
		return "", Predicate{}, nil, fmt.Errorf("recall: %q was merged into %q, which is not in the registry log; retry", name, owner)
	}
	return owner, spec, sc, nil
}

// match finds the existing predicate a new name most likely means, or ""
// when none is similar enough. See Config.MatchThreshold.
func (s *Store) match(sc *schema, name, valueType string, hint provenance) (string, error) {
	if s.matchAt <= 0 {
		return "", nil
	}
	description := strings.TrimSpace(hint.description)
	best, bestSim := "", 0.0
	for _, cand := range sc.reg.Names() {
		p := sc.reg[cand]
		if cand == NotePredicate || storageClass(p.valueType()) != storageClass(valueType) {
			continue
		}
		if hint.cardinality != "" && p.Cardinal() != hint.cardinality {
			continue
		}
		// Descriptions are compared only when both sides have one; a name
		// against a name and a description would score low for no reason.
		mine, theirs := predicateText(name, ""), predicateText(cand, "")
		if description != "" && p.Description != "" {
			mine, theirs = predicateText(name, description), predicateText(cand, p.Description)
		}
		sim, err := s.similarity(mine, theirs)
		if err != nil {
			return "", err
		}
		if sim > bestSim {
			best, bestSim = cand, sim
		}
	}
	if bestSim < s.matchAt {
		return "", nil
	}
	return best, nil
}

func predicateText(name, description string) string {
	text := strings.ReplaceAll(name, "_", " ")
	if description != "" {
		text += ". " + description
	}
	return text
}

// similarity is the cosine of two texts' vectors.
func (s *Store) similarity(a, b string) (float64, error) {
	va, err := s.nameVector(a)
	if err != nil {
		return 0, err
	}
	vb, err := s.nameVector(b)
	if err != nil {
		return 0, err
	}
	if len(va) != len(vb) {
		return 0, fmt.Errorf("recall: embedder returned vectors of different lengths")
	}
	var dot, na, nb float64
	for i := range va {
		dot += float64(va[i]) * float64(vb[i])
		na += float64(va[i]) * float64(va[i])
		nb += float64(vb[i]) * float64(vb[i])
	}
	if na == 0 || nb == 0 {
		return 0, nil
	}
	return dot / math.Sqrt(na*nb), nil
}

// nameVector embeds a predicate's text once per client.
func (s *Store) nameVector(text string) ([]float32, error) {
	l := s.reglog
	if l != nil {
		l.vmu.Lock()
		v, ok := l.vecs[text]
		l.vmu.Unlock()
		if ok {
			return v, nil
		}
	}
	v, err := s.embed(text)
	if err != nil {
		return nil, err
	}
	if l != nil {
		l.vmu.Lock()
		if l.vecs == nil {
			l.vecs = map[string][]float32{}
		}
		l.vecs[text] = v
		l.vmu.Unlock()
	}
	return v, nil
}

// Merge makes name read as into from now on: its events fold with into's,
// under into's definition, and a write naming it is filed under into. Events
// keep the name they were written under, so Split undoes the merge. Both must
// store the same type.
func (s *Store) Merge(name, into string) error {
	if s.registryErr != nil {
		return s.registryErr
	}
	sc, err := s.view()
	if err != nil {
		return err
	}
	name, into = s.predicateName(name), sc.canonical(s.predicateName(into))
	from, ok := sc.reg[name]
	if !ok {
		return fmt.Errorf("predicate %q is not defined", name)
	}
	to, ok := sc.reg[into]
	if !ok {
		return fmt.Errorf("predicate %q is not defined", into)
	}
	if name == into || name == NotePredicate || into == NotePredicate {
		return fmt.Errorf("cannot merge %q into %q", name, into)
	}
	if _, configured := s.registry[name]; configured {
		return fmt.Errorf("predicate %q is in this client's registry; rename it there instead", name)
	}
	if storageClass(from.valueType()) != storageClass(to.valueType()) {
		return fmt.Errorf("%w: %q stores %s values and %q stores %s", ErrRegistryMismatch, name, from.valueType(), into, to.valueType())
	}
	defer s.reglog.invalidate()
	_, _, _, err = s.recordMerge(sc, name, into)
	return err
}

// Split undoes a merge: name becomes its own predicate again, defined as the
// predicate it was merged into, and the events written under it fold on
// their own again.
func (s *Store) Split(name string) error {
	if s.registryErr != nil {
		return s.registryErr
	}
	sc, err := s.view()
	if err != nil {
		return err
	}
	name = s.predicateName(name)
	if !sc.merged[name] {
		return fmt.Errorf("predicate %q is not a merged name", name)
	}
	spec := sc.reg[sc.canonical(name)].clone()
	spec.Aliases = nil
	defer s.reglog.invalidate()
	_, err = s.appendDefinition(sc.log, registryRecord{Name: name, Predicate: spec})
	return err
}
