package web

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	agentapproval "github.com/n3ptune-cpu/Fairy-agent/pkg/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// The control routes against machines that check approvals themselves. The machines are testutil.Machine:
// the panel's requests go through Fairy-agent's real gate (challenges, the trust file, the offline check of
// the token); the identity provider is testutil.Issuer (https, its own key, tokens made at run time).

type approvalEnv struct {
	*env
	iss    *testutil.Issuer
	gb     *testutil.GatedBackend
	laptop *testutil.Machine // power_control, self_update
	tower  *testutil.Machine // fan_control
	admin  string            // the session cookie of an admin whose sign-in is an hour old
	csrf   []string
	// what require itself left behind: every machine it sets up is "not on record" until its id is accepted,
	// which is one alarm and its end in the audit log and in the Notifications list. The tests count from here.
	alarmBase, noteBase int
}

const approvalAdminSub = "u2" // controller()'s Pocket ID user id

func newApprovalEnv(t *testing.T) *approvalEnv {
	t.Helper()
	a := &approvalEnv{}
	a.env = setupWith(t, func(b poller.Backend) poller.Backend {
		a.gb = testutil.NewGatedBackend(b.(*testutil.Backend))
		return a.gb
	})
	a.iss = testutil.NewIssuer(t)
	a.s.Cfg.Approval = config.Approval{ClientID: testApprovalClient, Issuer: a.iss.URL(), RunApprovalWaitS: 900}
	a.s.SetIDPClient(a.iss.Client())
	a.s.control, a.s.approvalRL = newRateLimiter(1000), newRateLimiter(1000)
	a.laptop, a.tower = a.gb.Add(t, "laptop"), a.gb.Add(t, "tower")
	a.b.Mu.Lock()
	a.b.Metrics["tower"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["hostname"] = "tower"
		m["features"].(jsonx.M)["fan_control"] = true
		return m, nil
	}
	a.b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["agent_version"] = "41-aaaaaaa"
		m["features"].(jsonx.M)["self_update"] = true
		m["self_update"] = jsonx.M{"state": "idle", "since": 1.0}
		return m, nil
	}
	a.b.Mu.Unlock()
	a.admin = a.cookie(controller(), time.Hour)
	a.csrf = []string{"Cookie", a.admin, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	a.poll()
	return a
}

func (a *approvalEnv) poll() { a.s.Poller.PollOnce(context.Background()) }

// require gives machines a trust file for this issuer and client with the admin as the one allowed user (what
// "Turn on" writes), lets the panel see it, and takes each machine's id on record the one way a check that was
// set on the machine comes on record: an admin accepts the id the machine shows (audit 5, round 3, r3-x1-01;
// until then the panel took it at first sight). The button's own way through "Confirm it's you" is in
// approval_notonrecord_test.go.
func (a *approvalEnv) require(t *testing.T, machines ...*testutil.Machine) {
	t.Helper()
	a.setTrust(t, machines...)
	a.poll()
	for _, m := range machines {
		if ap := a.s.Poller.ApprovalOf(m.ID); ap == nil || ap.State != api.ApprovalRequired {
			t.Fatalf("%s: the panel does not see the machine's state as required: %+v", m.ID, ap)
		}
		if b := a.s.Poller.BindingOf(m.ID); b.Status != approval.BindNoRecord || b.Record.MachineID != "" {
			t.Fatalf("%s: a machine that shows the check must be \"not on record\", with nothing on record: %+v", m.ID, b)
		}
	}
	a.acceptShown(t, machines...)
	a.alarmBase, a.noteBase = len(a.auditEntries("", "approval.alarm")), 0
	_, a.noteBase = a.alarms()
}

// alarmEntries are the approval.alarm entries of the audit log since require, newest first.
func (a *approvalEnv) alarmEntries() []map[string]any {
	all := a.auditEntries("", "approval.alarm")
	return all[:len(all)-a.alarmBase]
}

// acceptShown is an admin's accept of the machine id each machine shows now.
func (a *approvalEnv) acceptShown(t *testing.T, machines ...*testutil.Machine) {
	t.Helper()
	for _, m := range machines {
		if what, _, err := a.s.Poller.AcceptMachine(m.ID, "test admin", poller.AcceptWant{MachineID: machineIDOf(m.ID)}); what != "id" || err != nil {
			t.Fatalf("%s: the accept of its id: %q %v", m.ID, what, err)
		}
		if b := a.s.Poller.BindingOf(m.ID); !b.OK() || b.Record.MachineID != machineIDOf(m.ID) || b.Record.How != approval.PinAccepted {
			t.Fatalf("%s: its id is not on record after the accept: %+v", m.ID, b)
		}
	}
}

