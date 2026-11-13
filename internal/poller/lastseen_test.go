package poller

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// seenEnv is a poller with a clock of the test's own (time of day and steady clock move together), a laptop
// that answers while on is set, and a tower that never does.
type seenEnv struct {
	t   *testing.T
	cfg *config.Panel
	p   *Poller
	b   *testutil.Backend
	now time.Time
	on  bool
}

func newSeenEnv(t *testing.T, cfg *config.Panel, at time.Time) *seenEnv {
	t.Helper()
	if cfg == nil {
		cfg = testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	}
	e := &seenEnv{t: t, cfg: cfg, b: testutil.NewBackend(), now: at}
	e.b.Metrics["laptop"] = func() (jsonx.M, error) {
		if !e.on {
			return nil, devices.Errf("connection failed")
		}
		return testutil.AgentMetrics(), nil
	}
	delete(e.b.Metrics, "tower")
	p, err := New(cfg, config.Secrets{}, e.b)
	if err != nil {
		t.Fatal(err)
	}
	began := at
	p.Clock = func() time.Time { return e.now }
	p.Mono = func() time.Duration { return e.now.Sub(began) }
	e.p = p
	return e
}

// poll lets d pass and asks every machine once.
func (e *seenEnv) poll(d time.Duration) {
	e.b.Mu.Lock()
	e.now = e.now.Add(d)
	e.b.Mu.Unlock()
	e.p.PollOnce(context.Background())
}

func (e *seenEnv) set(on bool) {
	e.b.Mu.Lock()
	e.on = on
	e.b.Mu.Unlock()
}

// file is what last-seen.json holds for the agent ("": nothing).
func (e *seenEnv) file(id string) string {
	e.t.Helper()
	seen, _ := adminstate.New(e.cfg.DataDir).AgentSeen()
	t, ok := seen[id]
	if !ok {
		return ""
	}
	return t.Format(time.RFC3339)
}

