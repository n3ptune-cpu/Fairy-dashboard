package web

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// A machine that checks passkey approvals itself is in no "Update unpowered hosts" run yet: a run has no
// approval to give it for the update and the shutdown it would send many minutes later. The page says so
// (also while the machine is off: its last answer stays), and a start that names it is refused before anybody
// is asked to confirm; the other machines are as before.
func TestRunsLeaveOutMachinesThatCheckApprovals(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	e.b.Mu.Lock()
	plain := e.b.Metrics["pc-a"]
	e.b.Metrics["pc-a"] = func() (jsonx.M, error) {
		m, err := plain()
		if err == nil {
			m["approval"] = jsonx.M{"state": "required", "enforced_by": "agent", "proto": 2.0, "machine_id": "farm-machine-id-of-pc-a-01",
				"issuer": "https://id.example.org", "client_id": "pc-actions", "users": 1.0, "fan_window_s": 30.0}
		}
		return m, err
	}
	f.on["pc-a"], f.on["pc-b"] = true, true
	e.b.Mu.Unlock()
	f.optIn("pc-a", "pc-b")
	f.tick()
	page := func() resp { return e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess) }
	// It shows the check and nothing is on record for it (audit 5, round 3, r3-x1-01: no id comes on record at
	// first sight any more): "not on record" is in no run either, and the row says why
	h := wuHostRow(t, page(), "pc-a")
	if h["choice"] != "" || h["approval"] != "alarm" || !strings.HasPrefix(h["why"].(string), "in no run: has the check on with a machine id that is not on record") {
		t.Fatalf("pc-a while it is not on record: %v", h)
	}
	if r := e.wuStart(a.sess, map[string]any{"update_only": []string{"pc-a"}}); r.code != 400 || !strings.Contains(string(r.raw), "PC-A is in no run: has the check on with a machine id that is not on record") {
		t.Fatalf("a start with a machine that is not on record: %d %s", r.code, r.raw)
	}
	f.accept(t, "pc-a", "farm-machine-id-of-pc-a-01") // an admin accepts its id: on record from here on
	h = wuHostRow(t, page(), "pc-a")
	if h["choice"] != "" || h["approval"] != "required" || !strings.HasPrefix(h["why"].(string), "needs approval: not supported in runs yet") {
		t.Fatalf("pc-a while it is on: %v", h)
	}
	if h := wuHostRow(t, page(), "pc-b"); h["choice"] != "update_only" || h["approval"] != nil {
		t.Fatalf("pc-b asks for nothing and is as before: %v", h)
	}
	// off: what it last said about itself still counts
	e.b.Mu.Lock()
	f.on["pc-a"] = false
	e.b.Mu.Unlock()
	f.offline()
	if h := wuHostRow(t, page(), "pc-a"); h["power"] != "off" || h["choice"] != "" || !strings.HasPrefix(h["why"].(string), "needs approval: not supported in runs yet") {
		t.Fatalf("pc-a while it is off: %v", h)
	}
	before := len(f.requests())
	for _, body := range []any{
		map[string]any{"hosts": []string{"pc-a"}},
		map[string]any{"update_only": []string{"pc-a"}},
		map[string]any{"hosts": []string{"pc-a"}, "update_only": []string{"pc-b"}},
	} {
		r := e.wuStart(a.sess, body)
		if d, _ := r.body["detail"].(string); r.code != 400 || !strings.HasPrefix(d, "PC-A needs approval: not supported in runs yet") {
			t.Fatalf("a start with pc-a %v: %d %s", body, r.code, r.raw)
		}
	}
	if act, hist := e.s.WakeRuns.Snapshot(); act != nil || len(hist) != 0 || len(f.requests()) != before {
		t.Fatalf("no run was started and nothing was sent: %v %v %v", act, hist, f.requests()[before:])
	}
	// a run with the other machine starts as it always did: the confirmation first
	r := e.wuStart(a.sess, map[string]any{"update_only": []string{"pc-b"}})
	if d, _ := r.body["detail"].(map[string]any); r.code != 401 || d["code"] != "step_up_required" || d["approval"] != nil {
		t.Fatalf("pc-b alone: %d %s", r.code, r.raw)
	}
}

