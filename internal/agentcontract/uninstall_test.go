package agentcontract_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	agentapi "github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// "Uninstall agent" end to end: the panel's real web API, poller and approval round trip, and Fairy-agent's
// real handler and gate on the machine. The machine's removal is counted (the Linux one starts a root unit).

func newUninstallPanel(t *testing.T, bOn bool) *approvalPanel {
	t.Helper()
	return startPanel(t, &approvalPanel{iss: testutil.NewIssuer(t),
		a: startGatedOpts(t, "pc-a", gatedOpts{uninstall: true}), b: startGatedOpts(t, "pc-b", gatedOpts{uninstall: bOn}), dir: t.TempDir()}, nil)
}

func (pn *approvalPanel) uninstall(g *gated) (int, map[string]any) {
	return pn.call("POST", "/api/systems/"+g.id+"/uninstall", map[string]any{"confirm": strings.ToUpper(g.id)})
}

// No trust on the machines: the panel refuses at once, the machine refuses whoever asks it, nothing is removed.
func TestE2EUninstallNeedsTheCheck(t *testing.T) {
	pn := newUninstallPanel(t, true)
	if st, out := pn.uninstall(pn.a); st != 409 || !strings.Contains(fmt.Sprint(out), "no passkey check set up") {
		t.Fatalf("through the panel without trust: %d %v", st, out)
	}
	if len(pn.a.sent("/uninstall")) != 0 || len(pn.a.sent(agentapi.PathApprovalChallenge)) != 0 {
		t.Fatal("the machine was asked")
	}
	if st, out := pn.a.direct(t, "/uninstall", []byte(`{}`)); st != 409 || out["code"] != agentapi.CodeApprovalOff {
		t.Fatalf("straight to the agent without trust: %d %v", st, out)
	}
	if n := pn.a.uninstalls.Load(); n != 0 {
		t.Fatalf("removed %d times", n)
	}
}

