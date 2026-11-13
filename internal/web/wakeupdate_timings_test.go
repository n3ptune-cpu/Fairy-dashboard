package web

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Build 14: after how long without an answer a machine counts as off is a setting (wake_update.off_after_s),
// and it and the shutdown's countdown can be set for one machine (the wake_update: block of its systems:
// entry). Through the routes: the page says "off" by the machine's own time and has the machine's own numbers
// in its row (and only there, and only where they are not the panel's); a run wakes the machine by its own
// time and asks it for its own countdown.

// silent7 lets seven minutes pass in which the farm's machines that are off give nothing.
func silent7(f *farm) {
	for i := 0; i < 90; i++ {
		f.tick()
	}
	f.wait(7*time.Minute - 90*time.Second)
}

func TestWakeUpdateAMachinesOwnTimings(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	e.s.Poller.Override("pc-a").WakeUpdate = &config.SystemWakeUpdate{OffAfterS: 360, ShutdownDelayS: 30} // systems: - {id: pc-a, wol: ..., wake_update: {...}}
	f.optIn("pc-a", "pc-b")
	silent7(f)
	page := e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
	if page.code != 200 || page.body["off_after_s"] != 900.0 {
		t.Fatalf("the page: %d, off_after_s %v", page.code, page.body["off_after_s"])
	}
	if s := page.body["settings"].(map[string]any); s["shutdown_delay_s"] != 120.0 || s["off_after_s"] != 900.0 {
		t.Fatalf("the panel's settings: %v", s)
	}
	pa, pb := wuHostRow(t, page, "pc-a"), wuHostRow(t, page, "pc-b")
	// seven minutes of silence: off for the machine that counts as off after six, "not sure" for the other
	if pa["power"] != "off" || pa["choice"] != "wake" || pa["off_after_s"] != 360.0 || pa["shutdown_delay_s"] != 30.0 || pa["not_sure"] != nil {
		t.Fatalf("pc-a (off_after_s 360, shutdown_delay_s 30): %v", pa)
	}
	if pb["power"] != "unclear" || pb["choice"] != "" || pb["not_sure"] != true || pb["off_after_s"] != nil || pb["shutdown_delay_s"] != nil ||
		pb["why"] != "not sure that it is off: no answer for 7 minutes so far, and Fairy takes a machine for off only after 15 minutes (it may be restarting)" {
		t.Fatalf("pc-b (the panel's timings): %v", pb)
	}
	// a run for both: pc-b is not sure and is sent nothing; pc-a is woken, updated and asked for ITS countdown
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if path == "/power" { // the agent's own answer: the countdown it scheduled
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(f.now.Add(30 * time.Second).Unix()), "delay_s": 30.0, "requested_by": "x"}}, nil
		}
		return 0, nil, nil
	}
	r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a", "pc-b"}})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code, string(r.raw))
	}
	run := f.finish()
	ha, hb := hostOf(run, "pc-a"), hostOf(run, "pc-b")
	if ha.State != wakerun.StOff || !strings.Contains(ha.Text, "and switched off again.") {
		t.Fatalf("pc-a: %s: %s", ha.State, ha.Text)
	}
	if hb.State != wakerun.StSkipped || !strings.Contains(hb.Text, "Fairy takes a machine for off only after 15 min without an answer") {
		t.Fatalf("pc-b: %s: %s", hb.State, hb.Text)
	}
	if got := strings.Join(f.requests(), ", "); got != "nas /wol, pc-a /update, pc-a /power" {
		t.Fatalf("what the agents were sent: %s", got)
	}
	e.b.Mu.Lock()
	all := strings.Join(e.b.Requests, "\n")
	e.b.Mu.Unlock()
	if !strings.Contains(all, "pc-a POST /power map[action:shutdown delay_s:30 requested_by:") || strings.Contains(all, "delay_s:120") {
		t.Fatalf("pc-a was not asked for its own countdown of 30 s:\n%s", all)
	}
	// the report: the machine's own numbers are in its entry, and only there
	if ha.OffAfterS == nil || *ha.OffAfterS != 360 || ha.ShutdownDelayS == nil || *ha.ShutdownDelayS != 30 || hb.OffAfterS != nil || hb.ShutdownDelayS != nil {
		t.Fatalf("the report: pc-a %v %v, pc-b %v %v", ha.OffAfterS, ha.ShutdownDelayS, hb.OffAfterS, hb.ShutdownDelayS)
	}
	last := e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess).body["last"].([]any)[0].(map[string]any)
	for _, x := range last["hosts"].([]any) {
		h := x.(map[string]any)
		if (h["id"] == "pc-a") != (h["off_after_s"] == 360.0 && h["shutdown_delay_s"] == 30.0) || (h["id"] == "pc-b" && (h["off_after_s"] != nil || h["shutdown_delay_s"] != nil)) {
			t.Errorf("the page's \"Last run\": %v", h)
		}
	}
}

