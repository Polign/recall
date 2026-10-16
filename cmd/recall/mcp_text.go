package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Polign/recall"
)

// The text-only surface (recall mcp -text). An agent passes what was said and
// asks in words; the server works out the statements, files them, and tells
// answers back as sentences. No predicate appears in a tool, an argument, an
// instruction, or an answer.

func (s *mcpServer) textTools() []mcpTool {
	tools := []mcpTool{
		{
			Name:        "remember",
			Title:       "Remember",
			Description: "Keep something worth remembering about the user or their work: a preference, a fact, a decision, or a correction. Pass the passage as text, in the user's words where you can. The server works out what it says. A newer statement replaces an older one about the same thing, and the older one stays in the history. Restating something already known changes nothing.",
			InputSchema: schema(map[string]any{
				"text": map[string]any{"type": "string", "maxLength": 32768, "description": "what to remember"},
				"observed_at": map[string]any{
					"type":        "string",
					"description": "when it was said, as an RFC3339 instant like 2023-05-20T02:21:00Z; omit for now. Use it only for something said earlier, such as an imported conversation.",
				},
			}, "text"),
		},
		{
			Name:        "recall",
			Title:       "Recall",
			Description: "Ask what is remembered, in words, before assuming anything about the user. Answers are what is believed now, as sentences. Each lists under before what it replaced and when, so a correction comes with the answer, and under elsewhere anything else believed about the same thing in other words, such as a later dislike beside a like; the newer statement usually holds. days_ago says how long ago it was stated. A time named in the question, such as \"last week\", is searched first. Pass as_of to ask what was believed at an earlier instant. Leave out question to get everything believed about the user, a profile to start a conversation with. Pass since to get only what changed after then, newest first: at the start of a session, since the last one, it says what is new.",
			InputSchema: schema(map[string]any{
				"question": map[string]any{"type": "string", "description": "what you want to know; omit for the user's profile, or with since for everything that changed"},
				"as_of": map[string]any{
					"type":        "string",
					"description": "RFC3339 instant, e.g. 2026-09-01T00:00:00Z. Answers as of then rather than now.",
				},
				"since": map[string]any{
					"type":        "string",
					"description": "RFC3339 instant. Only what was stated after then, each with what it replaced.",
				},
			}),
			Annotations: readOnlyTool,
		},
		{
			Name:        "forget",
			Title:       "Forget",
			Description: "Stop believing what a description names, such as \"my editor\" or \"that I live in Lisbon\". Returns what was withdrawn. Nothing is deleted: it stays in the history, and remembering it again brings it back.",
			InputSchema: schema(map[string]any{
				"text": map[string]any{"type": "string", "description": "what to forget, described in words"},
			}, "text"),
		},
	}
	if s.write {
		return tools
	}
	return tools[1:2]
}

func (s *mcpServer) textInstructions() string {
	var b strings.Builder
	b.WriteString("\n\nThis server remembers things durably, in collection " + s.collection + ".\n")
	if s.write {
		b.WriteString("Use remember when the user states a preference or a fact worth keeping, makes a decision, or corrects something; pass the passage as text. ")
		if s.memory.extractor == nil {
			b.WriteString("This server has no extraction model, so remembered text is kept as written: recall finds it by its words, but nothing replaces anything. Start the server with -extract-model to have it work out the facts. ")
		}
	}
	b.WriteString("Use recall before assuming anything about the user. A newer statement replaces an older one, and recall shows what it replaced under before; mention a correction when it matters to the answer. ")
	if s.write {
		b.WriteString("Use forget when the user asks you to forget something. ")
	}
	b.WriteString("Nothing is deleted, so recall with as_of answers what was believed earlier.\n")
	return b.String()
}

