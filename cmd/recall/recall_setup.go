package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Polign/recall/backend/local"
	"github.com/Polign/recall/internal/recallsetup"
	"golang.org/x/term"
)

// cmdRecall is recall setup, recall doctor, and the recall mcp -config-dir the
// plugin launcher runs. setup saves where memory lives (any backend), the
// collection and the models, checks the connection, and installs the Claude
// plugin; doctor reports the database the flags, environment and saved setup
// resolve to and checks it.
func cmdRecall(global *dbFlags, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: recall <setup|doctor|mcp> [options]")
	}
	fs := flag.NewFlagSet("recall "+args[0], flag.ContinueOnError)
	dir := fs.String("config-dir", recallHome(), "where Recall keeps its configuration and, for the local backend, its data (default $RECALL_HOME, then ~/.local/share/recall)")
	sub := addDatabaseFlags(fs)
	localFlag := fs.Bool("local", false, "keep memory in the local database, the same as -backend local")
	collection := fs.String("collection", "", "memory collection (default $RECALL_COLLECTION, then one named after the embedding model: recall_lexical_v1 for word search)")
	noPlugin := fs.Bool("no-plugin", false, "configure and check Recall without installing the Claude plugin")
	claude := fs.String("claude", "", "Claude executable (otherwise auto-detected)")
	server := fs.String("server", "", "polign-server executable for the local backend (otherwise alongside this CLI, then PATH)")
	extractModel := fs.String("extract-model", "", "model that works out what remembered text says, as provider:model (anthropic:claude-haiku-4-5-20251001, openai:<model>, ollama:<model>); with one, Recall serves the text-only tools. none clears a saved model")
	typed := fs.Bool("typed", false, "serve the typed tools (remember with subject, predicate and value) even with a model saved; -typed=false goes back to the text-only tools")
	embedModel := fs.String("embed-model", "", "model that embeds memory so recall searches by meaning, as provider:model[@dim] (openai:text-embedding-3-small@512, ollama:nomic-embed-text, voyage:voyage-3.5), or lexical for word search. Omitted, a new setup follows -extract-model: openai and ollama embed with their own provider (Ollama's model is pulled if missing); anthropic asks at a terminal. Changing it copies the saved memory into a new collection")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if strings.TrimSpace(*dir) == "" {
		return fmt.Errorf("no home directory: pass -config-dir or set RECALL_HOME")
	}
	absolute, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	*dir = absolute
	if *localFlag {
		if sub.given("backend") && *sub.backend != local.Name {
			return fmt.Errorf("choose either -local or -backend %s", *sub.backend)
		}
		_ = fs.Set("backend", local.Name)
	}
	setupOnly := *collection != "" || *noPlugin || *claude != "" || *server != "" || *extractModel != "" || *typed || *embedModel != ""
	switch args[0] {
	case "doctor":
		if setupOnly {
			return fmt.Errorf("those options belong to recall setup")
		}
		return doctor(global, sub, *dir, out)
	case "mcp":
		if setupOnly || sub.given("backend") || sub.given("url") || sub.given("key") {
			return fmt.Errorf("connection options belong to recall setup")
		}
		return serveSetup(*dir)
	case "setup":
	default:
		return fmt.Errorf("unknown Recall command %q (want setup, doctor, mcp)", args[0])
	}
	if runtime.GOOS == "windows" {
		// The plugin launcher is a /bin/sh script. The rest of the CLI runs
		// on Windows, local database included.
		return fmt.Errorf("recall setup is not supported on Windows yet. recall mcp works there with nothing configured, keeping memory in %s, or with -backend and -url", recallHome())
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	// This is a directory; the owner needs execute permission to traverse it.
	if err := os.Chmod(*dir, 0o700); err != nil { //nolint:gosec // G302: private directory, not a regular file
		return err
	}
	// Another process opening the same directory may be mid-setup; it
	// holds the lock only until its server answers.
	lock, err := recallsetup.LockWait(filepath.Join(*dir, "setup.lock"), 2*time.Minute)
	if err != nil {
		return err
	}
	defer lock.Close()
	var cfg recallsetup.Config
	configPath := filepath.Join(*dir, "config.json")
	readErr := recallsetup.Read(configPath, &cfg)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("read existing configuration: %w", readErr)
	}
	first := readErr != nil
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Preserve the installed path (e.g. Homebrew's stable symlink), not a
	// versioned Cellar target that disappears on the next upgrade.
	if invoked, err := exec.LookPath(os.Args[0]); err == nil {
		exe, err = filepath.Abs(invoked)
		if err != nil {
			return err
		}
	}
	cfg.Executable = exe
	if first {
		cfg.Collection = setting("COLLECTION") // "" is named after the embedder below
		cfg.Predicates = setting("PREDICATES")
		cfg.Open = true
	}
	// Where memory lives. On a first setup the flags and the environment
	// decide, with the local database as the default. A rerun keeps what was
	// saved unless a database flag changes it; the environment alone does
	// not, which is what "later runs preserve the saved connection" promises.
	d, err := setupTarget(global, sub, *dir, cfg, first)
	if err != nil {
		return err
	}
	previous := cfg
	cfg.Backend = d.Backend
	switch d.Backend {
	case local.Name:
		resolved, err := local.Resolve(d.URL)
		if err != nil {
			return err
		}
		cfg.URL = ""
		if resolved != *dir {
			cfg.URL = resolved
		}
		// A saved server path has to survive a rerun. Recomputing it would
		// silently move an explicitly chosen server back to whatever sits
		// next to this CLI.
		switch {
		case *server != "":
			if cfg.Server, err = filepath.Abs(*server); err != nil {
				return err
			}
		case d.Params["server"] != "":
			cfg.Server = d.Params["server"]
		}
		cfg.Key = ""
	default:
		cfg.URL = strings.TrimRight(d.URL, "/")
		cfg.Key = d.Key
		cfg.Server = ""
		if d.Backend == "polign" {
			if err := validateRecallURL(cfg.URL); err != nil {
				return err
			}
		}
	}
	switch *extractModel {
	case "":
	case "none":
		cfg.ExtractModel = ""
	default:
		if _, err := newExtractor(*extractModel); err != nil {
			return fmt.Errorf("-extract-model: %w", err)
		}
		cfg.ExtractModel = *extractModel
	}
	typedGiven := false
	fs.Visit(func(f *flag.Flag) { typedGiven = typedGiven || f.Name == "typed" })
	if typedGiven {
		cfg.Typed = *typed
	}
	// A first setup at a terminal is asked which model, if any; a setup
	// that already has a model, or an answer on the command line, or no
	// terminal to ask at, is not.
	if *extractModel == "" && cfg.ExtractModel == "" && !cfg.Typed && isTerminal(os.Stdin) {
		choice, err := askModel(os.Stdin, out, modelChoices())
		if err != nil {
			return err
		}
		cfg.ExtractModel = choice
	}
	spec, namedEmbed, err := setupEmbedding(cfg, *embedModel, first || *extractModel != "", os.Stdin, isTerminal(os.Stdin), out)
	if err != nil {
		return err
	}
	if spec, err = prepareEmbedding(spec, namedEmbed, out); err != nil {
		return err
	}
	switching := !first && spec != cfg.EmbedModel
	cfg.EmbedModel = spec
	if cfg.Collection == "" || switching && *collection == "" {
		cfg.Collection = defaultCollection(cfg.EmbedModel)
	}
	if *collection != "" {
		cfg.Collection = *collection
	}
	if cfg.Collection == "" {
		return fmt.Errorf("collection must not be empty")
	}
	if cfg.Predicates != "" {
		cfg.Predicates, err = filepath.Abs(cfg.Predicates)
		if err != nil {
			return err
		}
	}
	var claudePath string
	if !*noPlugin {
		claudePath, err = findClaude(*claude)
		if err != nil {
			return err
		}
	}
	// Connect, which for the local backend starts the directory's server.
	c, err := setupDatabase(*dir, cfg).connect(context.Background())
	if err != nil {
		return err
	}
	if cfg.Backend == local.Name {
		// The key the directory holds, kept in the configuration as every
		// release before 0.23.0 did.
		cfg.Key = c.key
	}
	if err := checkRecall(c, cfg); err != nil {
		return err
	}
	if err := settleCollection(c, previous, &cfg, switching, namedEmbed, out); err != nil {
		return err
	}
	// Commit only a working connection. A failed reconfiguration leaves the
	// previous endpoint and launcher intact.
	//
	// The check has to run against the written files, because the command the
	// plugin runs is the launcher, not "recall mcp" alone: it reads this
	// configuration and may start the server. Verifying the other command would
	// leave the one Claude actually uses untested, so write first and undo on
	// failure rather than check something easier.
	launchPath := filepath.Join(*dir, "launch")
	previousConfig, configExisted := os.ReadFile(configPath)
	previousLaunch, launchExisted := os.ReadFile(launchPath)
	if err := recallsetup.Write(configPath, cfg); err != nil {
		return err
	}
	launcher := "#!/bin/sh\nexec " + shellQuote(exe) + " mcp -config-dir " + shellQuote(*dir) + "\n"
	if err := recallsetup.WriteFile(launchPath, []byte(launcher), 0o700); err != nil {
		return err
	}
	if err := checkRecallLauncher(launchPath, cfg); err != nil {
		restoreRecallFile(configPath, previousConfig, configExisted, 0o600)
		restoreRecallFile(launchPath, previousLaunch, launchExisted, 0o700)
		return err
	}
	fmt.Fprintf(out, "Recall connected: %s\nCollection: %s\nConfiguration: %s\n", setupDatabase(*dir, cfg), cfg.Collection, *dir)
	fmt.Fprintln(out, describeSurface(cfg))
	fmt.Fprintln(out, describeSearch(cfg))
	if cfg.ExtractModel == "" {
		fmt.Fprintln(out, modelHint())
	}
	if cfg.Backend == local.Name {
		fmt.Fprintf(out, "Local data: %s\nThe local server starts automatically when Recall connects.\n", filepath.Join(setupDatabase(*dir, cfg).displayURL(), "data"))
	}
	if !*noPlugin {
		if err := installRecallPlugin(claudePath, out); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "Ready. Restart Claude Code, then ask:\n/recall:recall-memory Remember that I prefer concise answers.")
	return nil
}