// setTrust writes the trust files on the machines, as `helper-trust set` does there, without the panel
// hearing of it.
func (a *approvalEnv) setTrust(t *testing.T, machines ...*testutil.Machine) {
	t.Helper()
	for _, m := range machines {
		tr, err := agentapproval.NewTrust(agentapproval.Trust{Issuer: a.iss.URL(), ClientID: testApprovalClient, MachineID: machineIDOf(m.ID),
			Keys: []api.TrustKey{agentapproval.KeyFromRSA(a.iss.Kid, &a.iss.Key.PublicKey)}, Users: []string{approvalAdminSub}})
		if err != nil {
			t.Fatal(err)
		}
		if err := (agentapproval.Files{Path: m.TrustPath}).Create(tr); err != nil {
			t.Fatal(err)
		}
		m.Gate.StatusNow() // the machine notices its new trust file at once (a real one looks at most every 5 s)
	}
}

// machineIDOf is the machine id a test machine has in the trust file that require writes for it (set on the
// machine, as `helper-trust set` does): it comes on record when an admin accepts it.
func machineIDOf(agent string) string { return "id-of-" + agent + "-0123456789abcdef" }

func (a *approvalEnv) post(path string, body any) resp { return a.do("POST", path, body, a.csrf...) }

// asked reports whether r is the dialog for a machine's approval, and returns the kept action's id.
func approvalAsked(r resp) (id string, machines []any, ok bool) {
	d, _ := r.body["detail"].(map[string]any)
	ap, _ := d["approval"].(map[string]any)
	if r.code != 401 || d["code"] != "step_up_required" || ap == nil || ap["client_id"] != testApprovalClient || ap["client"] != nil || d["per_action"] != true {
		return "", nil, false
	}
	id, _ = d["pending_id"].(string)
	machines, _ = ap["machines"].([]any)
	return id, machines, id != ""
}

// approve is the rest of the dialog: "Continue", the passkey at Pocket ID, and the answer coming back to the
// panel. It returns the callback's answer.
func (a *approvalEnv) approve(t *testing.T, id string) resp {
	t.Helper()
	r := a.post(stepUpPath, map[string]any{"pending_id": id, "next": "/"})
	if r.code != 200 {
		t.Fatalf("Continue: %d %s", r.code, r.raw)
	}
	back := a.iss.Approve(t, fmt.Sprint(r.body["url"]))
	if back.Path != config.ApprovalCallbackPath || back.Host != "panel.example.com" {
		t.Fatalf("Pocket ID sends the browser to %s", back)
	}
	return a.do("GET", back.RequestURI(), nil, "Cookie", a.admin)
}

// result is the outcome of the action the panel ran after the approval (shown once by /api/me).
func (a *approvalEnv) result(t *testing.T) map[string]any {
	t.Helper()
	pr, _ := a.do("GET", "/api/me", nil, "Cookie", a.admin).body["pending_result"].(map[string]any)
	if pr == nil {
		t.Fatal("no result of the approved action")
	}
	return pr
}

func sentPaths(list []testutil.Sent) string {
	var out []string
	for _, s := range list {
		out = append(out, s.Path)
	}
	return strings.Join(out, " ")
}

func (a *approvalEnv) shutdownBody() map[string]any {
	name, _ := a.do("GET", "/api/systems/laptop", nil, "Cookie", a.admin).body["name"].(string)
	return map[string]any{"action": "shutdown", "delay_s": 10, "confirm": name}
}

// noLeak fails when any token the issuer handed out is in text.
func (a *approvalEnv) noLeak(t *testing.T, where, text string) {
	t.Helper()
	for _, tok := range a.iss.Tokens() {
		parts := strings.Split(tok, ".")
		if strings.Contains(text, tok) || strings.Contains(text, parts[2]) || strings.Contains(text, parts[1]) {
			t.Errorf("an ID token (or a part of it) is in %s", where)
		}
	}
}

