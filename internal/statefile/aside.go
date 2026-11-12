package statefile

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
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

// KeepAside moves a damaged file out of the way so that it is kept and the panel can start that file over: to
// <path>.corrupt-<UTC time> (yyyymmddThhmmssZ) in the same folder. It returns the new name.
//
// It never writes over anything: a name that is taken, by a file, a folder or a link, is left alone and the
// next one is tried (the time with -2, -3, ... -9). Before, the copy was <path>.corrupt: a second damaged file
// replaced the first kept copy without a word, and a folder of that name kept the panel down (audit 5,
// r3-p4-09, r3-p2-10). At most KeptCopies copies of one file are kept: the oldest are removed when a new one
// was made, so a file that is damaged at every start cannot fill the folder.
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
	pruneAside(path)
	return aside, nil
}

// pruneAside removes the oldest kept copies of path beyond KeptCopies. Only plain files with the name
// KeepAside gives are counted and removed; the time in the name puts them in order.
func pruneAside(path string) {
	dir, prefix := filepath.Dir(path), filepath.Base(path)+".corrupt-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for ; len(names) > KeptCopies; names = names[1:] {
		old := filepath.Join(dir, names[0])
		if err := os.Remove(old); err != nil {
			slog.Warn("an old copy of a damaged file could not be removed", "path", old, "error", err)
			continue
		}
		slog.Info(fmt.Sprintf("the oldest kept copy of a damaged file was removed (the %d newest are kept)", KeptCopies), "path", old)
	}
}
