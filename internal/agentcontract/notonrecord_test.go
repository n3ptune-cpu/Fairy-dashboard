package agentcontract_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	agentapi "github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	agentapproval "github.com/n3ptune-cpu/Fairy-agent/pkg/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// A machine id comes on record through Turn on or through an admin's accept, never because a host shows it
// (audit 5, round 3, r3-x1-01). With the real agent on both ends, as in binding_test.go.
//
// The finding: the panel took whatever id a host showed the first time it saw the host with the check on. A
// machine id is no secret, so a host whose answers cannot be trusted showed the id of ANOTHER machine, got it
// on record for itself, handed the panel's questions on to that machine, and the approval the admin gave for
// the hostile host was accepted by the other one. These are the auditor's two proofs
// (TestR3X1_FirstSightOfAnIDThePanelNeverSawOnItsMachine, TestR3X1_ANewcomerWithAnotherHostsIDIsOnAPar), turned
// round: they hold what must happen instead.

// metricsOf reads a machine's own /metrics with the read token (what somebody on the path, or with the read
// token, sees).
func metricsOf(t *testing.T, g *gated) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", g.srv.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+readTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s /metrics: %v %s", g.id, err, raw)
	}
	return out
}

// hostileAs makes the front (a host whose answers the attacker decides) show block() as its approval block
// (nil: none), and hand every POST of the panel on to the machine `to` (nil: to nobody; the attacker holds
// to's control token).
func (f *front) hostileAs(t *testing.T, to *gated, block func() any, got *[]relayed) {
	f.set(func(r *http.Request, body []byte, w http.ResponseWriter) bool {
		if r.Method == http.MethodGet && r.URL.Path == "/metrics" {
			m := metricsOf(t, f.real)
			if b := block(); b == nil {
				delete(m, "approval")
			} else {
				m["approval"] = b
			}
			answer(w, 200, m)
			return true
		}
		if r.Method != http.MethodPost || to == nil {
			return false
		}
		st, out := to.direct(t, r.URL.Path, body)
		f.mu.Lock()
		*got = append(*got, relayed{r.URL.Path, body, st, out})
		f.mu.Unlock()
		answer(w, st, out)
		return true
	})
}