// stat is last-seen.json as a file: a write puts a new file in the old one's place.
func (e *seenEnv) stat() os.FileInfo {
	e.t.Helper()
	st, err := os.Stat(filepath.Join(e.cfg.DataDir, adminstate.LastSeenFile))
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func shownSeen(p *Poller, id string) string {
	s, ok := snap(p)[id]
	if !ok {
		return "no row"
	}
	if s.LastSeen == nil {
		return "never"
	}
	return s.LastSeen.UTC().Format(time.RFC3339)
}

var seenStart = time.Date(2026, 11, 13, 16, 0, 0, 0, time.UTC)

// A machine that is off when the panel starts is shown with the time it was last seen, not with "never"
// (build 13: the time was in memory only). The time comes from last-seen.json; it is what the row shows and
// nothing else: the machine is "offline" and not "stale", and what decides on its present state has not seen
// it.
func TestLastSeenSurvivesARestart(t *testing.T) {
	e := newSeenEnv(t, nil, seenStart)
	e.set(true)
	e.poll(0)
	e.poll(5 * time.Second)
	if got := shownSeen(e.p, "laptop"); got != "2026-11-13T16:00:05Z" || shownSeen(e.p, "tower") != "never" {
		t.Fatalf("while it answers: laptop %s, tower %s", got, shownSeen(e.p, "tower"))
	}
	// it is switched off; the panel notices with the next poll and stops a while later
	e.set(false)
	e.poll(5 * time.Second)
	e.poll(time.Hour)
	e.p.FlushSeen()
	if got := e.file("laptop"); got != "2026-11-13T16:00:05Z" || e.file("tower") != "" {
		t.Fatalf("last-seen.json when the panel stops: laptop %q, tower %q", got, e.file("tower"))
	}

	// The panel starts again two hours later; the laptop is still off.
	r := newSeenEnv(t, e.cfg, seenStart.Add(2*time.Hour))
	if got := shownSeen(r.p, "laptop"); got != "no row" {
		t.Fatalf("before the first poll there is no row: %s", got)
	}
	r.poll(0)
	rows := snap(r.p)
	if got := shownSeen(r.p, "laptop"); got != "2026-11-13T16:00:05Z" || rows["laptop"].Status != "offline" {
		t.Fatalf("after the restart: last seen %s, status %s", got, rows["laptop"].Status)
	}
	if got := shownSeen(r.p, "tower"); got != "never" { // never seen by any panel: as before
		t.Fatalf("tower: %s", got)
	}
	// it is not the machine's present state: the poller has not heard it
	if info, seen := r.p.AgentInfo("laptop"); seen || info.Online || !info.SeenAt.IsZero() || info.SeenAgo != 0 {
		t.Fatalf("AgentInfo after the restart: %+v seen %v", info, seen)
	}
	if _, sys, _ := r.p.AgentSeen("laptop"); sys.LastSeen != nil || sys.Status != "offline" {
		t.Fatalf("the poller's own entry has a time, or is not offline: %v %s", sys.LastSeen, sys.Status)
	}
	// it comes back: the row has the new time, and the file with it
	r.set(true)
	r.poll(10 * time.Second)
	if got := shownSeen(r.p, "laptop"); got != "2026-11-13T18:00:10Z" || snap(r.p)["laptop"].Status != "online" || r.file("laptop") != "2026-11-13T18:00:10Z" {
		t.Fatalf("back on: shown %s, file %s", got, r.file("laptop"))
	}
}

// A kept time that is seconds old does not make a machine "stale" (amber) after a restart: a machine that has
// not answered since the panel started is "offline", as it was without the file.
func TestAKeptTimeNeverMakesAMachineStale(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	if err := adminstate.New(cfg.DataDir).SetAgentSeen(map[string]time.Time{"laptop": seenStart.Add(-2 * time.Second)}, nil); err != nil {
		t.Fatal(err)
	}
	e := newSeenEnv(t, cfg, seenStart)
	for i := 0; i < 5; i++ {
		e.poll(time.Second)
		if s := snap(e.p)["laptop"]; s.Status != "offline" || shownSeen(e.p, "laptop") != "2026-11-13T15:59:58Z" {
			t.Fatalf("poll %d: %s, last seen %s", i, s.Status, shownSeen(e.p, "laptop"))
		}
	}
}

// "Off" for "Update unpowered hosts" is a quarter of an hour of polls without any answer, counted by this
// process's own steady clock from its start. A time read from last-seen.json changes nothing about that: with it
// and without it the poller says the same about the machine at every step, and never "silent since before the
// panel started".
func TestAKeptTimeDoesNotCountAsSilence(t *testing.T) {
	plain := newSeenEnv(t, nil, seenStart)
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	if err := adminstate.New(cfg.DataDir).SetAgentSeen(map[string]time.Time{"laptop": seenStart.Add(-3 * time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	kept := newSeenEnv(t, cfg, seenStart)
	if shownSeen(kept.p, "laptop") != "no row" {
		t.Fatal("the fixture")
	}
	quiet := func(e *seenEnv) (time.Duration, string) {
		info, sys, _ := e.p.AgentSeen("laptop")
		if info.QuietSince.IsZero() {
			return -1, sys.Status
		}
		return e.now.Sub(info.QuietSince), sys.Status
	}
	step := func(d time.Duration) (time.Duration, string) {
		t.Helper()
		plain.poll(d)
		kept.poll(d)
		q1, s1 := quiet(plain)
		q2, s2 := quiet(kept)
		if q1 != q2 || s1 != s2 {
			t.Fatalf("at %s the poller says %s/%s without the file and %s/%s with it", plain.now.Format("15:04:05"), q1, s1, q2, s2)
		}
		return q2, s2
	}
	if q, st := step(0); q != 0 || st != "offline" { // the first poll that got nothing: silent since now
		t.Fatalf("right after the start: quiet for %s, %s", q, st)
	}
	if got := shownSeen(kept.p, "laptop"); got != "2026-11-13T13:00:00Z" {
		t.Fatalf("the row: %s", got)
	}
	var q time.Duration
	for i := 0; i < 89; i++ { // 14 min 50 s of polls
		q, _ = step(10 * time.Second)
	}
	if q != 14*time.Minute+50*time.Second {
		t.Fatalf("after 14:50 of polls: quiet for %s", q)
	}
	if q, _ = step(10 * time.Second); q != 15*time.Minute {
		t.Fatalf("after a quarter of an hour: quiet for %s", q)
	}
}

// When the times are written: the first good answer at once, then at most once a minute for all agents
// together; when an agent stops answering, with the poll that notices; when the panel stops; and an agent
// that is removed is taken out.
func TestLastSeenIsNotWrittenWithEveryPoll(t *testing.T) {
	e := newSeenEnv(t, nil, seenStart)
	e.set(true)
	e.poll(0)
	if got := e.file("laptop"); got != "2026-11-13T16:00:00Z" {
		t.Fatalf("the first good answer is written at once: %q", got)
	}
	written := e.stat()
	for i := 0; i < 11; i++ { // 55 s of polls: nothing is written
		e.poll(5 * time.Second)
	}
	if !os.SameFile(written, e.stat()) || e.file("laptop") != "2026-11-13T16:00:00Z" {
		t.Fatalf("the file was rewritten within the minute: %q", e.file("laptop"))
	}
	if got := shownSeen(e.p, "laptop"); got != "2026-11-13T16:00:55Z" { // what is shown is from memory
		t.Fatalf("shown: %s", got)
	}
	e.poll(5 * time.Second) // the minute is over
	if os.SameFile(written, e.stat()) || e.file("laptop") != "2026-11-13T16:01:00Z" {
		t.Fatalf("after a minute: %q", e.file("laptop"))
	}
	// it goes off 20 s later: the time of its last answer is in the file with the poll that notices
	e.poll(10 * time.Second)
	e.poll(10 * time.Second)
	e.set(false)
	written = e.stat()
	e.poll(5 * time.Second)
	if os.SameFile(written, e.stat()) || e.file("laptop") != "2026-11-13T16:01:20Z" {
		t.Fatalf("when it stopped answering: %q", e.file("laptop"))
	}
	written = e.stat()
	for i := 0; i < 30; i++ { // and nothing more while it is off
		e.poll(10 * time.Second)
	}
	if !os.SameFile(written, e.stat()) {
		t.Fatal("the file is rewritten for a machine that is off")
	}
	// An agent that answers every other poll, a second apart: not a write for every poll that fails.
	e.set(true)
	e.poll(time.Second)
	writes, last := 0, e.stat()
	for i := 0; i < 20; i++ {
		e.set(i%2 == 1)
		e.poll(time.Second)
		if st := e.stat(); !os.SameFile(last, st) {
			writes, last = writes+1, st
		}
	}
	if writes > 5 { // 20 s: one write when the minute is over at most, and one in five seconds for a poll that fails
		t.Fatalf("%d writes in 20 seconds", writes)
	}
	// what waits is written when the panel stops
	e.set(true)
	e.poll(time.Second)
	want := e.now.Format(time.RFC3339)
	if e.file("laptop") == want {
		t.Fatal("the fixture: the latest time is in the file already")
	}
	e.p.FlushSeen()
	if e.file("laptop") != want {
		t.Fatalf("after FlushSeen: %q, want %q", e.file("laptop"), want)
	}
	last = e.stat()
	e.p.FlushSeen() // nothing waits: nothing is written
	if !os.SameFile(last, e.stat()) {
		t.Fatal("FlushSeen wrote with nothing to write")
	}
	// none of this wrote state.json, the file with the kill switch: there is still none
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("state.json was written: %v", err)
	}
	// removed: the entry goes, and a host added under the id later was never seen
	e.p.RemoveAgent("laptop")
	if e.file("laptop") != "" {
		t.Fatalf("after RemoveAgent: %q", e.file("laptop"))
	}
	if err := e.p.AddAgent(config.Agent{ID: "laptop", Name: "Another laptop", URL: "http://laptop", Token: "read-word", Managed: true}, nil); err != nil {
		t.Fatal(err)
	}
	e.set(false)
	e.poll(time.Second)
	if got := shownSeen(e.p, "laptop"); got != "never" {
		t.Fatalf("a host added under the id of a removed one: last seen %s", got)
	}
}

// A host that is added in the panel under an id for which the file still has a time (a host taken out of
// config.yaml by hand) was never seen: the entry goes.
func TestAHostAddedUnderAKeptIDWasNeverSeen(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	if err := adminstate.New(cfg.DataDir).SetAgentSeen(map[string]time.Time{"old-box": seenStart.Add(-time.Hour), "laptop": seenStart.Add(-time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	e := newSeenEnv(t, cfg, seenStart)
	if err := e.p.AddAgent(config.Agent{ID: "old-box", Name: "New box", URL: "http://old-box", Token: "read-word", Managed: true}, nil); err != nil {
		t.Fatal(err)
	}
	e.poll(0)
	if got := shownSeen(e.p, "old-box"); got != "never" || e.file("old-box") != "" || e.file("laptop") == "" {
		t.Fatalf("old-box: shown %s, file %q; laptop's entry %q", got, e.file("old-box"), e.file("laptop"))
	}
}

// A time that is later than the panel's clock (written while the clock was ahead) is not shown.
func TestAKeptTimeFromTheFutureIsNotShown(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	if err := adminstate.New(cfg.DataDir).SetAgentSeen(map[string]time.Time{"laptop": seenStart.Add(time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	e := newSeenEnv(t, cfg, seenStart)
	e.poll(0)
	if got := shownSeen(e.p, "laptop"); got != "never" {
		t.Fatalf("shown: %s", got)
	}
	e.poll(2 * time.Hour) // the clock has passed it
	if got := shownSeen(e.p, "laptop"); got != "2026-11-13T17:00:00Z" {
		t.Fatalf("shown later: %s", got)
	}
}

// A last-seen.json that cannot be used does not keep the panel from starting, and nothing in it is shown:
// empty, cut off, not an object, larger than a state file may be, a link, a folder, a list of agents that is
// no object or holds more entries than the panel ever writes. The machines are polled and shown as never seen.
// state.json is another file and is not touched.
//
// What becomes of the file (review of build 14, F10). A PLAIN file that is damaged was left as it was for
// good, like every state file, and so the times were not kept until somebody deleted it: the feature stayed
// off without a word after the one line at the start. It is moved aside once, to last-seen.json.bad (an older
// one is replaced), and a new file is started, with one warning. A link or a folder at that name is never
// moved, read or written; that the times cannot be written is then logged once, not with every poll.
func TestALastSeenFileThatCannotBeUsedIsIgnored(t *testing.T) {
	big := func(path string) error { // a sparse file one byte over the limit of a state file
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Truncate(64<<20 + 1)
	}
	many := func(path string) error {
		var b strings.Builder
		b.WriteString(`{"agents": {"laptop": "2026-11-13T12:00:00Z"`)
		for i := 0; i <= adminstate.MaxAgentSeen; i++ {
			b.WriteString(`, "x-` + strconv.Itoa(i) + `": "2026-11-13T12:00:00Z"`)
		}
		b.WriteString("}}")
		return os.WriteFile(path, []byte(b.String()), 0o600)
	}
	text := func(s string) func(string) error {
		return func(path string) error { return os.WriteFile(path, []byte(s), 0o600) }
	}
	// a kept copy from an earlier start, or something else of that name
	oldBad := func(write func(path string) error) func(string) error {
		return func(path string) error {
			if err := os.WriteFile(path+".bad", []byte("an older one"), 0o600); err != nil {
				return err
			}
			return write(path)
		}
	}
	badIsAFolder := func(path string) error {
		if err := os.Mkdir(path+".bad", 0o700); err != nil {
			return err
		}
		return os.WriteFile(path, nil, 0o600)
	}
	for name, c := range map[string]struct {
		write    func(path string) error
		writable bool // the panel can write the file afterwards (it is a state file in order, or it was started over)
		said     bool // the start says that the file is not used
		aside    bool // the start moved it to last-seen.json.bad and says so instead
	}{
		"empty":                               {text(""), true, false, true},
		"cut off":                             {text(`{"agents": {"laptop": "2026-11-13T12:0`), true, false, true},
		"not an object":                       {text(`["laptop"]`), true, false, true},
		"larger than a state file":            {big, true, false, true},
		"cut off, and an older .bad is there": {oldBad(text(`{"agents": {"lap`)), true, false, true},
		"empty, and .bad is a folder":         {badIsAFolder, false, true, false},
		"a link to another file":              {func(path string) error { return os.Symlink("/dev/zero", path) }, false, true, false},
		"a link to nothing":                   {func(path string) error { return os.Symlink(path+".nowhere", path) }, false, true, false},
		"a folder":                            {func(path string) error { return os.Mkdir(path, 0o700) }, false, true, false},
		"agents is a list":                    {text(`{"agents": ["2026-11-13T12:00:00Z"]}`), true, false, false},
		"agents is a text":                    {text(`{"agents": "2026-11-13T12:00:00Z"}`), true, false, false},
		"entries that are no times":           {text(`{"agents": {"laptop": 17, "tower": {"at": "2026-11-13T12:00:00Z"}}}`), true, false, false},
		"more entries than ever written":      {many, true, true, false},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
			if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(cfg.DataDir, adminstate.LastSeenFile)
			if err := c.write(path); err != nil {
				t.Skipf("the file cannot be made here: %v", err)
			}
			badBefore, _ := os.Lstat(path + ".bad")
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			var logMu sync.Mutex
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&logMu, &logs}, nil)))
			defer slog.SetDefault(old)
			count := func(text string) int {
				logMu.Lock()
				defer logMu.Unlock()
				return strings.Count(logs.String(), text)
			}
			e := newSeenEnv(t, cfg, seenStart) // New fails the test when the start fails
			if n := count("the file is not used"); (n == 1) != c.said || n > 1 {
				t.Fatalf("the start said %d times that the file is not used", n)
			}
			if n := count("the file could not be used and was moved aside, a new one is started"); (n == 1) != c.aside || n > 1 {
				t.Fatalf("the start said %d times that the file was moved aside", n)
			}
			if c.aside { // the file that was found is the kept copy now, as it was; at its name there is nothing until the first write
				kept, err := os.Lstat(path + ".bad")
				if err != nil || !os.SameFile(before, kept) || kept.Size() != before.Size() || !kept.Mode().IsRegular() {
					t.Fatalf("last-seen.json.bad is not the file that was found: %v %v", kept, err)
				}
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("after the move there is still something at the file's name: %v", err)
				}
			} else if after, _ := os.Lstat(path + ".bad"); (after == nil) != (badBefore == nil) || (after != nil && !os.SameFile(after, badBefore)) {
				t.Fatalf("last-seen.json.bad was made or replaced although nothing was moved")
			}
			e.poll(0)
			if got := shownSeen(e.p, "laptop"); got != "never" || snap(e.p)["laptop"].Status != "offline" {
				t.Fatalf("the laptop, off: last seen %s, %s", got, snap(e.p)["laptop"].Status)
			}
			e.set(true)
			for i := 0; i < 4; i++ { // four minutes of answers: four times a write is due
				e.poll(61 * time.Second)
			}
			if s := snap(e.p)["laptop"]; s.Status != "online" || shownSeen(e.p, "laptop") != e.now.Format(time.RFC3339) {
				t.Fatalf("the laptop, on: %s, last seen %s", s.Status, shownSeen(e.p, "laptop"))
			}
			n := count("could not keep when the machines were last seen")
			if c.writable {
				if n != 0 || e.file("laptop") != e.now.Format(time.RFC3339) {
					t.Fatalf("the file can be written: %d warnings, laptop %q", n, e.file("laptop"))
				}
				return
			}
			if _, err := os.Lstat(path + ".nowhere"); !errors.Is(err, os.ErrNotExist) { // nothing was written through a link to nothing
				t.Fatalf("the target of the link to nothing: %v", err)
			}
			if n != 1 {
				t.Fatalf("%d warnings for a file that cannot be written, want one", n)
			}
			// and it is left as it was found: not replaced, not followed
			if st, err := os.Lstat(path); err != nil || !os.SameFile(before, st) || st.Mode() != before.Mode() || st.Size() != before.Size() {
				t.Fatalf("the file was touched: %v %v", st, err)
			}
		})
	}
}
