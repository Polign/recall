package recallsetup

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLockExcludesAnotherServerAndSurvivesReuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.lock")
	first, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := Lock(path); err == nil {
		second.Close()
		t.Fatal("two owners acquired the lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	third.Close()
}

func TestConfigurationReplacementStaysPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("NTFS has no Unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, Config{Key: "secret"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	var cfg Config
	if err := Read(path, &cfg); err != nil || cfg.Key != "secret" {
		t.Fatalf("read = %#v, %v", cfg, err)
	}
}
