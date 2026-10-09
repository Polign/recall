package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Polign/recall"
)

// The agent tools let a harness resume an agent from its own records instead
// of a snapshot: agent_resume takes the agent's lease and returns the context
// to start from, and the rest write what the next resume will read. The
// session holds each resumed agent, and its lease, until agent_release or
// until the client closes the session.
//
// They are off unless -agent is given, because most memory clients (an
// editor plugin, a chat app) never resume anything and should not see them.

var agentTools = map[string]bool{
	"agent_resume": true, "agent_acquire": true, "agent_release": true, "update_working_state": true, "agent_milestone": true,
	"record_turn": true, "recent_turns": true, "store_output": true, "fetch_output": true,
	"set_pointer": true, "remove_pointer": true, "list_pointers": true, "working_state_history": true,
}

type agentRuntime struct {
	mu     sync.Mutex
	agents map[string]*recall.Agent
}

func newAgentRuntime() *agentRuntime { return &agentRuntime{agents: map[string]*recall.Agent{}} }

func (r *agentRuntime) held(id string) (*recall.Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.agents[id]
	if a == nil {
		return nil, fmt.Errorf("agent %q is not resumed in this session; call agent_resume first", id)
	}
	return a, nil
}

// releaseAll hands every lease over when the session ends, so the next
// process need not wait out the TTL.
func (r *agentRuntime) releaseAll() {
	r.mu.Lock()
	agents := r.agents
	r.agents = map[string]*recall.Agent{}
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, a := range agents {
		_ = a.Release(ctx)
	}
}

