package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Explanation says why one belief is held: what it was drawn from, the name
// it was written under, the definitions that decide how its history folds,
// and that history.
type Explanation struct {
	Memory    string    `json:"memory"`
	Subject   string    `json:"subject"`
	Predicate string    `json:"predicate"`
	Value     any       `json:"value"`
	StatedAt  time.Time `json:"stated_at"`
	Source    string    `json:"source"`
	// WrittenAs is the name the statement was written under, when a merge
	// files it under another.
	WrittenAs string `json:"written_as,omitempty"`
	// Evidence is the excerpt the statement was drawn from, and SourceText
	// the whole text it came from.
	Evidence   string `json:"evidence,omitempty"`
	SourceText string `json:"source_text,omitempty"`
	// Rules are the definitions of the predicate and every name it reads,
	// oldest first.
	Rules []Rule `json:"rules"`
	// History is every statement about this subject and predicate, oldest
	// first, including the ones replaced or withdrawn.
	History []Event `json:"history"`
}

// Rule is one recorded definition, told for a reader.
type Rule struct {
	At          time.Time `json:"at,omitzero"`
	Name        string    `json:"name"`
	Change      string    `json:"change"`
	Cardinality string    `json:"cardinality,omitempty"`
	ValueType   string    `json:"value_type,omitempty"`
	Description string    `json:"description,omitempty"`
}

// Explain explains each belief a question finds, as Ask would answer it.
func (c *Client) Explain(ctx context.Context, question string) ([]Explanation, error) {
	if strings.TrimSpace(question) == "" {
		return nil, fmt.Errorf("recall: a question is required")
	}
	beliefs, err := c.Recall(ctx, Query{Text: question})
	if err != nil {
		return nil, err
	}
	out := make([]Explanation, 0, len(beliefs))
	for _, b := range beliefs {
		e, err := c.ExplainBelief(ctx, b)
		if err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, nil
}

// ExplainBelief explains one belief.
func (c *Client) ExplainBelief(ctx context.Context, b Belief) (Explanation, error) {
	s, err := c.forContext(ctx)
	if err != nil {
		return Explanation{}, err
	}
	sc, err := s.view()
	if err != nil {
		return Explanation{}, err
	}
	out := Explanation{Memory: Sentence(b.Subject, b.Predicate, b.Value), Subject: b.Subject, Predicate: b.Predicate, Value: b.Value, StatedAt: b.ObservedAt, Source: b.Source}
	if out.History, err = s.History(b.Subject, b.Predicate); err != nil {
		return out, err
	}
	for _, e := range out.History {
		if e.ID != b.EventID {
			continue
		}
		if e.Predicate != b.Predicate {
			out.WrittenAs = e.Predicate
		}
		out.Evidence = e.Evidence
		if e.EvidenceID != "" {
			if out.SourceText, err = c.sourceText(ctx, s, e.EvidenceID); err != nil {
				return out, err
			}
		}
	}

	names := sc.storedNames(b.Predicate)
	changes, err := decodeRegistryLog(sc.log)
	if err != nil {
		return out, err
	}
	for _, ch := range changes {
		if !slices.Contains(names, ch.Name) {
			continue
		}
		r := Rule{At: ch.At, Name: ch.Name, Cardinality: ch.Predicate.Cardinality, ValueType: ch.Predicate.valueType(), Description: ch.Predicate.Description}
		switch {
		case ch.AliasOf != "":
			r = Rule{At: ch.At, Name: ch.Name, Change: "merged into " + ch.AliasOf}
		case ch.Retroactive:
			r.Change = "corrected for its whole history"
		case ch.Auto:
			r.Change = "defined on first use"
		default:
			r.Change = "declared"
		}
		out.Rules = append(out.Rules, r)
	}
	if len(out.Rules) == 0 {
		if p, ok := sc.reg[b.Predicate]; ok {
			out.Rules = []Rule{{Name: b.Predicate, Change: "configured in this client's registry", Cardinality: p.Cardinality, ValueType: p.valueType(), Description: p.Description}}
		}
	}
	return out, nil
}

// sourceText reads the whole text an excerpt came from: by id when the
// backend reads events by id, otherwise from the notes it is kept among.
func (c *Client) sourceText(ctx context.Context, s *Store, id string) (string, error) {
	events, err := c.Events(ctx, []string{id})
	if errors.Is(err, ErrGetUnsupported) {
		if events, err = s.History(DefaultSubject, NotePredicate); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	for _, e := range events {
		if e.ID == id {
			text, _ := e.Value.(string)
			return text, nil
		}
	}
	return "", nil
}
