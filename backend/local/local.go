// Package local is the backend Recall uses when nothing names a database: a
// polign_db that Recall runs itself, kept in a directory on this machine.
//
// The first process to open a directory starts a polign-server for it on a
// free loopback port, with a generated key only this directory knows, and
// leaves it running; later processes find it through the runtime file the
// server writes. Nothing listens beyond this host. The server needs
// polign-server beside the recall binary or on PATH, which pip and Homebrew
// install with recall.
//
// Importing this package registers it as the "local" backend:
//
//	import _ "github.com/Polign/recall/backend/local"
//
//	b, err := recall.OpenBackend("local", recall.BackendOptions{URL: "~/agent-memory"})
//
// An empty URL means Home(), where every agent on the machine shares one
// memory. The one option is ?server=/path/to/polign-server.
package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Polign/recall"
	"github.com/Polign/recall/internal/recallsetup"
	"github.com/Polign/recall/polign"
)

// Name is the registered backend name.
const Name = "local"

// Files the directory holds, shared with recall setup and with clients that
// predate this package (polign-recall 0.22 reads runtime.json and local-key).
const (
	runtimeFile = "runtime.json"
	keyFile     = "local-key"
	lockFile    = "local.lock" // not setup.lock, which recall setup holds while it calls Ensure
	logFile     = "server.log"
	dataDir     = "data"
)

func init() {
	recall.RegisterBackend(Name, recall.BackendDriver{
		Open: func(o recall.BackendOptions) (recall.Backend, error) {
			return Open(Options{Dir: o.URL, Server: o.Params["server"]})
		},
		Params: []string{"server"},
	})
}

// Options say which directory holds the database and which polign-server
// runs it.
type Options struct {
	// Dir is the directory. Empty means Home(). A leading ~ is the user's
	// home directory.
	Dir string
	// Server is the polign-server executable. Empty means the one beside
	// the running binary, then the first on PATH.
	Server string
}

// Connection is where the running server listens and the key it accepts.
type Connection struct {
	// URL is the server's HTTP address on the loopback interface.
	URL string
	// Key is the API key the server was started with.
	Key string
	// Dir is the directory, made absolute.
	Dir string
}

// Open makes sure the directory's server is running and returns a backend
// connected to it. Unlike the other backends' constructors it may start a
// process and wait for it, which takes up to a few seconds the first time.
func Open(o Options) (*polign.Backend, error) {
	conn, err := Ensure(context.Background(), o)
	if err != nil {
		return nil, err
	}
	return polign.New(polign.Config{BaseURL: conn.URL, APIKey: conn.Key})
}

// Ensure starts the directory's server if it is not running and returns how
// to reach it. It is safe to call from several processes at once: they take
// turns on a lock file, and the second finds the first one's server.
func Ensure(ctx context.Context, o Options) (Connection, error) {
	dir, err := Resolve(o.Dir)
	if err != nil {
		return Connection{}, err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		// A directory this package creates is private: it holds a key. One
		// the user already had keeps its permissions; the key file itself is
		// always 0600.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Connection{}, fmt.Errorf("local: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: private directory, not a regular file
			return Connection{}, fmt.Errorf("local: %w", err)
		}
	} else if err != nil {
		return Connection{}, fmt.Errorf("local: %w", err)
	}
	// Another process may be mid-start; it holds the lock only until its
	// server answers.
	lock, err := recallsetup.LockWait(filepath.Join(dir, lockFile), 2*time.Minute)
	if err != nil {
		return Connection{}, fmt.Errorf("local: %w", err)
	}
	defer lock.Close()
	key, err := ensureKey(dir)
	if err != nil {
		return Connection{}, err
	}
	conn, ok, err := running(ctx, dir, key)
	if err != nil {
		return Connection{}, err
	}
	if ok {
		return conn, nil
	}
	server, err := ServerPath(o.Server)
	if err != nil {
		return Connection{}, err
	}
	if err := start(dir, server, key); err != nil {
		return Connection{}, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, ok, err := running(ctx, dir, key)
		if err != nil {
			return Connection{}, err
		}
		if ok {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return Connection{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return Connection{}, fmt.Errorf("local: the database in %s did not become ready; inspect %s", dir, filepath.Join(dir, logFile))
}

// Resolve makes dir absolute, expanding a leading ~; empty means Home().
func Resolve(dir string) (string, error) {
	if dir == "" {
		dir = Home()
		if dir == "" {
			return "", errors.New("local: no home directory; set RECALL_HOME")
		}
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("local: %w", err)
		}
		dir = filepath.Join(home, dir[1:])
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("local: %w", err)
	}
	return abs, nil
}

// running reports the directory's server when one answers and accepts this
// key. A server that answers but refuses the key is an error, not "not
// running": starting a second server on the same store would not help, and
// the one way out is to stop it or restore the key it was started with.
func running(ctx context.Context, dir, key string) (Connection, bool, error) {
	var state recallsetup.Runtime
	if err := recallsetup.Read(filepath.Join(dir, runtimeFile), &state); err != nil || state.URL == "" {
		return Connection{}, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	get := func(path string) (int, bool) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, state.URL+path, nil)
		if err != nil {
			return 0, false
		}
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, false
		}
		resp.Body.Close()
		return resp.StatusCode, true
	}
	ready := false
	for _, path := range []string{"/readyz", "/healthz"} {
		code, answered := get(path)
		if !answered {
			return Connection{}, false, nil
		}
		if code == http.StatusOK {
			ready = true
			break
		}
		if code != http.StatusNotFound { // a server older than /readyz answers 404 there
			return Connection{}, false, nil
		}
	}
	if !ready {
		return Connection{}, false, nil
	}
	// The health routes take no key. A data route does, and answers 404 for
	// a record that does not exist once the key is accepted.
	code, answered := get("/v1/collections/recall_probe/vectors/recall-probe")
	if !answered {
		return Connection{}, false, nil
	}
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return Connection{}, false, fmt.Errorf("local: the database server for %s (pid %d, %s) does not accept the key in %s; stop that server, or restore the key it was started with, then connect again",
			dir, state.PID, state.URL, filepath.Join(dir, keyFile))
	}
	return Connection{URL: state.URL, Key: key, Dir: dir}, true, nil
}

