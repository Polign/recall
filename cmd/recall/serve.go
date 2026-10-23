package main

// The `recall serve` subcommand: Recall's own front door. It serves the same
// memory tools as `recall mcp`, over HTTP, to many callers at once, each
// inside the namespace its credential names.
//
// A namespace is the unit of isolation. Each one gets its own collection in
// the backend, derived from the namespace name, so no filter stands between
// two tenants' records: a caller's key selects a collection, and nothing the
// caller sends can select another. The backend's own credentials never leave
// this process.
//
// Two surfaces share one dispatcher. POST /mcp is MCP over streamable HTTP:
// one JSON-RPC request per POST, one JSON response, notifications accepted
// with 202. POST /v1/<verb> takes the tool's arguments as the body and answers
// with the tool's result. Both check the credential first and refuse a request
// that names a namespace, agent or session the credential does not carry.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Polign/recall"
)

// principal is who a credential says is calling.
type principal struct {
	// Namespace is the only namespace this caller reaches.
	Namespace string
	// Agent, when set, is the only agent_id this caller may use; the server
	// fixes it from the workload identity in Kubernetes mode.
	Agent string
	// Session names the run, when the credential carries one.
	Session string
	// Admin keys manage keys and reach no namespace.
	Admin bool
	// KeyID is the id of the key presented, when a key store issued it.
	KeyID string
}

// authenticator turns a bearer credential into a principal.
type authenticator interface {
	Authenticate(ctx context.Context, credential string) (principal, error)
}

var errUnauthenticated = errors.New("unauthenticated")

// fileKeys is the simplest authenticator: keys listed in a file, one per
// line as "<key> <namespace>", with blank lines and # comments ignored. It
// serves single-operator installs and tests; the store-backed key set with
// expiry and revocation is layered on the same interface.
type fileKeys struct {
	byHash map[string]principal
}

func loadFileKeys(path string) (*fileKeys, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	keys := &fileKeys{byHash: map[string]principal{}}
	for n, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s:%d: want \"<key> <namespace>\"", path, n+1)
		}
		if err := validateNamespace(fields[1]); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n+1, err)
		}
		keys.byHash[hashCredential(fields[0])] = principal{Namespace: fields[1]}
	}
	if len(keys.byHash) == 0 {
		return nil, fmt.Errorf("%s: no keys", path)
	}
	return keys, nil
}

func (k *fileKeys) Authenticate(_ context.Context, credential string) (principal, error) {
	p, ok := k.byHash[hashCredential(credential)]
	if !ok {
		return principal{}, errUnauthenticated
	}
	return p, nil
}

func hashCredential(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// maxNamespaceLen matches polign_db's limit, so a namespace that is valid
// here is valid as a key binding there too.
const maxNamespaceLen = 128

// validateNamespace accepts letters, digits and "_-.:", starting with a
// letter or digit: the same rule polign_db applies to key namespaces.
func validateNamespace(ns string) error {
	if ns == "" {
		return errors.New("namespace: empty")
	}
	if len(ns) > maxNamespaceLen {
		return fmt.Errorf("namespace: %d bytes exceeds the %d byte limit", len(ns), maxNamespaceLen)
	}
	for i := 0; i < len(ns); i++ {
		c := ns[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '-', c == '.', c == ':':
			if i == 0 {
				return fmt.Errorf("namespace %q: must start with a letter or digit", ns)
			}
		default:
			return fmt.Errorf("namespace %q: byte %d is not allowed (use letters, digits, and _-.:)", ns, i)
		}
	}
	return nil
}

// namespaceCollection names the collection holding one namespace's memory.
// The name uses only lowercase letters, digits and underscores, which every
// backend accepts, and ends in a hash of the namespace so that two namespaces
// that sanitise alike ("a.b" and "a_b") never share a collection.
func namespaceCollection(base, ns string) string {
	var b strings.Builder
	for i := 0; i < len(ns); i++ {
		c := ns[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 'a' - 'A')
		default:
			b.WriteByte('_')
		}
	}
	sum := sha256.Sum256([]byte(ns))
	return base + "__" + b.String() + "_" + hex.EncodeToString(sum[:4])
}

// tenantServers hands out one MCP server per namespace, built on first use
// and kept for the life of the process.
type tenantServers struct {
	mu   sync.Mutex
	byNS map[string]*mcpServer
	make func(ns string) (*mcpServer, error)
}

