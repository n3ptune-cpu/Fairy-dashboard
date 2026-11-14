package power

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// What the review of the day history found in its file and its limits, each with the case that showed it.

// "A write that fails is said once in the log" was not so for the two failures that happen: a full disk and a
// folder that cannot be written. The write's error names its temporary file, and that name is new with every
// write; the log line was held back only while the error's whole text stayed the same, so it came every five
// minutes. It is the failure that is compared now, not the file's name.
func TestDayFileWriteThatFailsWithANewTemporaryNameIsStillSaidOnce(t *testing.T) {
	log := captureLog(t)
	path := dayPath(t)
	d := loaded(path, at(0, 0, 0))
	n, cause := 0, error(syscall.ENOSPC)
	d.writeFile = func(p string, _ []byte, _ os.FileMode) error {
		n++
		return &fs.PathError{Op: "write", Path: filepath.Join(filepath.Dir(p), fmt.Sprintf(".power-day.json.%d.tmp", 1000+n*7919)), Err: cause}
	}
	fail := func(periods int) {
		t.Helper()
		for i := 0; i < periods; i++ {
			rec(d, at(0, n*5, 0), 1)
			if d.Flush(at(0, n*5, 1), 5*time.Second) {
				t.Fatal("a write that fails counts as written")
			}
		}
	}
	said := func() int { return strings.Count(log.String(), "could not keep the last 24 hours") }
	fail(6)
	if said() != 1 {
		t.Fatalf("a full disk for 6 periods is in the log %d times, want once:\n%s", said(), log)
	}
	// the line names the folder's problem and one of the temporary names; another failure is said again
	if !strings.Contains(log.String(), "no space left on device") {
		t.Errorf("the line does not say why:\n%s", log)
	}
	cause = syscall.EROFS
	fail(3)
	if said() != 2 || !strings.Contains(log.String(), "read-only file system") {
		t.Errorf("another failure after the first: in the log %d times in all, want twice:\n%s", said(), log)
	}
	// the real thing: a folder nobody can write a file into, root neither (the kernel's own)
	if st, err := os.Stat("/sys/kernel"); runtime.GOOS != "linux" || err != nil || !st.IsDir() {
		return
	}
	log.Reset()
	e := loaded("/sys/kernel/"+DayFile, at(0, 0, 0))
	for i := 1; i <= 4; i++ {
		rec(e, at(0, i*5, 0), 1)
		if e.Flush(at(0, i*5, 1), 5*time.Second) {
			t.Skip("a file could be written into /sys/kernel here")
		}
	}
	if said() != 1 {
		t.Errorf("four failed writes into a folder that cannot be written are in the log %d times, want once:\n%s", said(), log)
	}
}

// A write that the panel was killed in the middle of left its temporary file in the data folder, and nothing
// ever took it away: 6 MB each time, for ever. The start removes them now, and nothing else: only plain files
// with the name a write of this file gives its temporary file.
func TestDayFileLeftoversOfAKilledWriteAreRemovedAtTheStart(t *testing.T) {
	log := captureLog(t)
	path := dayPath(t)
	dir := filepath.Dir(path)
	d := loaded(path, at(0, 0, 0))
	rec(d, at(0, 0, 0), 40)
	saved(t, d, at(0, 1, 0))
	put := func(name string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(`{"version":1`), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gone := []string{put(".power-day.json.1234567.tmp"), put(".power-day.json.tmp-of-an-older-build.tmp")}
	stay := []string{path, put(".notifications.json.1234567.tmp"), put(".power-day.json.1234567.tmp.keep"),
		put("power-day.json.1234567.tmp"), put(".power-day.jsonx.tmp"), put("power-day.json.bak")}
	folder := filepath.Join(dir, ".power-day.json.folder.tmp")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".power-day.json.link.tmp")
	if err := os.Symlink(filepath.Join(dir, "power-day.json.bak"), link); err != nil {
		t.Fatal(err)
	}
	stay = append(stay, folder, link)

	e := loaded(path, at(0, 2, 0))
	for _, p := range gone {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s is still there after the start (%v)", filepath.Base(p), err)
		}
	}
	for _, p := range stay {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s is not a leftover of a write of this file and was taken away: %v", filepath.Base(p), err)
		}
	}
	if n := strings.Count(log.String(), "removed the temporary file"); n != len(gone) {
		t.Errorf("the log names %d removed files, want %d:\n%s", n, len(gone), log)
	}
	if !e.Has("plug-a", at(0, 2, 0)) {
		t.Error("the day itself was not read")
	}
	// a start without a data folder, or with one that cannot be listed, is still a start
	loaded(filepath.Join(dir, "none", "deeper", DayFile), at(0, 2, 0))
}

