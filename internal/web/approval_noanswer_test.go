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
			if pr["ok"] != false || pr["message"] != "0 of 1 machines confirmed that they check approvals themselves now; not known for 1 (no answer). tower: not known" {
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
	if pr := a.result(t); pr["ok"] != true || pr["message"] != "1 of 2 machines confirmed that they check approvals themselves now; not known for 1 (no answer). laptop: accepted; tower: not known" {
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
	if r.code != 502 || r.body["detail"] != "laptop gave no answer to the question about an approval (HTTP 504). Nothing else was sent to it: try again." {
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

// One rule says whether an answer is the agent's own: powerctl.NoAnswer, for the question about an approval and
// for the trust of a Turn on as for the power, fan and update requests (they had a second one: "an error status
// with a reason or a code, but not 502 or 504").
func TestTheApprovalRoutesUseThePanelsOneRuleForNoAnswer(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   jsonx.M
		own    bool // the agent's own answer
	}{
		"a gateway's 502":                       {502, nil, false},
		"a gateway's 504 that answers in JSON":  {504, jsonx.M{"error": "upstream request timeout", "code": "approval_refused"}, false},
		"a redirect":                            {302, nil, false},
		"a proxy's login page, 401":             {401, nil, false},
		"an error page that is not the agent's": {500, nil, false},
		"a good status without an answer":       {200, nil, false},
		"the agent's own 503":                   {503, jsonx.M{"error": "the approval trust file on this machine cannot be used", "code": api.CodeApprovalUnavailable}, true},
		"the agent's own 401":                   {401, jsonx.M{"error": "unauthorized"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			// the question about an approval, for a shutdown
			a := newApprovalEnv(t)
			a.require(t, a.laptop)
			a.gb.Lie("laptop", func(path string) (int, jsonx.M) {
				if path == api.PathApprovalChallenge {
					return c.status, c.body
				}
				return 0, nil
			})
			r := a.post("/api/systems/laptop/power", a.shutdownBody())
			lost := fmt.Sprintf("laptop gave no answer to the question about an approval (HTTP %d). Nothing else was sent to it: try again.", c.status)
			if got := fmt.Sprint(r.body["detail"]); (got == lost && r.code == 502) == c.own {
				t.Errorf("the question: %d %q", r.code, got)
			}
			if _, _, asked := approvalAsked(r); asked || controlWrites(a.laptop) != 0 {
				t.Errorf("after such an answer to the question: asked %v, sent %q", asked, sentPaths(a.laptop.Asked()))
			}
			// the trust of a Turn on
			b := newApprovalEnv(t)
			row, _ := turnOnTower(t, b, func() {
				b.gb.Lie("tower", func(path string) (int, jsonx.M) {
					if path == api.PathApprovalTrust {
						return c.status, c.body
					}
					return 0, nil
				})
			})
			if (row["outcome"] == "no_answer") == c.own {
				t.Errorf("Turn on: %v", row)
			}
			if all := fmt.Sprint(row); !c.own && (strings.Contains(all, "Nothing was written") || strings.Contains(all, "refused")) {
				t.Errorf("Turn on says more than anybody knows: %v", row)
			}
		})
	}
}

// RK-02 (review of passkey-k): the machine takes the trust, and what comes back to the panel is a good status
// that is not the agent's answer: 200 with an empty object, 204, a proxy's own JSON. The row read "refused: it
// answered without saying that the check is on now (state: )" or "it answered HTTP 204. Nothing was written.",
// red, for a machine that checks approvals now. Such an answer is "no answer": the panel looks at the machine,
// finds the check on with the id and the proposal of this Turn on, and says "accepted"; and when the look
// shows nothing yet, "no answer: it may have been written".
func TestATurnOnAnsweredWithAGoodStatusThatIsNotTheAgents(t *testing.T) {
	for name, back := range map[string]func() (int, jsonx.M){
		"200 with an empty object":   func() (int, jsonx.M) { return 200, jsonx.M{} },
		"204":                        func() (int, jsonx.M) { return 204, nil },
		"200 with a proxy's JSON":    func() (int, jsonx.M) { return 200, jsonx.M{"ok": true, "message": "forwarded"} },
		"202 with the agent's words": func() (int, jsonx.M) { return 202, jsonx.M{"state": api.ApprovalRequired} },
	} {
		for _, taken := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, the machine took the trust: %v", name, taken), func(t *testing.T) {
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
						if taken {
							if st, _ := a.gb.Forward("tower", path, body); st != 200 {
								t.Errorf("the machine did not take the trust: %d", st)
							}
						}
						return back()
					})
				})
				if all := fmt.Sprint(row, pr); strings.Contains(all, "Nothing was written") || strings.Contains(all, "refused") || strings.Contains(all, "without saying") {
					t.Fatalf("it says more than anybody knows: %v | %v", row, pr)
				}
				on := a.tower.Gate.StatusNow().State == api.ApprovalRequired
				switch {
				case on != taken:
					t.Fatalf("the machine's state: %v", a.tower.Gate.StatusNow())
				case taken && (row["outcome"] != "accepted" || !strings.Contains(fmt.Sprint(row["text"]), "its state shows that it took the trust") || a.s.Poller.BindingOf("tower").Record.MachineID == ""):
					t.Fatalf("the machine took the trust: row %v, on record %+v", row, a.s.Poller.BindingOf("tower").Record)
				case !taken && (row["outcome"] != "no_answer" || row["text"] != trustNoAnswer || a.s.Poller.BindingOf("tower").Record.MachineID != ""):
					t.Fatalf("the machine did not take it: row %v, on record %+v", row, a.s.Poller.BindingOf("tower").Record)
				}
			})
		}
	}
	// the agent's own answers stay what they were: its 200 with a state that is not "required" is its word that
	// the check is not on, and its error with its own body is its "no" ("Nothing was written" is the agent's
	// then; a 500 with a JSON error is the agent's by the panel's rule for every route)
	for name, c := range map[string]struct {
		status int
		body   jsonx.M
		want   string
	}{
		"200 with the state off":    {200, jsonx.M{"state": api.ApprovalOff}, "it answered without saying that the check is on now (state: off)"},
		"500 with the agent's JSON": {500, jsonx.M{"error": "could not write the trust file: disk full"}, "could not write the trust file: disk full. Nothing was written."},
	} {
		a := newApprovalEnv(t)
		row, _ := turnOnTower(t, a, func() {
			a.gb.Lie("tower", func(path string) (int, jsonx.M) {
				if path == api.PathApprovalTrust {
					return c.status, c.body
				}
				return 0, nil
			})
		})
		if row["outcome"] != "refused" || row["text"] != c.want {
			t.Errorf("%s: %v", name, row)
		}
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

// The agent's answer of f439fc0 (review of its fix 7, RF7-02): in the minute after a shutdown, reboot or sleep
// that got no answer (504), a Cancel that found nothing to stop is answered 409, "... may still be scheduled:
// send the cancel again", and not 200. The panel's Cancel shows those words and never "cancelled" for it; the
// audit log has it as an error; and the cancel sent again, which the machine then takes, is "cancelled" as any
// cancel. On a machine with the check and on one without: a cancel is the same for both.
//
// (The panel sends a cancel only while it shows a countdown for the machine: powerctl answers "nothing pending"
// itself otherwise. That is the plain panel's rule and is not touched here.)
func TestACancelThatMayNotHaveStoppedItIsNotCancelled(t *testing.T) {
	const again = "nothing was pending when the cancel reached the helper, but the shutdown that got no answer a moment ago may still be scheduled: " +
		"send the cancel again (for a minute after such a request the agent cannot say that nothing is pending)"
	for _, check := range []bool{false, true} {
		a := newApprovalEnv(t)
		if check {
			a.require(t, a.laptop)
		}
		a.pendingAt(time.Now().Add(40 * time.Second))
		refuse := true
		a.gb.Lie("laptop", func(path string) (int, jsonx.M) {
			if path == "/power" && refuse {
				return 409, jsonx.M{"error": again}
			}
			return 0, nil
		})
		r := a.post("/api/systems/laptop/power", map[string]any{"action": "cancel"})
		// (the plain panel's powerctl hands a machine's 409 on as 400; the words are the machine's)
		if r.code < 400 || r.code > 499 || r.body["detail"] != again || strings.Contains(string(r.raw), "cancelled") || r.body["message"] != nil {
			t.Fatalf("check on %v: the cancel: %d %s", check, r.code, r.raw)
		}
		if _, _, asked := approvalAsked(r); asked {
			t.Fatalf("check on %v: a cancel asked for a passkey: %s", check, r.raw)
		}
		es := a.auditEntries("", "power.cancel")
		if len(es) != 1 || !strings.HasPrefix(fmt.Sprint(es[0]["outcome"]), "error 4") || !strings.Contains(fmt.Sprint(es[0]["outcome"]), "may still be scheduled") {
			t.Fatalf("check on %v: the audit log: %v", check, es)
		}
		// the same button again, and the machine has it by now
		refuse = false
		r = a.post("/api/systems/laptop/power", map[string]any{"action": "cancel"})
		if msg, _ := r.body["message"].(string); r.code != 200 || msg != "The shutdown of laptop is cancelled." {
			t.Fatalf("check on %v: the cancel sent again: %d %s", check, r.code, r.raw)
		}
	}
}
