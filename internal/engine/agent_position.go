package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A process that is snapshotted and restored by its runtime can come back
// behind its own records: a revert to an older snapshot, or a write retried
// across a suspend without knowing whether it landed. Recall never rolls the
// records back to match. The records are what happened; the restored process
// is told it is behind and asks what it missed (Since).
//
// Three things catch it, and each covers what the others miss:
//
//   - A position: the turn count, the action count and the working state
//     version. A write may carry the position its caller last saw (At), and
//     is refused with ErrPositionBehind when the records are ahead of it.
//   - Ids the harness already has: a turn's MessageID and an action's CallID.
//     A turn or action already recorded under one is returned as it stands,
//     so a blind retry or a small replay is a no-op.
//   - Create-only writes, on a backend that has them. A process holding the
//     Agent itself (in process, not over HTTP) is restored with its counters,
//     and would write seqs it already used. The write of an id that already
//     holds a record is refused, the Agent rereads its position from the
//     records, and the caller gets ErrPositionBehind.

var (
	// ErrPositionBehind means the agent's records are ahead of the position
	// the write was made from: the process was restored from an earlier
	// snapshot, or another process wrote meanwhile. Nothing was written. Call
	// Since with the position the error carries, then continue.
	ErrPositionBehind = errors.New("recall: agent records are ahead of this process; it was restored from an earlier point")
	// ErrPositionAhead means the caller's position is ahead of the records:
	// the backend lost writes the caller saw acknowledged. Nothing was written.
	ErrPositionAhead = errors.New("recall: position is ahead of the agent's records; the backend lost writes")
	// ErrRecordExists is what a CreateBackend returns when the id already
	// holds a record.
	ErrRecordExists = errors.New("recall: a record with this id already exists")
)

// Position is how far an agent's records go: the newest turn and action
// seqs and the working state version.
type Position struct {
	Turns   int64 `json:"turns"`
	Actions int64 `json:"actions"`
	State   int64 `json:"state"`
}

// String formats a position as turns.actions.state, the If-Position form.
func (p Position) String() string {
	return fmt.Sprintf("%d.%d.%d", p.Turns, p.Actions, p.State)
}

// ParsePosition reads the turns.actions.state form.
func ParsePosition(s string) (Position, error) {
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 3 {
		return Position{}, fmt.Errorf("recall: a position is turns.actions.state, got %q", s)
	}
	var n [3]int64
	for i, part := range parts {
		v, err := strconv.ParseInt(part, 10, 64)
		if err != nil || v < 0 {
			return Position{}, fmt.Errorf("recall: a position is three non-negative numbers, got %q", s)
		}
		n[i] = v
	}
	return Position{Turns: n[0], Actions: n[1], State: n[2]}, nil
}

// behind reports whether q has anything p does not.
func (p Position) behind(q Position) bool {
	return q.Turns > p.Turns || q.Actions > p.Actions || q.State > p.State
}

// PositionError is ErrPositionBehind or ErrPositionAhead with the positions.
type PositionError struct {
	// At is the position the write was made from.
	At Position
	// Position is where the records stand. After ErrPositionBehind the Agent
	// has already moved to it, so the next write continues from the records.
	Position Position
	Ahead    bool
}

func (e *PositionError) Error() string {
	if e.Ahead {
		return fmt.Sprintf("%s: at %s, records at %s", ErrPositionAhead, e.At, e.Position)
	}
	return fmt.Sprintf("%s: at %s, records at %s; call Since(%s) for what happened after", ErrPositionBehind, e.At, e.Position, e.At)
}

func (e *PositionError) Unwrap() error {
	if e.Ahead {
		return ErrPositionAhead
	}
	return ErrPositionBehind
}

// CreateBackend optionally writes a record only when its id holds none.
// Create must fail with an error wrapping ErrRecordExists, and write nothing,
// when the id holds a record. A non-nil fence applies as in PutFenced. A
// backend that cannot tell (an older server) may write as Put does; the
// position and the ids still catch a replay over HTTP.
type CreateBackend interface {
	Create(ctx context.Context, collection, id string, values []float32, metadata map[string]any, fence *Fence) error
}

