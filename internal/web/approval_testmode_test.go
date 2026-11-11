//go:build fairytest

package web

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// A test build has no sign-in, but the approval a machine verifies is real: the dialog's button goes to the
// identity provider for an action a machine checks, the answer is verified like in a release, and the one
// thing that is skipped is the comparison of the approver with the signed-in person (there is none), with a
// loud line in the log. Who may approve on a machine comes from approval.users alone: the test admin is no
// Pocket ID user and never gets onto a machine's list.
func TestTestBuildApprovalIsReal(t *testing.T) {
	var gb *testutil.GatedBackend
	e := setupWith(t, func(b poller.Backend) poller.Backend {
		gb = testutil.NewGatedBackend(b.(*testutil.Backend))
		return gb
	})
	e.s.Cfg.OIDC.ControlGroups = []string{TestGroup}
	iss := testutil.NewIssuer(t)
	iss.Set(func(i *testutil.Issuer) { i.Sub = "pocket-id-user-of-the-owner" })
	e.s.Cfg.Approval = config.Approval{ClientID: testApprovalClient, Issuer: iss.URL(), RunApprovalWaitS: 900}
	e.s.SetIDPClient(iss.Client())
	e.s.EnableTestMode(startKey)
	e.also = TestKeyCookie(strings.TrimPrefix(e.srv.URL, "http://"), startKey) // a browser that opened the printed address
	if !e.s.approvalAnyPerson {
		t.Fatal("EnableTestMode switches the same-person check off")
	}
	laptop := gb.Add(t, "laptop")
	e.s.Poller.PollOnce(context.Background())
	ck := e.open()
	hdr := []string{"Cookie", ck, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	logs := captureLog(t)

	// without approval.users nobody could approve: the machine is not ready, and the page says what to set
	page := e.do("GET", "/api/admin/approval", nil, "Cookie", ck)
	if page.code != 200 {
		t.Fatalf("page: %d %s", page.code, page.raw)
	}
	m := page.body["machines"].([]any)[0].(map[string]any)
	if page.body["any_person"] != true || m["id"] != "laptop" || m["ready"] != false || !strings.Contains(fmt.Sprint(m["why"]), "approval.users") || len(m["users"].([]any)) != 0 {
		t.Fatalf("page: %s", page.raw)
	}
	e.s.Cfg.Approval.Users = []string{"pocket-id-user-of-the-owner"}
	r := e.do("POST", "/api/admin/approval/turn-on", map[string]any{"hosts": []string{"laptop"}}, hdr...)
	id, _, ok := approvalAsked(r)
	if !ok || strings.Contains(fmt.Sprint(r.body["detail"].(map[string]any)["items"]), TestUser.Sub) {
		t.Fatalf("Turn on in a test build: %d %s", r.code, r.raw)
	}
	// the dialog's button: not "confirmed locally", but the address of the identity provider
	go1 := e.do("POST", stepUpPath, map[string]any{"pending_id": id, "next": "/"}, hdr...)
	if go1.code != 200 || !strings.HasPrefix(fmt.Sprint(go1.body["url"]), iss.URL()+"/authorize?") || len(laptop.Asked()) != 1 {
		t.Fatalf("the button for an approval must go to Pocket ID: %d %s", go1.code, go1.raw)
	}
	back := iss.Approve(t, fmt.Sprint(go1.body["url"]))
	if cb := e.do("GET", back.RequestURI(), nil, "Cookie", ck); cb.code != 303 {
		t.Fatalf("callback: %d %s", cb.code, cb.raw)
	}
	if ap := laptop.Gate.StatusNow(); ap.State != api.ApprovalRequired || ap.Users != 1 {
		t.Fatalf("the machine after Turn on: %+v", ap)
	}
	if got := logs.take(); !strings.Contains(got, "TEST BUILD: an approval was accepted without checking that the approver is the person signed in") {
		t.Errorf("the log does not say that the same-person check was skipped:\n%s", got)
	}
	e.s.Poller.PollOnce(context.Background())
	// an ordinary action (one no machine checks) is still confirmed locally, without the identity provider
	n := len(iss.Asked())
	st := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, hdr...)
	if d, _ := st.body["detail"].(map[string]any); st.code == 401 {
		c := e.do("POST", stepUpPath, map[string]any{"pending_id": d["pending_id"], "next": "/"}, hdr...)
		if c.code != 200 || strings.HasPrefix(fmt.Sprint(c.body["url"]), "http") {
			t.Fatalf("an ordinary confirmation in a test build: %d %s", c.code, c.raw)
		}
		ck = sessionCookie(e, c.hdr) // the local confirmation gives the session a new id, like a real one
		hdr[1] = ck
	} else {
		t.Fatalf("the ordinary action was not asked to confirm: %d %s", st.code, st.raw)
	}
	if len(iss.Asked()) != n {
		t.Fatal("an ordinary confirmation went to the identity provider")
	}
	// everything else of the approval is still checked: an answer that is too old is refused
	body := map[string]any{"action": "shutdown", "delay_s": 10, "confirm": e.do("GET", "/api/systems/laptop", nil, "Cookie", ck).body["name"]}
	r = e.do("POST", "/api/systems/laptop/power", body, hdr...)
	id, _, ok = approvalAsked(r)
	if !ok {
		t.Fatalf("a shutdown of the machine: %d %s", r.code, r.raw)
	}
	iss.Set(func(i *testutil.Issuer) {
		i.Edit = func(_ string, c map[string]any) { c["auth_time"] = c["auth_time"].(int64) - 600 }
	})
	go2 := e.do("POST", stepUpPath, map[string]any{"pending_id": id, "next": "/"}, hdr...)
	back = iss.Approve(t, fmt.Sprint(go2.body["url"]))
	if cb := e.do("GET", back.RequestURI(), nil, "Cookie", ck); cb.code != 400 || len(laptop.Passed()) != 0 {
		t.Fatalf("a stale approval in a test build: %d, passed %d", cb.code, len(laptop.Passed()))
	}
}
