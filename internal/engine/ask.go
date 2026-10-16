package engine

import (
	"context"
	"fmt"
	"slices"
	"sort"
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
	return c.AskWith(ctx, AskRequest{Question: question, AsOf: asOf})
}

// AskRequest is a read in words.
type AskRequest struct {
	// Question searches what is remembered. Empty reads without searching:
	// everything believed about the user (a profile), or, with Since,
	// everything that changed.
	Question string
	// AsOf answers as of an earlier instant; zero means now.
	AsOf time.Time
	// Since keeps only what was stated after this instant, each with what
	// it replaced: what changed since an agent last looked. Zero means any
	// time.
	Since time.Time
}

// AskWith answers an AskRequest. With a question it searches, as Ask does.
// Without one it reads, with no model and no search: the user's current
// beliefs, or with Since, every subject's beliefs stated since then,
// newest first.
func (c *Client) AskWith(ctx context.Context, r AskRequest) ([]Memory, error) {
	q := Query{Text: strings.TrimSpace(r.Question), AsOf: r.AsOf, ObservedAfter: r.Since}
	if q.Text == "" {
		q.Limit = MaxRecall
		if r.Since.IsZero() {
			q.Subject = DefaultSubject
		}
	}
	beliefs, err := c.Recall(ctx, q)
	if err != nil {
		return nil, err
	}
	s, err := c.forContext(ctx)
	if err != nil {
		return nil, err
	}
	if q.Text == "" {
		beliefs = slices.DeleteFunc(beliefs, func(b Belief) bool { return isReservedPair(pair{normalizeSubject(b.Subject), b.Predicate}) })
		if !r.Since.IsZero() {
			sort.SliceStable(beliefs, func(i, j int) bool { return beliefs[i].ObservedAt.After(beliefs[j].ObservedAt) })
		}
	} else if !s.textFirst {
		// A model embedder's similarities mean something, so an answer
		// leaves out what is far less related than its best match instead
		// of padding the reader's context with it. Hashed word vectors are
		// not compared.
		beliefs = keepRelevant(beliefs, askRelevanceRatio)
	}
	return s.tell(beliefs, q.Text)
}

// tell turns beliefs into Memories. Statements about the user are told under
// the user's name when one is known ("Maya lives in: Portland"), so a
// reader asked about Maya knows each memory is hers; the name itself is
// left out when the question already calls her by it.
func (s *Store) tell(beliefs []Belief, question string) ([]Memory, error) {
	out := make([]Memory, 0, len(beliefs))
	who := func(subject string) string { return subject }
	if names := s.userNames(); len(names) > 0 && strings.TrimSpace(names[0]) != "" {
		who = func(subject string) string {
			if normalizeSubject(subject) == DefaultSubject {
				return names[0]
			}
			return subject
		}
	}
	for _, b := range beliefs {
		if b.Predicate == "name" && normalizeSubject(b.Subject) == DefaultSubject && question != "" && namedIn(question, b.Value) {
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
		subject := who(b.Subject)
		if b.Predicate == "name" {
			subject = b.Subject // "user name: Maya", not "Maya name: Maya"
		}
		m := Memory{Text: Sentence(subject, b.Predicate, b.Value), Since: b.ObservedAt, EventID: b.EventID}
		for _, p := range b.Replaced {
			text := valueText(p.Value)
			// A correction across predicates names what it corrected
			// whole: "user likes: mangoes", not just "mangoes".
			if p.Predicate != "" && p.Predicate != b.Predicate {
				text = Sentence(subject, p.Predicate, p.Value)
			}
			m.Before = append(m.Before, Earlier{Text: text, Since: p.ObservedAt})
		}
		for _, p := range b.Elsewhere {
			m.Elsewhere = append(m.Elsewhere, Earlier{Text: Sentence(subject, p.Predicate, p.Value), Since: p.ObservedAt})
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

// askRelevanceRatio is how close to the best match a belief must score to
// stay in an answer. Measured on Recall's own comparison questions with
// all-MiniLM-L6-v2: the facts that answer a question scored at least 0.6 of
// the best one, and unrelated facts (an email for a question about food)
// under 0.3.
const askRelevanceRatio = 0.5

// keepRelevant drops beliefs whose relevance is known and under ratio of the
// best one's. A belief with no score (an exact read, a keyword-only hit) is
// kept, since nothing says it is unrelated.
func keepRelevant(beliefs []Belief, ratio float32) []Belief {
	var top float32
	for _, b := range beliefs {
		top = max(top, b.Relevance)
	}
	if top <= 0 {
		return beliefs
	}
	out := beliefs[:0:0]
	for _, b := range beliefs {
		if b.Relevance > 0 && b.Relevance < top*ratio {
			continue
		}
		out = append(out, b)
	}
	return out
}
