//go:build windows

package audit

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on f (released by the returned func).
func lockFile(f *os.File) func() {
	ol := new(windows.Overlapped)
	h := windows.Handle(f.Fd())
	if windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol) != nil {
		return func() {}
	}
	return func() { _ = windows.UnlockFileEx(h, 0, 1, 0, ol) }
}