// setupTarget is the database a setup saves. A first setup resolves it from
// the flags, the environment and the defaults, never from a database that
// happens to answer at the old address. A rerun keeps the saved database
// unless a database flag is given; then the flags decide and the environment
// fills what they leave out. -url alone keeps the saved backend when the URL
// fits it, else means polign, which is what -url meant before 0.23.0.
func setupTarget(global, sub *dbFlags, dir string, cfg recallsetup.Config, first bool) (database, error) {
	given := func(name string) bool { return sub.given(name) || global.given(name) }
	if !first && !given("backend") && !given("url") && !given("key") {
		return setupDatabase(dir, cfg), nil
	}
	if given("url") && !given("backend") {
		rawURL := *sub.url
		if !sub.given("url") {
			rawURL = *global.url
		}
		candidates := []string{"polign"}
		if !first {
			candidates = append([]string{cfg.BackendName()}, candidates...)
		}
		for _, name := range candidates {
			if fits(name, rawURL, "-url") == nil {
				_ = sub.fs.Set("backend", name)
				break
			}
		}
	}
	r := databases
	r.probe = func() bool { return false }
	return r.resolve(global, sub, dir, false)
}

// setupDatabase is the database a saved setup names.
func setupDatabase(dir string, cfg recallsetup.Config) database {
	from := "the setup saved in " + dir
	d := database{Backend: cfg.BackendName(), URL: cfg.URL, Key: cfg.Key, Params: map[string]string{}, BackendFrom: from, URLFrom: from, KeyFrom: from, Dir: dir, Setup: &cfg}
	if d.Backend == local.Name {
		if d.URL == "" {
			d.URL = dir
		}
		if cfg.Server != "" {
			d.Params["server"] = cfg.Server
		}
	}
	return d
}

