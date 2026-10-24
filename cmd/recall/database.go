package main

// One way to say where memory lives. Three things describe the database,
// the same three for every backend and every command:
//
//	-backend NAME   RECALL_BACKEND       local, polign, pgvector, mongodb, qdrant
//	-url URL        RECALL_BACKEND_URL   in the backend's own form
//	-key KEY        RECALL_BACKEND_KEY   when the backend takes one and the URL does not carry it
//
// Each resolves in the same order: the flag, the environment, the setup
// recall setup saved, the default. With nothing given the backend is local,
// a polign_db Recall runs itself in local.Home(). Anything a backend wants
// beyond that is a query key on the URL (postgres://host/db?schema=memory),
// which recall.OpenBackend hands to the driver.
//
// The names each backend had before 0.23.0 (POLIGN_URL, QDRANT_URL,
// DATABASE_URL and their keys) keep working as aliases, with one line on
// stderr naming the new one. So does the one case 0.23.0 would otherwise
// change: a polign_db already answering at the old default address is used
// when nothing names a backend, because that is where 0.22 looked.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Polign/recall"
	"github.com/Polign/recall/backend/local"
	"github.com/Polign/recall/internal/recallsetup"
)

// legacyDefaultURL is where recall looked before 0.23.0 when nothing named a
// database: the polign backend's default address.
const legacyDefaultURL = "http://localhost:23000"

// dbFlags are the three database flags, on the command line before the
// subcommand or after it.
type dbFlags struct {
	fs                *flag.FlagSet
	backend, url, key *string
}

// addDatabaseFlags defines the database flags on fs.
func addDatabaseFlags(fs *flag.FlagSet) *dbFlags {
	f := &dbFlags{fs: fs}
	f.backend = fs.String("backend", "", "which backend keeps memory: "+strings.Join(recall.Backends(), ", ")+" (default $RECALL_BACKEND, then the saved setup's, then local: a polign_db recall runs itself in "+local.Home()+")")
	f.url = fs.String("url", "", "where the database is, in the backend's own form: a directory for local, http://host:23000 for polign, postgres://user@host/db for pgvector, mongodb://host/db for mongodb, http://host:6333 for qdrant; options go on the query string, such as ?schema=memory (default $RECALL_BACKEND_URL, then the saved setup's, then the backend's default)")
	f.key = fs.String("key", "", "the database credential when the backend takes one and the URL does not carry it: the API key of polign and qdrant, the password of pgvector and mongodb (default $RECALL_BACKEND_KEY, then the saved setup's)")
	return f
}

// given reports whether the flag was set on the command line, so that an
// empty value given on purpose still counts.
func (f *dbFlags) given(name string) bool {
	if f == nil {
		return false
	}
	set := false
	f.fs.Visit(func(fl *flag.Flag) { set = set || fl.Name == name })
	return set
}

func (f *dbFlags) value(name string) string {
	switch name {
	case "backend":
		return *f.backend
	case "url":
		return *f.url
	}
	return *f.key
}

// database is where memory lives, resolved, with where each part came from.
type database struct {
	Backend string
	URL     string
	Key     string
	// Params are the backend's options not carried on the URL: the local
	// backend's polign-server, from a saved setup.
	Params map[string]string
	// BackendFrom, URLFrom and KeyFrom say which flag, variable or file
	// decided each part, for doctor and for error messages.
	BackendFrom, URLFrom, KeyFrom string
	// Dir is the configuration directory consulted for a saved setup.
	Dir string
	// Setup is the saved setup, when it supplied the backend or the URL.
	Setup *recallsetup.Config
}

// String names the database without its key.
func (d database) String() string {
	return d.Backend + " " + d.displayURL()
}

func (d database) displayURL() string {
	if d.Backend == local.Name {
		if dir, err := local.Resolve(d.URL); err == nil {
			return dir
		}
	}
	return d.URL
}

// sources says where each part came from, for doctor.
func (d database) sources() string {
	s := "backend from " + d.BackendFrom + ", URL from " + d.URLFrom
	if d.Key != "" {
		s += ", key from " + d.KeyFrom
	}
	return s
}

// report is what the MCP initialize reply says about where memory lives,
// for a client that named nothing: the backend and URL, and for the local
// backend the address its server listens on. Never the key.
func (d database) report(c *api) map[string]any {
	out := map[string]any{"backend": d.Backend, "url": d.displayURL()}
	if d.Backend == local.Name && c != nil {
		out["server"] = c.base
	}
	return out
}

// fromSetup reports whether the saved setup decided where memory lives, so a
// command can also take its collection, predicates and embedder.
func (d database) fromSetup() bool { return d.Setup != nil }

