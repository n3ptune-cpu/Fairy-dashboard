// Package statefile reads and writes the small JSON state files shared by the server and the admin CLI
// (state.json, sessions.json, tokens.json, users.json).
//
// Writes take an exclusive lock on <file>.lock, re-read, modify and atomically replace the file (mode 0600), so
// the CLI and the running server never lose each other's changes. Readers reload when the file's mtime changes.
//
// The files fail closed: a missing file is empty, but a file that cannot be read (wrong owner or permissions),
// is not valid JSON, or EXISTS AND IS EMPTY is an error. It is never treated as empty and never overwritten,
// and a file (or the data directory) owned by another user is not written at all, so running the CLI as the
// wrong user (e.g. root) cannot leave files the service can no longer read.
//
// WriteAtomic is the one way a file is replaced, here and for every other file the panel keeps (settings,
// notifications, managed hosts and their secrets, pictures, the update's status): a temporary file next to
// it, written and synced to the disk, then renamed over it, then the folder synced. Without the sync a power
// cut shortly after the rename can leave the new name with 0 bytes.
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
	"runtime"
	"sync"
	"time"
)

// Data is a state file's top-level JSON object.
type Data = map[string]any

// Read returns the file's top-level object: {} when the file does not exist, an error when it cannot be read,
// is empty or is not a JSON object. Callers must not treat an error as "empty".
//
// A file that exists with nothing in it is damaged, not new: Fairy never writes one (a state file holds at
// least "{}"), and a power cut or a full disk can leave one behind. Read as "no state" it would switch the
// kill switch off, forget who was removed and make the next sign-in the first admin again (r2-p4-10).
func Read(path string) (Data, error) {
	b, err := ReadFile(path, MaxBytes) // a regular file of a sane size, or it is not read (safe.go)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Data{}, nil
		}
		if IsNotAFile(err) {
			return nil, err
		}
		return nil, fmt.Errorf("cannot read %s: %w (it must be readable by the user Fairy runs as; "+
			"check its owner and mode, e.g. chown fairy:fairy and chmod 600)", path, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, &DamagedError{Path: path, What: fmt.Sprintf("is empty (%d bytes): Fairy never writes an empty state file, so this one is damaged "+
			"(a power cut or a full disk can leave it).", len(b))}
	}
	var d Data
	if err := json.Unmarshal(b, &d); err != nil || d == nil {
		if err == nil {
			err = errors.New("not a JSON object")
		}
		return nil, &DamagedError{Path: path, What: fmt.Sprintf("is corrupt (%v):", err)}
	}
	return d, nil
}

// DamagedError is a state file that could be read and does not hold a state: it is empty, or not a JSON object
// (cut off, or other content). It is not the error of a file that cannot be read at all, is not a plain file
// or belongs to another user.
type DamagedError struct {
	Path string
	What string // "is empty (0 bytes): ... ." or "is corrupt (...):"
}

func (e *DamagedError) Error() string {
	// The way out, said with what it costs. For the list of users "move it aside" is not a harmless fresh
	// start: without the file nobody has a role and nobody counts as removed, and the first person who signs
	// in is the first admin again.
	remedy := "restore it from a backup, or move it aside to start that file over"
	if filepath.Base(e.Path) == "users.json" {
		remedy = "restore it from a backup; moving it aside starts with no users, and the first person to sign in becomes admin"
	}
	return e.Path + " " + e.What + " Fairy will not use or overwrite it; " + remedy
}

// IsDamaged reports whether err is a *DamagedError.
func IsDamaged(err error) bool {
	var d *DamagedError
	return errors.As(err, &d)
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
	if err := WriteAtomic(path, b, 0o600); err != nil {
		return nil, err
	}
	return d, nil
}

// WriteAtomic replaces path with data (mode perm) so that the name holds either the old content or all of the
// new one, also after a power cut: the data goes to a new temporary file in the same folder, that file is
// synced to the disk, renamed over path, and the folder is synced so the rename itself is on the disk. The
// folder must exist. On any error the old file is untouched and the temporary one is removed.
//
// The folder's sync is best effort (Windows and some file systems cannot do it, and by then the file is
// replaced): without it a power cut can bring the OLD content back, never an empty file.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Chmod(perm); err != nil {
		return fail(err)
	}
	if err := fsync(f); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(dir)
	return nil
}

// fsync writes a file (or a folder) through to the disk. A variable so that a test can see the order of the
// calls; nothing else replaces it.
var fsync = func(f *os.File) error { return f.Sync() }

// syncDir asks the system to write a folder's entries to the disk (after a rename in it).
func syncDir(dir string) {
	if runtime.GOOS == "windows" { // a folder cannot be synced there
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = fsync(d)
		d.Close()
	}
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
	default:
		// "Does not exist" is what os.Stat also says of a symbolic link whose target is away (a share that is
		// not mounted, a file that is being moved). That is not a fresh start: something has this name, and
		// what the file said (the kill switch, the per-host switches, the revoke-all time) is only out of reach.
		// It read as "no state": control on, power control on for every host, every old session valid again
		// (audit 6, r4-p2-04). Only a name that is not there at all is "missing".
		if _, lerr := os.Lstat(w.Path); lerr == nil {
			mt, statErr = -3, errors.New("it is a symbolic link whose target does not exist (the panel reads its state files only as plain files)")
		} else if !errors.Is(lerr, fs.ErrNotExist) {
			mt, statErr = -3, lerr
		}
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
