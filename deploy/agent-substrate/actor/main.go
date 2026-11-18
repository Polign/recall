// The demo actor for Recall on Agent Substrate.
//
// It stands in for an agent. POST /v1/<operation> is passed to Recall with a
// placeholder Authorization header, which Substrate's egress gateway replaces
// with a JWT minted for this actor: the actor holds no key. POST /count
// increments a counter in process memory, to show what a snapshot keeps
// beside what Recall keeps.
//
// POST /h/* is a small harness whose progress lives in process memory, so a
// snapshot restore rolls it back the way it would roll back a real agent's
// harness. POST /inproc/* holds an agent in process through the Go client,
// against a polign_db the actor reaches directly, so the lease and the
// sequence numbers are in the snapshot too.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Polign/recall"
	"github.com/Polign/recall/polign"
)

func main() {
	recallURL := strings.TrimRight(os.Getenv("RECALL_URL"), "/")
	if recallURL == "" {
		log.Fatal("RECALL_URL is unset")
	}
	// No keep-alives: Substrate does not keep open connections across a
	// suspend, so every call dials again.
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	var count atomic.Int64

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("/count", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%d\n", count.Add(1))
	})
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, recallURL+r.URL.Path, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		// The gateway discards this value and puts the actor's JWT in its place.
		req.Header.Set("Authorization", "Bearer placeholder")
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})

	h := &harness{recallURL: recallURL, client: client}
	mux.HandleFunc("/h/resume", h.resume)
	mux.HandleFunc("/h/job", h.job)
	mux.HandleFunc("/h/call", h.call)

	ip := &inproc{polignURL: strings.TrimRight(os.Getenv("POLIGN_URL"), "/")}
	mux.HandleFunc("/inproc/resume", ip.resume)
	mux.HandleFunc("/inproc/turn", ip.turn)
	mux.HandleFunc("/inproc/turns", ip.turns)
	mux.HandleFunc("/inproc/peek", ip.peek)
	// /clock: the wall clock and the monotonic time since start, to see what
	// a restored process can tell about the time it spent suspended.
	start := time.Now()
	mux.HandleFunc("/clock", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"wall_unix_ms": time.Now().UnixMilli(), "mono_ms": time.Since(start).Milliseconds()})
	})
	log.Fatal(http.ListenAndServe(":80", mux))
}

// harness is what an agent harness keeps in process memory: the agent it
// resumed and how far through its job it is.
type harness struct {
	recallURL string
	client    *http.Client

	mu      sync.Mutex
	agentID string
	step    int
	action  int64
	// position is the Recall-Position of the last answer. Every write carries
	// it as If-Position; a revert puts the old one back, and the server then
	// says the records moved on.
	position string
}

// positionWrites are the verbs that carry If-Position.
var positionWrites = map[string]bool{"record_turn": true, "begin_action": true, "end_action": true, "update_working_state": true, "milestone": true}

