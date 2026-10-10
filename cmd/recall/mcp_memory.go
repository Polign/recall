package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Polign/recall"
	"github.com/Polign/recall/model"
)

// The memory tools. These are the product surface: an agent states what it
// learned and asks what it knows, and the rules about what that means live in
// recall rather than in the model's judgment.
//
// The default registry and lexical embedder make the tools usable without
// a schema file or embedding service.

// memoryEnabled reports whether the memory tools are live.
func (s *mcpServer) memoryEnabled() bool { return s.memory != nil }

func (s *mcpServer) memoryTools() []mcpTool {
	if !s.memoryEnabled() {
		return nil
	}
	if s.memory.text {
		return append(s.textTools(), s.agentTools()...)
	}
	subject := map[string]any{
		"type":        "string",
		"description": "who or what the statement is about, usually \"user\" or an entity name",
	}
	predicate := map[string]any{
		"type":        "string",
		"description": "the relation, which must be one from list_predicates; unregistered predicates are refused. When none fits, use note with the statement as the value",
	}
	if s.memory.open {
		predicate["description"] = "the relation in snake_case. Reuse one from list_predicates when it fits; a new name is defined by its first use, single-valued unless cardinality says multi"
	}
	tools := []mcpTool{
		{
			Name:        "list_predicates",
			Title:       "List what can be remembered",
			Description: "List the predicates this memory accepts, with each one's cardinality (single-valued predicates supersede, multi-valued ones accumulate) and value type. The set is closed: call this before remembering anything, because an unregistered predicate is refused rather than invented. The note predicate takes anything worth keeping that no other predicate fits, so nothing has to be dropped.",
			InputSchema: schema(map[string]any{}),
		},
		{
			Name:        "remember",
			Title:       "Remember a statement",
			Description: "Record a typed statement (subject, predicate, value), or extract facts from free text by passing text plus statements you propose. When the server has an extraction model, text alone is enough and the server proposes the statements. Each statement requires subject, predicate, value, and evidence quoting the text. Use list_predicates first. Never drop a statement because no predicate fits: in typed mode use predicate note with the statement as the value; in text mode propose the predicate you would want, and the whole text is kept as a note and the proposal reported under unfiled. Text with no statements at all is also kept as a note. Text mode validates all proposals before writes; a backend failure reports completed results and is not atomic. A single-valued predicate replaces whatever was believed before, a multi-valued one adds to it, and restating something already believed changes nothing. Nothing is ever overwritten: the previous statement stays readable through memory_history.",
			InputSchema: schema(map[string]any{
				"text":       map[string]any{"type": "string", "maxLength": 32768, "description": "Original text the statements come from. Required with statements; on a server with an extraction model it may be given alone. Cannot be combined with top-level typed fields."},
				"statements": map[string]any{"type": "array", "maxItems": 32, "description": "Your proposed facts from text; registry validation and the fold decide what is stored.", "items": schema(map[string]any{"subject": subject, "predicate": predicate, "value": map[string]any{"type": []string{"string", "number", "boolean"}}, "evidence": map[string]any{"type": "string", "description": "Exact quote from text supporting this fact"}}, "subject", "predicate", "value", "evidence")},
				"subject":    subject,
				"predicate":  predicate,
				"value": map[string]any{
					"description": "the value, matching the predicate's declared type (string, number, or boolean)",
				},
				"kind": map[string]any{
					"type":        "string",
					"enum":        []string{"fact", "preference"},
					"description": "fact or preference (default fact)",
				},
				"confidence": map[string]any{
					"type":        "number",
					"description": "how sure this is, in [0, 1] (default 1)",
				},
				"source": map[string]any{
					"type":        "string",
					"enum":        []string{"user_stated", "agent_inferred", "tool_result"},
					"description": "how this was come by (default user_stated). Say agent_inferred when you worked it out rather than being told.",
				},
				"observed_at": map[string]any{
					"type":        "string",
					"description": "when the statement was made, as an RFC3339 instant like 2023-05-20T02:21:00Z; omit for now. Use it only when recording something said earlier, such as an imported conversation. Works in typed and text mode. A statement dated before a later one for the same subject and predicate is kept as history and does not replace it.",
				},
			}),
		},
		{
			Name:        "recall",
			Title:       "Recall what is believed",
			Description: "Ask what is believed about a subject. Give subject and predicate for an exact answer, or query for retrieval over remembered text (lexical word overlap by default; semantic with a configured embedding service). Only current beliefs are returned, and each one lists under replaced the value it replaced, when that was stated, and its source, so a correction arrives with the answer. days_ago says how many days before as_of (or today) each belief was stated. Forgotten statements are not returned, and memory_history has the full chain. Pass as_of to ask what was believed at a past instant instead of now.",
			InputSchema: schema(map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "natural-language search over what is remembered; omit for an exact lookup",
				},
				"subject":   subject,
				"predicate": predicate,
				"as_of": map[string]any{
					"type":        "string",
					"description": "RFC3339 instant, e.g. 2026-09-01T00:00:00Z. Answers as of then rather than now, so you can ask what was believed before a correction.",
				},
				"observed_after": map[string]any{
					"type":        "string",
					"description": "RFC3339 instant: keep only beliefs stated at or after it. A query that names a time itself (\"two weeks ago\", \"last Saturday\", \"in March\") already searches that span first, so set this only to exclude everything else.",
				},
				"observed_before": map[string]any{
					"type":        "string",
					"description": "RFC3339 instant: keep only beliefs stated at or before it",
				},
				"with_sources": map[string]any{
					"type":        "boolean",
					"description": "also return, for each belief remembered from text, the excerpt it was drawn from (evidence) and the whole text it came from (source_text). Use it when the exact wording or surrounding detail matters.",
				},
				"min_confidence": map[string]any{
					"type":        "number",
					"description": "drop beliefs held less confidently than this",
				},
				"limit": map[string]any{"type": "integer", "description": "how many beliefs to return (default 20)"},
			}),
		},
		{
			Name:        "forget",
			Title:       "Forget a statement",
			Description: "Withdraw what is believed about a subject and predicate. With value given only that value is withdrawn; without it, all of them. This records a retraction rather than deleting anything, so asking what was believed before it still works.",
			InputSchema: schema(map[string]any{
				"subject":   subject,
				"predicate": predicate,
				"value": map[string]any{
					"description": "withdraw only this typed value (string, number, boolean); omit to withdraw every value for the predicate",
				},
			}, "subject", "predicate"),
		},
		{
			Name:        "explain",
			Title:       "Explain a belief",
			Description: "Why each belief a question finds is held: the text it was drawn from, the name it was written under when a merge filed it under another, the definitions that decide how it folds (declared, defined on first use, corrected, or merged), and its full history.",
			InputSchema: schema(map[string]any{
				"question": map[string]any{"type": "string", "description": "what to explain, in words"},
			}, "question"),
		},
		{
			Name:        "memory_history",
			Title:       "Read a belief's history",
			Description: "Every statement ever recorded for one subject and predicate, oldest first, including the ones since superseded or retracted. This is the audit trail: what was believed, when, how confidently, and on what basis.",
			InputSchema: schema(map[string]any{
				"subject":   subject,
				"predicate": predicate,
			}, "subject", "predicate"),
		},
	}
	if s.memory.open {
		for i := range tools {
			switch tools[i].Name {
			case "list_predicates":
				tools[i].Description = "List the predicates in use, with each one's cardinality (single-valued predicates supersede, multi-valued ones accumulate) and value type. Reuse one when it fits before naming a new one, so one relation is not split across two names."
			case "remember":
				tools[i].Description = strings.Replace(tools[i].Description, "Use list_predicates first. Never drop a statement because no predicate fits: in typed mode use predicate note with the statement as the value; in text mode propose the predicate you would want, and the whole text is kept as a note and the proposal reported under unfiled.", "Reuse a predicate from list_predicates when one fits; otherwise name a new one, and give cardinality multi when its values add up rather than replace each other.", 1)
				props := tools[i].InputSchema["properties"].(map[string]any)
				props["cardinality"] = map[string]any{"type": "string", "enum": []string{"single", "multi"}, "description": "for a predicate this write defines: single (a new value replaces the old, the default) or multi (values add up). Ignored for a predicate already in use"}
				props["description"] = map[string]any{"type": "string", "description": "for a predicate this write defines: one line saying what it records"}
			}
		}
	}
	out := make([]mcpTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Name == "remember" {
			textMode := []string{"text", "statements"}
			if s.memory.extractor != nil {
				textMode = []string{"text"}
			}
			tool.InputSchema["oneOf"] = []any{
				map[string]any{"required": textMode, "not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"subject"}}, map[string]any{"required": []string{"predicate"}}, map[string]any{"required": []string{"value"}}, map[string]any{"required": []string{"kind"}}, map[string]any{"required": []string{"source"}}, map[string]any{"required": []string{"confidence"}}}}},
				map[string]any{"required": []string{"subject", "predicate", "value"}, "not": map[string]any{"anyOf": []any{map[string]any{"required": []string{"text"}}, map[string]any{"required": []string{"statements"}}}}},
			}
			tool.InputSchema["additionalProperties"] = false
		}
		if tool.Name == "remember" || tool.Name == "forget" {
			if !s.write {
				continue
			}
		} else {
			tool.Annotations = readOnlyTool
		}
		out = append(out, tool)
	}
	return append(out, s.agentTools()...)
}

