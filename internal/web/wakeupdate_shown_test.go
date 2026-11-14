package web

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// A run uses the timings its start dialog showed. The dialog is drawn from one answer of the page's data and
// sends the stamps of that answer with the start; the panel starts nothing when the timings in force are
// other ones, and the lines of "Confirm it's you" name the two timings that decide what is switched off.

// shownBy is what a start dialog drawn from page sends as "timings" for the machines it lists.
func shownBy(t *testing.T, page resp, ids ...string) string {
	t.Helper()
	parts := []string{fmt.Sprint(page.body["timings_stamp"])}
	for _, id := range ids {
		parts = append(parts, fmt.Sprint(wuHostRow(t, page, id)["timings_stamp"]))
	}
	return strings.Join(parts, ".")
}

func TestARunStartsOnlyWithTheTimingsItsDialogShowed(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.optIn("pc-a", "pc-b")
	f.offline()
	get := func() resp { return e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess) }
	setOwn := func(id string, run adminstate.RunEdit) {
		t.Helper()
		if _, _, err := e.s.Poller.Admin.EditHostOptions("another admin", func(o *adminstate.HostOptions) error {
			he := o.Host(id)
			he.Run = run
			o.SetHost(id, he)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		e.s.Poller.Bump()
	}
	refused := func(what string, r resp) {
		t.Helper()
		if r.code != 409 || r.body["detail"] != wuTimingsNotShown {
			t.Fatalf("%s: %d %s", what, r.code, r.raw)
		}
		if act, hist := e.s.WakeRuns.Snapshot(); act != nil || len(hist) != 0 || len(f.requests()) != 0 {
			t.Fatalf("%s: a run was started, or something was sent", what)
		}
	}
	// the stamps: twelve hex digits each, the same for the same numbers, another one for another number
	page := get()
	stamp := regexp.MustCompile(`^[0-9a-f]{12}$`)
	pa, pb := fmt.Sprint(wuHostRow(t, page, "pc-a")["timings_stamp"]), fmt.Sprint(wuHostRow(t, page, "pc-b")["timings_stamp"])
	if panel := fmt.Sprint(page.body["timings_stamp"]); !stamp.MatchString(panel) || !stamp.MatchString(pa) || pa == pb || panel != fmt.Sprint(get().body["timings_stamp"]) {
		t.Fatalf("the page's stamps: %q, pc-a %q, pc-b %q", panel, pa, pb)
	}
	for _, tm := range config.RunTimings() { // every one of the six moves the page's stamp
		before := fmt.Sprint(get().body["timings_stamp"])
		e.rtSet(map[string]int{tm.Key: tm.Min + 1})
		if after := fmt.Sprint(get().body["timings_stamp"]); after == before {
			t.Errorf("%s changed and the page's stamp did not", tm.Key)
		}
		e.rtSet(map[string]int{tm.Key: 0})
	}

	// 1. the dialog is open (15 minutes, 2 minutes); another admin confirms 5 minutes and 10 s; Start: nothing
	// is started and nobody is asked to confirm
	page = get()
	shown := shownBy(t, page, "pc-a")
	e.rtSet(map[string]int{config.KeyOffAfter: 300, config.KeyShutdownDelay: 10})
	r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}, "timings": shown})
	refused("the panel's timings changed while the dialog was open", r)
	if st := e.auditEntries("", "wakeupdate.start"); len(st) != 1 || !strings.Contains(fmt.Sprint(st[0]["outcome"]), "error 409: the run timings are not the ones the start dialog showed; nothing was started") {
		t.Fatalf("the audit entry: %v", st)
	}
	// the dialog opened again: it is asked for, and the confirmation names the two timings in force
	page = get()
	r = e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}, "timings": shownBy(t, page, "pc-a")})
	d, _ := r.body["detail"].(map[string]any)
	if !stepUpRequired(r) || !slices.Equal(strs(d["items"]), []string{"PC-A: wake, update, switch off again (skipped if it turns out to be on)",
		"A machine counts as off after 5 minutes without an answer; shutdown countdown 10 seconds"}) {
		t.Fatalf("asked again: %d %s", r.code, r.raw)
	}
	// ... changed again before the confirmation: the replay starts nothing
	e.rtSet(map[string]int{config.KeyShutdownDelay: 20})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	res, _ := e.do("GET", "/api/me", nil, "Cookie", a.sess).body["pending_result"].(map[string]any)
	if res["ok"] != false || res["status"] != 409.0 || (res["message"] != wuTimingsChanged && res["message"] != wuTimingsNotShown) {
		t.Fatalf("changed between the click and the confirmation: %v", res)
	}
	e.rtSet(map[string]int{config.KeyOffAfter: 0, config.KeyShutdownDelay: 0})

	// 2. a listed machine's own timing changes while the dialog is open (Edit host)
	page = get()
	shown = shownBy(t, page, "pc-a", "pc-b")
	setOwn("pc-b", adminstate.RunEdit{ShutdownDelayS: 10})
	refused("a listed machine's own countdown changed", e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a", "pc-b"}, "timings": shown}))
	setOwn("pc-b", adminstate.RunEdit{OffAfterS: 300})
	refused("a listed machine's own off_after_s changed", e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}, "update_only": []string{"pc-b"}, "timings": shown}))
	// the confirmation says a machine's own two, where it has them
	page = get()
	r = e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a", "pc-b"}, "timings": shownBy(t, page, "pc-a", "pc-b")})
	d, _ = r.body["detail"].(map[string]any)
	if items := strs(d["items"]); !stepUpRequired(r) || len(items) != 4 || items[2] != "A machine counts as off after 15 minutes without an answer; shutdown countdown 2 minutes" ||
		items[3] != "PC-B, its own: counts as off after 5 minutes; shutdown countdown 2 minutes" {
		t.Fatalf("the lines with a machine's own timings: %d %v", r.code, d["items"])
	}
	// a machine that is not in the list changes its own: the dialog showed nothing about it, the start goes on
	shown = shownBy(t, page, "pc-a")
	setOwn("pc-b", adminstate.RunEdit{OffAfterS: 600, ShutdownDelayS: 45})
	if r = e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}, "timings": shown}); !stepUpRequired(r) {
		t.Fatalf("a change about a machine that is not listed: %d %s", r.code, r.raw)
	}

	// 3. what is not a stamp of the timings in force: the list's stamps in another order, one too few, one too
	// many, anything else
	page = get()
	good := shownBy(t, page, "pc-a", "pc-b")
	for what, bad := range map[string]string{"in another order": shownBy(t, page, "pc-b", "pc-a"), "one too few": shownBy(t, page, "pc-a"), "one too many": good + "." + pa,
		"the panel's alone": fmt.Sprint(page.body["timings_stamp"]), "a word": "now", "a dot": "."} {
		refused(what, e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a", "pc-b"}, "timings": bad}))
	}
	if r = e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a", "pc-b"}, "timings": 5}); r.code != 400 && r.code != 422 {
		t.Fatalf("a number for the stamps: %d %s", r.code, r.raw)
	}
	// with the dialog's stamps and nothing changed: asked, confirmed, started with the numbers in force
	r = e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}, "timings": shownBy(t, page, "pc-a")})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if act, _ := e.s.WakeRuns.Snapshot(); act == nil || act.Settings.OffAfterS != 900 || act.Settings.ShutdownDelayS != 120 {
		t.Fatalf("the run: %+v", act)
	}
	f.finish()
}

// The page sends the stamps of the answer its dialog was drawn from, for the machines it lists.
func TestUIStartDialogSendsWhatItShowed(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const wuShown = (d, body) => [d.timings_stamp, ...[...body.hosts, ...body.update_only].map((id) => d.hosts.find((h) => h.id === id)?.timings_stamp)].map((x) => x || "?").join(".");`,
		`    body.timings = wuShown(d, body);`,
	} {
		if strings.Count(js, want) != 1 {
			t.Errorf("app.js lacks (or has twice) %q", want)
		}
	}
	// d is the answer the dialog fetched when it opened and drew its numbers from, not the page's newest
	dlg := js[strings.Index(js, "async function openWakeUpdate() {"):]
	dlg = dlg[:strings.Index(dlg, "\n}\n")]
	if !strings.Contains(dlg, `try { d = await api("/api/admin/wake-update"); }`) || strings.Contains(dlg, "wuShown(wu.data") ||
		!strings.Contains(dlg, "${esc(fmtSpan(d.off_after_s))} without an answer") || !strings.Contains(dlg, "a countdown of ${esc(fmtSpan(d.settings.shutdown_delay_s))}") {
		t.Error("the start dialog does not send the stamps of the answer it drew its numbers from")
	}
}