type stateAnswer struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// state calls one State API verb for the harness's agent.
func (h *harness) state(ctx context.Context, verb string, args map[string]any) (stateAnswer, error) {
	if args == nil {
		args = map[string]any{}
	}
	args["agent_id"] = h.agentID
	body, _ := json.Marshal(args)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.recallURL+"/v1/agent/"+verb, bytes.NewReader(body))
	if err != nil {
		return stateAnswer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer placeholder")
	if positionWrites[verb] && h.position != "" {
		req.Header.Set("If-Position", h.position)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return stateAnswer{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return stateAnswer{}, err
	}
	if !json.Valid(raw) {
		raw, _ = json.Marshal(string(raw))
	}
	// A refusal carries the records' position too; the harness adopts it
	// only after it has asked what it missed.
	if p := resp.Header.Get("Recall-Position"); p != "" && resp.StatusCode == http.StatusOK {
		h.position = p
	}
	return stateAnswer{Status: resp.StatusCode, Body: raw}, nil
}

// behind reports a position_behind answer.
func behind(a stateAnswer) bool {
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	return a.Status == http.StatusConflict && json.Unmarshal(a.Body, &e) == nil && e.Error.Code == "position_behind"
}

// catchUp asks what happened after the harness's position and adopts the
// records' position. A real harness hands the briefing to its model.
func (h *harness) catchUp(ctx context.Context) (stateAnswer, error) {
	return h.state(ctx, "since", map[string]any{"at": h.position})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// resume: {"agent_id": "..."}. The harness keeps the id in memory, resumes,
// and starts its job from the beginning.
func (h *harness) resume(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.AgentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id is required"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agentID, h.step, h.action, h.position = in.AgentID, 0, 0, ""
	// A run before this one may have left the agent held for this actor.
	_, _ = h.state(r.Context(), "release", nil)
	a, err := h.state(r.Context(), "resume", nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// job runs whatever is left of a fixed job: a user turn, one external
// action (sending an invoice), an assistant turn. Each step is done once as
// far as this process remembers. "effects" counts the invoices this call
// sent; the caller keeps the tally outside the actor.
//
// The harness passes its own ids: a message id per turn and a call id for
// the invoice. After a revert it runs the job again from its old memory, and
// the ids give back what was recorded: the begin answers "done", so the
// invoice is not sent twice. A write the ids do not cover answers
// position_behind; the harness catches up and tries the step again.
func (h *harness) job(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ctx := r.Context()
	out := map[string]any{"agent_id": h.agentID, "from_step": h.step, "effects": 0}
	var calls []stateAnswer
	fail := func(a stateAnswer, err error) {
		if err != nil {
			out["error"] = err.Error()
		} else {
			out["error"] = a.Body
		}
		out["step"], out["calls"] = h.step, calls
		writeJSON(w, http.StatusOK, out)
	}
	for h.step < 4 {
		var a stateAnswer
		var err error
		switch h.step {
		case 0:
			a, err = h.state(ctx, "record_turn", map[string]any{"role": "user", "content": "Send the March invoice to Acme.", "message_id": "job-user-1"})
		case 1:
			a, err = h.state(ctx, "begin_action", map[string]any{"tool": "send_invoice", "class": "external", "args": `{"customer":"acme","month":"2026-03"}`, "call_id": "job-send-invoice"})
			if err == nil && a.Status == http.StatusOK {
				var act struct {
					Seq    int64
					Status string
				}
				_ = json.Unmarshal(a.Body, &act)
				h.action = act.Seq
				if act.Status == "done" {
					// Already sent before the revert: skip the effect.
					out["skipped"] = "send_invoice was already done"
					h.step++
				}
			}
		case 2:
			// The side effect. Nothing in Recall is involved; the caller
			// counts it.
			out["effects"] = 1
			a, err = h.state(ctx, "end_action", map[string]any{"seq": h.action, "status": "done", "result": "invoice 2026-03 sent to acme"})
		case 3:
			a, err = h.state(ctx, "record_turn", map[string]any{"role": "assistant", "content": "Sent the March invoice to Acme.", "message_id": "job-assistant-1"})
		}
		calls = append(calls, a)
		if err == nil && behind(a) {
			c, cerr := h.catchUp(ctx)
			calls = append(calls, c)
			if cerr != nil || c.Status != http.StatusOK {
				fail(c, cerr)
				return
			}
			out["caught_up"] = true
			continue
		}
		if err != nil || a.Status != http.StatusOK {
			fail(a, err)
			return
		}
		h.step++
	}
	out["step"], out["action_seq"], out["calls"] = h.step, h.action, calls
	writeJSON(w, http.StatusOK, out)
}

// call: {"verb": "...", "args": {...}, "repeat": n}. Sends one State API call
// for the agent in memory n times, as a retry after a lost answer would, and
// returns every answer.
func (h *harness) call(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Verb   string         `json:"verb"`
		Args   map[string]any `json:"args"`
		Repeat int            `json:"repeat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Verb == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "verb is required"})
		return
	}
	in.Repeat = max(in.Repeat, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	var answers []stateAnswer
	// A retry after a lost answer carries the position from before it.
	from := h.position
	for range in.Repeat {
		h.position = from
		args := map[string]any{}
		for k, v := range in.Args {
			args[k] = v
		}
		a, err := h.state(r.Context(), in.Verb, args)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		answers = append(answers, a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent_id": h.agentID, "answers": answers})
}

// inproc holds one agent in this process, as `recall mcp -agent` over stdio
// or the Go client would: the lease, its epoch and the turn sequence are all
// process memory.
type inproc struct {
	polignURL string

	mu    sync.Mutex
	agent *recall.Agent
}

// resume: {"agent_id": "...", "ttl_s": n}.
func (p *inproc) resume(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID string `json:"agent_id"`
		TTL     int    `json:"ttl_s"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.AgentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent_id is required"})
		return
	}
	if p.polignURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "POLIGN_URL is unset"})
		return
	}
	backend, err := polign.New(polign.Config{BaseURL: p.polignURL})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	client, err := recall.NewClient(recall.Config{Backend: backend, Collection: "inproc",
		Registry: recall.DefaultRegistry(), Embedder: recall.LexicalEmbedder{}})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.agent != nil {
		_ = p.agent.Release(r.Context())
		p.agent = nil
	}
	a, err := client.Resume(r.Context(), recall.ResumeRequest{AgentID: in.AgentID, LeaseTTL: time.Duration(in.TTL) * time.Second})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	p.agent = a
	writeJSON(w, http.StatusOK, map[string]any{"epoch": a.Epoch(), "fresh": a.Resumed().Fresh})
}

// turn: {"content": "..."} records an assistant turn.
func (p *inproc) turn(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content string `json:"content"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.agent == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not resumed"})
		return
	}
	t, err := p.agent.RecordTurn(r.Context(), recall.Turn{Role: "assistant", Content: in.Content})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "epoch": p.agent.Epoch()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seq": t.Seq, "epoch": p.agent.Epoch()})
}

// peek: {"agent_id": "..."} reads the agent's turns through a new client
// that takes no lease, independent of the held agent's sequence numbers.
func (p *inproc) peek(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AgentID string `json:"agent_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	backend, err := polign.New(polign.Config{BaseURL: p.polignURL})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	client, err := recall.NewClient(recall.Config{Backend: backend, Collection: "inproc",
		Registry: recall.DefaultRegistry(), Embedder: recall.LexicalEmbedder{}})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a, err := client.Resume(r.Context(), recall.ResumeRequest{AgentID: in.AgentID, Deferred: true})
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	ts, err := a.RecentTurns(r.Context(), 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := []string{}
	for _, t := range ts {
		out = append(out, fmt.Sprintf("%d:%s", t.Seq, t.Content))
	}
	writeJSON(w, http.StatusOK, map[string]any{"turn_seq": a.Resumed().TurnSeq, "turns": out})
}

// turns returns the recorded turns as the backend has them.
func (p *inproc) turns(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.agent == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "not resumed"})
		return
	}
	ts, err := p.agent.RecentTurns(r.Context(), 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	type row struct {
		Seq     int64  `json:"seq"`
		Content string `json:"content"`
	}
	out := []row{}
	for _, t := range ts {
		out = append(out, row{t.Seq, t.Content})
	}
	writeJSON(w, http.StatusOK, out)
}
