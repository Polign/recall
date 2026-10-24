package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Polign/recall/internal/recallsetup"
)

// testResolver resolves with a fixed environment and no database answering
// at the old default address unless the test says so.
func testResolver(env map[string]string, legacyAnswers bool, warn *strings.Builder) resolver {
	return resolver{
		env:   func(k string) string { return env[k] },
		probe: func() bool { return legacyAnswers },
		warn:  warn,
	}
}

// flags parses args into a fresh set of database flags.
func flags(t *testing.T, args ...string) *dbFlags {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	f := addDatabaseFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return f
}

func saveSetup(t *testing.T, dir string, cfg recallsetup.Config) {
	t.Helper()
	if err := recallsetup.Write(filepath.Join(dir, "config.json"), cfg); err != nil {
		t.Fatal(err)
	}
}

// resetWarnings forgets which deprecation notes were printed, so each test
// sees its own.
func resetWarnings() {
	warnedMu.Lock()
	defer warnedMu.Unlock()
	warned = map[string]bool{}
}

func TestResolveDatabase(t *testing.T) {
	t.Cleanup(resetWarnings)

	legacyLocal := recallsetup.Config{Server: "/opt/polign-server", Key: "plgn_local", Collection: "c"} // a 0.22 local setup: no backend field
	legacyRemote := recallsetup.Config{URL: "http://memory.internal:23000", Key: "plgn_remote", Collection: "c"}
	qdrantSetup := recallsetup.Config{Backend: "qdrant", URL: "http://q:6333", Key: "qk", Collection: "c"}

	cases := []struct {
		name        string
		global, sub []string
		env         map[string]string
		saved       *recallsetup.Config
		explicitDir bool
		legacy      bool

		backend, url, key string
		backendFrom       string
		fromSetup         bool
		server            string
		wantErr           string
		wantWarn          string
	}{
		{name: "nothing: local in the configuration directory", backend: "local", url: "DIR", backendFrom: "default"},
		{name: "0.22 default address answers", legacy: true, backend: "polign", url: legacyDefaultURL, backendFrom: "a polign_db answering at " + legacyDefaultURL, wantWarn: "RECALL_BACKEND=polign"},
		{name: "RECALL_BACKEND alone takes the default URL", env: map[string]string{"RECALL_BACKEND": "qdrant"}, backend: "qdrant", url: "http://localhost:6333", backendFrom: "RECALL_BACKEND"},
		{name: "the three new variables", env: map[string]string{"RECALL_BACKEND": "pgvector", "RECALL_BACKEND_URL": "postgres://u@h/db", "RECALL_BACKEND_KEY": "pw"}, backend: "pgvector", url: "postgres://u@h/db", key: "pw", backendFrom: "RECALL_BACKEND"},
		{name: "flags before the command", global: []string{"-backend", "mongodb", "-url", "mongodb://h/db", "-key", "pw"}, backend: "mongodb", url: "mongodb://h/db", key: "pw", backendFrom: "-backend"},
		{name: "flags after the command win over flags before it", global: []string{"-backend", "qdrant"}, sub: []string{"-backend", "polign", "-url", "http://h:23000"}, backend: "polign", url: "http://h:23000", backendFrom: "-backend"},
		{name: "flag over environment", global: []string{"-url", "postgres://flag/db"}, env: map[string]string{"RECALL_BACKEND": "pgvector", "RECALL_BACKEND_URL": "postgres://env/db"}, backend: "pgvector", url: "postgres://flag/db"},
		// 0.22 names.
		{name: "POLIGN_URL alone means polign there", env: map[string]string{"POLIGN_URL": "http://h:23000", "POLIGN_API_KEY": "k"}, backend: "polign", url: "http://h:23000", key: "k", backendFrom: "POLIGN_URL", wantWarn: "RECALL_BACKEND_URL replaces POLIGN_URL"},
		{name: "RECALL_BACKEND=qdrant with QDRANT_URL", env: map[string]string{"RECALL_BACKEND": "qdrant", "QDRANT_URL": "http://q:6333", "QDRANT_API_KEY": "qk"}, backend: "qdrant", url: "http://q:6333", key: "qk", wantWarn: "RECALL_BACKEND_KEY replaces QDRANT_API_KEY"},
		{name: "RECALL_BACKEND=pgvector with DATABASE_URL and PGPASSWORD", env: map[string]string{"RECALL_BACKEND": "pgvector", "DATABASE_URL": "postgres://h/db", "PGPASSWORD": "pw"}, backend: "pgvector", url: "postgres://h/db", key: "pw", wantWarn: "RECALL_BACKEND_URL replaces DATABASE_URL"},
		{name: "RECALL_BACKEND=mongodb with MONGODB_URI", env: map[string]string{"RECALL_BACKEND": "mongodb", "MONGODB_URI": "mongodb://h/db", "MONGODB_PASSWORD": "pw"}, backend: "mongodb", url: "mongodb://h/db", key: "pw", wantWarn: "RECALL_BACKEND_URL replaces MONGODB_URI"},
		{name: "new URL name wins over the old", env: map[string]string{"RECALL_BACKEND": "qdrant", "RECALL_BACKEND_URL": "http://new:6333", "QDRANT_URL": "http://old:6333"}, backend: "qdrant", url: "http://new:6333"},
		{name: "another application's DATABASE_URL is not followed", env: map[string]string{"DATABASE_URL": "postgres://app/db"}, backend: "local", url: "DIR"},
		// Saved setups.
		{name: "a 0.22 local setup", saved: &legacyLocal, backend: "local", url: "DIR", server: "/opt/polign-server", fromSetup: true, backendFrom: "the setup saved in DIR"},
		{name: "a 0.22 remote setup", saved: &legacyRemote, backend: "polign", url: "http://memory.internal:23000", key: "plgn_remote", fromSetup: true},
		{name: "a saved qdrant setup", saved: &qdrantSetup, backend: "qdrant", url: "http://q:6333", key: "qk", fromSetup: true},
		{name: "POLIGN_URL beats a saved local setup, as before 0.23", saved: &legacyLocal, env: map[string]string{"POLIGN_URL": "http://other:23000"}, backend: "polign", url: "http://other:23000", backendFrom: "POLIGN_URL", fromSetup: false, wantWarn: "POLIGN_URL"},
		{name: "the environment beats the saved setup", saved: &legacyRemote, env: map[string]string{"POLIGN_URL": "http://other:23000"}, backend: "polign", url: "http://other:23000", fromSetup: false, wantWarn: "POLIGN_URL"},
		{name: "an explicit -config-dir beats the environment", saved: &legacyRemote, explicitDir: true, env: map[string]string{"POLIGN_URL": "http://other:23000", "RECALL_BACKEND": "qdrant"}, backend: "polign", url: "http://memory.internal:23000", key: "plgn_remote", fromSetup: true},
		{name: "an explicit -config-dir with nothing saved", explicitDir: true, wantErr: "no setup is saved"},
		{name: "RECALL_BACKEND matching the saved setup takes its URL", saved: &qdrantSetup, env: map[string]string{"RECALL_BACKEND": "qdrant"}, backend: "qdrant", url: "http://q:6333", key: "qk", fromSetup: true},
		{name: "a different backend ignores the saved setup", saved: &qdrantSetup, env: map[string]string{"RECALL_BACKEND": "polign"}, backend: "polign", url: legacyDefaultURL, fromSetup: false},
		{name: "a saved setup beats the 0.22 default address", saved: &qdrantSetup, legacy: true, backend: "qdrant", url: "http://q:6333", key: "qk", fromSetup: true},
		// Mismatches.
		{name: "a postgres URL on the default backend", env: map[string]string{"RECALL_BACKEND_URL": "postgres://h/db"}, wantErr: "Set RECALL_BACKEND=pgvector"},
		{name: "an http URL on the default backend", env: map[string]string{"RECALL_BACKEND_URL": "http://h:23000"}, wantErr: "polign or qdrant"},
		{name: "a mongodb URL on pgvector", env: map[string]string{"RECALL_BACKEND": "pgvector", "RECALL_BACKEND_URL": "mongodb://h/db"}, wantErr: "Set RECALL_BACKEND=mongodb"},
		{name: "a directory on polign", global: []string{"-backend", "polign", "-url", "/tmp/memory"}, wantErr: "not in the polign backend's form"},
		{name: "an unknown backend", env: map[string]string{"RECALL_BACKEND": "redis"}, wantErr: "unknown backend"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			home := filepath.Join(t.TempDir(), "home")
			t.Setenv("RECALL_HOME", home)
			if tc.saved != nil {
				saveSetup(t, dir, *tc.saved)
			}
			var warn strings.Builder
			r := testResolver(tc.env, tc.legacy, &warn)
			resetWarnings()
			d, err := r.resolve(flags(t, tc.global...), flags(t, tc.sub...), dir, tc.explicitDir)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantURL := strings.NewReplacer("HOME", home, "DIR", dir).Replace(tc.url)
			if d.Backend != tc.backend || d.URL != wantURL || d.Key != tc.key {
				t.Fatalf("got %s %s key %q; want %s %s key %q", d.Backend, d.URL, d.Key, tc.backend, wantURL, tc.key)
			}
			if tc.backendFrom != "" && d.BackendFrom != strings.ReplaceAll(tc.backendFrom, "DIR", dir) {
				t.Fatalf("backend from %q, want %q", d.BackendFrom, tc.backendFrom)
			}
			if d.fromSetup() != tc.fromSetup {
				t.Fatalf("fromSetup = %v, want %v", d.fromSetup(), tc.fromSetup)
			}
			if d.Params["server"] != tc.server {
				t.Fatalf("server = %q, want %q", d.Params["server"], tc.server)
			}
			if tc.wantWarn != "" && !strings.Contains(warn.String(), tc.wantWarn) {
				t.Fatalf("stderr %q lacks %q", warn.String(), tc.wantWarn)
			}
			if tc.wantWarn == "" && warn.Len() > 0 {
				t.Fatalf("unexpected stderr: %s", warn.String())
			}
		})
	}
}

