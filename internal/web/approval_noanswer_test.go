package web

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// What the agent answers since its fix batch of audit 6 when its helper was sent a request and did not answer in
// time: 504, with these words for the trust of a Turn on and for the question about an approval.
const (
	agent504Trust    = "the helper was sent the trust and did not answer in time: it may be written all the same: look at the machine's approval state before anything else"
	agent504Question = "the helper was sent the question about an approval and did not answer in time: nothing was done; ask again"
)

// turnOnTower presses Turn on for tower, lets answer decide what the panel gets for the trust it sends, approves,
// and returns tower's row of "Last Turn on" and the result's message.
func turnOnTower(t *testing.T, a *approvalEnv, after func()) (row map[string]any, pr map[string]any) {
	t.Helper()
	id, _, ok := approvalAsked(a.turnOn("tower"))
	if !ok {
		t.Fatal("the dialog of Turn on")
	}
	after()
	if cb := a.approve(t, id); cb.code != 303 {
		t.Fatalf("callback: %d %s", cb.code, cb.raw)
	}
	pr = a.result(t)
	page, _ := a.page(t)
	for _, x := range page["last"].(map[string]any)["results"].([]any) {
		if x.(map[string]any)["id"] == "tower" {
			row = x.(map[string]any)
		}
	}
	return row, pr
}

// A Turn on whose request to the machine got no answer (the agent's fix batch of audit 6: it answers 504 when
// its helper was sent the trust and did not answer; a gateway's 502 or 504, a timeout). The panel said
// "refused ... Nothing was written." for the two statuses. Nobody knows that: the trust may be written. It has
// its own outcome now, "no answer: it may have been written; look at the machine's Approval.", and nothing
// comes on record.
func TestATurnOnWithoutAnAnswerIsNotKnown(t *testing.T) {
	for name, answer := range map[string]func(a *approvalEnv){
		"the agent's own 504": func(a *approvalEnv) {
			a.gb.Lie("tower", func(path string) (int, jsonx.M) {
				if path == api.PathApprovalTrust {
					return 504, jsonx.M{"error": agent504Trust}
				}
				return 0, nil
			})
		},
		"a gateway's 502 without a body": func(a *approvalEnv) {
			a.gb.Lie("tower", func(path string) (int, jsonx.M) {
				if path == api.PathApprovalTrust {
					return 502, nil
				}
				return 0, nil
			})
		},
		"a gateway's 504 with JSON": func(a *approvalEnv) {
			a.gb.Lie("tower", func(path string) (int, jsonx.M) {
				if path == api.PathApprovalTrust {
					return 504, jsonx.M{"error": "upstream timed out", "code": "gateway_timeout"}
				}
				return 0, nil
			})
		},
		"a timeout": func(a *approvalEnv) { a.gb.Fail("tower", devices.Errf("timeout")) },
	} {
		t.Run(name, func(t *testing.T) {
			a := newApprovalEnv(t)
			row, pr := turnOnTower(t, a, func() { answer(a) })
			if row["outcome"] != "no_answer" || row["text"] != "no answer: it may have been written; look at the machine's Approval." {
				t.Fatalf("tower's row: %v", row)
			}
			if all := fmt.Sprint(row, pr); strings.Contains(all, "Nothing was written") || strings.Contains(all, "refused") || strings.Contains(all, "not switched on") {
				t.Fatalf("it says more than anybody knows: %v | %v", row, pr)
			}
			if pr["ok"] != false || pr["message"] != "0 of 1 machines confirmed that they check approvals themselves now; not known for 1 (no answer). tower: no answer" {
				t.Fatalf("the result: %v", pr)
			}
			if b := a.s.Poller.BindingOf("tower"); b.Record.MachineID != "" {
				t.Fatalf("an id came on record for a machine that gave no answer: %+v", b)
			}
			if got := auditOutcomes(a.s.Cfg.AuditLog); !strings.Contains(got, "approval.turn_on error: no_answer: no answer: it may have been written") || strings.Contains(got, "approval.machine_id") {
				t.Fatalf("the audit log:\n%s", got)
			}
		})
	}
	// the page knows the outcome: its own chip, not the red "not switched on"
	if js := appJS(t); !strings.Contains(js, `no_answer: ["warn", "not known"]`) {
		t.Error("app.js has no chip for the outcome no_answer")
	}
}

// One machine answers, one does not: the sentence counts the one that said so and does not claim to know the
// other.
func TestATurnOnForTwoCountsWhatItKnows(t *testing.T) {
	a := newApprovalEnv(t)
	id, _, ok := approvalAsked(a.turnOn("laptop", "tower"))
	if !ok {
		t.Fatal("the dialog of Turn on")
	}
	a.gb.Lie("tower", func(path string) (int, jsonx.M) {
		if path == api.PathApprovalTrust {
			return 504, jsonx.M{"error": agent504Trust}
		}
		return 0, nil
	})
	if cb := a.approve(t, id); cb.code != 303 {
		t.Fatalf("callback: %d %s", cb.code, cb.raw)
	}
	if pr := a.result(t); pr["ok"] != true || pr["message"] != "1 of 2 machines confirmed that they check approvals themselves now; not known for 1 (no answer). laptop: accepted; tower: no answer" {
		t.Fatalf("the result: %v", pr)
	}
}

