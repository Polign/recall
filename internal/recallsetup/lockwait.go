package recallsetup

import (
	"os"
	"time"
)

// LockWait is Lock for a lock that is only ever held briefly, retrying until
// timeout instead of failing at once. Several processes that open the same
// local directory at the same moment each run setup; without waiting, all
// but one of them fail even though the next would have found the server
// already running. It must not be used for a lock held for a process's whole
// life, such as the server's own.
func LockWait(path string, timeout time.Duration) (*os.File, error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := Lock(path)
		if err == nil || time.Now().After(deadline) {
			return f, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}
