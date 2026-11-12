package statefile

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// KeptCopies is how many copies of one damaged file are kept next to it (KeepAside); older ones are removed.
const KeptCopies = 5

// asideStamp is the time in the name of a kept copy: UTC, as in sessions.json.corrupt-20261112T140501Z.
const asideStamp = "20060102T150405Z"

// asideTries is how many names KeepAside tries for one second: the time alone, then the time with -2 to -9.
const asideTries = 9

// asideNow is the clock of KeepAside (a test's can stand still).
var asideNow = time.Now

// asideName matches the part of a kept copy's name that follows "<name>.corrupt-", exactly as KeepAside writes
// it: the time (asideStamp), alone or with -2 to -9.
var asideName = regexp.MustCompile(`^\d{8}T\d{6}Z(-[2-9])?$`)

// KeepAside moves a damaged file out of the way so that it is kept and the panel can start that file over: to
// <path>.corrupt-<UTC time> (yyyymmddThhmmssZ) in the same folder. It returns the new name.
//
// It never writes over anything: a name that is taken, by a file, a folder or a link, is left alone and the
// next one is tried (the time with -2, -3, ... -9). Before, the copy was <path>.corrupt: a second damaged file
// replaced the first kept copy without a word, and a folder of that name kept the panel down (audit 5,
// r3-p4-09, r3-p2-10). At most KeptCopies copies of one file are kept: the oldest are removed when a new one
// was made, so a file that is damaged at every start cannot fill the folder. The copy that was just made is
// never one of those (pruneAside).
//
// The move is a rename: a symbolic link is moved as the link it is, never followed.
func KeepAside(path string) (string, error) {
	base := path + ".corrupt-" + asideNow().UTC().Format(asideStamp)
	aside, free := base, false
	for i := 2; i <= asideTries+1; i++ {
		if _, err := os.Lstat(aside); errors.Is(err, fs.ErrNotExist) {
			free = true
			break
		}
		aside = fmt.Sprintf("%s-%d", base, i)
	}
	if !free {
		return "", fmt.Errorf("every name from %s to %s-%d is taken", base, base, asideTries)
	}
	if err := os.Rename(path, aside); err != nil {
		return "", err
	}
	pruneAside(path, aside)
	return aside, nil
}

// pruneAside removes the oldest kept copies of path beyond KeptCopies, after the copy `made` was made.
//
// The copy that was just made always stays: it is the one the caller is about to name in the log as "kept".
// The others are put in order by when they were last written (the file's own time, which the move keeps), and
// by their names where that is the same. They used to be put in order by the time in their names alone, the
// new one with them: with a clock that is behind the copies already there (set back, or a flat clock battery
// at the start after a power cut, which is when files are found damaged) the new copy was "the oldest" and was
// removed in the same call that made it (second review of fix7a, R7A-06).
//
// Only plain files with exactly the name KeepAside gives are counted and removed (asideName). A file somebody
// made by hand that only begins like one (<name>.corrupt-keep, <name>.corrupt-<time>.bak) is left alone.
func pruneAside(path, made string) {
	dir, prefix := filepath.Dir(path), filepath.Base(path)+".corrupt-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type copyOf struct {
		name    string
		written time.Time
	}
	var older []copyOf
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !asideName.MatchString(name[len(prefix):]) || !e.Type().IsRegular() || name == filepath.Base(made) {
			continue
		}
		info, err := e.Info()
		if err != nil { // gone meanwhile
			continue
		}
		older = append(older, copyOf{name, info.ModTime()})
	}
	sort.Slice(older, func(i, j int) bool {
		if !older[i].written.Equal(older[j].written) {
			return older[i].written.Before(older[j].written)
		}
		return older[i].name < older[j].name
	})
	for ; len(older) > KeptCopies-1; older = older[1:] { // the one just made is the first of the KeptCopies
		old := filepath.Join(dir, older[0].name)
		if err := os.Remove(old); err != nil {
			slog.Warn("an old copy of a damaged file could not be removed", "path", old, "error", err)
			continue
		}
		slog.Info(fmt.Sprintf("the oldest kept copy of a damaged file was removed (the %d newest are kept)", KeptCopies), "path", old)
	}
}
