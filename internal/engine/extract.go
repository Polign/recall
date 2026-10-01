package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
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
	// Episode is the note holding the whole text, written first so that every
	// statement can name it as its evidence. Restating text already kept
	// returns the existing note.
	Episode *RememberResult `json:"episode,omitempty"`
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
// The text itself is kept first, whole, as a note about DefaultSubject: the
// episode. Every statement then records its quoted excerpt as Evidence and the
// episode as EvidenceID, so what was said stays readable however it was
// interpreted. A proposal whose predicate is not registered is also kept as a
// note under its own subject. A malformed proposal for a registered predicate
// still fails the batch, because the caller can correct it.
func (c *Client) RememberText(ctx context.Context, text string, extractor Extractor) (ExtractionResult, error) {
	return c.RememberTextAt(ctx, text, extractor, time.Time{})
}

// RememberTextAt is RememberText for text that was said at observedAt, such as
// an imported conversation. Every statement and note it writes carries that
// time; zero means now. See RememberRequest.ObservedAt.
func (c *Client) RememberTextAt(ctx context.Context, text string, extractor Extractor, observedAt time.Time) (ExtractionResult, error) {
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
		if len(strings.TrimSpace(p.Evidence)) > MaxEvidenceBytes {
			return out, fmt.Errorf("recall: proposal %d: evidence is longer than %d bytes; quote the part that supports the fact", i, MaxEvidenceBytes)
		}
		proposals[i].Predicate = c.registry.canonical(strings.TrimSpace(p.Predicate))
		p = proposals[i]
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
	out.Proposals = proposals

	confidence := noteConfidence
	episode, err := c.Remember(ctx, RememberRequest{Subject: DefaultSubject, Predicate: NotePredicate, Value: text, Source: "agent_inferred", Confidence: &confidence, ObservedAt: observedAt})
	if err != nil {
		return out, fmt.Errorf("recall: keeping the text failed before any statement was written: %w", err)
	}
	out.Episode = &episode
	if len(proposals) == 0 {
		noteSubjects = []string{DefaultSubject}
	}

	for _, i := range filed {
		p := proposals[i]
		kind := "fact"
		if strings.HasPrefix(p.Predicate, "prefers_") {
			kind = "preference"
		}
		confidence := 0.8
		r, err := c.Remember(ctx, RememberRequest{Subject: p.Subject, Predicate: p.Predicate, Value: p.Value, Kind: kind, Source: "agent_inferred", Confidence: &confidence,
			ObservedAt: observedAt, Evidence: p.Evidence, EvidenceID: episode.Stored.EventID})
		if err != nil {
			return out, fmt.Errorf("recall: proposal %d failed after %d completed writes: %w", i, len(out.Results), err)
		}
		out.Results = append(out.Results, r)
	}
	// Results keep their order: statements, then one note per subject. The
	// note about DefaultSubject is the episode, already written, so callers
	// reading Results for it still find it where they did.
	for _, subject := range noteSubjects {
		if subject == DefaultSubject {
			out.Results = append(out.Results, episode)
			continue
		}
		confidence := noteConfidence
		r, err := c.Remember(ctx, RememberRequest{Subject: subject, Predicate: NotePredicate, Value: text, Source: "agent_inferred", Confidence: &confidence, ObservedAt: observedAt})
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
