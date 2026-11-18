package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Polign/recall"
)

// The agent tools let a harness resume an agent from its own records instead
// of a snapshot: agent_resume takes the agent's lease and returns the context
// to start from, and the rest write what the next resume will read.
// begin_action and end_action bracket each tool call that touches the world,
// so a resume knows which calls a crash left unfinished. The
// session holds each resumed agent, and its lease, until agent_release or
// until the client closes the session.
//
// They are off unless -agent is given, because most memory clients (an
// editor plugin, a chat app) never resume anything and should not see them.

var agentTools = map[string]bool{
	"agent_resume": true, "agent_acquire": true, "agent_release": true, "update_working_state": true, "agent_milestone": true,
	"record_turn": true, "recent_turns": true, "store_output": true, "fetch_output": true,
	"set_pointer": true, "remove_pointer": true, "list_pointers": true, "working_state_history": true,
	"begin_action": true, "end_action": true, "pending_actions": true, "agent_context": true, "agent_since": true,
}

type agentRuntime struct {
	mu     sync.Mutex
	agents map[string]*recall.Agent
	// epochs is the lease epoch handed to the caller when each agent was
	// resumed or acquired. A stateless HTTP caller presents it on every
	// write (If-Lease), and one this server did not issue for the agent it
	// holds is refused: a later resume issued a newer one.
	epochs map[string]uint64
	// sessions names the session that resumed each agent, when the caller's
	// credential carries one. recall serve keeps one runtime per namespace
	// for the life of the process, so the agent stays held after the pod
	// that resumed it is gone; the session is how a later pod tells it is
	// not the holder.
	sessions map[string]string
	// bound marks the agents held for a credential bound to them. Those are
	// released after idle with no call, and resumed again by the next call,
	// so a runtime built to hold many sleeping actors does not keep a lease
	// renewed for each.
	bound    map[string]bool
	lastUsed map[string]time.Time
	idle     time.Duration
	sweeping bool
	stop     chan struct{}
}

// newAgentRuntime holds the resumed agents of one session or namespace. idle
// is how long an agent held for a bound credential may go without a call
// before it is released; zero keeps it until the session ends.
func newAgentRuntime(idle time.Duration) *agentRuntime {
	return &agentRuntime{agents: map[string]*recall.Agent{}, sessions: map[string]string{}, epochs: map[string]uint64{},
		bound: map[string]bool{}, lastUsed: map[string]time.Time{}, idle: idle}
}

// touch records a call on an agent. It must be called with r.mu held.
func (r *agentRuntime) touchLocked(id string) {
	if r.agents[id] != nil {
		r.lastUsed[id] = time.Now()
	}
}

// forgetLocked drops an agent from every map. It must be called with r.mu
// held; the caller releases the agent.
func (r *agentRuntime) forgetLocked(id string) {
	delete(r.agents, id)
	delete(r.sessions, id)
	delete(r.epochs, id)
	delete(r.bound, id)
	delete(r.lastUsed, id)
}

// sweepLocked starts the idle release once a bound agent is held. It must
// be called with r.mu held.
func (r *agentRuntime) sweepLocked() {
	if r.sweeping || r.idle <= 0 {
		return
	}
	r.sweeping = true
	r.stop = make(chan struct{})
	every := max(r.idle/4, time.Second)
	go func(stop chan struct{}) {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.releaseIdle(time.Now().Add(-r.idle))
			}
		}
	}(r.stop)
}

// releaseIdle releases bound agents with no call since before.
func (r *agentRuntime) releaseIdle(before time.Time) {
	r.mu.Lock()
	var idle []*recall.Agent
	for id, bound := range r.bound {
		if bound && r.lastUsed[id].Before(before) {
			idle = append(idle, r.agents[id])
			r.forgetLocked(id)
		}
	}
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, a := range idle {
		if a != nil {
			_ = a.Release(ctx)
		}
	}
}

// sessionKey carries the caller's session from the HTTP front door to the
// agent tools. Over stdio there is none, and nothing changes.
type sessionKey struct{}

func withSession(ctx context.Context, session string) context.Context {
	if session == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKey{}, session)
}

func sessionFrom(ctx context.Context) string {
	s, _ := ctx.Value(sessionKey{}).(string)
	return s
}

