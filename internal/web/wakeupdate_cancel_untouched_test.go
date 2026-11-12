package web

import (
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// r1-b1-11 (audit 5, bug run): a run for machines that are on, cancelled before it reached them. The answer
// to the cancel and the page's report said "the machines it has woken stay on" although it woke none, and
// each machine had a green "done · stays on". The answer no longer speaks of woken machines, and the page
// gets each machine as skipped and not touched, with the sentence that it was not updated.
func TestWakeUpdateCancelBeforeTheRunReachedAMachine(t *testing.T) {
	e := setup(t)
	f := newFarm(t, e)
	f.optIn("laptop")
	admin := e.cookie(controller(), 0)
	run, err := e.s.WakeRuns.Start(wakerun.StartRequest{UpdateOnly: []string{"laptop"}, By: "test-admin", BySub: "u2"})
	if err != nil {
		t.Fatal(err)
	}
	r := e.do("DELETE", "/api/admin/wake-update/"+run.ID, nil, append([]string{"Cookie", admin}, wuWrite...)...)
	if r.code != 200 || r.body["message"] != "run cancelled: nothing else is started" {
		t.Fatalf("cancel: %d %s", r.code, r.raw)
	}
	done := f.finish()
	h := hostOf(done, "laptop")
	if done.State != wakerun.RunCancelled || h.State != wakerun.StSkipped || h.On != wakerun.OnUntouched ||
		!strings.Contains(h.Text, "Nothing was sent to it: it was not updated. It was already on and stays on.") {
		t.Fatalf("the cancelled run: %s, laptop %s / %s: %s", done.State, h.State, h.On, h.Text)
	}
	if got := f.requests(); len(got) != 0 {
		t.Fatalf("the run sent: %v", got)
	}
	// the page: the last run, with the machine's state and no wake
	page := e.do("GET", "/api/admin/wake-update", nil, "Cookie", admin)
	runs, _ := page.body["last"].([]any)
	if page.code != 200 || len(runs) != 1 {
		t.Fatalf("the page: %d %s", page.code, page.raw)
	}
	hosts, _ := runs[0].(map[string]any)["hosts"].([]any)
	if len(hosts) != 1 {
		t.Fatalf("the page: %d %s", page.code, page.raw)
	}
	row := hosts[0].(map[string]any)
	if row["state"] != "skipped" || row["on"] != "untouched" || row["wake_sent_at"] != nil {
		t.Fatalf("the machine on the page: %v", row)
	}
}