// connect returns the HTTP client the memory layer opens the backend with.
// For the local backend this starts the directory's server when it is not
// running and connects to it as a polign_db.
func (d database) connect(ctx context.Context) (*api, error) {
	if d.Backend == local.Name {
		conn, err := local.Ensure(ctx, local.Options{Dir: d.URL, Server: d.Params["server"]})
		if err != nil {
			return nil, err
		}
		return &api{base: conn.URL, key: conn.Key, backend: "polign"}, nil
	}
	return &api{base: strings.TrimRight(d.URL, "/"), key: d.Key, backend: d.Backend, params: d.Params}, nil
}

// resolver works out the database from flags, the environment, a saved
// setup and the defaults. Tests give it their own environment and probe.
type resolver struct {
	env   func(string) string
	probe func() bool // a polign_db answers at legacyDefaultURL
	warn  io.Writer
}

var databases = resolver{env: os.Getenv, probe: probeLegacyDefault, warn: os.Stderr}

// resolveDatabase is databases.resolve.
func resolveDatabase(global, sub *dbFlags, dir string, explicitDir bool) (database, error) {
	return databases.resolve(global, sub, dir, explicitDir)
}

// resolve finds the database. dir is the configuration directory, consulted
// for a saved setup; explicitDir means the command line named it, which the
// plugin's launcher does so that a stray variable cannot redirect Claude's
// memory, and then the saved setup wins over the environment.
func (r resolver) resolve(global, sub *dbFlags, dir string, explicitDir bool) (database, error) {
	d := database{Dir: dir, Params: map[string]string{}}
	flag := func(name string) (string, string) {
		for _, f := range []*dbFlags{sub, global} {
			if f.given(name) {
				return f.value(name), "-" + name
			}
		}
		return "", ""
	}
	var saved *recallsetup.Config
	if dir != "" {
		var cfg recallsetup.Config
		if err := recallsetup.Read(filepath.Join(dir, "config.json"), &cfg); err == nil {
			saved = &cfg
		} else if explicitDir {
			return d, fmt.Errorf("no setup is saved in %s; run recall setup (%w)", dir, err)
		}
	}
	// The saved setup decides where memory lives when it supplies the URL;
	// its collection, predicates and embedder then apply too (fromSetup).
	useSaved := func(what string) {
		switch what {
		case "backend":
			d.Backend, d.BackendFrom = saved.BackendName(), "the setup saved in "+dir
		case "url":
			d.Setup = saved
			d.URL, d.URLFrom = saved.URL, "the setup saved in "+dir
			if saved.BackendName() == local.Name {
				if d.URL == "" {
					d.URL = dir
				}
				if saved.Server != "" {
					d.Params["server"] = saved.Server
				}
			}
		case "key":
			// The local backend's key lives in its directory, not here.
			if saved.BackendName() != local.Name {
				d.Key, d.KeyFrom = saved.Key, "the setup saved in "+dir
			}
		}
	}

	// The backend.
	legacy := false
	d.Backend, d.BackendFrom = flag("backend")
	if d.Backend == "" && explicitDir {
		useSaved("backend")
	}
	if d.Backend == "" {
		if v := r.env("RECALL_BACKEND"); v != "" {
			d.Backend, d.BackendFrom = v, "RECALL_BACKEND"
		}
	}
	if d.Backend == "" && r.env("RECALL_BACKEND_URL") == "" && r.env("POLIGN_URL") != "" {
		// Before 0.23.0, POLIGN_URL on its own named a polign_db, and it
		// beat the saved setup, as the environment does here.
		d.Backend, d.BackendFrom = "polign", "POLIGN_URL"
	}
	if d.Backend == "" && saved != nil {
		useSaved("backend")
	}
	if d.Backend == "" && r.probe() {
		d.Backend, d.BackendFrom, legacy = "polign", "a polign_db answering at "+legacyDefaultURL, true
		fmt.Fprintf(r.warn, "recall: a polign_db answers at %s, so memory is kept there, as before 0.23. Set RECALL_BACKEND=polign to say so, or RECALL_BACKEND=local to use the local database in %s instead.\n", legacyDefaultURL, local.Home())
	}
	if d.Backend == "" {
		d.Backend, d.BackendFrom = local.Name, "default"
	}
	driver, ok := recall.LookupBackend(d.Backend)
	if !ok {
		return d, fmt.Errorf("unknown backend %q from %s (want one of %s)", d.Backend, d.BackendFrom, strings.Join(recall.Backends(), ", "))
	}
	savedFits := saved != nil && saved.BackendName() == d.Backend

	// The URL.
	d.URL, d.URLFrom = flag("url")
	if d.URL == "" && explicitDir && savedFits {
		useSaved("url")
	}
	if d.URL == "" {
		if v := r.env("RECALL_BACKEND_URL"); v != "" {
			d.URL, d.URLFrom = v, "RECALL_BACKEND_URL"
		}
	}
	if d.URL == "" && driver.URLEnv != "" {
		if v := r.env(driver.URLEnv); v != "" {
			d.URL, d.URLFrom = v, driver.URLEnv
			r.deprecated(driver.URLEnv, "RECALL_BACKEND_URL")
		}
	}
	if d.URL == "" && savedFits {
		useSaved("url")
	}
	if d.URL == "" && legacy {
		d.URL, d.URLFrom = legacyDefaultURL, "the address recall used before 0.23"
	}
	if d.URL == "" {
		d.URL, d.URLFrom = driver.DefaultURL, "default"
		if d.Backend == local.Name {
			// The local database lives in the configuration directory, as
			// recall setup -local has always kept it; that is RECALL_HOME
			// unless -config-dir says otherwise.
			d.URL = dir
			if d.URL == "" {
				d.URL = local.Home()
			}
			if d.URL == "" {
				return d, errors.New("no home directory for the local database; set RECALL_HOME or name a database with -backend and -url")
			}
		}
	}
	if err := fits(d.Backend, d.URL, d.URLFrom); err != nil {
		return d, err
	}

	// The key.
	d.Key, d.KeyFrom = flag("key")
	if d.Key == "" && explicitDir && savedFits {
		useSaved("key")
	}
	if d.Key == "" {
		if v := r.env("RECALL_BACKEND_KEY"); v != "" {
			d.Key, d.KeyFrom = v, "RECALL_BACKEND_KEY"
		}
	}
	if d.Key == "" && driver.KeyEnv != "" {
		if v := r.env(driver.KeyEnv); v != "" {
			d.Key, d.KeyFrom = v, driver.KeyEnv
			r.deprecated(driver.KeyEnv, "RECALL_BACKEND_KEY")
		}
	}
	if d.Key == "" && savedFits && d.URLFrom == "the setup saved in "+dir {
		useSaved("key")
	}
	if d.Key == "" {
		d.KeyFrom = "none"
	}
	return d, nil
}