func (r *agentRuntime) held(ctx context.Context, id string) (*recall.Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.agents[id]
	if a == nil {
		return nil, &notResumedError{agent: id}
	}
	if owner, caller := r.sessions[id], sessionFrom(ctx); owner != "" && caller != "" && owner != caller {
		return nil, &heldElsewhereError{agent: id, owner: owner, caller: caller}
	}
	r.touchLocked(id)
	return a, nil
}

// heldElsewhereError refuses a call on an agent another session resumed.
type heldElsewhereError struct{ agent, owner, caller string }

func (e *heldElsewhereError) Error() string {
	return fmt.Sprintf("agent %q was resumed by session %q; this session (%q) holds nothing: call agent_resume", e.agent, e.owner, e.caller)
}

// notResumedError refuses a call on an agent this server does not hold.
type notResumedError struct{ agent string }

func (e *notResumedError) Error() string {
	return fmt.Sprintf("agent %q is not resumed in this session; call agent_resume first", e.agent)
}

// alreadyResumedError refuses a second resume of an agent this session holds.
type alreadyResumedError struct{ agent string }

func (e *alreadyResumedError) Error() string {
	return fmt.Sprintf("agent %q is already resumed in this session; call agent_release first to resume it again", e.agent)
}

// checkEpoch compares the epoch a caller presents with the one this server
// issued for the agent it holds. It is how a request with no session of its
// own proves it is still the holder.
func (r *agentRuntime) checkEpoch(id string, epoch uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agents[id] == nil {
		return &notResumedError{agent: id}
	}
	issued := r.epochs[id]
	if issued == 0 {
		return recall.ErrLeaseNotHeld
	}
	if issued != epoch {
		return fmt.Errorf("%w: epoch %d is not the one issued for agent %q; a later agent_resume replaced it", recall.ErrLeaseLost, epoch, id)
	}
	return nil
}

// takeOver releases an agent held for another session, so that session's
// replacement can resume it. A credential fixed to one agent and one session
// is minted per pod, so a new session for the same agent means the pod that
// held it was replaced; its lease would otherwise be kept alive by this
// process forever. Releasing ends the old epoch, so a write still in flight
// from the old holder is fenced by the store. It returns the old session.
func (r *agentRuntime) takeOver(ctx context.Context, id string) (string, bool) {
	caller := sessionFrom(ctx)
	r.mu.Lock()
	a, owner := r.agents[id], r.sessions[id]
	if a == nil || caller == "" || owner == "" || owner == caller {
		r.mu.Unlock()
		return "", false
	}
	r.forgetLocked(id)
	r.mu.Unlock()
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = a.Release(releaseCtx)
	return owner, true
}

