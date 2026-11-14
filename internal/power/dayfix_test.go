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

// ---- a clock that is wrong

// minutes is the number of minutes with readings the source has in the 24 hours that end now.
func minutes(d *DayStore, id string, now time.Time) (n int) {
	for _, v := range d.Series(id, now).Avg {
		if v == v {
			n++
		}
	}
	return n
}

// tenHours is a file with ten hours of two sources, saved at 22:00, and the file's bytes.
func tenHours(t *testing.T) (path string, was []byte) {
	t.Helper()
	path = dayPath(t)
	d := loaded(path, at(0, 0, 0))
	for i := 0; i < 600; i++ {
		d.Record(at(0, i, 0), []DaySample{{ID: "plug-a", W: 50}, {ID: "pc-a/plug-b", W: 20}}, nil)
	}
	saved(t, d, at(10, 0, 0))
	was, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, was
}

// A panel that starts with its clock behind (a machine without a clock of its own, before the time comes from
// the network) found a file "from the future": every minute in it was after the clock, so none was taken, and
// five minutes later the file was replaced with the nothing the panel had. The day was gone although the clock
// was right a minute later. Such a file is left alone now: not read and not replaced, until the clock has
// passed the time it was saved at. Then it is read, and the day goes on.
func TestDayFileFromALaterTimeThanTheClockIsKeptUntilTheClockIsThere(t *testing.T) {
	log := captureLog(t)
	path, was := tenHours(t)
	e := loaded(path, at(-48, 0, 0)) // the start, two days behind
	if e.Len() != 0 {
		t.Fatal("minutes that are after the panel's clock were taken")
	}
	for i := 0; i <= 7*12; i++ { // seven minutes with that clock: samples, and the tick asks for a write
		now := at(-48, 0, 5*i)
		rec(e, now, 50)
		e.SaveIfDue(now)
	}
	if e.Flush(at(-48, 7, 1), 5*time.Second) {
		t.Error("a stop with the clock behind says that everything is written")
	}
	if now, err := os.ReadFile(path); err != nil || string(now) != string(was) {
		t.Fatalf("the file was replaced while the clock was behind the time it was saved at (%v, %d bytes, were %d)", err, len(now), len(was))
	}
	if n := strings.Count(log.String(), "later time than the panel's clock"); n != 1 {
		t.Errorf("the log says %d times that the file is left alone, want once:\n%s", n, log)
	}
	// the time comes: the next tick reads the file (not in its own time), and from then on it is written
	now := at(10, 1, 0)
	rec(e, now, 50)
	e.SaveIfDue(now)
	if !e.Flush(at(10, 1, 5), 5*time.Second) {
		t.Fatal("after the clock was set right the day is not written")
	}
	if a, b := minutes(e, "plug-a", at(10, 1, 5)), minutes(e, "pc-a/plug-b", at(10, 1, 5)); a != 601 || b != 600 {
		t.Fatalf("after the clock was set right: %d and %d minutes, want the file's 600 and the one of now", a, b)
	}
	f := loaded(path, at(10, 2, 0))
	if a, b := minutes(f, "plug-a", at(10, 2, 0)), minutes(f, "pc-a/plug-b", at(10, 2, 0)); a != 601 || b != 600 {
		t.Errorf("in the file after that: %d and %d minutes", a, b)
	}
	// a file that was saved just now, or a second ago, is read at the start as ever
	if g := loaded(path, at(10, 1, 5)); minutes(g, "plug-a", at(10, 1, 5)) != 601 {
		t.Error("a file saved in this second is not read at the start")
	}
}

