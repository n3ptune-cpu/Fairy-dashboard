package web

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// The round trip of an approval, with a kept action the test puts into the store itself (the routes that make
// one are tested in approval_actions_test.go): what Pocket ID is asked, what the callback verifies, that the
// kept action runs exactly once and only with its own approval, and that the token goes nowhere else.

const testApprovalClient = "pc-actions"

// approvalAdmin is signedIn with the approval client configured (same Pocket ID as the sign-in).
func approvalAdmin(t *testing.T) *armed {
	t.Helper()
	a := signedIn(t)
	a.e.s.Cfg.Approval.ClientID = testApprovalClient
	return a
}

// goodApproval makes the identity provider answer like Pocket ID does for the client "Fairy PC actions" after
// a passkey: the audience is that client, the authentication is from now.
func (a *armed) goodApproval() {
	a.idp.claims = map[string]any{"aud": testApprovalClient, "amr": []string{"phr"}, "auth_time": a.clk.now().Unix()}
}

func testChallenge(n byte) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = n + byte(i)
	}
	return approvaltestB64(b)
}

// ran is what the kept action's handler saw when the panel ran it.
type ran struct {
	times int
	token string
	sub   string
	err   error
}

// testMachineID is the machine id the round trip's tests have on record for "laptop".
const testMachineID = "laptop-machine-id-0123456789"

// keepWithPlan puts a kept action with an approval plan for one shutdown of "laptop" into the session's store,
// as askApproval does, and returns its id and what its handler records.
func (a *armed) keepWithPlan(t *testing.T) (string, *ran) {
	t.Helper()
	delay, action := 60, "shutdown"
	req := api.ApprovalRequest{Verb: api.ApprovePower, Action: &action, DelayS: &delay}
	canon, _ := req.Canonical()
	plan, err := approval.NewPlan([]approval.Bound{{Target: approval.Target{AgentID: "laptop", Name: "LAPTOP", Request: req},
		Canonical: canon, Challenge: testChallenge(7), MachineID: testMachineID}})
	if err != nil {
		t.Fatal(err)
	}
	out := &ran{}
	e := a.e
	p := &pendingAction{ID: randToken(18), Method: "POST", Pattern: "POST /api/systems/{id}/power", Vals: map[string]string{"id": "laptop"},
		URL: url.URL{Path: "/api/systems/laptop/power"}, Body: `{}`, Label: "Shut down LAPTOP", Action: "power action on laptop (action: shutdown)",
		At: e.s.now(), Client: client{ip: testIP, ua: uaHashOf(testUA)}, Approval: newApprovalPlan(plan),
		run: func(c *reqCtx) error {
			out.times++
			if g := c.approved(); g != nil {
				ap, err := g.For("laptop", testMachineID, req)
				out.err, out.sub = err, g.sub
				if ap != nil {
					out.token = ap.Token
				}
			}
			c.json(200, map[string]any{"ok": true})
			return nil
		}}
	e.s.codec.live.keepPending(sidOf(e, a.sess), p, e.s.now())
	return p.ID, out
}

// startApproval is "Continue" in the dialog: the query of the address at Pocket ID the panel answers with.
func (a *armed) startApproval(t *testing.T, id string) url.Values {
	t.Helper()
	r := a.e.do("POST", stepUpPath, map[string]any{"pending_id": id, "next": "/#/system/laptop"},
		"Cookie", a.sess, "X-Requested-With", "fairy", "Content-Type", "application/json")
	if r.code != 200 {
		t.Fatalf("Continue: %d %s", r.code, r.raw)
	}
	loc, err := url.Parse(fmt.Sprint(r.body["url"]))
	if err != nil || !strings.HasPrefix(loc.String(), a.idp.srv.URL+"/authorize?") {
		t.Fatalf("the approval's address: %q", r.body["url"])
	}
	q := loc.Query()
	a.idp.mu.Lock()
	a.idp.challenge, a.idp.nonce = q.Get("code_challenge"), q.Get("nonce")
	a.idp.mu.Unlock()
	return q
}

