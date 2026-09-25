package engine

import (
	"context"
	"fmt"
	"slices"
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
	// Unfiled lists the proposals whose predicate is not registered. Their
	// text was kept as a note rather than refused; the predicates they name
	// are the ones this registry is missing.
	Unfiled []Proposal `json:"unfiled,omitempty"`
}

// DefaultSubject is who a note is about when the text yielded no proposal to
// take a subject from.
const DefaultSubject = "user"

// noteConfidence marks a note as weaker than an extracted fact (0.8): it
// records that something was said, not what it means.
const noteConfidence = 0.5

// RememberText validates the entire proposal batch before the first write.
// Writes are sequential, not transactional. On a storage failure, the returned
// result contains the successful prefix, so callers must not blindly retry.
//
// Nothing stated is dropped. A proposal whose predicate is not registered, or
// text that yielded no proposals at all, is kept as a note holding the whole
// text, once per subject. A malformed proposal for a registered predicate
// still fails the batch, because the caller can correct it.
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
	var filed []int
	var noteSubjects []string
	for i, p := range proposals {
		subject := normalizeSubject(p.Subject)
		if subject == "" {
			return out, fmt.Errorf("recall: proposal %d: subject is required", i)
		}
		if strings.TrimSpace(p.Evidence) == "" || !strings.Contains(text, p.Evidence) {
			return out, fmt.Errorf("recall: proposal %d: evidence must quote the input", i)
		}
		spec, ok := c.registry[p.Predicate]
		if !ok {
			out.Unfiled = append(out.Unfiled, p)
			if !slices.Contains(noteSubjects, subject) {
				noteSubjects = append(noteSubjects, subject)
			}
			continue
		}
		if _, err := normalizeValue(p.Predicate, spec, p.Value); err != nil {
			return out, fmt.Errorf("recall: proposal %d: %w", i, err)
		}
		filed = append(filed, i)
	}
	if len(proposals) == 0 {
		noteSubjects = []string{DefaultSubject}
	}
	out.Proposals = proposals
	for _, i := range filed {
		p := proposals[i]
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
	for _, subject := range noteSubjects {
		confidence := noteConfidence
		r, err := c.Remember(ctx, RememberRequest{Subject: subject, Predicate: NotePredicate, Value: text, Source: "agent_inferred", Confidence: &confidence})
		if err != nil {
			return out, fmt.Errorf("recall: note for %q failed after %d completed writes: %w", subject, len(out.Results), err)
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