// runMemoryTool dispatches the memory tools, reporting whether it handled the
// call so the raw database tools stay reachable underneath.
func (s *mcpServer) runMemoryTool(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	if !s.memoryEnabled() {
		return "", false, nil
	}
	if s.agents != nil && agentTools[name] {
		out, err := s.runAgentTool(ctx, name, args)
		return out, true, err
	}
	switch name {
	case "remember", "forget":
		if !s.write {
			return "", true, fmt.Errorf("this server is read-only; it was started without -write")
		}
	case "recall":
	case "list_predicates", "memory_history", "explain":
		if s.memory.text {
			return "", false, nil
		}
	default:
		return "", false, nil
	}
	m := s.memory.forRequest(ctx)
	if m.text {
		out, err := m.runTextTool(name, args)
		return out, true, err
	}
	var out string
	var err error
	switch name {
	case "list_predicates":
		out, err = m.toolListPredicates()
	case "remember":
		out, err = m.toolRemember(args)
	case "recall":
		out, err = m.toolRecall(args)
	case "forget":
		out, err = m.toolForget(args)
	case "memory_history":
		out, err = m.toolHistory(args)
	case "explain":
		out, err = m.toolExplain(args)
	}
	return out, true, err
}

func (m *memoryRuntime) toolListPredicates() (string, error) {
	reg := m.registry()
	type entry struct {
		Predicate   string `json:"predicate"`
		Cardinality string `json:"cardinality"`
		ValueType   string `json:"value_type"`
		Description string `json:"description"`
	}
	out := make([]entry, 0, len(reg))
	for _, name := range reg.Names() {
		p := reg[name]
		vt := p.ValueType
		if vt == "" {
			vt = "string"
		}
		out = append(out, entry{name, p.Cardinality, vt, p.Description})
	}
	text, err := marshal(out)
	return text, err
}

