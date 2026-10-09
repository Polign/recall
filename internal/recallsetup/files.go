// Package recallsetup holds the private on-disk configuration shared by the
// Recall launcher and the local server. It never lives in a plugin cache.
package recallsetup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

type Config struct {
	Executable string `json:"executable"`
	Server     string `json:"server,omitempty"`
	URL        string `json:"url,omitempty"`
	Key        string `json:"api_key,omitempty"`
	Collection string `json:"collection"`
	Predicates string `json:"predicates,omitempty"`
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