// doctor says where memory lives, where that came from, and whether it
// answers. With a saved setup in play it also checks the plugin launcher.
func doctor(global, sub *dbFlags, dir string, out io.Writer) error {
	d, err := resolveDatabase(global, sub, dir, false)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Doctor version: %s\nDatabase: %s\n  %s\n", version, d, d.sources())
	check := recallsetup.Config{Collection: settingOr("COLLECTION", defaultLexicalCollection)}
	switch {
	case d.fromSetup():
		check = *d.Setup
		fmt.Fprintf(out, "Configured CLI: %s\nConfiguration: %s\nCollection: %s\n", check.Executable, dir, check.Collection)
		fmt.Fprintln(out, describeSurface(check))
		fmt.Fprintln(out, describeSearch(check))
		if path, err := exec.LookPath("recall"); err == nil && path != check.Executable {
			fmt.Fprintf(out, "PATH selects %s; Recall uses the saved executable above.\n", path)
		}
	default:
		if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
			fmt.Fprintf(out, "The setup saved in %s is not in use, because the %s names a database.\n", dir, d.BackendFrom)
		} else {
			fmt.Fprintf(out, "No setup is saved in %s. recall mcp serves this database with its defaults (collection %s); run recall setup to save a configuration and install the Claude plugin.\n", dir, check.Collection)
		}
	}
	c, err := d.connect(context.Background())
	if err != nil {
		return err
	}
	if err := checkRecall(c, check); err != nil {
		return err
	}
	fmt.Fprintln(out, "Connection and memory read: OK")
	if !d.fromSetup() {
		return nil
	}
	if err := checkRecallLauncher(filepath.Join(dir, "launch"), check); err != nil {
		return err
	}
	fmt.Fprintln(out, "MCP handshake and memory tools: OK\nIf Claude still shows a cached failure, restart Claude and check /mcp.")
	return nil
}

