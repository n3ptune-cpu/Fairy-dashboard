package web

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

const checkGoneText = " had the check on and now shows none. Fairy sends it no fan, power or update request until the check is back or an admin accepts that it is off. Nothing was sent."

// A host that is on record as one with the check, whose check is gone before the panel's next poll (audit 6,
// r4-x1-02; R4: such a host "gets no approval and no fan, power or update request from the panel").
//
// The panel asked it for a challenge, took "needed: false, reason: off" for an answer, and went the ordinary
// way: inside the 3-minute window the shutdown was sent at once, unapproved, and the machine (which checks
// nothing any more) ran it. The alarm came with the next poll, after the request. Now the answer "off" from a
// host on record is "check gone": refused with the alarm's sentence, nothing sent, and nothing is sent until an
// admin accepts that the check is off.
func TestACheckGoneBeforeTheNextPollIsSentNothing(t *testing.T) {
	for _, c := range []struct {
		name, path, sent, action string
		body                     func(a *approvalEnv) any
		machine                  func(a *approvalEnv) *testutil.Machine
	}{
		{"shutdown", "/api/systems/laptop/power", "/power", "power.shutdown", func(a *approvalEnv) any { return a.shutdownBody() }, func(a *approvalEnv) *testutil.Machine { return a.laptop }},
		{"update", "/api/admin/updates/laptop", "/update", "update.agent", func(*approvalEnv) any { return map[string]any{} }, func(a *approvalEnv) *testutil.Machine { return a.laptop }},
		{"fan mode", "/api/systems/tower/fan/mode", "/fan/mode", "fan.mode", func(*approvalEnv) any { return map[string]any{"mode": "full"} }, func(a *approvalEnv) *testutil.Machine { return a.tower }},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newApprovalEnv(t)
			m := c.machine(a)
			a.require(t, m)
			body := c.body(a)
			a.clearTrust(t, m) // on the machine: fairy-agent helper-trust clear. No poll since.
			if b := a.s.Poller.BindingOf(m.ID); !b.OK() || b.Record.MachineID == "" {
				t.Fatalf("before the next poll the panel's view is \"on record, in order\": %+v", b)
			}
			fresh := []string{"Cookie", a.cookie(controller(), 0), "X-Requested-With", "fairy", "Content-Type", "application/json"}
			for who, hdr := range map[string][]string{"inside the panel's window": fresh, "outside it": a.csrf} {
				r := a.do("POST", c.path, body, hdr...)
				if r.code != 409 || r.body["detail"] != m.ID+checkGoneText+" An admin resolves it in the Approval box on the host's page." {
					t.Fatalf("%s: %d %s", who, r.code, r.raw)
				}
			}
			if n := controlWrites(m); n != 0 || keptID(a.env, a.admin) != "" || len(a.iss.Asked()) != 0 {
				t.Fatalf("the machine was sent %q (kept: %q)", sentPaths(m.Asked()), keptID(a.env, a.admin))
			}
			if es := a.auditEntries("", c.action); len(es) != 2 || !strings.Contains(fmt.Sprint(es[0]["outcome"]), "had the check on and now shows none") {
				t.Fatalf("the audit log: %v", es)
			}
			// the next poll: the alarm, once; and the panel still sends nothing
			a.poll()
			if b := a.s.Poller.BindingOf(m.ID); b.Status != approval.BindVanished {
				t.Fatalf("after the poll: %+v", b)
			}
			if es := a.alarmEntries(); len(es) != 1 || es[0]["outcome"] != "alarm: vanished" {
				t.Fatalf("the alarm: %v", es)
			}
			if r := a.do("POST", c.path, body, fresh...); r.code != 409 || !strings.Contains(fmt.Sprint(r.body["detail"]), checkGoneText) || controlWrites(m) != 0 {
				t.Fatalf("after the alarm: %d %s", r.code, r.raw)
			}
			// an admin accepts that the check is off: a machine without the check, the ordinary way again
			if what, _, err := a.accept(m.ID, "test admin"); what != "off" || err != nil {
				t.Fatalf("the accept: %q %v", what, err)
			}
			if r := a.do("POST", c.path, body, fresh...); r.code != 200 {
				t.Fatalf("after the accept: %d %s", r.code, r.raw)
			}
			if p := m.Passed(); len(p) != 1 || p[0].Path != c.sent || p[0].Approved {
				t.Fatalf("after the accept: %+v", p)
			}
		})
	}
}