// ---- room for a new source

// dayOf is one sample of each of the ids.
func dayOf(ids ...string) []DaySample {
	out := make([]DaySample, 0, len(ids))
	for _, id := range ids {
		out = append(out, DaySample{ID: id, W: 1})
	}
	return out
}

func listed(ids ...string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// names is n ids "<host>/<prefix>0" ...
func names(host, prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s/%s%02d", host, prefix, i)
	}
	return out
}

// After a start the panel lists only what has answered since: the plugs of a site that is away are in the
// store with their day (from the file) and on no list. They were the first to go when another host reported
// sources the store had no room for, seconds after the start. In the first dayEvictHold after the file was
// read, nothing that is not listed goes for a new source: the new one waits.
func TestDayOfASiteThatIsAwayIsNotTakenOutRightAfterAStart(t *testing.T) {
	path := dayPath(t)
	d := loaded(path, at(0, 0, 0))
	var others []string
	for i := 0; i < 9; i++ { // 63 sources of nine hosts that are there, and one of a site that is away
		others = append(others, names(fmt.Sprintf("pc-%c", 'a'+i), "plug-", 7)...)
	}
	for m := 0; m < 300; m++ {
		d.Record(at(0, m, 0), dayOf("site-b/plug-a"), nil)
	}
	d.Record(at(7, 59, 0), dayOf(others...), nil)
	saved(t, d, at(8, 0, 0))

	e := loaded(path, at(8, 0, 10)) // the start; site-b is still away
	if e.Len() != MaxDayDevices || !e.Has("site-b/plug-a", at(8, 0, 10)) {
		t.Fatalf("the rig: %d sources after the start", e.Len())
	}
	// another host reports sources nobody has seen before, new ones with every tick
	now := at(8, 0, 10)
	for round := 0; round < 12; round++ {
		now = at(8, 0, 15+5*round)
		ids := names("pc-new", fmt.Sprintf("r%d-", round), MaxRemoteSources)
		e.Record(now, dayOf(ids...), listed(ids...))
	}
	if !e.Has("site-b/plug-a", now) {
		t.Fatal("the day of a site that is away was taken out for another host's new sources a minute after the start")
	}
	for _, id := range others {
		if !e.Has(id, now) {
			t.Fatalf("%s lost its day to a new source right after the start", id)
		}
	}
	if e.Len() != MaxDayDevices {
		t.Errorf("%d sources, want the %d of before", e.Len(), MaxDayDevices)
	}
	// until the last second of the hold (a quarter of an hour); then the one that was silent longest goes for a
	// new source, as ever
	hold := at(8, 0, 10).Add(15 * time.Minute)
	e.Record(hold.Add(-time.Second), dayOf("pc-new/late"), listed(append(others, "pc-new/late")...))
	if !e.Has("site-b/plug-a", hold) || e.Has("pc-new/late", hold) {
		t.Fatal("something went a second before the hold was over")
	}
	e.Record(hold, dayOf("pc-new/late"), listed(append(others, "pc-new/late")...))
	if e.Has("site-b/plug-a", hold) || !e.Has("pc-new/late", hold) || e.Len() != MaxDayDevices {
		t.Errorf("after the hold a new source still has no room: %d sources, the new one has a day: %v", e.Len(), e.Has("pc-new/late", hold))
	}

	// a source whose day is over goes at once, also in the hold: nothing is lost with it
	path = dayPath(t)
	g := loaded(path, at(0, 0, 0))
	g.Record(at(0, 0, 0), dayOf("site-b/plug-a"), nil)
	g.Record(at(23, 50, 0), dayOf(others...), nil)
	saved(t, g, at(23, 55, 0))
	h := loaded(path, at(23, 55, 0))
	h.Record(at(23, 59, 0), dayOf("pc-new/late"), listed("pc-new/late"))
	if h.Has("pc-new/late", at(23, 59, 0)) || !h.Has("site-b/plug-a", at(23, 59, 0)) {
		t.Fatal("the rig: a minute before its day is over the source of the site that is away went")
	}
	h.Record(at(24, 0, 0), dayOf("pc-new/late"), listed("pc-new/late"))
	if !h.Has("pc-new/late", at(24, 0, 0)) || h.Len() != MaxDayDevices {
		t.Errorf("a source whose day is over did not make room in the hold (%d sources)", h.Len())
	}
}

