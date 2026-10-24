package recall

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// BackendOptions is what a registered backend is opened with.
type BackendOptions struct {
	// URL is the database endpoint, in the backend's own form: an HTTP(S)
	// address for polign and qdrant, a connection string for pgvector and
	// mongodb, a directory for local.
	URL string
	// APIKey is the credential, when the database wants one and the URL
	// does not carry it: the API key of polign and qdrant, the password of
	// pgvector and mongodb.
	APIKey string
	// Params are the backend's own options. OpenBackend fills them from the
	// URL's query string: every key the driver lists in Params is moved
	// here and removed from the URL, so a backend can take options on the
	// same string that names the database (postgres://host/db?schema=memory)
	// and the driver below it never sees keys it does not know.
	Params map[string]string
}

// BackendDriver is one storage backend a program can open by name. Backend
// packages register a driver from init, as database/sql drivers do, so a
// program picks backends by importing them:
//
//	import _ "github.com/Polign/recall/backend/qdrant"
//
//	b, err := recall.OpenBackend("qdrant", recall.BackendOptions{URL: "http://localhost:6333"})
type BackendDriver struct {
	// Open connects to the database. It should validate options without
	// making requests, as the built-in backends do.
	Open func(BackendOptions) (Backend, error)
	// DefaultURL is the endpoint used when none is given.
	DefaultURL string
	// URLEnv and KeyEnv name the environment variables the recall command
	// read the endpoint and credential from before 0.23.0, such as
	// QDRANT_URL and QDRANT_API_KEY. The command reads them still, as
	// deprecated aliases of RECALL_BACKEND_URL and RECALL_BACKEND_KEY.
	//
	// Deprecated: new backends need not set them; they go away with the
	// aliases.
	URLEnv, KeyEnv string
	// Params lists the query keys the backend takes as options. OpenBackend
	// moves them from the URL to BackendOptions.Params before Open runs.
	// Keys not listed stay on the URL for the driver underneath.
	Params []string
}

var (
	driversMu sync.RWMutex
	drivers   = map[string]BackendDriver{}
)

// RegisterBackend makes a backend available by name. It panics when the name
// is empty or taken or the driver has no Open, because each is a programming
// error that should fail at startup.
func RegisterBackend(name string, d BackendDriver) {
	driversMu.Lock()
	defer driversMu.Unlock()
	if strings.TrimSpace(name) == "" || d.Open == nil {
		panic("recall: RegisterBackend needs a name and an Open function")
	}
	if _, dup := drivers[name]; dup {
		panic("recall: RegisterBackend called twice for " + name)
	}
	drivers[name] = d
}

// LookupBackend returns the driver registered under name.
func LookupBackend(name string) (BackendDriver, bool) {
	driversMu.RLock()
	defer driversMu.RUnlock()
	d, ok := drivers[name]
	return d, ok
}

// Backends lists the registered backend names, sorted.
func Backends() []string {
	driversMu.RLock()
	defer driversMu.RUnlock()
	names := make([]string, 0, len(drivers))
	for name := range drivers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// OpenBackend opens the backend registered under name. An empty URL means the
// driver's DefaultURL. The query keys the driver lists in Params are moved
// from the URL into opts.Params; a key given in both places keeps the one in
// opts.Params.
func OpenBackend(name string, opts BackendOptions) (Backend, error) {
	d, ok := LookupBackend(name)
	if !ok {
		return nil, fmt.Errorf("recall: unknown backend %q (registered: %s)", name, strings.Join(Backends(), ", "))
	}
	if opts.URL == "" {
		opts.URL = d.DefaultURL
	}
	if len(d.Params) > 0 && strings.Contains(opts.URL, "?") {
		rest, params, err := SplitParams(opts.URL, d.Params)
		if err != nil {
			return nil, fmt.Errorf("recall: %s: %w", name, err)
		}
		opts.URL = rest
		if len(params) > 0 && opts.Params == nil {
			opts.Params = map[string]string{}
		}
		for k, v := range params {
			if _, given := opts.Params[k]; !given {
				opts.Params[k] = v
			}
		}
	}
	return d.Open(opts)
}

// SplitParams takes the listed keys out of rawURL's query string and
// returns the URL without them beside the values found. Keys not listed are
// left in place. A key given twice is an error, since the two values cannot
// both be meant.
//
// Only the query string is parsed. The rest of the URL is passed through
// untouched, so connection strings Go's URL parser would refuse but the
// driver underneath accepts (a MongoDB host list, say) still work; a URL
// without any of the keys comes back as it was.
func SplitParams(rawURL string, keys []string) (string, map[string]string, error) {
	base, query, found := strings.Cut(rawURL, "?")
	if !found {
		return rawURL, nil, nil
	}
	fragment := ""
	if i := strings.IndexByte(query, '#'); i >= 0 {
		query, fragment = query[:i], query[i:]
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		return "", nil, err
	}
	params := map[string]string{}
	for _, k := range keys {
		vals, ok := q[k]
		if !ok {
			continue
		}
		if len(vals) != 1 {
			return "", nil, fmt.Errorf("option %s is given %d times", k, len(vals))
		}
		params[k] = vals[0]
		q.Del(k)
	}
	if len(params) == 0 {
		return rawURL, nil, nil
	}
	rest := base
	if enc := q.Encode(); enc != "" {
		rest += "?" + enc
	}
	return rest + fragment, params, nil
}