// serveSetup serves memory over MCP from what recall setup saved in dir,
// with its collection, model and embedder: the command the plugin launcher
// runs.
func serveSetup(dir string) error {
	var cfg recallsetup.Config
	if err := recallsetup.Read(filepath.Join(dir, "config.json"), &cfg); err != nil {
		return fmt.Errorf("recall is not configured: run recall setup (%w)", err)
	}
	c, err := setupDatabase(dir, cfg).connect(context.Background())
	if err != nil {
		return err
	}
	spec := settingOr("EXTRACT_MODEL", cfg.ExtractModel)
	text := spec != "" && !cfg.Typed
	// A saved setup embeds with what setup saved, never with what the
	// extraction model would pick: the collection was written with that
	// embedder.
	emb, err := openEmbedding(settingOr("EMBED_MODEL", cfg.EmbedModel), false)
	if err != nil {
		return fmt.Errorf("embedding model %q: %w", cfg.EmbedModel, err)
	}
	backend, err := openMemoryBackend(c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	emb, _, err = fitEmbedding(ctx, backend, cfg.Collection, emb)
	cancel()
	if err != nil {
		return fmt.Errorf("%w; run recall setup again", err)
	}
	mem, err := newMemoryRuntime(c, cfg.Collection, cfg.Predicates, emb.Embedder, cfg.Predicates == "" && (cfg.Open || text))
	if err != nil {
		return err
	}
	if mem.extractor, err = newExtractor(spec); err != nil {
		return fmt.Errorf("extraction model %q: %w", spec, err)
	}
	mem.text = text
	s := &mcpServer{api: c, collection: cfg.Collection, search: emb.spec, memory: mem, write: true, enc: json.NewEncoder(os.Stdout), database: setupDatabase(dir, cfg).report(c)}
	return s.serve(context.Background(), os.Stdin)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// validateRecallURL checks a polign_db address: an http(s) endpoint without
// credentials, query, or fragment, since the key travels in a header.
func validateRecallURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("recall URL must be an http(s) endpoint without credentials, query, or fragment; use -key or RECALL_BACKEND_KEY for authentication")
	}
	return nil
}