// A shutdown of a machine that requires approvals: asked every time (also seconds after a sign-in), one
// request to Pocket ID, the machine's real check accepts it, and the token is nowhere but in that request.
func TestApprovedShutdown(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	logs := captureLog(t)
	var answers []string
	fresh := a.cookie(controller(), 0) // signed in this second: the panel's own window is open
	r := a.do("POST", "/api/systems/laptop/power", a.shutdownBody(), "Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json")
	if _, _, ok := approvalAsked(r); !ok {
		t.Fatalf("inside the panel's window the machine's approval is still asked for: %d %s", r.code, r.raw)
	}
	r = a.post("/api/systems/laptop/power", a.shutdownBody())
	answers = append(answers, string(r.raw))
	id, machines, ok := approvalAsked(r)
	if !ok || len(machines) != 1 || machines[0] != "laptop" {
		t.Fatalf("the dialog: %d %s", r.code, r.raw)
	}
	if d := r.body["detail"].(map[string]any); d["action"] != "power action on laptop (action: shutdown)" || !strings.Contains(fmt.Sprint(d["message"]), "checks this approval itself") {
		t.Fatalf("the dialog's texts: %v", d)
	}
	if got := sentPaths(a.laptop.Asked()); got != "/approval/challenge /approval/challenge" || len(a.laptop.Passed()) != 0 {
		t.Fatalf("before the approval the machine was sent: %s", got)
	}
	cb := a.approve(t, id)
	answers = append(answers, string(cb.raw))
	if cb.code != 303 {
		t.Fatalf("callback: %d %s", cb.code, cb.raw)
	}
	q := a.iss.Asked()[0]
	if q.Get("client_id") != testApprovalClient || q.Get("max_age") != "0" || len(q.Get("nonce")) != 43 {
		t.Fatalf("Pocket ID was asked: %v", q)
	}
	passed := a.laptop.Passed()
	if len(passed) != 1 || passed[0].Path != "/power" || !passed[0].Approved || passed[0].Body["action"] != "shutdown" || passed[0].Body["delay_s"] != 10.0 {
		t.Fatalf("what the machine's gate let through: %+v", passed)
	}
	// the entry the machine got is its own challenge with the exact text of the request
	ap := passed[0].Body["approval"].(map[string]any)
	entries := ap["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["request"] != "power action=shutdown delay_s=10" {
		t.Fatalf("entries: %v", entries)
	}
	pr := a.result(t)
	answers = append(answers, fmt.Sprint(pr))
	if pr["ok"] != true || pr["status"] != 200.0 {
		t.Fatalf("result: %v", pr)
	}
	audit, _ := os.ReadFile(a.s.Cfg.AuditLog)
	if !strings.Contains(string(audit), `"action":"power.shutdown"`) || !strings.Contains(string(audit), `"approval":"sent"`) {
		t.Errorf("audit: %s", audit)
	}
	a.noLeak(t, "an answer to the browser", strings.Join(answers, "\n"))
	a.noLeak(t, "the log", logs.take())
	a.noLeak(t, "the audit log", string(audit))
	if len(a.iss.Tokens()) != 1 {
		t.Fatalf("%d tokens were asked for", len(a.iss.Tokens()))
	}
	// the next shutdown is asked again: the approval opened no window
	if _, _, ok := approvalAsked(a.post("/api/systems/laptop/power", a.shutdownBody())); !ok {
		t.Fatal("the second shutdown must ask for its own approval")
	}
	// cancel needs none (and no challenge is asked for it)
	n := len(a.laptop.Asked())
	old := a.cookie(controller(), 0)
	if r := a.do("POST", "/api/systems/laptop/power", map[string]any{"action": "cancel"}, "Cookie", old, "X-Requested-With", "fairy", "Content-Type", "application/json"); r.code == 401 {
		t.Fatalf("cancel asked to confirm inside the window: %s", r.raw)
	}
	for _, s := range a.laptop.Asked()[n:] {
		if s.Path == api.PathApprovalChallenge {
			t.Fatal("a challenge was asked for cancel")
		}
	}
}

// The kept action is bound to the exact request: if what runs after the approval is not what was approved, the
// panel refuses it itself and the machine is not sent anything.
func TestApprovedActionIsBoundToItsRequest(t *testing.T) {
	for name, edit := range map[string]func(body string) string{
		"another delay":  func(b string) string { return strings.Replace(b, `"delay_s":10`, `"delay_s":11`, 1) },
		"another action": func(b string) string { return strings.Replace(b, `"action":"shutdown"`, `"action":"reboot"`, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			a := newApprovalEnv(t)
			a.require(t, a.laptop)
			id, _, ok := approvalAsked(a.post("/api/systems/laptop/power", a.shutdownBody()))
			if !ok {
				t.Fatal("not asked")
			}
			l := a.s.codec.live
			l.mu.Lock()
			p := l.m[sidOf(a.env, a.admin)].pending
			changed := edit(p.Body)
			if changed == p.Body {
				l.mu.Unlock()
				t.Fatal("the test did not change the kept body:", p.Body)
			}
			p.Body = changed
			l.mu.Unlock()
			if cb := a.approve(t, id); cb.code != 303 {
				t.Fatalf("callback: %d %s", cb.code, cb.raw)
			}
			pr := a.result(t)
			if pr["ok"] != false || !strings.Contains(fmt.Sprint(pr["message"]), "not the request that was approved") {
				t.Fatalf("result: %v", pr)
			}
			if len(a.laptop.Passed()) != 0 || strings.Contains(sentPaths(a.laptop.Asked()), "/power") {
				t.Fatalf("the machine was sent: %s", sentPaths(a.laptop.Asked()))
			}
		})
	}
}

