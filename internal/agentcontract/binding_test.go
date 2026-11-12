package agentcontract_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	agentapi "github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Which machine an approval is for (audit 5, r1-x1-01), with the real agent on both ends: the agent's own
// handler and gate (startGated), the panel's real routes, a sign-in and approvals at the stand-in issuer.
//
// The finding: the approval named a challenge and a request, nothing of the machine. A host whose answers
// cannot be trusted (a hostile agent, or somebody on the plain-HTTP path to it) answered the panel's question
// with a challenge it had fetched from ANOTHER machine, and that machine then accepted the approval the admin
// had given for the hostile host. Every approval now names the machine id the panel has ON RECORD for the host
// it asked (internal/approval, binding.go), and a machine accepts only an entry with its own id.

// front is a host whose answers the test decides: the panel reaches the machine `real` through it.
type front struct {
	real *gated
	srv  *httptest.Server
	mu   sync.Mutex
	hook func(r *http.Request, body []byte, w http.ResponseWriter) (handled bool)
}

func (f *front) set(h func(r *http.Request, body []byte, w http.ResponseWriter) bool) {
	f.mu.Lock()
	f.hook = h
	f.mu.Unlock()
}

func newFront(t *testing.T, real *gated) *front {
	f := &front{real: real}
	inner := real.srv.Config.Handler
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		h := f.hook
		f.mu.Unlock()
		if h != nil {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			if h(r, body, w) {
				return
			}
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func answer(w http.ResponseWriter, status int, out map[string]any) {
	raw, _ := json.Marshal(out)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// frontedPanel is newApprovalPanel with pc-b behind a front.
func frontedPanel(t *testing.T) (*approvalPanel, *front) {
	t.Helper()
	pn := &approvalPanel{iss: testutil.NewIssuer(t), a: startGated(t, "pc-a"), b: startGated(t, "pc-b"), dir: t.TempDir()}
	fb := newFront(t, pn.b)
	return startPanel(t, pn, map[string]string{"pc-b": fb.srv.URL}), fb
}

// relayTo makes the front hand every question and every control request of the panel on to the machine
// `to`, and answer with what that machine said (lie nil) or with lie's answer.
func (f *front) relayTo(t *testing.T, to *gated, got *[]relayed, lie func(path string) (int, map[string]any)) {
	f.set(func(r *http.Request, body []byte, w http.ResponseWriter) bool {
		if r.Method != http.MethodPost {
			return false // it goes on showing its own /metrics
		}
		st, out := to.direct(t, r.URL.Path, body)
		f.mu.Lock()
		*got = append(*got, relayed{r.URL.Path, body, st, out})
		f.mu.Unlock()
		if lie != nil && r.URL.Path != agentapi.PathApprovalChallenge {
			st, out = lie(r.URL.Path)
		}
		answer(w, st, out)
		return true
	})
}

// relayed is one request the front handed on: what the panel sent, and what the other machine answered.
type relayed struct {
	path string
	body []byte
	st   int
	out  map[string]any
}

// auditLines are the audit entries with the action, in order.
func (pn *approvalPanel) auditLines(action string) []map[string]any {
	pn.t.Helper()
	raw, _ := os.ReadFile(pn.audit)
	var out []map[string]any
	for _, l := range strings.Split(string(raw), "\n") {
		e := map[string]any{}
		if json.Unmarshal([]byte(l), &e) == nil && e["action"] == action {
			out = append(out, e)
		}
	}
	return out
}

// machines is the approval page (admins, in a browser): what the panel has on record, by host.
func (pn *approvalPanel) machines() map[string]map[string]any {
	pn.t.Helper()
	st, out := pn.call("GET", "/api/admin/approval", nil)
	if st != 200 {
		pn.t.Fatalf("the approval page: %d %v", st, out)
	}
	ms := map[string]map[string]any{}
	for _, m := range out["machines"].([]any) {
		ms[m.(map[string]any)["id"].(string)] = m.(map[string]any)
	}
	return ms
}

// settle lets the panel ask the machines until it has seen what the test just did to one of them: a machine
// computes its answer at most every 250 ms, and on a busy computer one round can miss an answer.
func (pn *approvalPanel) settle(host, binding string) {
	pn.t.Helper()
	for i := 0; i < 20; i++ {
		pn.poll()
		if b, _ := pn.machines()[host]["binding"].(string); b == binding {
			return
		}
	}
	pn.t.Fatalf("%s: the panel never came to %q: %v", host, binding, pn.machines()[host])
}

// stopped: the panel itself refused (409) with the sentence, and asked nobody to confirm anything.
// acceptBody is what the button in the Approval box sends for the agent: what the approval page shows for it
// now (the machine id it shows, in full, or that its check is off).
func (pn *approvalPanel) acceptBody(agent string) map[string]any {
	pn.t.Helper()
	m := pn.machines()[agent]
	if m["can_accept"] != true {
		pn.t.Fatalf("%s has nothing to accept on the approval page: %v", agent, m)
	}
	if m["accept"] == "off" {
		return map[string]any{"off": true}
	}
	return map[string]any{"machine_id": m["shown_id"]}
}

func stopped(t *testing.T, what string, st int, out map[string]any, sentence string) {
	t.Helper()
	if d, _ := out["detail"].(string); st != 409 || !strings.Contains(d, sentence) || !strings.Contains(d, "Nothing was sent.") {
		t.Fatalf("%s must be stopped by the panel (%q): %d %v", what, sentence, st, out)
	}
}

// confirm is "Confirm it's you" for a kept action that no machine is asked about: Continue, the sign-in at the
// issuer, the panel's callback, and the result of the action the panel then ran.
func (pn *approvalPanel) confirm(st int, out map[string]any) map[string]any {
	pn.t.Helper()
	d, _ := out["detail"].(map[string]any)
	id, _ := d["pending_id"].(string)
	if st != 401 || d["code"] != "step_up_required" || d["approval"] != nil || id == "" {
		pn.t.Fatalf("the panel must ask to confirm: %d %v", st, out)
	}
	st, out = pn.call("POST", "/api/me/step-up", map[string]any{"pending_id": id, "next": "/"})
	if st != 200 {
		pn.t.Fatalf("Continue: %d %v", st, out)
	}
	back := pn.iss.Approve(pn.t, fmt.Sprint(out["url"]))
	if resp := pn.get(back.RequestURI()); resp.StatusCode != 303 {
		pn.t.Fatalf("the sign-in's callback: %d", resp.StatusCode)
	}
	_, me := pn.call("GET", "/api/me", nil)
	res, _ := me["pending_result"].(map[string]any)
	if res == nil {
		pn.t.Fatal("the confirmed action did not run: no pending_result in /api/me")
	}
	return res
}

// setMachineID gives the machine another id in its own trust file, as `fairy-agent helper-trust set
// --new-machine-id` does on the machine.
func (g *gated) setMachineID(t *testing.T, id string) {
	t.Helper()
	raw, err := os.ReadFile(g.trust)
	f := map[string]any{}
	if err != nil || json.Unmarshal(raw, &f) != nil {
		t.Fatalf("%s: its trust file: %v", g.id, err)
	}
	f["machine_id"] = id
	raw, _ = json.Marshal(f)
	if err := os.WriteFile(g.trust, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if st := g.gate.StatusNow(); st.State != agentapi.ApprovalRequired || st.MachineID != id {
		t.Fatalf("%s after the new id: %+v", g.id, st)
	}
}

const otherMachineID = "another-id-0123456789abcdef"

// The auditor's case, r1-x1 (TestR1X1Binding_ApprovalNotBoundToMachine), on the fixed tree. pc-a and pc-b both
// have the check on. The attacker holds the control token and decides what pc-b answers. The admin chooses
// "shut down pc-b"; pc-b answers the panel's question with a challenge it fetched from pc-a for the same
// request, and hands the approved request on to pc-a.
//
// It used to end with pc-a scheduled to power off and "pc-b" in the dialog, the result and the audit log. Now:
// the approval names the id on record for pc-b, pc-a refuses it ("nonce"), nothing is scheduled anywhere, and
// the refusal is shown and audited for pc-b.
func TestBindingARelayedChallengeIsRefused(t *testing.T) {
	pn, fb := frontedPanel(t)
	pn.turnOn("pc-a", "pc-b") // real Turn on: both locked, each with the id the panel made up for it
	idA, idB := pn.a.machineID(t), pn.b.machineID(t)
	if ms := pn.machines(); idA == idB || ms["pc-a"]["machine_id"] != idA || ms["pc-b"]["machine_id"] != idB || ms["pc-b"]["pinned"] != approval.PinTurnOn {
		t.Fatalf("on record: %v / %v, the machines have %s / %s", ms["pc-a"], ms["pc-b"], idA, idB)
	}
	if st, out := pn.a.direct(t, "/power", []byte(`{"action":"shutdown","delay_s":60}`)); st != 403 || out["code"] != agentapi.CodeApprovalRequired {
		t.Fatalf("baseline: pc-a must refuse a shutdown with the token and no approval: %d %v", st, out)
	}
	var got []relayed
	fb.relayTo(t, pn.a, &got, nil)
	before := len(pn.b.sent("/power")) + len(pn.b.sent(agentapi.PathApprovalChallenge))

	st, out := pn.shutdown(pn.b, 60) // the admin acts on pc-b
	id, machines, _, ok := asked(st, out)
	if !ok || len(machines) != 1 || machines[0] != "PC-B" {
		t.Fatalf("the shutdown of pc-b did not reach the dialog: %d %v", st, out)
	}
	res := pn.approve(id)
	if len(pn.a.power.Scheduled) != 0 || len(pn.b.power.Scheduled) != 0 {
		t.Fatalf("something was scheduled: pc-a %v, pc-b %v", pn.a.power.Scheduled, pn.b.power.Scheduled)
	}
	// what pc-a said to the relayed request: its own verifier refused, because the entry names another machine
	if len(got) != 2 || got[0].path != agentapi.PathApprovalChallenge || got[0].st != 200 || got[1].path != "/power" || got[1].st != 403 ||
		got[1].out["code"] != agentapi.CodeApprovalRefused || !strings.Contains(fmt.Sprint(got[1].out["error"]), "approval refused (nonce)") {
		t.Fatalf("what pc-a answered to the relayed requests: %+v", got)
	}
	// the entry the panel made names the id on record for pc-b, with pc-a's challenge in it; the real pc-b
	// was never asked
	var sent struct {
		Approval struct {
			Entries []agentapi.MachineEntry `json:"entries"`
		} `json:"approval"`
	}
	if err := json.Unmarshal(got[1].body, &sent); err != nil || len(sent.Approval.Entries) != 1 || sent.Approval.Entries[0].MachineID != idB ||
		sent.Approval.Entries[0].Challenge != got[0].out["challenge"] {
		t.Fatalf("the approval the panel sent: %v %+v", err, sent.Approval.Entries)
	}
	if n := len(pn.b.sent("/power")) + len(pn.b.sent(agentapi.PathApprovalChallenge)); n != before {
		t.Fatalf("the real pc-b got %d requests", n-before)
	}
	// the refusal is shown ...
	if msg := fmt.Sprint(res["message"]); res["ok"] != false || !strings.Contains(msg, "PC-B did not accept the approval") ||
		!strings.Contains(msg, "What answered is not the machine Fairy has on record for this host") {
		t.Fatalf("the result shown to the admin: %v", res)
	}
	// ... and audited for pc-b; nothing in the log says pc-a was switched off
	lines := pn.auditLines("power.shutdown")
	if len(lines) != 1 || lines[0]["target"] != "pc-b" || !strings.HasPrefix(fmt.Sprint(lines[0]["outcome"]), "error 4") ||
		!strings.Contains(fmt.Sprint(lines[0]["outcome"]), "PC-B did not accept the approval (PC-B says: approval refused (nonce)") {
		t.Fatalf("the audit entries of the shutdown: %v", lines)
	}
	// the admin's approval is used up for nothing: pc-a's own challenge was consumed or is still refused
	if st, out := pn.a.direct(t, "/power", []byte(`{"action":"shutdown","delay_s":60}`)); st != 403 {
		t.Fatalf("pc-a afterwards: %d %v", st, out)
	}

	// The same with a host that lies about the answer ("scheduled"): the panel can only show what the host
	// says about itself, but the other machine is still not switched off.
	got = nil
	fb.relayTo(t, pn.a, &got, func(string) (int, map[string]any) {
		return 200, map[string]any{"action": "shutdown", "pending": map[string]any{"action": "shutdown", "delay_s": 60}}
	})
	st, out = pn.shutdown(pn.b, 60)
	if id, _, _, ok = asked(st, out); !ok {
		t.Fatalf("the second shutdown of pc-b: %d %v", st, out)
	}
	pn.approve(id)
	if len(pn.a.power.Scheduled) != 0 || len(pn.b.power.Scheduled) != 0 || len(got) != 2 || got[1].st != 403 {
		t.Fatalf("with a lying host: pc-a %v, pc-b %v, relayed %+v", pn.a.power.Scheduled, pn.b.power.Scheduled, got)
	}
	pn.noLeak()
}

// A laundered Turn on: pc-a has no check yet. The admin switches the check on for pc-b, and pc-b hands the
// question and the proposal on to pc-a. pc-a takes the trust (the proof is bound to ITS challenge), with the
// machine id the panel made up for pc-b. Nothing is gained by that: the values are the admin's. But the panel
// now has one id for two hosts, sees it with pc-a's next answer, and flags both: neither is asked for an
// approval or sent anything until an admin has looked.
func TestBindingALaunderedTurnOnEndsAsOneIDOnTwoHosts(t *testing.T) {
	pn, fb := frontedPanel(t)
	var got []relayed
	fb.relayTo(t, pn.a, &got, nil)
	res := pn.turnOn("pc-b")
	if res["ok"] != true {
		t.Fatalf("the Turn on the admin saw: %v", res)
	}
	if _, err := os.Stat(pn.b.trust); err == nil {
		t.Fatal("pc-b itself has a trust file")
	}
	idA := pn.a.machineID(t) // pc-a took the trust, and the id that was made up for pc-b
	fb.set(nil)
	pn.settle("pc-a", approval.BindDuplicate)
	pn.settle("pc-b", approval.BindDuplicate)
	ms := pn.machines()
	if ms["pc-b"]["machine_id"] != idA || ms["pc-b"]["binding"] != approval.BindDuplicate || ms["pc-a"]["binding"] != approval.BindDuplicate ||
		fmt.Sprint(ms["pc-a"]["with"]) != "[PC-B]" || fmt.Sprint(ms["pc-b"]["with"]) != "[PC-A]" {
		t.Fatalf("both hosts must be flagged as one id on two hosts: pc-a %v / pc-b %v", ms["pc-a"], ms["pc-b"])
	}
	// CHANGED (audit 5, round 3, r3-x1-01): nothing came on record for pc-a because it shows the id. The id is
	// on record for pc-b, where the panel issued it; pc-a's box names pc-b and has no button. (It used to be on
	// record for both, "on a par", each with the button "This host keeps its machine id".) pc-b shows no check
	// of its own, so it has nothing to accept either.
	if a, b := ms["pc-a"], ms["pc-b"]; a["machine_id"] != nil || a["pinned"] != nil || a["can_accept"] != nil || fmt.Sprint(a["held_by"]) != "[PC-B]" ||
		b["pinned"] != approval.PinTurnOn || b["can_accept"] != nil {
		t.Fatalf("what is on record, and who has a button: pc-a %v / pc-b %v", a, b)
	}
	if ap := pn.state("pc-a"); ap["binding"] != approval.BindDuplicate {
		t.Fatalf("pc-a's block for everyone who sees it: %v", ap)
	}
	asked0 := len(pn.a.sent(agentapi.PathApprovalChallenge))
	for g, sentence := range map[*gated]string{pn.a: "PC-A has the same machine id as PC-B, which keeps it", pn.b: "PC-B and PC-A have the same machine id"} {
		st, out := pn.shutdown(g, 60)
		stopped(t, "the shutdown of "+g.id, st, out, sentence)
		st, out = pn.call("POST", "/api/admin/updates/"+g.id, map[string]any{})
		stopped(t, "the update of "+g.id, st, out, sentence)
	}
	if len(pn.a.sent(agentapi.PathApprovalChallenge)) != asked0 || len(pn.a.sent("/power"))+len(pn.b.sent("/power"))+len(pn.a.sent("/update"))+len(pn.b.sent("/update")) != 0 ||
		len(pn.a.power.Scheduled)+len(pn.b.power.Scheduled) != 0 {
		t.Fatal("a flagged machine was asked or sent something")
	}
	// the alarm is in the audit log, for both, without an id in full
	al := pn.auditLines("approval.alarm")
	targets := map[string]string{}
	for _, e := range al {
		targets[fmt.Sprint(e["target"])] = fmt.Sprint(e["outcome"])
	}
	if targets["pc-a"] != "alarm: duplicate" || targets["pc-b"] != "alarm: duplicate" {
		t.Fatalf("the alarms in the audit log: %v", al)
	}
	if raw, _ := os.ReadFile(pn.audit); strings.Contains(string(raw), idA) {
		t.Fatal("the audit log has a machine id in full")
	}
	// the attacker's token still opens nothing on pc-a: it checks approvals now
	if st, out := pn.a.direct(t, "/power", []byte(`{"action":"shutdown","delay_s":60}`)); st != 403 || out["code"] != agentapi.CodeApprovalRequired {
		t.Fatalf("pc-a with the token alone: %d %v", st, out)
	}
	// The id pc-a holds is on record for pc-b, and one id is accepted for one host only: an accept for pc-a,
	// made up without the page, is refused.
	if st, out := pn.call("POST", "/api/admin/approval/accept/pc-a", map[string]any{"machine_id": idA}); st != 409 ||
		!strings.Contains(fmt.Sprint(out["detail"]), "PC-A: nothing was accepted. The machine id it shows is kept by PC-B") {
		t.Fatalf("a made-up accept on pc-a: %d %v", st, out)
	}
	// The way out is on the machine, as pc-a's box says: pc-a gets an id of its own (helper-trust set
	// --new-machine-id), an admin accepts that one in the panel, and pc-a is approved again by approvals made
	// for it alone.
	pn.a.setMachineID(t, otherMachineID)
	pn.settle("pc-a", approval.BindNoRecord)
	if m := pn.machines()["pc-a"]; m["can_accept"] != true || m["accept"] != "first" || m["shown_id"] != otherMachineID || m["held_by"] != nil {
		t.Fatalf("pc-a with an id of its own: %v", m)
	}
	if res := pn.confirm(pn.call("POST", "/api/admin/approval/accept/pc-a", pn.acceptBody("pc-a"))); res["ok"] != true || res["message"] != "PC-A: the machine id it shows is on record now." {
		t.Fatalf("the accept on pc-a: %v", res)
	}
	st, out := pn.shutdown(pn.a, 60)
	id, _, _, ok := asked(st, out)
	if !ok {
		t.Fatalf("pc-a after the accept: %d %v", st, out)
	}
	if res := pn.approve(id); res["ok"] != true || len(pn.a.power.Scheduled) != 1 || len(pn.b.power.Scheduled) != 0 {
		t.Fatalf("the approved shutdown of pc-a: %v, scheduled %v", res, pn.a.power.Scheduled)
	}
	// pc-b, the host that handed the Turn on on: the id that is on record for it is nobody's now, and it shows
	// no check itself. It is "Check gone", gets nothing, and an admin can accept that its check is off.
	pn.settle("pc-b", approval.BindVanished)
	st, out = pn.shutdown(pn.b, 60)
	stopped(t, "the shutdown of pc-b after pc-a has its own id", st, out, "PC-B had the check on and now shows none")
	if m := pn.machines(); m["pc-b"]["can_accept"] != true || m["pc-b"]["accept"] != "off" || m["pc-a"]["binding"] != nil {
		t.Fatalf("the approval page at the end: pc-a %v / pc-b %v", m["pc-a"], m["pc-b"])
	}
}

// The id a machine shows is not the one on record ("identity changed"): no question, no approval, a flag. One
// action of an admin in a browser, behind "Confirm it's you", takes the new id on record; the next approval is
// made with it and the real machine accepts it.
func TestBindingIdentityChangedAndAccept(t *testing.T) {
	pn := newApprovalPanel(t)
	pn.turnOn("pc-a", "pc-b")
	idB := pn.b.machineID(t)
	pn.b.setMachineID(t, otherMachineID)
	pn.settle("pc-b", approval.BindChanged)
	if ms := pn.machines(); ms["pc-b"]["binding"] != approval.BindChanged || ms["pc-b"]["machine_id"] != idB || ms["pc-b"]["shown_id"] != otherMachineID ||
		ms["pc-b"]["can_accept"] != true || ms["pc-a"]["binding"] != nil {
		t.Fatalf("the approval page: pc-b %v / pc-a %v", ms["pc-b"], ms["pc-a"])
	}
	if ap := pn.state("pc-b"); ap["binding"] != approval.BindChanged || ap["machine_id"] != nil {
		t.Fatalf("pc-b's block: %v", ap)
	}
	asked0, tokens0 := len(pn.b.sent(agentapi.PathApprovalChallenge)), len(pn.iss.Tokens())
	st, out := pn.shutdown(pn.b, 60)
	stopped(t, "the shutdown of pc-b", st, out, "shows another machine id than the one Fairy has on record")
	st, out = pn.call("POST", "/api/systems/pc-b/fan/mode", map[string]any{"mode": "full"})
	stopped(t, "the fan mode of pc-b", st, out, "shows another machine id")
	st, out = pn.call("POST", "/api/admin/updates/pc-b", map[string]any{})
	stopped(t, "the update of pc-b", st, out, "shows another machine id")
	if len(pn.b.sent(agentapi.PathApprovalChallenge)) != asked0 || len(pn.iss.Tokens()) != tokens0 || len(pn.b.sent("/power"))+len(pn.b.sent("/update"))+len(pn.b.sent("/fan/mode")) != 0 {
		t.Fatal("pc-b was asked for a challenge or sent something, or somebody was sent to Pocket ID")
	}
	// pc-a is not affected
	st, out = pn.shutdown(pn.a, 60)
	if _, _, _, ok := asked(st, out); !ok {
		t.Fatalf("pc-a must still be asked: %d %v", st, out)
	}
	if al := pn.auditLines("approval.alarm"); len(al) != 1 || al[0]["target"] != "pc-b" || al[0]["outcome"] != "alarm: changed" {
		t.Fatalf("the alarm in the audit log: %v", al)
	}

	// who may accept: nobody without the session of an admin in a browser
	const path = "/api/admin/approval/accept/pc-b"
	ctl, _, err := pn.ws.Tokens.Create("ops", "u2", "control", "cli:test", 0)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", pn.srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+ctl)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("an API token's accept: %v %v", resp, err)
	}
	req, _ = http.NewRequest("POST", pn.srv.URL+path, nil) // the session's cookie, without the page's header
	if resp, err := pn.c.Do(req); err != nil || resp.StatusCode != 403 {
		t.Fatalf("an accept without the CSRF header: %v %v", resp, err)
	}
	if ms := pn.machines(); ms["pc-b"]["binding"] != approval.BindChanged {
		t.Fatal("a refused accept changed the record")
	}
	// the admin: asked to confirm although the sign-in is seconds old, told what would be accepted
	st, out = pn.call("POST", path, pn.acceptBody("pc-b"))
	if d, _ := out["detail"].(map[string]any); !strings.Contains(fmt.Sprint(d["action"]), "it shows the machine id "+approval.ShortID(otherMachineID)+"; on record is "+approval.ShortID(idB)) {
		t.Fatalf("what the dialog says would be accepted: %d %v", st, out)
	}
	if res := pn.confirm(st, out); res["ok"] != true || !strings.Contains(fmt.Sprint(res["message"]), "the machine id it shows is on record now") {
		t.Fatalf("the accept: %v", res)
	}
	if ms := pn.machines(); ms["pc-b"]["binding"] != nil || ms["pc-b"]["machine_id"] != otherMachineID || ms["pc-b"]["pinned"] != approval.PinAccepted {
		t.Fatalf("after the accept: %v", ms["pc-b"])
	}
	if ac := pn.auditLines("approval.accept"); len(ac) != 1 || ac[0]["target"] != "pc-b" || ac[0]["outcome"] != "ok" {
		t.Fatalf("the accept in the audit log: %v", ac)
	}
	// and the real machine accepts an approval made with the id it has now
	st, out = pn.shutdown(pn.b, 60)
	id, _, _, ok := asked(st, out)
	if !ok {
		t.Fatalf("pc-b after the accept: %d %v", st, out)
	}
	if res := pn.approve(id); res["ok"] != true || len(pn.b.power.Scheduled) != 1 {
		t.Fatalf("the approved shutdown of pc-b: %v, scheduled %v", res, pn.b.power.Scheduled)
	}
	if raw, _ := os.ReadFile(pn.audit); strings.Contains(string(raw), otherMachineID) || strings.Contains(string(raw), idB) {
		t.Fatal("the audit log has a machine id in full")
	}
	pn.noLeak()
}

// A machine that had the check on and shows none any more (its trust file was removed on the machine, or
// something else answers in its name) is an alarm. It is never silently a machine without the check: the real
// agent would now take a shutdown on the panel's confirmation alone, and the panel sends it none.
func TestBindingAVanishedCheckIsAnAlarm(t *testing.T) {
	pn := newApprovalPanel(t)
	pn.turnOn("pc-a", "pc-b")
	if err := os.Remove(pn.b.trust); err != nil {
		t.Fatal(err)
	}
	if st := pn.b.gate.StatusNow(); st.State != agentapi.ApprovalOff {
		t.Fatalf("pc-b after its trust file was removed: %+v", st)
	}
	// the machine itself is open now: this is what the panel must not use
	pn.settle("pc-b", approval.BindVanished)
	if ms := pn.machines(); ms["pc-b"]["binding"] != approval.BindVanished || ms["pc-b"]["can_accept"] != true || ms["pc-b"]["accept"] != "off" {
		t.Fatalf("the approval page: %v", ms["pc-b"])
	}
	if ap := pn.state("pc-b"); ap["binding"] != approval.BindVanished {
		t.Fatalf("pc-b's block: %v", ap)
	}
	st, out := pn.shutdown(pn.b, 60)
	stopped(t, "the shutdown of pc-b", st, out, "had the check on and now shows none")
	st, out = pn.call("POST", "/api/admin/updates/pc-b", map[string]any{})
	stopped(t, "the update of pc-b", st, out, "had the check on and now shows none")
	if len(pn.b.sent("/power"))+len(pn.b.sent("/update")) != 0 || len(pn.b.power.Scheduled) != 0 {
		t.Fatal("pc-b was sent something")
	}
	if al := pn.auditLines("approval.alarm"); len(al) != 1 || al[0]["target"] != "pc-b" || al[0]["outcome"] != "alarm: vanished" {
		t.Fatalf("the alarm in the audit log: %v", al)
	}
	st, notes := pn.call("GET", "/api/notifications", nil)
	if raw, _ := json.Marshal(notes); st != 200 || !strings.Contains(string(raw), "approval_alarm") {
		t.Fatalf("no notification about the alarm: %d %s", st, raw)
	}
	// in a run it is not either
	if st, page := pn.call("GET", "/api/admin/wake-update", nil); st != 200 || runRow(t, page, "pc-b")["approval"] != "alarm" || runRow(t, page, "pc-b")["choice"] != "" {
		t.Fatalf("pc-b on the page of the runs: %d %v", st, runRow(t, page, "pc-b"))
	}
	// an admin accepts that the check is off: from then on it is a machine without the check
	if res := pn.confirm(pn.call("POST", "/api/admin/approval/accept/pc-b", pn.acceptBody("pc-b"))); res["ok"] != true || !strings.Contains(fmt.Sprint(res["message"]), "its check is off") {
		t.Fatalf("the accept: %v", res)
	}
	st, out = pn.shutdown(pn.b, 60) // the sign-in of the accept is seconds old: it runs
	if st != 200 || len(pn.b.power.Scheduled) != 1 {
		t.Fatalf("pc-b as a machine without the check: %d %v", st, out)
	}
}

func runRow(t *testing.T, page map[string]any, id string) map[string]any {
	t.Helper()
	hosts, _ := page["hosts"].([]any)
	for _, h := range hosts {
		if m := h.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("%s is not on the page of the runs: %v", id, page)
	return nil
}

// A restart of the panel keeps what is on record: the ids (machines.json in the data folder), and with them
// the rule that a machine with the check is in no "Update unpowered hosts" run, also while it is silent
// (r1-b2-03: after a restart such a machine was offered to a run, woken, and left on).
func TestBindingARestartKeepsTheRecordAndTheRunExclusion(t *testing.T) {
	pn := newApprovalPanel(t)
	pn.turnOn("pc-a")
	idA := pn.a.machineID(t)
	for _, id := range []string{"pc-a", "pc-b"} {
		if err := pn.p.Admin.SetWakeUpdate(id, true); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := adminstate.New(filepath.Join(pn.dir, "data")).Machines()
	if err != nil || recs["pc-a"].MachineID != idA || recs["pc-a"].How != approval.PinTurnOn || recs["pc-a"].State != agentapi.ApprovalRequired {
		t.Fatalf("machines.json: %v %+v", err, recs)
	}
	if info, err := os.Stat(adminstate.MachinesPath(filepath.Join(pn.dir, "data"))); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("machines.json: %v %v", err, info)
	}
	// the panel stops; pc-a is switched off (it does not answer any more)
	pn.srv.Close()
	down := newFront(t, pn.a)
	down.set(func(_ *http.Request, _ []byte, w http.ResponseWriter) bool {
		panic(http.ErrAbortHandler) // the connection is dropped: a machine that is off
	})
	p2 := startPanel(t, &approvalPanel{iss: pn.iss, a: pn.a, b: pn.b, dir: pn.dir}, map[string]string{"pc-a": down.srv.URL})
	ms := p2.machines()
	if ms["pc-a"]["machine_id"] != idA || ms["pc-a"]["pinned"] != approval.PinTurnOn || ms["pc-a"]["binding"] != nil {
		t.Fatalf("after the restart, on record: %v", ms["pc-a"])
	}
	if ap := p2.state("pc-a"); ap["state"] != "required" || ap["last_known"] != true {
		t.Fatalf("pc-a's block after the restart, while it is silent: %v", ap)
	}
	st, page := p2.call("GET", "/api/admin/wake-update", nil)
	if h := runRow(t, page, "pc-a"); st != 200 || h["choice"] != "" || h["approval"] != "required" || !strings.HasPrefix(fmt.Sprint(h["why"]), "needs approval: not supported in runs yet") {
		t.Fatalf("pc-a is offered to a run after the restart: %v", h)
	}
	for _, body := range []any{map[string]any{"update_only": []string{"pc-a"}}, map[string]any{"update_only": []string{"pc-a", "pc-b"}}} {
		if st, out := p2.call("POST", "/api/admin/wake-update", body); st != 400 || !strings.HasPrefix(fmt.Sprint(out["detail"]), "PC-A needs approval: not supported in runs yet") {
			t.Fatalf("a run with pc-a %v: %d %v", body, st, out)
		}
	}
	// and no shutdown on the panel's own confirmation: the machine is asked, and it does not answer
	if st, out := p2.shutdown(p2.a, 60); st == 200 || st == 401 {
		t.Fatalf("a shutdown of the silent pc-a: %d %v", st, out)
	}
	// it comes back: the same id, nothing to flag, and an approval made with the id on record is accepted
	p3 := startPanel(t, &approvalPanel{iss: pn.iss, a: pn.a, b: pn.b, dir: pn.dir}, nil)
	if ms := p3.machines(); ms["pc-a"]["machine_id"] != idA || ms["pc-a"]["binding"] != nil || ms["pc-a"]["pinned"] != approval.PinTurnOn {
		t.Fatalf("after the second restart: %v", ms["pc-a"])
	}
	st, out := p3.shutdown(p3.a, 60)
	id, _, _, ok := asked(st, out)
	if !ok {
		t.Fatalf("pc-a after the second restart: %d %v", st, out)
	}
	if res := p3.approve(id); res["ok"] != true || len(pn.a.power.Scheduled) != 1 {
		t.Fatalf("the approved shutdown: %v", res)
	}
}

// Who learns a machine id: an admin in a browser, on the approval page. A viewer does not, an API token does
// not (read or control, also an admin's), and no other answer of the panel carries one. The decision is the
// panel's default and has no switch: the id is what an approval's tie to its machine rests on, and the flag
// ("identity changed", "same id as ...") says everything a viewer can act on.
func TestBindingViewersAndTokensLearnNoMachineIDs(t *testing.T) {
	pn := newApprovalPanel(t)
	pn.turnOn("pc-a", "pc-b")
	pn.b.setMachineID(t, otherMachineID) // an alarm: the answers that describe it are looked at too
	pn.settle("pc-b", approval.BindChanged)
	ids := []string{pn.a.machineID(t), otherMachineID}
	recs, err := adminstate.New(filepath.Join(pn.dir, "data")).Machines()
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, recs["pc-b"].MachineID)
	has := func(raw string) bool {
		for _, id := range ids {
			if strings.Contains(raw, id) {
				return true
			}
		}
		return false
	}
	paths := []string{"/api/systems", "/api/systems/pc-a", "/api/systems/pc-b", "/api/me", "/api/summary", "/api/notifications", "/api/admin/approval",
		"/api/admin/updates", "/api/admin/wake-update", "/api/audit", "/api/sources", "/api/systems/pc-b/poll"}
	read, _, err := pn.ws.Tokens.Create("ha", "u2", "read", "cli:test", 0)
	if err != nil {
		t.Fatal(err)
	}
	ctl, _, err := pn.ws.Tokens.Create("ops", "u2", "control", "cli:test", 0)
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(c *http.Client, path, bearer string) (int, string) {
		req, _ := http.NewRequest("GET", pn.srv.URL+path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	admin := pn.c
	// the admin in a browser: the approval page, and only there
	for _, p := range paths {
		st, raw := fetch(admin, p, "")
		if has(raw) != (p == "/api/admin/approval") {
			t.Errorf("an admin's GET %s (%d): a machine id in it: %v", p, st, has(raw))
		}
	}
	for who, bearer := range map[string]string{"a read token": read, "a control token of an admin": ctl} {
		for _, p := range paths {
			if st, raw := fetch(http.DefaultClient, p, bearer); has(raw) {
				t.Errorf("%s gets a machine id from GET %s (%d)", who, p, st)
			}
		}
	}
	// a viewer, signed in at the issuer like everybody
	pn.iss.Set(func(i *testutil.Issuer) {
		i.Sub = "viewer-7"
		i.Edit = func(_ string, claims map[string]any) {
			claims["groups"] = []string{"homelab"}
			claims["email"] = "v@example.com"
		}
	})
	pn.signIn()
	// an admin gave the new account the role of a viewer of the site the two machines are in
	if err := pn.ws.Access.CreateSite("Den"); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"pc-a", "pc-b"} {
		if _, err := pn.ws.Access.Assign(h, "Den"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := pn.ws.Access.SetRole("viewer-7", "viewer", []string{"Den"}, "test", true); err != nil {
		t.Fatal(err)
	}
	if st, me := pn.call("GET", "/api/me", nil); st != 200 || me["can_control"] == true || me["sub"] != "viewer-7" || me["role"] != "viewer" {
		t.Fatalf("the viewer: %d %v", st, me)
	}
	seen := 0
	for _, p := range paths {
		st, raw := fetch(pn.c, p, "")
		if has(raw) {
			t.Errorf("a viewer gets a machine id from GET %s (%d)", p, st)
		}
		if st == 200 && strings.Contains(raw, `"binding":"changed"`) {
			seen++
		}
	}
	if seen == 0 {
		t.Error("the viewer sees no flag for pc-b at all: the test looked at nothing")
	}
	// the page's stream of changes carries the block too: no id in it
	if raw, _ := os.ReadFile(pn.audit); has(string(raw)) {
		t.Error("the audit log has a machine id in full")
	}
}
