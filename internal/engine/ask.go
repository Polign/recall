package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Memory is one belief told as a sentence, for callers that never see
// predicates.
type Memory struct {
	// Text says what is believed: "user favorite editor: zed". A note is its
	// own words.
	Text string `json:"text"`
	// Since is when it was stated.
	Since time.Time `json:"since"`
	// Before lists, as text, what this replaced, with when each was stated.
	Before []Earlier `json:"before,omitempty"`
	// EventID identifies the statement, for history and audits.
	EventID string `json:"event_id"`
}

// Earlier is a value a memory replaced.
type Earlier struct {
	Text  string    `json:"text"`
	Since time.Time `json:"since"`
}

// Ask answers a question in words with what is believed now: a search over
// everything remembered, folded to current beliefs and told as sentences. A
// time the question names, such as "last week", is searched first.
func (c *Client) Ask(ctx context.Context, question string) ([]Memory, error) {
	return c.AskAt(ctx, question, time.Time{})
}

// AskAt is Ask about what was believed at asOf. Zero means now.
func (c *Client) AskAt(ctx context.Context, question string, asOf time.Time) ([]Memory, error) {
	if strings.TrimSpace(question) == "" {
		return nil, fmt.Errorf("recall: a question is required")
	}
	beliefs, err := c.Recall(ctx, Query{Text: question, AsOf: asOf})
	if err != nil {
		return nil, err
	}
	out := make([]Memory, 0, len(beliefs))
	for _, b := range beliefs {
		m := Memory{Text: Sentence(b.Subject, b.Predicate, b.Value), Since: b.ObservedAt, EventID: b.EventID}
		for _, p := range b.Replaced {
			m.Before = append(m.Before, Earlier{Text: valueText(p.Value), Since: p.ObservedAt})
		}
		out = append(out, m)
	}
	return out, nil
}

// Sentence tells one statement in words. A note is its own words; anything
// else is the subject, the predicate's words, and the value.
func Sentence(subject, predicate string, value any) string {
	if predicate == NotePredicate {
		return valueText(value)
	}
	return subject + " " + strings.ReplaceAll(predicate, "_", " ") + ": " + valueText(value)
}

func valueText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}