// WriteOption adjusts one agent write.
type WriteOption func(*writeOptions)

type writeOptions struct {
	at     *Position
	callID string
}

func applyWriteOptions(opts []WriteOption) writeOptions {
	var o writeOptions
	for _, f := range opts {
		if f != nil {
			f(&o)
		}
	}
	return o
}

// At makes a write conditional on the records standing at p, the position
// the caller last saw. When they are ahead the write fails with
// ErrPositionBehind; when behind, with ErrPositionAhead. A write the ids show
// was already made (a turn's MessageID, an action's CallID) returns the
// recorded one before the position is checked, so a retried write carrying
// the position from before it is not refused.
func At(p Position) WriteOption {
	return func(o *writeOptions) { o.at = &p }
}

// CallID gives BeginAction the harness's id for the tool call, usually the
// model's tool call id. An action already begun under it is returned with its
// current status and result instead of a new action.
func CallID(id string) WriteOption {
	return func(o *writeOptions) { o.callID = strings.TrimSpace(id) }
}

// Position returns how far the agent's records go, as this Agent knows them.
func (a *Agent) Position() Position {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.positionLocked()
}

func (a *Agent) positionLocked() Position {
	return Position{Turns: a.turnSeq, Actions: a.actSeq, State: a.state.Version}
}

// checkAtLocked refuses a write made from a position other than the Agent's.
func (a *Agent) checkAtLocked(at *Position) error {
	if at == nil {
		return nil
	}
	cur := a.positionLocked()
	switch {
	case at.behind(cur):
		return &PositionError{At: *at, Position: cur}
	case cur.behind(*at):
		return &PositionError{At: *at, Position: cur, Ahead: true}
	}
	return nil
}

// catchUpLocked rereads the newest turn, action and working state from the
// records, after a create-only write showed they moved past this Agent. It
// reports whether the position moved.
func (a *Agent) catchUpLocked(ctx context.Context) (bool, error) {
	before := a.positionLocked()
	heads, _, err := a.list(ctx, recWorkingHead, nil, 1)
	if err != nil {
		return false, err
	}
	if len(heads) > 0 {
		var ws WorkingState
		if err := decodeBody(heads[0], &ws); err != nil {
			return false, err
		}
		if ws.Version > a.state.Version {
			a.state = ws
		}
	}
	latest, err := a.latestTurn(ctx, a.turnSeq)
	if err != nil {
		return false, err
	}
	latestAct, err := a.latestAction(ctx, max(a.actSeq, a.state.ActionSeq))
	if err != nil {
		return false, err
	}
	if latest > a.turnSeq {
		turns, err := a.turnsBetween(ctx, a.turnSeq, latest)
		if err != nil {
			return false, err
		}
		for _, t := range turns {
			a.noteMessageLocked(t)
		}
	}
	a.turnSeq = max(a.turnSeq, latest)
	a.actSeq = max(a.actSeq, latestAct)
	return a.positionLocked() != before, nil
}

// behindLocked handles a create-only write the backend refused: it catches
// the Agent up and returns the PositionError for the caller.
func (a *Agent) behindLocked(ctx context.Context, cause error) error {
	at := a.positionLocked()
	if _, err := a.catchUpLocked(ctx); err != nil {
		return fmt.Errorf("%w; rereading the records failed: %v", cause, err)
	}
	return &PositionError{At: at, Position: a.positionLocked()}
}

// repeatedLocked reports a write answered with what was already recorded.
func (a *Agent) repeatedLocked() {
	if a.req.OnRepeat != nil {
		a.req.OnRepeat()
	}
}

// noteMessageLocked remembers a turn's message id, so a repeat of it is found
// without a read. Old ids are dropped once the set grows; a repeat that old
// is caught by the position instead.
func (a *Agent) noteMessageLocked(t Turn) {
	if t.MessageID == "" {
		return
	}
	if a.messages == nil {
		a.messages = map[string]int64{}
	}
	a.messages[t.MessageID] = t.Seq
	if len(a.messages) > 2*maxRecentTurns {
		for id, seq := range a.messages {
			if seq <= a.turnSeq-maxRecentTurns {
				delete(a.messages, id)
			}
		}
	}
}