// The same when the machine still has its check and something at its address answers the question with "off",
// or with an exception that is not one for this request: nothing is sent to it. (The machine itself refused the
// unapproved request; the panel must not send it.)
func TestAHostOnRecordThatSaysNoApprovalIsNeededIsNotBelieved(t *testing.T) {
	const canon = "power action=shutdown delay_s=10"
	for reason, want := range map[string]string{
		"off":           "laptop" + checkGoneText,
		"fan_window":    "laptop has the check on, and answered that this needs no approval (fan_window), which its check does not say for this request. Nothing was sent.",
		"boot_shutdown": "", // the machine's own exception for a shutdown: taken, and the ordinary confirmation follows
	} {
		t.Run(reason, func(t *testing.T) {
			a := newApprovalEnv(t)
			a.require(t, a.laptop)
			body := a.shutdownBody()
			a.gb.Lie("laptop", func(path string) (int, jsonx.M) {
				if path == api.PathApprovalChallenge {
					return 200, jsonx.M{"needed": false, "reason": reason, "request": canon}
				}
				return 0, nil
			})
			fresh := []string{"Cookie", a.cookie(controller(), 0), "X-Requested-With", "fairy", "Content-Type", "application/json"}
			r := a.do("POST", "/api/systems/laptop/power", body, fresh...)
			if want == "" {
				// (the real machine then refuses the request without an approval: it is not shortly after its boot)
				if r.code == 409 || !strings.Contains(sentPaths(a.laptop.Asked()), "/power") {
					t.Fatalf("the machine's own exception is taken: %d %s", r.code, r.raw)
				}
				return
			}
			if r.code != 409 || !strings.HasPrefix(fmt.Sprint(r.body["detail"]), want) {
				t.Fatalf("%d %s", r.code, r.raw)
			}
			if n := controlWrites(a.laptop); n != 0 {
				t.Fatalf("the machine was sent %q", sentPaths(a.laptop.Asked()))
			}
		})
	}
}

// An API token cannot give an approval, so a host on record as one with the check is sent nothing by a token,
// whatever its state was at the last poll. (One that read "broken" was left to answer for itself; with its
// trust file removed since, it ran the token's unapproved shutdown.)
func TestATokenSendsNothingToAHostOnRecord(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	body := a.shutdownBody()
	if err := os.WriteFile(a.laptop.TrustPath, []byte("not a trust file"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.laptop.Gate.StatusNow()
	a.poll()
	if ap := a.s.Poller.ApprovalOf("laptop"); ap == nil || ap.State != api.ApprovalBroken || !a.s.Poller.BindingOf("laptop").OK() {
		t.Fatalf("laptop reads as broken, in order: %+v", ap)
	}
	a.clearTrust(t, a.laptop) // the broken file is removed on the machine; no poll since
	tok, _, err := a.s.Tokens.Create("script", approvalAdminSub, "control", "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	r := a.do("POST", "/api/systems/laptop/power", body, "Authorization", "Bearer "+tok, "Content-Type", "application/json")
	if r.code != 403 || !strings.Contains(fmt.Sprint(r.body["detail"]), "an API token cannot give one") {
		t.Fatalf("the token: %d %s", r.code, r.raw)
	}
	if n := len(a.laptop.Asked()); n != 0 {
		t.Fatalf("the machine was sent %q", sentPaths(a.laptop.Asked()))
	}
}
