package recall

import (
	"context"
	"fmt"
	"strings"
)

// Proposal is an untrusted model suggestion, never a belief until Remember
// validates and folds it. Evidence must be an exact excerpt from the input.
type Proposal struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Value     any    `json:"value"`
	Evidence  string `json:"evidence"`
}

type Extractor interface {
	Extract(context.Context, string, Registry) ([]Proposal, error)
}

type ExtractionResult struct {
	Proposals []Proposal       `json:"proposals"`
	Results   []RememberResult `json:"results"`
}

// RememberText validates the entire proposal batch before the first write.
// Writes are sequential, not transactional. On a storage failure, the returned
// result contains the successful prefix, so callers must not blindly retry.
func (c *Client) RememberText(ctx context.Context, text string, extractor Extractor) (ExtractionResult, error) {
	out := ExtractionResult{Results: []RememberResult{}}
	if ctx == nil {
		return out, fmt.Errorf("recall: context is required")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if nilInterface(extractor) {
		return out, fmt.Errorf("recall: free-text remember requires an extractor; use typed remember or supply model proposals")
	}
	if strings.TrimSpace(text) == "" || len(text) > 32768 {
		return out, fmt.Errorf("recall: text must contain 1 to 32768 bytes")
	}
	proposals, err := extractor.Extract(ctx, text, c.Registry())
	if err != nil {
		return out, err
	}
	if len(proposals) > 32 {
		return out, fmt.Errorf("recall: extractor exceeded 32 proposals")
	}
	for i, p := range proposals {
		spec, ok := c.registry[p.Predicate]
		if !ok {
			return out, fmt.Errorf("recall: proposal %d: predicate %q is not registered", i, p.Predicate)
		}
		if normalizeSubject(p.Subject) == "" {
			return out, fmt.Errorf("recall: proposal %d: subject is required", i)
		}
		if _, err := normalizeValue(p.Predicate, spec, p.Value); err != nil {
			return out, fmt.Errorf("recall: proposal %d: %w", i, err)
		}
		if strings.TrimSpace(p.Evidence) == "" || !strings.Contains(text, p.Evidence) {
			return out, fmt.Errorf("recall: proposal %d: evidence must quote the input", i)
		}
	}
	out.Proposals = proposals
	for i, p := range proposals {
		kind := "fact"
		if strings.HasPrefix(p.Predicate, "prefers_") {
			kind = "preference"
		}
		confidence := 0.8
		r, err := c.Remember(ctx, RememberRequest{Subject: p.Subject, Predicate: p.Predicate, Value: p.Value, Kind: kind, Source: "agent_inferred", Confidence: &confidence})
		if err != nil {
			return out, fmt.Errorf("recall: proposal %d failed after %d completed writes: %w", i, len(out.Results), err)
		}
		out.Results = append(out.Results, r)
	}
	return out, nil
}

// ProposedStatements adapts proposals made by the calling agent. This lets an
// MCP host use its own model for extraction without another model service.
// RememberText still treats every proposal as untrusted and validates the batch.
type ProposedStatements []Proposal

func (p ProposedStatements) Extract(ctx context.Context, _ string, _ Registry) ([]Proposal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]Proposal(nil), p...), nil
}
