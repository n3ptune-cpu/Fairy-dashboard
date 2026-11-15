//go:build linux

package statefile

import (
	"io/fs"
	"syscall"
)

// fileIDOf: the device, the number of the file and the time its entry changed.
func fileIDOf(st fs.FileInfo) fileID {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}
	}
	return fileID{dev: uint64(s.Dev), ino: uint64(s.Ino), ctime: s.Ctim.Nano()}
}
