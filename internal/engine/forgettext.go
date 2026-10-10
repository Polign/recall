package engine

import (
	"context"
	"fmt"
	"slices"
)

// Selector picks which remembered statements a request is about. It is how a
// request in words, such as "forget my editor", becomes withdrawals: the
// client finds candidate beliefs by searching for the request, and the
// selector, usually a model, decides which of them it names.
type Selector interface {
	// Select returns the indexes of the candidates the request names, and
	// none when no candidate clearly matches.
	Select(ctx context.Context, request string, candidates []Belief) ([]int, error)
}

// maxForgetCandidates bounds how many beliefs a selector is shown.
const maxForgetCandidates = 20

// ForgetTextResult reports what a request to forget withdrew.
type ForgetTextResult struct {
	Withdrawn []Belief `json:"withdrawn"`
}

// ForgetText withdraws the beliefs a request in words names. It searches the
// current beliefs for the request, asks the selector which of them it means,
// and withdraws each one. Like Forget it records retractions and deletes
// nothing, so a wrong choice is undone by remembering the value again.
func (c *Client) ForgetText(ctx context.Context, request string, selector Selector) (ForgetTextResult, error) {
	var out ForgetTextResult
	if nilInterface(selector) {
		return out, fmt.Errorf("recall: forgetting by text requires a selector")
	}
	candidates, err := c.Recall(ctx, Query{Text: request, Limit: maxForgetCandidates})
	if err != nil {
		return out, err
	}
	// Forgetting withdraws facts. The texts they were drawn from are the
	// record of what was said, and one text can hold facts about many
	// things, so a text is offered only when no fact matches.
	facts := slices.DeleteFunc(slices.Clone(candidates), func(b Belief) bool { return b.Predicate == NotePredicate })
	if len(facts) > 0 {
		candidates = facts
	}
	if len(candidates) == 0 {
		return out, nil
	}
	picked, err := selector.Select(ctx, request, slices.Clone(candidates))
	if err != nil {
		return out, err
	}
	seen := map[int]bool{}
	for _, i := range picked {
		if i < 0 || i >= len(candidates) {
			return out, fmt.Errorf("recall: selector chose candidate %d of %d", i, len(candidates))
		}
		if seen[i] {
			continue
		}
		seen[i] = true
		b := candidates[i]
		n, err := c.Forget(ctx, ForgetRequest{Subject: b.Subject, Predicate: b.Predicate, Value: b.Value})
		if err != nil {
			return out, fmt.Errorf("recall: withdrawing %s %s failed after %d withdrawals: %w", b.Subject, b.Predicate, len(out.Withdrawn), err)
		}
		if n > 0 {
			out.Withdrawn = append(out.Withdrawn, b)
		}
	}
	return out, nil
}
