//go:build !windows

package recallsetup

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Lock uses a kernel lock, released even after a crash. Never unlink the lock
// file: a replacement inode would let two servers own the same data directory.
func Lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another process holds %s: %w", path, err)
	}
	return f, nil
}