// The panel's own setting (wake_update.off_after_s: 360): the page gives it as off_after_s, every machine
// without a time of its own counts as off after six minutes, and one with its own, longer time does not; its
// row and its "not sure" sentence have its own number.
func TestWakeUpdateOffAfterIsThePanelsSetting(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	e.s.WakeRuns.Settings = e.s.WakeRuns.Settings.WithOffAfter(360) // what New does with wake_update.off_after_s
	e.s.Poller.Override("pc-b").WakeUpdate = &config.SystemWakeUpdate{OffAfterS: 1800}
	f.optIn("pc-a", "pc-b")
	silent7(f)
	page := e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
	if page.body["off_after_s"] != 360.0 || page.body["settings"].(map[string]any)["off_after_s"] != 360.0 {
		t.Fatalf("the page's off_after_s: %v / %v", page.body["off_after_s"], page.body["settings"])
	}
	if pa := wuHostRow(t, page, "pc-a"); pa["power"] != "off" || pa["choice"] != "wake" || pa["off_after_s"] != nil {
		t.Fatalf("pc-a (the panel's 360 s): %v", pa)
	}
	if pb := wuHostRow(t, page, "pc-b"); pb["power"] != "unclear" || pb["off_after_s"] != 1800.0 ||
		pb["why"] != "not sure that it is off: no answer for 7 minutes so far, and Fairy takes a machine for off only after 30 minutes (it may be restarting)" {
		t.Fatalf("pc-b (its own 1800 s): %v", pb)
	}
	// a machine whose own time is the panel's has nothing of its own in its row
	e.s.Poller.Override("pc-b").WakeUpdate = &config.SystemWakeUpdate{OffAfterS: 360, ShutdownDelayS: 120}
	if pb := wuHostRow(t, e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess), "pc-b"); pb["power"] != "off" || pb["off_after_s"] != nil || pb["shutdown_delay_s"] != nil {
		t.Fatalf("pc-b (its own values are the panel's): %v", pb)
	}
}

// The configuration's wake_update.off_after_s reaches the run manager (web.New).
func TestWakeUpdateOffAfterComesFromTheConfiguration(t *testing.T) {
	e := setupSystems(t, nil)
	if s := e.s.WakeRuns.Settings; s.OffAfter != 15*time.Minute || s.OffAfterS != 900 {
		t.Fatalf("without the key: %+v", s)
	}
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	cfg.WakeUpdate.OffAfterS = 360
	s := New(cfg, e.s.Secrets, e.s.Poller, nil, "test")
	if got := s.WakeRuns.Settings; got.OffAfter != 6*time.Minute || got.OffAfterS != 360 || got.ShutdownDelayS != 120 {
		t.Fatalf("wake_update.off_after_s: 360: %+v", got)
	}
}

// The machine's own timings follow it when its row changes its id (a standalone Proxmox host: shown under its
// agent's id while it is off after a restart of the panel, under its node's id once it runs). The block is in
// the entry under the node's id, where the wol: is. The page has the numbers under the id the machine is shown
// under, before and after; and a run that starts under the agent's id and goes on under the node's asks for
// the machine's own countdown.
func TestWakeUpdateOwnTimingsFollowTheMachineAcrossItsIDs(t *testing.T) {
	h := setupNodeOff(t, testutil.TwoHostsConfig(t, "    wake_update: {off_after_s: 360, shutdown_delay_s: 30}\n"),
		&twoHosts{agent: "lab-b-agent", node: "lab-b-node-pve-b", cluster: "lab-b", name: "PVE-B", others: map[string]string{"lab-a": "PVE-A"}}, "lab-a-agent")
	e := h.e
	admin := e.cookie(controller(), 0)
	row := func(id string) map[string]any {
		t.Helper()
		return wuHostRow(t, e.do("GET", "/api/admin/wake-update", nil, "Cookie", admin), id)
	}
	h.f.optIn("lab-b-agent")
	silent7(h.f)
	if got := h.shown(); got != "lab-b-agent" {
		t.Fatalf("shown as %s", got)
	}
	if r := row("lab-b-agent"); r["power"] != "off" || r["choice"] != "wake" || r["off_after_s"] != 360.0 || r["shutdown_delay_s"] != 30.0 {
		t.Fatalf("shown under its agent's id, seven minutes silent: %v", r)
	}
	if r := row("lab-a-node-pve-a"); r["off_after_s"] != nil || r["shutdown_delay_s"] != nil { // the other host has none
		t.Fatalf("PVE-A: %v", r)
	}
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if path == "/power" {
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(h.f.now.Add(30 * time.Second).Unix()), "delay_s": 30.0, "requested_by": "x"}}, nil
		}
		return 0, nil, nil
	}
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"lab-b-agent"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if act, _ := e.s.WakeRuns.Snapshot(); act == nil || hostOf(act, "lab-b-agent").WakeSentAt != nil {
			break
		}
		h.f.tick()
	}
	host, _ := h.end()
	if got := h.shown(); got != "lab-b-node-pve-b" {
		t.Fatalf("after the run PVE-B is shown as %s", got)
	}
	if host.State != wakerun.StOff || host.ShutdownDelayS == nil || *host.ShutdownDelayS != 30 || host.OffAfterS == nil || *host.OffAfterS != 360 {
		t.Fatalf("the run: %s (%v, %v): %s", host.State, host.OffAfterS, host.ShutdownDelayS, host.Text)
	}
	e.b.Mu.Lock()
	all := strings.Join(e.b.Requests, "\n")
	e.b.Mu.Unlock()
	if !strings.Contains(all, "lab-b-agent POST /power map[action:shutdown delay_s:30 requested_by:") || strings.Contains(all, "delay_s:120") {
		t.Fatalf("the machine was not asked for its own countdown:\n%s", all)
	}
	// shown under its node's id now: the same numbers
	if r := row("lab-b-node-pve-b"); r["off_after_s"] != 360.0 || r["shutdown_delay_s"] != 30.0 {
		t.Fatalf("shown under its node's id: %v", r)
	}
}
