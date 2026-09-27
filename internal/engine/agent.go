package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// An agent resumes from what it wrote down, not from a snapshot of its
// process. It keeps four kinds of record in a collection of its own:
//
//   - one working state: a handoff note to its next instance, superseded on
//     every update, with each earlier version kept;
//   - pointers to where its work lives (a branch, an object, an image);
//   - its turns, verbatim, so the last few can be replayed;
//   - large tool outputs, kept whole and fetched by reference.
//
// Resume takes the agent's lease, reads those records back, and assembles a
// context within a token budget. Only the lease holder may write, so two pods
// resuming the same agent cannot both act for it.
//
// Agent records never share a collection with memory events: every record in
// a memory collection must decode as an event, and an export or audit fails
// on anything else.

var (
	// ErrLeaseHeld means another process holds the agent's lease. Nothing was
	// written; the caller may wait and retry, or give up.
	ErrLeaseHeld = errors.New("recall: agent lease is held by another process")
	// ErrLeaseLost means another process took the agent over. The holder must
	// stop acting for it: every later write from this Agent fails.
	ErrLeaseLost = errors.New("recall: agent lease was lost to another process")
	// ErrLeaseUnsupported means the backend cannot grant leases. Resume with
	// Unleased only when something else guarantees one process per agent.
	ErrLeaseUnsupported = errors.New("recall: backend does not support agent leases")
	// ErrAgentClosed means Release already ran on this Agent.
	ErrAgentClosed = errors.New("recall: agent was released")
	// ErrLeaseNotHeld means a deferred resume has not taken the lease yet.
	// Nothing was written; call AcquireLease, then write.
	ErrLeaseNotHeld = errors.New("recall: agent lease not acquired yet; call AcquireLease before writing")
)

// LeaseGrant is one held epoch. The epoch is the fencing token: it only ever
// increases, and a holder presenting an old one has been superseded.
type LeaseGrant struct {
	Epoch  uint64
	Holder string
	TTL    time.Duration
}

// LeaseHeldError names who holds a lease the caller could not take.
type LeaseHeldError struct {
	Holder  string
	Expires time.Time
}

func (e *LeaseHeldError) Error() string {
	if e.Holder == "" {
		return ErrLeaseHeld.Error()
	}
	return fmt.Sprintf("%s: held by %q until %s", ErrLeaseHeld, e.Holder, e.Expires.UTC().Format(time.RFC3339))
}

func (e *LeaseHeldError) Unwrap() error { return ErrLeaseHeld }

// LeaseBackend optionally grants per-agent leases. Acquire fails with
// ErrLeaseHeld while another holder's epoch is live; Renew fails with
// ErrLeaseLost once the chain has moved past the given epoch.
type LeaseBackend interface {
	AcquireLease(ctx context.Context, collection, name, holder string, ttl time.Duration) (LeaseGrant, error)
	RenewLease(ctx context.Context, collection, name, holder string, epoch uint64, ttl time.Duration) (LeaseGrant, error)
	ReleaseLease(ctx context.Context, collection, name, holder string, epoch uint64) error
}