// What the panel measures itself while the file waits is not lost when the file comes in: in a minute that
// both have, the panel's own readings count (they are the ones under the clock it has now).
func TestDayFileFromALaterTimeIsMergedWithWhatWasMeasuredMeanwhile(t *testing.T) {
	path, _ := tenHours(t)          // readings from 12:00 to 21:59, saved at 22:00
	e := loaded(path, at(9, 30, 0)) // the clock was set back by half an hour before this start
	for i := 0; i < 30; i++ {
		rec(e, at(9, 30+i, 0), 80) // 21:30 to 21:59 a second time, with 80 W
	}
	if minutes(e, "plug-a", at(9, 59, 30)) != 30 {
		t.Fatal("the file was read although the clock is before the time it was saved at")
	}
	e.SaveIfDue(at(10, 0, 0))
	if !e.Flush(at(10, 0, 1), 5*time.Second) {
		t.Fatal("not written")
	}
	s := seriesOf(t, e, "plug-a", at(10, 0, 1))
	if len(s.Avg) != 600 || *s.Avg[0] != 50 || *s.Avg[569] != 50 || *s.Avg[570] != 80 || *s.Avg[599] != 80 {
		t.Fatalf("the day after the file came in: %d minutes, 12:00 %v, 21:29 %v, 21:30 %v, 21:59 %v",
			len(s.Avg), *s.Avg[0], *s.Avg[569], *s.Avg[570], *s.Avg[599])
	}
	// the minute that was open goes on with its own samples
	rec(e, at(9, 59, 30), 20)
	if s := seriesOf(t, e, "plug-a", at(10, 0, 2)); *s.Avg[599] != 50 || *s.Min[599] != 20 {
		t.Errorf("21:59 after one more sample: avg %v min %v, want 50 (80 and 20) and 20", *s.Avg[599], *s.Min[599])
	}
}

// The other way round: the file was written while the clock was ahead, and the clock is right now. The panel
// cannot tell the two apart. It waits for an hour of its own running, then the file is replaced: without that
// end, a file from a clock that was a year ahead would keep the panel from ever writing a day again.
func TestDayFileFromALaterTimeIsGivenUpAfterAnHour(t *testing.T) {
	log := captureLog(t)
	path, was := tenHours(t)
	e := loaded(path, at(0, 0, 0)) // ten hours before the file was saved
	for i := 0; i < 60; i++ {
		rec(e, at(0, i, 0), 50)
		e.SaveIfDue(at(0, i, 0))
		e.Flush(at(0, i, 1), 5*time.Second)
	}
	if now, _ := os.ReadFile(path); string(now) != string(was) {
		t.Fatal("the file was replaced in the first hour")
	}
	rec(e, at(1, 0, 0), 50)
	e.SaveIfDue(at(1, 0, 0))
	if !e.Flush(at(1, 0, 1), 5*time.Second) {
		t.Fatal("after an hour the day is still not written")
	}
	if !strings.Contains(log.String(), "is replaced now") {
		t.Errorf("the log does not say that the file is given up:\n%s", log)
	}
	f := loaded(path, at(1, 0, 2))
	if n := minutes(f, "plug-a", at(1, 0, 2)); n != 61 || f.Len() != 1 {
		t.Errorf("the file after the hour: %d minutes of plug-a, %d sources; want what the panel measured itself (61 minutes, 1 source)", n, f.Len())
	}
}

// A host that is removed while the file waits does not come back with the file.
func TestDayFileFromALaterTimeARemovedHostStaysRemoved(t *testing.T) {
	path, _ := tenHours(t)
	e := loaded(path, at(-48, 0, 0))
	e.Forget("pc-a/", at(-48, 0, 30))
	rec(e, at(10, 1, 0), 50)
	e.SaveIfDue(at(10, 1, 0))
	if !e.Flush(at(10, 1, 5), 5*time.Second) {
		t.Fatal("not written")
	}
	if e.Has("pc-a/plug-b", at(10, 1, 5)) || minutes(e, "plug-a", at(10, 1, 5)) != 601 {
		t.Errorf("after the file came in: the removed host's plug has a day: %v; plug-a has %d minutes", e.Has("pc-a/plug-b", at(10, 1, 5)), minutes(e, "plug-a", at(10, 1, 5)))
	}
	if f := loaded(path, at(10, 2, 0)); f.Has("pc-a/plug-b", at(10, 2, 0)) {
		t.Error("the removed host's plug is in the file again")
	}
}