func TestDatabaseFlagsGivenMeansSetEvenWhenEmpty(t *testing.T) {
	f := flags(t, "-key", "")
	if !f.given("key") || f.given("url") {
		t.Fatalf("given: key %v url %v", f.given("key"), f.given("url"))
	}
	var none *dbFlags
	if none.given("key") {
		t.Fatal("a nil flag set reported a flag")
	}
}

func TestSettingReadsNewNameThenOldWithOneNote(t *testing.T) {
	var warn strings.Builder
	r := testResolver(map[string]string{"POLIGN_COLLECTION": "old", "RECALL_PREDICATES": "new.json", "POLIGN_PREDICATES": "old.json"}, false, &warn)
	resetWarnings()
	t.Cleanup(resetWarnings)
	if got := r.setting("COLLECTION"); got != "old" {
		t.Fatalf("COLLECTION = %q", got)
	}
	if got := r.setting("PREDICATES"); got != "new.json" {
		t.Fatalf("PREDICATES = %q", got)
	}
	if got := r.setting("EMBED_MODEL"); got != "" {
		t.Fatalf("EMBED_MODEL = %q", got)
	}
	r.setting("COLLECTION")
	if n := strings.Count(warn.String(), "POLIGN_COLLECTION"); n != 1 || strings.Contains(warn.String(), "PREDICATES") {
		t.Fatalf("stderr = %q", warn.String())
	}
}