func (a *armed) approvalCallback(state string) resp {
	return a.e.do("GET", "/auth/approval/callback?code=c&state="+url.QueryEscape(state), nil, "Cookie", a.sess)
}

func TestApprovalRoundTrip(t *testing.T) {
	a := approvalAdmin(t)
	// the address config check tells the owner to register is the one the panel answers on
	if _, pat := a.e.s.mux.Handler(mustRequest("GET", config.ApprovalCallbackPath)); pat != "GET "+config.ApprovalCallbackPath {
		t.Fatalf("the callback %s is routed to %q", config.ApprovalCallbackPath, pat)
	}
	logs := captureLog(t)
	id, out := a.keepWithPlan(t)
	q := a.startApproval(t, id)
	// One request to Pocket ID: the other client, PKCE, the plan's nonce, a fresh authentication, the callback.
	wantNonce := "" // the nonce of the one-entry list, made here with the agent module's function
	{
		n, err := api.ApprovalNonce([]api.MachineEntry{{MachineID: testMachineID, Challenge: testChallenge(7), Request: "power action=shutdown delay_s=60"}})
		if err != nil {
			t.Fatal(err)
		}
		wantNonce = n
	}
	if q.Get("client_id") != testApprovalClient || q.Get("nonce") != wantNonce || q.Get("max_age") != "0" || q.Get("code_challenge_method") != "S256" ||
		q.Get("redirect_uri") != "https://panel.example.com/auth/approval/callback" || q.Get("response_type") != "code" ||
		!strings.Contains(q.Get("scope"), "openid") || strings.Contains(q.Get("scope"), "groups") || q.Get("state") == "" {
		t.Fatalf("the request to Pocket ID: %v", q)
	}
	a.goodApproval()
	sidBefore := sidOf(a.e, a.sess)
	r := a.approvalCallback(q.Get("state"))
	if r.code != 303 || r.hdr.Get("Location") != "/#/system/laptop" || !a.idp.verifierOK {
		t.Fatalf("callback: %d %q %s", r.code, r.hdr.Get("Location"), r.raw)
	}
	if out.times != 1 || out.err != nil || out.token == "" || strings.Count(out.token, ".") != 2 || out.sub != "u1" {
		t.Fatalf("the kept action ran %d times, token %d bytes, approver %q, err %v", out.times, len(out.token), out.sub, out.err)
	}
	// It is no sign-in: the session keeps its id, and the panel's window is not opened by it.
	if sidOf(a.e, a.sess) != sidBefore || len(r.hdr.Values("Set-Cookie")) != 0 && sidOf(a.e, sessionCookie(a.e, r.hdr)) != sidBefore {
		t.Fatal("an approval must not change the session")
	}
	if id := a.shutdown(); id == "" {
		t.Fatal("an approval must not open the panel's confirmation window for other actions")
	}
	// The same answer again, and the kept action's state at the sign-in's callback: nothing runs. (Since the
	// rebase onto fix8 the page says what came of the action, 409 "Already approved ... already sent", and no
	// longer 400 "The approval was not accepted ... Nothing was done." for an action that ran: approvalrepeat.go.)
	if r := a.approvalCallback(q.Get("state")); r.code != 409 || !strings.Contains(string(r.raw), "The action was already sent. It is not sent again.") ||
		len(r.hdr.Values("Set-Cookie")) != 0 || out.times != 1 {
		t.Fatalf("a second callback: %d %s, ran %d times", r.code, r.raw, out.times)
	}
	// The result is shown once; the token is in no answer to the browser, no log line and no audit entry.
	me := a.e.do("GET", "/api/me", nil, "Cookie", a.sess)
	if pr, _ := me.body["pending_result"].(map[string]any); pr == nil || pr["ok"] != true {
		t.Fatalf("the result of the approved action: %s", me.raw)
	}
	audit, _ := os.ReadFile(a.e.s.Cfg.AuditLog)
	for where, text := range map[string]string{"the callback's answer": string(r.raw), "/api/me": string(me.raw), "the log": logs.take(), "the audit log": string(audit)} {
		if strings.Contains(text, out.token) || strings.Contains(text, strings.Split(out.token, ".")[2]) {
			t.Errorf("the token is in %s", where)
		}
	}
	if !strings.Contains(string(audit), `"auth.approval"`) || !strings.Contains(string(audit), `"runs":"power action on laptop (action: shutdown)"`) ||
		!strings.Contains(string(audit), `"approver":"u1"`) {
		t.Errorf("the audit log lacks the approval:\n%s", audit)
	}
}

