package main

// The `recall mcp` subcommand: a Model Context Protocol server that gives an
// MCP client such as Claude Code typed, correctable memory kept in a
// polign_db instance.
//
// It speaks JSON-RPC 2.0 over stdio, which is the MCP stdio transport, and
// reaches polign_db only through its HTTP API. Nothing but protocol messages
// may reach stdout, so every diagnostic goes to stderr through the standard
// logger.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Polign/recall"
)

// mcpVersions are the protocol revisions this server speaks, newest first.
// The negotiated version is the client's when it names one we know, and the
// newest we know otherwise.
var mcpVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// JSON-RPC 2.0 error codes.
const (
	codeParse          = -32700
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

// rpcRequest is an incoming call. A request without an id is a notification
// and gets no response.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcpTool is one entry of a tools/list response.
type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

// readOnlyTool annotates the tools that cannot change anything, so a client
// can show them as safe without parsing prose.
var readOnlyTool = map[string]any{"readOnlyHint": true, "openWorldHint": false}

// mcpServer serves the memory tools over stdio. The encoder is guarded
// because requests are handled concurrently: a slow cold read must not block
// a ping.
type mcpServer struct {
	api        *api
	collection string // the memory collection
	// search names the embedder when memory is searched by meaning; empty
	// is word search.
	search string
	write  bool // expose the writing tools
	memory *memoryRuntime
	// agents holds the agents this session resumed; nil unless -agent.
	agents *agentRuntime

	mu  sync.Mutex
	enc *json.Encoder
}

// cmdMCP serves memory over MCP. With a server named by -url or POLIGN_URL it
// connects there, configured by its flags. Without one it serves what recall
// setup configured, starting the managed local server if that is the setup.
func cmdMCP(c *api, urlGiven bool, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	collection := fs.String("collection", os.Getenv("POLIGN_COLLECTION"), "memory collection (default $POLIGN_COLLECTION, then recall_lexical_v1)")
	write := fs.Bool("write", false, "also expose the writing tools; read-only by default")
	predicates := fs.String("predicates", os.Getenv("POLIGN_PREDICATES"), "predicate registry JSON file; omitted uses the 15-predicate starter registry")
	embedURL := fs.String("embed-url", os.Getenv("POLIGN_EMBED_URL"), "embedding service that answers POST /embed {text} with {values}; needs an explicit -collection")
	embedModel := fs.String("embed-model", os.Getenv("POLIGN_EMBED_MODEL"), "model that embeds memory so recall searches by meaning, as provider:model[@dim]: openai:text-embedding-3-small@512, ollama:nomic-embed-text, voyage:voyage-3.5, or lexical for word search. Omitted follows -extract-model: openai:* uses openai:text-embedding-3-small@512, ollama:* uses ollama:nomic-embed-text, anthropic:* and no model use word search (default $POLIGN_EMBED_MODEL)")
	extractModel := fs.String("extract-model", os.Getenv("POLIGN_EXTRACT_MODEL"), "model that works out the statements in text remembered without any, as provider:model: anthropic:claude-opus-5-5, openai:<model>, or ollama:<model> (default $POLIGN_EXTRACT_MODEL; keys from ANTHROPIC_API_KEY or OPENAI_API_KEY)")
	text := fs.Bool("text", true, "serve the text-only tools: remember(text), recall(question), forget(text), with no predicates in them. The server works out what text says with -extract-model; without one, text is kept as written and nothing replaces anything. This is the default; -typed serves the typed tools instead")
	typed := fs.Bool("typed", os.Getenv("RECALL_TYPED") == "1", "serve the typed tools instead: remember(subject, predicate, value), list_predicates, memory_history, explain, over the starter registry or -predicates (default on when $RECALL_TYPED is 1)")
	open := fs.Bool("open", os.Getenv("RECALL_OPEN") == "1", "let writes define predicates: a predicate nothing has defined is defined by its first use. The text tools do this unless -predicates is given; the typed tools only with this flag (default on when $RECALL_OPEN is 1)")
	agent := fs.Bool("agent", false, "also expose the agent resume tools (agent_resume, update_working_state, record_turn, ...); requires -write")
	dir := fs.String("config-dir", recallHome(), "configuration written by recall setup, used when no server URL is given (default $POLIGN_RECALL_HOME, then ~/.config/polign/recall)")
	// polign mcp took -memory-only; recall serves nothing else.
	_ = fs.Bool("memory-only", true, "accepted for compatibility with polign mcp; always on")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	// An explicit -config-dir means the saved connection, even when the
	// environment also names a server: the setup launcher passes it so a
	// stray POLIGN_URL cannot redirect Claude's memory.
	explicitDir := false
	fs.Visit(func(f *flag.Flag) { explicitDir = explicitDir || f.Name == "config-dir" })
	// recall setup saves a connection to its own backend only.
	setupApplies := c.backend == "" || c.backend == setupBackend
	if explicitDir && !setupApplies {
		return fmt.Errorf("mcp: -config-dir holds a %s setup; with -backend %s, name the server with -url", setupBackend, c.backend)
	}
	if explicitDir {
		return serveConfigured(*dir)
	}
	if !urlGiven && setupApplies {
		// Without a server named, serve what recall setup saved.
		if _, err := os.Stat(filepath.Join(*dir, "config.json")); err == nil {
			return serveConfigured(*dir)
		}
	}
	// With nothing named or saved, the memory runtime opens the backend's
	// default address.

	serveText, openAll, err := surface(fs, *text, *typed, *open, *predicates)
	if err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	extract := *extractModel
	if !serveText {
		extract = "" // the typed tools extract nothing, so nothing to follow
	}
	emb, err := chooseEmbedding(*embedURL, *embedModel, extract)
	if err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	backend, err := openMemoryBackend(c)
	if err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	if emb, err = fitMCPCollection(backend, collection, emb); err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	mem, err := newMemoryRuntime(c, *collection, *predicates, emb, openAll)
	if err != nil {
		return fmt.Errorf("mcp: memory: %w", err)
	}
	if mem.extractor, err = newExtractor(*extractModel); err != nil {
		return fmt.Errorf("mcp: -extract-model: %w", err)
	}
	mem.text = serveText
	s := &mcpServer{api: c, collection: *collection, search: emb.spec, write: *write, memory: mem, enc: json.NewEncoder(os.Stdout)}
	if *agent {
		if !*write {
			return errors.New("mcp: -agent requires -write")
		}
		if _, ok := mem.backend.(recall.LeaseBackend); !ok {
			return fmt.Errorf("mcp: -agent needs agent leases, which the %s backend does not provide", c.backend)
		}
		s.agents = newAgentRuntime()
	}
	err = s.serve(context.Background(), os.Stdin)
	if s.agents != nil {
		s.agents.releaseAll()
	}
	if err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	return nil
}

// fitMCPCollection settles the collection and the embedder together. With
// no collection named, each embedder writes its own (defaultCollection); a
// model embedder that only followed the extraction model stays out of the
// way of memory already kept in the default lexical collection, so an
// upgrade never hides what was remembered. A named collection must hold
// vectors the embedder makes (fitEmbedding). Notes go to stderr, which MCP
// clients log.
func fitMCPCollection(backend recall.Backend, collection *string, emb embedding) (embedding, error) {
	if *collection == "" && strings.HasPrefix(emb.spec, "url:") {
		return emb, errors.New("-embed-url requires an explicit -collection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if *collection == "" {
		*collection = defaultCollection(emb.spec)
		if emb.derived {
			old, _ := storedDim(ctx, backend, defaultLexicalCollection)
			now, _ := storedDim(ctx, backend, *collection)
			if old > 0 && now == 0 {
				fmt.Fprintf(os.Stderr, "recall: memory is in %s, which keeps word search. To search it by meaning with %s, copy it: "+
					"recall reembed -from %s -to %s -embed-model %s\n", defaultLexicalCollection, emb.spec,
					defaultLexicalCollection, *collection, emb.spec)
				*collection = defaultLexicalCollection
				return lexical, nil
			}
		}
	}
	fitted, note, err := fitEmbedding(ctx, backend, *collection, emb)
	if note != "" {
		fmt.Fprintln(os.Stderr, "recall: "+note)
	}
	return fitted, err
}

// surface works out which tools to serve and whether writes may define
// predicates. The text tools are the default, with writes defining
// predicates unless a predicates file fixes the vocabulary. -typed serves
// the typed tools over the fixed vocabulary (the starter registry or the
// predicates file), as every release before 0.18 did; -open lets writes
// define predicates on either surface. Asking for both surfaces is a mistake.
func surface(fs *flag.FlagSet, text, typed, open bool, predicates string) (serveText, openAll bool, err error) {
	textGiven := false
	fs.Visit(func(f *flag.Flag) { textGiven = textGiven || f.Name == "text" })
	if textGiven && text && typed {
		return false, false, errors.New("-text and -typed name different tools; pass one")
	}
	serveText = text && !typed
	return serveText, open || serveText && predicates == "", nil
}

// serve reads requests until the client closes stdin.
func (s *mcpServer) serve(ctx context.Context, r io.Reader) error {
	dec := json.NewDecoder(bufio.NewReader(r))
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		var req rpcRequest
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			// The decoder cannot resynchronise mid-value, so a malformed
			// message ends the session. Say so on the way out.
			s.reply(&rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{Code: codeParse, Message: err.Error()}})
			return fmt.Errorf("read: %w", err)
		}
		if len(req.ID) == 0 {
			// A notification (notifications/initialized, and the cancellation
			// of work we do not report progress on) wants no answer.
			continue
		}
		wg.Add(1)
		go func(req rpcRequest) {
			defer wg.Done()
			result, rerr := s.dispatch(ctx, req.Method, req.Params)
			s.reply(&rpcResponse{ID: req.ID, Result: result, Error: rerr})
		}(req)
	}
}

