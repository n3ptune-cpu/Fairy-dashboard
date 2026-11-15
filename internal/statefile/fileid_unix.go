//go:build !linux && !windows

package statefile

import (
	"io/fs"
	"syscall"
)

// fileIDOf: the device and the number of the file. (The time its entry changed has another name in each of
// these systems; Fairy is released for Linux, fileid_linux.go, which has it.)
func fileIDOf(st fs.FileInfo) fileID {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}
	}
	return fileID{dev: uint64(s.Dev), ino: uint64(s.Ino)}
}
