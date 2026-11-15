package web

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// "Edit host": what is confirmed is what is saved. A request that is kept for "Confirm it's you" carries
// the lines its confirmation shows. The panel's replay makes the lines again from what is stored then, and
// saves only when they are the same: otherwise 409, nothing is saved, and the audit log has the refusal with
// the lines that were confirmed.

func hoRefused(res map[string]any) bool {
	return res["ok"] == false && res["status"] == 409.0 && res["message"] == hoNotShown
}

// hoRefusal checks the newest audit entry of the machine: the refusal, with exactly the confirmed lines.
func hoRefusal(t *testing.T, e *env, id string, items []string, what string) {
	t.Helper()
	for _, x := range e.auditEntries("", "host.options") {
		if x["target"] != id {
			continue
		}
		p, _ := x["params"].(map[string]any)
		if !strings.HasPrefix(fmt.Sprint(x["outcome"]), "error 409: the host was changed after the confirmation was asked for; nothing was saved") ||
			!slices.Equal(strs(p["confirmed"]), items) || p["lines"] != nil || p["changes"] != nil {
			t.Errorf("%s: the newest audit entry: %v", what, x)
		}
		return
	}
	t.Errorf("%s: no audit entry", what)
}

// A machine's own run timing. A Reset says "back to Run timings: 900 s" (or 120 s) with the sentence about
// what that means. Another admin changes the panel's timing before the confirmation: the machine would end
// with a number the confirmation never showed. Nothing is saved; the machine keeps its own number.
func TestEditHostAConfirmedResetSavesOnlyWhatItShowed(t *testing.T) {
	for _, c := range []struct {
		what   string
		own    adminstate.RunEdit
		field  string
		line   string
		panel  map[string]int
		stored string
		api    bool // the other admin's change goes through the API, with its own confirmation
	}{
		{"off-after, the other admin through the API", adminstate.RunEdit{OffAfterS: 1800}, config.KeyOffAfter,
			"Counts as off after: back to Run timings: 900 s = 15 minutes (was 1800 s = 30 minutes)", map[string]int{config.KeyOffAfter: 300}, "off_after_s=1800", true},
		{"off-after", adminstate.RunEdit{OffAfterS: 1800}, config.KeyOffAfter,
			"Counts as off after: back to Run timings: 900 s = 15 minutes (was 1800 s = 30 minutes)", map[string]int{config.KeyOffAfter: 300}, "off_after_s=1800", false},
		{"the countdown", adminstate.RunEdit{ShutdownDelayS: 300}, config.KeyShutdownDelay,
			"Shutdown countdown: back to Run timings: 120 s = 2 minutes (was 300 s = 5 minutes)", map[string]int{config.KeyShutdownDelay: 10}, "shutdown_delay_s=300", false},
	} {
		a := signedIn(t)
		e := a.e
		newFarm(t, e)
		e.s.control, e.s.login = newRateLimiter(1000), newRateLimiter(1000)
		b := a.secondSession()
		body := map[string]any{}
		for _, tm := range config.MachineRunTimings() {
			if v := c.own.Value(tm.Key); v != 0 {
				body[tm.Key] = v
			}
		}
		if _, res := a.ehConfirmed("pc-a", map[string]any{"wake_update": body}); res["ok"] != true || e.storedRun("pc-a") != c.stored {
			t.Fatalf("%s: the machine's own number: %v, stored %q", c.what, res, e.storedRun("pc-a"))
		}
		how, r := a.ehAsk("pc-a", map[string]any{"reset": []any{map[string]any{"field": c.field}}})
		d, _ := r.body["detail"].(map[string]any)
		items := strs(d["items"])
		if how != "each" || len(items) != 2 || items[0] != c.line {
			t.Fatalf("%s: the Reset was answered %q with %q", c.what, how, items)
		}
		if c.api {
			b.rtConfirmed(map[string]any{"off_after_s": 300})
		} else {
			e.rtSet(c.panel)
		}
		if cb := a.confirm(pendingID(r), false); cb.code != 303 {
			t.Fatal(c.what, cb.code)
		}
		if res := a.rtResult(); !hoRefused(res) {
			t.Errorf("%s: a Reset confirmed for %q was answered %v", c.what, items[0], res)
		}
		if got := e.storedRun("pc-a"); got != c.stored {
			t.Errorf("%s: stored %q, want %q (nothing saved)", c.what, got, c.stored)
		}
		if row := a.ehRun("pc-a", c.field); row["value"] != float64(c.own.Value(c.field)) || row["from"] != "panel" {
			t.Errorf("%s: the dialog afterwards: %v", c.what, row)
		}
		hoRefusal(t, e, "pc-a", items, c.what)
		// asked again, the dialog says the number that counts now, and that is what is saved
		items, res := a.ehConfirmed("pc-a", map[string]any{"reset": []any{map[string]any{"field": c.field}}})
		if res["ok"] != true || e.storedRun("pc-a") != "" || len(items) != 2 || !strings.Contains(items[0], fmt.Sprintf(": back to Run timings: %d s", c.panel[c.field])) {
			t.Errorf("%s: asked again: confirmed %q, answered %v, stored %q", c.what, items, res, e.storedRun("pc-a"))
		}
	}
}

