package model

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Polign/recall"
)

// replacedCase is one question answered from a belief that replaced another.
type replacedCase struct {
	name      string
	subject   string
	predicate string
	// values are asserted in order, a day apart, so the last one is current
	// and the one before it is listed under replaced.
	values   []string
	question string
	options  []string
	want     string
}

var replacedCases = []replacedCase{
	{
		name: "revoked exception", subject: "customer-4812", predicate: "refund_exception",
		values:   []string{"approved", "revoked"},
		question: "Can I still use my refund exception?",
		options:  []string{"yes", "no"}, want: "no",
	},
	{
		name: "switched editor", subject: "user", predicate: "prefers_editor",
		values:   []string{"vim", "neovim"},
		question: "Open the config file in my usual editor. Which editor do you launch?",
		options:  []string{"vim", "neovim"}, want: "neovim",
	},
	{
		name: "moved address", subject: "customer-221", predicate: "shipping_address",
		values:   []string{"12 Pine St, Austin TX", "480 Lake Ave, Denver CO"},
		question: "Where should today's order ship?",
		options:  []string{"12 Pine St, Austin TX", "480 Lake Ave, Denver CO"}, want: "480 Lake Ave, Denver CO",
	},
	{
		name: "downgraded plan", subject: "account-77", predicate: "plan",
		values:   []string{"pro", "free"},
		question: "Does this account get the Pro export feature?",
		options:  []string{"yes", "no"}, want: "no",
	},
	{
		name: "moved standup", subject: "team-platform", predicate: "standup_time",
		values:   []string{"10:00", "14:00", "09:30"},
		question: "What time is standup?",
		options:  []string{"10:00", "14:00", "09:30"}, want: "09:30",
	},
	// The correction is only useful if the model can read it when asked.
	{
		name: "asked about the past", subject: "customer-4812", predicate: "refund_exception",
		values:   []string{"approved", "revoked"},
		question: "Was my refund exception ever approved?",
		options:  []string{"yes", "no"}, want: "yes",
	},
}

// TestLiveModelAnswersFromCurrentNotReplaced checks that putting the replaced
// value in front of a model does not make it answer with that value. It calls
// a real model, so it runs only when RECALL_LIVE_MODEL names one, such as
// ollama:qwen3:8b or anthropic:claude-haiku-4-5. Each case is one short request.
func TestLiveModelAnswersFromCurrentNotReplaced(t *testing.T) {
	spec := os.Getenv("RECALL_LIVE_MODEL")
	if spec == "" {
		t.Skip("set RECALL_LIVE_MODEL, such as ollama:qwen3:8b, to run against a real model")
	}
	cfg, err := Parse(spec)
	if err != nil {
		t.Fatal(err)
	}
	ex, err := NewExtractor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The prompt is what a plain agent would say. It does not explain
	// replaced, because the field has to read correctly without coaching.
	const system = "You are a support and coding assistant. Answer the user's question from the memory " +
		"records your recall tool returned. Reply with JSON choosing one of the allowed answers."
	for _, c := range replacedCases {
		t.Run(c.name, func(t *testing.T) {
			memory := recalledJSON(t, c)
			text := fmt.Sprintf("recall tool result for %s %s:\n%s\n\nUser: %s", c.subject, c.predicate, memory, c.question)
			schema := map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"answer": map[string]any{"type": "string", "enum": c.options}},
				"required":             []string{"answer"},
				"additionalProperties": false,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			raw, err := ex.c.complete(ctx, system, text, schema)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Answer string `json:"answer"`
			}
			if err := json.Unmarshal([]byte(jsonObject(raw)), &got); err != nil {
				t.Fatalf("reply %q: %v", raw, err)
			}
			if got.Answer != c.want {
				t.Errorf("answered %q, want %q, given:\n%s", got.Answer, c.want, memory)
			}
		})
	}
}

// recalledJSON folds the case's log and renders the beliefs the way the MCP
// recall tool does, replaced values included.
func recalledJSON(t *testing.T, c replacedCase) string {
	t.Helper()
	start := time.Date(2026, 9, 12, 15, 0, 0, 0, time.UTC)
	var events []recall.Event
	for i, v := range c.values {
		events = append(events, recall.Event{
			ID: fmt.Sprintf("e%d", i), Kind: "fact", Subject: c.subject, Predicate: c.predicate,
			Value: v, Confidence: 1, Source: "user_stated", ObservedAt: start.AddDate(0, 0, i),
		})
	}
	beliefs := recall.Fold(events, recall.Single, time.Time{})
	if len(beliefs) != 1 || len(beliefs[0].Replaced) != 1 {
		t.Fatalf("fixture folded to %+v, want one belief replacing one value", beliefs)
	}
	raw, err := json.Marshal(beliefs)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