func (t *tenantServers) get(ns string) (*mcpServer, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s, ok := t.byNS[ns]; ok {
		return s, nil
	}
	s, err := t.make(ns)
	if err != nil {
		return nil, err
	}
	t.byNS[ns] = s
	return s, nil
}

func (t *tenantServers) releaseAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.byNS {
		if s.agents != nil {
			s.agents.releaseAll()
		}
	}
}

// serveConfig is everything cmdServe resolves before listening.
type serveConfig struct {
	backend   recall.Backend
	registry  recall.Registry
	embedder  recall.Embedder
	extractor recall.Extractor
	base      string // collection name prefix
	open      bool
	text      bool
	agents    bool // expose the resume tools when the backend has leases
}

// newTenantServers builds the per-namespace server factory. Every namespace
// shares the backend, registry, embedder and extractor; only the collection
// differs.
func newTenantServers(cfg serveConfig) *tenantServers {
	_, leases := cfg.backend.(recall.LeaseBackend)
	return &tenantServers{byNS: map[string]*mcpServer{}, make: func(ns string) (*mcpServer, error) {
		collection := namespaceCollection(cfg.base, ns)
		client, err := recall.NewClient(recall.Config{Backend: cfg.backend, Collection: collection, Registry: cfg.registry, Embedder: cfg.embedder, Materialize: true, Open: cfg.open})
		if err != nil {
			return nil, err
		}
		mem := &memoryRuntime{client: client, backend: cfg.backend, ctx: context.Background(), extractor: cfg.extractor, open: cfg.open, text: cfg.text}
		s := &mcpServer{collection: collection, write: true, memory: mem}
		if cfg.agents && leases {
			s.agents = newAgentRuntime()
		}
		return s, nil
	}}
}

// frontDoor is the HTTP handler: authentication, then one of the two
// surfaces.
type frontDoor struct {
	auth    authenticator
	tenants *tenantServers
	// keys is the store behind the /v1/keys endpoints; nil serves none.
	keys *keyStore
	// kube exchanges Kubernetes workload tokens for keys; nil serves none.
	kube *kubeAuth
	mux  *http.ServeMux
}

func newFrontDoor(auth authenticator, tenants *tenantServers, keys *keyStore) *frontDoor {
	f := &frontDoor{auth: auth, tenants: tenants, keys: keys, mux: http.NewServeMux()}
	f.mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	f.mux.HandleFunc("/v1/auth/kubernetes", f.handleKubernetesAuth)
	f.mux.Handle("/mcp", f.authenticated(f.handleMCP))
	f.mux.Handle("/v1/", f.authenticated(f.handleREST))
	return f
}

func (f *frontDoor) ServeHTTP(w http.ResponseWriter, r *http.Request) { f.mux.ServeHTTP(w, r) }

// maxBody bounds a request body. remember takes 32 KiB of text and 32
// statements; four mebibytes leaves room for a large recorded turn.
const maxBody = 4 << 20

type httpError struct {
	status  int
	code    string
	message string
}

func (e *httpError) Error() string { return e.code + ": " + e.message }

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message}})
}

