package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ResumeContext is what a resumed agent starts from: its own note, where its
// work lives, what it remembers that bears on its focus, and its last few
// turns, assembled within a token budget. Briefing renders all of it as one
// message a harness can hand the model; the fields carry the same content
// for harnesses that want to lay it out themselves.
type ResumeContext struct {
	AgentID string `json:"agent_id"`
	// Fresh means the agent had no records: this is its first start.
	Fresh bool   `json:"fresh"`
	Epoch uint64 `json:"epoch,omitempty"`
	// LeaseHeld is false after a deferred resume: the context is ready, but
	// writes wait for AcquireLease.
	LeaseHeld    bool          `json:"lease_held"`
	WorkingState *WorkingState `json:"working_state,omitempty"`
	Pointers     []Pointer     `json:"pointers,omitempty"`
	Outputs      []OutputRef   `json:"outputs,omitempty"`
	Memories     []Belief      `json:"memories,omitempty"`
	RecentTurns  []Turn        `json:"recent_turns,omitempty"`
	// Omitted counts what did not fit the budget.
	Omitted struct {
		Turns    int `json:"turns,omitempty"`
		Memories int `json:"memories,omitempty"`
		Outputs  int `json:"outputs,omitempty"`
	} `json:"omitted"`
	// TurnSeq is the newest turn recorded before this resume.
	TurnSeq     int64  `json:"turn_seq"`
	TokenBudget int    `json:"token_budget"`
	Tokens      int    `json:"tokens"`
	Briefing    string `json:"briefing"`
}

// maxMemories bounds how many memories resume retrieves before the budget
// trims them.
const maxMemories = 20

func (a *Agent) load(ctx context.Context, budget int) (ResumeContext, error) {
	if budget == 0 {
		budget = defaultTokenBudget
	}
	rc := ResumeContext{AgentID: a.id, Epoch: a.Epoch(), LeaseHeld: a.LeaseHeld(), TokenBudget: budget}

	heads, _, err := a.list(ctx, recWorkingHead, nil, 1)
	if err != nil {
		return rc, err
	}
	var ws WorkingState
	if len(heads) > 0 {
		if err := decodeBody(heads[0], &ws); err != nil {
			return rc, err
		}
	}
	latest, err := a.latestTurn(ctx, ws.TurnSeq)
	if err != nil {
		return rc, err
	}
	pointers, err := a.Pointers(ctx)
	if err != nil {
		return rc, err
	}
	outputs, err := a.latestOutputs(ctx)
	if err != nil {
		return rc, err
	}
	turns, err := a.recentTurns(ctx, latest, maxRecentTurns)
	if err != nil {
		return rc, err
	}
	memories, err := a.relevantMemories(ctx, ws)
	if err != nil {
		return rc, err
	}

	a.mu.Lock()
	a.state = ws
	a.turnSeq = latest
	a.mu.Unlock()

	rc.Fresh = len(heads) == 0 && latest == 0 && len(pointers) == 0 && len(outputs) == 0
	rc.TurnSeq = latest
	if len(heads) > 0 {
		w := cloneWorkingState(ws)
		rc.WorkingState = &w
	}
	rc.Pointers = pointers
	assemble(&rc, turns, memories, outputs)
	return rc, nil
}