// With the check: the machine's challenge, the passkey, the request once; the approval is for this request on
// this machine and nothing else, and used once.
func TestE2EUninstallApproved(t *testing.T) {
	pn := newUninstallPanel(t, true)
	pn.turnOn("pc-a", "pc-b")
	if st, out := pn.a.direct(t, "/uninstall", []byte(`{}`)); st != 403 || out["code"] != agentapi.CodeApprovalRequired {
		t.Fatalf("straight to the agent without an approval: %d %v", st, out)
	}

	// an approval that the test keeps from the agent: a good one, for pc-a's uninstall
	pn.a.mu.Lock()
	pn.a.hold = "/uninstall"
	pn.a.mu.Unlock()
	st, out := pn.uninstall(pn.a)
	id, machines, _, ok := asked(st, out)
	if !ok || len(machines) != 1 || machines[0] != "PC-A" {
		t.Fatalf("the uninstall must ask for the machine's approval: %d %v", st, out)
	}
	if d := out["detail"].(map[string]any); !strings.Contains(fmt.Sprint(d), "uninstall of its agent") {
		t.Errorf("the dialog does not say what the passkey signs: %v", d)
	}
	if res := pn.approve(id); res["ok"] == true {
		t.Fatalf("the request was kept from the agent, and the panel says: %v", res)
	}
	held := pn.a.held
	if !bytes.Contains(held, []byte(`"approval"`)) {
		t.Fatalf("the request carried no approval: %s", held)
	}
	// that approval on the other machine, and for other requests on this one: refused, nothing removed
	if st, out := pn.b.direct(t, "/uninstall", held); st != 403 || out["code"] != agentapi.CodeApprovalRefused {
		t.Errorf("pc-a's approval on pc-b: %d %v", st, out)
	}
	m := map[string]any{}
	_ = json.Unmarshal(held, &m)
	asShutdown, _ := json.Marshal(map[string]any{"action": "shutdown", "delay_s": 60, "approval": m["approval"]})
	if st, out := pn.a.direct(t, "/power", asShutdown); st != 403 || out["code"] != agentapi.CodeApprovalRefused {
		t.Errorf("a shutdown under the uninstall's approval: %d %v", st, out)
	}
	if len(pn.a.power.Scheduled) != 0 || pn.a.uninstalls.Load() != 0 || pn.b.uninstalls.Load() != 0 {
		t.Fatal("a swapped request was carried out")
	}
	// and a shutdown's approval does not uninstall
	pn.a.mu.Lock()
	pn.a.hold = "/power"
	pn.a.mu.Unlock()
	st, out = pn.shutdown(pn.a, 60)
	if id, _, _, ok = asked(st, out); !ok {
		t.Fatalf("the shutdown: %d %v", st, out)
	}
	pn.approve(id)
	_ = json.Unmarshal(pn.a.held, &m)
	asUninstall, _ := json.Marshal(map[string]any{"approval": m["approval"]})
	if st, out := pn.a.direct(t, "/uninstall", asUninstall); st != 403 || out["code"] != agentapi.CodeApprovalRefused {
		t.Errorf("an uninstall under a shutdown's approval: %d %v", st, out)
	}

	// the good approval, exactly where it was given: removed, once; the same again is refused
	if st, out := pn.a.direct(t, "/uninstall", held); st != 202 || out["state"] != agentapi.UninstallStarted {
		t.Fatalf("the request the approval was given for: %d %v", st, out)
	}
	if st, out := pn.a.direct(t, "/uninstall", held); st == 202 {
		t.Fatalf("and a second time: %d %v", st, out)
	}
	if n := pn.a.uninstalls.Load(); n != 1 {
		t.Fatalf("removed %d times", n)
	}

	// pc-b, the whole way through the panel
	st, out = pn.uninstall(pn.b)
	if id, _, _, ok = asked(st, out); !ok {
		t.Fatalf("pc-b: %d %v", st, out)
	}
	if res := pn.approve(id); res["ok"] != true || !strings.Contains(fmt.Sprint(res["message"]), "removing itself") {
		t.Fatalf("pc-b's approved uninstall: %v", res)
	}
	if n := pn.b.uninstalls.Load(); n != 1 {
		t.Fatalf("pc-b removed %d times", n)
	}
	log, _ := os.ReadFile(pn.audit)
	if !strings.Contains(string(log), "agent_uninstall") || !strings.Contains(string(log), "auth.approval") {
		t.Errorf("the audit log lacks the approval or the uninstall:\n%s", log)
	}
	pn.noLeak()
}

// Off in a machine's config.yaml: the panel offers nothing and asks nobody for a passkey; the agent says 404.
func TestE2EUninstallOffOnTheMachine(t *testing.T) {
	pn := newUninstallPanel(t, false)
	pn.turnOn("pc-a", "pc-b")
	_, sys := pn.call("GET", "/api/systems/pc-b", nil)
	if sys["uninstall"] == true {
		t.Fatal("the panel shows Uninstall for a machine that does not offer it")
	}
	if _, sys := pn.call("GET", "/api/systems/pc-a", nil); sys["uninstall"] != true {
		t.Fatal("the panel does not show Uninstall for a machine that offers it")
	}
	before := len(pn.b.sent(agentapi.PathApprovalChallenge))
	if st, out := pn.uninstall(pn.b); st != 409 || !strings.Contains(fmt.Sprint(out), "uninstall.enabled") {
		t.Fatalf("through the panel: %d %v", st, out)
	}
	if len(pn.b.sent(agentapi.PathApprovalChallenge)) != before || len(pn.b.sent("/uninstall")) != 0 {
		t.Fatal("the machine was asked")
	}
	if st, out := pn.b.direct(t, "/uninstall", []byte(`{}`)); st != 404 || out["code"] != agentapi.CodeUninstallDisabled {
		t.Fatalf("straight to the agent: %d %v", st, out)
	}
}