// A typed number is saved as its confirmation showed it: with nothing in between, and with a change in
// between that none of its lines is about. When the value its line calls "was" is another one by then, it is
// not saved either. And when the first line is still true and a later one is not, nothing of the request is
// saved, the first line's change included.
func TestEditHostATypedRunTimingIsSavedAsShownOrNotAtAll(t *testing.T) {
	a := signedIn(t)
	e := a.e
	newFarm(t, e)
	e.s.control, e.s.login = newRateLimiter(1000), newRateLimiter(1000)
	kept := func(body map[string]any) (string, []string) {
		t.Helper()
		how, r := a.ehAsk("pc-a", body)
		d, _ := r.body["detail"].(map[string]any)
		if how != "each" || len(strs(d["items"])) == 0 {
			t.Fatalf("%v was answered %q: %s", body, how, r.raw)
		}
		return pendingID(r), strs(d["items"])
	}
	lastLines := func() []string {
		for _, x := range e.auditEntries("", "host.options") {
			if p, _ := x["params"].(map[string]any); x["target"] == "pc-a" && x["outcome"] == "ok" {
				return strs(p["lines"])
			}
		}
		return nil
	}
	// nothing in between
	id, items := kept(map[string]any{"wake_update": map[string]any{"off_after_s": 600}})
	if cb := a.confirm(id, false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if res := a.rtResult(); res["ok"] != true || e.storedRun("pc-a") != "off_after_s=600" || !slices.Equal(lastLines(), items) {
		t.Fatalf("a typed time: %v, stored %q, audit %q, confirmed %q", res, e.storedRun("pc-a"), lastLines(), items)
	}
	// a change in between that no line is about: the panel's countdown, and another machine's own time
	id, items = kept(map[string]any{"wake_update": map[string]any{"off_after_s": 1200}})
	e.rtSet(map[string]int{config.KeyShutdownDelay: 30, config.KeySettle: 20})
	if _, _, err := e.s.Poller.Admin.EditHostOptions("other admin", func(o *adminstate.HostOptions) error {
		o.SetHost("pc-b", adminstate.HostEdit{Run: adminstate.RunEdit{OffAfterS: 300}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if cb := a.confirm(id, false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if res := a.rtResult(); res["ok"] != true || e.storedRun("pc-a") != "off_after_s=1200" || !slices.Equal(lastLines(), items) {
		t.Fatalf("a typed time with an unrelated change in between: %v, stored %q, audit %q, confirmed %q", res, e.storedRun("pc-a"), lastLines(), items)
	}
	// the countdown's line says "was 30 s"; by the confirmation the panel's countdown is 3600 s
	id, items = kept(map[string]any{"wake_update": map[string]any{"shutdown_delay_s": 60}})
	if items[0] != "Shutdown countdown: 60 s = 1 minute (was 30 s)" {
		t.Fatalf("the rig: %q", items)
	}
	e.rtSet(map[string]int{config.KeyShutdownDelay: 3600})
	if cb := a.confirm(id, false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if res := a.rtResult(); !hoRefused(res) || e.storedRun("pc-a") != "off_after_s=1200" {
		t.Errorf("a countdown shortened from an hour on a confirmation that said \"was 30 s\": %v, stored %q", res, e.storedRun("pc-a"))
	}
	hoRefusal(t, e, "pc-a", items, "a typed countdown")
	// the first line is still true, a later one is not: a new name next to a Reset of the time
	id, items = kept(map[string]any{"name": "Den", "reset": []any{map[string]any{"field": "off_after_s"}}})
	if len(items) != 3 || items[0] != "Name: Den (was PC-A)" || items[1] != "Counts as off after: back to Run timings: 900 s = 15 minutes (was 1200 s = 20 minutes)" {
		t.Fatalf("the rig: %q", items)
	}
	e.rtSet(map[string]int{config.KeyOffAfter: 300})
	if cb := a.confirm(id, false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if res := a.rtResult(); !hoRefused(res) || e.storedRun("pc-a") != "off_after_s=1200" || e.storedHost("pc-a")["name"] != nil {
		t.Errorf("a name and a Reset whose line is no longer true: %v, stored %v", res, e.storedHost("pc-a"))
	}
	hoRefusal(t, e, "pc-a", items, "a name and a Reset")
}

// The same for every other part of a request: a name, a site and a Wake-on-LAN detail. The other admin's
// change stays as they left it.
func TestEditHostAConfirmationSavesNoNameSiteOrWakeOnLANItDidNotShow(t *testing.T) {
	type rig struct {
		*hedRig
		id    string
		items []string
	}
	keep := func(t *testing.T, host string, body map[string]any) *rig {
		t.Helper()
		r := &rig{hedRig: hedSetup(t)}
		p := r.do("PUT", "/api/admin/hosts/"+host+"/options", body, "Content-Type", "application/json", "Cookie", r.a.sess, "X-Requested-With", "fairy")
		d, _ := p.body["detail"].(map[string]any)
		if r.id, r.items = pendingID(p), strs(d["items"]); !stepUpRequired(p) || d["pending"] != true || len(r.items) == 0 {
			t.Fatalf("PUT options of %s %v: not kept with its lines: %d %s", host, body, p.code, p.raw)
		}
		return r
	}
	confirm := func(t *testing.T, r *rig) map[string]any {
		t.Helper()
		if cb := r.a.confirm(r.id, false); cb.code != 303 {
			t.Fatalf("the confirmation: %d %s", cb.code, cb.raw)
		}
		return r.a.rtResult()
	}
	rename := func(t *testing.T, r *rig, id, name string) { // another admin's change, written to the store as a save writes it
		t.Helper()
		if _, _, err := r.s.Poller.Admin.EditHostOptions("other admin", func(o *adminstate.HostOptions) error {
			e := o.Hosts[id]
			e.Name = name
			o.SetHost(id, e)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		r.s.Poller.Bump()
	}

	// a name: "Den (was PC-B)", and the host is called Attic by then
	r := keep(t, "pc-b", map[string]any{"name": "Den"})
	if !slices.Equal(r.items, []string{"Name: Den (was PC-B)"}) {
		t.Fatalf("the rig: %q", r.items)
	}
	r.ok("pc-b", map[string]any{"name": "Attic"})
	if res := confirm(t, r); !hoRefused(res) || r.row("pc-b")["name"] != "Attic" {
		t.Errorf("a name confirmed as \"was PC-B\" over another admin's name: %v, called %v", res, r.row("pc-b")["name"])
	}
	hoRefusal(t, r.env, "pc-b", r.items, "a name")

	// ... while a change of another host in between is nobody's business: saved as shown
	r = keep(t, "pc-b", map[string]any{"name": "Den"})
	r.ok("pc-a", map[string]any{"name": "Loft"})
	if res := confirm(t, r); res["ok"] != true || r.row("pc-b")["name"] != "Den" {
		t.Errorf("a name with another host renamed in between: %v, called %v", res, r.row("pc-b")["name"])
	}
	if _, p := r.lastChange("pc-b"); !slices.Equal(strs(p["lines"]), r.items) {
		t.Errorf("the audit entry of a saved name: %v, confirmed %q", p["lines"], r.items)
	}

	// a site: "site-a (was none)", and the Users page gave the host to site-b by then
	r = keep(t, "pc-b", map[string]any{"site": "site-a"})
	if r.items[0] != "Site: site-a (was none (admins only))" {
		t.Fatalf("the rig: %q", r.items)
	}
	if _, err := r.s.Access.Assign("pc-b", "site-b"); err != nil {
		t.Fatal(err)
	}
	begun := 0 // it is refused on the look at the request: no write is begun, the site's file is not touched
	r.s.hostEditStep = func(step string) { begun++ }
	if res := confirm(t, r); !hoRefused(res) || begun != 0 {
		t.Errorf("a site confirmed as \"was none\" over another admin's site: %v (%d steps of a write)", res, begun)
	}
	r.s.hostEditStep = nil
	if got := r.s.Access.Load().Assign; len(got) != 1 || got["pc-b"] != "site-b" || len(r.auditEntries("", "site.assign")) != 0 {
		t.Errorf("their site is not what is stored: %v (site.assign entries: %d)", got, len(r.auditEntries("", "site.assign")))
	}
	hoRefusal(t, r.env, "pc-b", r.items, "a site")

	// the first line (the site) is still true, the second (the name) is not: nothing is saved, the site neither
	r = keep(t, "pc-b", map[string]any{"site": "site-a", "name": "Den"})
	if len(r.items) < 2 || r.items[0] != "Site: site-a (was none (admins only))" || r.items[1] != "Name: Den (was PC-B)" {
		t.Fatalf("the rig: %q", r.items)
	}
	rename(t, r, "pc-b", "Attic")
	if res := confirm(t, r); !hoRefused(res) || len(r.s.Access.Load().Assign) != 0 || r.row("pc-b")["name"] != "Attic" {
		t.Errorf("a site and a name whose line is no longer true: %v, sites %v, called %v", res, r.s.Access.Load().Assign, r.row("pc-b")["name"])
	}
	hoRefusal(t, r.env, "pc-b", r.items, "a site and a name")

	// ... and when the name changes in the instant between the look at the request and its locked write: the
	// site was stored a moment before, and is put back
	r = keep(t, "pc-b", map[string]any{"site": "site-a", "name": "Den"})
	steps := 0
	r.s.hostEditStep = func(step string) {
		if step == "planned" {
			steps++
			rename(t, r, "pc-b", "Attic")
		}
	}
	if res := confirm(t, r); steps != 1 || !hoRefused(res) || len(r.s.Access.Load().Assign) != 0 || r.row("pc-b")["name"] != "Attic" {
		t.Errorf("a name changed in the instant before the write (%d): %v, sites %v, called %v", steps, res, r.s.Access.Load().Assign, r.row("pc-b")["name"])
	}
	if e, p := r.lastChange("pc-b"); p["site_not_put_back"] != nil || !strings.HasPrefix(fmt.Sprint(e["outcome"]), "error 409: the host was changed after the confirmation") {
		t.Errorf("the audit entry: %v", e)
	}
	if l := r.auditEntries("", "site.assign"); len(l) != 0 {
		t.Errorf("a site change that was not made is in the audit log: %v", l)
	}

	// Wake-on-LAN: "port 7 (was 9)", and another admin decided the port by then
	r = keep(t, "pc-a", map[string]any{"wol": map[string]any{"port": 7}})
	if _, _, err := r.s.Poller.Admin.EditHostOptions("other admin", func(o *adminstate.HostOptions) error {
		o.SetHost("pc-a", adminstate.HostEdit{Wol: &adminstate.WolEdit{MACs: []string{"02:00:00:00:00:a7"}, Port: 7}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.s.Poller.Bump()
	if res := confirm(t, r); !hoRefused(res) {
		t.Errorf("a Wake-on-LAN change over another admin's: %v (confirmed %q)", res, r.items)
	}
	if w := r.s.Poller.PowerOverride("pc-a").Wol; w == nil || !slices.Equal(w.AllMACs(), []string{"02:00:00:00:00:a7"}) || w.Port != 7 {
		t.Errorf("their Wake-on-LAN is not what counts: %+v", w)
	}
	hoRefusal(t, r.env, "pc-a", r.items, "Wake-on-LAN")
}

// The README says the rule and quotes the answer as the panel gives it.
func TestREADMESaysEditHostSavesWhatWasConfirmed(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := strings.Join(strings.Fields(string(b)), " ")
	for _, want := range []string{
		"**What you confirm is what is saved.** When a save asks you to confirm it's you, the confirmation lists every line it will change, with the value before and after.",
		"nothing is saved (\"" + hoNotShown[:strings.Index(hoNotShown, ", so what you confirmed")] + " ...\"): open Edit host again and press Save.",
		"A Reset that said \"back to Run timings: 900 s\" never leaves the machine with another number.",
		"its site is taken out again, unless somebody gave the host another site in that moment (theirs stays).",
		"\"" + hoSiteStays[:strings.Index(hoSiteStays, " Open Edit host")] + "\"",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("the README lacks %q", want)
		}
	}
	if !strings.HasSuffix(hoNotShown, "Nothing was saved: open Edit host again and press Save.") {
		t.Errorf("the answer does not end with what to do: %q", hoNotShown)
	}
}
