// Package statefile reads and writes the small JSON state files shared by the server and the admin CLI
// (state.json, sessions.json, tokens.json, users.json).
//
// Writes take an exclusive lock on <file>.lock, re-read, modify and atomically replace the file (mode 0600), so
// the CLI and the running server never lose each other's changes. Readers reload when the file is another one
// than at their last look (Watched), and always after a write of their own.
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
	"crypto/sha256"
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
	b, found, err := content(path)
	if err != nil {
		return nil, err
	}
	if !found {
		return Data{}, nil
	}
	return parse(path, b)
}

// content is the bytes of a state file. found is false, without an error, when the file does not exist.
func content(path string) (b []byte, found bool, err error) {
	b, err = ReadFile(path, MaxBytes) // a regular file of a sane size, or it is not read (safe.go)
	switch {
	case err == nil:
		return b, true, nil
	case errors.Is(err, fs.ErrNotExist):
		return nil, false, nil
	case IsNotAFile(err):
		return nil, false, err
	}
	return nil, false, fmt.Errorf("cannot read %s: %w (it must be readable by the user Fairy runs as; "+
		"check its owner and mode, e.g. chown fairy:fairy and chmod 600)", path, err)
}

// parse is the state in the bytes of a state file, or why they hold none (a *DamagedError).
func parse(path string, b []byte) (Data, error) {
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

// Watched is a cached view of a state file. It looks at the file at most every `every` (Get), or now (Look),
// and reads it again when the look finds another file than the last one did (see stamp), and at every look
// for a short time after it first saw the file as it is (see YoungFor). After a write of this process the view
// is told to read the file, whatever a look at it says (Refresh).
//
// When the file becomes unreadable or corrupt the view is empty and Err reports why (fail closed); the problem
// is logged once per change of the file.
type Watched struct {
	Path    string
	every   time.Duration
	mu      sync.Mutex
	data    Data
	err     error
	seen    stamp             // what the last look at the file found
	since   time.Time         // when a look first found that
	settled bool              // the file was read YoungFor or more after `since`: its stamp alone says what it holds
	sum     [sha256.Size]byte // of the bytes the view was read from (zero: none were read)
	checked time.Time
	Version int // moves with every change of what the view holds; beside other requests read it with Ver or GetVer
	Reads   int // how often the view read the file (for tests: a look that reads when it need not is not seen otherwise)
}

// YoungFor is how long a view goes on reading a file at every look after it first saw the file as its stamp
// says it. During that time the stamp is not trusted, and after it (and one more read) it is.
//
// Why: a stamp cannot tell two files apart that were written in one step of the file's clock, have the same
// size and, when a third write got the number of the first file again, the same number (see stamp). Such
// writes can only happen while the file's clock still shows the time in the stamp. Once the view has seen a
// stamp for longer than one step of that clock, every later write carries a later time, the stamp changes
// with it, and a read made from then on is the last one that is needed.
//
// How long: the step is the timer tick on Linux (4 ms at 250 Hz; measured on the machine this was written on:
// 1987 of 1999 files written one after the other had the time of the one before on a ramfs, 635 of 999 on an
// ext3 with the two fsyncs of a write), and one second on the coarsest file systems Fairy's writes work on
// (ext3, ext4 with small inodes, older NFS). Two seconds are that second and a second of margin.
//
// It is counted on this process's own clock from the moment of the first look, never by comparing the file's
// time with the time of day. A file whose time is in the future or long past (a clock that was set, a restored
// backup, a file server with another clock) is young for exactly as long as any other, and no longer: what it
// costs is a read and a hash at each look in those two seconds, and one read after them.
//
// What it rests on: the file's clock goes forward, about as fast as this process's, in steps of at most
// YoungFor. A clock that is set back and reaches the time in the stamp a second time is not covered.
const YoungFor = 2 * time.Second

// timeNow is the time. A variable so that a test can let YoungFor pass without waiting; nothing else replaces it.
var timeNow = time.Now

// stamp is what one look at a state file finds without reading it. Two looks with the same stamp are taken as
// the same content, so everything that is cheap and moves with a write is in it.
//
// The time of the last change and the size alone are not enough (they were all of it up to build 16). A file's
// times come from a clock that moves with the timer tick (every 4 ms at 250 Hz: on Linux before 6.13 always,
// on a newer one still on some file systems), and with far larger steps on some file systems (a second, or
// two). Fairy's own writes of one file are often less than a millisecond apart and of the same length ("site-a"
// for "site-b", one version for the next, 900 for 600). The second of two such writes was not seen: the view
// kept the first for as long as nothing else wrote the file, and the panel showed, checked and decided from
// it. So the stamp also holds which file it is (fileID): every write puts a new file under the name
// (WriteAtomic), and the file of one write and of the write right before it are never the same one, because
// the older is still there when the newer is made.
//
// What a stamp alone cannot tell: two or more writes since the last look, all in one step of the clock, the
// last of the same size as the file that was looked at, and in a file whose number the file system gave out
// again (the file of the write before last is free by then); on Windows, where the stamp is the time and the
// size (fileid_windows.go), one such write. So a view does not go by the stamp alone while such a write is
// still possible (YoungFor), and never after a write of this process (Refresh reads).
type stamp struct {
	what        int8 // notLooked, missing, outOfReach or there
	mtime, size int64
	id          fileID
}

const (
	notLooked  int8 = iota // the view has not looked at the file yet
	missing                // nothing has the name: an empty state, and no error
	outOfReach             // something has the name and cannot be looked at: no state, and an error
	there                  // a file: its time, size and fileID say which
)

// fileID is what the system says of a file beyond its time and size, for a stamp: the device and the number of
// the file, which a file that took the name by a rename does not share with the one that had it, and the time
// its entry changed (ctime: it moves with every write, and with a chmod, chown or rename, and nothing sets it
// back). Each is zero where the system does not say it (fileIDOf, one per system).
type fileID struct {
	dev, ino uint64
	ctime    int64
}

// statFile looks at a file. A variable so that a test can hold still what a look says; nothing else replaces it.
var statFile = os.Stat

// look is the stamp of the file as it is now. The error is why a name that is there cannot be looked at.
func look(path string) (stamp, error) {
	st, err := statFile(path)
	switch {
	case err == nil:
		return stamp{what: there, mtime: st.ModTime().UnixNano(), size: st.Size(), id: fileIDOf(st)}, nil
	case !errors.Is(err, fs.ErrNotExist):
		return stamp{what: outOfReach}, err // e.g. the directory is not searchable: an error, not "missing"
	}
	// "Does not exist" is what os.Stat also says of a symbolic link whose target is away (a share that is not
	// mounted, a file that is being moved). That is not a fresh start: something has this name, and what the
	// file said (the kill switch, the per-host switches, the revoke-all time) is only out of reach. It read as
	// "no state": control on, power control on for every host, every old session valid again (audit 6,
	// r4-p2-04). Only a name that is not there at all is "missing".
	if _, lerr := os.Lstat(path); lerr == nil {
		return stamp{what: outOfReach}, errors.New("it is a symbolic link whose target does not exist (the panel reads its state files only as plain files)")
	} else if !errors.Is(lerr, fs.ErrNotExist) {
		return stamp{what: outOfReach}, lerr
	}
	return stamp{what: missing}, nil
}

// NewWatched returns a cached view that checks the file at most once a second.
func NewWatched(path string) *Watched { return NewWatchedEvery(path, time.Second) }

// NewWatchedEvery returns a cached view that checks the file at most every `every`; every=0 stats it on every
// Get (used for API tokens, so a revoke applies at once).
func NewWatchedEvery(path string, every time.Duration) *Watched {
	return &Watched{Path: path, every: every, data: Data{}}
}

// How a view is asked for its data.
const (
	lookInTime = iota // Get: a look at the file at most every `every`
	lookNow           // Look: a look now
	readNow           // Refresh: a look now, and the file is read whatever the look says
)

// Get returns the cached data, reloading it if the file changed ({} while the file is unreadable: see Err).
func (w *Watched) Get() Data {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.getLocked(lookInTime)
}

// Look is Get with a look at the file now, not at most every `every`: one stat, and the file is read only when
// it is another one than at the last look, or still young (YoungFor). For what must not be up to a second old
// when another process wrote it (the kill switch, a host's power switch, a session that was ended).
func (w *Watched) Look() Data {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.getLocked(lookNow)
}

// Refresh reads the file now. It is what this process calls after it wrote the file: what it wrote is in the
// view when Refresh returns, also when a look at the file cannot tell the new file from the one before (see
// stamp). The version moves only when what was read is something else than the view held.
func (w *Watched) Refresh() Data {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.getLocked(readNow)
}

// Ver is the view's version: a number that moves whenever what the view holds changes. It is the field Version,
// read under the view's lock: the field is written under it (getLocked), by every request that finds the file
// changed, so code that runs beside other requests asks Ver or GetVer and does not read the field.
func (w *Watched) Ver() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Version
}

