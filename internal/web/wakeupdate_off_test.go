package web

import (
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// What "off" is for a run, through the real poller and the real routes (review finding W1). A machine that has
// not answered for a minute and a half is "offline" on every page, and for a run it is "not sure": it may be
// restarting. The page does not offer it, and a run that names it anyway sends nothing to it: the panel
// decides, not the list a client sends. After a quarter of an hour without an answer the same machine is off:
// the page offers it, and a run wakes it, updates it and switches it off again.
func TestWakeUpdateOffIsAQuarterOfAnHourWithoutAnAnswer(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	for i := 0; i < 90; i++ {
		f.tick()
	}
	f.optIn("pc-a", "pc-b")
	get := func() resp {
		r := e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
		if r.code != 200 {
			t.Fatal(r.code, string(r.raw))
		}
		return r
	}
	if _, sys, _ := e.s.Poller.AgentSeen("pc-a"); sys.Status != "offline" {
		t.Fatalf("after a minute and a half the panel shows it as %q", sys.Status)
	}
	h := wuHostRow(t, get(), "pc-a")
	if h["power"] != "unclear" || h["choice"] != "" || h["not_sure"] != true ||
		h["why"] != "not sure that it is off: no answer for 1 minute so far, and Fairy takes a machine for off only after 15 minutes (it may be restarting)" {
		t.Fatalf("the page, 90 s without an answer: %v", h)
	}
	// A client that sends it as "off" all the same: the start is accepted (the list is allowed), and the run
	// itself finds that it is not sure. Nothing goes out.
	r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a", "pc-b"}, "update_if_on": true})
	if !stepUpRequired(r) {
		t.Fatal(r.code, string(r.raw))
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if act, _ := e.s.WakeRuns.Snapshot(); act == nil || len(act.Hosts) != 2 || act.Hosts[0].QuietForS == nil || *act.Hosts[0].QuietForS >= 900 {
		t.Fatalf("the run: %+v", act)
	}
	// pc-b comes back by itself before the run gets to it: it was restarting. Its system started 10 s ago. It
	// is on, so it is only updated ("update it too" was ticked) and stays on.
	f.on["pc-b"], f.booted["pc-b"], f.ready["pc-b"] = true, f.now.Add(-10*time.Second), f.now
	run := f.finish()
	if h := hostOf(run, "pc-a"); h.State != wakerun.StSkipped || h.Text != wakerun.NotSure || h.On != wakerun.OnUntouched || h.WakeSentAt != nil {
		t.Fatalf("pc-a: %s (%s): %s", h.State, h.On, h.Text)
	}
	if h := hostOf(run, "pc-b"); h.State != wakerun.StStayedOn || !h.AlreadyOn || h.WakeSentAt != nil || h.Text != "It was already on and stays on. It was updated to 17-bbbbbbb." {
		t.Fatalf("pc-b: %s (%s): %s", h.State, h.On, h.Text)
	}
	if got := strings.Join(f.requests(), ", "); got != "pc-b /update" {
		t.Fatalf("requests: %s", got)
	}
	for i := 0; i < 300; i++ { // pc-b is on and stays on
		f.tick()
	}
	if !f.on["pc-b"] || len(f.requests()) != 1 {
		t.Fatalf("pc-b on=%v, requests %v", f.on["pc-b"], f.requests())
	}
	if wuHostRow(t, get(), "pc-b")["choice"] != "update_only" {
		t.Fatal("pc-b answers: the page offers it as a machine that is on")
	}
	said := false
	for _, en := range e.auditEntries("", "wakeupdate.host") {
		said = said || (en["target"] == "pc-a" && en["outcome"] == wakerun.NotSure)
	}
	if !said {
		t.Fatalf("the audit log does not say why pc-a was left alone: %v", e.auditEntries("", "wakeupdate.host"))
	}

	// a quarter of an hour without an answer: off
	f.wait(wakerun.MinOff)
	h = wuHostRow(t, get(), "pc-a")
	if h["power"] != "off" || h["choice"] != "wake" || h["not_sure"] != nil || h["why"] != nil {
		t.Fatalf("the page after a quarter of an hour: %v", h)
	}
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if path == "/power" {
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(f.now.Add(120 * time.Second).Unix()), "delay_s": 120.0, "requested_by": "x"}}, nil
		}
		return 0, nil, nil
	}
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	run = f.finish()
	if h := hostOf(run, "pc-a"); h.State != wakerun.StOff || h.Text != "Woken, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again." || f.on["pc-a"] {
		t.Fatalf("pc-a: %s: %s (on=%v)", h.State, h.Text, f.on["pc-a"])
	}
	if got := strings.Join(f.requests(), ", "); got != "pc-b /update, nas /wol, pc-a /update, pc-a /power" {
		t.Fatalf("what the agents were sent: %s", got)
	}
}