// Machines whose approval is off, and agents without the block, are handled exactly as before: no challenge is
// asked, the panel's own confirmation and window apply, and the request carries no approval.
func TestApprovalOffIsAsBefore(t *testing.T) {
	a := newApprovalEnv(t) // both machines: no trust file, state off
	if ap := a.s.Poller.ApprovalOf("laptop"); ap == nil || ap.State != api.ApprovalOff {
		t.Fatalf("the block of a machine without trust: %+v", ap)
	}
	r := a.post("/api/systems/laptop/power", a.shutdownBody())
	d, _ := r.body["detail"].(map[string]any)
	if r.code != 401 || d["code"] != "step_up_required" || d["approval"] != nil || d["per_action"] != nil {
		t.Fatalf("an ordinary Confirm it's you: %d %s", r.code, r.raw)
	}
	fresh := a.cookie(controller(), 0)
	hdr := []string{"Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	for _, c := range []struct {
		path string
		body any
	}{
		{"/api/systems/laptop/power", a.shutdownBody()},
		{"/api/systems/tower/fan/mode", map[string]any{"mode": "full"}},
		{"/api/systems/tower/fan/duty", map[string]any{"zone": 0, "percent": 50}},
		{"/api/admin/updates/laptop", map[string]any{}},
	} {
		if r := a.do("POST", c.path, c.body, hdr...); r.code != 200 {
			t.Fatalf("%s inside the window: %d %s", c.path, r.code, r.raw)
		}
	}
	for _, m := range []*testutil.Machine{a.laptop, a.tower} {
		for _, s := range m.Asked() {
			if s.Path == api.PathApprovalChallenge || s.Body["approval"] != nil {
				t.Errorf("%s was asked %s (approval %v)", m.ID, s.Path, s.Body["approval"])
			}
		}
	}
	if got := sentPaths(a.laptop.Passed()) + " | " + sentPaths(a.tower.Passed()); got != "/power /update | /fan/mode /fan/duty" {
		t.Fatalf("sent: %s", got)
	}
	if len(a.iss.Asked()) != 0 {
		t.Fatal("Pocket ID was asked for an approval")
	}
}

// The fan window is the machine's: after one approved fan change, this session's further fan changes on that
// machine go through without another approval while the machine says so. A power action still needs its own.
func TestFanWindow(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.tower, a.laptop)
	id, machines, ok := approvalAsked(a.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"}))
	if !ok || machines[0] != "tower" {
		t.Fatal("the first fan change must ask for the machine's approval")
	}
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true {
		t.Fatalf("approved fan change: %d", cb.code)
	}
	if p := a.tower.Passed(); len(p) != 1 || p[0].Path != "/fan/mode" || !p[0].Approved || p[0].Body["mode"] != "full" {
		t.Fatalf("passed: %+v", p)
	}
	// inside the window: sent at once, without an approval and without the panel's own confirmation
	if r := a.post("/api/systems/tower/fan/duty", map[string]any{"zone": 0, "percent": 60}); r.code != 200 {
		t.Fatalf("a second fan change inside the machine's window: %d %s", r.code, r.raw)
	}
	if p := a.tower.Passed(); len(p) != 2 || p[1].Path != "/fan/duty" || p[1].Approved || p[1].Body["approval"] != nil {
		t.Fatalf("passed: %+v", p)
	}
	// another session of the same admin did not approve anything: the panel's own confirmation applies to it
	other := a.cookie(controller(), time.Hour)
	r := a.do("POST", "/api/systems/tower/fan/duty", map[string]any{"zone": 0, "percent": 70}, "Cookie", other, "X-Requested-With", "fairy", "Content-Type", "application/json")
	if d, _ := r.body["detail"].(map[string]any); r.code != 401 || d["approval"] != nil {
		t.Fatalf("another session inside the machine's window: %d %s", r.code, r.raw)
	}
	// a power action never uses the fan window
	if _, _, ok := approvalAsked(a.post("/api/systems/laptop/power", a.shutdownBody())); !ok {
		t.Fatal("a power action must ask for its own approval")
	}
	// after the window (the machine's clock): asked again
	later := time.Now().Add(31 * time.Second)
	a.tower.SetClock(func() time.Time { return later }, time.Hour)
	if _, _, ok := approvalAsked(a.post("/api/systems/tower/fan/duty", map[string]any{"zone": 0, "percent": 80})); !ok {
		t.Fatal("after the machine's fan window a fan change must ask again")
	}
	if len(a.tower.Passed()) != 2 {
		t.Fatalf("passed: %+v", a.tower.Passed())
	}
}

// An update of one agent: the approval is for "apply_update" on that machine.
func TestApprovedUpdate(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	id, _, ok := approvalAsked(a.post("/api/admin/updates/laptop", map[string]any{}))
	if !ok {
		t.Fatal("the update must ask for the machine's approval")
	}
	cb := a.approve(t, id)
	// what the page shows afterwards is "Update laptop: started. Its row shows how it goes.": the agent took the
	// request, and the row says how it went. It used to be "done" (audit 5, r2-b2-04).
	if res := a.result(t); cb.code != 303 || res["ok"] != true || res["message"] != "started. Its row shows how it goes." {
		t.Fatalf("approved update: %d %v", cb.code, res)
	}
	if js := appJS(t); !strings.Contains(js, "else if (r) flash(`${label}: ${r.message || \"started\"}`);") {
		t.Error("app.js: a single Update that ran at once must show the panel's own words too")
	}
	p := a.laptop.Passed()
	if len(p) != 1 || p[0].Path != "/update" || !p[0].Approved {
		t.Fatalf("passed: %+v", p)
	}
	if e := p[0].Body["approval"].(map[string]any)["entries"].([]any)[0].(map[string]any); e["request"] != "apply_update" {
		t.Fatalf("entry: %v", e)
	}
}

