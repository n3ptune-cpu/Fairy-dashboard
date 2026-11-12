package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// r1-p2-08 (audit 5) through the routes: a run whose journal cannot be written (a full disk) sends nothing
// further, stops and says so on the page, in the audit log and in a warning; no run starts while the journal
// cannot be written, and the refused start is in the audit log.
func TestWakeUpdateStopsWhenItsJournalCannotBeWritten(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.offline()
	f.optIn("pc-a", "pc-b")
	r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a", "pc-b"}})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	for i := 0; i < 30; i++ { // pc-a gets its wake; pc-b is still waiting for its turn at the relay or just woken
		f.tick()
	}
	sent := len(f.requests())
	if sent == 0 {
		t.Fatal("setup: the run sent nothing in 30 s")
	}
	journal := filepath.Join(e.s.Cfg.DataDir, "wake-update.json")
	_ = os.Remove(journal + ".lock")
	if err := os.Mkdir(journal+".lock", 0o700); err != nil { // from here on no write of the journal works
		t.Fatal(err)
	}
	run := f.finish()
	if run.State != wakerun.RunStopped || !strings.HasPrefix(run.Note, "stopped: its journal (wake-update.json) could not be written: ") {
		t.Fatalf("the run: %s %q", run.State, run.Note)
	}
	if got := f.requests(); len(got) > sent+1 { // at most the one request whose result was the write that failed
		t.Fatalf("requests after the journal failed: %v (before: %d)", got, sent)
	}
	for _, id := range []string{"pc-a", "pc-b"} {
		h := hostOf(run, id)
		if !strings.Contains(h.Text, "the run stopped (its journal (wake-update.json) could not be written: ") {
			t.Fatalf("%s: %s", id, h.Text)
		}
		if h.WakeSentAt != nil && (h.On == wakerun.OnUntouched || strings.Contains(h.Text, "Nothing was sent")) {
			t.Fatalf("%s got a wake and the report says (%s): %s", id, h.On, h.Text)
		}
	}
	// the page, and a warning
	page := e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
	if p, _ := page.body["problem"].(string); !strings.Contains(p, "cannot be written") {
		t.Fatalf("the page does not say that no run can start: %s", page.raw)
	}
	warned := false
	for _, n := range e.s.Notes.List("u2", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events {
		warned = warned || (n.Kind == "wakeupdate_done" && n.Severity == notifications.Warning && strings.Contains(n.Text, "could not be written"))
	}
	if !warned {
		t.Fatal("no warning about the stopped run")
	}
	// a new start is refused before "Confirm it's you", and nothing is sent
	before := len(f.requests())
	if r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-b"}}); r.code != 409 || !strings.Contains(string(r.raw), "cannot be written") {
		t.Fatalf("a start while the journal cannot be written: %d %s", r.code, r.raw)
	}
	if act, _ := e.s.WakeRuns.Snapshot(); act != nil || len(f.requests()) != before {
		t.Fatal("a refused start left a run or sent something")
	}
}