// accept is an admin's accept of the machine id mid that the farm machine shows (checksItself, and a tick since):
// the one way the id of a check that was set on the machine comes on record.
func (f *farm) accept(t *testing.T, id, mid string) {
	t.Helper()
	if what, _, err := f.e.s.Poller.AcceptMachine(id, "test admin", poller.AcceptWant{MachineID: mid}); what != "id" || err != nil {
		t.Fatalf("%s: the accept of its id: %q %v", id, what, err)
	}
	if b := f.e.s.Poller.BindingOf(id); b.Record.MachineID != mid || b.Record.How != approval.PinAccepted {
		t.Fatalf("%s after the accept: %+v", id, b)
	}
}

// checksItself makes the farm machine id one that requires approvals, with the machine id mid (scheme 2).
func (f *farm) checksItself(id, mid string) {
	f.e.b.Mu.Lock()
	plain := f.e.b.Metrics[id]
	f.e.b.Metrics[id] = func() (jsonx.M, error) {
		m, err := plain()
		if err == nil {
			m["approval"] = jsonx.M{"state": "required", "enforced_by": "agent", "proto": 2.0, "machine_id": mid, "issuer": "https://id.example.org",
				"client_id": "pc-actions", "users": 1.0, "fan_window_s": 30.0}
		}
		return m, err
	}
	f.e.b.Mu.Unlock()
}

// restarted is the farm for a panel that was started again on the same folders (env.restart): the same
// machines in the state they are in, the farm's clock going on, and the new panel's clocks set to it.
func (f *farm) restarted(e2 *env) *farm {
	f2 := *f
	f2.e = e2
	p := e2.s.Poller
	p.Clock = func() time.Time { return f2.now }
	e2.s.WakeRuns.Now = p.Clock
	began := f2.now
	e2.s.WakeRuns.Mono = func() time.Duration { return f2.now.Sub(began) }
	e2.b.Mu.Lock()
	for _, id := range []string{"pc-a", "pc-b"} {
		e2.b.Metrics[id] = func() (jsonx.M, error) { return f2.metrics(id) }
	}
	f2.seen = len(e2.b.Requests)
	e2.b.Mu.Unlock()
	return &f2
}

