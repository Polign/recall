package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Polign/recall"
)

// Positions over the wire. Every State API answer carries the agent's
// position after the call: a "position" field in an object answer, and the
// Recall-Position header over HTTP for every answer. A write may carry "at",
// the position its caller last saw, as a body field (an object or the
// turns.actions.state form) or as the If-Position header; when the records
// are ahead of it the write answers 409 position_behind with the records'
// position, and the caller asks agent_since what it missed.

const (
	headerIfPosition = "If-Position"
	headerPosition   = "Recall-Position"
)

// positionWrites are the tools At applies to: the writes that move the
// position, and end_action, which settles what a begin started.
var positionWrites = map[string]bool{
	"record_turn": true, "begin_action": true, "end_action": true, "update_working_state": true, "agent_milestone": true,
}

// callerKey carries what the credential says about the caller to the tools.
type callerKey struct{}

type caller struct {
	// bound means the credential fixes the agent (a Kubernetes key, a
	// Substrate actor token): the server holds the lease for it, resumes it
	// in place, and releases it when it goes idle.
	bound bool
}

func withCaller(ctx context.Context, p principal) context.Context {
	return context.WithValue(ctx, callerKey{}, caller{bound: p.Agent != ""})
}

func callerFrom(ctx context.Context) caller {
	c, _ := ctx.Value(callerKey{}).(caller)
	return c
}

// positionSink receives the agent's position after a tool ran, so the HTTP
// layer can put it in a header whatever the answer's shape.
type positionSink struct{}

func withPositionSink(ctx context.Context) (context.Context, *recall.Position) {
	p := new(recall.Position)
	return context.WithValue(ctx, positionSink{}, p), p
}

func reportPosition(ctx context.Context, pos recall.Position) {
	if p, ok := ctx.Value(positionSink{}).(*recall.Position); ok {
		*p = pos
	}
}

// parseAt reads an "at" argument: the turns.actions.state form or an object
// with turns, actions and state.
func parseAt(raw json.RawMessage) (*recall.Position, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		p, err := recall.ParsePosition(s)
		if err != nil {
			return nil, fmt.Errorf("arguments: at: %w", err)
		}
		return &p, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("arguments: at must be \"turns.actions.state\" or {\"turns\":..,\"actions\":..,\"state\":..}")
	}
	for k := range obj {
		if k != "turns" && k != "actions" && k != "state" {
			return nil, fmt.Errorf("arguments: at does not take %q", k)
		}
	}
	var p recall.Position
	if err := json.Unmarshal(raw, &p); err != nil || p.Turns < 0 || p.Actions < 0 || p.State < 0 {
		return nil, fmt.Errorf("arguments: at holds three non-negative integers: turns, actions, state")
	}
	return &p, nil
}

// ifPosition folds the If-Position header into the arguments as "at". The
// two must agree when both are given. It reports whether the call carries a
// position at all.
func ifPosition(r *http.Request, args json.RawMessage) (json.RawMessage, bool, *httpError) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, false, &httpError{http.StatusBadRequest, "invalid_argument", "arguments must be a JSON object"}
	}
	h := strings.TrimSpace(r.Header.Get(headerIfPosition))
	if h == "" {
		_, present := m["at"]
		return args, present, nil
	}
	hp, err := recall.ParsePosition(h)
	if err != nil {
		return nil, false, &httpError{http.StatusBadRequest, "invalid_argument", "If-Position must be turns.actions.state, the position the last answer carried"}
	}
	if raw, ok := m["at"]; ok {
		bp, err := parseAt(raw)
		if err != nil {
			return nil, false, &httpError{http.StatusBadRequest, "invalid_argument", err.Error()}
		}
		if bp != nil && *bp != hp {
			return nil, false, &httpError{http.StatusBadRequest, "invalid_argument", "If-Position and the at field disagree"}
		}
	}
	m["at"], _ = json.Marshal(hp.String())
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false, &httpError{http.StatusBadRequest, "invalid_argument", err.Error()}
	}
	return out, true, nil
}

// withPosition adds the position to an object answer. Other answers (lists)
// are left as they are; the header carries the position for them.
func withPosition(out string, pos recall.Position) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		return out
	}
	m["position"], _ = json.Marshal(pos)
	b, err := json.Marshal(m)
	if err != nil {
		return out
	}
	return string(b)
}

// writeStateError writes a State API failure; a position error carries the
// records' position, which is where the caller continues from after
// agent_since.
func writeStateError(w http.ResponseWriter, err error) {
	status, code := stateError(err)
	var pe *recall.PositionError
	if errors.As(err, &pe) {
		w.Header().Set(headerPosition, pe.Position.String())
		writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": err.Error()}, "position": pe.Position, "at": pe.At})
		return
	}
	writeError(w, status, code, err.Error())
}
