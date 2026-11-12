//go:build !windows

package statefile

import (
	"os"
	"syscall"
)

// openNoFollow opens path for reading without following a final symbolic link (O_NOFOLLOW) and without
// waiting (O_NONBLOCK: a named pipe opens at once; for a regular file the flag changes nothing).
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
