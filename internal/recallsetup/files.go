// Package recallsetup holds the private on-disk configuration shared by the
// Recall launcher and the local server. It never lives in a plugin cache.
package recallsetup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

type Config struct {
	Executable string `json:"executable"`
	// Backend is the registered backend name the setup connects to. Setups
	// written before 0.23.0 have none: with a URL they meant polign, without
	// one the managed local database. BackendName reads either.
	Backend string `json:"backend,omitempty"`
	// Server is the polign-server the local backend runs, when one was chosen.
	Server string `json:"server,omitempty"`
	// URL is the database, in the backend's own form. For the local backend
	// it is the data directory, or empty for the configuration directory.
	URL        string `json:"url,omitempty"`
	Key        string `json:"api_key,omitempty"`
	Collection string `json:"collection"`
	Predicates string `json:"predicates,omitempty"`
	// ExtractModel is the provider:model that works out the statements in
	// remembered text. With one, Recall serves the text-only tools.
	ExtractModel string `json:"extract_model,omitempty"`
	// Open lets writes define predicates, so no predicates file is needed.
	// Setups made before it existed keep a closed vocabulary.
	Open bool `json:"open,omitempty"`
	// Typed keeps the typed tools even when a model is saved: the agent
	// names predicates and the model extracts nothing.
	Typed bool `json:"typed,omitempty"`
	// EmbedModel is the provider:model[@dim] that embeds memory, so recall
	// searches by meaning. Empty is Recall's built-in word search, which is
	// what every setup made before it existed keeps.
	EmbedModel string `json:"embed_model,omitempty"`
}

// BackendName is the backend a setup connects to, reading setups written
// before the field existed as what they meant then.
func (c Config) BackendName() string {
	switch {
	case c.Backend != "":
		return c.Backend
	case strings.Contains(c.URL, "://"):
		return "polign"
	}
	return "local"
}

type Runtime struct {
	URL string `json:"url"`
	PID int    `json:"pid"`
}

func Read(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func Write(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(path, append(b, '\n'), 0o600)
}

func WriteFile(path string, b []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".recall-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir makes a rename in dir durable. Windows has no directory fsync, and
// some filesystems refuse it; both are treated as done.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