// What the callback refuses. After each refusal the kept action is gone: it never runs later.
func TestApprovalCallbackRefusals(t *testing.T) {
	big := strings.Repeat("x", api.MaxApprovalToken)
	for name, c := range map[string]struct {
		claims func(a *armed, m map[string]any)
		state  func(s string) string
		reason string
	}{
		"a token of the sign-in client":   {claims: func(_ *armed, m map[string]any) { m["aud"] = "cid" }, reason: "invalid id_token"},
		"a token of another client":       {claims: func(_ *armed, m map[string]any) { m["aud"] = "someone-else" }, reason: "invalid id_token"},
		"another nonce":                   {claims: func(_ *armed, m map[string]any) { m["nonce"] = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" }, reason: "nonce"},
		"another person":                  {claims: func(_ *armed, m map[string]any) { m["sub"] = "u2" }, reason: "user"},
		"an authentication 10 min ago":    {claims: func(a *armed, m map[string]any) { m["auth_time"] = a.clk.now().Add(-10 * time.Minute).Unix() }, reason: "auth_time"},
		"no auth_time":                    {claims: func(_ *armed, m map[string]any) { delete(m, "auth_time") }, reason: "auth_time_missing"},
		"no passkey":                      {claims: func(_ *armed, m map[string]any) { m["amr"] = []string{"pwd"} }, reason: "amr"},
		"a token larger than 8192 bytes":  {claims: func(_ *armed, m map[string]any) { m["padding"] = big }, reason: "token_size"},
		"an expired token":                {claims: func(_ *armed, m map[string]any) { m["exp"] = time.Now().Add(-time.Hour).Unix() }, reason: "invalid id_token"},
		"another issuer":                  {claims: func(_ *armed, m map[string]any) { m["iss"] = "https://evil.example" }, reason: "invalid id_token"},
		"another state":                   {state: func(string) string { return "not-the-state" }, reason: "nothing waiting"},
		"no state":                        {state: func(string) string { return "" }, reason: "nothing waiting"},
		"Pocket ID answers with an error": {state: func(s string) string { return s + "&error=access_denied" }, reason: "access_denied"},
	} {
		t.Run(name, func(t *testing.T) {
			a := approvalAdmin(t)
			id, out := a.keepWithPlan(t)
			q := a.startApproval(t, id)
			a.goodApproval()
			if c.claims != nil {
				c.claims(a, a.idp.claims)
			}
			state := q.Get("state")
			var r resp
			if c.state != nil {
				r = a.e.do("GET", "/auth/approval/callback?code=c&state="+c.state(state), nil, "Cookie", a.sess)
			} else {
				r = a.approvalCallback(state)
			}
			if r.code != 400 || out.times != 0 || !strings.Contains(string(r.raw), "Nothing was done") {
				t.Fatalf("answer %d, the kept action ran %d times: %s", r.code, out.times, r.raw)
			}
			if got := auditOutcomes(a.e.s.Cfg.AuditLog); !strings.Contains(got, "auth.approval failed: "+c.reason) {
				t.Errorf("the audit log lacks %q:\n%s", "auth.approval failed: "+c.reason, got)
			}
			// nothing is left armed: a good answer for the same round trip afterwards runs nothing
			a.goodApproval()
			if r := a.approvalCallback(state); r.code != 400 || out.times != 0 || keptID(a.e, a.sess) != "" {
				t.Fatalf("after the refusal: %d, ran %d times, still kept %q", r.code, out.times, keptID(a.e, a.sess))
			}
		})
	}
}

// A kept action that waits for an approval runs only after its own round trip: an ordinary "Confirm it's you"
// (the sign-in client, by the dialog's own button or by a link) never runs it, and forgets it.
func TestApprovalActionNeverRunsOnAnOrdinaryConfirmation(t *testing.T) {
	a := approvalAdmin(t)
	_, out := a.keepWithPlan(t)
	if r := a.stepUp("&next=/", false); r.code != 303 {
		t.Fatal("the ordinary step-up itself works:", r.code)
	}
	if out.times != 0 || keptID(a.e, a.sess) != "" {
		t.Fatalf("a sign-in link ran or kept the action: ran %d, kept %q", out.times, keptID(a.e, a.sess))
	}
	// the answer of an approval round trip, sent to the sign-in's callback: refused there, nothing runs
	id, out := a.keepWithPlan(t)
	q := a.startApproval(t, id)
	a.goodApproval()
	if r := a.e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", a.sess); r.code != 400 || out.times != 0 {
		t.Fatalf("the sign-in's callback: %d, ran %d times", r.code, out.times)
	}
	if r := a.approvalCallback(q.Get("state")); r.code != 400 || out.times != 0 {
		t.Fatalf("after that the action is gone: %d, ran %d times", r.code, out.times)
	}
	// resumePending itself refuses it (the last line of defence, whoever calls it)
	id, out = a.keepWithPlan(t)
	_ = id
	l := a.e.s.codec.live
	l.mu.Lock()
	p := l.m[sidOf(a.e, a.sess)].pending
	l.mu.Unlock()
	a.e.s.resumePending(&reqCtx{s: a.e.s, r: mustRequest("GET", "/auth/callback")}, p)
	if out.times != 0 {
		t.Fatal("resumePending ran an action that needs an approval")
	}
}

// The approval's answer is taken only from the client that asked, with a session, and without an API token.
func TestApprovalCallbackNeedsTheSameClientAndSession(t *testing.T) {
	a := approvalAdmin(t)
	id, out := a.keepWithPlan(t)
	q := a.startApproval(t, id)
	a.goodApproval()
	if r := a.e.do("GET", "/auth/approval/callback?code=c&state="+q.Get("state"), nil, "Cookie", a.sess, "User-Agent", "another browser"); r.code != 400 || out.times != 0 {
		t.Fatalf("another browser: %d, ran %d times", r.code, out.times)
	}
	if !strings.Contains(auditOutcomes(a.e.s.Cfg.AuditLog), "auth.approval failed: another client") {
		t.Error("the audit log does not say that another client came back")
	}
	id, out = a.keepWithPlan(t)
	q = a.startApproval(t, id)
	if r := a.e.do("GET", "/auth/approval/callback?code=c&state="+q.Get("state"), nil); r.code != 401 || out.times != 0 {
		t.Fatalf("without a session: %d, ran %d times", r.code, out.times)
	}
	if r := a.e.do("GET", "/auth/approval/callback?code=c&state=x", nil, "Authorization", "Bearer fy_x"); r.code != 403 {
		t.Fatalf("an API token on the callback: %d", r.code)
	}
	// a Pocket ID that cannot be reached: the button says so, and the action is forgotten
	a = approvalAdmin(t)
	id, out = a.keepWithPlan(t)
	a.e.s.Cfg.Approval.ClientID = ""
	a.e.s.Cfg.Approval.Issuer = "http://127.0.0.1:1"
	if r := a.e.do("POST", stepUpPath, map[string]any{"pending_id": id, "next": "/"}, "Cookie", a.sess, "X-Requested-With", "fairy",
		"Content-Type", "application/json"); r.code != 502 || keptID(a.e, a.sess) != "" || out.times != 0 {
		t.Fatalf("an unreachable Pocket ID: %d %s", r.code, r.raw)
	}
}

func approvaltestB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func uaHashOf(ua string) [sha256.Size]byte { return sha256.Sum256([]byte(ua)) }

func mustRequest(method, path string) *http.Request { return httptest.NewRequest(method, path, nil) }