// r1-b2-03 (audit 5): the panel kept what a machine said about its check in memory only. After a restart of the
// panel, a machine with the check on that was off was unknown to it: after the quarter of an hour it was
// offered to "Update unpowered hosts" as an ordinary machine that is off, woken, could not be updated or
// switched off (it asks for an approval the run does not have) and was LEFT ON. The panel now has the machine
// on record (machines.json): it is in no run, also while it is silent, also after a restart, and its page and
// the Updates page still say that it checks approvals.
func TestAMachineWithTheCheckIsInNoRunAfterARestartOfThePanel(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	const mid = "farm-machine-id-of-pc-a-01"
	f.checksItself("pc-a", mid)
	e.b.Mu.Lock()
	f.on["pc-a"], f.on["pc-b"] = true, true
	e.b.Mu.Unlock()
	f.optIn("pc-a", "pc-b")
	f.tick()
	f.accept(t, "pc-a", mid)
	if b := e.s.Poller.BindingOf("pc-a"); !b.OK() || b.Record.MachineID != mid || b.Record.State != "required" {
		t.Fatalf("on record while it answers: %+v", b)
	}
	// both machines are switched off; then the panel restarts (an update of the panel is one)
	e.b.Mu.Lock()
	f.on["pc-a"], f.on["pc-b"] = false, false
	e.b.Mu.Unlock()
	f.tick()
	e2 := e.restart()
	f2 := f.restarted(e2) // the same machines for the new panel, both off
	if _, heard := e2.s.Poller.AgentInfo("pc-a"); heard {
		t.Fatal("the test needs a panel that has not heard from pc-a since it started")
	}
	f2.offline() // no answer for a quarter of an hour and more
	page := e2.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
	if page.code != 200 {
		t.Fatalf("the page: %d %s", page.code, page.raw)
	}
	h := wuHostRow(t, page, "pc-a")
	if h["power"] != "off" || h["choice"] != "" || h["approval"] != "required" || !strings.HasPrefix(h["why"].(string), "needs approval: not supported in runs yet") {
		t.Fatalf("pc-a after the restart, off for a quarter of an hour: %v", h)
	}
	if h := wuHostRow(t, page, "pc-b"); h["power"] != "off" || h["choice"] != "wake" || h["approval"] != nil {
		t.Fatalf("pc-b has no check and is offered as before: %v", h)
	}
	// a start that names it is refused before anybody is asked to confirm, and nothing is sent: no wake
	before := len(f2.requests())
	for _, body := range []any{map[string]any{"hosts": []string{"pc-a"}}, map[string]any{"hosts": []string{"pc-a", "pc-b"}}} {
		r := e2.wuStart(a.sess, body)
		if d, _ := r.body["detail"].(string); r.code != 400 || !strings.HasPrefix(d, "PC-A needs approval: not supported in runs yet") {
			t.Fatalf("a start with pc-a %v: %d %s", body, r.code, r.raw)
		}
	}
	if act, hist := e2.s.WakeRuns.Snapshot(); act != nil || len(hist) != 0 || len(f2.requests()) != before {
		t.Fatalf("no run was started and nothing was sent: %v", f2.requests()[before:])
	}
	// its page and the Updates page still know: the block is made from the record, marked "last known"
	x := e2.do("GET", "/api/systems/pc-a", nil, "Cookie", a.sess)
	if ap, _ := x.body["approval"].(map[string]any); ap == nil || ap["state"] != "required" || ap["last_known"] != true || strings.Contains(string(x.raw), mid) {
		t.Fatalf("the system's block after the restart: %s", x.raw)
	}
	rows := map[string]any{}
	for _, r := range e2.do("GET", "/api/admin/updates", nil, "Cookie", a.sess).body["agents"].([]any) {
		rows[r.(map[string]any)["id"].(string)] = r.(map[string]any)["approval"]
	}
	if rows["pc-a"] != "required" || rows["pc-b"] != nil {
		t.Fatalf("the Updates page's rows: %v", rows)
	}
	for _, m := range e2.do("GET", "/api/admin/approval", nil, "Cookie", a.sess).body["machines"].([]any) {
		if m := m.(map[string]any); m["id"] == "pc-a" && (m["state"] != "required" || m["last_known"] != true || m["machine_id"] != mid) {
			t.Fatalf("the approval page after the restart: %v", m)
		}
	}
	// a shutdown from its page is not sent on the panel's own confirmation either: the machine is asked first
	// (it does not answer: it is off)
	r := e2.do("POST", "/api/systems/pc-a/power", map[string]any{"action": "wake"}, "Cookie", a.sess, "X-Requested-With", "fairy", "Content-Type", "application/json")
	if r.code == 400 && strings.Contains(string(r.raw), "approval") {
		t.Fatalf("waking needs no approval: %d %s", r.code, r.raw)
	}
	// it comes back and answers: the block is the machine's own again, the record is as it was, and it is
	// still in no run
	f2.checksItself("pc-a", mid)
	e2.b.Mu.Lock()
	f2.on["pc-a"] = true
	e2.b.Mu.Unlock()
	f2.tick()
	if b := e2.s.Poller.BindingOf("pc-a"); !b.OK() || b.Record.MachineID != mid || !b.Report.Heard {
		t.Fatalf("pc-a back online: %+v", b)
	}
	if h := wuHostRow(t, e2.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess), "pc-a"); h["choice"] != "" || h["approval"] != "required" {
		t.Fatalf("pc-a back online: %v", h)
	}
}