// A machine that refuses the connection is on (finding W2): its own network stack sent the refusal, only its
// agent is not listening. However long that lasts, the page shows it as on, and a run sends it nothing. The
// same for a Proxmox node that the cluster shows as online while the agent on it is silent.
func TestWakeUpdateARefusedConnectionIsOn(t *testing.T) {
	e := setup(t)
	f := newFarm(t, e)
	refuse := func() (jsonx.M, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	e.b.Mu.Lock()
	e.b.Metrics["pc-a"] = refuse
	e.b.Mu.Unlock()
	if err := e.s.Poller.AddAgent(config.Agent{ID: "pve-agent", Name: "PVE agent", LinksTo: "node-srv-pvea", Managed: true, Token: "read-word", ControlToken: "control-word",
		URL: "http://pve-agent", PowerControl: true, SelfUpdate: true}, &config.SystemOverride{ID: "node-srv-pvea", Wol: &config.Wol{MAC: "00:00:5e:00:53:09", Relay: "nas"}}); err != nil {
		t.Fatal(err)
	}
	e.b.Mu.Lock()
	e.b.Metrics["pve-agent"] = func() (jsonx.M, error) { return nil, os.ErrDeadlineExceeded } // the agent on the node is silent
	e.b.Mu.Unlock()
	f.offline()
	f.optIn("pc-a", "pc-b", "node-srv-pvea")
	admin := e.cookie(controller(), 0)
	page := e.do("GET", "/api/admin/wake-update", nil, "Cookie", admin)
	if h := wuHostRow(t, page, "pc-a"); h["power"] != "on" || h["choice"] != "" || h["why"] != "it is on (the machine refused the connection), and its agent does not answer" {
		t.Fatalf("pc-a on the page: %v", h)
	}
	if h := wuHostRow(t, page, "node-srv-pvea"); h["power"] != "on" || h["choice"] != "" || h["why"] != "it is on (the Proxmox API shows it as online), and its agent does not answer" {
		t.Fatalf("the node on the page: %v", h)
	}
	if h := wuHostRow(t, page, "pc-b"); h["power"] != "off" || h["choice"] != "wake" {
		t.Fatalf("pc-b, which sends nothing at all: %v", h)
	}
	ops := runOps{e.s}
	if o := ops.Observe("pc-a"); o.OnBy != wakerun.OnByRefused || wakerun.OffNow(o, f.now) || o.QuietSince.IsZero() {
		t.Fatalf("the run's view of pc-a: %+v", o)
	}
	if o := ops.Observe("node-srv-pvea"); o.OnBy != "the Proxmox API shows it as online" || wakerun.OffNow(o, f.now) {
		t.Fatalf("the run's view of the node: %+v", o)
	}
	if o := ops.Observe("pc-b"); o.OnBy != "" || !wakerun.OffNow(o, f.now) {
		t.Fatalf("the run's view of pc-b: %+v", o)
	}
	f.wakes = false
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a", "node-srv-pvea"}, UpdateIfOn: true, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	run := f.finish()
	if h := hostOf(run, "pc-a"); h.State != wakerun.StSkipped || h.On != wakerun.OnYes || !h.AlreadyOn ||
		h.Text != "It is on (the machine refused the connection), and its agent does not answer. Nothing was sent to it and it stays on." {
		t.Fatalf("pc-a: %s (%s): %s", h.State, h.On, h.Text)
	}
	if h := hostOf(run, "node-srv-pvea"); h.State != wakerun.StSkipped || h.On != wakerun.OnYes || !h.AlreadyOn ||
		h.Text != "It is on (the Proxmox API shows it as online), and its agent does not answer. Nothing was sent to it and it stays on." {
		t.Fatalf("the node: %s (%s): %s", h.State, h.On, h.Text)
	}
	if got := f.requests(); len(got) != 0 {
		t.Fatalf("requests: %v", got)
	}
	// The panel's polls get no answer at all, and only the run's direct question is refused (the machine came
	// up between the two): the same.
	e.b.Mu.Lock()
	n := 0
	e.b.Metrics["pc-b"] = func() (jsonx.M, error) {
		if n++; n > 1 { // the first call from here on is the poll, the next is the run's question
			return refuse()
		}
		return f.metrics("pc-b")
	}
	e.b.Mu.Unlock()
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-b"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	run = f.finish()
	if h := hostOf(run, "pc-b"); h.State != wakerun.StSkipped || h.On != wakerun.OnYes || h.ProbedOff ||
		h.Text != "It is on (the machine refused the connection), and its agent does not answer. Nothing was sent to it and it stays on." {
		t.Fatalf("pc-b: %s (%s): %s", h.State, h.On, h.Text)
	}
	if got := f.requests(); len(got) != 0 {
		t.Fatalf("requests: %v", got)
	}
}
