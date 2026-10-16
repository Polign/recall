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
	// Elsewhere lists, as text, what else is believed about the same subject and
	// value under another wording, with when it was stated: "user likes:
	// mangoes" beside "user dislikes: mangoes". The newer one is usually
	// the one that holds.
	Elsewhere []Earlier `json:"elsewhere,omitempty"`
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
	s, err := c.forContext(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Memory, 0, len(beliefs))
	for _, b := range beliefs {
		// The name a question calls the user by tells the asker nothing
		// and would take a place an answer could use.
		if b.Predicate == "name" && normalizeSubject(b.Subject) == DefaultSubject && namedIn(question, b.Value) {
			continue
		}
		// A text that statements were filed from is told by those
		// statements; repeating it would say everything twice.
		if b.Predicate == NotePredicate {
			filed, err := s.episodeFiled(b.EventID)
			if err != nil {
				return nil, err
			}
			if filed {
				continue
			}
		}
		m := Memory{Text: Sentence(b.Subject, b.Predicate, b.Value), Since: b.ObservedAt, EventID: b.EventID}
		for _, p := range b.Replaced {
			text := valueText(p.Value)
			// A correction across predicates names what it corrected
			// whole: "user likes: mangoes", not just "mangoes".
			if p.Predicate != "" && p.Predicate != b.Predicate {
				text = Sentence(b.Subject, p.Predicate, p.Value)
			}
			m.Before = append(m.Before, Earlier{Text: text, Since: p.ObservedAt})
		}
		for _, p := range b.Elsewhere {
			m.Elsewhere = append(m.Elsewhere, Earlier{Text: Sentence(b.Subject, p.Predicate, p.Value), Since: p.ObservedAt})
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

// namedIn reports whether question calls someone by name, every word of it,
// without asking about the name itself ("what is my name", "what am I
// called").
func namedIn(question string, name any) bool {
	n, ok := name.(string)
	if !ok || strings.TrimSpace(n) == "" {
		return false
	}
	asked := map[string]bool{}
	for _, w := range words(question) {
		asked[w] = true
	}
	if asked["name"] || asked["named"] || asked["called"] {
		return false
	}
	for _, w := range words(n) {
		if !asked[w] {
			return false
		}
	}
	return true
}