func TestSetupTargetKeepsTheSavedDatabaseUnlessAFlagChangesIt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("RECALL_HOME", filepath.Join(t.TempDir(), "home"))
	saved := recallsetup.Config{Backend: "qdrant", URL: "http://q:6333", Key: "qk", Collection: "c"}
	saveSetup(t, dir, saved)
	orig := databases
	databases.env = func(k string) string {
		return map[string]string{"RECALL_BACKEND": "polign", "RECALL_BACKEND_URL": "http://env:23000"}[k]
	}
	databases.probe = func() bool { return true }
	databases.warn = &strings.Builder{}
	t.Cleanup(func() { databases = orig })

	// A rerun with no database flag keeps what was saved, whatever the
	// environment says and whatever answers at the old address.
	d, err := setupTarget(flags(t), flags(t), dir, saved, false)
	if err != nil || d.Backend != "qdrant" || d.URL != "http://q:6333" || d.Key != "qk" {
		t.Fatalf("rerun = %v %v", d, err)
	}
	// -url alone keeps the saved backend when the URL fits it.
	d, err = setupTarget(flags(t), flags(t, "-url", "http://q2:6333"), dir, saved, false)
	if err != nil || d.Backend != "qdrant" || d.URL != "http://q2:6333" || d.Key != "" {
		t.Fatalf("-url on qdrant = %v %v", d, err)
	}
	// -url alone on a local setup means polign, as it did before 0.23.
	local := recallsetup.Config{Server: "/opt/polign-server", Collection: "c"}
	d, err = setupTarget(flags(t), flags(t, "-url", "http://h:23000"), dir, local, false)
	if err != nil || d.Backend != "polign" || d.URL != "http://h:23000" {
		t.Fatalf("-url on local = %v %v", d, err)
	}
	// -backend alone takes the URL from the environment, not the saved one.
	d, err = setupTarget(flags(t), flags(t, "-backend", "polign"), dir, saved, false)
	if err != nil || d.Backend != "polign" || d.URL != "http://env:23000" {
		t.Fatalf("-backend = %v %v", d, err)
	}
	// A first setup never follows a database answering at the old address,
	// and keeps the local database in the configuration directory.
	databases.env = func(string) string { return "" }
	fresh := t.TempDir()
	d, err = setupTarget(flags(t), flags(t), fresh, recallsetup.Config{}, true)
	if err != nil || d.Backend != "local" || d.URL != fresh {
		t.Fatalf("first = %v %v", d, err)
	}
}

