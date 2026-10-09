package recall

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// BackendOptions is what a registered backend is opened with.
type BackendOptions struct {
	// URL is the database endpoint.
	URL string
	// APIKey is the credential, when the database wants one.
	APIKey string
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
	// URLEnv and KeyEnv name the environment variables a program reads the
	// endpoint and credential from when its flags do not set them.
	URLEnv, KeyEnv string
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
// driver's DefaultURL.
func OpenBackend(name string, opts BackendOptions) (Backend, error) {
	d, ok := LookupBackend(name)
	if !ok {
		return nil, fmt.Errorf("recall: unknown backend %q (registered: %s)", name, strings.Join(Backends(), ", "))
	}
	if opts.URL == "" {
		opts.URL = d.DefaultURL
	}
	return d.Open(opts)
}
