package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"strconv"
	"strings"
	"time"

	"github.com/Polign/recall/internal/recallsetup"
)

func recallHome() string {
	if dir := os.Getenv("POLIGN_RECALL_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Returning "" here would become a relative path under the current
		// directory once filepath.Abs runs, quietly scattering configuration
		// and a private key wherever setup happened to be invoked.
		return ""
	}
	return filepath.Join(home, ".config", "polign", "recall")
}

func cmdRecall(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: recall <setup|doctor|mcp> [options]")
	}
	if runtime.GOOS == "windows" {
		// The plugin launcher is a /bin/sh script and the local database is
		// managed through Unix conventions. The server and the rest of the
		// CLI run on Windows; Recall setup does not yet.
		return fmt.Errorf("recall %s is not supported on Windows yet; run polign-server directly and point the Recall plugin at it with POLIGN_URL", args[0])
	}
	fs := flag.NewFlagSet("recall "+args[0], flag.ContinueOnError)
	dir := fs.String("config-dir", recallHome(), "persistent Recall configuration and local data directory (default $POLIGN_RECALL_HOME, then ~/.config/polign/recall)")
	endpoint := fs.String("url", "", "connect to an existing server; API key comes from POLIGN_API_KEY")
	local := fs.Bool("local", false, "use a managed local database, ignoring POLIGN_URL")
	collection := fs.String("collection", "", "memory collection (default $POLIGN_COLLECTION, then recall_lexical_v1)")
	noPlugin := fs.Bool("no-plugin", false, "configure and check Recall without installing the Claude plugin")
	claude := fs.String("claude", "", "Claude executable (otherwise auto-detected)")
	server := fs.String("server", "", "Polign server executable (otherwise alongside this CLI)")
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
		return fmt.Errorf("no home directory: pass -config-dir or set POLIGN_RECALL_HOME")
	}
	absolute, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	*dir = absolute
	if args[0] != "setup" {
		if *endpoint != "" || *local || *collection != "" || *noPlugin || *claude != "" || *server != "" {
			return fmt.Errorf("connection options belong to recall setup")
		}
		var cfg recallsetup.Config
		if err := recallsetup.Read(filepath.Join(*dir, "config.json"), &cfg); err != nil {
			return fmt.Errorf("recall is not configured: run recall setup (%w)", err)
		}
		switch args[0] {
		case "doctor":
			fmt.Fprintf(out, "Configured CLI: %s\nDoctor version: %s\nConfiguration: %s\nCollection: %s\n", cfg.Executable, version, *dir, cfg.Collection)
			if path, err := exec.LookPath("recall"); err == nil && path != cfg.Executable {
				fmt.Fprintf(out, "PATH selects %s; Recall uses the saved executable above.\n", path)
			}
			c, err := recallAPI(*dir, cfg)
			if err != nil {
				return err
			}
			if err := checkRecall(c, cfg); err != nil {
				return err
			}
			fmt.Fprintf(out, "Database: %s\nAuthentication and memory read: OK\n", c.base)
			if err := checkRecallLauncher(filepath.Join(*dir, "launch")); err != nil {
				return err
			}
			fmt.Fprintln(out, "MCP handshake and memory tools: OK\nIf Claude still shows a cached failure, restart Claude and check /mcp.")
			return nil
		case "mcp":
			if err := ensureRecallServer(*dir, cfg); err != nil {
				return err
			}
			c, err := recallAPI(*dir, cfg)
			if err != nil {
				return err
			}
			mem, err := newMemoryRuntime(c, cfg.Collection, cfg.Predicates, "", false)
			if err != nil {
				return err
			}
			if mem.extractor, err = newExtractor(os.Getenv("POLIGN_EXTRACT_MODEL")); err != nil {
				return fmt.Errorf("POLIGN_EXTRACT_MODEL: %w", err)
			}
			s := &mcpServer{api: c, collection: cfg.Collection, memory: mem, write: true, enc: json.NewEncoder(os.Stdout)}
			return s.serve(context.Background(), os.Stdin)
		default:
			return fmt.Errorf("unknown Recall command %q (want setup, doctor, mcp)", args[0])
		}
	}
	if *local && *endpoint != "" {
		return fmt.Errorf("choose either -local or -url")
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
	if readErr != nil {
		cfg.URL = os.Getenv("POLIGN_URL")
		cfg.Key = os.Getenv("POLIGN_API_KEY")
		cfg.Collection = envOr("POLIGN_COLLECTION", "recall_lexical_v1")
		cfg.Predicates = os.Getenv("POLIGN_PREDICATES")
	}
	if *endpoint != "" {
		cfg.URL = *endpoint
		cfg.Key = os.Getenv("POLIGN_API_KEY")
		cfg.Server = ""
	}
	if *local {
		cfg.URL = ""
		cfg.Server = ""
		cfg.Key = ""
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
	if cfg.URL != "" {
		if err := validateRecallURL(cfg.URL); err != nil {
			return err
		}
		cfg.URL = strings.TrimRight(cfg.URL, "/")
	} else {
		// A saved server path has to survive a rerun, which is what "subsequent
		// runs preserve the saved connection" promises. Recomputing it
		// unconditionally would silently move an explicitly chosen server back
		// to whatever sits next to this CLI.
		if *server != "" {
			if cfg.Server, err = filepath.Abs(*server); err != nil {
				return err
			}
		} else if cfg.Server == "" {
			cfg.Server = localServerPath(exe)
		}
		if err := checkRecallBinary(cfg.Server, "-runtime-file"); err != nil {
			return err
		}
		// Reuse the private local key even when switching back from a remote
		// server. The server registers this key itself at every start (see
		// ensureRecallServer), so setup only has to make sure a well-formed key
		// exists; it never touches the data directory.
		key, err := readLocalKey(*dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !validLocalKey(key) {
			if key, err = generateLocalKey(); err != nil {
				return err
			}
			if err := writeLocalKey(*dir, key); err != nil {
				return err
			}
		}
		cfg.Key = key
	}
	var claudePath string
	if !*noPlugin {
		claudePath, err = findClaude(*claude)
		if err != nil {
			return err
		}
	}
	if err := ensureRecallServer(*dir, cfg); err != nil {
		return err
	}
	c, err := recallAPI(*dir, cfg)
	if err != nil {
		return err
	}
	if err := checkRecall(c, cfg); err != nil {
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
	if err := checkRecallLauncher(launchPath); err != nil {
		restoreRecallFile(configPath, previousConfig, configExisted, 0o600)
		restoreRecallFile(launchPath, previousLaunch, launchExisted, 0o700)
		return err
	}
	fmt.Fprintf(out, "Recall connected: %s\nCollection: %s\nConfiguration: %s\n", c.base, cfg.Collection, *dir)
	if cfg.URL == "" {
		fmt.Fprintf(out, "Local data: %s\nThe local server starts automatically when Recall connects.\n", filepath.Join(*dir, "data"))
	}
	if !*noPlugin {
		if err := installRecallPlugin(claudePath, out); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "Ready. Restart Claude Code, then ask:\n/recall:recall-memory Remember that I prefer concise answers.")
	return nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func validateRecallURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("recall URL must be an http(s) endpoint without credentials, query, or fragment; use POLIGN_API_KEY for authentication")
	}
	return nil
}

func recallAPI(dir string, cfg recallsetup.Config) (*api, error) {
	endpoint := cfg.URL
	if endpoint == "" {
		var state recallsetup.Runtime
		if err := recallsetup.Read(filepath.Join(dir, "runtime.json"), &state); err != nil {
			return nil, fmt.Errorf("local database is stopped; run recall setup to start it (%w)", err)
		}
		endpoint = state.URL
	}
	if err := validateRecallURL(endpoint); err != nil {
		return nil, err
	}
	return &api{base: endpoint, key: cfg.Key}, nil
}

func checkRecall(c *api, cfg recallsetup.Config) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
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
	mem, err := newMemoryRuntime(c, cfg.Collection, cfg.Predicates, "", false)
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
	return fmt.Errorf("memory read failed (check POLIGN_API_KEY and the collection, then rerun setup -url with the same endpoint): %w", err)
}

func checkRecallBinary(path, capability string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, _ := exec.CommandContext(ctx, path, "-help").CombinedOutput() //nolint:gosec // G702: executable explicitly selected by the local user; no shell
	if !strings.Contains(string(b), capability) {
		return fmt.Errorf("%s does not support Recall setup; upgrade Polign and run the CLI from the same installation", path)
	}
	return nil
}

func ensureRecallServer(dir string, cfg recallsetup.Config) error {
	if cfg.URL != "" {
		return nil
	}
	if c, err := recallAPI(dir, cfg); err == nil && checkRecall(c, cfg) == nil {
		return nil
	}
	if cfg.Server == "" {
		return fmt.Errorf("local server is not configured; run recall setup")
	}
	if err := checkRecallBinary(cfg.Server, "-runtime-file"); err != nil {
		return err
	}
	logPath := filepath.Join(dir, "server.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	// The server registers the local key from this file at every start, so a
	// data directory that was deleted or restored from a backup older than the
	// key gets its credential record back on the next launch instead of
	// answering 401 for ever. Recreating the file from the saved configuration
	// also carries over a setup written by an earlier release, which kept the
	// key in a different file.
	if cfg.Key == "" {
		return fmt.Errorf("local API key is not configured; run recall setup")
	}
	keyPath := filepath.Join(dir, localKeyName)
	if key, err := readLocalKey(dir); err != nil || key != cfg.Key {
		if err := writeLocalKey(dir, cfg.Key); err != nil {
			return err
		}
	}
	cmd := exec.Command(cfg.Server, "-store", "fs:"+filepath.Join(dir, "data"), "-http", "127.0.0.1:0", "-grpc", "127.0.0.1:0", "-require-data-key", "-bootstrap-key-file", keyPath, "-bootstrap-key-namespace", "recall", "-telemetry=false", "-runtime-file", filepath.Join(dir, "runtime.json")) //nolint:gosec,noctx // G204: saved local executable; the detached server must outlive this command
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the child while the CLI lives; detachment lets the server outlive
	// this invocation. The server's kernel lock excludes concurrent starters.
	go func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := recallAPI(dir, cfg); err == nil && checkRecall(c, cfg) == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("local Recall database did not become ready; inspect %s, then rerun recall setup", logPath)
}

// localKeyName holds the key the local server accepts, as the single
// "plgn_<id>_<secret>" line that polign-server -bootstrap-key-file reads. The
// shape mirrors the server's key format; the server stays the authority and
// refuses to start on a key it cannot parse.
const localKeyName = "local-key"

func generateLocalKey() (string, error) {
	var id [8]byte
	var secret [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	if _, err := rand.Read(secret[:]); err != nil {
		return "", err
	}
	return "plgn_" + hex.EncodeToString(id[:]) + "_" + hex.EncodeToString(secret[:]), nil
}

func validLocalKey(key string) bool {
	rest, ok := strings.CutPrefix(key, "plgn_")
	if !ok {
		return false
	}
	id, secret, ok := strings.Cut(rest, "_")
	if !ok || len(id) != 16 || len(secret) != 64 {
		return false
	}
	for _, c := range id + secret {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func readLocalKey(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, localKeyName))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func writeLocalKey(dir, key string) error {
	return recallsetup.WriteFile(filepath.Join(dir, localKeyName), []byte(key+"\n"), 0o600)
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
// without a model call or a memory write. Database authentication is checked
// separately above.
func checkRecallLauncher(launchPath string) error {
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
			listed = names["remember"] && names["recall"] && names["forget"] && names["memory_history"] && names["list_predicates"]
		}
	}
	if !initialized || !listed {
		return fmt.Errorf("the configured launcher does not expose the Recall MCP protocol and five memory tools")
	}
	return nil
}

// serveConfigured serves memory over MCP from what recall setup saved in dir.
func serveConfigured(dir string) error {
	return cmdRecall([]string{"mcp", "-config-dir", dir}, os.Stdout)
}

// localServerPath is the polign-server the managed local database runs: the
// one installed beside this binary (a pip environment, Homebrew, a release
// archive), else the first on PATH. polign_db ships it; Recall does not.
func localServerPath(exe string) string {
	beside := filepath.Join(filepath.Dir(exe), "polign-server")
	if runtime.GOOS == "windows" {
		beside += ".exe"
	}
	if _, err := os.Stat(beside); err == nil {
		return beside
	}
	if found, err := exec.LookPath("polign-server"); err == nil {
		if abs, err := filepath.Abs(found); err == nil {
			return abs
		}
	}
	return beside // checkRecallBinary reports it missing, naming where it looked
}
