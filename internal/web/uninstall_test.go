package web

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// "Uninstall agent" (Fairy-agent 0.11.0) against machines that check approvals with Fairy-agent's real gate.
// The panel never sends POST /uninstall without the machine's own challenge and a passkey for exactly it.

// uninstallEnv is an approvalEnv whose laptop offers uninstall (features.uninstall, and its config on).
func uninstallEnv(t *testing.T) *approvalEnv {
	t.Helper()
	a := newApprovalEnv(t)
	a.laptop.Uninstall = true
	a.b.Mu.Lock()
	old := a.b.Metrics["laptop"]
	a.b.Metrics["laptop"] = func() (jsonx.M, error) {
		m, err := old()
		if m != nil {
			m["features"].(jsonx.M)["uninstall"] = true
		}
		return m, err
	}
	a.b.Mu.Unlock()
	a.poll()
	return a
}

func uninstallPath(id string) string { return "/api/systems/" + id + "/uninstall" }

func sentTo(m *testutil.Machine, path string) bool {
	for _, s := range m.Asked() {
		if s.Path == path {
			return true
		}
	}
	return false
}

// Who may, and the panel's own "no" before the machine is asked anything.
func TestUninstallRefusedByThePanel(t *testing.T) {
	a := uninstallEnv(t)
	a.require(t, a.laptop, a.tower)
	ok := map[string]any{"confirm": "laptop"}
	// a viewer; no CSRF header
	v := a.cookie(viewer, 0)
	if r := a.do("POST", uninstallPath("laptop"), ok, "Cookie", v, "X-Requested-With", "fairy"); r.code != 403 {
		t.Errorf("viewer: %d %s", r.code, r.raw)
	}
	if r := a.do("POST", uninstallPath("laptop"), ok, "Cookie", a.admin); r.code != 403 {
		t.Errorf("no CSRF header: %d %s", r.code, r.raw)
	}
	// the kill switch
	_ = a.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "maintenance")
	if r := a.post(uninstallPath("laptop"), ok); r.code != 423 {
		t.Errorf("kill switch: %d %s", r.code, r.raw)
	}
	_ = a.s.Poller.Admin.SetControlDisabled(false, "root", "")
	// the typed name
	for _, body := range []any{nil, map[string]any{}, map[string]any{"confirm": "laptops"}} {
		if r := a.post(uninstallPath("laptop"), body); r.code != 400 || !strings.Contains(r.raw2(), "type the host's name (laptop)") {
			t.Errorf("typed name %v: %d %s", body, r.code, r.raw)
		}
	}
	// an agent that does not offer it (tower's config has no uninstall.enabled)
	if r := a.post(uninstallPath("tower"), map[string]any{"confirm": "tower"}); r.code != 409 || !strings.Contains(r.raw2(), "uninstall.enabled") {
		t.Errorf("not offered: %d %s", r.code, r.raw)
	}
	for _, m := range []*testutil.Machine{a.laptop, a.tower} {
		if sentTo(m, api.PathUninstall) || sentTo(m, api.PathApprovalChallenge) {
			t.Fatalf("%s was asked before every check of the panel passed: %s", m.ID, sentPaths(m.Asked()))
		}
	}
	if n := len(a.auditEntries("", uninstallAudit)); n < 4 { // three typed names and the agent that does not offer it
		t.Errorf("only %d refusals were audited", n)
	}
}

// A machine without a trust file: refused at once, by the panel's record, before anybody is sent to sign in;
// nothing is sent. And when the panel's record is a poll behind (the machine's trust was cleared a moment ago),
// the machine's own answer to the question refuses it.
func TestUninstallWithoutTrust(t *testing.T) {
	a := uninstallEnv(t)
	r := a.post(uninstallPath("laptop"), map[string]any{"confirm": "laptop"})
	if r.code != 409 || !strings.Contains(r.raw2(), "no passkey check set up") {
		t.Fatalf("without trust: %d %s", r.code, r.raw)
	}
	if len(a.laptop.Asked()) != 0 || a.laptop.Uninstalled {
		t.Fatalf("asked: %s", sentPaths(a.laptop.Asked()))
	}
	// the panel's record says "required", the machine's trust is gone
	a.require(t, a.laptop)
	if err := os.Remove(a.laptop.TrustPath); err != nil {
		t.Fatal(err)
	}
	a.laptop.Gate.StatusNow()
	r = a.post(uninstallPath("laptop"), map[string]any{"confirm": "laptop"})
	if r.code != 409 || sentTo(a.laptop, api.PathUninstall) || a.laptop.Uninstalled {
		t.Fatalf("trust gone on the machine: %d %s, asked %s", r.code, r.raw, sentPaths(a.laptop.Asked()))
	}
}