// relevantMemories searches the memory collection with what the agent is
// doing now. Without an embedder, or with nothing to search for, it returns
// nothing rather than failing the resume.
func (a *Agent) relevantMemories(ctx context.Context, ws WorkingState) ([]Belief, error) {
	parts := []string{ws.Focus}
	parts = append(parts, ws.OpenQuestions...)
	if strings.TrimSpace(strings.Join(parts, "")) == "" {
		parts = []string{ws.Goal}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	if text == "" || a.c.embedder == nil {
		return nil, nil
	}
	beliefs, err := a.c.Recall(ctx, Query{Text: text, Limit: maxMemories})
	if err != nil && !isMissingCollection(err) && !errors.Is(err, ErrEmbedderRequired) {
		return nil, fmt.Errorf("recall: resume memories: %w", err)
	}
	return beliefs, nil
}

// assemble fills the budget in priority order. The header, the working state
// and the pointers are always included: without them the agent cannot know
// what it was doing. What is left is shared between recent turns (newest
// first), memories (best first) and output references, and a share one of
// them does not use goes to the others.
func assemble(rc *ResumeContext, turns []Turn, memories []Belief, outputs []OutputRef) {
	var b strings.Builder
	writeHeader(&b, rc)
	if rc.WorkingState != nil {
		writeWorkingState(&b, *rc.WorkingState)
	}
	if len(rc.Pointers) > 0 {
		b.WriteString("\n## Where your work lives\n")
		for _, p := range rc.Pointers {
			b.WriteString("- " + pointerText(p) + "\n")
		}
	}
	fixed := estimateTokens(b.String())
	left := max(rc.TokenBudget-fixed, 0)

	turnShare, memShare := left*4/10, left*4/10
	outShare := left - turnShare - memShare

	keptTurns, turnCost := fitTurns(turns, turnShare)
	keptMems, memCost := fitMemories(memories, memShare+turnShare-turnCost)
	keptOuts, outCost := fitOutputs(outputs, outShare+(memShare+turnShare-turnCost-memCost))
	// Anything still unused buys older turns.
	if spare := left - turnCost - memCost - outCost; spare > 0 && len(keptTurns) < len(turns) {
		keptTurns, turnCost = fitTurns(turns, turnCost+spare)
	}

	if len(keptOuts) > 0 {
		b.WriteString("\n## Stored tool outputs (fetch by ref when you need the detail)\n")
		for _, o := range keptOuts {
			fmt.Fprintf(&b, "- %s (%s, ~%d tokens): %s\n", o.Ref, orDash(o.Tool), o.Tokens, oneLine(o.Summary))
		}
	}
	if len(keptMems) > 0 {
		b.WriteString("\n## What you remember that bears on this\n")
		for _, m := range keptMems {
			b.WriteString("- " + beliefText(m) + "\n")
		}
	}
	if len(keptTurns) > 0 {
		b.WriteString("\n## Your most recent turns, verbatim\n")
		for _, t := range keptTurns {
			b.WriteString(turnText(t))
		}
	}

	rc.RecentTurns = keptTurns
	rc.Memories = keptMems
	rc.Outputs = keptOuts
	rc.Omitted.Turns = len(turns) - len(keptTurns)
	rc.Omitted.Memories = len(memories) - len(keptMems)
	rc.Omitted.Outputs = len(outputs) - len(keptOuts)
	rc.Briefing = b.String()
	rc.Tokens = estimateTokens(rc.Briefing)
}

func writeHeader(b *strings.Builder, rc *ResumeContext) {
	if rc.WorkingState == nil && rc.TurnSeq == 0 {
		fmt.Fprintf(b, "You are agent %s, starting for the first time. Nothing was recorded by an earlier run.\n", rc.AgentID)
		return
	}
	fmt.Fprintf(b, "You are agent %s, resuming work. Your previous process stopped; this is the note it left you, "+
		"pointers to its work, and its last turns. Treat it as a hint, not the truth: check that the world still "+
		"matches it before your first action, then update your working state.\n", rc.AgentID)
}

func writeWorkingState(b *strings.Builder, ws WorkingState) {
	fmt.Fprintf(b, "\n## Working state (version %d, step %d, written %s)\n", ws.Version, ws.Step, ws.UpdatedAt.UTC().Format(time.RFC3339))
	line := func(label, v string) {
		if v = strings.TrimSpace(v); v != "" {
			fmt.Fprintf(b, "%s: %s\n", label, v)
		}
	}
	list := func(label string, items []string, numbered bool) {
		if len(items) == 0 {
			return
		}
		b.WriteString(label + ":\n")
		for i, it := range items {
			if numbered {
				fmt.Fprintf(b, "%d. %s\n", i+1, it)
			} else {
				b.WriteString("- " + it + "\n")
			}
		}
	}
	line("Goal", ws.Goal)
	list("Plan", ws.Plan, true)
	line("Progress", ws.Progress)
	line("Last milestone", ws.LastMilestone)
	line("Current focus", ws.Focus)
	list("Decisions", ws.Decisions, false)
	list("Open questions", ws.OpenQuestions, false)
	line("Notes", ws.Notes)
}

func fitTurns(turns []Turn, budget int) ([]Turn, int) {
	cost, start := 0, len(turns)
	for i := len(turns) - 1; i >= 0; i-- {
		c := estimateTokens(turnText(turns[i]))
		if cost+c > budget {
			break
		}
		cost += c
		start = i
	}
	return turns[start:], cost
}

func fitMemories(ms []Belief, budget int) ([]Belief, int) {
	cost := 0
	for i, m := range ms {
		c := estimateTokens(beliefText(m)) + 1
		if cost+c > budget {
			return ms[:i], cost
		}
		cost += c
	}
	return ms, cost
}

func fitOutputs(os []OutputRef, budget int) ([]OutputRef, int) {
	cost := 0
	for i, o := range os {
		c := estimateTokens(o.Ref+o.Tool+oneLine(o.Summary)) + 8
		if cost+c > budget {
			return os[:i], cost
		}
		cost += c
	}
	return os, cost
}

func turnText(t Turn) string {
	who := t.Role
	if t.Name != "" {
		who += " " + t.Name
	}
	return fmt.Sprintf("[%s #%d] %s\n", who, t.Seq, t.Content)
}

func beliefText(m Belief) string {
	return fmt.Sprintf("%s %s: %v", m.Subject, m.Predicate, m.Value)
}

func pointerText(p Pointer) string {
	keys := make([]string, 0, len(p.Fields))
	for _, k := range []string{"repo", "branch", "sha", "uri", "image", "lockfile_hash", "setup", "kind", "id", "url", "command", "port"} {
		if v := p.Fields[k]; v != "" {
			keys = append(keys, k+"="+v)
		}
	}
	var extra []string
	for k, v := range p.Fields {
		if !knownPointerField(k) {
			extra = append(extra, k+"="+v)
		}
	}
	sort.Strings(extra)
	keys = append(keys, extra...)
	s := fmt.Sprintf("%s (%s): %s", p.Name, p.Type, strings.Join(keys, " "))
	if p.Note != "" {
		s += " (" + p.Note + ")"
	}
	return s
}

func knownPointerField(k string) bool {
	switch k {
	case "repo", "branch", "sha", "uri", "image", "lockfile_hash", "setup", "kind", "id", "url", "command", "port":
		return true
	}
	return false
}

func workingStateText(ws WorkingState) string {
	return strings.Join(append(append([]string{ws.Goal, ws.Focus, ws.Progress}, ws.Plan...), ws.OpenQuestions...), "\n")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