// WorkingState is the agent's note to its next instance. Resume always
// includes it, whole, ahead of everything else, so it should say what a
// colleague would need to pick the work up: the goal, the plan, what is done,
// what is being done now, and why things were decided the way they were.
type WorkingState struct {
	Goal          string   `json:"goal,omitempty"`
	Plan          []string `json:"plan,omitempty"`
	Progress      string   `json:"progress,omitempty"`
	Focus         string   `json:"focus,omitempty"`
	Decisions     []string `json:"decisions,omitempty"`
	OpenQuestions []string `json:"open_questions,omitempty"`
	Notes         string   `json:"notes,omitempty"`
	// Step is the harness's step counter when the note was written.
	Step int64 `json:"step,omitempty"`
	// LastMilestone names the last point the agent declared durable.
	LastMilestone string `json:"last_milestone,omitempty"`

	// Written by the library; ignored on input.
	Version   int64     `json:"version,omitempty"`
	TurnSeq   int64     `json:"turn_seq,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// Turn is one message of the agent's run, kept verbatim.
type Turn struct {
	Seq  int64  `json:"seq"`
	Role string `json:"role"`
	// Name is the tool that produced a tool turn.
	Name    string `json:"name,omitempty"`
	Content string `json:"content"`
	// OutputRef is set when the content was too large to keep in the turn and
	// was stored as an output instead; Content then holds its opening.
	OutputRef string `json:"output_ref,omitempty"`
	// MessageID is the harness's own id for the message, when it has one.
	// A harness that restores its message history after a resume uses it to
	// tell messages already recorded from new ones.
	MessageID string `json:"message_id,omitempty"`
	// Brief is a shorter form of Content for the resume briefing, when the
	// harness has one: a tool call with its long arguments elided, say. The
	// record keeps Content whole; only the briefing shows Brief instead.
	Brief string    `json:"brief,omitempty"`
	At    time.Time `json:"at"`
}

// Output is a large tool result, kept whole and retrieved by reference.
type Output struct {
	Ref     string    `json:"ref"`
	Tool    string    `json:"tool,omitempty"`
	Summary string    `json:"summary,omitempty"`
	Content string    `json:"content"`
	At      time.Time `json:"at"`
}

// OutputRef describes a stored output without its content.
type OutputRef struct {
	Ref     string    `json:"ref"`
	Tool    string    `json:"tool,omitempty"`
	Summary string    `json:"summary,omitempty"`
	Tokens  int       `json:"tokens"`
	At      time.Time `json:"at"`
}

// Pointer types. A pointer says where a piece of the agent's work lives; the
// work itself stays in the system that already persists it.
const (
	PointerGitRef   = "git_ref"
	PointerObject   = "object"
	PointerEnv      = "env"
	PointerExternal = "external"
	PointerProcess  = "process"
)

// pointerFields lists, per type, the fields at least one of which must be set.
var pointerFields = map[string][]string{
	PointerGitRef:   {"sha", "branch"},
	PointerObject:   {"uri"},
	PointerEnv:      {"image", "lockfile_hash", "setup"},
	PointerExternal: {"id", "url"},
	PointerProcess:  {"command"},
}

// Pointer is a typed reference to where some of the agent's work lives.
// Setting a pointer with an existing name replaces it.
type Pointer struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Fields map[string]string `json:"fields"`
	Note   string            `json:"note,omitempty"`
	At     time.Time         `json:"at"`
}

// ResumeRequest names the agent to resume and how to build its context.
type ResumeRequest struct {
	AgentID string
	// Collection holds the agent's records; empty means the memory
	// collection's name with "_agents" appended.
	Collection string
	// TokenBudget bounds the assembled context. Zero means 8000.
	TokenBudget int
	// LeaseTTL is how long each lease epoch lasts. Zero means 60 seconds.
	LeaseTTL time.Duration
	// Holder identifies this process in lease records. Empty means
	// host:pid:random.
	Holder string
	// Unleased skips the lease. Use it only on a backend without leases, when
	// something else already guarantees one process per agent.
	Unleased bool
	// Deferred reads the agent's records without taking its lease, so the
	// context is ready at once even while a crashed process's lease is still
	// live. Writes fail with ErrLeaseNotHeld until AcquireLease succeeds.
	// Reading needs no lease: only writes can conflict.
	Deferred bool
	// NoKeepAlive stops the background renewal. The lease is then renewed
	// only by writes, and an agent idle past its TTL can be taken over.
	NoKeepAlive bool
	// OutputThreshold is the size, in estimated tokens, above which a turn's
	// content is stored as an output and the turn keeps a reference. Zero
	// means 2000; negative disables it.
	OutputThreshold int
}

const (
	defaultTokenBudget     = 8000
	defaultOutputThreshold = 2000
	// turnPage groups turns so that the newest can be found without listing
	// them all: turn s lives on page (s-1)/turnPage.
	turnPage = 64
	// maxBriefBytes bounds a turn's short form for the briefing.
	maxBriefBytes = 4096
	// maxOutputBytes bounds one stored output.
	maxOutputBytes = 1 << 20
	// maxPointers bounds how many pointers one agent can hold.
	maxPointers = 1000
	// maxRecentTurns bounds how far back resume looks for verbatim turns.
	maxRecentTurns = 200
)

var agentIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
var recordNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

var validRoles = map[string]bool{"system": true, "user": true, "assistant": true, "tool": true}

// Record kinds, stored in the "record" metadata field.
const (
	recWorkingHead = "working_state_head"
	recWorking     = "working_state"
	recTurn        = "turn"
	recOutput      = "output"
	recPointer     = "pointer"
)

// Agent is a resumed agent. Its methods are safe for concurrent use; writes
// are refused once the lease is lost or Release has run.
type Agent struct {
	c          *Client
	id         string
	collection string
	threshold  int
	resumed    ResumeContext
	req        ResumeRequest

	mu      sync.Mutex
	state   WorkingState
	turnSeq int64
	outSeq  int64
	closed  bool

	lease *agentLease
}

// ID returns the agent's id.
func (a *Agent) ID() string { return a.id }

// Collection returns the collection holding the agent's records.
func (a *Agent) Collection() string { return a.collection }

// Resumed returns the context assembled when the agent was resumed.
func (a *Agent) Resumed() ResumeContext { return a.resumed }

// Epoch returns the current lease epoch, or zero for an agent that holds
// no lease.
func (a *Agent) Epoch() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lease == nil {
		return 0
	}
	return a.lease.epoch()
}

// WorkingState returns the current working state.
func (a *Agent) WorkingState() WorkingState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cloneWorkingState(a.state)
}

// Resume takes the agent's lease and rebuilds its context from its records.
// It is also how an agent starts for the first time: with nothing recorded,
// the context is marked Fresh and holds only relevant memories.
//
// The returned Agent holds the lease until Release. Call Release on a clean
// shutdown so the next process need not wait out the TTL.
func (c *Client) Resume(ctx context.Context, req ResumeRequest) (*Agent, error) {
	if ctx == nil {
		return nil, fmt.Errorf("recall: context must not be nil")
	}
	if !agentIDPattern.MatchString(req.AgentID) {
		return nil, fmt.Errorf("recall: agent id must be 1-128 letters, digits, '.', '_' or '-', got %q", req.AgentID)
	}
	collection := strings.TrimSpace(req.Collection)
	if collection == "" {
		collection = c.collection + "_agents"
	}
	if collection == c.collection {
		return nil, fmt.Errorf("recall: agent records need their own collection, not the memory collection %q", c.collection)
	}
	if req.Unleased && req.Deferred {
		return nil, fmt.Errorf("recall: a resume is either unleased or deferred, not both")
	}
	if req.TokenBudget < 0 || req.LeaseTTL < 0 {
		return nil, fmt.Errorf("recall: token budget and lease ttl must not be negative")
	}
	threshold := req.OutputThreshold
	if threshold == 0 {
		threshold = defaultOutputThreshold
	}
	a := &Agent{c: c, id: req.AgentID, collection: collection, threshold: threshold, req: req}

	if !req.Unleased {
		lb, ok := c.backend.(LeaseBackend)
		if !ok {
			return nil, ErrLeaseUnsupported
		}
		if !req.Deferred {
			l, err := acquireAgentLease(ctx, lb, collection, req)
			if err != nil {
				return nil, err
			}
			a.lease = l
			if !req.NoKeepAlive {
				l.keepAlive()
			}
		}
	}

	resumed, err := a.load(ctx, req.TokenBudget)
	if err != nil {
		a.abandon()
		return nil, err
	}
	a.resumed = resumed
	return a, nil
}

// AcquireLease takes the lease for an agent resumed with Deferred. It tries
// once: while another process holds the lease it returns a *LeaseHeldError,
// and the caller retries when it likes. On success it rereads the newest
// turn and working state, because the previous holder may have written after
// the deferred read, and later writes must continue from its records rather
// than overwrite them. Calling it again once held does nothing.
func (a *Agent) AcquireLease(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("recall: context must not be nil")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrAgentClosed
	}
	if a.lease != nil || a.req.Unleased {
		return nil
	}
	lb, ok := a.c.backend.(LeaseBackend)
	if !ok {
		return ErrLeaseUnsupported
	}
	l, err := acquireAgentLease(ctx, lb, a.collection, a.req)
	if err != nil {
		return err
	}
	heads, _, err := a.list(ctx, recWorkingHead, nil, 1)
	if err == nil && len(heads) > 0 {
		var ws WorkingState
		if err = decodeBody(heads[0], &ws); err == nil && ws.Version > a.state.Version {
			a.state = ws
		}
	}
	var latest int64
	if err == nil {
		latest, err = a.latestTurn(ctx, a.turnSeq)
	}
	if err != nil {
		// Holding a lease this process cannot safely write under helps
		// nobody; hand it back so a retry starts clean.
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = l.release(releaseCtx)
		return err
	}
	a.turnSeq = max(a.turnSeq, latest)
	a.lease = l
	if !a.req.NoKeepAlive {
		l.keepAlive()
	}
	return nil
}

// LeaseHeld reports whether this process holds the agent's lease: always
// true for an unleased agent, false for a deferred one until AcquireLease.
func (a *Agent) LeaseHeld() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.req.Unleased || a.lease != nil
}

// abandon releases the lease after a failed resume, best effort.
func (a *Agent) abandon() {
	a.mu.Lock()
	l := a.lease
	a.mu.Unlock()
	if l != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = l.release(ctx)
	}
}

// Release ends this process's hold on the agent. The lease is handed over at
// once rather than left to expire. Later writes fail with ErrAgentClosed.
func (a *Agent) Release(ctx context.Context) error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	l := a.lease
	a.mu.Unlock()
	if l == nil {
		return nil
	}
	return l.release(ctx)
}

// UpdateWorkingState supersedes the working state. The previous version stays
// readable through WorkingStateHistory. Library-owned fields are overwritten.
func (a *Agent) UpdateWorkingState(ctx context.Context, ws WorkingState) (WorkingState, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.writable(ctx); err != nil {
		return WorkingState{}, err
	}
	next := cloneWorkingState(ws)
	next.Version = a.state.Version + 1
	next.TurnSeq = a.turnSeq
	next.UpdatedAt = time.Now().UTC()
	body, err := json.Marshal(next)
	if err != nil {
		return WorkingState{}, err
	}
	text := workingStateText(next)
	// The versioned copy first: if the head write then fails, the head still
	// names the previous version, and the orphan copy is only history.
	if err := a.put(ctx, a.recordID("ws", strconv.FormatInt(next.Version, 10)), recWorking, text, body, map[string]any{"version": next.Version}); err != nil {
		return WorkingState{}, err
	}
	if err := a.put(ctx, a.recordID("ws", "head"), recWorkingHead, text, body, nil); err != nil {
		return WorkingState{}, err
	}
	a.state = next
	return cloneWorkingState(next), nil
}

// Milestone records that the agent reached a durable point: the working
// state's LastMilestone and Progress are updated together. Everything after
// the last milestone is what a crash can cost.
func (a *Agent) Milestone(ctx context.Context, name, progress string) (WorkingState, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return WorkingState{}, fmt.Errorf("recall: milestone needs a name")
	}
	ws := a.WorkingState()
	ws.LastMilestone = name
	if progress = strings.TrimSpace(progress); progress != "" {
		ws.Progress = progress
	}
	return a.UpdateWorkingState(ctx, ws)
}

// WorkingStateHistory returns earlier working states, newest first.
func (a *Agent) WorkingStateHistory(ctx context.Context, limit int) ([]WorkingState, error) {
	if limit <= 0 || limit > MaxHistoryEvents {
		limit = 20
	}
	rows, _, err := a.list(ctx, recWorking, nil, MaxHistoryEvents)
	if err != nil {
		return nil, err
	}
	out := make([]WorkingState, 0, len(rows))
	for _, r := range rows {
		var ws WorkingState
		if err := decodeBody(r, &ws); err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// RecordTurn appends one turn. Content larger than the output threshold is
// stored as an output, and the turn keeps its opening and a reference.
func (a *Agent) RecordTurn(ctx context.Context, t Turn) (Turn, error) {
	role := strings.TrimSpace(t.Role)
	if !validRoles[role] {
		return Turn{}, fmt.Errorf(`recall: turn role must be "system", "user", "assistant" or "tool", got %q`, t.Role)
	}
	messageID := strings.TrimSpace(t.MessageID)
	if len(messageID) > 256 {
		return Turn{}, fmt.Errorf("recall: turn message id must be at most 256 bytes")
	}
	if len(t.Brief) > maxBriefBytes {
		return Turn{}, fmt.Errorf("recall: turn brief must be at most %d bytes", maxBriefBytes)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.writable(ctx); err != nil {
		return Turn{}, err
	}
	turn := Turn{Seq: a.turnSeq + 1, Role: role, Name: strings.TrimSpace(t.Name), Content: t.Content, MessageID: messageID, Brief: t.Brief, At: time.Now().UTC()}
	if a.threshold > 0 && estimateTokens(t.Content) > a.threshold {
		out, err := a.storeOutputLocked(ctx, Output{Tool: turn.Name, Content: t.Content})
		if err != nil {
			return Turn{}, err
		}
		turn.OutputRef = out.Ref
		turn.Content = opening(t.Content, 400) + fmt.Sprintf("\n[... %d more tokens stored as output %s]", estimateTokens(t.Content), out.Ref)
	}
	body, err := json.Marshal(turn)
	if err != nil {
		return Turn{}, err
	}
	page := strconv.FormatInt((turn.Seq-1)/turnPage, 10)
	if err := a.put(ctx, a.recordID("turn", strconv.FormatInt(turn.Seq, 10)), recTurn, turn.Content, body,
		map[string]any{"seq": turn.Seq, "page": page}); err != nil {
		return Turn{}, err
	}
	a.turnSeq = turn.Seq
	return turn, nil
}

// StoreOutput keeps a large tool output whole. An empty Ref gets one; storing
// under an existing ref replaces that output.
func (a *Agent) StoreOutput(ctx context.Context, o Output) (OutputRef, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.writable(ctx); err != nil {
		return OutputRef{}, err
	}
	out, err := a.storeOutputLocked(ctx, o)
	if err != nil {
		return OutputRef{}, err
	}
	return out.ref(), nil
}

func (a *Agent) storeOutputLocked(ctx context.Context, o Output) (Output, error) {
	if len(o.Content) > maxOutputBytes {
		return Output{}, fmt.Errorf("recall: output is %d bytes; the limit is %d, store larger artifacts as objects and set a pointer", len(o.Content), maxOutputBytes)
	}
	o.Ref = strings.TrimSpace(o.Ref)
	if o.Ref == "" {
		a.outSeq++
		o.Ref = fmt.Sprintf("out-%d-%d", a.turnSeq+1, a.outSeq)
	}
	if !recordNamePattern.MatchString(o.Ref) {
		return Output{}, fmt.Errorf("recall: output ref must be 1-128 letters, digits, '.', '_' or '-', got %q", o.Ref)
	}
	o.At = time.Now().UTC()
	o.Summary = strings.TrimSpace(o.Summary)
	if o.Summary == "" {
		o.Summary = opening(o.Content, 200)
	}
	body, err := json.Marshal(o)
	if err != nil {
		return Output{}, err
	}
	if err := a.put(ctx, a.recordID("out", o.Ref), recOutput, o.Summary+"\n"+opening(o.Content, 2000), body,
		map[string]any{"ref": o.Ref, "tokens": estimateTokens(o.Content)}); err != nil {
		return Output{}, err
	}
	return o, nil
}

func (o Output) ref() OutputRef {
	return OutputRef{Ref: o.Ref, Tool: o.Tool, Summary: o.Summary, Tokens: estimateTokens(o.Content), At: o.At}
}

// FetchOutput returns a stored output by reference.
func (a *Agent) FetchOutput(ctx context.Context, ref string) (Output, error) {
	rows, _, err := a.list(ctx, recOutput, map[string]any{"ref": strings.TrimSpace(ref)}, 1)
	if err != nil {
		return Output{}, err
	}
	if len(rows) == 0 {
		return Output{}, fmt.Errorf("recall: agent %q has no output %q", a.id, ref)
	}
	var o Output
	return o, decodeBody(rows[0], &o)
}

// SetPointer records where a piece of the agent's work lives, replacing any
// pointer of the same name.
func (a *Agent) SetPointer(ctx context.Context, p Pointer) (Pointer, error) {
	p.Name = strings.TrimSpace(p.Name)
	if !recordNamePattern.MatchString(p.Name) {
		return Pointer{}, fmt.Errorf("recall: pointer name must be 1-128 letters, digits, '.', '_' or '-', got %q", p.Name)
	}
	required, ok := pointerFields[p.Type]
	if !ok {
		return Pointer{}, fmt.Errorf("recall: pointer type must be one of git_ref, object, env, external, process, got %q", p.Type)
	}
	fields := map[string]string{}
	for k, v := range p.Fields {
		if k, v = strings.TrimSpace(k), strings.TrimSpace(v); k != "" && v != "" {
			fields[k] = v
		}
	}
	present := false
	for _, k := range required {
		present = present || fields[k] != ""
	}
	if !present {
		return Pointer{}, fmt.Errorf("recall: a %s pointer needs one of: %s", p.Type, strings.Join(required, ", "))
	}
	p.Fields = fields
	p.At = time.Now().UTC()

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.writable(ctx); err != nil {
		return Pointer{}, err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return Pointer{}, err
	}
	if err := a.put(ctx, a.recordID("ptr", p.Name), recPointer, pointerText(p), body, map[string]any{"name": p.Name}); err != nil {
		return Pointer{}, err
	}
	return p, nil
}

// RemovePointer withdraws a pointer. It is kept as a tombstone, so a resume
// never sees it again.
func (a *Agent) RemovePointer(ctx context.Context, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.writable(ctx); err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	return a.put(ctx, a.recordID("ptr", name), recPointer, "removed pointer "+name, []byte(`{}`),
		map[string]any{"name": name, "removed": true})
}

// Pointers returns the agent's current pointers, sorted by name.
func (a *Agent) Pointers(ctx context.Context) ([]Pointer, error) {
	rows, total, err := a.list(ctx, recPointer, nil, maxPointers)
	if err != nil {
		return nil, err
	}
	if total > maxPointers {
		return nil, fmt.Errorf("recall: agent %q has %d pointers; the limit is %d", a.id, total, maxPointers)
	}
	var out []Pointer
	for _, r := range rows {
		if removed, _ := r.Metadata["removed"].(bool); removed {
			continue
		}
		var p Pointer
		if err := decodeBody(r, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RecentTurns returns up to n of the newest turns, oldest first.
func (a *Agent) RecentTurns(ctx context.Context, n int) ([]Turn, error) {
	a.mu.Lock()
	latest := a.turnSeq
	a.mu.Unlock()
	return a.recentTurns(ctx, latest, n)
}

func (a *Agent) recentTurns(ctx context.Context, latest int64, n int) ([]Turn, error) {
	if n <= 0 || latest <= 0 {
		return nil, nil
	}
	var out []Turn
	for page := (latest - 1) / turnPage; page >= 0 && len(out) < n; page-- {
		turns, err := a.turnsOnPage(ctx, page)
		if err != nil {
			return nil, err
		}
		sort.Slice(turns, func(i, j int) bool { return turns[i].Seq > turns[j].Seq })
		for _, t := range turns {
			if t.Seq <= latest && len(out) < n {
				out = append(out, t)
			}
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (a *Agent) turnsOnPage(ctx context.Context, page int64) ([]Turn, error) {
	rows, _, err := a.list(ctx, recTurn, map[string]any{"page": strconv.FormatInt(page, 10)}, turnPage)
	if err != nil {
		return nil, err
	}
	turns := make([]Turn, 0, len(rows))
	for _, r := range rows {
		var t Turn
		if err := decodeBody(r, &t); err != nil {
			return nil, err
		}
		turns = append(turns, t)
	}
	return turns, nil
}

// latestTurn finds the newest turn at or after hint. The working state
// records the turn count when it was written, so only the pages written
// since then are read.
func (a *Agent) latestTurn(ctx context.Context, hint int64) (int64, error) {
	latest := hint
	page := int64(0)
	if hint > 0 {
		page = (hint - 1) / turnPage
	}
	for {
		turns, err := a.turnsOnPage(ctx, page)
		if err != nil {
			return 0, err
		}
		for _, t := range turns {
			latest = max(latest, t.Seq)
		}
		if len(turns) < turnPage {
			return latest, nil
		}
		page++
	}
}

func (a *Agent) latestOutputs(ctx context.Context) ([]OutputRef, error) {
	rows, _, err := a.list(ctx, recOutput, nil, maxRecentTurns)
	if err != nil {
		return nil, err
	}
	refs := make([]OutputRef, 0, len(rows))
	for _, r := range rows {
		var o Output
		if err := decodeBody(r, &o); err != nil {
			return nil, err
		}
		refs = append(refs, o.ref())
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].At.After(refs[j].At) })
	return refs, nil
}

// --- storage -----------------------------------------------------------------

func (a *Agent) recordID(kind, key string) string { return a.id + ":" + kind + ":" + key }

// writable must be called with a.mu held.
func (a *Agent) writable(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("recall: context must not be nil")
	}
	if a.closed {
		return ErrAgentClosed
	}
	if a.lease != nil {
		return a.lease.guard(ctx)
	}
	if a.req.Deferred {
		return ErrLeaseNotHeld
	}
	return ctx.Err()
}

func (a *Agent) put(ctx context.Context, id, kind, text string, body []byte, extra map[string]any) error {
	values, err := a.embed(ctx, text)
	if err != nil {
		return err
	}
	md := map[string]any{"recall_agent": a.id, "record": kind, "body": string(body)}
	if a.lease != nil {
		md["epoch"] = float64(a.lease.epoch())
	}
	for k, v := range extra {
		md[k] = v
	}
	return a.c.backend.Put(ctx, a.collection, id, values, md)
}

func (a *Agent) embed(ctx context.Context, text string) ([]float32, error) {
	var e Embedder = LexicalEmbedder{}
	if a.c.embedder != nil {
		e = a.c.embedder
	}
	if strings.TrimSpace(text) == "" {
		text = a.id
	}
	v, err := e.Embed(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("recall: embed: %w", err)
	}
	return v, nil
}

func (a *Agent) list(ctx context.Context, kind string, filter map[string]any, limit int) ([]StoredVector, int, error) {
	f := map[string]any{"recall_agent": a.id, "record": kind}
	for k, v := range filter {
		f[k] = v
	}
	rows, total, err := a.c.backend.List(ctx, a.collection, f, limit)
	if err != nil && isMissingCollection(err) {
		return nil, 0, nil
	}
	return rows, total, err
}

// isMissingCollection reports a listing of a collection nothing has been
// written to yet, which for an agent means "no records", not a failure.
func isMissingCollection(err error) bool {
	var nf interface{ NotFound() bool }
	if errors.As(err, &nf) && nf.NotFound() {
		return true
	}
	return false
}

func decodeBody(r StoredVector, dst any) error {
	raw, _ := r.Metadata["body"].(string)
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		return fmt.Errorf("recall: agent record %q is malformed: %w", r.ID, err)
	}
	return nil
}

func cloneWorkingState(w WorkingState) WorkingState {
	w.Plan = append([]string(nil), w.Plan...)
	w.Decisions = append([]string(nil), w.Decisions...)
	w.OpenQuestions = append([]string(nil), w.OpenQuestions...)
	return w
}

// estimateTokens is the usual four-characters-per-token rule of thumb. The
// budget is a bound on context size, not a billing figure, so an estimate
// that is off by a tenth is fine.
func estimateTokens(s string) int { return (len(s) + 3) / 4 }

func opening(s string, chars int) string {
	if len(s) <= chars {
		return s
	}
	cut := chars
	for cut > 0 && (s[cut]&0xC0) == 0x80 { // do not split a UTF-8 sequence
		cut--
	}
	return s[:cut] + "..."
}

// --- lease -------------------------------------------------------------------

// agentLease tracks one held lease on the client side. Every write first
// passes guard, which renews once a third of the TTL has gone by, so each
// write lands well inside an epoch the server granted.
type agentLease struct {
	lb         LeaseBackend
	collection string
	name       string
	holder     string
	ttl        time.Duration

	mu      sync.Mutex
	ep      uint64
	granted time.Time
	lost    bool
	stop    chan struct{}
	done    chan struct{}
}

func defaultHolder() string {
	host, _ := os.Hostname()
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s:%d:%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}

func acquireAgentLease(ctx context.Context, lb LeaseBackend, collection string, req ResumeRequest) (*agentLease, error) {
	holder := req.Holder
	if holder == "" {
		holder = defaultHolder()
	}
	ttl := req.LeaseTTL
	if ttl == 0 {
		ttl = 60 * time.Second
	}
	start := time.Now()
	g, err := lb.AcquireLease(ctx, collection, req.AgentID, holder, ttl)
	if err != nil {
		return nil, err
	}
	if g.TTL > 0 {
		ttl = g.TTL
	}
	return &agentLease{lb: lb, collection: collection, name: req.AgentID, holder: holder, ttl: ttl, ep: g.Epoch, granted: start}, nil
}

func (l *agentLease) epoch() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ep
}

// guard renews when due and reports whether a write may proceed. The grant
// time is taken before the request is sent, so the client never believes an
// epoch lasts longer than the server made it.
func (l *agentLease) guard(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost {
		return ErrLeaseLost
	}
	age := time.Since(l.granted)
	if age < l.ttl/3 {
		return nil
	}
	start := time.Now()
	g, err := l.lb.RenewLease(ctx, l.collection, l.name, l.holder, l.ep, l.ttl)
	if errors.Is(err, ErrLeaseLost) {
		l.lost = true
		return ErrLeaseLost
	}
	if err != nil {
		// A transient failure leaves the current epoch trustworthy until
		// its safety margin: a third of the TTL before it lapses.
		if age < l.ttl*2/3 {
			return nil
		}
		return fmt.Errorf("recall: agent lease could not be renewed: %w", err)
	}
	l.ep = g.Epoch
	l.granted = start
	return nil
}

func (l *agentLease) keepAlive() {
	l.stop = make(chan struct{})
	l.done = make(chan struct{})
	go func() {
		defer close(l.done)
		t := time.NewTicker(l.ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), l.ttl/3)
				err := l.guard(ctx)
				cancel()
				if errors.Is(err, ErrLeaseLost) {
					return
				}
			}
		}
	}()
}

func (l *agentLease) release(ctx context.Context) error {
	if l.stop != nil {
		close(l.stop)
		<-l.done
		l.stop = nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost {
		return nil
	}
	l.lost = true
	return l.lb.ReleaseLease(ctx, l.collection, l.name, l.holder, l.ep)
}