// releaseAll hands every lease over when the session ends, so the next
// process need not wait out the TTL.
func (r *agentRuntime) releaseAll() {
	r.mu.Lock()
	agents := r.agents
	r.agents = map[string]*recall.Agent{}
	r.epochs = map[string]uint64{}
	r.bound = map[string]bool{}
	r.lastUsed = map[string]time.Time{}
	if r.sweeping {
		close(r.stop)
		r.sweeping = false
	}
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
	at := map[string]any{"type": "string", "description": "the position your last answer carried, as turns.actions.state; " +
		"the write is refused with position_behind when the records have moved past it, which means your process was restored " +
		"from an earlier point: call agent_since with it"}
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
				"at":             at,
			}, "agent_id"),
		},
		{
			Name:        "agent_milestone",
			Title:       "Declare a milestone",
			Description: "Record that you reached a durable point worth keeping (tests pass, a section is drafted), not every step. A crash costs at most the work since the last milestone.",
			InputSchema: schema(map[string]any{"agent_id": id, "name": str("the milestone reached"), "progress": str("what is now done"), "at": at}, "agent_id", "name"),
		},
		{
			Name:  "record_turn",
			Title: "Record a turn",
			Description: "Append one message of the run, verbatim, so a resume can replay the last few. Content over the output threshold is stored as an output and the turn keeps a reference. " +
				"A message_id already recorded returns the recorded turn and writes nothing, so a retry is safe.",
			InputSchema: schema(map[string]any{
				"agent_id":   id,
				"role":       map[string]any{"type": "string", "enum": []string{"system", "user", "assistant", "tool"}},
				"content":    str("the message text"),
				"name":       str("the tool, for a tool turn"),
				"message_id": str("your harness's id for this message, so a restored history can tell recorded messages from new ones"),
				"brief":      str("a shorter form of content for the resume briefing, such as a tool call with long arguments elided; the record keeps content whole"),
				"at":         at,
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
			Name:  "begin_action",
			Title: "Record an action before it runs",
			Description: "Call before any tool call that touches the world, and run the tool only if this succeeds. If this process dies before " +
				"end_action, the next resume lists the action as unfinished, so it is checked instead of repeated. class says what " +
				"running it again would do: pure (reads only; safe to repeat), idempotent (repeating with the same key does nothing more; " +
				"pass the returned key to the tool), external (could happen twice: send, post, charge, delete; never repeated blindly). " +
				"Returns seq and key. With call_id, a call already begun is returned as it stands, with its status and result, instead of a new action.",
			InputSchema: schema(map[string]any{
				"agent_id": id,
				"tool":     str("the tool about to run"),
				"class":    map[string]any{"type": "string", "enum": []string{"pure", "idempotent", "external"}},
				"args":     str("the call's arguments, usually JSON"),
				"call_id":  str("your harness's id for this tool call, usually the model's tool call id"),
				"at":       at,
			}, "agent_id", "tool", "class"),
		},
		{
			Name:  "end_action",
			Title: "Record how an action ended",
			Description: "Close an action begun with begin_action: done or failed with its result, or, settling an unfinished action after a " +
				"resume, abandoned (it changed nothing) or unknown (still cannot tell). Ending it again with the same outcome returns it; a different outcome is refused.",
			InputSchema: schema(map[string]any{
				"agent_id":   id,
				"seq":        map[string]any{"type": "integer", "description": "the action's seq from begin_action"},
				"status":     map[string]any{"type": "string", "enum": []string{"done", "failed", "abandoned", "unknown"}},
				"result":     str("what the tool returned"),
				"error":      str("why it failed"),
				"resolution": str("for an unfinished action: how you settled it, such as found in the sent folder"),
				"at":         at,
			}, "agent_id", "seq", "status"),
		},
		{
			Name:        "pending_actions",
			Title:       "List unfinished actions",
			Description: "Actions begun and never ended, or ended unknown, oldest first. Settle each before doing anything else.",
			InputSchema: schema(map[string]any{"agent_id": id}, "agent_id"),
		},
		{
			Name:        "agent_context",
			Title:       "Rebuild the resume context",
			Description: "The context agent_resume returned, rebuilt from the records as they stand now: call it after settling unfinished actions to get a briefing without them.",
			InputSchema: schema(map[string]any{"agent_id": id}, "agent_id"),
		},
		{
			Name:        "working_state_history",
			Title:       "Read working state history",
			Description: "Earlier versions of the working state, newest first.",
			InputSchema: schema(map[string]any{"agent_id": id, "limit": map[string]any{"type": "integer", "description": "how many (default 20)"}}, "agent_id"),
		},
		{
			Name:  "agent_since",
			Title: "Catch up after a restore",
			Description: "What the records hold after a position: the turns, the actions in every status, and the working state if it changed, " +
				"with a briefing. Call it after position_behind with the position you had: those things happened while you do not remember " +
				"them, so do not do them again. Continue from the position it returns.",
			InputSchema: schema(map[string]any{
				"agent_id":     id,
				"at":           map[string]any{"type": "string", "description": "the position to report from, as turns.actions.state"},
				"token_budget": map[string]any{"type": "integer", "description": "bound on the briefing, in tokens (default 8000)"},
			}, "agent_id", "at"),
		},
	}
	for i := range tools {
		switch tools[i].Name {
		case "recent_turns", "fetch_output", "list_pointers", "working_state_history", "pending_actions", "agent_context", "agent_since":
			tools[i].Annotations = readOnlyTool
		}
	}
	return tools
}

