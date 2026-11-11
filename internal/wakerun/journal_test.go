package wakerun

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// restart is the panel starting again: a new manager over the same journal, with a world of its own that
// records every request and knows nothing of the run.
func restart(t *testing.T, old *world) *world {
	t.Helper()
	w := &world{t: t, now: old.now.Add(30 * time.Second), ms: old.ms, order: old.order, agentHits: map[string][]time.Time{}}
	w.m = New(old.m.path, old.m.Settings, w)
	w.m.Now = func() time.Time { return w.now }
	w.m.Event = func(e Event) { w.events = append(w.events, e) }
	return w
}

// The panel restarts in the middle of a run. After the restart it reports, once, which machines the run had
// woken and where each one was; it sends nothing, and it never takes the run up again.
func TestRestartReportsTheInterruptedRunAndNeverContinuesIt(t *testing.T) {
	w := newWorld(t)
	w.off("pc-count").bootTakes = 20 * time.Second
	w.off("pc-upd").bootTakes = 150 * time.Second
	w.off("pc-wake").wakeBoots = false
	var ids = []string{"pc-count", "pc-upd", "pc-wake"}
	for _, id := range []string{"q0", "q1", "q2", "q3", "q4", "q5", "q6", "q7", "q8"} {
		w.off(id).wakeBoots = false
		ids = append(ids, id)
	}
	w.running("win-test").updTakes = 30 * time.Minute
	started := w.start(StartRequest{Hosts: ids, UpdateOnly: []string{"win-test"}})

	// the journal exists before anything was sent: a crash right after "start" is already on record
	b, err := os.ReadFile(w.m.path)
	if err != nil || len(w.calls) != 0 || !strings.Contains(string(b), started.ID) {
		t.Fatalf("the journal before the first step: %v, requests %v", err, w.calls)
	}
	if st, _ := os.Stat(w.m.path); st.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode %v", st.Mode().Perm())
	}
	for i := 0; i < 600; i++ {
		a, _ := w.m.Snapshot()
		if host(a, "pc-count").State == StShutting && host(a, "pc-upd").State == StUpdating && host(a, "pc-upd").UpdateSentAt != nil {
			break
		}
		w.tick()
	}
	a, _ := w.m.Snapshot()
	if host(a, "pc-count").State != StShutting || host(a, "pc-upd").State != StUpdating || host(a, "pc-wake").State != StWaking || host(a, "q8").State != StQueued {
		t.Fatalf("not the moment the test wants: %s", describe(a))
	}
	sent := len(w.calls)

	// the panel dies here. It starts again:
	w2 := restart(t, w)
	if err := w2.m.Load(); err != nil {
		t.Fatal(err)
	}
	act, hist := w2.m.Snapshot()
	if act != nil || len(hist) != 1 || hist[0].ID != started.ID || hist[0].State != RunInterrupted || hist[0].By != "Ann" {
		t.Fatalf("after the restart: active %v, history %d", act, len(hist))
	}
	r := hist[0]
	has(t, host(r, "pc-count"), StInterrupted, "The panel restarted while its shutdown was counting down (due at", "check that it went off")
	has(t, host(r, "pc-upd"), StInterrupted, "The run had woken it and it was ON (updating)", "Its update was asked for and may still be running")
	has(t, host(r, "pc-wake"), StInterrupted, "A wake was sent to it at", "if it switched on, it is ON")
	has(t, host(r, "q8"), StInterrupted, "The panel restarted before the run reached it. Nothing was sent to it.")
	has(t, host(r, "win-test"), StInterrupted, "It was already on and stays on.")
	if host(r, "pc-upd").On != OnYes || host(r, "pc-wake").On != OnUnknown || host(r, "q8").On != OnUntouched || host(r, "pc-count").On != OnUnknown {
		t.Fatalf("what the report says about power: %s", describe(r))
	}
	if len(w2.events) != 1 || w2.events[0].Kind != EvInterrupted || w2.events[0].Run.ID != started.ID || !strings.Contains(w2.events[0].Run.Summary, "PC-UPD: The panel restarted during the run") {
		t.Fatalf("the report: %+v", w2.events)
	}
	// it is never continued: an hour of steps sends nothing
	for i := 0; i < 3600; i++ {
		w2.now = w2.now.Add(time.Second)
		w2.sim()
		w2.poll()
		w2.m.Tick(context.Background())
	}
	if len(w2.calls) != 0 || len(w.calls) != sent || len(w2.events) != 1 {
		t.Fatalf("after the restart the panel sent %v (events %d)", w2.calls, len(w2.events))
	}
	// pc-count's own countdown, started before the restart, ran out on the machine by itself; pc-upd is on
	if w.ms["pc-count"].on || !w.ms["pc-upd"].on {
		t.Fatalf("the machines: pc-count on=%v pc-upd on=%v", w.ms["pc-count"].on, w.ms["pc-upd"].on)
	}
	// a second restart does not report it again, and it stays in the list of runs
	w3 := restart(t, w2)
	if err := w3.m.Load(); err != nil {
		t.Fatal(err)
	}
	if _, hist := w3.m.Snapshot(); len(w3.events) != 0 || len(hist) != 1 || !hist[0].Reported {
		t.Fatalf("second restart: %d events", len(w3.events))
	}
	// and a new run can be started; the interrupted one is history
	w3.ms["pc-upd"].on = false
	w3.advance(MinOff + time.Minute) // it counts as off only after that long without an answer
	w3.start(StartRequest{Hosts: []string{"pc-upd"}})
	done := w3.finish()
	has(t, host(done, "pc-upd"), StOff)
	if _, hist := w3.m.Snapshot(); len(hist) != 2 || hist[1].State != RunInterrupted {
		t.Fatal("history after the new run")
	}
}

