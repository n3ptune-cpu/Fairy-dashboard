//go:build windows

package statefile

import (
	"os"

	"golang.org/x/sys/windows"
)

func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	ol := new(windows.Overlapped)
	h := windows.Handle(f.Fd())
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = windows.UnlockFileEx(h, 0, 1, 0, ol); f.Close() }, nil
}
