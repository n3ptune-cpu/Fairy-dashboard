package statefile

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// kept lists the kept copies of name in dir, oldest first.
func kept(t *testing.T, dir, name string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, name+".corrupt-*"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(m)
	return m
}

// Audit 5, round 3 (r3-p4-09, r3-p2-10): a damaged file is kept under a name with the time in it, never over
// a copy that is already there, and at most five copies of one file stay.
func TestKeepAsideNeverWritesOverAnOlderCopy(t *testing.T) {
	now := time.Date(2026, 11, 12, 14, 5, 1, 0, time.FixedZone("CET", 3600))
	asideNow = func() time.Time { return now }
	t.Cleanup(func() { asideNow = time.Now })
	dir := t.TempDir()
	p := filepath.Join(dir, "sessions.json")
	damage := func(content string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	damage("FIRST damaged copy")
	first, err := KeepAside(p)
	if err != nil || first != p+".corrupt-20261112T130501Z" { // UTC, whatever the machine's zone is
		t.Fatalf("the first copy: %q %v", first, err)
	}
	if !regexp.MustCompile(`\.corrupt-\d{8}T\d{6}Z$`).MatchString(first) {
		t.Fatalf("the name: %s", first)
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatal("the damaged file is still in place")
	}
	// a second damaged file in the same second: another name, and the first copy is as it was
	damage("second damaged copy")
	second, err := KeepAside(p)
	if err != nil || second != first+"-2" {
		t.Fatalf("the second copy: %q %v", second, err)
	}
	if b, _ := os.ReadFile(first); string(b) != "FIRST damaged copy" {
		t.Fatalf("the first kept copy now holds %q", b)
	}
	if b, _ := os.ReadFile(second); string(b) != "second damaged copy" {
		t.Fatalf("the second kept copy holds %q", b)
	}
	// a name that is taken by a folder, or by a link, is left alone too
	now = now.Add(time.Second)
	stamp := p + ".corrupt-20261112T130502Z"
	if err := os.Mkdir(stamp, 0o700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(dir, "nowhere"), stamp+"-2"); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(stamp+"-2", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	damage("third")
	third, err := KeepAside(p)
	if err != nil || third != stamp+"-3" {
		t.Fatalf("with the time's own name taken by a folder and -2 by a link: %q %v", third, err)
	}
	if st, err := os.Lstat(stamp); err != nil || !st.IsDir() {
		t.Fatal("the folder was touched")
	}
	// every name of the second taken: an error, and the damaged file stays where it is
	now = now.Add(time.Second)
	stamp = p + ".corrupt-20261112T130503Z"
	for i := 1; i <= asideTries; i++ {
		name := stamp
		if i > 1 {
			name = stamp + "-" + string(rune('0'+i))
		}
		if err := os.Mkdir(name, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	damage("fourth")
	if got, err := KeepAside(p); err == nil || !strings.Contains(err.Error(), "is taken") {
		t.Fatalf("with every name taken: %q %v", got, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "fourth" {
		t.Fatalf("after a move that failed the file holds %q", b)
	}
	// a file that is not there
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := KeepAside(p); err == nil {
		t.Fatal("moving a file that does not exist")
	}
}

func TestKeepAsideKeepsTheFiveNewestCopies(t *testing.T) {
	now := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	asideNow = func() time.Time { return now }
	t.Cleanup(func() { asideNow = time.Now })
	dir := t.TempDir()
	p := filepath.Join(dir, "tokens.json")
	other := filepath.Join(dir, "sessions.json.corrupt-20200101T000000Z") // another file's copy, and an old-style one
	old := p + ".corrupt"
	for _, f := range []string{other, old} {
		if err := os.WriteFile(f, []byte("not mine"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var names []string
	for i := 1; i <= 8; i++ {
		if err := os.WriteFile(p, []byte{byte('0' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
		name, err := KeepAside(p)
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
		now = now.Add(time.Hour)
		if got, want := len(kept(t, dir, "tokens.json")), min(i, KeptCopies); got != want {
			t.Fatalf("after %d damaged files: %d kept copies, want %d", i, got, want)
		}
	}
	got := kept(t, dir, "tokens.json")
	if strings.Join(got, "\n") != strings.Join(names[3:], "\n") {
		t.Fatalf("kept: %v, want the five newest: %v", got, names[3:])
	}
	for i, name := range got {
		if b, _ := os.ReadFile(name); len(b) != 1 || int(b[0]-'0') != i+4 {
			t.Fatalf("%s holds %q", name, b)
		}
	}
	for _, f := range []string{other, old} {
		if b, _ := os.ReadFile(f); string(b) != "not mine" {
			t.Fatalf("%s was touched", f)
		}
	}
}

// A symbolic link is moved as the link it is: what it points to is neither read nor changed.
func TestKeepAsideMovesALinkNotItsTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need a privilege on Windows")
	}
	dir := t.TempDir()
	target, p := filepath.Join(dir, "elsewhere"), filepath.Join(dir, "settings.json")
	if err := os.WriteFile(target, []byte("kept as it is"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, p); err != nil {
		t.Fatal(err)
	}
	aside, err := KeepAside(p)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(aside); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the kept copy is not the link: %v %v", st, err)
	}
	if b, _ := os.ReadFile(target); string(b) != "kept as it is" {
		t.Fatalf("the link's target holds %q", b)
	}
}

// Second review of fix7a (R7A-06): the kept copies were put in order by the time in their names, and the
// "oldest" beyond five were removed right after a new one was made. When the panel host's clock is behind the
// copies that are already there (a clock that was set back; a flat clock battery at the start after a power
// cut, which is when files are found damaged), the copy that was just made is the "oldest": KeepAside removed
// it at once, returned its name, and the caller logged "the damaged file is kept as <that name>". Nothing was
// left of the file. The reviewer's proof.
func TestKeepAsideNeverRemovesTheCopyItJustMade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	now := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	asideNow = func() time.Time { return now }
	t.Cleanup(func() { asideNow = time.Now })
	var older []string
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(path, []byte("damaged "+string(rune('a'+i))), 0o600); err != nil {
			t.Fatal(err)
		}
		name, err := KeepAside(path)
		if err != nil {
			t.Fatal(err)
		}
		// written an hour apart, as their names say
		if err := os.Chtimes(name, now, now); err != nil {
			t.Fatal(err)
		}
		older = append(older, name)
		now = now.Add(time.Hour)
	}
	for round := 1; round <= 3; round++ { // the clock is behind, at one start after the other
		now = time.Date(2020, 1, 1, 0, 0, 5*round, 0, time.UTC)
		content := "everybody's settings, cut off " + string(rune('0'+round))
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		aside, err := KeepAside(path)
		if err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(aside); err != nil || string(b) != content {
			t.Fatalf("round %d: KeepAside says the file is kept as %s, and that copy holds %q: %v", round, filepath.Base(aside), b, err)
		}
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("round %d: the file is still in place", round)
		}
		got := kept(t, dir, "settings.json")
		if len(got) != KeptCopies {
			t.Fatalf("round %d: %d kept copies, want %d: %v", round, len(got), KeptCopies, got)
		}
		// what went is the copy that was written first, not the one with the "oldest" name
		for _, gone := range older[:round] {
			if _, err := os.Stat(gone); err == nil {
				t.Fatalf("round %d: %s, the copy that was written first, is still there: %v", round, filepath.Base(gone), got)
			}
		}
		for _, there := range older[round:] {
			if _, err := os.Stat(there); err != nil {
				t.Fatalf("round %d: %s was removed: %v", round, filepath.Base(there), got)
			}
		}
	}
}

// Only names that KeepAside itself gives are counted and removed. A file somebody made by hand that merely
// begins like a kept copy (settings.json.corrupt-keep, a note, a copy of a copy) is not the panel's to remove.
func TestKeepAsideLeavesFilesAlonethatOnlyLookLikeItsCopies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	now := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	asideNow = func() time.Time { return now }
	t.Cleanup(func() { asideNow = time.Now })
	long := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC) // older than every copy, by the file's time and by the name
	var mine []string
	for _, name := range []string{"settings.json.corrupt-anything", "settings.json.corrupt-keep-this", "settings.json.corrupt-20190101T000000Z.bak",
		"settings.json.corrupt-20190101T000000Z-10", "settings.json.corrupt-20190101T000000Z-1", "settings.json.corrupt-2019", "settings.json.corrupt-",
		"settings.json.corrupt-20190101t000000z", "settings.json.corrupt-20190101T000000Z copy"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("the owner's"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, long, long); err != nil {
			t.Fatal(err)
		}
		mine = append(mine, p)
	}
	for i := 0; i < 12; i++ { // twice in one second too: the names with -2
		if err := os.WriteFile(path, []byte{byte('a' + i)}, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := KeepAside(path); err != nil {
			t.Fatal(err)
		}
		if i%2 == 1 {
			now = now.Add(time.Hour)
		}
	}
	for _, p := range mine {
		if b, err := os.ReadFile(p); err != nil || string(b) != "the owner's" {
			t.Errorf("%s was removed or changed: %q %v", filepath.Base(p), b, err)
		}
	}
	exact := regexp.MustCompile(`^settings\.json\.corrupt-\d{8}T\d{6}Z(-[2-9])?$`)
	n := 0
	for _, p := range kept(t, dir, "settings.json") {
		if exact.MatchString(filepath.Base(p)) {
			n++
		}
	}
	if n != KeptCopies {
		t.Errorf("%d kept copies of the panel's own, want %d: %v", n, KeptCopies, kept(t, dir, "settings.json"))
	}
}