// authenticated wraps a handler with bearer authentication and hands it the
// caller's principal.
func (f *frontDoor) authenticated(next func(w http.ResponseWriter, r *http.Request, p principal)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, credential, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		credential = strings.TrimSpace(credential)
		if !strings.EqualFold(scheme, "Bearer") || credential == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="recall"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "send a Recall key as Authorization: Bearer <key>")
			return
		}
		p, err := f.auth.Authenticate(r.Context(), credential)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="recall"`)
			writeError(w, http.StatusUnauthorized, "unauthenticated", "the key is not known or no longer valid")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next(w, r, p)
	})
}

// tenant opens the caller's namespace server, refusing an admin key, which
// reaches no namespace. It reports whether the handler may go on.
func (f *frontDoor) tenant(w http.ResponseWriter, p principal) (*mcpServer, bool) {
	if p.Admin || p.Namespace == "" {
		writeError(w, http.StatusForbidden, "namespace_mismatch", "an admin key manages keys and reaches no namespace")
		return nil, false
	}
	s, err := f.tenants.get(p.Namespace)
	if err != nil {
		log.Printf("serve: namespace %s: %v", p.Namespace, err)
		writeError(w, http.StatusInternalServerError, "internal", "could not open the namespace")
		return nil, false
	}
	return s, true
}

// checkIdentity enforces the identity envelope on tool arguments: a request
// may repeat the namespace, agent or session its credential carries, and is
// refused when it names another. Fields that match are removed, so the tools
// never see them. The returned arguments are what the tool runs with.
func checkIdentity(p principal, args json.RawMessage) (json.RawMessage, *httpError) {
	if len(args) == 0 {
		return args, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return nil, &httpError{http.StatusBadRequest, "invalid_argument", "arguments must be a JSON object"}
	}
	check := func(field, want string, strip bool) *httpError {
		raw, ok := m[field]
		if !ok {
			return nil
		}
		var got string
		if err := json.Unmarshal(raw, &got); err != nil {
			return &httpError{http.StatusBadRequest, "invalid_argument", field + " must be a string"}
		}
		if want != "" && got != want {
			return &httpError{http.StatusForbidden, "namespace_mismatch", fmt.Sprintf("the credential does not carry %s %q", field, got)}
		}
		if strip {
			delete(m, field)
		}
		return nil
	}
	if err := check("namespace", p.Namespace, true); err != nil {
		return nil, err
	}
	if err := check("session", p.Session, true); err != nil {
		return nil, err
	}
	// agent_id stays in the arguments: the resume tools need it. It is only
	// checked when the credential fixes the agent.
	if err := check("agent_id", p.Agent, false); err != nil {
		return nil, err
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, &httpError{http.StatusBadRequest, "invalid_argument", err.Error()}
	}
	return out, nil
}

// handleMCP is MCP over streamable HTTP, in its simplest conforming form:
// a client POSTs one JSON-RPC message and receives one JSON response. There
// is no server-initiated stream, so GET answers 405, and no session state,
// so DELETE is a no-op.
func (f *frontDoor) handleMCP(w http.ResponseWriter, r *http.Request, p principal) {
	s, ok := f.tenant(w, p)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST one JSON-RPC message")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_argument", err.Error())
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: codeParse, Message: err.Error()}})
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.Method == "tools/call" {
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &call); err != nil {
			writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: codeInvalidParams, Message: err.Error()}})
			return
		}
		args, herr := checkIdentity(p, call.Arguments)
		if herr != nil {
			// A refused identity is a tool result, as every tool failure is,
			// so the model reads the reason.
			writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: toolResult(herr.Error(), true)})
			return
		}
		params, _ := json.Marshal(map[string]any{"name": call.Name, "arguments": args})
		req.Params = params
	}
	result, rerr := s.dispatch(withSession(r.Context(), p.Session), req.Method, req.Params)
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rerr})
}

// restTools maps the REST verbs to the tools behind them.
var restTools = map[string]string{
	"remember":   "remember",
	"recall":     "recall",
	"forget":     "forget",
	"history":    "memory_history",
	"predicates": "list_predicates",
	"explain":    "explain",
	"diff":       "diff",
	"timeline":   "timeline",
}

// handleREST serves POST /v1/<verb> with the tool's arguments as the body.
// The answer is the tool's result: JSON when the tool returned JSON, which
// the memory tools do, and {"text": ...} otherwise.
func (f *frontDoor) handleREST(w http.ResponseWriter, r *http.Request, p principal) {
	verb := strings.TrimPrefix(r.URL.Path, "/v1/")
	if verb == "keys" || strings.HasPrefix(verb, "keys/") {
		f.handleKeys(w, r, p, strings.TrimPrefix(verb, "keys"))
		return
	}
	tool, ok := restTools[verb]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "unknown operation; want one of remember, recall, forget, history, timeline, diff, predicates, explain")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST the operation's arguments as JSON")
		return
	}
	s, ok := f.tenant(w, p)
	if !ok {
		return
	}
	if tool == "memory_history" && s.memory != nil && s.memory.text {
		tool = "history"
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_argument", err.Error())
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}")
	}
	args, herr := checkIdentity(p, body)
	if herr != nil {
		writeError(w, herr.status, herr.code, herr.message)
		return
	}
	out, err := s.runTool(r.Context(), tool, args)
	if err != nil {
		code := "tool_error"
		if strings.HasPrefix(err.Error(), "arguments:") {
			code = "invalid_argument"
		}
		writeError(w, http.StatusBadRequest, code, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if json.Valid([]byte(out)) {
		_, _ = io.WriteString(w, out)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"text": out})
}

// handleKeys serves key management. Admin keys create, list and revoke;
// any key renews itself with POST /v1/keys/renew.
func (f *frontDoor) handleKeys(w http.ResponseWriter, r *http.Request, p principal, rest string) {
	if f.keys == nil {
		writeError(w, http.StatusNotFound, "not_found", "this server keeps no key store; keys come from -keys-file")
		return
	}
	rest = strings.Trim(rest, "/")
	if rest == "renew" {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST to renew")
			return
		}
		if p.KeyID == "" {
			writeError(w, http.StatusBadRequest, "invalid_argument", "this credential is not a stored key")
			return
		}
		rec, err := f.keys.renew(r.Context(), p.KeyID)
		if errors.Is(err, errRenewWithToken) {
			writeError(w, http.StatusConflict, "renew_with_token", err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, keyView(rec))
		return
	}
	if !p.Admin {
		writeError(w, http.StatusForbidden, "forbidden", "key management needs an admin key")
		return
	}
	switch {
	case rest == "" && r.Method == http.MethodPost:
		var req struct {
			Namespace string `json:"namespace"`
			Agent     string `json:"agent"`
			Session   string `json:"session"`
			Note      string `json:"note"`
			Admin     bool   `json:"admin"`
			ExpiresIn string `json:"expires_in"`
		}
		if err := strictArgs(mustReadAll(r.Body), &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
			return
		}
		var ttl time.Duration
		if req.ExpiresIn != "" {
			d, err := time.ParseDuration(req.ExpiresIn)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_argument", "expires_in: "+err.Error())
				return
			}
			ttl = d
		}
		token, rec, err := f.keys.create(r.Context(), req.Namespace, req.Agent, req.Session, req.Note, req.Admin, ttl)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_argument", err.Error())
			return
		}
		view := keyView(rec)
		view["key"] = token
		writeJSON(w, http.StatusCreated, view)
	case rest == "" && r.Method == http.MethodGet:
		recs, err := f.keys.list(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		views := make([]map[string]any, 0, len(recs))
		for _, rec := range recs {
			views = append(views, keyView(rec))
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": views})
	case rest != "" && r.Method == http.MethodDelete:
		if err := f.keys.revoke(r.Context(), rest); err != nil {
			writeError(w, http.StatusNotFound, "not_found", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST or GET /v1/keys, DELETE /v1/keys/<id>, POST /v1/keys/renew")
	}
}

func mustReadAll(r io.Reader) json.RawMessage {
	b, _ := io.ReadAll(r)
	if len(strings.TrimSpace(string(b))) == 0 {
		return json.RawMessage("{}")
	}
	return b
}

// keyView is what a key looks like over HTTP: everything but the hash.
func keyView(rec keyRecord) map[string]any {
	v := map[string]any{"id": rec.ID, "namespace": rec.Namespace, "admin": rec.Admin, "revoked": rec.Revoked, "created_at": rec.CreatedAt.UTC().Format(time.RFC3339)}
	if rec.Agent != "" {
		v["agent"] = rec.Agent
	}
	if rec.Session != "" {
		v["session"] = rec.Session
	}
	if rec.Note != "" {
		v["note"] = rec.Note
	}
	if !rec.ExpiresAt.IsZero() {
		v["expires_at"] = rec.ExpiresAt.UTC().Format(time.RFC3339)
		v["ttl"] = rec.TTL.String()
		v["renewable"] = rec.RenewWith == ""
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// cmdServe runs the front door.
func cmdServe(global *dbFlags, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	sub := addDatabaseFlags(fs)
	listen := fs.String("listen", envOr("RECALL_LISTEN", ":8080"), "address to listen on (default $RECALL_LISTEN, then :8080)")
	keysFile := fs.String("keys-file", os.Getenv("RECALL_KEYS_FILE"), "also accept the keys in this file, one \"<key> <namespace>\" per line (default $RECALL_KEYS_FILE)")
	bootstrapKey := fs.String("bootstrap-key-file", os.Getenv("RECALL_BOOTSTRAP_KEY_FILE"), "file holding one admin key to register at startup if the store does not know it, so an install driven from manifests can mint keys over HTTP (default $RECALL_BOOTSTRAP_KEY_FILE)")
	cacheTTL := fs.Duration("auth-cache-ttl", 30*time.Second, "how long a key check is reused; a revoked or expired key is refused within this time")
	kube := fs.Bool("kubernetes", os.Getenv("RECALL_KUBERNETES") == "1", "exchange projected Kubernetes service account tokens for keys at POST /v1/auth/kubernetes (default on when $RECALL_KUBERNETES is 1)")
	kubeAPI := fs.String("kubernetes-api", os.Getenv("RECALL_KUBERNETES_API"), "API server URL (default from KUBERNETES_SERVICE_HOST and _PORT)")
	kubeCA := fs.String("kubernetes-ca-file", envOr("RECALL_KUBERNETES_CA_FILE", defaultKubeCAFile), "CA bundle for the API server")
	kubeToken := fs.String("kubernetes-token-file", envOr("RECALL_KUBERNETES_TOKEN_FILE", defaultKubeTokenFile), "this server's own service account token, for TokenReview")
	kubeAudience := fs.String("kubernetes-audience", envOr("RECALL_KUBERNETES_AUDIENCE", defaultKubeAudience), "audience a pod's projected token must carry")
	kubeTenantLabel := fs.String("kubernetes-tenant-label", os.Getenv("RECALL_KUBERNETES_TENANT_LABEL"), "label on the service account that names the tenant; empty uses the service account's namespace")
	kubeKeyTTL := fs.Duration("kubernetes-key-ttl", defaultKubeKeyTTL, "lifetime of a key minted for a pod; the pod renews it")
	kubeSandboxClaims := fs.Bool("kubernetes-sandbox-claims", os.Getenv("RECALL_KUBERNETES_SANDBOX_CLAIMS") == "1", "with agent-sandbox, the agent is the name of the SandboxClaim that owns the pod's Sandbox (pod -> Sandbox -> SandboxClaim); a Sandbox with no claim falls back to -kubernetes-agent-label, then the pod's name. Needs get on pods, sandboxes and sandboxclaims (default on when $RECALL_KUBERNETES_SANDBOX_CLAIMS is 1)")
	kubeAgentLabel := fs.String("kubernetes-agent-label", os.Getenv("RECALL_KUBERNETES_AGENT_LABEL"), "label on the pod that names the agent, for pods handed out by a pool under generated names; empty uses the pod's name. The namespace stays <tenant>:<agent>")
	collection := fs.String("collection", settingOr("COLLECTION", "recall_lexical_v1"), "collection name prefix; each namespace gets its own collection under it")
	predicates := fs.String("predicates", setting("PREDICATES"), "predicate registry JSON file; omitted uses the starter registry")
	embedURL := fs.String("embed-url", setting("EMBED_URL"), "embedding service that answers POST /embed {text} with {values}")
	embedModel := fs.String("embed-model", setting("EMBED_MODEL"), "model that embeds memory so recall searches by meaning, as provider:model[@dim]: openai:text-embedding-3-small@512, ollama:nomic-embed-text, or voyage:voyage-3.5. Omitted is word search; unlike recall mcp, serve does not follow -extract-model, because every tenant's collection was written with the embedder it was started with (default $RECALL_EMBED_MODEL)")
	extractModel := fs.String("extract-model", setting("EXTRACT_MODEL"), "model that works out the statements in text remembered without any, as provider:model")
	text := fs.Bool("text", true, "serve the text-only tools (the default)")
	typed := fs.Bool("typed", os.Getenv("RECALL_TYPED") == "1", "serve the typed tools instead (default on when $RECALL_TYPED is 1)")
	open := fs.Bool("open", os.Getenv("RECALL_OPEN") == "1", "let writes define predicates; the text tools do unless -predicates is given, the typed tools only with this flag (default on when $RECALL_OPEN is 1)")
	agent := fs.Bool("agent", true, "expose the resume tools when the backend provides leases")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	var auth authChain
	if *keysFile != "" {
		fileAuth, err := loadFileKeys(*keysFile)
		if err != nil {
			return fmt.Errorf("serve: keys: %w", err)
		}
		auth = append(auth, fileAuth)
	}
	serveText, openAll, err := surface(fs, *text, *typed, *open, *predicates)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	registry := recall.DefaultRegistry()
	if openAll && *predicates == "" {
		registry = openSeeds()
	}
	if *predicates != "" {
		raw, err := readFileTrimmed(*predicates)
		if err != nil {
			return fmt.Errorf("serve: predicates: %w", err)
		}
		if registry, err = recall.LoadRegistry(raw); err != nil {
			return fmt.Errorf("serve: predicates: %w", err)
		}
	}
	emb, err := chooseEmbedding(*embedURL, *embedModel, "")
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	embedder := emb.Embedder
	extractor, err := newExtractor(*extractModel)
	if err != nil {
		return fmt.Errorf("serve: -extract-model: %w", err)
	}
	d, err := resolveDatabase(global, sub, recallHome(), false)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	c, err := d.connect(context.Background())
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	// The first log line says where memory is, so an install that meant to
	// name a database and did not sees that it is writing to a local
	// directory.
	log.Printf("serve: memory in %s (%s)", d, d.sources())
	name := d.Backend
	backend, err := openMemoryBackend(c)
	if err != nil {
		return fmt.Errorf("serve: %s: %w", d, err)
	}
	tenants := newTenantServers(serveConfig{backend: backend, registry: registry, embedder: embedder, extractor: extractor, base: *collection, open: openAll, text: serveText, agents: *agent})
	_, leases := backend.(recall.LeaseBackend)
	if *agent && !leases {
		log.Printf("serve: the %s backend has no leases; the resume tools are not served", name)
	}
	var keys *keyStore
	if _, ok := backend.(recall.GetBackend); ok {
		keys = newKeyStore(backend, *collection, *cacheTTL)
		auth = append(auth, keys)
		if *bootstrapKey != "" {
			raw, err := readFileTrimmed(*bootstrapKey)
			if err != nil {
				return fmt.Errorf("serve: bootstrap key: %w", err)
			}
			rec, created, err := keys.adopt(context.Background(), string(raw), "", "bootstrap admin key", true)
			if err != nil {
				return fmt.Errorf("serve: bootstrap key: %w", err)
			}
			if created {
				log.Printf("serve: registered bootstrap admin key %s", rec.ID)
			} else if !rec.Admin {
				return fmt.Errorf("serve: bootstrap key %s is on record without admin rights; it will not be rebound", rec.ID)
			}
		}
	} else if *bootstrapKey != "" {
		return fmt.Errorf("serve: the %s backend cannot read records by id, so it keeps no key store", name)
	}
	if len(auth) == 0 {
		return errors.New("serve: no way to authenticate callers; the backend keeps no key store, so give -keys-file")
	}
	var kubeExchange *kubeAuth
	if *kube {
		if keys == nil {
			return fmt.Errorf("serve: -kubernetes needs the key store, which the %s backend cannot keep", name)
		}
		kubeExchange, err = newKubeAuth(kubeAuthConfig{apiServer: *kubeAPI, caFile: *kubeCA, tokenFile: *kubeToken, audience: *kubeAudience, tenantLabel: *kubeTenantLabel, agentLabel: *kubeAgentLabel, sandboxClaims: *kubeSandboxClaims, keyTTL: *kubeKeyTTL}, keys)
		if err != nil {
			return fmt.Errorf("serve: -kubernetes: %w", err)
		}
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	door := newFrontDoor(auth, tenants, keys)
	door.kube = kubeExchange
	srv := &http.Server{Handler: door, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("recall serve %s listening on %s: backend %s, resume tools %v", version, ln.Addr(), name, *agent && leases)
	if keys != nil {
		log.Printf("serve: key store in collection %s; a revoked or expired key is refused within %s", keys.collection, *cacheTTL)
	}
	if kubeExchange != nil {
		tenantFrom := "the service account's namespace"
		if *kubeTenantLabel != "" {
			tenantFrom = "the service account's " + *kubeTenantLabel + " label"
		}
		agentFrom := "the pod's name"
		if *kubeAgentLabel != "" {
			agentFrom = "the pod's " + *kubeAgentLabel + " label, else its name"
		}
		if *kubeSandboxClaims {
			agentFrom = "the SandboxClaim that owns the pod's Sandbox, else " + agentFrom
		}
		log.Printf("serve: kubernetes workload tokens with audience %q exchanged for %s keys; tenant from %s; agent from %s", *kubeAudience, *kubeKeyTTL, tenantFrom, agentFrom)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdown)
	tenants.releaseAll()
	return err
}
