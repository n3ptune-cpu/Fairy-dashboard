package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"
)

// Follow prints entries appended to the log from now on (`fairy admin audit tail --follow`) until stop is
// closed (nil: forever). A line written in two parts is joined before it is decoded, a line longer than
// maxScanLine is skipped without holding it in memory, and when the file is rotated (or truncated) Follow
// finishes the old file and continues with the new one from its start.
func Follow(path string, show func(map[string]any), stop <-chan struct{}, every time.Duration) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { f.Close() }()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	r := bufio.NewReaderSize(f, block)
	var partial []byte
	skipping, draining := false, false
	for {
		chunk, err := r.ReadSlice('\n')
		switch {
		case err == nil: // a whole line (or the end of one)
			if !skipping {
				line := append(partial, chunk...)
				var e map[string]any
				if json.Unmarshal(line, &e) == nil && e != nil {
					show(e)
				}
			}
			partial, skipping = nil, false
			continue
		case errors.Is(err, bufio.ErrBufferFull) || (err == io.EOF && len(chunk) > 0):
			if !skipping {
				partial = append(partial, chunk...)
				if len(partial) > maxScanLine {
					partial, skipping = nil, true
				}
			}
			if err != io.EOF {
				continue
			}
		case err != io.EOF:
			return err
		}
		// at the end of the file: was it rotated or truncated? Read the old file to its end once more first
		// (a line may have landed just before the rotation), then go on with the new one.
		if replaced(f, path) {
			if !draining {
				draining = true
				continue
			}
			if nf, err := os.Open(path); err == nil {
				f.Close()
				f, r, partial, skipping, draining = nf, bufio.NewReaderSize(nf, block), nil, false, false
				continue
			}
		}
		select {
		case <-stop:
			return nil
		case <-time.After(every):
		}
	}
}

// replaced reports whether path is now another file than f, or f was truncated below the read position.
func replaced(f *os.File, path string) bool {
	cur, err := f.Stat()
	if err != nil {
		return false
	}
	now, err := os.Stat(path)
	if err != nil {
		return false
	}
	off, _ := f.Seek(0, io.SeekCurrent)
	return !os.SameFile(cur, now) || cur.Size() < off
}