// A machine whose trust file cannot be used is refused cleanly: the answer says what is wrong and where it is
// repaired, nobody is asked for a passkey, and the audit log has the refusal.
func TestBrokenMachineIsRefusedClearly(t *testing.T) {
	a := newApprovalEnv(t)
	if err := os.WriteFile(a.laptop.TrustPath, []byte("{ this is not a trust file"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.poll()
	if ap := a.s.Poller.ApprovalOf("laptop"); ap == nil || ap.State != api.ApprovalBroken || ap.Error == "" {
		t.Fatalf("the block of a machine with a damaged trust file: %+v", ap)
	}
	for _, c := range []struct {
		path string
		body any
	}{{"/api/systems/laptop/power", a.shutdownBody()}, {"/api/admin/updates/laptop", map[string]any{}}} {
		r := a.post(c.path, c.body)
		msg := fmt.Sprint(r.body["detail"])
		if r.code != 409 || !strings.Contains(msg, "cannot check approvals right now") || !strings.Contains(msg, "fairy-agent helper-trust show") {
			t.Fatalf("%s: %d %s", c.path, r.code, r.raw)
		}
	}
	if len(a.laptop.Passed()) != 0 || len(a.iss.Asked()) != 0 || keptID(a.env, a.admin) != "" {
		t.Fatal("a broken machine: something was sent, asked or kept")
	}
	if got := auditOutcomes(a.s.Cfg.AuditLog); !strings.Contains(got, "power.shutdown error 409") || !strings.Contains(got, "update.agent error 409") {
		t.Errorf("audit:\n%s", got)
	}
	// the page's data says so too (a viewer sees the state, see TestApprovalStateIsVisible)
	x := a.do("GET", "/api/systems/laptop", nil, "Cookie", a.admin).body["approval"].(map[string]any)
	if x["state"] != "broken" || x["error"] == "" {
		t.Fatalf("the system's approval block: %v", x)
	}
}

// A machine that trusts another Pocket ID or another client than this panel asks is not sent to Pocket ID for:
// the approval would be refused there.
func TestMachineTrustsAnotherClient(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	a.s.Cfg.Approval.ClientID = "another-client"
	r := a.post("/api/systems/laptop/power", a.shutdownBody())
	if msg := fmt.Sprint(r.body["detail"]); r.code != 409 || !strings.Contains(msg, "another Pocket ID or client") || !strings.Contains(msg, testApprovalClient) {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	// and without an approval: block at all, the panel says that it has no client to ask with
	a.s.Cfg.Approval = config.Approval{}
	r = a.post("/api/systems/laptop/power", a.shutdownBody())
	if msg := fmt.Sprint(r.body["detail"]); r.code != 409 || !strings.Contains(msg, "no Pocket ID client") {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	if len(a.laptop.Passed()) != 0 || len(a.iss.Asked()) != 0 {
		t.Fatal("something was sent or asked")
	}
}

// Who may: an API token cannot give an approval (and spends none of the machine's requests finding that out),
// a viewer is refused before any machine is asked, and the checks of every write apply before the challenge.
func TestApprovalActionsAuthz(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop, a.tower)
	tok, _, err := a.s.Tokens.Create("script", approvalAdminSub, "control", "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	r := a.do("POST", "/api/systems/laptop/power", a.shutdownBody(), "Authorization", "Bearer "+tok, "Content-Type", "application/json")
	if r.code != 403 || !strings.Contains(fmt.Sprint(r.body["detail"]), "an API token cannot give one") {
		t.Fatalf("API token: %d %s", r.code, r.raw)
	}
	v := a.cookie(viewer, 0)
	for name, hdr := range map[string][]string{
		"a viewer":                {"Cookie", v, "X-Requested-With", "fairy", "Content-Type", "application/json"},
		"without the CSRF header": {"Cookie", a.admin, "Content-Type", "application/json"},
		"from another origin":     {"Cookie", a.admin, "X-Requested-With", "fairy", "Origin", "https://evil.example", "Content-Type", "application/json"},
		"not signed in":           {"X-Requested-With", "fairy", "Content-Type", "application/json"},
	} {
		if r := a.do("POST", "/api/systems/laptop/power", a.shutdownBody(), hdr...); r.code != 403 && r.code != 401 {
			t.Errorf("%s: %d %s", name, r.code, r.raw)
		}
		if r := a.do("POST", "/api/systems/tower/fan/mode", map[string]any{"mode": "full"}, hdr...); r.code != 403 && r.code != 401 {
			t.Errorf("%s (fan): %d %s", name, r.code, r.raw)
		}
	}
	if len(a.laptop.Asked())+len(a.tower.Asked()) != 0 || len(a.iss.Asked()) != 0 {
		t.Fatalf("a machine or Pocket ID was asked: %s | %s", sentPaths(a.laptop.Asked()), sentPaths(a.tower.Asked()))
	}
	// the kill switch stops it before the machine is asked
	if err := a.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "maintenance"); err != nil {
		t.Fatal(err)
	}
	if r := a.post("/api/systems/laptop/power", a.shutdownBody()); r.code != 423 || len(a.laptop.Asked()) != 0 {
		t.Fatalf("kill switch: %d, asked %d", r.code, len(a.laptop.Asked()))
	}
	_ = a.s.Poller.Admin.SetControlDisabled(false, "root", "")
	// a wrong typed name is refused before anybody is asked for a passkey
	bad := a.shutdownBody()
	bad["confirm"] = "not-the-name"
	if r := a.post("/api/systems/laptop/power", bad); r.code != 400 || len(a.laptop.Asked()) != 0 {
		t.Fatalf("wrong typed name: %d %s, asked %d", r.code, r.raw, len(a.laptop.Asked()))
	}
}

// A machine that answers the question about an approval for another request than the one asked is not trusted
// with anything.
func TestMachineAnswersForAnotherRequest(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	a.b.Mu.Lock()
	a.b.Reply = nil
	a.b.Mu.Unlock()
	a.gb.Lie("laptop", func(path string) (int, jsonx.M) {
		if path == api.PathApprovalChallenge {
			return 200, jsonx.M{"needed": true, "challenge": testChallenge(3), "request": "power action=reboot delay_s=10",
				"issuer": a.iss.URL(), "client_id": testApprovalClient, "expires_in_s": 180.0}
		}
		return 0, nil
	})
	r := a.post("/api/systems/laptop/power", a.shutdownBody())
	if r.code != 424 || !strings.Contains(fmt.Sprint(r.body["detail"]), "another request than the one asked") || keptID(a.env, a.admin) != "" {
		t.Fatalf("%d %s", r.code, r.raw)
	}
}

// Everyone who can see a machine sees its approval state; nothing in it is a secret.
func TestApprovalStateIsVisible(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	u := viewer
	u.Sub = "viewer-9"
	v := a.cookie(u, 0)
	if err := a.s.Access.CreateSite("Den"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.Access.Assign("laptop", "Den"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.s.Access.SetRole(u.Sub, "viewer", []string{"Den"}, "test", true); err != nil {
		t.Fatal(err)
	}
	r := a.do("GET", "/api/systems/laptop", nil, "Cookie", v)
	if r.code != 200 {
		t.Fatalf("the viewer cannot see laptop: %d %s", r.code, r.raw)
	}
	// and no action: the viewer is refused before the machine is asked
	if r := a.do("POST", "/api/systems/laptop/power", a.shutdownBody(), "Cookie", v, "X-Requested-With", "fairy", "Content-Type", "application/json"); r.code != 403 || len(a.laptop.Asked()) != 0 {
		t.Fatalf("a viewer's shutdown: %d, the machine was asked %d times", r.code, len(a.laptop.Asked()))
	}
	x, _ := r.body["approval"].(map[string]any)
	if x == nil || x["state"] != "required" || x["enforced_by"] != "agent" || x["users"] != 1.0 || x["issuer"] != a.iss.URL() ||
		len(x["keys"].([]any)) != 1 || x["fan_window_s"] != 30.0 {
		t.Fatalf("approval block for a viewer: %v", x)
	}
	// the id as a value of its own: "u2" is two characters, and the answer carries a key (base64) and a
	// thumbprint in which they turn up by chance once in a few hundred runs
	if strings.Contains(string(r.raw), `"`+approvalAdminSub+`"`) {
		t.Errorf("the allowed users' ids are not part of the block: %s", r.raw)
	}
	if list, isList := x["users"].([]any); isList {
		t.Errorf("the block lists the allowed users: %v", list)
	}
}

// The panel's view of a machine can be a poll behind: a machine whose check was switched on a moment ago, on
// the machine itself, still reads "off". The panel then sends the request as before and the machine refuses it
// ("approval_required"). The panel has no machine id on record for it, and it never takes one from an answer
// that comes with a challenge: so nobody is sent to Pocket ID, the page says what to do, and nothing passes.
// With the machine's next answer the panel sees the check. CHANGED (audit 5, round 3, r3-x1-01): the id does NOT
// come on record by that; the machine is "not on record" and still gets no approval. Only after an admin has
// accepted the id it shows does the same button ask for the approval.
func TestCheckSwitchedOnSinceTheLastPoll(t *testing.T) {
	for name, c := range map[string]struct {
		path string
		body func(a *approvalEnv) any
		m    func(a *approvalEnv) *testutil.Machine
		sent string
	}{
		"shutdown": {"/api/systems/laptop/power", func(a *approvalEnv) any { return a.shutdownBody() }, func(a *approvalEnv) *testutil.Machine { return a.laptop }, "/power"},
		"update":   {"/api/admin/updates/laptop", func(*approvalEnv) any { return map[string]any{} }, func(a *approvalEnv) *testutil.Machine { return a.laptop }, "/update"},
		"fan mode": {"/api/systems/tower/fan/mode", func(*approvalEnv) any { return map[string]any{"mode": "full"} }, func(a *approvalEnv) *testutil.Machine { return a.tower }, "/fan/mode"},
	} {
		t.Run(name, func(t *testing.T) {
			a := newApprovalEnv(t)
			m := c.m(a)
			a.setTrust(t, m) // on the machine; the panel still believes "off"
			if ap := a.s.Poller.ApprovalOf(m.ID); ap == nil || ap.State != api.ApprovalOff {
				t.Fatalf("the panel's view: %+v", ap)
			}
			fresh := a.cookie(controller(), 0) // inside the panel's own window: nothing is asked by the panel itself
			hdr := []string{"Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json"}
			tokens := len(a.iss.Tokens())
			r := a.do("POST", c.path, c.body(a), hdr...)
			// (audit 6, r4-p3-05 / r4-b1-02: the page has no "Approval box" any more; it was "... accepts it in the Approval box on its page first.")
			const notOnRecord = " checks approvals itself, and its machine id is not on record: an admin accepts it first (press Approval on its page). Nothing was sent."
			if _, _, ok := approvalAsked(r); ok || r.code != 409 || !strings.Contains(string(r.raw), m.ID+notOnRecord) {
				t.Fatalf("without an id on record nobody is asked for a passkey: %d %s", r.code, r.raw)
			}
			if len(m.Passed()) != 0 || len(a.iss.Tokens()) != tokens {
				t.Fatalf("something passed the gate, or somebody was sent to Pocket ID: %+v", m.Passed())
			}
			a.poll() // the machine's next answer: the panel sees the check, and takes nothing on record
			if b := a.s.Poller.BindingOf(m.ID); b.Status != approval.BindNoRecord || b.Record.MachineID != "" || !b.Checks() {
				t.Fatalf("the machine must be \"not on record\" after its next answer: %+v", b)
			}
			asked := len(m.Asked())
			r = a.do("POST", c.path, c.body(a), hdr...)
			if _, _, ok := approvalAsked(r); ok || r.code != 409 || !strings.Contains(string(r.raw), m.ID+notOnRecord) {
				t.Fatalf("a machine that is not on record is asked for no approval: %d %s", r.code, r.raw)
			}
			if len(m.Asked()) != asked || len(m.Passed()) != 0 || len(a.iss.Tokens()) != tokens {
				t.Fatalf("a machine that is not on record was asked or sent something: %+v", m.Asked()[asked:])
			}
			a.acceptShown(t, m) // an admin accepts the id it shows
			r = a.do("POST", c.path, c.body(a), hdr...)
			id, _, ok := approvalAsked(r)
			if !ok {
				t.Fatalf("with the id on record the button must ask for the approval: %d %s", r.code, r.raw)
			}
			if len(m.Passed()) != 0 {
				t.Fatalf("something passed the gate: %+v", m.Passed())
			}
			a.admin, a.csrf[1] = fresh, fresh
			if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true {
				t.Fatalf("approving it: %d", cb.code)
			}
			if p := m.Passed(); len(p) != 1 || p[0].Path != c.sent || !p[0].Approved {
				t.Fatalf("passed: %+v", p)
			}
		})
	}
}

// selfUpdating makes "tower" a second machine with self-update, as config.yaml would have it.
func (a *approvalEnv) selfUpdating(t *testing.T) {
	t.Helper()
	ag, _ := a.s.Poller.AgentByID("tower")
	ag.SelfUpdate, ag.ControlTokenEnv = true, "CTL_TOWER"
	a.s.Poller.RemoveAgent("tower")
	if err := a.s.Poller.AddAgent(ag, nil); err != nil {
		t.Fatal(err)
	}
	a.b.Mu.Lock()
	a.b.Metrics["tower"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["hostname"], m["agent_version"] = "tower", "41-aaaaaaa"
		m["features"].(jsonx.M)["fan_control"], m["features"].(jsonx.M)["self_update"] = true, true
		m["self_update"] = jsonx.M{"state": "idle", "since": 1.0}
		return m, nil
	}
	a.b.Mu.Unlock()
	a.poll()
}

func updateResults(r map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	list, _ := r["results"].([]any)
	for _, x := range list {
		out[x.(map[string]any)["id"].(string)] = x.(map[string]any)
	}
	return out
}

// "Update all": one approval covers the updates of all machines that check approvals themselves. Each machine
// gets the same list in the same order and finds its own entry in it.
func TestUpdateAllWithOneApproval(t *testing.T) {
	a := newApprovalEnv(t)
	a.selfUpdating(t)
	a.require(t, a.laptop, a.tower)
	r := a.post("/api/admin/updates", map[string]any{})
	id, machines, ok := approvalAsked(r)
	if !ok || len(machines) != 2 {
		t.Fatalf("the dialog: %d %s", r.code, r.raw)
	}
	items := fmt.Sprint(r.body["detail"].(map[string]any)["items"])
	if !strings.Contains(items, "laptop: update of its agent (checked on the machine)") || !strings.Contains(items, "tower: update of its agent (checked on the machine)") {
		t.Fatalf("the dialog's lines: %s", items)
	}
	if cb := a.approve(t, id); cb.code != 303 {
		t.Fatalf("callback: %d %s", cb.code, cb.raw)
	}
	if pr := a.result(t); pr["ok"] != true {
		t.Fatalf("result: %v", pr)
	}
	if len(a.iss.Tokens()) != 1 {
		t.Fatalf("%d approvals for one batch", len(a.iss.Tokens()))
	}
	var lists []string
	for _, m := range []*testutil.Machine{a.laptop, a.tower} {
		p := m.Passed()
		if len(p) != 1 || p[0].Path != "/update" || !p[0].Approved {
			t.Fatalf("%s: %+v", m.ID, p)
		}
		lists = append(lists, fmt.Sprint(p[0].Body["approval"].(map[string]any)["entries"]))
	}
	if lists[0] != lists[1] || strings.Count(lists[0], "apply_update") != 2 {
		t.Fatalf("the machines got different lists:\n%s\n%s", lists[0], lists[1])
	}
	if got := auditOutcomes(a.s.Cfg.AuditLog); strings.Count(got, "update.agent ok") != 2 {
		t.Errorf("audit:\n%s", got)
	}
}

// A mixed batch: one machine checks approvals, one does not. Still one confirmation: the approval for the
// first is the confirmation for both, and only the first gets it.
func TestUpdateAllMixed(t *testing.T) {
	a := newApprovalEnv(t)
	a.selfUpdating(t)
	a.require(t, a.laptop)
	r := a.post("/api/admin/updates", map[string]any{})
	id, machines, ok := approvalAsked(r)
	if !ok || len(machines) != 1 || machines[0] != "laptop" {
		t.Fatal("the dialog names the machine that checks")
	}
	// and it lists the machine the same passkey updates without a check of its own (audit 5, r1-b2-04)
	if items := fmt.Sprint(r.body["detail"].(map[string]any)["items"]); items != "[laptop: update of its agent (checked on the machine) tower: update of its agent (no check on the machine)]" {
		t.Fatalf("the dialog's lines: %s", items)
	}
	if len(a.tower.Asked()) != 0 {
		t.Fatalf("a machine without the check was asked: %+v", a.tower.Asked())
	}
	if cb := a.approve(t, id); cb.code != 303 || a.result(t)["ok"] != true {
		t.Fatal("approving the batch")
	}
	if p := a.laptop.Passed(); len(p) != 1 || !p[0].Approved {
		t.Fatalf("laptop: %+v", p)
	}
	if p := a.tower.Passed(); len(p) != 1 || p[0].Path != "/update" || p[0].Body["approval"] != nil {
		t.Fatalf("tower: %+v", p)
	}
	if len(a.iss.Asked()) != 1 {
		t.Fatalf("Pocket ID was asked %d times", len(a.iss.Asked()))
	}
}

// A batch with a machine whose trust file is broken: that one is reported and not sent to, the other is
// updated with its approval. With nothing but the broken one, nobody is asked for a passkey.
func TestUpdateAllWithABrokenMachine(t *testing.T) {
	a := newApprovalEnv(t)
	a.selfUpdating(t)
	a.require(t, a.laptop)
	if err := os.WriteFile(a.tower.TrustPath, []byte("not a trust file"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.poll()
	r := a.post("/api/admin/updates", map[string]any{})
	id, machines, ok := approvalAsked(r)
	if !ok || len(machines) != 1 || !strings.Contains(fmt.Sprint(r.body["detail"].(map[string]any)["items"]), "tower: NOT sent (tower cannot check approvals right now") {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	if cb := a.approve(t, id); cb.code != 303 {
		t.Fatal(cb.code)
	}
	a.result(t)
	if p := a.laptop.Passed(); len(p) != 1 || !p[0].Approved {
		t.Fatalf("laptop: %+v", p)
	}
	if len(a.tower.Passed()) != 0 {
		t.Fatalf("the broken machine was sent: %+v", a.tower.Passed())
	}
	if got := auditOutcomes(a.s.Cfg.AuditLog); !strings.Contains(got, "update.agent ok") || !strings.Contains(got, "update.agent error 409") {
		t.Errorf("audit:\n%s", got)
	}
	// a broken machine next to one that is not asked at all: the panel's own confirmation still stands for
	// the other one (the broken machine's refusal is no confirmation)
	c := newApprovalEnv(t)
	c.selfUpdating(t)
	if err := os.WriteFile(c.laptop.TrustPath, []byte("not a trust file"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.poll()
	r = c.post("/api/admin/updates", map[string]any{}) // the admin's sign-in is an hour old
	if d, _ := r.body["detail"].(map[string]any); r.code != 401 || d["code"] != "step_up_required" || d["approval"] != nil || len(c.tower.Passed()) != 0 {
		t.Fatalf("a batch with a broken machine and one without the check: %d %s (tower was sent %d)", r.code, r.raw, len(c.tower.Passed()))
	}
	// only the broken machine can be updated: said in the results, without a passkey
	b := newApprovalEnv(t)
	if err := os.WriteFile(b.laptop.TrustPath, []byte("not a trust file"), 0o600); err != nil {
		t.Fatal(err)
	}
	b.poll()
	fresh := b.cookie(controller(), 0)
	r = b.do("POST", "/api/admin/updates", map[string]any{}, "Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json")
	res := updateResults(r.body)
	if r.code != 200 || res["laptop"]["ok"] != false || !strings.Contains(fmt.Sprint(res["laptop"]["error"]), "cannot check approvals right now") || len(b.iss.Asked()) != 0 {
		t.Fatalf("%d %s", r.code, r.raw)
	}
}