func (s *mcpServer) runAgentTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	var base struct {
		AgentID string          `json:"agent_id"`
		At      json.RawMessage `json:"at"`
	}
	if err := json.Unmarshal(args, &base); err != nil {
		return "", err
	}
	at, err := parseAt(base.At)
	if err != nil {
		return "", err
	}
	if at != nil && !positionWrites[name] && name != "agent_since" {
		return "", fmt.Errorf("arguments: %s does not take at", name)
	}
	var opts []recall.WriteOption
	if at != nil && positionWrites[name] {
		opts = append(opts, recall.At(*at))
	}
	if name == "agent_resume" {
		out, a, err := s.toolAgentResume(ctx, args)
		if err != nil {
			return "", err
		}
		reportPosition(ctx, a.Position())
		return withPosition(out, a.Position()), nil
	}
	if name == "agent_acquire" {
		a, err := s.agents.held(ctx, base.AgentID)
		if err != nil {
			return "", err
		}
		if err := a.AcquireLease(ctx); err != nil {
			return "", err
		}
		rep, err := a.Reconcile(ctx, nil)
		if err != nil {
			return "", err
		}
		epoch := a.Epoch()
		s.agents.mu.Lock()
		s.agents.epochs[base.AgentID] = epoch
		s.agents.mu.Unlock()
		reportPosition(ctx, a.Position())
		return marshal(map[string]any{"acquired": true, "epoch": epoch, "pending_actions": nonNil(rep.Open), "briefing": a.Resumed().Briefing, "position": a.Position()})
	}
	if name == "agent_release" {
		// Releasing another session's agent would hand its lease away.
		var elsewhere *heldElsewhereError
		if _, err := s.agents.held(ctx, base.AgentID); errors.As(err, &elsewhere) {
			return "", err
		}
		s.agents.mu.Lock()
		a := s.agents.agents[base.AgentID]
		s.agents.forgetLocked(base.AgentID)
		s.agents.mu.Unlock()
		if a == nil {
			return marshal(map[string]any{"released": false})
		}
		if err := a.Release(ctx); err != nil {
			return "", err
		}
		return marshal(map[string]any{"released": true})
	}
	a, err := s.heldOrResume(ctx, base.AgentID)
	if err != nil {
		return "", err
	}
	out, err := s.agentCall(ctx, a, name, args, at, opts)
	if err != nil {
		return "", err
	}
	reportPosition(ctx, a.Position())
	return withPosition(out, a.Position()), nil
}

// heldOrResume returns the held agent. For a caller whose credential is
// bound to the agent, an agent the server does not hold (after a restart, or
// released while idle) is resumed in place: the credential already says who
// may act for it, so there is nothing for the caller to do.
func (s *mcpServer) heldOrResume(ctx context.Context, id string) (*recall.Agent, error) {
	a, err := s.agents.held(ctx, id)
	var notResumed *notResumedError
	if !errors.As(err, &notResumed) || !callerFrom(ctx).bound {
		return a, err
	}
	args, _ := json.Marshal(map[string]any{"agent_id": id})
	if _, a, err = s.toolAgentResume(ctx, args); err != nil {
		var already *alreadyResumedError
		if errors.As(err, &already) {
			return s.agents.held(ctx, id)
		}
		return nil, err
	}
	return a, nil
}

func (s *mcpServer) agentCall(ctx context.Context, a *recall.Agent, name string, args json.RawMessage, at *recall.Position, opts []recall.WriteOption) (string, error) {
	switch name {
	case "update_working_state":
		return toolUpdateWorkingState(ctx, a, args, opts)
	case "agent_milestone":
		var in struct{ Name, Progress string }
		if err := json.Unmarshal(args, &in); err != nil {
			return "", err
		}
		return marshalResult(a.Milestone(ctx, in.Name, in.Progress, opts...))
	case "record_turn":
		// Not recall.Turn: its "at" is the time recorded, and in the
		// arguments "at" is the caller's position.
		var t struct {
			Role, Name, Content, Brief string
			MessageID                  string `json:"message_id"`
		}
		if err := json.Unmarshal(args, &t); err != nil {
			return "", err
		}
		return marshalResult(a.RecordTurn(ctx, recall.Turn{Role: t.Role, Name: t.Name, Content: t.Content, MessageID: t.MessageID, Brief: t.Brief}, opts...))
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
	case "begin_action":
		var in struct {
			Tool, Class, Args string
			CallID            string `json:"call_id"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", err
		}
		if in.CallID != "" {
			opts = append(opts, recall.CallID(in.CallID))
		}
		return marshalResult(a.BeginAction(ctx, in.Tool, recall.ActionClass(in.Class), in.Args, opts...))
	case "end_action":
		var in struct {
			Seq                               int64
			Status, Result, Error, Resolution string
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", err
		}
		return marshalResult(a.EndAction(ctx, in.Seq, recall.ActionEnd{Status: recall.ActionStatus(in.Status), Result: in.Result, Error: in.Error, Resolution: in.Resolution}, opts...))
	case "agent_context":
		return marshalResult(a.Refresh(ctx))
	case "pending_actions":
		acts, err := a.PendingActions(ctx)
		return marshalResult(nonNil(acts), err)
	case "working_state_history":
		var in struct{ Limit int }
		if err := json.Unmarshal(args, &in); err != nil {
			return "", err
		}
		return marshalResult(a.WorkingStateHistory(ctx, in.Limit))
	case "agent_since":
		if at == nil {
			return "", fmt.Errorf("arguments: agent_since needs at, the position to report from")
		}
		var in struct {
			TokenBudget int `json:"token_budget"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return "", err
		}
		return marshalResult(a.Since(ctx, *at, in.TokenBudget))
	}
	return "", fmt.Errorf("unknown agent tool %q", name)
}

