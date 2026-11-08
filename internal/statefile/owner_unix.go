//go:build !windows

package statefile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// checkOwner refuses to write a state file (or into a data directory) that belongs to another user: a file
// rewritten by e.g. root would become unreadable for the service.
func checkOwner(path string) error {
	me := os.Geteuid()
	for _, p := range []string{filepath.Dir(path), path} {
		st, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if s, ok := st.Sys().(*syscall.Stat_t); ok && int(s.Uid) != me {
			return &OwnerError{Path: p, Owner: int(s.Uid), Me: me}
		}
	}
	return nil
}