// A machine whose binding is an alarm is in no run either, and the reason says so.
func TestAMachineWithAnAlarmIsInNoRun(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.checksItself("pc-a", "farm-machine-id-of-pc-a-01")
	e.b.Mu.Lock()
	f.on["pc-a"], f.on["pc-b"] = true, true
	e.b.Mu.Unlock()
	f.optIn("pc-a", "pc-b")
	f.tick()
	f.accept(t, "pc-a", "farm-machine-id-of-pc-a-01")
	f.checksItself("pc-b", "farm-machine-id-of-pc-a-01") // a copy of pc-a's disk
	f.tick()
	page := e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
	for _, id := range []string{"pc-a", "pc-b"} {
		if h := wuHostRow(t, page, id); h["choice"] != "" || h["approval"] != "alarm" || !strings.HasPrefix(h["why"].(string), "in no run: has the same machine id as") {
			t.Fatalf("%s: %v", id, h)
		}
	}
	if r := e.wuStart(a.sess, map[string]any{"update_only": []string{"pc-b"}}); r.code != 400 || !strings.Contains(string(r.raw), "PC-B is in no run: has the same machine id as PC-A") {
		t.Fatalf("a start with a flagged machine: %d %s", r.code, r.raw)
	}
}

// r1-b2-05 (audit 5): "Switch on" for a machine with the check answered "can now be chosen for Update
// unpowered hosts" while its row said that it cannot. The answer now says what the row says, and the row says
// it before the switch too.
func TestSwitchOnForRunsSaysWhatTheRowSays(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.checksItself("pc-a", "farm-machine-id-of-pc-a-01")
	e.b.Mu.Lock()
	f.on["pc-a"], f.on["pc-b"] = true, true
	e.b.Mu.Unlock()
	f.tick()
	f.accept(t, "pc-a", "farm-machine-id-of-pc-a-01")
	page := func() resp { return e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess) }
	if h := wuHostRow(t, page(), "pc-a"); h["opted_in"] != false || !strings.HasPrefix(h["why"].(string), "needs approval: not supported in runs yet") {
		t.Fatalf("pc-a before the switch: %v", h)
	}
	put := func(id string) resp {
		return e.do("PUT", "/api/systems/"+id+"/wake-update", map[string]any{"enabled": true}, append([]string{"Cookie", a.sess}, wuWrite...)...)
	}
	r := put("pc-a")
	if cb := a.confirm(pendingID(r), false); !stepUpRequired(r) || cb.code != 303 {
		t.Fatalf("the confirmation: %d %s", r.code, r.raw)
	}
	// inside the window now: the page asks first (review of passkey-k, RK-04: nothing asked here, and the switch
	// was recorded; approval_runs_switch_test.go), and the answer to the request that says so is the message
	if r = put("pc-a"); r.code != 409 {
		t.Fatalf("pc-a inside the window, not asked: %d %s", r.code, r.raw)
	}
	r = e.do("PUT", "/api/systems/pc-a/wake-update", map[string]any{"enabled": true, "asked": true}, append([]string{"Cookie", a.sess}, wuWrite...)...)
	if msg, _ := r.body["message"].(string); r.code != 200 || msg != "PC-A is switched on, but no run can take it: it checks a passkey approval itself" || strings.Contains(msg, "can now be chosen") {
		t.Fatalf("pc-a: %d %s", r.code, r.raw)
	}
	if h := wuHostRow(t, page(), "pc-a"); h["opted_in"] != true || h["choice"] != "" || !strings.HasPrefix(h["why"].(string), "needs approval: not supported in runs yet") {
		t.Fatalf("pc-a after the switch: %v", h)
	}
	if r := put("pc-b"); r.code != 200 || !strings.HasPrefix(r.body["message"].(string), "PC-B can now be chosen for \"Update unpowered hosts\"") {
		t.Fatalf("pc-b: %d %s", r.code, r.raw)
	}
	// (in its column, and under the name on a phone, where that column is scrolled out of view: audit 5, r2-b2-07)
	if js := appJS(t); !strings.Contains(js, "const whyNot = (h) => ((h.opted_in && !h.choice) || h.approval ? h.why || \"\" : \"\");") ||
		!strings.Contains(js, "${whyNot(h) ? `<div class=\"note wu-why-col\">${esc(whyNot(h))}</div>` : \"\"}") || !strings.Contains(js, "${whyNot(h) ? `<span class=\"note wu-why\">${esc(whyNot(h))}</span>` : \"\"}") {
		t.Error("the row shows the reason only after the switch")
	}
}
