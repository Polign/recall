package recallsetup

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLockWaitWaitsForABriefHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setup.lock")
	held, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = held.Close()
	}()
	start := time.Now()
	got, err := LockWait(path, 5*time.Second)
	if err != nil {
		t.Fatalf("LockWait while a brief holder finishes: %v", err)
	}
	defer got.Close()
	if waited := time.Since(start); waited < 200*time.Millisecond {
		t.Fatalf("took the lock after %s while it was still held", waited)
	}
}

func TestLockWaitGivesUpAtItsTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setup.lock")
	held, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if f, err := LockWait(path, 300*time.Millisecond); err == nil {
		_ = f.Close()
		t.Fatal("LockWait took a lock another holder still has")
	}
}