// noteCallLocked does the same for an action's call id.
func (a *Agent) noteCallLocked(act Action) {
	if act.CallID == "" {
		return
	}
	if a.calls == nil {
		a.calls = map[string]int64{}
	}
	a.calls[act.CallID] = act.Seq
	if len(a.calls) > 2*maxRecentTurns {
		for id, seq := range a.calls {
			if seq <= a.actSeq-maxRecentTurns {
				delete(a.calls, id)
			}
		}
	}
}

// recordedTurnLocked returns the turn recorded under a message id, if this
// Agent knows of one.
func (a *Agent) recordedTurnLocked(ctx context.Context, messageID string) (Turn, bool, error) {
	seq, ok := a.messages[messageID]
	if !ok || messageID == "" {
		return Turn{}, false, nil
	}
	turns, err := a.turnsOnPage(ctx, (seq-1)/turnPage)
	if err != nil {
		return Turn{}, false, err
	}
	for _, t := range turns {
		if t.Seq == seq && t.MessageID == messageID {
			return t, true, nil
		}
	}
	return Turn{}, false, nil
}

// recordedActionLocked returns the action begun under a call id. The set of
// recent ids answers most lookups; a miss asks the records, since repeating
// an external action costs more than one read.
func (a *Agent) recordedActionLocked(ctx context.Context, callID string) (Action, bool, error) {
	if callID == "" {
		return Action{}, false, nil
	}
	if seq, ok := a.calls[callID]; ok {
		act, err := a.action(ctx, seq)
		if err == nil && act.CallID == callID {
			return act, true, nil
		}
	}
	rows, _, err := a.list(ctx, recAction, map[string]any{"call_id": callID}, 1)
	if err != nil || len(rows) == 0 {
		return Action{}, false, err
	}
	var act Action
	if err := decodeBody(rows[0], &act); err != nil {
		return Action{}, false, err
	}
	return act, act.CallID == callID, nil
}

// SinceReport is what happened after a position: the turns, the actions in
// every status, and the working state if it changed, with a briefing for the
// model.
type SinceReport struct {
	AgentID string `json:"agent_id"`
	// From is the position asked about; Position is where the records stand.
	From     Position `json:"from"`
	Position Position `json:"position"`
	Turns    []Turn   `json:"turns,omitempty"`
	// Actions begun after From, in every status, oldest first.
	Actions []Action `json:"actions,omitempty"`
	// WorkingState is the current working state, when it changed after From.
	WorkingState *WorkingState `json:"working_state,omitempty"`
	Omitted      struct {
		Turns int `json:"turns,omitempty"`
	} `json:"omitted"`
	TokenBudget int    `json:"token_budget"`
	Tokens      int    `json:"tokens"`
	Briefing    string `json:"briefing"`
}

// maxSinceTurns bounds how many turns Since reads.
const maxSinceTurns = maxRecentTurns

// Since reports what the records hold after from, within a token budget
// (zero means 8000). The actions and the working state are always included;
// the newest turns fill what is left. A position ahead of the records is
// ErrPositionAhead. Since reads only, and moves the Agent to the records'
// position, so a caller that adopts Position carries on from there.
func (a *Agent) Since(ctx context.Context, from Position, budget int) (SinceReport, error) {
	if ctx == nil {
		return SinceReport{}, fmt.Errorf("recall: context must not be nil")
	}
	if budget < 0 {
		return SinceReport{}, fmt.Errorf("recall: token budget must not be negative")
	}
	if budget == 0 {
		budget = defaultTokenBudget
	}
	a.mu.Lock()
	if _, err := a.catchUpLocked(ctx); err != nil {
		a.mu.Unlock()
		return SinceReport{}, err
	}
	cur := a.positionLocked()
	ws := cloneWorkingState(a.state)
	a.mu.Unlock()

	rep := SinceReport{AgentID: a.id, From: from, Position: cur, TokenBudget: budget}
	if cur.behind(from) {
		return rep, &PositionError{At: from, Position: cur, Ahead: true}
	}
	if cur.State > from.State {
		rep.WorkingState = &ws
	}
	var turns []Turn
	if cur.Turns > from.Turns {
		lo := max(from.Turns, cur.Turns-maxSinceTurns)
		var err error
		if turns, err = a.turnsBetween(ctx, lo, cur.Turns); err != nil {
			return rep, err
		}
		rep.Omitted.Turns = int(lo - from.Turns)
	}
	if cur.Actions > from.Actions {
		acts, err := a.actionsBetween(ctx, from.Actions, cur.Actions)
		if err != nil {
			return rep, err
		}
		rep.Actions = acts
	}

	var b strings.Builder
	writeSinceHeader(&b, rep, len(turns))
	writeSinceActions(&b, rep.Actions)
	if rep.WorkingState != nil {
		writeWorkingState(&b, *rep.WorkingState)
	}
	left := max(budget-estimateTokens(b.String()), 0)
	kept, _ := fitTurns(turns, left)
	if len(kept) > 0 {
		b.WriteString("\n## Turns recorded after that point, verbatim\n")
		for _, t := range kept {
			b.WriteString(turnText(t))
		}
	}
	rep.Turns = kept
	rep.Omitted.Turns += len(turns) - len(kept)
	rep.Briefing = b.String()
	rep.Tokens = estimateTokens(rep.Briefing)
	return rep, nil
}