func TestSetupDatabaseReadsA022LocalSetup(t *testing.T) {
	dir := t.TempDir()
	d := setupDatabase(dir, recallsetup.Config{Server: "/opt/polign-server", Key: "plgn_x"})
	if d.Backend != "local" || d.URL != dir || d.Params["server"] != "/opt/polign-server" || !d.fromSetup() {
		t.Fatalf("%+v", d)
	}
	if !strings.Contains(d.String(), dir) {
		t.Fatalf("String() = %q", d.String())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

func TestInitializeReportsTheDatabase(t *testing.T) {
	d := database{Backend: "local", URL: t.TempDir()}
	s := &mcpServer{api: &api{base: "http://unused"}, database: d.report(&api{base: "http://127.0.0.1:9"})}
	reply := s.initialize(nil).(map[string]any)
	info := reply["serverInfo"].(map[string]any)
	got := info["database"].(map[string]any)
	if got["backend"] != "local" || got["url"] != d.URL || got["server"] != "http://127.0.0.1:9" {
		t.Fatalf("database = %v", got)
	}
	if _, leaked := got["key"]; leaked {
		t.Fatal("the key must not be reported")
	}
	bare := (&mcpServer{api: &api{base: "http://unused"}}).initialize(nil).(map[string]any)
	if _, ok := bare["serverInfo"].(map[string]any)["database"]; ok {
		t.Fatal("a server with no database resolved reported one")
	}
	remote := database{Backend: "qdrant", URL: "http://q:6333", Key: "secret"}.report(&api{base: "http://q:6333"})
	if remote["server"] != nil || remote["url"] != "http://q:6333" {
		t.Fatalf("remote = %v", remote)
	}
}