// One host's sources never take more than MaxRemoteSources places, whatever it reports: a host that reports
// new ids all the time (a device list that is made anew with every start, a broken or hostile agent) takes its
// own older ones out, not the days of other hosts. Without that, sixteen new ids with every answer emptied the
// store of everything that was not listed in four answers.
func TestDayOneHostTakesNoMoreThanItsOwnPlaces(t *testing.T) {
	d := NewDayStore()
	away := names("site-b", "plug-", 12) // days of hosts that are away: on no list
	away = append(away, names("site-c", "plug-", 12)...)
	away = append(away, names("site-d", "plug-", 16)...)
	there := names("pc-a", "plug-", 12) // listed
	there = append(there, names("pc-b", "plug-", 12)...)
	d.Record(at(0, 0, 0), dayOf(away...), nil)
	d.Record(at(0, 1, 0), dayOf(there...), nil)
	if d.Len() != MaxDayDevices {
		t.Fatalf("the rig: %d sources", d.Len())
	}
	now := at(0, 1, 0)
	var last []string
	for round := 0; round < 40; round++ {
		now = at(0, 2, 5*round)
		last = names("pc-new", fmt.Sprintf("r%d-", round), MaxRemoteSources)
		d.Record(now, append(dayOf(there...), dayOf(last...)...), listed(append(there, last...)...))
	}
	kept := 0
	for _, id := range away {
		if d.Has(id, now) {
			kept++
		}
	}
	// the store was full: the host's first sixteen took sixteen places of what was not listed; every one after
	// that took the place of one of its own
	if want := len(away) - MaxRemoteSources; kept != want {
		t.Errorf("%d of %d days of hosts that are away are left after one host reported %d new ids, want %d", kept, len(away), 40*MaxRemoteSources, want)
	}
	for _, id := range last {
		if !d.Has(id, now) {
			t.Errorf("%s, which the host reports now, has no day", id)
		}
	}
	for _, id := range there {
		if !d.Has(id, now) {
			t.Errorf("%s is listed and lost its day", id)
		}
	}
	if d.Len() != MaxDayDevices {
		t.Errorf("%d sources", d.Len())
	}

	// the same with room in the store: sixteen places, and the host's own older ids go for its new ones
	e := NewDayStore()
	e.Record(at(0, 0, 0), dayOf("site-b/plug-a", "plug-a"), nil)
	for round := 0; round < 5; round++ {
		last = names("pc-new", fmt.Sprintf("r%d-", round), MaxRemoteSources)
		e.Record(at(0, 1, 5*round), dayOf(last...), listed(last...))
	}
	if e.Len() != 2+MaxRemoteSources || !e.Has("site-b/plug-a", at(0, 2, 0)) || !e.Has("plug-a", at(0, 2, 0)) || !e.Has(last[0], at(0, 2, 0)) {
		t.Errorf("with room in the store: %d sources, want %d", e.Len(), 2+MaxRemoteSources)
	}
	// ... but never one of its own that is listed, and none at all without a list
	seventeen := names("pc-full", "plug-", MaxRemoteSources+1)
	e.Record(at(0, 3, 0), dayOf(seventeen...), listed(seventeen...))
	if !e.Has(seventeen[0], at(0, 3, 0)) || e.Has(seventeen[MaxRemoteSources], at(0, 3, 0)) {
		t.Error("a seventeenth source of one host got a day, or took a listed one's")
	}
	e.Record(at(0, 4, 0), dayOf("pc-new/without-a-list"), nil)
	if e.Has("pc-new/without-a-list", at(0, 4, 0)) {
		t.Error("without a list of what is there, a host's seventeenth source took the place of another")
	}
	// sources of config.yaml (no host before the name) are as many as config.yaml has: no limit of their own
	var own []string
	for i := 0; i < 30; i++ {
		own = append(own, fmt.Sprintf("strip-%02d", i))
	}
	e.Record(at(0, 5, 0), dayOf(own...), nil)
	if !e.Has("strip-29", at(0, 5, 0)) {
		t.Error("the panel's own sources are limited to a host's number")
	}
}
