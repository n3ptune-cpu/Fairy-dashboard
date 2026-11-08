//go:build !windows

package audit

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive lock on f (released by the returned func).
func lockFile(f *os.File) func() {
	if unix.Flock(int(f.Fd()), unix.LOCK_EX) != nil {
		return func() {}
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
}