// fits checks that the URL is in the backend's own form, so that a
// connection string meant for one database is never handed to another. The
// error names the backend the URL fits.
func fits(backend, rawURL, from string) error {
	scheme := ""
	if i := strings.Index(rawURL, "://"); i >= 0 {
		scheme = strings.ToLower(rawURL[:i])
	}
	var want []string
	switch backend {
	case local.Name:
		want = []string{""}
	case "polign", "qdrant":
		want = []string{"http", "https"}
	case "pgvector":
		want = []string{"postgres", "postgresql"}
	case "mongodb":
		want = []string{"mongodb", "mongodb+srv"}
	default:
		return nil
	}
	for _, w := range want {
		if scheme == w {
			return nil
		}
	}
	form := "a directory"
	if want[0] != "" {
		form = "a " + strings.Join(want, ":// or ") + ":// URL"
	}
	msg := fmt.Sprintf("the URL from %s is not in the %s backend's form (%s)", from, backend, form)
	switch other := backendForScheme(scheme); {
	case other != "":
		msg += fmt.Sprintf("; it looks like a %s URL. Set RECALL_BACKEND=%s to use it, or change the URL", other, other)
	case scheme == "http" || scheme == "https":
		msg += "; it looks like a polign or qdrant URL. Set RECALL_BACKEND to one of them, or change the URL"
	}
	return errors.New(msg)
}

// backendForScheme names the backend a URL scheme belongs to, when only one
// does. http is shared by polign and qdrant, so it names neither.
func backendForScheme(scheme string) string {
	switch scheme {
	case "postgres", "postgresql":
		return "pgvector"
	case "mongodb", "mongodb+srv":
		return "mongodb"
	case "":
		return local.Name
	}
	return ""
}

// deprecated says once per name that an old variable still works and what
// replaces it.
func (r resolver) deprecated(old, replacement string) {
	warnedMu.Lock()
	defer warnedMu.Unlock()
	if warned[old] {
		return
	}
	warned[old] = true
	fmt.Fprintf(r.warn, "recall: %s replaces %s, which still works for now\n", replacement, old)
}

var (
	warnedMu sync.Mutex
	warned   = map[string]bool{}
)

// probeLegacyDefault reports whether something answers at the address
// recall used before 0.23.0. Nothing listening is refused at once on the
// loopback interface, so the check costs nothing on a machine without one.
func probeLegacyDefault() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, legacyDefaultURL+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// setting reads one of Recall's own settings: RECALL_<name>, else the
// POLIGN_<name> spelling every release before 0.23.0 used, which keeps
// working with a line on stderr naming the new one.
func setting(name string) string {
	return databases.setting(name)
}

func (r resolver) setting(name string) string {
	if v := r.env("RECALL_" + name); v != "" {
		return v
	}
	if v := r.env("POLIGN_" + name); v != "" {
		r.deprecated("POLIGN_"+name, "RECALL_"+name)
		return v
	}
	return ""
}

// settingOr is setting with a default.
func settingOr(name, def string) string {
	if v := setting(name); v != "" {
		return v
	}
	return def
}

// recallHome is the configuration directory: local.Home().
func recallHome() string { return local.Home() }
