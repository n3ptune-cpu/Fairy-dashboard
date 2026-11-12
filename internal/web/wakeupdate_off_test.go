package web

import (
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
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
	if o := ops.Observe("pc-a"); o.OnBy != wakerun.OnByRefused || wakerun.OffNow(o, f.now) || !o.QuietSince.IsZero() {
		t.Fatalf("the run's view of pc-a (a refusal is a sign of life: no silence has begun): %+v", o)
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

// The 15 minutes are 15 minutes of silence since the last sign of life, and a sign of life is more than an
// answer: a refused connection (the machine itself sent the refusal) and, for a Proxmox node, the cluster
// showing it as online. A machine that ran with its agent stopped for a quarter of an hour and then says
// nothing for a minute is not "off for 15 minutes": one minute of silence is what a restart looks like, and it
// would come back with a fresh uptime, like a machine the run woke. It is "not sure" and left alone, and it
// counts as off only 15 minutes after its last sign of life. (Before this, the refused polls counted as
// minutes without an answer.)
func TestWakeUpdateSilenceCountsFromTheLastSignOfLife(t *testing.T) {
	e := setup(t)
	f := newFarm(t, e)
	mode, nodeUp := "refuse", true
	e.b.Mu.Lock()
	e.b.Metrics["pc-a"] = func() (jsonx.M, error) { // it runs, its agent is stopped; later nothing answers at all
		if mode == "refuse" {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
		}
		return nil, os.ErrDeadlineExceeded
	}
	e.b.PVE = func() ([]jsonx.M, error) { // the cluster shows the node as online; later as offline
		rows := testutil.PVEResources()
		if !nodeUp {
			rows[0] = jsonx.M{"id": "node/SRV-PVEA", "type": "node", "node": "SRV-PVEA", "status": "offline", "maxcpu": 8.0, "maxmem": float64(32 << 30)}
		}
		return rows, nil
	}
	e.b.Mu.Unlock()
	if err := e.s.Poller.AddAgent(config.Agent{ID: "pve-agent", Name: "PVE agent", LinksTo: "node-srv-pvea", Managed: true, Token: "read-word", ControlToken: "control-word",
		URL: "http://pve-agent", PowerControl: true, SelfUpdate: true}, &config.SystemOverride{ID: "node-srv-pvea", Wol: &config.Wol{MAC: "00:00:5e:00:53:09", Relay: "nas"}}); err != nil {
		t.Fatal(err)
	}
	e.b.Mu.Lock()
	e.b.Metrics["pve-agent"] = func() (jsonx.M, error) { return nil, os.ErrDeadlineExceeded } // the agent on the node is silent all the time
	e.b.Mu.Unlock()
	for i := 0; i < 90; i++ {
		f.tick()
	}
	f.wait(14 * time.Minute)
	f.optIn("pc-a", "pc-b", "node-srv-pvea")
	admin := e.cookie(controller(), 0)
	page := func() resp { return e.do("GET", "/api/admin/wake-update", nil, "Cookie", admin) }
	ops := runOps{e.s}
	both := []string{"pc-a", "node-srv-pvea"}

	// a quarter of an hour in which each of the two showed that it runs: on, and no silence has begun
	for _, id := range both {
		if h := wuHostRow(t, page(), id); h["power"] != "on" || h["choice"] != "" {
			t.Fatalf("%s while it shows that it runs: %v", id, h)
		}
		if o := ops.Observe(id); o.OnBy == "" || !o.QuietSince.IsZero() || wakerun.QuietFor(o, f.now) != 0 {
			t.Fatalf("%s: a machine that shows that it runs is not silent: %+v", id, o)
		}
	}
	if h := wuHostRow(t, page(), "pc-b"); h["power"] != "off" || h["choice"] != "wake" {
		t.Fatalf("pc-b, which sent nothing at all in that time: %v", h)
	}

	// then both go silent (a restart begins like this). The silence counts from the first poll that gets
	// nothing: for pc-a the poll ten seconds after its last refusal. For the node it is that poll too, or the one
	// after it: the agent's poll and the cluster's run side by side, and the agent's may still see the node as
	// online. Three minutes later:
	e.b.Mu.Lock()
	mode, nodeUp = "silent", false
	e.b.Mu.Unlock()
	f.wait(3 * time.Minute)
	quiet := map[string]time.Duration{"pc-a": 170 * time.Second, "node-srv-pvea": wakerun.QuietFor(ops.Observe("node-srv-pvea"), f.now)}
	if q := quiet["node-srv-pvea"]; q != 170*time.Second && q != 160*time.Second {
		t.Fatalf("the node's silence began with one of the two polls after the cluster showed it as offline, not %v ago", q)
	}
	for _, id := range both {
		h := wuHostRow(t, page(), id)
		why := "not sure that it is off: no answer for " + spanWords(quiet[id]) + " so far, and Fairy takes a machine for off only after 15 minutes (it may be restarting)"
		if h["power"] != "unclear" || h["choice"] != "" || h["not_sure"] != true || h["why"] != why {
			t.Fatalf("%s after %v of silence: %v", id, quiet[id], h)
		}
		if o := ops.Observe(id); o.OnBy != "" || wakerun.OffNow(o, f.now) || wakerun.QuietFor(o, f.now) != quiet[id] {
			t.Fatalf("%s: the run's view: %+v, quiet for %v", id, o, wakerun.QuietFor(o, f.now))
		}
	}
	// a run that names them anyway sends them nothing: no wake, so nothing can come back "woken" and be switched off
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: both, UpdateIfOn: true, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	run := f.finish()
	for _, id := range both {
		if h := hostOf(run, id); h.State != wakerun.StSkipped || h.On != wakerun.OnUntouched || h.Text != wakerun.NotSure || h.WakeSentAt != nil || h.ProbedOff {
			t.Fatalf("%s in a run: %s (%s): %s", id, h.State, h.On, h.Text)
		}
	}
	if got := f.requests(); len(got) != 0 {
		t.Fatalf("requests: %v", got)
	}

	// still not sure just before 15 minutes of silence, and off from 15 minutes after the last sign of life
	f.wait(11 * time.Minute) // just under fourteen minutes of silence (the run above waited for nothing)
	for _, id := range both {
		if o := ops.Observe(id); wakerun.OffNow(o, f.now) || wakerun.QuietFor(o, f.now) >= wakerun.MinOff {
			t.Fatalf("%s after %v of silence must not be off yet", id, wakerun.QuietFor(o, f.now))
		}
		if h := wuHostRow(t, page(), id); h["power"] != "unclear" {
			t.Fatalf("%s just before the 15 minutes: %v", id, h)
		}
	}
	f.wait(2 * time.Minute)
	for _, id := range both {
		if o := ops.Observe(id); !wakerun.OffNow(o, f.now) || wakerun.QuietFor(o, f.now) < wakerun.MinOff {
			t.Fatalf("%s after %v of silence is off: %+v", id, wakerun.QuietFor(o, f.now), o)
		}
	}
	if h := wuHostRow(t, page(), "pc-a"); h["power"] != "off" || h["choice"] != "wake" {
		t.Fatalf("pc-a 15 minutes after its last refusal: %v", h)
	}
	if h := wuHostRow(t, page(), "node-srv-pvea"); h["power"] != "off" {
		t.Fatalf("the node 15 minutes after the cluster last showed it as online: %v", h)
	}
}

// r1-p2-03 (audit 5): a machine that answers with something that is not its agent's answer is on. An HTTP
// status (401 after its token changed, 500), a body that is not JSON: each needs a machine that runs at that
// address. Before, a quarter of an hour of those was "nothing at all for 15 minutes": the page offered the
// machine as "Off", ticked. Now the page shows it as on, however long it lasts, a run sends it nothing, and
// the 15 minutes begin with the first poll after the last such answer.
func TestWakeUpdateAnAnswerThatIsNotTheAgentsIsOn(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		sign string
	}{
		"401":  {&devices.HTTPStatusError{Code: 401}, "its address answered: http 401"},
		"500":  {&devices.HTTPStatusError{Code: 500}, "its address answered: http 500"},
		"junk": {devices.Errf("invalid response"), "its address answered: invalid response"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			f := newFarm(t, e)
			silent := false
			e.b.Mu.Lock()
			e.b.Metrics["pc-a"] = func() (jsonx.M, error) {
				if silent {
					return nil, os.ErrDeadlineExceeded
				}
				return nil, c.err
			}
			e.b.Mu.Unlock()
			f.offline() // more than a quarter of an hour of such answers
			f.optIn("pc-a", "pc-b")
			admin := e.cookie(controller(), 0)
			page := func() resp { return e.do("GET", "/api/admin/wake-update", nil, "Cookie", admin) }
			if h := wuHostRow(t, page(), "pc-a"); h["power"] != "on" || h["choice"] != "" || h["why"] != "it is on ("+c.sign+"), and its agent does not answer" {
				t.Fatalf("pc-a on the page: %v", h)
			}
			if h := wuHostRow(t, page(), "pc-b"); h["power"] != "off" || h["choice"] != "wake" {
				t.Fatalf("pc-b, which sends nothing at all: %v", h)
			}
			ops := runOps{e.s}
			if o := ops.Observe("pc-a"); o.OnBy != c.sign || wakerun.OffNow(o, f.now) || !o.QuietSince.IsZero() {
				t.Fatalf("the run's view of pc-a: %+v", o)
			}
			// a client that names it as "off" all the same: nothing goes out
			if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, UpdateIfOn: true, By: "Alice", BySub: "u2"}); err != nil {
				t.Fatal(err)
			}
			run := f.finish()
			if h := hostOf(run, "pc-a"); h.State != wakerun.StSkipped || h.On != wakerun.OnYes || !h.AlreadyOn || h.WakeSentAt != nil ||
				h.Text != "It is on ("+c.sign+"), and its agent does not answer. Nothing was sent to it and it stays on." {
				t.Fatalf("pc-a: %s (%s): %s", h.State, h.On, h.Text)
			}
			if got := f.requests(); len(got) != 0 {
				t.Fatalf("requests: %v", got)
			}
			// then nothing at all: "not sure" for 15 minutes from the first silent poll, then off
			e.b.Mu.Lock()
			silent = true
			e.b.Mu.Unlock()
			f.wait(14 * time.Minute)
			if h := wuHostRow(t, page(), "pc-a"); h["power"] != "unclear" || h["not_sure"] != true {
				t.Fatalf("14 minutes after its last answer: %v", h)
			}
			f.wait(2 * time.Minute)
			if h := wuHostRow(t, page(), "pc-a"); h["power"] != "off" || h["choice"] != "wake" {
				t.Fatalf("16 minutes after its last answer: %v", h)
			}
		})
	}
}