// start runs polign-server detached, so it outlives this process, on the
// directory's data with the directory's key.
func start(dir, server, key string) error {
	if err := checkServer(server); err != nil {
		return err
	}
	logPath := filepath.Join(dir, logFile)
	out, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("local: %w", err)
	}
	defer out.Close()
	cmd := exec.Command(server, //nolint:gosec,noctx // G204: the executable beside this binary or chosen by the user; the detached server must outlive this call
		"-store", "fs:"+filepath.Join(dir, dataDir),
		"-http", "127.0.0.1:0", "-grpc", "127.0.0.1:0",
		"-require-data-key", "-bootstrap-key-file", filepath.Join(dir, keyFile), "-bootstrap-key-namespace", "recall",
		"-telemetry=false",
		"-runtime-file", filepath.Join(dir, runtimeFile))
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("local: start %s: %w", server, err)
	}
	// Reap the child while this process lives; detachment lets the server
	// outlive it. The server's own lock on the runtime file excludes a
	// second starter.
	go func() { _ = cmd.Wait() }()
	return nil
}

// ServerPath finds the polign-server to run: explicit when given, else the
// one beside the running binary (a pip environment, Homebrew, a release
// archive), else the first on PATH. polign_db ships it; Recall does not.
func ServerPath(explicit string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("local: %w", err)
		}
		return abs, nil
	}
	name := "polign-server"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(beside); err == nil {
			return beside, nil
		}
	}
	if found, err := exec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(found); err == nil {
			return abs, nil
		}
	}
	return "", errors.New("local: polign-server is not installed, so Recall cannot run a local database. Install it with `pip install polign-db` or `brew install polign/tap/polign`, or name a database with -backend and -url")
}

// checkServer makes sure the executable is a polign-server new enough to
// write the runtime file this package reads.
func checkServer(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, _ := exec.CommandContext(ctx, path, "-help").CombinedOutput() //nolint:gosec // G702: the executable beside this binary or chosen by the user; no shell
	if !strings.Contains(string(b), "-runtime-file") {
		return fmt.Errorf("local: %s cannot run a local Recall database (it predates -runtime-file); upgrade polign_db", path)
	}
	return nil
}

// ensureKey returns the directory's key, generating one when the file is
// missing or malformed. The server registers the key from the file at every
// start, so a data directory restored from an older backup gets its
// credential back on the next launch instead of answering 401 for ever.
func ensureKey(dir string) (string, error) {
	key, err := ReadKey(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("local: %w", err)
	}
	if ValidKey(key) {
		return key, nil
	}
	if key, err = GenerateKey(); err != nil {
		return "", fmt.Errorf("local: %w", err)
	}
	if err := WriteKey(dir, key); err != nil {
		return "", fmt.Errorf("local: %w", err)
	}
	return key, nil
}

// ReadKey reads the directory's key file.
func ReadKey(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, keyFile))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// WriteKey writes the directory's key file, readable by the owner only.
func WriteKey(dir, key string) error {
	return recallsetup.WriteFile(filepath.Join(dir, keyFile), []byte(key+"\n"), 0o600)
}

// GenerateKey makes a key in the shape polign-server accepts from
// -bootstrap-key-file: plgn_<16 hex>_<64 hex>. The server stays the authority
// and refuses to start on a key it cannot parse.
func GenerateKey() (string, error) {
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

// ValidKey reports whether key has the shape GenerateKey makes.
func ValidKey(key string) bool {
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