// One history tick with a wrong clock (a day or more back: a machine that woke up with its clock at zero, a
// restored one) emptied the day of every source that was sampled in it: every bucket was "after" that minute.
// Such a sample is not taken now, and nothing is emptied for it.
func TestDayOneSampleUnderAClockFarBackDoesNotEmptyTheDay(t *testing.T) {
	d := NewDayStore()
	for i := 0; i < 600; i++ {
		rec(d, at(0, i, 0), 50)
	}
	for _, back := range []time.Time{at(-24*365, 0, 0), at(-14, -1, 0)} { // a year; a day before the newest minute
		rec(d, back, 70)
		if n := minutes(d, "plug-a", at(9, 59, 30)); n != 600 {
			t.Fatalf("one sample with the clock at %s left %d of 600 minutes", back.Format("2006-01-02 15:04"), n)
		}
		if minutes(d, "plug-a", back) != 0 {
			t.Errorf("the sample with the clock at %s was taken", back.Format("2006-01-02 15:04"))
		}
	}
	rec(d, at(10, 0, 5), 50) // and right again: the day goes on
	s := seriesOf(t, d, "plug-a", at(10, 0, 10))
	if n := minutes(d, "plug-a", at(10, 0, 10)); n != 601 || s.Summary.MaxW != 50 {
		t.Errorf("after the clock was right again: %d minutes, highest %v W; want 601 and 50", n, s.Summary.MaxW)
	}
	// one tick with the clock far ahead: the samples after it are "far back" for the store, but it is the one
	// minute ahead that is the odd one, and the day goes on at once
	rec(d, at(24*365+11, 0, 0), 70) // (23:00, a minute of the day that has no reading: its place in the ring is free)
	rec(d, at(10, 1, 0), 50)
	if n := minutes(d, "plug-a", at(10, 1, 5)); n != 602 {
		t.Errorf("after one sample with the clock a year ahead: %d minutes, want 602", n)
	}
	if n := minutes(d, "plug-a", at(24*365+11, 0, 30)); n != 0 {
		t.Errorf("the minute of the clock that was ahead is still there")
	}
	// less than a day back is a clock that was set back, as before: what was measured after that minute goes
	rec(d, at(9, 0, 0), 50)
	if n := minutes(d, "plug-a", at(10, 1, 5)); n != 541 {
		t.Errorf("after the clock was set back by an hour: %d minutes, want the 541 until then", n)
	}
}

// A clock that is set back by more than a day and stays there: after ten minutes of it the store believes it,
// and the source's day begins anew (as it does at once for a clock that is set back by less).
func TestDayAClockThatStaysFarBackIsBelievedAfterTenMinutes(t *testing.T) {
	d := NewDayStore()
	for i := 0; i < 600; i++ {
		rec(d, at(0, i, 0), 50)
	}
	for i := 0; i < 120; i++ { // 9 minutes 55 seconds
		rec(d, at(-72, 0, 5*i), 70)
	}
	if a, b := minutes(d, "plug-a", at(-72, 9, 55)), minutes(d, "plug-a", at(9, 59, 30)); a != 0 || b != 600 {
		t.Fatalf("in the first ten minutes of a clock three days back: %d minutes there and %d of the day before, want 0 and 600", a, b)
	}
	for i := 0; i < 24; i++ { // two minutes more
		rec(d, at(-72, 10, 5*i), 70)
	}
	if a, b := minutes(d, "plug-a", at(-72, 11, 55)), minutes(d, "plug-a", at(9, 59, 30)); a != 2 || b != 0 {
		t.Errorf("after ten minutes: %d minutes under the new clock and %d of the day before, want 2 and 0", a, b)
	}
	// a sample under the right clock in between ends the count: ten minutes anew
	e := NewDayStore()
	for i := 0; i < 600; i++ {
		rec(e, at(0, i, 0), 50)
	}
	for i := 0; i < 9; i++ {
		rec(e, at(-72, i, 0), 70)
	}
	rec(e, at(10, 0, 0), 50)
	for i := 9; i < 18; i++ {
		rec(e, at(-72, i, 0), 70)
	}
	if n := minutes(e, "plug-a", at(10, 0, 30)); n != 601 {
		t.Errorf("a clock that jumps between right and far back: %d minutes of the day are left, want 601", n)
	}
}