func (m *memoryRuntime) toolRemember(args json.RawMessage) (string, error) {
	var a struct {
		Text       string             `json:"text"`
		Statements *[]recall.Proposal `json:"statements"`
		Subject    string             `json:"subject"`
		Predicate  string             `json:"predicate"`
		Value      any                `json:"value"`
		Kind       string             `json:"kind"`
		Confidence *float64           `json:"confidence"`
		Source     string             `json:"source"`
		ObservedAt string             `json:"observed_at"`
		// Cardinality and Description define a predicate the write names
		// for the first time, on an open server.
		Cardinality string `json:"cardinality"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	var observedAt time.Time
	if a.ObservedAt != "" {
		t, err := time.Parse(time.RFC3339, a.ObservedAt)
		if err != nil {
			return "", fmt.Errorf("observed_at must be an RFC3339 instant like 2023-05-20T02:21:00Z: %w", err)
		}
		observedAt = t
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return "", err
	}
	if _, textMode := fields["text"]; textMode {
		_, dated := fields["observed_at"]
		_, proposed := fields["statements"]
		if len(fields) != 1+btoi(dated)+btoi(proposed) {
			return "", fmt.Errorf("text mode takes text, statements, and observed_at only; propose facts with quoted evidence using list_predicates")
		}
		if m.client == nil {
			return "", fmt.Errorf("free-text remember requires the context-aware memory client")
		}
		extractor := m.extractor
		if a.Statements != nil {
			extractor = recall.ProposedStatements(*a.Statements)
		} else if extractor == nil {
			return "", fmt.Errorf("this server has no extraction model (start it with -extract-model), so pass the statements you propose from the text")
		}
		result, err := m.client.RememberTextAt(m.ctx, a.Text, extractor, observedAt)
		if err != nil && len(result.Results) > 0 {
			encoded, _ := marshalWithoutEvidence(map[string]any{"partial": result, "error": err.Error()})
			return "", fmt.Errorf("%s", encoded)
		}
		if err != nil {
			return "", err
		}
		return marshalWithoutEvidence(result)
	}
	if _, ok := fields["statements"]; ok {
		return "", fmt.Errorf("statements requires original text")
	}
	if m.client != nil {
		res, err := m.client.Remember(m.ctx, recall.RememberRequest{Subject: a.Subject, Predicate: a.Predicate, Value: a.Value, Kind: a.Kind, Confidence: a.Confidence, Source: a.Source, ObservedAt: observedAt,
			Cardinality: recall.Cardinality(a.Cardinality), Description: a.Description})
		if err != nil {
			return "", err
		}
		return marshalWithoutEvidence(res)
	}
	if !observedAt.IsZero() {
		return "", fmt.Errorf("observed_at requires the context-aware memory client")
	}
	if a.Kind == "" {
		a.Kind = "fact"
	}
	confidence := 1.0
	if a.Confidence != nil {
		confidence = *a.Confidence
	}
	res, err := m.store.Remember(a.Kind, a.Subject, a.Predicate, a.Value, confidence, a.Source)
	if err != nil {
		return "", err
	}
	return marshal(res)
}

func (m *memoryRuntime) toolRecall(args json.RawMessage) (string, error) {
	var a struct {
		Query          string  `json:"query"`
		Subject        string  `json:"subject"`
		Predicate      string  `json:"predicate"`
		AsOf           string  `json:"as_of"`
		ObservedAfter  string  `json:"observed_after"`
		ObservedBefore string  `json:"observed_before"`
		MinConfidence  float64 `json:"min_confidence"`
		Limit          int     `json:"limit"`
		WithSources    bool    `json:"with_sources"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	q := recall.Query{
		Text:          a.Query,
		Subject:       a.Subject,
		Predicate:     a.Predicate,
		MinConfidence: a.MinConfidence,
		Limit:         a.Limit,
	}
	if a.AsOf != "" {
		t, err := time.Parse(time.RFC3339, a.AsOf)
		if err != nil {
			return "", fmt.Errorf("as_of must be an RFC3339 instant like 2026-09-01T00:00:00Z: %w", err)
		}
		q.AsOf = t
	}
	for _, bound := range []struct {
		name, value string
		into        *time.Time
	}{{"observed_after", a.ObservedAfter, &q.ObservedAfter}, {"observed_before", a.ObservedBefore, &q.ObservedBefore}} {
		if bound.value == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, bound.value)
		if err != nil {
			return "", fmt.Errorf("%s must be an RFC3339 instant like 2026-09-01T00:00:00Z: %w", bound.name, err)
		}
		*bound.into = t
	}
	if q.Text == "" && q.Subject == "" && q.Predicate == "" {
		return "", fmt.Errorf("give query for a semantic search, or subject and predicate for an exact one")
	}
	var beliefs []recall.Belief
	var err error
	if m.client != nil {
		beliefs, err = m.client.Recall(m.ctx, q)
	} else {
		beliefs, err = m.store.Recall(q)
	}
	if err != nil {
		return "", err
	}
	var out string
	if a.WithSources {
		out, err = m.withSources(beliefs)
	} else {
		out, err = marshalWithoutEvidence(beliefs)
	}
	if err != nil {
		return "", err
	}
	ref := q.AsOf
	if ref.IsZero() {
		ref = time.Now()
	}
	return withDaysAgo(out, ref)
}

