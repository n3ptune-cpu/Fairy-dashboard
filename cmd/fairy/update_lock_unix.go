//go:build !windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// lockUpdate takes the one lock of `fairy update` on this host: an exclusive flock on path, without waiting.
// It is held until the returned func is called or the process ends (the system lets go of it then, also
// after a crash), so the file itself is never removed. errUpdateRunning: another run holds it.
func lockUpdate(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errUpdateRunning
		}
		return nil, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, nil
}
