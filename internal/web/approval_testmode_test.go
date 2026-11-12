//go:build fairytest

package web

import (
	"context"
	"fmt"
	"html"
	"net/url"
	"regexp"
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

// The way back from Pocket ID in a test build (audit 5, r2-b2-01). Pocket ID shows a page before it sends the
// browser back, so that return is a navigation another site started, and a browser sends neither of the two
// SameSite=Strict cookies with it. The gate answers exactly that request (a GET for the approval callback
// without the key cookie) with a page of its own that asks for the same address again; the second request
// carries the cookies and the mark, and runs the kept action. Everything else stays refused without the key.
func TestTestModeTheWayBackFromPocketID(t *testing.T) {
	var gb *testutil.GatedBackend
	e := setupWith(t, func(b poller.Backend) poller.Backend {
		gb = testutil.NewGatedBackend(b.(*testutil.Backend))
		return gb
	})
	e.s.Cfg.OIDC.ControlGroups = []string{TestGroup}
	iss := testutil.NewIssuer(t)
	iss.Set(func(i *testutil.Issuer) { i.Sub = "pocket-id-user-of-the-owner" })
	e.s.Cfg.Approval = config.Approval{ClientID: testApprovalClient, Issuer: iss.URL(), RunApprovalWaitS: 900,
		Users: []string{"pocket-id-user-of-the-owner"}}
	e.s.SetIDPClient(iss.Client())
	e.s.EnableTestMode(startKey)
	key := TestKeyCookie(strings.TrimPrefix(e.srv.URL, "http://"), startKey)
	e.also = key
	laptop := gb.Add(t, "laptop")
	e.s.Poller.PollOnce(context.Background())
	ck := e.open()
	hdr := []string{"Cookie", ck, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	id, _, ok := approvalAsked(e.do("POST", "/api/admin/approval/turn-on", map[string]any{"hosts": []string{"laptop"}}, hdr...))
	if !ok {
		t.Fatal("Turn on was not asked to approve")
	}
	go1 := e.do("POST", stepUpPath, map[string]any{"pending_id": id, "next": "/"}, hdr...)
	back := iss.Approve(t, fmt.Sprint(go1.body["url"]))

	// 1. what the browser sends when Pocket ID's page sends it back: the address, and no cookie at all
	e.also = ""
	first := e.do("GET", back.RequestURI(), nil)
	body := string(first.raw)
	if first.code != 200 || len(first.hdr.Values("Set-Cookie")) != 0 || first.hdr.Get("Cache-Control") != "no-store" ||
		first.hdr.Get("Referrer-Policy") != "no-referrer" || first.hdr.Get("Content-Security-Policy") != CSP ||
		!strings.HasPrefix(first.hdr.Get("Content-Type"), "text/html") {
		t.Fatalf("the return without cookies: %d %v\n%s", first.code, first.hdr, body)
	}
	if strings.Contains(body, "<script") || strings.Contains(body, startKey) || strings.Contains(body, strings.SplitN(key, "=", 2)[1]) ||
		strings.Contains(body, "fairytest-banner") || len(laptop.Passed()) != 0 {
		t.Fatalf("the page must show nothing of the panel and run nothing: %s", body)
	}
	// the address it asks for again: the same path, state and code as they came, and the mark
	m := regexp.MustCompile(`<meta http-equiv="refresh" content="0; url=([^"]+)">`).FindStringSubmatch(body)
	if m == nil || !strings.Contains(body, `<a href="`+m[1]+`" rel="noreferrer">Continue</a>`) {
		t.Fatalf("no refresh and link to one address: %s", body)
	}
	again, err := url.Parse(html.UnescapeString(m[1]))
	if err != nil || again.Path != config.ApprovalCallbackPath || again.Host != "" || again.Query().Get(testBackParam) != "1" {
		t.Fatalf("the second address: %q %v", m[1], err)
	}
	for _, k := range []string{"state", "code"} {
		if got, want := again.Query().Get(k), back.Query().Get(k); got != want || want == "" {
			t.Fatalf("%s did not survive the page: %q, want %q", k, got, want)
		}
	}

	// 2. the second request without the key cookie (a browser that never opened the printed address): refused, once
	if r := e.do("GET", again.RequestURI(), nil, "Cookie", ck); r.code != 403 || !strings.Contains(string(r.raw), testKeyRefusal) || len(laptop.Passed()) != 0 {
		t.Fatalf("the marked request without the key: %d %s", r.code, r.raw)
	}
	// the key alone is no session: the callback's own answer, and nothing ran
	if r := e.do("GET", "/auth/approval/callback?state=x&code=y&"+testBackParam+"=1", nil, "Cookie", key); r.code != 401 || len(laptop.Passed()) != 0 {
		t.Fatalf("the marked request with the key and without a session: %d %s", r.code, r.raw)
	}

	// 3. the second request as the browser sends it (a navigation of this site: both cookies): the kept action runs
	e.also = key
	if cb := e.do("GET", again.RequestURI(), nil, "Cookie", ck); cb.code != 303 {
		t.Fatalf("the marked request with both cookies: %d %s", cb.code, cb.raw)
	}
	if ap := laptop.Gate.StatusNow(); ap.State != api.ApprovalRequired {
		t.Fatalf("the machine after Turn on through the page: %+v", ap)
	}
	// 4. the same address once more, either form: nothing is waiting any more
	for _, p := range []string{again.RequestURI(), back.RequestURI()} {
		if cb := e.do("GET", p, nil, "Cookie", ck); cb.code != 400 || !strings.Contains(string(cb.raw), "No action is waiting") {
			t.Fatalf("a callback address used again: %d %s", cb.code, cb.raw)
		}
	}

	// 5. the page is for that one path and GET only, and it writes its address out again instead of repeating it
	e.also = ""
	for _, p := range [][2]string{{"HEAD", "/auth/approval/callback?state=a&code=b"}, {"POST", "/auth/approval/callback?state=a&code=b"},
		{"GET", "/auth/approval/callback/?state=a&code=b"}, {"GET", "/auth/approval/callbackx?state=a"}, {"GET", "/auth/approval?state=a"},
		{"GET", "/auth/callback?state=a&code=b"}, {"GET", "/auth/login"}, {"GET", "/api/auth/approval/callback"}, {"GET", "/"},
		{"GET", "/auth/approval/callback?state=a&" + testBackParam + "="},
		{"GET", "/auth/approval/callback?state=" + strings.Repeat("a", testBackMaxQuery)}} {
		r := e.do(p[0], p[1], nil)
		if r.code != 403 || len(r.hdr.Values("Set-Cookie")) != 0 || strings.Contains(string(r.raw), "refresh") {
			t.Errorf("%s %.60s without the key: %d %.200s", p[0], p[1], r.code, r.raw)
		}
	}
	hostile := e.do("GET", `/auth/approval/callback?state=%22%3E%3Cscript%3Ealert(1)%3C/script%3E&code=a%2Bb+c&x=%27+url=//evil.example/&y=1;z=2&code=2`, nil)
	hb := string(hostile.raw)
	want := "/auth/approval/callback?code=a%2Bb+c&amp;code=2&amp;fairytest_back=1&amp;state=%22%3E%3Cscript%3Ealert%281%29%3C%2Fscript%3E&amp;x=%27+url%3D%2F%2Fevil.example%2F" // a member with ";" is not one (net/url) and is left out
	if hostile.code != 200 || strings.Count(hb, want) != 2 || strings.Contains(hb, "<script") || strings.Contains(hb, "'") || strings.Contains(hb, ";z") {
		t.Fatalf("a hostile query on the page: %d\n%s", hostile.code, hb)
	}
}
