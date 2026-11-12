package statefile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// MaxBytes is the most a state file may hold to be read at all. Every state file of the panel is far below it
// (state.json with every setting used is some kilobytes, the run journal is capped at one megabyte by its own
// reader); a file larger than this was not written by Fairy.
const MaxBytes = 64 << 20

// NotAFileError is a state file (or a picture) in the data folder that is not a plain file of a sane size: a
// symbolic link, a named pipe, a device, a folder, or more bytes than its reader takes. Fairy does not read
// it: a link to /dev/zero read to its end takes all memory, and a pipe that nobody writes to is waited on for
// ever (r2-p2-10).
type NotAFileError struct {
	Path string
	What string // "a symbolic link", "a named pipe", "larger than ... bytes", ...
}

func (e *NotAFileError) Error() string {
	return fmt.Sprintf("%s is %s, not a plain file Fairy wrote: Fairy does not read it; look at it, then move it away", e.Path, e.What)
}

// what names a file that is not a regular one.
func what(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return "a symbolic link"
	case m&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case m&fs.ModeDevice != 0, m&fs.ModeCharDevice != 0:
		return "a device"
	case m&fs.ModeSocket != 0:
		return "a socket"
	case m.IsDir():
		return "a folder"
	}
	return "not a regular file"
}

// OpenRegular opens path for reading if it is a regular file, and only then. The last part of the path is not
// followed when it is a symbolic link, and the open never waits (a named pipe without a writer would block it
// for ever). A missing file is the usual error (fs.ErrNotExist); anything that is not a regular file is a
// *NotAFileError. The caller closes the file and bounds what it reads from it (ReadFile does both).
func OpenRegular(path string) (*os.File, error) {
	f, err := openNoFollow(path)
	if err != nil {
		if st, lerr := os.Lstat(path); lerr == nil && !st.Mode().IsRegular() { // the open refused it: say what it is
			return nil, &NotAFileError{Path: path, What: what(st.Mode())}
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, &NotAFileError{Path: path, What: what(st.Mode())}
	}
	return f, nil
}

// ReadFile reads a regular file of at most max bytes (OpenRegular). A larger file is a *NotAFileError and is
// not read to its end: at most max+1 bytes are ever taken into memory.
func ReadFile(path string, max int64) ([]byte, error) {
	f, err := OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, &NotAFileError{Path: path, What: fmt.Sprintf("larger than %d bytes", max)}
	}
	return b, nil
}

// IsNotAFile reports whether err is a *NotAFileError.
func IsNotAFile(err error) bool {
	var e *NotAFileError
	return errors.As(err, &e)
}