// checkRecall checks a connection without writing: that a polign_db is
// ready, and that a memory read with the configured collection and
// credential succeeds on any backend.
func checkRecall(c *api, cfg recallsetup.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if c.backend == "" || c.backend == setupBackend {
		if err := c.doCtx(ctx, http.MethodGet, "/readyz", nil, nil); err != nil {
			// Recall-capable 0.6.4 servers predate the readiness route.
			var status *apiError
			if errors.As(err, &status) && status.Code == http.StatusNotFound {
				err = c.doCtx(ctx, http.MethodGet, "/healthz", nil, nil)
			}
			if err != nil {
				return fmt.Errorf("database is not ready at %s; check the URL and server: %w", c.base, err)
			}
		}
	}
	mem, err := newMemoryRuntime(c, cfg.Collection, cfg.Predicates, nil, false)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 5; attempt++ {
		_, err = mem.forRequest(ctx).toolRecall(json.RawMessage(`{"subject":"__recall_connection_check__"}`))
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "log changed while materializing; retry") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if c.key != "" {
		err = errors.New(strings.ReplaceAll(err.Error(), c.key, "[redacted]"))
	}
	return fmt.Errorf("memory read failed (check the key and the collection, then rerun setup with the same database): %w", err)
}

func findClaude(explicit string) (string, error) {
	paths := []string{explicit}
	if explicit == "" {
		if path, err := exec.LookPath("claude"); err == nil {
			paths = append(paths, path)
		}
		home, _ := os.UserHomeDir()
		paths = append(paths, filepath.Join(home, ".local", "bin", "claude"), "/opt/homebrew/bin/claude", "/usr/local/bin/claude")
	}
	// An installation that answers but is too old must not end the search: an
	// obsolete claude earlier on PATH is exactly the shadowing the plugin
	// launcher goes out of its way to survive. Keep looking, and report the
	// newest one actually seen only when nothing works.
	var tooOld, tooOldVersion string
	for _, path := range paths {
		if path == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		b, err := exec.CommandContext(ctx, path, "--version").Output()
		cancel()
		if err != nil {
			continue
		}
		v := strings.Fields(string(b))
		if len(v) == 0 || !versionAtLeast(v[0], 2, 1, 251) {
			if tooOld == "" {
				tooOld, tooOldVersion = path, strings.TrimSpace(string(b))
			}
			continue
		}
		return path, nil
	}
	if tooOld != "" {
		return "", fmt.Errorf("the Claude Code installation at %s reports %q, which is older than the required 2.1.251; run %s update, then rerun setup", tooOld, tooOldVersion, shellQuote(tooOld))
	}
	return "", fmt.Errorf("could not find Claude Code; install it from https://code.claude.com/docs/en/quickstart, then rerun setup; use -no-plugin for another MCP host")
}

// versionAtLeast compares a dotted version against a minimum.
//
// It tolerates more components than it is given and a pre-release suffix on the
// last one, because reporting "too old" for a version string this code has
// simply never seen sends the user to run an update that cannot change the
// answer. Only a version that is genuinely lower, or carries no leading number
// at all, is rejected.
func versionAtLeast(raw string, want ...int) bool {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(raw), "v"), ".")
	if len(parts) < len(want) {
		return false
	}
	for i, target := range want {
		digits := parts[i]
		if cut := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); cut >= 0 {
			digits = digits[:cut]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return false
		}
		if n != target {
			return n > target
		}
	}
	return true
}

