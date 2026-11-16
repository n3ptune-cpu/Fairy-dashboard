package web

import (
	"fmt"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// "Requested by" of a countdown is what the machine's agent answers, and somebody who runs the agent of a
// machine they may see can make it answer anything. The panel looked that text up among its accounts: it
// answered "Juniper" for the admin's address, for the address in other letters, for the admin's name and for
// the account id, so it could be asked whose user name an address or a name is. Now the text is shown when it
// is what the panel shows an account by, and nothing is looked up for it.
func TestRequestedByIsNotLookedUpAmongTheAccounts(t *testing.T) {
	r := newNamesRig(t, true)
	e := r.env
	e.cookie(model.User{Sub: "9a1b2c3d-wanda", Name: model.P("Waiting Wanda"), PreferredUsername: model.P("juniper"), Email: model.P("wanda@example.org"), EmailVerified: true}, 0)
	says := func(said string) string {
		e.b.Mu.Lock()
		e.b.Metrics["tower"] = func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			m["hostname"] = "tower"
			m["power_control"] = jsonx.M{"enabled": true, "pending": jsonx.M{"action": "shutdown", "at": float64(time.Now().Add(10 * time.Minute).Unix()), "delay_s": 600.0, "requested_by": said}}
			return m, nil
		}
		e.b.Mu.Unlock()
		e.s.Poller.PollOnce(t.Context())
		x := e.do("GET", "/api/systems/tower", nil, "Cookie", r.ck["sub-l"]) // a viewer of the site Bob
		pp, _ := x.body["power_pending"].(map[string]any)
		if x.code != 200 || pp == nil {
			t.Fatalf("the countdown of tower: %d %s", x.code, clipText(string(x.raw), 300))
		}
		// the same in the power menu and in the list
		pw := e.do("GET", "/api/systems/tower/power", nil, "Cookie", r.ck["sub-l"])
		if pending, _ := pw.body["pending"].(map[string]any); pending == nil || pending["requested_by"] != pp["requested_by"] {
			t.Errorf("the agent says %q: the power menu has %v, the machine's page %v", said, pw.body["pending"], pp["requested_by"])
		}
		return fmt.Sprint(pp["requested_by"])
	}
	for _, c := range []struct{ said, want string }{
		// what this panel told the agent, as the agent repeats it
		{"Juniper", "Juniper"},
		{"PineTop", "PineTop"},
		{"viewer-one", "viewer-one"},
		{"juniper  id 9a1b2c", "juniper · id 9a1b2c"},
		{"address hidden", "(address hidden)"},
		{"unknown", "unknown"},
		// what somebody makes an agent say
		{"owner@example.com", "(address hidden)"},
		{"OWNER@EXAMPLE.COM", "(address hidden)"},
		{"friend@example.com unverified", "(address hidden)"},
		{"old@example.com", "(address hidden)"},
		{"nobody@example.org", "(address hidden)"},
		{"Real Name", "(name hidden)"},
		{"real name", "(name hidden)"},
		{"Other Person", "(name hidden)"},
		{"Old Timer", "(name hidden)"},
		{"sub-n", "(name hidden)"},
		{"Some Stranger", "(name hidden)"},
	} {
		if got := says(c.said); got != c.want {
			t.Errorf("the agent of tower says requested_by=%q: a viewer is shown %q, want %q", c.said, got, c.want)
		}
	}
	// what the panel tells an agent comes back as that account: the countdown the admin starts is the admin's
	if got := says(e.s.whoShown(&model.User{Sub: "sub-n", Auth: "session"})); got != "Juniper" {
		t.Errorf("what the panel tells an agent about the admin, when the agent repeats it: %q", got)
	}
}