// The panel looks at the machine before it says anything final. When the machine took the trust and only its
// answer was lost, its state shows the check on with the machine id the panel made up and the hash of the
// proposal: that is "accepted", and the id is on record as after an answer.
func TestATurnOnWhoseAnswerWasLostIsReadFromTheMachine(t *testing.T) {
	a := newApprovalEnv(t)
	var mu sync.Mutex
	inside := false
	row, pr := turnOnTower(t, a, func() {
		a.gb.Relay("tower", func(path string, body any) (int, jsonx.M) {
			mu.Lock()
			again := inside
			inside = true
			mu.Unlock()
			if path != api.PathApprovalTrust || again {
				return 0, nil
			}
			if st, _ := a.gb.Forward("tower", path, body); st != 200 { // the machine takes it ...
				t.Errorf("the machine did not take the trust: %d", st)
			}
			return 504, jsonx.M{"error": agent504Trust} // ... and its answer does not come
		})
	})
	if row["outcome"] != "accepted" || !strings.Contains(fmt.Sprint(row["text"]), "it gave no answer to the request, and its state shows that it took the trust") {
		t.Fatalf("tower's row: %v", row)
	}
	if pr["ok"] != true || !strings.Contains(fmt.Sprint(pr["message"]), "1 of 1 machines now check approvals themselves. tower: accepted") {
		t.Fatalf("the result: %v", pr)
	}
	b := a.s.Poller.BindingOf("tower")
	if b.Record.MachineID == "" || b.Record.How != approval.PinTurnOn {
		t.Fatalf("the record: %+v", b)
	}
	a.poll()
	if b := a.s.Poller.BindingOf("tower"); !b.OK() || b.Report.MachineID != b.Record.MachineID {
		t.Fatalf("after the next poll: %+v", b)
	}
}

// And when the trust is written after all, later than the panel's look: nothing is on record, so the machine is
// "not on record" and an admin accepts the id it shows. The panel takes nothing on record by itself.
func TestATurnOnThatIsWrittenLateIsNotOnRecord(t *testing.T) {
	a := newApprovalEnv(t)
	var mu sync.Mutex
	var kept any
	row, _ := turnOnTower(t, a, func() {
		a.gb.Relay("tower", func(path string, body any) (int, jsonx.M) {
			mu.Lock()
			defer mu.Unlock()
			if path != api.PathApprovalTrust || kept != nil {
				return 0, nil
			}
			kept = body
			return 504, jsonx.M{"error": agent504Trust}
		})
	})
	if row["outcome"] != "no_answer" {
		t.Fatalf("tower's row: %v", row)
	}
	if st, _ := a.gb.Forward("tower", api.PathApprovalTrust, kept); st != 200 { // the helper writes it now
		t.Fatalf("the late write: %d", st)
	}
	a.poll()
	b := a.s.Poller.BindingOf("tower")
	if b.Status != approval.BindNoRecord || b.Record.MachineID != "" || b.Report.MachineID == "" {
		t.Fatalf("tower after the late write: %+v", b)
	}
	ag, _ := a.s.Poller.AgentByID("tower")
	if m := a.s.machineFor(ag, nil); !m.CanAccept || m.Accept != "first" {
		t.Fatalf("its row on the approval page: %+v", m)
	}
}

// The question about an approval: 502 and 504 are "no answer", in the panel's words, and nothing follows.
// (They read "the helper was sent the question ..." or "agent error 504" with the status of a refusal, and a
// Turn on listed the machine as "refused".)
func TestAQuestionWithoutAnAnswerIsNoAnswer(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	a.gb.Lie("laptop", func(path string) (int, jsonx.M) {
		if path == api.PathApprovalChallenge {
			return 504, jsonx.M{"error": agent504Question}
		}
		return 0, nil
	})
	r := a.post("/api/systems/laptop/power", a.shutdownBody())
	if r.code != 502 || r.body["detail"] != "laptop gave no answer to the question about an approval (HTTP 504). No request was sent to it: try again." {
		t.Fatalf("a shutdown: %d %s", r.code, r.raw)
	}
	if controlWrites(a.laptop) != 0 || keptID(a.env, a.admin) != "" {
		t.Fatalf("after a question without an answer: sent %q", sentPaths(a.laptop.Asked()))
	}
	// Turn on: such a machine is "no answer", not "refused"
	b := newApprovalEnv(t)
	b.gb.Lie("tower", func(path string) (int, jsonx.M) {
		if path == api.PathApprovalChallenge {
			return 502, nil
		}
		return 0, nil
	})
	r = b.turnOn("laptop", "tower")
	if _, _, ok := approvalAsked(r); !ok || !strings.Contains(fmt.Sprint(r.body["detail"].(map[string]any)["items"]), "tower: NOT switched on (tower gave no answer to the question about an approval (HTTP 502)") {
		t.Fatalf("Turn on: %d %s", r.code, r.raw)
	}
}

// A cancel that came too late (the agent's 409 since its fix batch: "too late to cancel: systemctl poweroff was
// already run") is the machine's own "no" with its words, never "cancelled".
func TestACancelThatTheAgentCallsTooLateIsNotCancelled(t *testing.T) {
	a := newApprovalEnv(t)
	a.pendingAt(time.Now().Add(1 * time.Second)) // the page still shows the countdown
	const late = "too late to cancel: systemctl poweroff was already run: the shutdown is being carried out"
	a.gb.Lie("laptop", func(path string) (int, jsonx.M) {
		if path == "/power" {
			return 409, jsonx.M{"error": late}
		}
		return 0, nil
	})
	r := a.post("/api/systems/laptop/power", map[string]any{"action": "cancel"})
	if r.code == 200 || r.body["detail"] != late || strings.Contains(string(r.raw), "cancelled") {
		t.Fatalf("the cancel: %d %s", r.code, r.raw)
	}
	if es := a.auditEntries("", "power.cancel"); len(es) != 1 || !strings.Contains(fmt.Sprint(es[0]["outcome"]), "error") || !strings.Contains(fmt.Sprint(es[0]["outcome"]), "too late to cancel") {
		t.Fatalf("the audit log: %v", es)
	}
}