func (m *memoryRuntime) runTextTool(name string, args json.RawMessage) (string, error) {
	if m.client == nil {
		return "", fmt.Errorf("the text tools need the context-aware memory client")
	}
	switch name {
	case "remember":
		return m.textRemember(args)
	case "recall":
		return m.textRecall(args)
	case "forget":
		return m.textForget(args)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

// remembered is one statement a remember call filed, told in words.
type remembered struct {
	Text     string   `json:"text"`
	Replaces []string `json:"replaces,omitempty"`
	Known    bool     `json:"already_known,omitempty"`
}

func (m *memoryRuntime) textRemember(args json.RawMessage) (string, error) {
	var a struct {
		Text       string `json:"text"`
		ObservedAt string `json:"observed_at"`
	}
	if err := strictArgs(args, &a); err != nil {
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
	if m.extractor == nil {
		// Without a model nothing can be worked out of the text, so it is
		// kept as written: searchable, but it replaces nothing.
		confidence := 0.5
		if _, err := m.client.Remember(m.ctx, recall.RememberRequest{Subject: recall.DefaultSubject, Predicate: recall.NotePredicate, Value: a.Text, Source: "agent_inferred", Confidence: &confidence, ObservedAt: observedAt}); err != nil {
			return "", err
		}
		return marshal(map[string]any{"kept_as_text": true, "note": "This server has no extraction model, so the text was kept as written. Recall finds it by its words, but it does not replace anything remembered earlier."})
	}
	result, err := m.client.RememberTextAt(m.ctx, a.Text, m.extractor, observedAt)
	filed := make([]remembered, 0, len(result.Results))
	for _, r := range result.Results {
		if r.Stored.Predicate == recall.NotePredicate {
			continue
		}
		item := remembered{Text: recall.Sentence(r.Stored.Subject, r.Stored.Predicate, r.Stored.Value), Known: r.Existing}
		for _, old := range r.Superseded {
			item.Replaces = append(item.Replaces, recall.Sentence(old.Subject, old.Predicate, old.Value))
		}
		filed = append(filed, item)
	}
	out := map[string]any{"remembered": filed, "kept_as_text": result.Episode != nil}
	if err != nil {
		out["error"] = err.Error()
		encoded, _ := marshal(out)
		return "", fmt.Errorf("%s", encoded)
	}
	return marshal(out)
}

// recalled is one memory as the text surface returns it.
type recalled struct {
	Text    string    `json:"text"`
	Since   string    `json:"since"`
	DaysAgo int       `json:"days_ago"`
	Before  []earlier `json:"before,omitempty"`
	// Elsewhere is what else is believed about the same thing in other words,
	// such as a later dislike beside a like.
	Elsewhere []earlier `json:"elsewhere,omitempty"`
}

type earlier struct {
	Text  string `json:"text"`
	Since string `json:"since"`
}

func (m *memoryRuntime) textRecall(args json.RawMessage) (string, error) {
	var a struct {
		Question string `json:"question"`
		AsOf     string `json:"as_of"`
		Since    string `json:"since"`
	}
	if err := strictArgs(args, &a); err != nil {
		return "", err
	}
	instant := func(name, v string) (time.Time, error) {
		if v == "" {
			return time.Time{}, nil
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, fmt.Errorf("%s must be an RFC3339 instant like 2026-09-01T00:00:00Z: %w", name, err)
		}
		return t, nil
	}
	asOf, err := instant("as_of", a.AsOf)
	if err != nil {
		return "", err
	}
	since, err := instant("since", a.Since)
	if err != nil {
		return "", err
	}
	memories, err := m.client.AskWith(m.ctx, recall.AskRequest{Question: a.Question, AsOf: asOf, Since: since})
	if err != nil {
		return "", err
	}
	ref := asOf
	if ref.IsZero() {
		ref = time.Now()
	}
	out := make([]recalled, 0, len(memories))
	for _, mem := range memories {
		r := recalled{Text: mem.Text, Since: day(mem.Since), DaysAgo: int(ref.Sub(mem.Since).Hours() / 24)}
		for _, e := range mem.Before {
			r.Before = append(r.Before, earlier{Text: e.Text, Since: day(e.Since)})
		}
		for _, e := range mem.Elsewhere {
			r.Elsewhere = append(r.Elsewhere, earlier{Text: e.Text, Since: day(e.Since)})
		}
		out = append(out, r)
	}
	return marshal(map[string]any{"memories": out})
}

func (m *memoryRuntime) textForget(args json.RawMessage) (string, error) {
	var a struct {
		Text string `json:"text"`
	}
	if err := strictArgs(args, &a); err != nil {
		return "", err
	}
	selector, ok := m.extractor.(recall.Selector)
	if !ok {
		return "", fmt.Errorf("forgetting by description needs an extraction model; start the server with -extract-model")
	}
	result, err := m.client.ForgetText(m.ctx, a.Text, selector)
	forgotten := make([]string, 0, len(result.Withdrawn))
	for _, b := range result.Withdrawn {
		forgotten = append(forgotten, recall.Sentence(b.Subject, b.Predicate, b.Value))
	}
	if err != nil {
		encoded, _ := marshal(map[string]any{"forgotten": forgotten, "error": err.Error()})
		return "", fmt.Errorf("%s", encoded)
	}
	return marshal(map[string]any{"forgotten": forgotten})
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// strictArgs decodes tool arguments, refusing fields the tool does not take,
// so a typed call sent to the text surface fails instead of being half read.
func strictArgs(args json.RawMessage, into any) error {
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("arguments: %w", err)
	}
	return nil
}