// ---- a host that is removed

// The history tick takes its list of sources first and records after that. A host that was removed in
// between got a day again for the plug it relayed, from the one sample that was on its way: a line of one
// minute for a host that is gone, which stayed for a day and went into the file. For a minute after a
// removal no sample starts a day for a source of that host.
func TestDayASampleOnItsWayWhenItsHostIsRemovedStartsNoDay(t *testing.T) {
	d := NewDayStore()
	d.Record(at(0, 0, 0), dayOf("pc-a/plug-a", "pc-b/plug-b"), nil)
	if !d.Forget("pc-a/", at(0, 0, 2)) {
		t.Fatal("nothing was forgotten")
	}
	d.Record(at(0, 0, 1), dayOf("pc-a/plug-a", "pc-b/plug-b", "pc-ab/plug-c"), nil) // taken before the removal
	d.Record(at(0, 0, 3), dayOf("pc-a/plug-a", "pc-a/plug-new"), nil)               // and a tick that began before it
	if d.Has("pc-a/plug-a", at(0, 0, 3)) || d.Has("pc-a/plug-new", at(0, 0, 3)) {
		t.Fatal("a sample that came after the host's removal started a day for its plug")
	}
	if !d.Has("pc-b/plug-b", at(0, 0, 3)) || !d.Has("pc-ab/plug-c", at(0, 0, 3)) {
		t.Error("the removal of pc-a keeps a source of another host from being recorded")
	}
	// a minute later the id is free again: a host added under it starts its own day
	d.Record(at(0, 1, 1), dayOf("pc-a/plug-a"), nil)
	if d.Has("pc-a/plug-a", at(0, 1, 1)) {
		t.Error("59 seconds after the removal a sample started a day")
	}
	d.Record(at(0, 1, 2), dayOf("pc-a/plug-a"), nil)
	if !d.Has("pc-a/plug-a", at(0, 1, 2)) {
		t.Error("a minute after the removal a host under the same id still gets no day")
	}
	if len(d.forgot) != 0 {
		t.Errorf("the list of removals keeps %d entries after their minute", len(d.forgot))
	}
	// nor does it keep one whose minute never ends because the clock went back
	d.Forget("pc-c/", at(5, 0, 0))
	d.Record(at(4, 58, 0), dayOf("pc-c/plug"), nil)
	if len(d.forgot) != 0 || !d.Has("pc-c/plug", at(4, 58, 0)) {
		t.Errorf("after the clock went back by two minutes: %d removals kept", len(d.forgot))
	}
}

// The same with the history tick itself against the removal, many times: whichever comes first, the removed
// host's plug has no day afterwards.
func TestDayRemoveAgainstTheHistoryTick(t *testing.T) {
	pm := manager(t, nil, nil, nil)
	rows := []map[string]any{{"id": "plug-a", "ok": true, "watts": 40.0}}
	stray := 0
	for round := 0; round < 400; round++ {
		pm.UpdateRemote("pc-a", rows, false, nil, nil)
		pm.SampleHistory()
		if !pm.Day.Has("pc-a/plug-a", time.Now()) {
			t.Fatal("the rig: the relayed plug has no day")
		}
		tick := make(chan struct{})
		go func() { defer close(tick); pm.SampleHistory() }()
		pm.UpdateRemote("pc-a", nil, false, nil, nil) // what Remove does: the host's sources go, and their days
		pm.Day.Forget("pc-a/", time.Now())
		<-tick
		if pm.Day.Has("pc-a/plug-a", time.Now()) {
			stray++
		}
		pm.Day.mu.Lock()
		pm.Day.forgot = nil // the next round is a host added again, later than a minute
		pm.Day.mu.Unlock()
	}
	if stray > 0 {
		t.Errorf("%d of 400 removals left a day for the removed host's plug", stray)
	}
}
