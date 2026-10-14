package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
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
	// Cardinality ("single" or "multi") and Description define a predicate
	// the proposal coins, for an open client. They are ignored for a
	// predicate that is already defined.
	Cardinality string `json:"cardinality,omitempty"`
	Description string `json:"description,omitempty"`
	// Replaces names beliefs of the same subject that this statement makes
	// untrue, under any predicate. See RememberRequest.Replaces.
	Replaces []BeliefRef `json:"replaces,omitempty"`
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

// The proposals each model extraction made are recorded beside the episode
// they came from, as an event about ExtractionSubject whose EvidenceID is the
// episode. Remembering the same text again replays them instead of asking the
// model again, so a write retried after a crash files the text exactly as the
// first attempt did, and restating something files it the way it was filed
// before. Replayed proposals still go through the fold, so restating "I
// prefer vim" after a switch to zed makes vim current again. Answers leave
// these records out.
const (
	ExtractionSubject   = "recall:extraction"
	ExtractionPredicate = "extraction"
)

// DefaultSubject is who a note is about when the text yielded no proposal to
// take a subject from.
const DefaultSubject = "user"

// noteConfidence marks a note as weaker than an extracted fact (0.8): it
// records that something was said, not what it means.
const noteConfidence = 0.5

// maxProposals bounds one extraction batch.
const maxProposals = 32

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
	extractCtx := ctx
	if !observedAt.IsZero() {
		extractCtx = context.WithValue(ctx, observedAtKey{}, observedAt)
	}
	// An open client offers the extractor everything already defined, so it
	// reuses those names before coining new ones.
	vocab := c.Registry()
	if c.open {
		v, err := c.Vocabulary(ctx)
		if err != nil {
			return out, err
		}
		vocab = v
	}
	// Statements the caller proposed are its own to repeat; only a model's
	// extraction is recorded and replayed.
	_, callerProposed := extractor.(ProposedStatements)
	s, err := c.forContext(ctx)
	if err != nil {
		return out, err
	}
	var proposals []Proposal
	replayed := false
	if !callerProposed {
		if proposals, replayed, err = s.recordedExtraction(text); err != nil {
			return out, err
		}
	}
	if !replayed {
		if c.open {
			extractCtx = context.WithValue(extractCtx, openVocabularyKey{}, true)
		}
		// The extractor sees what memory holds now, so a statement that
		// updates one of those facts lands under the same predicate.
		if !callerProposed {
			held, err := c.held(ctx)
			if err != nil {
				return out, err
			}
			extractCtx = context.WithValue(extractCtx, heldKey{}, held)
		}
		if proposals, err = extractor.Extract(extractCtx, text, vocab); err != nil {
			return out, err
		}
	}
	if len(proposals) > maxProposals {
		return out, fmt.Errorf("recall: extractor exceeded %d proposals", maxProposals)
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
		name := strings.TrimSpace(p.Predicate)
		if c.open {
			name = normalizePredicateName(name)
		}
		proposals[i].Predicate = vocab.canonical(name)
		p = proposals[i]
		spec, ok := vocab[p.Predicate]
		if !ok && c.open {
			if !predicateName.MatchString(p.Predicate) || len(p.Predicate) > maxPredicateName || p.Predicate == RegistryPredicate {
				return out, fmt.Errorf("recall: proposal %d: %q cannot name a predicate", i, p.Predicate)
			}
			if p.Cardinality != "" && p.Cardinality != string(Single) && p.Cardinality != string(Multi) {
				return out, fmt.Errorf("recall: proposal %d: cardinality must be single or multi, got %q", i, p.Cardinality)
			}
			if _, err := inferValueType(p.Value); err != nil {
				return out, fmt.Errorf("recall: proposal %d: %w", i, err)
			}
			filed = append(filed, i)
			continue
		}
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
	if !callerProposed && !replayed {
		if err := s.recordExtraction(episode.Stored, proposals); err != nil {
			return out, fmt.Errorf("recall: recording the extraction failed after the text was kept: %w", err)
		}
	}
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
			ObservedAt: observedAt, Evidence: p.Evidence, EvidenceID: episode.Stored.EventID,
			Cardinality: Cardinality(p.Cardinality), Description: p.Description, Replaces: p.Replaces})
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