// With trust: the machine's challenge, the passkey, then POST /uninstall with that approval, once; audited
// and notified.
func TestUninstallWithApproval(t *testing.T) {
	a := uninstallEnv(t)
	a.require(t, a.laptop)
	r := a.post(uninstallPath("laptop"), map[string]any{"confirm": "laptop"})
	id, machines, ok := approvalAsked(r)
	if !ok || len(machines) != 1 {
		t.Fatalf("no passkey dialog: %d %s", r.code, r.raw)
	}
	if !strings.Contains(r.raw2(), "uninstall of its agent") {
		t.Errorf("the dialog does not say what the passkey signs: %s", r.raw)
	}
	if sentTo(a.laptop, api.PathUninstall) {
		t.Fatal("sent before the passkey")
	}
	if cb := a.approve(t, id); cb.code != 303 {
		t.Fatalf("callback: %d %s", cb.code, cb.raw)
	}
	res := a.result(t)
	if res["ok"] != true || !strings.Contains(fmt.Sprint(res["message"]), "removing itself") {
		t.Fatalf("result: %v", res)
	}
	p := a.laptop.Passed()
	if len(p) != 1 || p[0].Path != api.PathUninstall || !p[0].Approved || !a.laptop.Uninstalled {
		t.Fatalf("passed: %+v", p)
	}
	au := a.auditEntries("", uninstallAudit)
	if len(au) == 0 || fmt.Sprint(au[0]["outcome"]) != "ok" || !strings.Contains(fmt.Sprint(au[0]), "approval:sent") {
		t.Fatalf("audit: %v", au)
	}
	if n := a.do("GET", "/api/notifications", nil, "Cookie", a.admin); !strings.Contains(n.raw2(), uninstallAudit) {
		t.Errorf("notification: %s", n.raw)
	}
	// a second time: the machine's once (its answer, said in words), nothing more done
	r = a.post(uninstallPath("laptop"), map[string]any{"confirm": "laptop"})
	if id, _, ok := approvalAsked(r); ok {
		a.approve(t, id)
		if res := a.result(t); res["ok"] != false || !strings.Contains(fmt.Sprint(res["message"]), "already started") {
			t.Errorf("second uninstall: %v", res)
		}
	}
	if len(a.laptop.Passed()) != 1 {
		t.Fatalf("passed twice: %+v", a.laptop.Passed())
	}
}

// A machine that says "needs no approval" for an uninstall is not believed: nothing is sent.
func TestUninstallNeverWithoutApproval(t *testing.T) {
	a := uninstallEnv(t)
	a.require(t, a.laptop)
	a.gb.Lie("laptop", func(path string) (int, jsonx.M) {
		if path == api.PathApprovalChallenge {
			return 200, jsonx.M{"needed": false, "reason": "off", "request": "uninstall"}
		}
		return 0, nil
	})
	r := a.post(uninstallPath("laptop"), map[string]any{"confirm": "laptop"})
	if r.code != 409 || !strings.Contains(r.raw2(), "only with a passkey approval") {
		t.Fatalf("a machine that says none is needed: %d %s", r.code, r.raw)
	}
	if sentTo(a.laptop, api.PathUninstall) || a.laptop.Uninstalled {
		t.Fatal("sent without an approval")
	}
}

// The machine's refusals of POST /uninstall, in words.
func TestUninstallRefusalWords(t *testing.T) {
	for code, want := range map[string]string{
		api.CodeApprovalOff:       "no passkey check set up",
		api.CodeUninstallDisabled: "uninstall.enabled",
		api.CodeUninstallBusy:     "already started",
		api.CodeHelperRequired:    "elevated helper",
		api.CodeUpdaterBusy:       "installing an update",
	} {
		err := uninstallRefusal("laptop", 409, jsonx.M{"code": code, "error": "x"}, "")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", code, err)
		}
	}
	if err := uninstallRefusal("laptop", 401, jsonx.M{}, ""); err == nil || !strings.Contains(err.Error(), "control token") {
		t.Errorf("401: %v", err)
	}
}