func installRecallPlugin(claude string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := exec.CommandContext(ctx, claude, "plugin", "marketplace", "list", "--json").Output()
	if err != nil {
		return fmt.Errorf("list Claude marketplaces: %w", err)
	}
	var markets []struct {
		Name string `json:"name"`
		Repo string `json:"repo"`
	}
	if err := json.Unmarshal(b, &markets); err != nil {
		return err
	}
	marketArgs := []string{"plugin", "marketplace", "add", "https://github.com/Polign/polign.git"}
	for _, market := range markets {
		if market.Name == "polign" {
			if !strings.EqualFold(market.Repo, "Polign/polign") {
				return fmt.Errorf("a different marketplace already uses the name polign; resolve it in Claude before installing Recall")
			}
			marketArgs = []string{"plugin", "marketplace", "update", "polign"}
		}
	}
	// -y accepts the marketplace-declared command without a prompt. Setup runs
	// with no stdin attached, so without it an install that asks for
	// confirmation blocks until this context expires.
	for _, args := range [][]string{marketArgs, {"plugin", "install", "-y", "recall@polign"}, {"plugin", "update", "recall@polign"}} {
		cmd := exec.CommandContext(ctx, claude, args...)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("claude %s failed; the verified Recall configuration is saved, so rerun setup to retry: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}

// restoreRecallFile puts back what a failed reconfiguration replaced, and
// removes the file when there was nothing there before.
func restoreRecallFile(path string, previous []byte, readErr error, mode os.FileMode) {
	if readErr != nil {
		_ = os.Remove(path)
		return
	}
	_ = recallsetup.WriteFile(path, previous, mode)
}

// checkRecallLauncher runs the exact command the plugin runs, over stdio,
// without a model call or a memory write, and checks it lists the tools the
// setup serves: remember, recall and forget, plus memory_history and
// list_predicates for the typed tools. Database authentication is checked
// separately above.
func checkRecallLauncher(launchPath string, cfg recallsetup.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", launchPath)
	cmd.Stdin = strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-06-18\"}}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		// Without this the failure stringifies to "exit status 1", which is
		// the least useful thing doctor could possibly report.
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = "no output on stderr"
		}
		return fmt.Errorf("MCP check failed running %s: %w: %s", launchPath, err, detail)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	initialized, listed := false, false
	for {
		var reply struct {
			ID     int `json:"id"`
			Result struct {
				ProtocolVersion string    `json:"protocolVersion"`
				Tools           []mcpTool `json:"tools"`
			} `json:"result"`
		}
		if err := dec.Decode(&reply); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		if reply.ID == 1 {
			initialized = reply.Result.ProtocolVersion != ""
		}
		if reply.ID == 2 {
			names := map[string]bool{}
			for _, tool := range reply.Result.Tools {
				names[tool.Name] = true
			}
			listed = names["remember"] && names["recall"] && names["forget"]
			if typedSetup(cfg) {
				listed = listed && names["memory_history"] && names["list_predicates"]
			}
		}
	}
	if !initialized || !listed {
		want := "remember, recall and forget"
		if typedSetup(cfg) {
			want = "remember, recall, forget, memory_history and list_predicates"
		}
		return fmt.Errorf("the configured launcher does not expose the Recall MCP protocol and the memory tools %s", want)
	}
	return nil
}

// typedSetup reports whether a saved setup serves the typed tools: it has
// no extraction model, or keeps the typed tools with -typed. Others serve
// the text-only tools.
func typedSetup(cfg recallsetup.Config) bool {
	return settingOr("EXTRACT_MODEL", cfg.ExtractModel) == "" || cfg.Typed
}

// serveConfigured serves memory over MCP from what recall setup saved in dir.
func serveConfigured(dir string) error { return serveSetup(dir) }

// describeSurface says which tools a saved setup serves, and why.
func describeSurface(cfg recallsetup.Config) string {
	switch {
	case cfg.ExtractModel != "" && cfg.Typed:
		return "Extraction model: " + cfg.ExtractModel + ", unused (typed tools, -typed; setup again with -typed=false for the text-only tools)"
	case cfg.ExtractModel != "":
		return "Extraction model: " + cfg.ExtractModel + " (text-only tools: remember, recall, forget)"
	case cfg.Predicates != "":
		return "Extraction model: none (typed tools, predicates from " + cfg.Predicates + ")"
	case cfg.Open:
		return "Extraction model: none (typed tools; the agent names predicates and new ones are defined on first use)"
	}
	return "Extraction model: none (typed tools with the starter predicates)"
}

// isTerminal reports whether f is a terminal, where a question can be asked.
// A character device is not enough: /dev/null is one too.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd())) // #nosec G115 -- a file descriptor fits an int.
}

// modelChoice is one answer to the setup question: a model spec, or "" for
// none, and what choosing it means.
type modelChoice struct {
	spec, note string
}

// modelChoices lists what setup can offer at this machine: a hosted model
// for each provider key in the environment and every local Ollama model that
// generates text, after none. Setup never picks one itself: a hosted model
// receives every remembered passage and bills each one, and a local one can
// add seconds to every write, so the choice is the user's.
func modelChoices() []modelChoice {
	choices := []modelChoice{{"", "typed tools; the agent names the facts itself"}}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		choices = append(choices, modelChoice{"anthropic:claude-haiku-4-5-20251001", "ANTHROPIC_API_KEY is set; every remembered passage is sent to Anthropic and billed"})
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		choices = append(choices, modelChoice{"openai:gpt-5-mini", "OPENAI_API_KEY is set; every remembered passage is sent to OpenAI and billed"})
	}
	for _, name := range ollamaModels() {
		choices = append(choices, modelChoice{"ollama:" + name, "local; can add seconds to every write"})
	}
	return choices
}