type observedAtKey struct{}

// ObservedAt is when the text an extractor is reading was said, as given to
// RememberTextAt. Zero means now. An extractor uses it to resolve relative
// dates such as "last week".
func ObservedAt(ctx context.Context) time.Time {
	t, _ := ctx.Value(observedAtKey{}).(time.Time)
	return t
}

// AdmitProposals keeps the proposals RememberText would accept for text and
// drops the rest, converting a string value to its predicate's declared
// number or boolean type. RememberText refuses a whole batch for one bad
// proposal so that the agent that made it can correct it; an extractor
// backed by a model cannot be asked, so it filters with this instead.
// Proposals naming an unregistered predicate are kept, since RememberText
// files their text as a note.
func (r Registry) AdmitProposals(text string, proposals []Proposal) []Proposal {
	out := make([]Proposal, 0, len(proposals))
	for _, p := range proposals {
		if len(out) == maxProposals {
			break
		}
		p.Subject = strings.TrimSpace(p.Subject)
		p.Predicate = r.canonical(strings.TrimSpace(p.Predicate))
		p.Evidence = repairEvidence(text, p.Evidence)
		if normalizeSubject(p.Subject) == "" || p.Evidence == "" {
			continue
		}
		if spec, ok := r[p.Predicate]; ok {
			p.Value = coerceValue(spec, p.Value)
			if _, err := normalizeValue(p.Predicate, spec, p.Value); err != nil {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// repairEvidence returns the excerpt of text a model's evidence quotes. A
// quote that differs only in case or surrounding space becomes the exact
// excerpt, and no quote at all becomes the whole text when that fits
// MaxEvidenceBytes: the model left the citation out, and the text is what
// supports the statement. A quote the text does not contain stays empty, so
// the proposal is dropped: a statement whose support had to be invented is
// not a fact the text states.
func repairEvidence(text, evidence string) string {
	evidence = strings.TrimSpace(evidence)
	switch {
	case evidence == "":
		if whole := strings.TrimSpace(text); whole != "" && len(whole) <= MaxEvidenceBytes {
			return whole
		}
		return ""
	case len(evidence) > MaxEvidenceBytes:
		return ""
	case strings.Contains(text, evidence):
		return evidence
	}
	if i := strings.Index(strings.ToLower(text), strings.ToLower(evidence)); i >= 0 {
		return text[i : i+len(evidence)]
	}
	return ""
}

// coerceValue reads a number or boolean a model wrote as a string.
func coerceValue(spec Predicate, v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	s = strings.TrimSpace(s)
	switch spec.valueType() {
	case typeNumber:
		if f, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64); err == nil {
			return f
		}
	case typeBoolean:
		switch strings.ToLower(s) {
		case "true", "yes":
			return true
		case "false", "no":
			return false
		}
	}
	return v
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

type openVocabularyKey struct{}

// OpenVocabulary reports whether the client asking for an extraction accepts
// predicates the extractor coins. A closed client files only the predicates
// it offered.
func OpenVocabulary(ctx context.Context) bool {
	open, _ := ctx.Value(openVocabularyKey{}).(bool)
	return open
}

type heldKey struct{}

// maxHeld bounds how many current beliefs an extractor is shown.
const maxHeld = 64

// Held returns the beliefs memory held when the extractor was asked, newest
// first and at most maxHeld, so it can file a statement that updates one of
// them under that fact's predicate, or name it in Proposal.Replaces. Notes
// and the records of earlier extractions are left out. Empty for proposals
// the caller made itself.
func Held(ctx context.Context) []Belief {
	held, _ := ctx.Value(heldKey{}).([]Belief)
	return held
}

// WithHeld returns a context carrying held as what Held reports, for testing
// an extractor without a store.
func WithHeld(ctx context.Context, held []Belief) context.Context {
	return context.WithValue(ctx, heldKey{}, held)
}

// held gathers what Held returns: see there.
func (c *Client) held(ctx context.Context) ([]Belief, error) {
	beliefs, err := c.Recall(ctx, Query{Limit: 4 * maxHeld, NoElsewhere: true})
	if err != nil {
		return nil, err
	}
	out := make([]Belief, 0, len(beliefs))
	for _, b := range beliefs {
		if b.Predicate == NotePredicate || b.Predicate == RegistryPredicate || b.Predicate == ExtractionPredicate || b.Subject == ExtractionSubject {
			continue
		}
		out = append(out, b)
	}
	slices.SortStableFunc(out, func(a, b Belief) int { return b.ObservedAt.Compare(a.ObservedAt) })
	if len(out) > maxHeld {
		out = out[:maxHeld]
	}
	return out, nil
}

// recordedExtraction returns the proposals recorded for the episode that
// holds text, when one does. A recording whose evidence no longer quotes the
// text exactly, which happens when the text differs only in case, is not
// replayed.
func (s *Store) recordedExtraction(text string) ([]Proposal, bool, error) {
	sc, err := s.view()
	if err != nil {
		return nil, false, err
	}
	events, err := s.History(DefaultSubject, NotePredicate)
	if err != nil {
		return nil, false, err
	}
	episode := ""
	for _, b := range sc.fold(NotePredicate, events, s.now()) {
		if b.Value == text {
			episode = b.EventID
			break
		}
	}
	if episode == "" {
		return nil, false, nil
	}
	rows, _, err := s.db.List(s.collection, map[string]any{"subject": ExtractionSubject, "predicate": ExtractionPredicate, "evidence_id": episode}, 1)
	if err != nil || len(rows) == 0 {
		return nil, false, err
	}
	e, err := s.decodeEvent(rows[0].ID, rows[0].Metadata)
	if err != nil {
		return nil, false, err
	}
	raw, _ := e.Value.(string)
	var proposals []Proposal
	if err := json.Unmarshal([]byte(raw), &proposals); err != nil {
		return nil, false, fmt.Errorf("%w: extraction record %q is malformed", ErrInvalidEvent, e.ID)
	}
	for _, p := range proposals {
		if !strings.Contains(text, p.Evidence) {
			return nil, false, nil
		}
	}
	return proposals, true, nil
}

// recordExtraction keeps what a model proposed for an episode.
func (s *Store) recordExtraction(episode Belief, proposals []Proposal) error {
	if proposals == nil {
		proposals = []Proposal{}
	}
	value, err := json.Marshal(proposals)
	if err != nil {
		return err
	}
	at := episode.ObservedAt
	return s.append(Event{
		ID:         eventID(ExtractionSubject, ExtractionPredicate, string(value), false, at),
		Kind:       "fact",
		Subject:    ExtractionSubject,
		Predicate:  ExtractionPredicate,
		Value:      string(value),
		Confidence: 1,
		Source:     "tool_result",
		ObservedAt: at,
		EvidenceID: episode.EventID,
	})
}

// episodeFiled reports whether a model extraction filed any statement from
// the episode with this id.
func (s *Store) episodeFiled(id string) (bool, error) {
	rows, _, err := s.db.List(s.collection, map[string]any{"subject": ExtractionSubject, "predicate": ExtractionPredicate, "evidence_id": id}, 1)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	e, err := s.decodeEvent(rows[0].ID, rows[0].Metadata)
	if err != nil {
		return false, err
	}
	raw, _ := e.Value.(string)
	var proposals []Proposal
	if err := json.Unmarshal([]byte(raw), &proposals); err != nil {
		return false, fmt.Errorf("%w: extraction record %q is malformed", ErrInvalidEvent, e.ID)
	}
	return len(proposals) > 0, nil
}