func (s *mcpServer) agentTools() []mcpTool {
	if s.agents == nil {
		return nil
	}
	id := map[string]any{"type": "string", "description": "the agent's id: 1-128 letters, digits, '.', '_' or '-'"}
	strs := func(desc string) map[string]any {
		return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	tools := []mcpTool{
		{
			Name:  "agent_resume",
			Title: "Resume an agent",
			Description: "Take the agent's lease and rebuild the context it should start from: its working state, pointers to its work, " +
				"relevant memories and its most recent turns, within token_budget. Returns the parts and a briefing to hand the model. " +
				"Fails if another process holds the agent. Also how an agent starts for the first time (fresh is then true).",
			InputSchema: schema(map[string]any{
				"agent_id":          id,
				"token_budget":      map[string]any{"type": "integer", "description": "bound on the assembled context, in tokens (default 8000)"},
				"lease_ttl_seconds": map[string]any{"type": "integer", "description": "how long each lease epoch lasts (default 60); the session renews it in the background"},
				"holder":            str("names this process in lease records (default host:pid:random)"),
				"output_threshold":  map[string]any{"type": "integer", "description": "turns larger than this many tokens are stored as outputs (default 2000; negative disables)"},
				"defer_lease":       map[string]any{"type": "boolean", "description": "return the context at once without taking the lease, even while a crashed process's lease is live; writes then fail until agent_acquire succeeds"},
			}, "agent_id"),
		},
		{
			Name:  "agent_acquire",
			Title: "Take a deferred agent's lease",
			Description: "Take the lease for an agent resumed with defer_lease. Tries once and fails while another process holds it; retry until it succeeds. " +
				"Returns acquired and the epoch. Writes made by the previous holder after the resume are picked up, not overwritten.",
			InputSchema: schema(map[string]any{"agent_id": id}, "agent_id"),
		},
		{
			Name:        "agent_release",
			Title:       "Release an agent",
			Description: "Hand the agent's lease over at once, so the next process can resume it without waiting out the TTL. Call on clean shutdown.",
			InputSchema: schema(map[string]any{"agent_id": id}, "agent_id"),
		},
		{
			Name:  "update_working_state",
			Title: "Update your working state",
			Description: "Supersede your working state: the note your next instance resumes from. Fields you give replace the current ones; " +
				"fields you omit are kept. Write it when your plan or progress changes, not after every step, as a colleague taking over " +
				"would need it: the goal, the plan, what is done, what you are doing now, decisions with their reasons, and open questions.",
			InputSchema: schema(map[string]any{
				"agent_id":       id,
				"goal":           str("what the whole task is for"),
				"plan":           strs("the remaining steps, in order"),
				"progress":       str("what is done so far"),
				"focus":          str("what you are doing right now"),
				"decisions":      strs("decisions made, each with its reason"),
				"open_questions": strs("what is still unknown"),
				"notes":          str("anything else the next instance needs"),
				"step":           map[string]any{"type": "integer", "description": "the harness's step counter"},
			}, "agent_id"),
		},
		{
			Name:        "agent_milestone",
			Title:       "Declare a milestone",
			Description: "Record that you reached a durable point worth keeping (tests pass, a section is drafted), not every step. A crash costs at most the work since the last milestone.",
			InputSchema: schema(map[string]any{"agent_id": id, "name": str("the milestone reached"), "progress": str("what is now done")}, "agent_id", "name"),
		},
		{
			Name:        "record_turn",
			Title:       "Record a turn",
			Description: "Append one message of the run, verbatim, so a resume can replay the last few. Content over the output threshold is stored as an output and the turn keeps a reference.",
			InputSchema: schema(map[string]any{
				"agent_id":   id,
				"role":       map[string]any{"type": "string", "enum": []string{"system", "user", "assistant", "tool"}},
				"content":    str("the message text"),
				"name":       str("the tool, for a tool turn"),
				"message_id": str("your harness's id for this message, so a restored history can tell recorded messages from new ones"),
				"brief":      str("a shorter form of content for the resume briefing, such as a tool call with long arguments elided; the record keeps content whole"),
			}, "agent_id", "role", "content"),
		},
		{
			Name:        "recent_turns",
			Title:       "Read recent turns",
			Description: "The newest turns, oldest first.",
			InputSchema: schema(map[string]any{"agent_id": id, "limit": map[string]any{"type": "integer", "description": "how many (default 20)"}}, "agent_id"),
		},
		{
			Name:        "store_output",
			Title:       "Store a large output",
			Description: "Keep a large tool output whole, to fetch by reference later instead of carrying it in context.",
			InputSchema: schema(map[string]any{"agent_id": id, "content": str("the output"), "tool": str("the tool that produced it"), "summary": str("one line on what it holds"), "ref": str("a ref to store it under; omitted assigns one")}, "agent_id", "content"),
		},
		{
			Name:        "fetch_output",
			Title:       "Fetch a stored output",
			Description: "Return a stored output by its ref, in full.",
			InputSchema: schema(map[string]any{"agent_id": id, "ref": str("the output's ref")}, "agent_id", "ref"),
		},
		{
			Name:  "set_pointer",
			Title: "Point to your work",
			Description: "Record where a piece of your work lives, replacing any pointer of the same name. Types and their fields: " +
				"git_ref (repo, branch, sha), object (uri), env (image, lockfile_hash, setup), external (kind, id, url), process (command, port).",
			InputSchema: schema(map[string]any{
				"agent_id": id,
				"name":     str("a stable name for this piece of work"),
				"type":     map[string]any{"type": "string", "enum": []string{"git_ref", "object", "env", "external", "process"}},
				"fields":   map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				"note":     str("what it is"),
			}, "agent_id", "name", "type", "fields"),
		},
		{
			Name:        "remove_pointer",
			Title:       "Remove a pointer",
			Description: "Withdraw a pointer that no longer applies.",
			InputSchema: schema(map[string]any{"agent_id": id, "name": str("the pointer's name")}, "agent_id", "name"),
		},
		{
			Name:        "list_pointers",
			Title:       "List pointers",
			Description: "Every current pointer to the agent's work.",
			InputSchema: schema(map[string]any{"agent_id": id}, "agent_id"),
		},
		{
			Name:        "working_state_history",
			Title:       "Read working state history",
			Description: "Earlier versions of the working state, newest first.",
			InputSchema: schema(map[string]any{"agent_id": id, "limit": map[string]any{"type": "integer", "description": "how many (default 20)"}}, "agent_id"),
		},
	}
	for i := range tools {
		switch tools[i].Name {
		case "recent_turns", "fetch_output", "list_pointers", "working_state_history":
			tools[i].Annotations = readOnlyTool
		}
	}
	return tools
}

func (s *mcpServer) runAgentTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	var base struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(args, &base); err != nil {
		return "", err
	}
	if name == "agent_resume" {
		return s.toolAgentResume(ctx, args)
	}
	if name == "agent_acquire" {
		a, err := s.agents.held(base.AgentID)
		if err != nil {
			return "", err
		}
		if err := a.AcquireLease(ctx); err != nil {
			return "", err
		}
		return marshal(map[string]any{"acquired": true, "epoch": a.Epoch()})
	}
	if name == "agent_release" {
		s.agents.mu.Lock()
		a := s.agents.agents[base.AgentID]
		delete(s.agents.agents, base.AgentID)
		s.agents.mu.Unlock()
		if a == nil {
			return marshal(map[string]any{"released": false})
		}
		if err := a.Release(ctx); err != nil {
			return "", err
		}
		return marshal(map[string]any{"released": true})
	}
	a, err := s.agents.held(base.AgentID)
	if err != nil {
		return "", err
	}
	switch name {
	case "update_working_state":
		return toolUpdateWorkingState(ctx, a, args)
	case "agent_milestone":
		var in struct{ Name, Progress string }
		if err := json.Unmarshal(args, &in); err != nil {
			return "", err
		}
		return marshalResult(a.Milestone(ctx, in.Name, in.Progress))
	case "record_turn":
		var t recall.Turn
		if err := json.Unmarshal(args, &t); err != nil {
			return "", err
		}
		return marshalResult(a.RecordTurn(ctx, recall.Turn{Role: t.Role, Name: t.Name, Content: t.Content, MessageID: t.MessageID, Brief: t.Brief}))
	case "recent_turns":
		var in struct{ Limit int }
		if err := json.Unmarshal(args, &in); err != nil {
			return "", err
		}
		if in.Limit <= 0 {
			in.Limit = 20
		}
		turns, err := a.RecentTurns(ctx, min(in.Limit, 200))
		if turns == nil {
			turns = []recall.Turn{}
		}
		return marshalResult(turns, err)
	case "store_output":
		var o recall.Output
		if err := json.Unmarshal(args, &o); err != nil {
			return "", err
		}
		return marshalResult(a.StoreOutput(ctx, recall.Output{Ref: o.Ref, Tool: o.Tool, Summary: o.Summary, Content: o.Content}))
	case "fetch_output":
		var in struct{ Ref string }
		if err := json.Unmarshal(args, &in); err != nil {
			return "", err
		}
		return marshalResult(a.FetchOutput(ctx, in.Ref))
	case "set_pointer":
		var p recall.Pointer
		if err := json.Unmarshal(args, &p); err != nil {
			return "", err
		}
		return marshalResult(a.SetPointer(ctx, recall.Pointer{Name: p.Name, Type: p.Type, Fields: p.Fields, Note: p.Note}))
	case "remove_pointer":
		var in struct{ Name string }
		if err := json.Unmarshal(args, &in); err != nil {
			return "", err
		}
		if err := a.RemovePointer(ctx, in.Name); err != nil {
			return "", err
		}
		return marshal(map[string]any{"removed": in.Name})
	case "list_pointers":
		ps, err := a.Pointers(ctx)
		if ps == nil {
			ps = []recall.Pointer{}
		}
		return marshalResult(ps, err)
	case "working_state_history":
		var in struct{ Limit int }
		if err := json.Unmarshal(args, &in); err != nil {
			return "", err
		}
		return marshalResult(a.WorkingStateHistory(ctx, in.Limit))
	}
	return "", fmt.Errorf("unknown agent tool %q", name)
}

