//go:build windows

package recallsetup

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// Lock takes an exclusive byte-range lock over the whole file. Like flock on
// Unix it is a kernel lock, so it is released when the process exits for any
// reason, including a crash. Never delete the lock file: a replacement would
// let two servers own the same data directory.
func Lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// Lock the maximal range so any overlapping lock attempt conflicts,
	// regardless of the file's length.
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, ^uint32(0), ^uint32(0), ol)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another process holds %s: %w", path, err)
	}
	return f, nil
}
