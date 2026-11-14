package web

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// "Edit host", the site, and what a save does when something else happens in the same moment (R6, N2).

// A site changed on the Users page (or in another Edit host) between the moment this request was looked at
// and the moment it is stored is somebody else's change: it is put back as THEY left it, and this request is
// not made. It used to be overwritten, and on a failure "put back" to the value this request had read.
func TestEditHostSiteChangedBySomebodyElseInThatMoment(t *testing.T) {
	r := hedSetup(t)
	done := false
	r.s.hostEditStep = func(step string) {
		if step == "planned" && !done {
			done = true
			if _, err := r.s.Access.Assign("pc-b", "site-b"); err != nil {
				t.Error(err)
			}
		}
	}
	p := r.put("pc-b", map[string]any{"site": "site-a", "name": "Den"})
	if !done || p.code != 409 || !strings.Contains(string(p.raw), "the site of this host was changed by somebody else just now") {
		t.Fatalf("a save over somebody else's site change: %d %s", p.code, p.raw)
	}
	if got := r.s.Access.Load().Assign; len(got) != 1 || got["pc-b"] != "site-b" {
		t.Errorf("their change is not what is stored: %v", got)
	}
	if r.storedOptions() != nil || r.row("pc-b")["name"] != "PC-B" {
		t.Errorf("the rest of the request was made: %v", r.storedOptions())
	}
	if e, _ := r.lastChange("pc-b"); !strings.HasPrefix(fmt.Sprint(e["outcome"]), "error 409") {
		t.Errorf("audit: %v", e)
	}
	if l := r.auditEntries("", "site.assign"); len(l) != 0 {
		t.Errorf("a site change that was not made is in the audit log: %v", l)
	}
	// the dialog opened again shows their value, and the save goes through
	if s := r.host("pc-b", "site"); s["value"] != "site-b" || s["from"] != "panel" {
		t.Errorf("the dialog after it: %v", s)
	}
	if p := r.ok("pc-b", map[string]any{"site": "site-a"}); !slices.Equal(strs(p.body["changed"]), []string{"Site: site-a (was site-b)"}) {
		t.Errorf("the next save: %s", p.raw)
	}
}

// A change that is stored is in the audit log, whatever happens to the host right after: the entry is written
// before the machine is looked at again for the answer. A host removed in that moment used to turn a saved
// change into an error without an entry.
func TestEditHostChangeIsOnRecordWhenTheHostGoesInThatMoment(t *testing.T) {
	r := hedSetup(t)
	done := false
	r.s.hostEditStep = func(step string) {
		if step == "stored" && !done {
			done = true
			r.s.Poller.RemoveAgent("pc-b")
		}
	}
	p := r.put("pc-b", map[string]any{"name": "Den", "site": "site-a"})
	if !done || p.code != 200 || p.body["message"] != "changes saved" || !slices.Equal(strs(p.body["changed"]), []string{"Site: site-a (was none (admins only))", "Name: Den (was PC-B)"}) {
		t.Fatalf("the answer for a change that was stored: %d %s", p.code, p.raw)
	}
	if g := r.do("GET", "/api/admin/hosts/pc-b/options", nil, r.hdr[:2]...); g.code != 404 {
		t.Fatalf("the rig: pc-b is gone: %d", g.code)
	}
	var mine map[string]any
	for _, e := range r.auditEntries("", "host.options") {
		if e["target"] == "pc-b" {
			mine = e
			break
		}
	}
	params, _ := mine["params"].(map[string]any)
	if mine == nil || mine["outcome"] != "ok" || changeOf(params, "name") == nil || changeOf(params, "site") == nil || params["machine"] != "PC-B" {
		t.Errorf("audit: %v", mine)
	}
	if l := r.auditEntries("", "site.assign"); len(l) != 1 || l[0]["target"] != "pc-b" {
		t.Errorf("audit, the site: %v", l)
	}
}

// A site changed in "Edit host" is in the audit log twice: as a field of the host.options entry (with the
// rest of that save), and as the entry the Users page writes for a site change, site.assign with the same
// target and params, so that one search finds every change of a site.
func TestEditHostSiteChangeIsAuditedLikeTheUsersPages(t *testing.T) {
	r := hedSetup(t)
	last := func() map[string]any {
		t.Helper()
		l := r.auditEntries("", "site.assign")
		if len(l) == 0 {
			t.Fatal("no audit entry site.assign")
		}
		return l[0]
	}
	// the Users page's own entry, for the shape
	if p := r.do("PUT", "/api/admin/assign", map[string]any{"target": "pc-b", "site": "site-a"}, append([]string{"Content-Type", "application/json"}, r.hdr...)...); p.code != 200 {
		t.Fatalf("the Users page: %d %s", p.code, p.raw)
	}
	users := last()
	up, _ := users["params"].(map[string]any)
	if users["target"] != "pc-b" || up["site"] != "site-a" || up["before"] != "" || len(up) != 2 {
		t.Fatalf("the Users page's entry: %v", users)
	}
	r.ok("pc-b", map[string]any{"site": "site-b", "name": "Den"})
	e := last()
	p, _ := e["params"].(map[string]any)
	if e["target"] != "pc-b" || e["outcome"] != "ok" || e["actor"] != users["actor"] || p["site"] != "site-b" || p["before"] != "site-a" || p["via"] != "Edit host" || len(p) != 3 {
		t.Errorf("the entry of Edit host: %v", e)
	}
	_, hp := r.lastChange("pc-b")
	if c := changeOf(hp, "site"); c == nil || c["before"] != "site-a" || c["after"] != "site-b" || changeOf(hp, "name") == nil {
		t.Errorf("the host.options entry of the same save: %v", hp)
	}
	// back to the default: "" like on the Users page
	r.ok("pc-b", map[string]any{"site": ""})
	if e, p := last(), last()["params"].(map[string]any); e["target"] != "pc-b" || p["site"] != "" || p["before"] != "site-b" {
		t.Errorf("back to the default: %v", e)
	}
	// nothing changed, nothing written: the same site again, and a save without a site
	n := len(r.auditEntries("", "site.assign"))
	r.ok("pc-b", map[string]any{"site": ""})
	r.ok("pc-b", map[string]any{"name": "Attic"})
	if got := len(r.auditEntries("", "site.assign")); got != n {
		t.Errorf("%d entries site.assign for saves that changed no site", got-n)
	}
}
