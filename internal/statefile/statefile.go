// Package statefile reads and writes the small JSON state files shared by the server and the admin CLI
// (state.json, sessions.json, tokens.json, users.json).
//
// Writes take an exclusive lock on <file>.lock, re-read, modify and atomically replace the file (mode 0600), so
// the CLI and the running server never lose each other's changes. Readers reload when the file's mtime changes.
//
// The files fail closed: a missing file is empty, but a file that cannot be read (wrong owner or permissions) or
// is not valid JSON is an error. It is never treated as empty and never overwritten, and a file (or the data
// directory) owned by another user is not written at all, so running the CLI as the wrong user (e.g. root)
// cannot leave files the service can no longer read.
package statefile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Data is a state file's top-level JSON object.
type Data = map[string]any

// Read returns the file's top-level object: {} when the file does not exist (or is empty), an error when it
// cannot be read or is not a JSON object. Callers must not treat an error as "empty".
func Read(path string) (Data, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Data{}, nil
		}
		return nil, fmt.Errorf("cannot read %s: %w (it must be readable by the user Fairy runs as; "+
			"check its owner and mode, e.g. chown fairy:fairy and chmod 600)", path, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return Data{}, nil
	}
	var d Data
	if err := json.Unmarshal(b, &d); err != nil || d == nil {
		if err == nil {
			err = errors.New("not a JSON object")
		}
		return nil, fmt.Errorf("%s is corrupt (%v): Fairy will not use or overwrite it; restore it from a backup, "+
			"or move it aside to start that file over", path, err)
	}
	return d, nil
}

// Check reports whether a state file is usable: readable, valid and writable by this process (it and its
// directory belong to the current user). A missing file is fine.
func Check(path string) error {
	if _, err := Read(path); err != nil {
		return err
	}
	return checkOwner(path)
}

// CheckOwner reports an OwnerError when path (if it exists) or its directory belongs to another user than the
// one running this process (always nil on Windows).
func CheckOwner(path string) error { return checkOwner(path) }

// Update is a locked read-modify-write. fn mutates the map in place; returning an error aborts the write. It
// refuses (and leaves the file alone) when the file is unreadable, corrupt or owned by another user.
func Update(path string, fn func(Data) error) (Data, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := checkOwner(path); err != nil {
		return nil, err
	}
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	d, err := Read(path)
	if err != nil {
		return nil, err
	}
	if err := fn(d); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return nil, err
	}
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d.tmp", filepath.Base(path), os.Getpid()))
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	return d, nil
}

// Watched is a cached view of a state file, reloaded when its mtime changes (checked at most every `every`).
// When the file becomes unreadable or corrupt the view is empty and Err reports why (fail closed); the problem
// is logged once per change of the file.
type Watched struct {
	Path    string
	every   time.Duration
	mu      sync.Mutex
	data    Data
	err     error
	mtime   int64
	checked time.Time
	Version int
}

// NewWatched returns a cached view that checks the file at most once a second.
func NewWatched(path string) *Watched { return NewWatchedEvery(path, time.Second) }

// NewWatchedEvery returns a cached view that checks the file at most every `every`; every=0 stats it on every
// Get (used for API tokens, so a revoke applies at once).
func NewWatchedEvery(path string, every time.Duration) *Watched {
	return &Watched{Path: path, every: every, data: Data{}, mtime: -1}
}

// Get returns the cached data, reloading it if the file changed ({} while the file is unreadable: see Err).
func (w *Watched) Get() Data {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.getLocked(false)
}

// Refresh checks the file now (after this process wrote it).
func (w *Watched) Refresh() Data {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.getLocked(true)
}

// Err is why the file could not be used at the last check (nil when it is fine or missing).
func (w *Watched) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.getLocked(false)
	return w.err
}

func (w *Watched) getLocked(force bool) Data {
	now := time.Now()
	if !force && now.Sub(w.checked) < w.every {
		return w.data
	}
	w.checked = now
	var mt int64 = -2
	st, statErr := os.Stat(w.Path)
	switch {
	case statErr == nil:
		mt = st.ModTime().UnixNano() ^ st.Size()<<1
	case !errors.Is(statErr, fs.ErrNotExist):
		mt = -3 // e.g. the directory is not searchable: an error, not "missing"
	}
	if mt != w.mtime || (w.err != nil && mt == -3) {
		w.mtime = mt
		w.Version++
		var err error
		switch mt {
		case -2:
			w.data, err = Data{}, nil
		case -3:
			w.data, err = Data{}, fmt.Errorf("cannot read %s: %w", w.Path, statErr)
		default:
			w.data, err = Read(w.Path)
			if err != nil {
				w.data = Data{}
			}
		}
		if err != nil && (w.err == nil || w.err.Error() != err.Error()) {
			slog.Error("state file unusable: failing closed until it is fixed", "path", w.Path, "error", err)
		}
		w.err = err
	}
	return w.data
}

// OwnerError is a state file or data directory owned by another user than the one running Fairy.
type OwnerError struct {
	Path      string
	Owner, Me int
}

func (e *OwnerError) Error() string {
	return fmt.Sprintf("%s belongs to uid %d but this process runs as uid %d: refusing to change it, so the panel "+
		"can still read it. Run the command as the service user (e.g. sudo -u fairy fairy admin ...), or give "+
		"it back to that user (e.g. chown fairy:fairy %s)", e.Path, e.Owner, e.Me, e.Path)
}
