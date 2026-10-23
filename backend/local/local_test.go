package local_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Polign/recall"
	"github.com/Polign/recall/backend/local"
)

func TestHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("RECALL_HOME", "")
	t.Setenv("POLIGN_RECALL_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	if runtime.GOOS == "windows" {
		t.Skip("the default home is a Unix path")
	}
	if got, want := local.Home(), filepath.Join(home, ".local", "share", "recall"); got != want {
		t.Fatalf("default = %q, want %q", got, want)
	}
	t.Setenv("XDG_DATA_HOME", "/data")
	if got := local.Home(); got != "/data/recall" {
		t.Fatalf("XDG = %q", got)
	}
	// A setup saved where releases before 0.23.0 kept it is found.
	old := filepath.Join(home, ".config", "polign", "recall")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := local.Home(); got != old {
		t.Fatalf("with an old setup = %q, want %q", got, old)
	}
	t.Setenv("POLIGN_RECALL_HOME", "/polign-home")
	if got := local.Home(); got != "/polign-home" {
		t.Fatalf("POLIGN_RECALL_HOME = %q", got)
	}
	t.Setenv("RECALL_HOME", "/recall-home")
	if got := local.Home(); got != "/recall-home" {
		t.Fatalf("RECALL_HOME = %q", got)
	}
}

func TestResolveExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	got, err := local.Resolve("~/memory")
	if err != nil || got != filepath.Join(home, "memory") {
		t.Fatalf("%q, %v", got, err)
	}
	t.Setenv("RECALL_HOME", filepath.Join(home, "r"))
	if got, err := local.Resolve(""); err != nil || got != filepath.Join(home, "r") {
		t.Fatalf("empty = %q, %v", got, err)
	}
}

func TestKeyShape(t *testing.T) {
	key, err := local.GenerateKey()
	if err != nil || !local.ValidKey(key) {
		t.Fatalf("%q %v", key, err)
	}
	for _, bad := range []string{"", "plgn_short", "rcl_0123456789abcdef_" + key[22:], key + "0", key[:len(key)-1] + "G"} {
		if local.ValidKey(bad) {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestServerPathWithoutOneSaysHowToInstall(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := local.ServerPath(""); err == nil || !strings.Contains(err.Error(), "pip install polign-db") {
		t.Fatalf("err = %v", err)
	}
}

// polignServer finds a polign-server for the integration tests, or skips.
func polignServer(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("POLIGN_SERVER_BIN"); bin != "" {
		return bin
	}
	bin, err := exec.LookPath("polign-server")
	if err != nil {
		if os.Getenv("RECALL_REQUIRE_POLIGN") != "" {
			t.Fatal("polign-server not found: set POLIGN_SERVER_BIN or put it on PATH")
		}
		t.Skip("polign-server not found; set POLIGN_SERVER_BIN to run the integration tests")
	}
	return bin
}

func TestEnsureStartsOnceAndIsFoundAgain(t *testing.T) {
	server := polignServer(t)
	dir := filepath.Join(t.TempDir(), "memory")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	first, err := local.Ensure(ctx, local.Options{Dir: dir, Server: server})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopServer(t, dir) })
	if first.Dir != dir || first.URL == "" || !local.ValidKey(first.Key) {
		t.Fatalf("%+v", first)
	}
	for _, name := range []string{"runtime.json", "local-key", "server.log", "data"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A second caller, as another process would, finds the same server.
	second, err := local.Ensure(ctx, local.Options{Dir: dir})
	if err != nil || second.URL != first.URL || second.Key != first.Key {
		t.Fatalf("second = %+v, %v; first = %+v", second, err, first)
	}
	// Through the registry, with the directory as the URL and the server as
	// an option.
	b, err := recall.OpenBackend(local.Name, recall.BackendOptions{URL: dir + "?server=" + server})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, "probe", "e1", make([]float32, 4), map[string]any{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	got, total, err := b.List(ctx, "probe", nil, 10)
	if err != nil || total != 1 || len(got) != 1 || got[0].ID != "e1" {
		t.Fatalf("list = %v %d %v", got, total, err)
	}
}

// The conformance suite is not run here: the local backend is the polign
// backend under a server this package starts, and that server's serving mode
// (cold-first, as recall setup has always started it) answers text search
// only once an index is published. The round trip above is what the local
// backend adds; the polign backend's own tests cover the rest.

// stopServer stops the detached server a test started, by the pid in its
// runtime file, so that test directories do not leave servers behind.
func stopServer(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	var state struct {
		PID int `json:"pid"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "runtime.json"))
	if err != nil || json.Unmarshal(b, &state) != nil || state.PID <= 0 {
		return
	}
	p, err := os.FindProcess(state.PID)
	if err != nil {
		return
	}
	_ = p.Signal(os.Interrupt)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := p.Signal(syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = p.Kill()
}