func (s *mcpServer) reply(resp *rpcResponse) {
	resp.JSONRPC = "2.0"
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enc.Encode(resp); err != nil {
		log.Printf("mcp: write response: %v", err)
	}
}

func (s *mcpServer) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return s.initialize(params), nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools()}, nil
	case "tools/call":
		return s.callTool(ctx, params)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: fmt.Sprintf("unsupported method %q", method)}
	}
}

func (s *mcpServer) initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	negotiated := mcpVersions[0]
	if slices.Contains(mcpVersions, p.ProtocolVersion) {
		negotiated = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": negotiated,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "recall", "version": version},
		"instructions":    s.instructions(),
	}
}

// instructions tell the model what this server remembers and whether it
// may write.
func (s *mcpServer) instructions() string {
	instructions := "Recall agent memory. Search uses lexical word overlap, so ask with the words the memory would use."
	if s.search != "" {
		by := s.search
		if strings.HasPrefix(by, "url:") {
			by = "an embedding service" // the operator's address is not the model's business
		}
		instructions = "Recall agent memory. Search matches meaning (" + by + ") as well as words."
	}
	if !s.write {
		instructions += " This server is read-only."
	}
	return instructions + s.memoryInstructions()
}

func (s *mcpServer) tools() []mcpTool { return s.memoryTools() }

func schema(props map[string]any, required ...string) map[string]any {
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// --- tool calls ---------------------------------------------------------------

func (s *mcpServer) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	out, err := s.runTool(ctx, p.Name, p.Arguments)
	if err != nil {
		// A failed tool call is a result, not a protocol error: the model
		// reads the message and can correct the call itself.
		return toolResult(err.Error(), true), nil
	}
	return toolResult(out, false), nil
}

func (s *mcpServer) runTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if out, handled, err := s.runMemoryTool(ctx, name, args); handled {
		return out, err
	}
	return "", fmt.Errorf("unknown memory tool %q", name)
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
}

func marshal(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