// askModel puts the setup question to the user and returns the model spec
// chosen: a number from the list, a provider:model typed in, or "" for none,
// which an empty answer also means. A spec that does not parse is asked
// again; end of input means none.
func askModel(in io.Reader, out io.Writer, choices []modelChoice) (string, error) {
	fmt.Fprintln(out, "\nExtraction model? With one, Recall works out the facts in remembered text and serves the text-only tools. Without one, text is kept as written and the agent names the facts itself.")
	for i, c := range choices {
		name := c.spec
		if name == "" {
			name = "none"
		}
		fmt.Fprintf(out, "  %d. %s (%s)\n", i+1, name, c.note)
	}
	reader := bufio.NewReader(in)
	for {
		fmt.Fprint(out, "Choose a number, or type provider:model [1]: ")
		line, err := reader.ReadString('\n')
		answer := strings.TrimSpace(line)
		if answer == "" {
			if err != nil {
				fmt.Fprintln(out)
			}
			return "", nil
		}
		if n, convErr := strconv.Atoi(answer); convErr == nil {
			if n >= 1 && n <= len(choices) {
				return choices[n-1].spec, nil
			}
			fmt.Fprintf(out, "There is no choice %d.\n", n)
		} else if _, specErr := newExtractor(answer); specErr != nil {
			fmt.Fprintf(out, "%v\n", specErr)
		} else {
			return answer, nil
		}
		if err != nil {
			return "", nil
		}
	}
}

// modelHint tells a setup without a model what one would add, naming the
// local Ollama models it can see. Setup never picks one itself: a hosted
// model receives every remembered passage and bills each one, and a local
// one can add seconds to every write, so the choice is the user's.
func modelHint() string {
	hint := "To let Recall work out facts from text itself, and serve the text-only tools with no predicates in them, run setup again with -extract-model, for example -extract-model anthropic:claude-haiku-4-5-20251001 (reads ANTHROPIC_API_KEY where Claude runs)."
	if models := ollamaModels(); len(models) > 0 {
		hint += " Local Ollama models found: " + strings.Join(models, ", ") + " (use -extract-model ollama:<name>)."
	}
	return hint
}

// ollamaModels lists the local Ollama models that generate text, if Ollama
// answers within a moment.
func ollamaModels() []string {
	models, _ := ollamaTags()
	var out []string
	for _, m := range models {
		if slices.Contains(m.Capabilities, "completion") {
			out = append(out, m.Name)
		}
	}
	return out
}