// What the journal holds: ids, names, states, times, who started the run. It is JSON a person can read, and
// the finished runs it keeps are bounded.
func TestJournalContentAndBounds(t *testing.T) {
	w := newWorld(t)
	w.running("pc-a")
	for i := 0; i < historyKept+3; i++ {
		w.start(StartRequest{UpdateOnly: []string{"pc-a"}})
		w.finish()
		w.ms["pc-a"].version, w.ms["pc-a"].lastAt = "16-aaaaaaa", 0
		yes := true
		w.ms["pc-a"].behind = &yes
		w.advance(time.Minute)
	}
	b, err := os.ReadFile(w.m.path)
	if err != nil {
		t.Fatal(err)
	}
	var jf struct {
		Version int               `json:"version"`
		Active  *Run              `json:"active"`
		History []json.RawMessage `json:"history"`
	}
	if err := json.Unmarshal(b, &jf); err != nil || jf.Version != 1 || jf.Active != nil || len(jf.History) != historyKept {
		t.Fatalf("journal: version %d, active %v, %d finished runs (%v)", jf.Version, jf.Active, len(jf.History), err)
	}
	var first map[string]any
	_ = json.Unmarshal(jf.History[0], &first)
	for _, k := range []string{"id", "state", "started_at", "ended_at", "by", "list_hash", "hosts", "summary", "settings"} {
		if _, ok := first[k]; !ok {
			t.Errorf("a finished run lacks %q", k)
		}
	}
	if len(b) > 200<<10 {
		t.Fatalf("the journal of %d short runs is %d bytes", historyKept, len(b))
	}
}

// A journal that cannot be used is left as it is, and no run starts until someone has looked at it.
func TestUnusableJournalStopsNewRuns(t *testing.T) {
	big := `{"version":1,"active":null,"history":[],"pad":"` + strings.Repeat("x", journalMax) + `"}`
	manyHosts := `{"version":1,"active":{"id":"r1","state":"running","hosts":[` + strings.TrimSuffix(strings.Repeat(`{"id":"h","state":"shutting_down"},`, MaxHosts+1), ",") + `]},"history":[]}`
	for name, content := range map[string]string{
		"not JSON":                     "{not json",
		"another version":              `{"version":7,"active":null,"history":[]}`,
		"larger than it is":            big,
		"a run without an id":          `{"version":1,"active":{"state":"running","hosts":[]},"history":[]}`,
		"more machines than a run has": manyHosts,
		"null in the history":          `{"version":1,"active":null,"history":[null]}`,
	} {
		w := newWorld(t)
		w.off("pc-a")
		if err := os.WriteFile(w.m.path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := w.m.Load(); err == nil || w.m.JournalError() == nil {
			t.Fatalf("%s: loaded", name)
		}
		if _, err := w.m.Start(StartRequest{Hosts: []string{"pc-a"}, By: "Ann"}); err == nil || !strings.Contains(err.Error(), "the run journal cannot be used") {
			t.Fatalf("%s: a run started: %v", name, err)
		}
		w.advance(10 * time.Second)
		if after, _ := os.ReadFile(w.m.path); string(after) != content || len(w.calls) != 0 || len(w.events) != 0 {
			t.Fatalf("%s: the file was changed, or something was sent (%v)", name, w.calls)
		}
		// moved away, the panel works again
		if err := os.Remove(w.m.path); err != nil {
			t.Fatal(err)
		}
		if err := w.m.Load(); err != nil || w.m.JournalError() != nil {
			t.Fatalf("%s: after the file is gone: %v", name, err)
		}
		w.start(StartRequest{Hosts: []string{"pc-a"}})
		w.finish()
	}
}

// A journal that claims a run with a countdown in progress and machines the run "woke" is still only a
// report: nothing in it makes the panel send a request. (Whoever can write the data folder can make the panel
// say something false, and nothing more.)
func TestJournalIsNeverAnInstruction(t *testing.T) {
	w := newWorld(t)
	victim := w.running("pc-a")
	forged := `{"version":1,"history":[],"active":{"id":"feedfacefeedface","state":"running","started_at":"2026-11-11T02:00:00Z","by":"Mallory",
	  "list_hash":"00000000","settings":{"online_timeout_s":300,"settle_s":60,"shutdown_delay_s":120},
	  "hosts":[{"id":"pc-a","name":"PC-A","mode":"wake","state":"settling","since":"2026-11-11T02:10:00Z","text":"x","start_status":"offline",
	    "probed_off":true,"wake_sent_at":"2026-11-11T02:01:00Z","wakes":1,"online_at":"2026-11-11T02:02:00Z","boot_uptime_s":20,"log":[]}]}}`
	if err := os.WriteFile(w.m.path, []byte(forged), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.m.Load(); err != nil {
		t.Fatal(err)
	}
	w.advance(30 * time.Minute)
	act, hist := w.m.Snapshot()
	if act != nil || len(hist) != 1 || hist[0].State != RunInterrupted || len(w.calls) != 0 || !victim.on {
		t.Fatalf("active %v, requests %v, machine on %v", act, w.calls, victim.on)
	}
	if len(w.events) != 1 || w.events[0].Kind != EvInterrupted {
		t.Fatalf("events: %+v", w.events)
	}
}
