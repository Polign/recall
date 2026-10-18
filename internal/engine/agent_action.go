package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// An action is one tool call recorded around its run: BeginAction before the
// tool runs, EndAction after it returns. A crash between the two leaves the
// action open, and the next resume lists it, so the new process knows the
// call may or may not have happened instead of guessing from the turns.
// Reconcile then closes each open action according to its class.
//
// The begin and end writes share one record id, so the end replaces the
// begin and finding what is open is one filtered listing.

// ActionClass says what running a tool again would do. It decides what a
// resume does with an action a crash left open.
type ActionClass string

const (
	// ActionPure reads and changes nothing: run it again if still needed.
	ActionPure ActionClass = "pure"
	// ActionIdempotent changes the world, but running it again with the same
	// Key does no more than running it once: a PUT by id, a push of a known
	// sha, a payment with an idempotency key. The tool must pass Key on.
	ActionIdempotent ActionClass = "idempotent"
	// ActionExternal changes the world and running it again could do it
	// twice: send a message, post, charge without a key, delete. It is never
	// run again blindly.
	ActionExternal ActionClass = "external"
)

// ActionStatus is where an action stands. Started and Unknown are open.
type ActionStatus string

const (
	ActionStarted ActionStatus = "started"
	ActionDone    ActionStatus = "done"
	ActionFailed  ActionStatus = "failed"
	// ActionAbandoned closes a pure action a crash interrupted; nothing came
	// of it, and the agent redoes it if it still needs the answer.
	ActionAbandoned ActionStatus = "abandoned"
	// ActionUnknown marks an external action no one could confirm either
	// way. It stays open, and in every briefing, until it is ended.
	ActionUnknown ActionStatus = "unknown"
)

func (s ActionStatus) open() bool { return s == ActionStarted || s == ActionUnknown }

// Action is one recorded tool call.
type Action struct {
	Seq   int64       `json:"seq"`
	Tool  string      `json:"tool"`
	Class ActionClass `json:"class"`
	// Args is the call's arguments as the harness gave them, usually JSON.
	Args string `json:"args,omitempty"`
	// Key is fixed when the action begins and is the same on every retry:
	// the idempotency key an idempotent tool passes to the outside system.
	Key    string       `json:"key"`
	Status ActionStatus `json:"status"`
	Result string       `json:"result,omitempty"`
	Error  string       `json:"error,omitempty"`
	// ArgsRef and ResultRef are set when Args or Result was too large to keep
	// in the record and was stored as an output; the field keeps its opening.
	ArgsRef   string `json:"args_ref,omitempty"`
	ResultRef string `json:"result_ref,omitempty"`
	// Resolution says how an action was closed after a crash.
	Resolution string    `json:"resolution,omitempty"`
	Epoch      uint64    `json:"epoch,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitzero"`
}

// ActionEnd is how an action finished.
type ActionEnd struct {
	Status ActionStatus
	Result string
	Error  string
	// Resolution notes how an action interrupted by a crash was settled.
	Resolution string
}

// Resolver settles one action a crash left open. Reconcile calls it for
// idempotent actions, which it should run again with the action's Key, and
// for external ones, which it should check against the outside system (was
// a message with this subject sent after StartedAt?). Returning a Status of
// ActionUnknown, or no Resolver at all, leaves an external action open and
// flagged in the briefing.
type Resolver func(ctx context.Context, a Action) (ActionEnd, error)

// ReconcileReport lists what Reconcile did.
type ReconcileReport struct {
	Closed []Action `json:"closed,omitempty"`
	// Open are the actions still open afterwards: external ones nobody could
	// confirm, and idempotent ones with no resolver to run them.
	Open []Action `json:"open,omitempty"`
}

const (
	// actionPage groups actions the way turnPage groups turns.
	actionPage = 64
	// maxOpenActions bounds how many open actions a resume reads.
	maxOpenActions = 200
	maxToolName    = 128
)

var validClasses = map[ActionClass]bool{ActionPure: true, ActionIdempotent: true, ActionExternal: true}
var validEnds = map[ActionStatus]bool{ActionDone: true, ActionFailed: true, ActionAbandoned: true, ActionUnknown: true}