// withDaysAgo adds to each belief how many whole days before ref it was
// stated, so a model reading the answer does not have to do calendar
// arithmetic to tell "last week" from "last month".
func withDaysAgo(beliefsJSON string, ref time.Time) (string, error) {
	var beliefs []map[string]any
	if err := json.Unmarshal([]byte(beliefsJSON), &beliefs); err != nil {
		return "", err
	}
	if len(beliefs) == 0 {
		return beliefsJSON, nil
	}
	day := func(t time.Time) time.Time {
		t = t.UTC()
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	}
	for _, b := range beliefs {
		at, _ := b["observed_at"].(string)
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			continue
		}
		b["days_ago"] = int(day(ref).Sub(day(t)).Hours() / 24)
	}
	return marshal(beliefs)
}

// sourcedBelief is a belief with the whole text its evidence points to. The
// field is source_text, not source, which already names how the belief was
// come by (user_stated, agent_inferred).
type sourcedBelief struct {
	recall.Belief
	SourceText string `json:"source_text,omitempty"`
}

// withSources adds, to each belief remembered from text, the text itself.
func (m *memoryRuntime) withSources(beliefs []recall.Belief) (string, error) {
	out := make([]sourcedBelief, len(beliefs))
	var ids []string
	for i, b := range beliefs {
		out[i].Belief = b
		if b.EvidenceID != "" {
			ids = append(ids, b.EvidenceID)
		}
	}
	if len(ids) > 0 {
		if m.client == nil {
			return "", fmt.Errorf("with_sources requires the context-aware memory client")
		}
		events, err := m.client.Events(m.ctx, ids)
		if err != nil {
			return "", err
		}
		text := make(map[string]string, len(events))
		for _, e := range events {
			if v, ok := e.Value.(string); ok {
				text[e.ID] = v
			}
		}
		for i := range out {
			out[i].SourceText = text[out[i].EvidenceID]
		}
	}
	return marshal(out)
}