func writeSinceHeader(b *strings.Builder, rep SinceReport, turns int) {
	if !rep.From.behind(rep.Position) {
		fmt.Fprintf(b, "Agent %s: nothing was recorded after position %s.\n", rep.AgentID, rep.From)
		return
	}
	fmt.Fprintf(b, "Agent %s: your process was restored to an earlier point (position %s). The records went on to %s: "+
		"%d turns, %d actions and %d working state versions you do not remember. They happened; do not do them again.\n",
		rep.AgentID, rep.From, rep.Position, rep.Position.Turns-rep.From.Turns, rep.Position.Actions-rep.From.Actions, rep.Position.State-rep.From.State)
}

func writeSinceActions(b *strings.Builder, acts []Action) {
	if len(acts) == 0 {
		return
	}
	b.WriteString("\n## Actions after that point\n")
	for _, act := range acts {
		fmt.Fprintf(b, "- #%d %s(%s) [%s, key %s]: %s", act.Seq, act.Tool, oneLine(opening(act.Args, 200)), act.Class, act.Key, act.Status)
		switch {
		case act.Status == ActionDone && act.Result != "":
			b.WriteString(", result: " + oneLine(opening(act.Result, 300)))
		case act.Status == ActionFailed && act.Error != "":
			b.WriteString(", error: " + oneLine(opening(act.Error, 300)))
		case act.Status.open():
			b.WriteString(". Not known whether this happened. Check before repeating it; never repeat it blindly.")
		}
		b.WriteString("\n")
	}
}

// turnsBetween returns the turns with seq in (after, upTo], oldest first.
func (a *Agent) turnsBetween(ctx context.Context, after, upTo int64) ([]Turn, error) {
	var out []Turn
	if upTo <= after {
		return nil, nil
	}
	for page := after / turnPage; page <= (upTo-1)/turnPage; page++ {
		turns, err := a.turnsOnPage(ctx, page)
		if err != nil {
			return nil, err
		}
		for _, t := range turns {
			if t.Seq > after && t.Seq <= upTo {
				out = append(out, t)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// actionsBetween returns the actions with seq in (after, upTo], oldest first.
func (a *Agent) actionsBetween(ctx context.Context, after, upTo int64) ([]Action, error) {
	var out []Action
	for page := after / actionPage; page <= (upTo-1)/actionPage; page++ {
		rows, _, err := a.list(ctx, recAction, map[string]any{"page": strconv.FormatInt(page, 10)}, actionPage)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			var act Action
			if err := decodeBody(r, &act); err != nil {
				return nil, err
			}
			if act.Seq > after && act.Seq <= upTo {
				out = append(out, act)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// wallNow is the wall clock without Go's monotonic reading. A process
// restored from a snapshot sees almost no monotonic time across the suspend
// (gVisor measured 6 ms across 21 s), so lease ages are taken from both.
func wallNow() time.Time { return time.Now().Round(0) }