// BeginAction records that a tool call is about to run. Call it before the
// call and run the tool only if it succeeds; pass the returned Key to an
// idempotent tool.
func (a *Agent) BeginAction(ctx context.Context, tool string, class ActionClass, args string) (Action, error) {
	tool = strings.TrimSpace(tool)
	if tool == "" || len(tool) > maxToolName {
		return Action{}, fmt.Errorf("recall: action needs a tool name of at most %d bytes", maxToolName)
	}
	if !validClasses[class] {
		return Action{}, fmt.Errorf(`recall: action class must be "pure", "idempotent" or "external", got %q`, class)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.writable(ctx); err != nil {
		return Action{}, err
	}
	act := Action{Seq: a.actSeq + 1, Tool: tool, Class: class, Status: ActionStarted, StartedAt: time.Now().UTC()}
	act.Key = actionKey(a.id, act.Seq)
	if a.lease != nil {
		act.Epoch = a.lease.epoch()
	}
	var err error
	if act.Args, act.ArgsRef, err = a.spill(ctx, act, "args", args); err != nil {
		return Action{}, err
	}
	if err := a.putAction(ctx, act); err != nil {
		return Action{}, err
	}
	a.actSeq = act.Seq
	return act, nil
}

// EndAction closes an open action with how it finished. Ending an action
// that is already closed is an error: the first outcome stands.
func (a *Agent) EndAction(ctx context.Context, seq int64, end ActionEnd) (Action, error) {
	if !validEnds[end.Status] {
		return Action{}, fmt.Errorf(`recall: an action ends "done", "failed", "abandoned" or "unknown", got %q`, end.Status)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.writable(ctx); err != nil {
		return Action{}, err
	}
	act, err := a.action(ctx, seq)
	if err != nil {
		return Action{}, err
	}
	if !act.Status.open() {
		return Action{}, fmt.Errorf("recall: action %d (%s) already ended %s", seq, act.Tool, act.Status)
	}
	act.Status = end.Status
	act.Error = end.Error
	if r := strings.TrimSpace(end.Resolution); r != "" {
		act.Resolution = r
	}
	if act.Result, act.ResultRef, err = a.spill(ctx, act, "result", end.Result); err != nil {
		return Action{}, err
	}
	if end.Status != ActionUnknown {
		act.EndedAt = time.Now().UTC()
	}
	if err := a.putAction(ctx, act); err != nil {
		return Action{}, err
	}
	return act, nil
}

// PendingActions returns the open actions, oldest first.
func (a *Agent) PendingActions(ctx context.Context) ([]Action, error) {
	var out []Action
	for _, st := range []ActionStatus{ActionStarted, ActionUnknown} {
		rows, _, err := a.list(ctx, recAction, map[string]any{"status": string(st)}, maxOpenActions)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			var act Action
			if err := decodeBody(r, &act); err != nil {
				return nil, err
			}
			// A listing filtered on status can lag the record it indexes;
			// trust the body.
			if act.Status.open() {
				out = append(out, act)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// Reconcile settles every action a crash left open, before the agent takes
// another step:
//
//   - pure actions are abandoned; the agent redoes them if it needs to;
//   - idempotent actions go to resolve, which runs them again with their
//     Key; without a resolver they stay open for the harness;
//   - external actions go to resolve, which checks whether they happened;
//     without a resolver, or when it cannot tell, they are marked unknown
//     and stay open, flagged in every briefing until someone ends them.
//
// Nothing is ever run again by Recall itself.
func (a *Agent) Reconcile(ctx context.Context, resolve Resolver) (ReconcileReport, error) {
	var rep ReconcileReport
	pending, err := a.PendingActions(ctx)
	if err != nil {
		return rep, err
	}
	for _, act := range pending {
		var end ActionEnd
		switch {
		case act.Class == ActionPure:
			end = ActionEnd{Status: ActionAbandoned, Resolution: "interrupted; pure, not run again"}
		case resolve == nil && act.Class == ActionIdempotent:
			rep.Open = append(rep.Open, act)
			continue
		case resolve == nil:
			if act.Status == ActionUnknown {
				rep.Open = append(rep.Open, act)
				continue
			}
			end = ActionEnd{Status: ActionUnknown, Resolution: "interrupted; no resolver to check whether it happened"}
		default:
			if end, err = resolve(ctx, act); err != nil {
				return rep, fmt.Errorf("recall: resolve action %d (%s): %w", act.Seq, act.Tool, err)
			}
			if !validEnds[end.Status] {
				return rep, fmt.Errorf("recall: resolver ended action %d with %q", act.Seq, end.Status)
			}
			if end.Status == ActionUnknown && act.Status == ActionUnknown {
				rep.Open = append(rep.Open, act)
				continue
			}
		}
		closed, err := a.EndAction(ctx, act.Seq, end)
		if err != nil {
			return rep, err
		}
		if closed.Status.open() {
			rep.Open = append(rep.Open, closed)
		} else {
			rep.Closed = append(rep.Closed, closed)
		}
	}
	if len(rep.Closed) > 0 {
		// The briefing was assembled before these closed; rebuild it so the
		// model is not told to settle what is already settled.
		rc, err := a.load(ctx, a.req.TokenBudget)
		if err != nil {
			return rep, err
		}
		a.mu.Lock()
		a.resumed = rc
		a.mu.Unlock()
	}
	return rep, nil
}

// actionKey is stable for an action across every process that resumes it.
func actionKey(agent string, seq int64) string {
	sum := sha256.Sum256([]byte(agent + ":" + strconv.FormatInt(seq, 10)))
	return hex.EncodeToString(sum[:16])
}

// spill keeps a large field as an output, leaving its opening in the record.
// It must be called with a.mu held.
func (a *Agent) spill(ctx context.Context, act Action, part, text string) (string, string, error) {
	if a.threshold <= 0 || estimateTokens(text) <= a.threshold {
		return text, "", nil
	}
	out, err := a.storeOutputLocked(ctx, Output{Ref: fmt.Sprintf("act-%d-%s", act.Seq, part), Tool: act.Tool, Content: text})
	if err != nil {
		return "", "", err
	}
	return opening(text, 400) + fmt.Sprintf("\n[... %d more tokens stored as output %s]", estimateTokens(text), out.Ref), out.Ref, nil
}

func (a *Agent) putAction(ctx context.Context, act Action) error {
	body, err := json.Marshal(act)
	if err != nil {
		return err
	}
	page := strconv.FormatInt((act.Seq-1)/actionPage, 10)
	return a.put(ctx, a.recordID("act", strconv.FormatInt(act.Seq, 10)), recAction, act.Tool+" "+opening(act.Args, 400), body,
		map[string]any{"seq": act.Seq, "act": strconv.FormatInt(act.Seq, 10), "page": page, "status": string(act.Status), "class": string(act.Class)})
}

func (a *Agent) action(ctx context.Context, seq int64) (Action, error) {
	rows, _, err := a.list(ctx, recAction, map[string]any{"act": strconv.FormatInt(seq, 10)}, 1)
	if err != nil {
		return Action{}, err
	}
	if len(rows) == 0 {
		return Action{}, fmt.Errorf("recall: agent %q has no action %d", a.id, seq)
	}
	var act Action
	return act, decodeBody(rows[0], &act)
}

// latestAction finds the newest action at or after hint, reading only the
// pages written since, as latestTurn does for turns.
func (a *Agent) latestAction(ctx context.Context, hint int64) (int64, error) {
	latest := hint
	page := int64(0)
	if hint > 0 {
		page = (hint - 1) / actionPage
	}
	for {
		rows, _, err := a.list(ctx, recAction, map[string]any{"page": strconv.FormatInt(page, 10)}, actionPage)
		if err != nil {
			return 0, err
		}
		for _, r := range rows {
			var act Action
			if err := decodeBody(r, &act); err != nil {
				return 0, err
			}
			latest = max(latest, act.Seq)
		}
		if len(rows) < actionPage {
			return latest, nil
		}
		page++
	}
}