// marshalWithoutEvidence drops the evidence and evidence_id fields from every
// belief and event in v. polign-recall 0.5.0 and earlier build their Belief
// and Event objects from every field a tool returns and fail on one they do
// not know, so these fields reach only a caller that asks with with_sources.
// Proposals keep their evidence, which those clients already read.
func marshalWithoutEvidence(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return "", err
	}
	var strip func(any)
	strip = func(node any) {
		switch n := node.(type) {
		case map[string]any:
			_, belief := n["event_id"]
			_, id := n["id"]
			_, observed := n["observed_at"]
			if belief || id && observed {
				delete(n, "evidence")
				delete(n, "evidence_id")
			}
			for _, child := range n {
				strip(child)
			}
		case []any:
			for _, child := range n {
				strip(child)
			}
		}
	}
	strip(tree)
	return marshal(tree)
}

func (m *memoryRuntime) toolForget(args json.RawMessage) (string, error) {
	var a struct {
		Subject   string `json:"subject"`
		Predicate string `json:"predicate"`
		Value     any    `json:"value"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return "", err
	}
	if raw, ok := fields["value"]; ok && string(bytes.TrimSpace(raw)) == "null" {
		return "", fmt.Errorf("forget value must be string, number, or boolean; omit it to withdraw all")
	}

	var n int
	var err error
	if m.client != nil {
		n, err = m.client.Forget(m.ctx, recall.ForgetRequest{Subject: a.Subject, Predicate: a.Predicate, Value: a.Value, All: a.Value == nil})
	} else {
		value := ""
		if a.Value != nil {
			value = fmt.Sprint(a.Value)
		}
		n, err = m.store.Forget(a.Subject, a.Predicate, value)
	}
	if err != nil {
		return "", err
	}
	return marshal(map[string]any{"withdrawn": n})
}

func (m *memoryRuntime) toolHistory(args json.RawMessage) (string, error) {
	var a struct {
		Subject   string `json:"subject"`
		Predicate string `json:"predicate"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	var events []recall.Event
	var err error
	if m.client != nil {
		events, err = m.client.History(m.ctx, a.Subject, a.Predicate)
	} else {
		events, err = m.store.History(a.Subject, a.Predicate)
	}
	if err != nil {
		return "", err
	}
	return marshalWithoutEvidence(events)
}

// memoryRuntime holds immutable configuration. Each HTTP-backed call gets
// its own store and adapter, including context and embedding-error state.
type memoryRuntime struct {
	client   *recall.Client
	backend  recall.Backend
	ctx      context.Context
	store    *recall.Store
	newStore func(context.Context) *recall.Store
	// extractor proposes statements for text remembered without any; nil
	// unless the server was given a model.
	extractor recall.Extractor
	// open lets writes define predicates (recall mcp -open or -text).
	open bool
	// text serves the text-only tools (recall mcp -text).
	text bool
}

func (m *memoryRuntime) forRequest(ctx context.Context) *memoryRuntime {
	if m.client != nil {
		return &memoryRuntime{client: m.client, ctx: ctx, extractor: m.extractor, open: m.open, text: m.text}
	}
	if m.newStore == nil {
		return m
	}
	return &memoryRuntime{store: m.newStore(ctx)}
}

// newMemoryRuntime wires recall to the database c names.
//
// With open, writes may define predicates and the registry only seeds the
// vocabulary.
func newMemoryRuntime(c *api, collection, predicatesPath, embedURL string, open bool) (*memoryRuntime, error) {
	registry := recall.DefaultRegistry()
	if open && predicatesPath == "" {
		registry = openSeeds()
	}
	if predicatesPath != "" {
		raw, err := readFileTrimmed(predicatesPath)
		if err != nil {
			return nil, fmt.Errorf("predicates: %w", err)
		}
		registry, err = recall.LoadRegistry(raw)
		if err != nil {
			return nil, err
		}
	}
	var embedder recall.Embedder = recall.LexicalEmbedder{}
	if embedURL != "" {
		embedder = newRemoteEmbedder(embedURL, 0)
	}
	name := c.backend
	if name == "" {
		name = setupBackend
	}
	backend, err := recall.OpenBackend(name, recall.BackendOptions{URL: c.base, APIKey: c.key})
	if err != nil {
		return nil, err
	}
	client, err := recall.NewClient(recall.Config{Backend: backend, Collection: collection, Registry: registry, Embedder: embedder, Materialize: true, Open: open})
	if err != nil {
		return nil, err
	}
	return &memoryRuntime{client: client, backend: backend, ctx: context.Background(), open: open}, nil
}

// newExtractor builds the model extractor named by spec, such as
// "anthropic:claude-opus-5-5"; an empty spec means none.
func newExtractor(spec string) (recall.Extractor, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	cfg, err := model.Parse(spec)
	if err != nil {
		return nil, err
	}
	return model.NewExtractor(cfg)
}

// memoryInstructions describes the memory tools for the client's system
// prompt, including the predicate vocabulary, which is closed.
func (s *mcpServer) memoryInstructions() string {
	if !s.memoryEnabled() {
		return ""
	}
	if s.memory.text {
		return s.textInstructions()
	}
	var b strings.Builder
	b.WriteString("\n\nThis server also remembers things durably, in collection " + s.collection + ".\n")
	if s.write {
		b.WriteString("Use remember when the user states a preference or a fact worth keeping. ")
		if s.memory.extractor != nil {
			b.WriteString("To keep a passage of conversation, pass it to remember as text alone; the server works out the statements in it. ")
		}
	}
	b.WriteString("Use recall before ")
	b.WriteString("assuming anything about them. Supersession is automatic: stating a new value for a ")
	b.WriteString("single-valued predicate replaces the old one. Recall returns the current value with the one it ")
	b.WriteString("replaced under replaced; tell the user about a correction when it matters to the answer. The full ")
	b.WriteString("chain stays readable through memory_history. Nothing is deleted, so recall with as_of answers what ")
	b.WriteString("was believed earlier.\n")
	if s.memory.open {
		b.WriteString("These predicates are in use. Reuse one when it fits; otherwise name a new one in snake_case, and it is defined by its first use:\n")
	} else {
		b.WriteString("Predicates are a closed set. These are the ones that exist:\n")
	}
	b.WriteString(s.memory.registry().PromptTable())
	b.WriteString("When a statement is worth keeping and no other predicate fits, remember it with predicate " +
		recall.NotePredicate + " and the statement in the user's words as the value. Never drop it. " +
		"Recall returns typed beliefs ahead of notes.\n")
	return b.String()
}

// readFileTrimmed reads a file, trimming surrounding whitespace so that a
// registry saved with a trailing newline still parses.
func readFileTrimmed(path string) ([]byte, error) {
	raw, err := os.ReadFile(path) // #nosec G703 -- The operator selects the registry path at startup; tool calls cannot choose it.
	if err != nil {
		return nil, err
	}
	return bytes.TrimSpace(raw), nil
}

func (m *memoryRuntime) registry() recall.Registry {
	if m.client != nil && m.open {
		ctx := m.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if vocab, err := m.client.Vocabulary(ctx); err == nil {
			return vocab
		}
	}
	if m.client != nil {
		return m.client.Registry()
	}
	return m.store.Registry()
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (m *memoryRuntime) toolExplain(args json.RawMessage) (string, error) {
	var a struct {
		Question string `json:"question"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	if m.client == nil {
		return "", fmt.Errorf("explain needs the context-aware memory client")
	}
	explanations, err := m.client.Explain(m.ctx, a.Question)
	if err != nil {
		return "", err
	}
	return marshal(explanations)
}

// openSeeds is the vocabulary an open server starts from when no predicates
// file is given: the coding starter, for the agents Recall began with, and
// the personal one, for assistants that remember a person. Generic seeds are
// what keep an extractor from coining one predicate per fact; where both
// starters define a name, the coding one is kept.
func openSeeds() recall.Registry {
	seeds := recall.DefaultRegistry()
	personal, _ := recall.StarterRegistry(recall.StarterPersonal)
	for name, p := range personal {
		if _, ok := seeds[name]; !ok {
			seeds[name] = p
		}
	}
	return seeds
}
