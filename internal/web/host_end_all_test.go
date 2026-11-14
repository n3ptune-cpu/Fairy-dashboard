package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/power"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Build 16 put three things about one added host into the panel, each from its own piece of work, and each
// with its own tests of what a Re-pair and a Remove of the host do with it:
//
//   - what "Edit host" changed for it: the name, the site, a link of a plug it relays (state.json and the
//     Users page's store);
//   - its own timings for "Update unpowered hosts", changed in "Edit host" (state.json, inside the same entry);
//   - the last 24 hours of the plug it relays (the graph: in memory and in power-day.json).
//
// They meet in one request. This is that request for a host that has all three:
//
//   - a new name and another site change nothing about the plug's day: it is kept under the source's id,
//     which is made of the host's id and the plug's, and neither is touched. The site decides who is given
//     the day, from the moment it is changed.
//   - Re-pair (new tokens, the same machine) keeps all three.
//   - Remove takes all three, at once and in the files, and nothing of another host.
func TestRepairKeepsAndRemoveTakesEverythingThePanelHoldsAboutAHost(t *testing.T) {
	cfg := hedConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := managed.NewStore(cfg.DataDir)
	if err := st.Put(managed.Host{ID: "box-m", Name: "Box M", URL: "http://box-m", Site: "site-a", OS: "linux", AddedAt: time.Now().UTC(),
		Options: managed.Options{PowerControl: true, SelfUpdate: true}}, managed.Secret{Token: strings.Repeat("m", 32), ControlToken: strings.Repeat("c", 32)}); err != nil {
		t.Fatal(err)
	}
	b := hedBackend()
	b.Metrics["box-m"] = func() (jsonx.M, error) { // it relays a plug of its own, "desk", 80 W
		m := testutil.AgentMetrics()
		m["hostname"] = "box-m"
		return m, nil
	}
	c := *cfg
	c.Agents, c.Systems, c.Proxmox = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems), slices.Clone(cfg.Proxmox)
	if _, skipped, err := st.Merge(&c); err != nil || len(skipped) != 0 {
		t.Fatal(err, skipped)
	}
	r := hedStart(t, &c, b)
	r.s.adminRL = newRateLimiter(1000)
	r.s.Enroll.Scripts, r.s.Enroll.PanelAddr, r.s.Enroll.Interval = testInstallers, "100.64.0.10", 10*time.Millisecond
	r.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
		return 200, enroll.Response{AgentVersion: "42-abc", Platform: "linux", Token: strings.Repeat("r", 32), ControlToken: strings.Repeat("d", 32)}, "", nil
	}
	hdr := append([]string{"Content-Type", "application/json"}, r.hdr...)
	admin := r.hdr[:2]
	viewer := func(sub, site string) []string {
		u := model.User{Sub: sub, Name: model.P(sub), Email: model.P(sub + "@example.com"), Groups: []string{"viewers"}}
		ck := r.cookie(u, 0)
		if _, _, err := r.s.Access.SetRole(sub, access.RoleViewer, []string{site}, "test", true); err != nil {
			t.Fatal(err)
		}
		return []string{"Cookie", ck}
	}
	ofA, ofB := viewer("u8", "site-a"), viewer("u9", "site-b")

	day := r.s.Poller.Power.Day
	file := filepath.Join(cfg.DataDir, power.DayFile)
	const own, other = "box-m/desk", "pc-a/desk"
	// an hour of readings of both plugs, so that there is a day to keep and a day to lose
	for m := 60; m >= 2; m-- {
		day.Record(time.Now().Add(-time.Duration(m)*time.Minute), []power.DaySample{{ID: own, W: 40}, {ID: other, W: 20}}, nil)
	}
	r.poll()
	start := day.Series(own, time.Now())
	if start.Summary == nil || start.Summary.CoveredS < 60*60 || !day.Has(other, time.Now()) {
		t.Fatalf("the rig: the day of the added host's plug: %+v; the other plug has one: %v", start.Summary, day.Has(other, time.Now()))
	}
	if a, bb := dayOf(r.env, own, ofA...), dayOf(r.env, own, ofB...); a.code != 200 || bb.code != 404 {
		t.Fatalf("the rig: box-m is of site-a: its plug's day for a viewer of site-a %d, of site-b %d", a.code, bb.code)
	}
	sameDay := func(when string) {
		t.Helper()
		now := day.Series(own, time.Now())
		if now.Summary == nil || now.Start != start.Start || now.Summary.CoveredS < start.Summary.CoveredS {
			t.Fatalf("%s: the day of %s: before %+v from %d, now %+v from %d", when, own, start.Summary, start.Start, now.Summary, now.Start)
		}
		if g := dayOf(r.env, own, admin...); g.code != 200 || g.body["source"] != own || g.body["start"] != float64(start.Start) {
			t.Fatalf("%s: the day as the page asks for it: %d %.300s", when, g.code, g.raw)
		}
	}

	// --- Edit host: a name, a site, a plug out of the total, and the machine's own "counts as off after"
	r.ok("box-m", map[string]any{"name": "Media box", "plugs": []any{map[string]any{"id": own, "in_total": false}}})
	r.poll()
	sameDay("after the new name")
	if got := r.row("box-m")["name"]; got != "Media box" {
		t.Fatalf("the row's name: %v", got)
	}
	r.ok("box-m", map[string]any{"site": "site-b"})
	r.poll()
	sameDay("after the other site")
	// the site decides who is given the day, from now on: the same day, for the other viewers
	if a, bb := dayOf(r.env, own, ofA...), dayOf(r.env, own, ofB...); a.code != 404 || bb.code != 200 || bb.body["start"] != float64(start.Start) {
		t.Errorf("after the site change: the day for a viewer of site-a %d, of site-b %d %.200s", a.code, bb.code, bb.raw)
	}
	if _, res := r.a.ehConfirmed("box-m", map[string]any{"wake_update": map[string]any{"off_after_s": 360}}); res["ok"] != true {
		t.Fatalf("the machine's own timing: %v", res)
	}
	held := func(when string) {
		t.Helper()
		if h := r.storedHost("box-m"); h["name"] != "Media box" || r.storedRun("box-m") != "off_after_s=360" {
			t.Fatalf("%s: what Edit host keeps for the machine itself: %v", when, h)
		}
		if a := r.storedAgent("box-m"); a == nil || !strings.Contains(fmt.Sprint(a["power_exclude_from_total"]), "desk:true") {
			t.Fatalf("%s: what Edit host keeps about its plug: %v", when, a)
		}
		if off, _ := r.s.Poller.WakeUpdateOverride("box-m"); off != 360 {
			t.Fatalf("%s: its own \"counts as off after\" in force: %d", when, off)
		}
		if got := r.s.Access.Load().Assign["box-m"]; got != "site-b" {
			t.Fatalf("%s: its site: %q", when, got)
		}
		if got := r.row("box-m")["name"]; got != "Media box" {
			t.Fatalf("%s: the row's name: %v", when, got)
		}
	}
	held("before the Re-pair")

	// --- Re-pair: new tokens for the same machine. Everything stays.
	p := r.do("POST", "/api/admin/hosts/box-m/repair", map[string]any{"confirm": "Box M"}, hdr...)
	if p.code != 201 {
		t.Fatal("Re-pair:", p.code, string(p.raw))
	}
	ended := false
	for i := 0; i < 300 && !ended; i++ {
		time.Sleep(10 * time.Millisecond)
		r.poll()
		g := r.do("GET", "/api/admin/enrollments/"+p.body["id"].(string), nil, admin...)
		ended = g.body["done"] == true && g.body["state"] == "online"
	}
	if !ended {
		t.Fatal("the Re-pair never ended online")
	}
	if sec, err := st.Secret("box-m"); err != nil || sec.Token != strings.Repeat("r", 32) {
		t.Fatalf("the rig: the Re-pair did not bring new tokens: %v", err)
	}
	r.poll()
	sameDay("after the Re-pair")
	held("after the Re-pair")
	if a, bb := dayOf(r.env, own, ofA...), dayOf(r.env, own, ofB...); a.code != 404 || bb.code != 200 {
		t.Errorf("after the Re-pair: the day for a viewer of site-a %d, of site-b %d", a.code, bb.code)
	}

	// --- Remove: the end of the host in the panel. Everything of it goes, nothing of another host.
	r.ok("pc-a", map[string]any{"name": "Gaming PC"})
	if rm := r.do("DELETE", "/api/admin/hosts/box-m", map[string]any{"confirm": "Box M"}, hdr...); rm.code != 200 {
		t.Fatal("Remove:", rm.code, string(rm.raw))
	}
	if day.Has(own, time.Now()) {
		t.Error("the day of the removed host's plug is still there")
	}
	if raw, err := os.ReadFile(file); err != nil || strings.Contains(string(raw), own) || !strings.Contains(string(raw), other) {
		t.Errorf("%s after the Remove (%v) names the removed host's plug, or not the other one: %.200s", power.DayFile, err, raw)
	}
	if g := dayOf(r.env, own, admin...); g.code != 404 {
		t.Errorf("the day of the removed host's plug, as the page asks for it: %d %.200s", g.code, g.raw)
	}
	if h := r.storedHost("box-m"); h != nil {
		t.Errorf("Edit host still keeps something for the removed machine itself (its name, its own run timings): %v", h)
	}
	if a := r.storedAgent("box-m"); a != nil {
		t.Errorf("Edit host still keeps something about the removed host's plug: %v", a)
	}
	if all := fmt.Sprint(r.storedOptions()); strings.Contains(all, "box-m") {
		t.Errorf("state.json still names the removed host under host_options: %s", all)
	}
	if off, delay := r.s.Poller.WakeUpdateOverride("box-m"); off != 0 || delay != 0 {
		t.Errorf("the removed machine's own run timings are still in force: %d, %d", off, delay)
	}
	if hosts, _ := st.Load(); len(hosts) != 0 {
		t.Errorf("the host is still in the list of added hosts: %v", hosts)
	}
	// the other host: its day, its name
	if !day.Has(other, time.Now()) || r.storedHost("pc-a")["name"] != "Gaming PC" {
		t.Errorf("the Remove of box-m took something of pc-a: its plug has a day %v, Edit host keeps %v", day.Has(other, time.Now()), r.storedHost("pc-a"))
	}
	// and a history tick that was on its way brings nothing back
	r.s.Poller.Power.SampleHistory()
	if day.Has(own, time.Now()) {
		t.Error("the day came back with the next history tick")
	}
	// the audit log has the Remove's own entries for what went: the Edit host changes, and the host
	var forgot map[string]any
	for _, en := range r.auditEntries("", "host.options") {
		if pr, _ := en["params"].(map[string]any); en["target"] == "box-m" && pr["because"] == "host removed" {
			forgot = pr
		}
	}
	if forgot == nil {
		t.Error("no audit entry host.options for what the Remove took out of Edit host")
	}
	if rm := r.auditEntries("", "host.remove"); len(rm) == 0 || rm[0]["target"] != "box-m" || rm[0]["outcome"] != "ok" {
		t.Errorf("audit, host.remove: %v", rm)
	}
}