// toolAgentResume resumes an agent and returns its context. For a caller
// bound to the agent, an agent already held is not an error: its context is
// rebuilt and returned, since the credential, not the session, says who
// holds it.
func (s *mcpServer) toolAgentResume(ctx context.Context, args json.RawMessage) (string, *recall.Agent, error) {
	var in struct {
		AgentID         string `json:"agent_id"`
		TokenBudget     int    `json:"token_budget"`
		LeaseTTLSeconds int    `json:"lease_ttl_seconds"`
		Holder          string `json:"holder"`
		OutputThreshold int    `json:"output_threshold"`
		DeferLease      bool   `json:"defer_lease"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", nil, err
	}
	s.agents.takeOver(ctx, in.AgentID)
	s.agents.mu.Lock()
	held, already := s.agents.agents[in.AgentID]
	s.agents.touchLocked(in.AgentID)
	s.agents.mu.Unlock()
	if already && callerFrom(ctx).bound {
		if held.LeaseHeld() {
			if _, err := held.Reconcile(ctx, nil); err != nil {
				return "", nil, err
			}
		}
		rc, err := held.Refresh(ctx)
		if err != nil {
			return "", nil, err
		}
		out, err := marshal(rc)
		return out, held, err
	}
	if already {
		return "", nil, &alreadyResumedError{agent: in.AgentID}
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
		OnRepeat:        usage.Repeat,
	})
	if err != nil {
		return "", nil, err
	}
	s.agents.mu.Lock()
	if s.agents.agents[in.AgentID] != nil {
		s.agents.mu.Unlock()
		_ = a.Release(ctx)
		return "", nil, &alreadyResumedError{agent: in.AgentID}
	}
	s.agents.agents[in.AgentID] = a
	s.agents.lastUsed[in.AgentID] = time.Now()
	if session := sessionFrom(ctx); session != "" {
		s.agents.sessions[in.AgentID] = session
	}
	if callerFrom(ctx).bound {
		s.agents.bound[in.AgentID] = true
		s.agents.sweepLocked()
	}
	// The epoch in the answer is the caller's proof of holding on later
	// stateless requests. A deferred resume holds none yet; agent_acquire
	// issues it.
	resumed := a.Resumed()
	if resumed.LeaseHeld {
		s.agents.epochs[in.AgentID] = resumed.Epoch
	}
	s.agents.mu.Unlock()
	// Settle what can be settled without the outside world before the model
	// reads the briefing: interrupted pure actions are closed, and external
	// ones are marked unknown. Idempotent ones wait for the harness or the
	// model to run them again with their key. A deferred resume holds no
	// lease yet, so it reconciles in agent_acquire.
	if a.LeaseHeld() {
		if _, err := a.Reconcile(ctx, nil); err != nil {
			return "", nil, err
		}
	}
	out, err := marshal(a.Resumed())
	return out, a, err
}

// toolUpdateWorkingState merges: a field present in the call replaces the
// current one, and an absent field is kept.
func toolUpdateWorkingState(ctx context.Context, a *recall.Agent, args json.RawMessage, opts []recall.WriteOption) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return "", err
	}
	delete(fields, "agent_id")
	delete(fields, "at")
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
	return marshalResult(a.UpdateWorkingState(ctx, next, opts...))
}

// nonNil keeps an empty list a JSON [] rather than null.
func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func marshalResult[T any](v T, err error) (string, error) {
	if err != nil {
		return "", err
	}
	return marshal(v)
}
