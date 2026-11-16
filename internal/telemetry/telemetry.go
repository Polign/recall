// Package telemetry sends a fixed-schema, anonymous usage heartbeat unless it
// is turned off. It never sees memory content, subjects, predicates, database
// addresses or keys: callers hand it a tool name and nothing else.
//
// TELEMETRY.md at the repository root lists every field. Keep Payload, that
// file and the collector's allowlist (polign_telemetry) in step.
package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Endpoint is the only destination a heartbeat is sent to.
const Endpoint = "https://telemetry.polign.com/v1/recall/ping"

// Notice is what a user is told the first time an install ID is created.
const Notice = "Recall sends anonymous usage statistics: version, platform, and counts of memory operations, never what is remembered. " +
	"See what is sent with `recall telemetry`; turn it off with `recall telemetry off` or RECALL_TELEMETRY=0."

const (
	idFile  = "telemetry-id"
	offFile = "telemetry-off"

	startDelay = 5 * time.Second
	interval   = time.Hour
	sendWait   = 5 * time.Second
	closeWait  = 2 * time.Second
)

// Payload is the complete JSON body of a heartbeat.
type Payload struct {
	Version   string `json:"version"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	InstallID string `json:"install_id"`
	// ProcessID is random per run, so the collector can tell one process's
	// cumulative counters from the next one's.
	ProcessID string `json:"process_id"`
	Mode      string `json:"mode"`      // mcp, serve or setup
	Backend   string `json:"backend"`   // local, polign, qdrant, pgvector, mongodb
	Embedder  string `json:"embedder"`  // provider only: lexical, local, openai, ...
	Extractor string `json:"extractor"` // provider only, or none
	Surface   string `json:"surface"`   // text or typed
	Client    string `json:"client"`    // the MCP client's name over stdio, else ""

	UptimeSeconds uint64 `json:"uptime_seconds"`
	Remembers     uint64 `json:"remembers"`
	Recalls       uint64 `json:"recalls"`
	Forgets       uint64 `json:"forgets"`
	Replacements  uint64 `json:"replacements"`
	Inspections   uint64 `json:"inspections"`
	StateCalls    uint64 `json:"state_calls"`
}

// Config describes the process a Reporter speaks for. Embedder and Extractor
// take the full provider:model spec; only the provider leaves the machine.
type Config struct {
	Version   string
	Home      string // the Recall home directory, where the install ID lives
	Mode      string
	Backend   string
	Embedder  string
	Extractor string
	Text      bool
}

// Disabled says why telemetry is off for this build, environment and home,
// or "" when it is on.
func Disabled(version, home string) string {
	if version == "" || version == "dev" {
		return "development build"
	}
	if v, ok := os.LookupEnv("RECALL_TELEMETRY"); ok && !truthy(v, true) {
		return "RECALL_TELEMETRY=" + v
	}
	if v := os.Getenv("DO_NOT_TRACK"); truthy(v, false) {
		return "DO_NOT_TRACK is set"
	}
	if v := os.Getenv("POLIGN_CONFIDENTIAL"); truthy(v, false) {
		return "POLIGN_CONFIDENTIAL is set"
	}
	if v := os.Getenv("CI"); truthy(v, false) {
		return "running in CI"
	}
	if home == "" {
		return "no Recall home directory"
	}
	if _, err := os.Stat(filepath.Join(home, offFile)); err == nil {
		return "turned off with recall telemetry off"
	}
	return ""
}

// truthy reads a boolean environment value; anything unreadable counts as
// set, except empty, which is whatever empty says.
func truthy(v string, empty bool) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return empty
	}
	switch strings.ToLower(v) {
	case "off", "no":
		return false
	case "on", "yes":
		return true
	}
	b, err := strconv.ParseBool(v)
	return err != nil || b
}

// SetOff turns telemetry off (or back on) for everything that uses home.
func SetOff(home string, off bool) error {
	if home == "" {
		return errors.New("no Recall home directory: set RECALL_HOME")
	}
	marker := filepath.Join(home, offFile)
	if !off {
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	return os.WriteFile(marker, []byte("recall telemetry off\n"), 0o600)
}

// Preview is the payload a process described by cfg would send, with zero
// counters. It reads the install ID when one exists and never creates one.
func Preview(cfg Config) Payload {
	p := base(cfg)
	if b, err := os.ReadFile(filepath.Join(cfg.Home, idFile)); err == nil && validID(strings.TrimSpace(string(b))) {
		p.InstallID = strings.TrimSpace(string(b))
	}
	return p
}

func base(cfg Config) Payload {
	surface := "typed"
	if cfg.Text {
		surface = "text"
	}
	extractor := provider(cfg.Extractor)
	if extractor == "lexical" {
		extractor = "none"
	}
	return Payload{
		Version: cfg.Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Mode: label(cfg.Mode), Backend: label(cfg.Backend),
		Embedder: provider(cfg.Embedder), Extractor: extractor, Surface: surface,
	}
}

// provider keeps the part of a provider:model spec before the colon. A model
// name can be one a user made up, so it stays on the machine.
func provider(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, "lexical") || strings.EqualFold(spec, "none") {
		return "lexical"
	}
	name, _, _ := strings.Cut(spec, ":")
	if l := label(name); l != "" {
		return l
	}
	return "other"
}

// label reduces a name to lowercase letters, digits, dot, underscore and
// hyphen, at most 48 characters, starting with a letter or digit.
func label(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '_' || r == '-':
			if b.Len() > 0 {
				b.WriteRune(r)
			}
		case r == ' ' || r == '/':
			if b.Len() > 0 {
				b.WriteByte('-')
			}
		}
		if b.Len() == 48 {
			break
		}
	}
	return b.String()
}

// Reporter counts tool calls and reports them from a background loop. A nil
// Reporter is telemetry turned off, and every method is safe on it.
type Reporter struct {
	base     Payload
	started  time.Time
	endpoint string
	client   *http.Client

	clientName atomic.Pointer[string]
	counts     [6]atomic.Uint64

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

const (
	cRemember = iota
	cRecall
	cForget
	cReplace
	cInspect
	cState
)

// Start begins reporting for cfg and returns nil when telemetry is off or the
// install ID cannot be kept. notice is called once per install, the first
// time an ID is created. Nothing here blocks on the network.
func Start(cfg Config, notice func(string)) *Reporter {
	r := newReporter(cfg, notice)
	if r == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan struct{})
	go r.run(ctx)
	return r
}

// Once sends a single heartbeat for a command that does not stay running,
// such as setup, waiting at most two seconds.
func Once(cfg Config, notice func(string)) {
	r := newReporter(cfg, notice)
	if r == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeWait)
	defer cancel()
	r.send(ctx)
}

func newReporter(cfg Config, notice func(string)) *Reporter {
	if Disabled(cfg.Version, cfg.Home) != "" {
		return nil
	}
	id, created, err := installID(filepath.Join(cfg.Home, idFile))
	if err != nil {
		return nil
	}
	if created && notice != nil {
		notice(Notice)
	}
	var pid [8]byte
	if _, err := rand.Read(pid[:]); err != nil {
		return nil
	}
	p := base(cfg)
	p.InstallID, p.ProcessID = id, hex.EncodeToString(pid[:])
	return &Reporter{
		base: p, started: time.Now(), endpoint: Endpoint,
		client: &http.Client{
			Timeout: sendWait,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// SetClient records the MCP client's name, as its initialize request gave it.
func (r *Reporter) SetClient(name string) {
	if r == nil || r.base.Mode != "mcp" {
		return // a server has many clients, and no one of them names it
	}
	name = label(name)
	r.clientName.Store(&name)
}

// Tool counts one successful tool call. replaced says a remember call
// replaced something already believed.
func (r *Reporter) Tool(name string, replaced bool) {
	if r == nil {
		return
	}
	switch name {
	case "remember":
		r.counts[cRemember].Add(1)
		if replaced {
			r.counts[cReplace].Add(1)
		}
	case "recall":
		r.counts[cRecall].Add(1)
	case "forget":
		r.counts[cForget].Add(1)
	case "history", "memory_history", "explain", "timeline", "diff":
		r.counts[cInspect].Add(1)
	case "list_predicates":
	default:
		r.counts[cState].Add(1)
	}
}

// Snapshot is what would be sent now.
func (r *Reporter) Snapshot() Payload {
	p := r.base
	if name := r.clientName.Load(); name != nil {
		p.Client = *name
	}
	p.UptimeSeconds = uint64(time.Since(r.started) / time.Second)
	p.Remembers = r.counts[cRemember].Load()
	p.Recalls = r.counts[cRecall].Load()
	p.Forgets = r.counts[cForget].Load()
	p.Replacements = r.counts[cReplace].Load()
	p.Inspections = r.counts[cInspect].Load()
	p.StateCalls = r.counts[cState].Load()
	return p
}

// Close stops the loop and sends the final counters, waiting at most two
// seconds so a process that is offline still exits promptly.
func (r *Reporter) Close() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		if r.cancel != nil {
			r.cancel()
			<-r.done
		}
		ctx, cancel := context.WithTimeout(context.Background(), closeWait)
		defer cancel()
		r.send(ctx)
	})
}

// run reports shortly after start, once the client has introduced itself,
// and then every hour. A failed send is dropped; the next one carries the
// same cumulative counters.
func (r *Reporter) run(ctx context.Context) {
	defer close(r.done)
	timer := time.NewTimer(startDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		r.send(ctx)
		timer.Reset(interval)
	}
}

func (r *Reporter) send(ctx context.Context) bool {
	body, err := json.Marshal(r.Snapshot())
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "recall-telemetry")
	resp, err := r.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func validID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && id == strings.ToLower(id)
}

// installID reads the random install ID, creating it when there is none. It
// is 128 random bits and nothing about the machine.
func installID(file string) (id string, created bool, err error) {
	b, err := os.ReadFile(file)
	if err == nil {
		id = strings.TrimSpace(string(b))
		if !validID(id) {
			return "", false, fmt.Errorf("telemetry: invalid install ID file")
		}
		return id, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", false, err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", false, err
	}
	id = hex.EncodeToString(random[:])
	// O_EXCL so two processes starting together agree on one ID: the loser
	// reads the winner's.
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			time.Sleep(20 * time.Millisecond) // let the winner finish writing
			id, _, err = installID(file)
			return id, false, err
		}
		return "", false, err
	}
	_, werr := f.WriteString(id + "\n")
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(file)
		return "", false, err
	}
	return id, true, nil
}