func (s *mcpServer) toolAgentResume(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		AgentID         string `json:"agent_id"`
		TokenBudget     int    `json:"token_budget"`
		LeaseTTLSeconds int    `json:"lease_ttl_seconds"`
		Holder          string `json:"holder"`
		OutputThreshold int    `json:"output_threshold"`
		DeferLease      bool   `json:"defer_lease"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	s.agents.mu.Lock()
	_, already := s.agents.agents[in.AgentID]
	s.agents.mu.Unlock()
	if already {
		return "", fmt.Errorf("agent %q is already resumed in this session; call agent_release first to resume it again", in.AgentID)
	}
	// The agent outlives this call: its lease keep-alive and later writes
	// run on the session, not on the request that started it.
	a, err := s.memory.client.Resume(context.WithoutCancel(ctx), recall.ResumeRequest{
		AgentID:         in.AgentID,
		TokenBudget:     in.TokenBudget,
		LeaseTTL:        time.Duration(in.LeaseTTLSeconds) * time.Second,
		Holder:          in.Holder,
		OutputThreshold: in.OutputThreshold,
		Deferred:        in.DeferLease,
	})
	if err != nil {
		return "", err
	}
	s.agents.mu.Lock()
	if s.agents.agents[in.AgentID] != nil {
		s.agents.mu.Unlock()
		_ = a.Release(ctx)
		return "", fmt.Errorf("agent %q was resumed concurrently in this session", in.AgentID)
	}
	s.agents.agents[in.AgentID] = a
	s.agents.mu.Unlock()
	return marshal(a.Resumed())
}

// toolUpdateWorkingState merges: a field present in the call replaces the
// current one, and an absent field is kept.
func toolUpdateWorkingState(ctx context.Context, a *recall.Agent, args json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return "", err
	}
	delete(fields, "agent_id")
	allowed := map[string]bool{"goal": true, "plan": true, "progress": true, "focus": true, "decisions": true, "open_questions": true, "notes": true, "step": true}
	var unknown []string
	for k := range fields {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return "", fmt.Errorf("update_working_state does not take %v", unknown)
	}
	ws := a.WorkingState()
	merged, err := json.Marshal(ws)
	if err != nil {
		return "", err
	}
	var current map[string]json.RawMessage
	if err := json.Unmarshal(merged, &current); err != nil {
		return "", err
	}
	for k, v := range fields {
		current[k] = v
	}
	merged, err = json.Marshal(current)
	if err != nil {
		return "", err
	}
	var next recall.WorkingState
	if err := json.Unmarshal(merged, &next); err != nil {
		return "", err
	}
	return marshalResult(a.UpdateWorkingState(ctx, next))
}

func marshalResult[T any](v T, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return marshal(v)
}