// GetVer is Get with the version of the data it returns: the two are of one look at the file.
func (w *Watched) GetVer() (Data, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	d := w.getLocked(lookInTime)
	return d, w.Version
}

// Err is why the file could not be used at the last check (nil when it is fine or missing).
func (w *Watched) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.getLocked(lookInTime)
	return w.err
}

func (w *Watched) getLocked(how int) Data {
	now := timeNow()
	if how == lookInTime && now.Sub(w.checked) < w.every {
		return w.data
	}
	w.checked = now
	seen, lookErr := look(w.Path)
	other := seen != w.seen
	if other {
		w.since, w.settled = now, false
	}
	// The same stamp as at the last look is the same file, with three exceptions. A file that cannot be looked
	// at (outOfReach) has no time to tell one such state from the next, so it is judged again at every check.
	// After a write of this process (readNow) the file is read: its stamp can be the one of the file before.
	// And a file the view has not known for long enough can have been written again under the same stamp by
	// somebody who did not tell the view (YoungFor).
	if !other && !(seen.what == outOfReach && w.err != nil) && !(seen.what == there && (how == readNow || !w.settled)) {
		return w.data
	}
	var (
		data Data
		sum  [sha256.Size]byte
		err  error
		same bool // the bytes the view was read from: nothing to parse, nothing to change
	)
	switch seen.what {
	case missing:
		data = Data{}
	case outOfReach:
		data, err = Data{}, fmt.Errorf("cannot read %s: %w", w.Path, lookErr)
	default:
		w.Reads++
		b, found, rerr := content(w.Path)
		if rerr == nil && found {
			sum = sha256.Sum256(b)
		}
		switch {
		case rerr != nil:
			data, err = Data{}, rerr
		case !found: // gone between the look and the read: an empty state, as Read says
			data = Data{}
		case !other && sum == w.sum:
			same = true
		default:
			if data, err = parse(w.Path, b); err != nil {
				data = Data{}
			}
		}
		// A read this long after the stamp was first seen is the last one the stamp needs (YoungFor).
		if now.Sub(w.since) >= YoungFor {
			w.settled = true
		}
	}
	// The version moves only when what was found is another thing than before: the switches look at the file
	// at every request now (audit 6, r4-p2-03), and whoever follows Version would be told of a change ten
	// times a second for as long as the file stays out of reach, or with every write of this process that
	// changed nothing, or with every look at a young file. Under the same stamp, the same bytes are the same
	// thing, and so is the same error where no bytes were read; the view stays as it is.
	if same || (!other && sum == w.sum && (err == nil) == (w.err == nil) && (err == nil || err.Error() == w.err.Error())) {
		return w.data
	}
	w.seen, w.sum = seen, sum
	w.Version++
	if err != nil && (w.err == nil || w.err.Error() != err.Error()) {
		slog.Error("state file unusable: failing closed until it is fixed", "path", w.Path, "error", err)
	}
	w.data, w.err = data, err
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