// The strong case, with no alarm before the fix. pc-a's check was set on the machine (helper-trust set), and
// what the panel sees of pc-a has no approval block (somebody on the plain-HTTP path takes it out; the same
// holds for a machine that is no agent of this panel). pc-b, hostile, shows pc-a's block and hands every POST
// on to pc-a.
//
// It used to end with pc-a's id on record for pc-b at first sight, nothing flagged, and pc-a scheduled to
// power off by the approval the admin gave for "PC-B". Now: nothing comes on record for pc-b, pc-b is "not on
// record", nobody is asked for a passkey, no question and no request reaches pc-a, and nothing is scheduled.
func TestAHostThatShowsAnIDIsNotTakenOnRecordAtFirstSight(t *testing.T) {
	pn := &approvalPanel{iss: testutil.NewIssuer(t), a: startGated(t, "pc-a"), b: startGated(t, "pc-b"), dir: t.TempDir()}
	fa, fb := newFront(t, pn.a), newFront(t, pn.b)
	fa.hostileAs(t, nil, func() any { return nil }, nil) // on the path to pc-a: its block never reaches the panel
	startPanel(t, pn, map[string]string{"pc-a": fa.srv.URL, "pc-b": fb.srv.URL})
	const idA = "id-of-pc-a-set-on-the-machine-0123"
	tr, err := agentapproval.NewTrust(agentapproval.Trust{Issuer: pn.iss.URL(), ClientID: approvalClient, MachineID: idA,
		Keys: []agentapi.TrustKey{agentapproval.KeyFromRSA(pn.iss.Kid, &pn.iss.Key.PublicKey)}, Users: []string{pn.iss.Sub}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (agentapproval.Files{Path: pn.a.trust}).Create(tr); err != nil {
		t.Fatal(err)
	}
	pn.a.gate.StatusNow()
	if st, out := pn.a.direct(t, "/power", []byte(`{"action":"shutdown","delay_s":60}`)); st != 403 || out["code"] != agentapi.CodeApprovalRequired {
		t.Fatalf("baseline: pc-a refuses a shutdown with the control token alone: %d %v", st, out)
	}
	var got []relayed
	fb.hostileAs(t, pn.a, func() any { return metricsOf(t, pn.a)["approval"] }, &got)
	for i := 0; i < 3; i++ {
		pn.poll()
	}
	ms := pn.machines()
	// no pin: nothing is on record for pc-b, and the page says what it is and offers the one button
	if b := ms["pc-b"]; b["machine_id"] != nil || b["pinned"] != nil || b["shown_id"] != idA || b["binding"] != approval.BindNoRecord ||
		b["can_accept"] != true || b["accept"] != "first" {
		t.Fatalf("pc-b must be \"not on record\", with nothing on record: %v", b)
	}
	if ap := pn.state("pc-b"); ap["binding"] != approval.BindNoRecord {
		t.Fatalf("pc-b's block for everyone who sees it: %v", ap)
	}
	// one alarm entry, when it became so; none at the polls after it
	if al := pn.auditLines("approval.alarm"); len(al) != 1 || al[0]["target"] != "pc-b" || al[0]["outcome"] != "alarm: "+approval.BindNoRecord {
		t.Fatalf("the alarm entries: %v", al)
	}
	if n := len(pn.auditLines("approval.machine_id")); n != 0 {
		t.Fatalf("%d ids came on record", n)
	}
	// no approval is asked: the shutdown and the update of pc-b are stopped by the panel, in one sentence
	tokens := len(pn.iss.Tokens())
	st, out := pn.shutdown(pn.b, 60)
	stopped(t, "the shutdown of pc-b", st, out, "PC-B checks approvals itself, and its machine id is not on record")
	st, out = pn.call("POST", "/api/admin/updates/pc-b", map[string]any{})
	stopped(t, "the update of pc-b", st, out, "PC-B checks approvals itself, and its machine id is not on record")
	if len(got) != 0 || len(pn.iss.Tokens()) != tokens {
		t.Fatalf("a question or a request was handed on to pc-a, or somebody was sent to the issuer: %+v", got)
	}
	// nothing scheduled on pc-a (nor on pc-b)
	if len(pn.a.power.Scheduled)+len(pn.b.power.Scheduled) != 0 || len(pn.a.sent("/power"))+len(pn.a.sent("/update")) != 0 {
		t.Fatalf("something was scheduled or sent: pc-a %v, pc-b %v", pn.a.power.Scheduled, pn.b.power.Scheduled)
	}
	// and pc-a, whose block the panel cannot see: the machine refuses the plain request itself, the panel has
	// no id on record for it and asks for no passkey
	st, out = pn.shutdown(pn.a, 60)
	stopped(t, "the shutdown of pc-a", st, out, "PC-A checks approvals itself, and its machine id is not on record")
	if len(pn.a.power.Scheduled) != 0 || len(pn.iss.Tokens()) != tokens {
		t.Fatalf("pc-a: %v", pn.a.power.Scheduled)
	}
}

// The visible case. pc-a got its id from the panel's own Turn on (made up by the panel, proven with a
// passkey). pc-b, which has no record, then shows pc-a's block and hands every POST on to pc-a.
//
// It used to end with pc-a's id on record for pc-b too, the two "on a par", and the button "This host keeps
// its machine id" on pc-b: one press made pc-b the keeper, and approvals for PC-B then switched PC-A off. Now
// pc-b has nothing on record and no button, an accept for it is 409, and while both show the id neither gets
// an approval; after the admin's word for pc-a, pc-a works and pc-b gets nothing.
func TestANewcomerThatShowsAnotherHostsIDHasNoButton(t *testing.T) {
	pn, fb := frontedPanel(t)
	pn.turnOn("pc-a")
	idA := pn.a.machineID(t)
	if ms := pn.machines(); ms["pc-a"]["pinned"] != approval.PinTurnOn || ms["pc-a"]["binding"] != nil || ms["pc-b"]["machine_id"] != nil {
		t.Fatalf("start: %v / %v", ms["pc-a"], ms["pc-b"])
	}
	var got []relayed
	fb.hostileAs(t, pn.a, func() any { return metricsOf(t, pn.a)["approval"] }, &got)
	pn.settle("pc-b", approval.BindDuplicate)
	ms := pn.machines()
	// the newcomer: nothing on record, no button, the host that keeps the id by name
	if b := ms["pc-b"]; b["machine_id"] != nil || b["pinned"] != nil || b["shown_id"] != idA || b["can_accept"] != nil || b["accept"] != nil || fmt.Sprint(b["held_by"]) != "[PC-A]" {
		t.Fatalf("the newcomer must have nothing on record and no button: %v", b)
	}
	// an accept made up without the page: 409, nobody is asked to confirm
	st, out := pn.call("POST", "/api/admin/approval/accept/pc-b", map[string]any{"machine_id": idA})
	if d := fmt.Sprint(out["detail"]); st != 409 || !strings.Contains(d, "PC-B: nothing was accepted. The machine id it shows is kept by PC-A, and one id is accepted for one host only.") ||
		!strings.Contains(d, "Give PC-B a new id on the machine itself (fairy-agent helper-trust set --new-machine-id), then accept the new id here.") {
		t.Fatalf("an accept on the newcomer: %d %v", st, out)
	}
	// while two hosts show one id, neither is asked for an approval (the protocol's rule: both count as
	// unprotected until an admin has looked)
	st, out = pn.shutdown(pn.a, 60)
	stopped(t, "the shutdown of pc-a", st, out, "have the same machine id")
	st, out = pn.shutdown(pn.b, 60)
	stopped(t, "the shutdown of pc-b", st, out, "PC-B has the same machine id as PC-A, which keeps it")
	// the original has the one button; the admin's word for it
	if a := ms["pc-a"]; a["can_accept"] != true || a["accept"] != "keep" || a["held_by"] != nil || a["pinned"] != approval.PinTurnOn {
		t.Fatalf("the original: %v", a)
	}
	res := pn.confirm(pn.call("POST", "/api/admin/approval/accept/pc-a", pn.acceptBody("pc-a")))
	if res["ok"] != true || !strings.Contains(fmt.Sprint(res["message"]), "PC-A keeps the machine id. PC-B stays flagged") {
		t.Fatalf("the accept on the original: %v", res)
	}
	// from now on: pc-a is approved again, by its own approval; pc-b gets none, and nothing of pc-b's reaches pc-a
	got = nil
	st, out = pn.shutdown(pn.b, 60)
	stopped(t, "the shutdown of pc-b after the admin's word for pc-a", st, out, "PC-B has the same machine id as PC-A, which keeps it")
	if st, out := pn.call("POST", "/api/admin/approval/accept/pc-b", map[string]any{"machine_id": idA}); st != 409 || !strings.Contains(fmt.Sprint(out["detail"]), "kept by PC-A") {
		t.Fatalf("an accept on the newcomer afterwards: %d %v", st, out)
	}
	if len(got) != 0 || len(pn.a.power.Scheduled) != 0 {
		t.Fatalf("something of pc-b's reached pc-a: %+v, scheduled %v", got, pn.a.power.Scheduled)
	}
	st, out = pn.shutdown(pn.a, 60)
	id, machines, _, ok := asked(st, out)
	if !ok || len(machines) != 1 || machines[0] != "PC-A" {
		t.Fatalf("the shutdown of pc-a: %d %v", st, out)
	}
	if res := pn.approve(id); res["ok"] != true || len(pn.a.power.Scheduled) != 1 || len(pn.b.power.Scheduled) != 0 {
		t.Fatalf("the approved shutdown of pc-a: %v, scheduled %v / %v", res, pn.a.power.Scheduled, pn.b.power.Scheduled)
	}
	for _, l := range pn.auditLines("power.shutdown") {
		if l["outcome"] == "ok" && l["target"] != "pc-a" {
			t.Fatalf("a shutdown went through for another host than pc-a: %v", l)
		}
	}
}
