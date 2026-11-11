package agentcontract_test

// The check on the machine itself, end to end on Linux: the panel's real web API, poller and approval round trip
// talk over HTTP to the real fairy-agent handler with its real gate (pkg/approval: challenges, the trust file,
// the offline check of the ID token), and a fake Pocket ID (testutil.Issuer: https, its own key, a token signed
// at run time for every request). Nothing of the check is scripted: a request that reaches the fake BMC, the
// fake shutdown command or the updater got past the agent's own verifier.
//
// What is not real here: the passkey (the issuer approves every request it is asked), the Windows helper (the
// agents check themselves, as on Linux), and the hardware.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/agenttest"
	agentapi "github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/collect"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/ipmi/fakebmc"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/approval"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release/releasetest"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/web"
)

const approvalClient = "pc-actions"

// gated is one machine: the real agent handler with its own gate, behind a recorder of what the panel sends.
type gated struct {
	id      string
	srv     *httptest.Server
	trust   string // the machine's trust file
	bmc     *fakebmc.BMC
	power   *agenttest.PowerExec
	applied atomic.Int32 // the agent started its installer

	mu    sync.Mutex
	ahead time.Duration // how far the machine's clock is ahead of the test's
	posts []post        // every POST the panel sent, in order
	hold  string        // the next POST to this path is kept from the agent and answered 503
	held  []byte        // its body
}

type post struct {
	path string
	body []byte
}

func (g *gated) now() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Add(g.ahead)
}

// later moves the machine's clock ahead.
func (g *gated) later(d time.Duration) {
	g.mu.Lock()
	g.ahead += d
	g.mu.Unlock()
}

func (g *gated) sent(path string) [][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out [][]byte
	for _, p := range g.posts {
		if p.path == path {
			out = append(out, p.body)
		}
	}
	return out
}

// direct sends a control request straight to the agent, as someone holding the panel's control token would.
func (g *gated) direct(t *testing.T, path string, body []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", g.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+ctlTok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Direct", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func startGated(t *testing.T, id string) *gated {
	t.Helper()
	cfg, err := agent.Parse([]byte(`
listen: 127.0.0.1:9101
ipmi: {enabled: true}
fan_control: {enabled: true}
power_control: {enabled: true}
self_update: {enabled: true}
control_rate_per_min: 60
`), filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	g := &gated{id: id, bmc: fakebmc.X11SCAF(), power: &agenttest.PowerExec{}, trust: filepath.Join(t.TempDir(), approval.TrustFileName)}
	bmc := agent.NewBMC(g.bmc, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go bmc.Run(ctx)
	for deadline := time.Now().Add(5 * time.Second); bmc.Readings() == nil && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	gh := releasetest.New(t, release.AgentRepo)
	asset := release.AssetName(release.AgentProgram, "linux", "amd64")
	gh.Publish("build-42", release.AgentProgram, "42-1a2b3c4", map[string][]byte{asset: []byte("fairy-agent 42")}, nil)
	stage := t.TempDir()
	upd := agent.NewUpdater(agent.UpdaterOptions{Version: "contract", Key: gh.Pub, Client: gh.Client(),
		StageDir: filepath.Join(stage, "update"), ResultPath: filepath.Join(stage, "result.json"), Asset: asset,
		ResultWait: 100 * time.Millisecond, Apply: func(context.Context) error { g.applied.Add(1); return nil }})
	gate := approval.NewGate(approval.Options{Files: approval.Files{Path: g.trust}, EnforcedBy: agentapi.EnforcedByAgent,
		Now: g.now, Uptime: func() (time.Duration, error) { return time.Hour, nil }})
	var a *agent.Agent
	a = agent.New(cfg, agent.Tokens{Read: readTok, Control: ctlTok}, agent.Deps{Collector: agenttest.NewCollector(), GPU: agenttest.GPU{},
		BMC: bmc, Updater: upd, Version: "contract", Approval: gate,
		Fan:   agent.NewFan(g.bmc, bmc, cfg.FanControl, func() []collect.Temp { return a.LocalTemps() }),
		Power: agent.NewPower(g.power, cfg.PowerControl.MinDelayS)})
	h := a.Handler()
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("X-Direct") == "" {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			g.mu.Lock()
			g.posts = append(g.posts, post{r.URL.Path, body})
			held := g.hold == r.URL.Path
			if held {
				g.hold, g.held = "", body
			}
			g.mu.Unlock()
			if held {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(503)
				_, _ = w.Write([]byte(`{"error":"kept from the agent by the test"}`))
				return
			}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// approvalPanel is the panel in front of two such machines, and one browser with an admin's session that was
// made by a real sign-in at the issuer.
type approvalPanel struct {
	t      *testing.T
	iss    *testutil.Issuer
	a, b   *gated
	p      *poller.Poller
	srv    *httptest.Server
	c      *http.Client
	audit  string
	mu     sync.Mutex
	bodies []string // every answer the browser got
}

func newApprovalPanel(t *testing.T) *approvalPanel {
	t.Helper()
	pn := &approvalPanel{t: t, iss: testutil.NewIssuer(t), a: startGated(t, "pc-a"), b: startGated(t, "pc-b")}
	t.Setenv("E2E_READ", readTok)
	t.Setenv("E2E_CONTROL", ctlTok)
	dir := t.TempDir()
	pn.audit = filepath.Join(dir, "audit.log")
	entry := func(g *gated) string {
		return fmt.Sprintf("  - {id: %s, name: %s, url: %q, token_env: E2E_READ, power_control: true, self_update: true, fan_control: {vendor: supermicro, control_token_env: E2E_CONTROL}}\n", g.id, strings.ToUpper(g.id), g.srv.URL)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
public_url: https://panel.example.com
request_timeout: 2
control_rate_per_min: 60
data_dir: %q
audit_log: %q
oidc: {issuer: %q, client_id: cid, allowed_groups: [homelab], control_groups: [admins]}
approval: {client_id: %s}
agents:
`, filepath.Join(dir, "data"), pn.audit, pn.iss.URL(), approvalClient) + entry(pn.a) + entry(pn.b)))
	if err != nil {
		t.Fatal(err)
	}
	secrets := config.Secrets{SessionSecret: strings.Repeat("s", 48), InsecureCookies: true}
	if pn.p, err = poller.New(cfg, secrets, nil); err != nil {
		t.Fatal(err)
	}
	ws := web.New(cfg, secrets, pn.p, nil, "test")
	ws.SetIDPClient(pn.iss.Client())
	pn.srv = httptest.NewServer(ws.Handler())
	t.Cleanup(pn.srv.Close)
	jar, _ := cookiejar.New(nil)
	pn.c = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	pn.poll()
	// the sign-in: the panel sends the browser to the issuer, the issuer back to the panel's callback
	resp := pn.get("/auth/login")
	back := pn.iss.Approve(t, resp.Header.Get("Location"))
	if back.Path != "/auth/callback" {
		t.Fatalf("the sign-in comes back to %s", back)
	}
	if resp := pn.get(back.RequestURI()); resp.StatusCode != 303 {
		t.Fatalf("sign-in callback: %d", resp.StatusCode)
	}
	if st, me := pn.call("GET", "/api/me", nil); st != 200 || me["can_control"] != true || me["approval_client"] != true {
		t.Fatalf("the signed-in admin: %d %v", st, me)
	}
	return pn
}

// poll lets the panel ask both agents now (an agent computes its /metrics at most every 250 ms).
func (pn *approvalPanel) poll() {
	time.Sleep(300 * time.Millisecond)
	pn.p.PollOnce(context.Background())
}

func (pn *approvalPanel) get(path string) *http.Response {
	pn.t.Helper()
	resp, err := pn.c.Get(pn.srv.URL + path)
	if err != nil {
		pn.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	pn.mu.Lock()
	pn.bodies = append(pn.bodies, string(raw), fmt.Sprint(resp.Header))
	pn.mu.Unlock()
	return resp
}

func (pn *approvalPanel) call(method, path string, body any) (int, map[string]any) {
	pn.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, pn.srv.URL+path, rd)
	req.Header.Set("X-Requested-With", "fairy")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := pn.c.Do(req)
	if err != nil {
		pn.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	pn.mu.Lock()
	pn.bodies = append(pn.bodies, string(raw), fmt.Sprint(resp.Header))
	pn.mu.Unlock()
	out := map[string]any{}
	if len(raw) > 0 && raw[0] == '{' {
		_ = json.Unmarshal(raw, &out)
	}
	return resp.StatusCode, out
}

// asked reports whether the answer is the dialog for an approval the machines check, and returns the kept
// action's id, the machines it names and its lines.
func asked(st int, out map[string]any) (id string, machines []any, items string, ok bool) {
	d, _ := out["detail"].(map[string]any)
	ap, _ := d["approval"].(map[string]any)
	if st != 401 || d["code"] != "step_up_required" || ap == nil || ap["client"] != "Fairy PC actions" {
		return "", nil, "", false
	}
	id, _ = d["pending_id"].(string)
	machines, _ = ap["machines"].([]any)
	return id, machines, fmt.Sprint(d["items"]), id != ""
}

// approve is the rest of the dialog: Continue, the issuer, the panel's callback, and the result of the action
// the panel then ran by itself.
func (pn *approvalPanel) approve(id string) map[string]any {
	pn.t.Helper()
	st, out := pn.call("POST", "/api/me/step-up", map[string]any{"pending_id": id, "next": "/"})
	if st != 200 {
		pn.t.Fatalf("Continue: %d %v", st, out)
	}
	to, err := url.Parse(fmt.Sprint(out["url"]))
	if err != nil || !strings.HasPrefix(to.String(), pn.iss.URL()+"/authorize?") || to.Query().Get("client_id") != approvalClient ||
		to.Query().Get("max_age") != "0" || to.Query().Get("code_challenge_method") != "S256" {
		pn.t.Fatalf("the browser is sent to %v", out["url"])
	}
	back := pn.iss.Approve(pn.t, to.String())
	if back.Path != config.ApprovalCallbackPath {
		pn.t.Fatalf("the approval comes back to %s", back)
	}
	if resp := pn.get(back.RequestURI()); resp.StatusCode != 303 {
		pn.t.Fatalf("approval callback: %d", resp.StatusCode)
	}
	_, me := pn.call("GET", "/api/me", nil)
	res, _ := me["pending_result"].(map[string]any)
	if res == nil {
		pn.t.Fatal("the approved action did not run: no pending_result in /api/me")
	}
	return res
}

func (pn *approvalPanel) state(id string) map[string]any {
	pn.t.Helper()
	_, sys := pn.call("GET", "/api/systems/"+id, nil)
	ap, _ := sys["approval"].(map[string]any)
	if ap == nil {
		pn.t.Fatalf("%s has no approval block: %v", id, sys)
	}
	return ap
}

// turnOn switches the check on for the machines through the panel, with one approval, and returns the result.
func (pn *approvalPanel) turnOn(ids ...string) map[string]any {
	pn.t.Helper()
	st, out := pn.call("POST", "/api/admin/approval/turn-on", map[string]any{"hosts": ids})
	id, machines, _, ok := asked(st, out)
	if !ok || len(machines) != len(ids) {
		pn.t.Fatalf("Turn on: %d %v", st, out)
	}
	res := pn.approve(id)
	pn.poll()
	return res
}

func (pn *approvalPanel) shutdown(g *gated, delay int) (int, map[string]any) {
	return pn.call("POST", "/api/systems/"+g.id+"/power", map[string]any{"action": "shutdown", "delay_s": delay, "confirm": strings.ToUpper(g.id)})
}

// noLeak: no ID token the issuer handed out is in anything the browser got or in the audit log.
func (pn *approvalPanel) noLeak() {
	pn.t.Helper()
	log, _ := os.ReadFile(pn.audit)
	pn.mu.Lock()
	all := strings.Join(pn.bodies, "\n") + string(log)
	pn.mu.Unlock()
	toks := pn.iss.Tokens()
	if len(toks) < 2 {
		pn.t.Fatalf("the issuer handed out %d tokens: nothing to look for", len(toks))
	}
	for _, tok := range toks {
		parts := strings.Split(tok, ".")
		if strings.Contains(all, tok) || strings.Contains(all, parts[1]) || strings.Contains(all, parts[2]) {
			pn.t.Fatal("an ID token (or a part of one) is in an answer to the browser or in the audit log")
		}
	}
}

// Turn on, proven: the machine gets its trust from the panel only together with an approval for exactly that
// trust, both machines with ONE approval, and afterwards the panel cannot change it again.
func TestE2ETurnOnThenLocked(t *testing.T) {
	pn := newApprovalPanel(t)
	for _, g := range []*gated{pn.a, pn.b} {
		if ap := pn.state(g.id); ap["state"] != "off" || ap["enforced_by"] != "agent" {
			t.Fatalf("%s before Turn on: %v", g.id, ap)
		}
	}
	// while the check is off everything is as it always was: the fresh sign-in is the confirmation
	if st, out := pn.shutdown(pn.a, 60); st != 200 || len(pn.a.power.Scheduled) != 1 {
		t.Fatalf("a shutdown while the check is off: %d %v", st, out)
	}
	if st, out := pn.call("POST", "/api/systems/pc-a/power", map[string]any{"action": "cancel"}); st != 200 {
		t.Fatalf("cancel: %d %v", st, out)
	}
	if n := len(pn.a.sent(agentapi.PathApprovalChallenge)); n != 0 {
		t.Fatalf("a machine whose check is off was asked for %d challenges", n)
	}
	tokens := len(pn.iss.Tokens())
	res := pn.turnOn("pc-a", "pc-b")
	if res["ok"] != true || !strings.Contains(fmt.Sprint(res["message"]), "2 of 2 machines now check approvals themselves") {
		t.Fatalf("Turn on: %v", res)
	}
	if got := len(pn.iss.Tokens()) - tokens; got != 1 {
		t.Fatalf("%d approvals for one Turn on of two machines", got)
	}
	for _, g := range []*gated{pn.a, pn.b} {
		ap := pn.state(g.id)
		if ap["state"] != "required" || ap["issuer"] != pn.iss.URL() || ap["client_id"] != approvalClient || ap["users"] != 1.0 ||
			len(ap["keys"].([]any)) != 1 || ap["keys"].([]any)[0].(map[string]any)["kid"] != pn.iss.Kid {
			t.Fatalf("%s after Turn on: %v", g.id, ap)
		}
		if st, err := os.Stat(g.trust); err != nil || st.Size() == 0 {
			t.Fatalf("%s has no trust file: %v", g.id, err)
		}
	}
	before, _ := os.ReadFile(pn.a.trust)
	// locked: a second Turn on is not offered, and the machine itself refuses a new trust, also with the
	// control token and a proposal of its own
	st, out := pn.call("POST", "/api/admin/approval/turn-on", map[string]any{"hosts": []string{"pc-a"}})
	if _, _, _, ok := asked(st, out); ok || st < 400 {
		t.Fatalf("a second Turn on: %d %v", st, out)
	}
	trusts := pn.a.sent(agentapi.PathApprovalTrust)
	if len(trusts) != 1 {
		t.Fatalf("pc-a was sent %d trusts", len(trusts))
	}
	if st, out := pn.a.direct(t, agentapi.PathApprovalTrust, trusts[0]); st != 409 || out["code"] != agentapi.CodeTrustAlreadySet {
		t.Fatalf("the same trust again, straight to the agent: %d %v", st, out)
	}
	var other map[string]any
	_ = json.Unmarshal(trusts[0], &other)
	other["users"] = []string{"someone-else"}
	raw, _ := json.Marshal(other)
	if st, out := pn.a.direct(t, agentapi.PathApprovalTrust, raw); st != 409 || out["code"] != agentapi.CodeTrustAlreadySet {
		t.Fatalf("another list of users, straight to the agent: %d %v", st, out)
	}
	if after, _ := os.ReadFile(pn.a.trust); !bytes.Equal(before, after) {
		t.Fatal("the trust file changed after it was set")
	}
	pn.noLeak()
}

// A machine that has no trust yet refuses a trust that comes without an approval for it, or with the approval
// that was given for another proposal.
func TestE2ETrustNeedsItsOwnApproval(t *testing.T) {
	pn := newApprovalPanel(t)
	pn.a.mu.Lock()
	pn.a.hold = agentapi.PathApprovalTrust
	pn.a.mu.Unlock()
	st, out := pn.call("POST", "/api/admin/approval/turn-on", map[string]any{"hosts": []string{"pc-a"}})
	id, _, _, ok := asked(st, out)
	if !ok {
		t.Fatalf("Turn on: %d %v", st, out)
	}
	pn.approve(id) // the trust was kept from the agent: it is in pn.a.held, with a good approval
	var sent map[string]any
	if err := json.Unmarshal(pn.a.held, &sent); err != nil || sent["approval"] == nil {
		t.Fatalf("what the panel sent: %s", pn.a.held)
	}
	send := func(change func(m map[string]any)) (int, map[string]any) {
		m := map[string]any{}
		_ = json.Unmarshal(pn.a.held, &m)
		change(m)
		raw, _ := json.Marshal(m)
		return pn.a.direct(t, agentapi.PathApprovalTrust, raw)
	}
	if st, out := send(func(m map[string]any) { delete(m, "approval") }); st != 403 || out["code"] != agentapi.CodeTrustProofRequired {
		t.Fatalf("a trust without an approval: %d %v", st, out)
	}
	if st, out := send(func(m map[string]any) { m["users"] = []string{"attacker"} }); st != 403 || out["code"] != agentapi.CodeTrustProofRefused {
		t.Fatalf("another list of users under the approval for the admin's list: %d %v", st, out)
	}
	if _, err := os.Stat(pn.a.trust); err == nil {
		t.Fatal("a refused trust was written")
	}
	if st, out := send(func(map[string]any) {}); st != 200 {
		t.Fatalf("the trust the approval was given for: %d %v", st, out)
	}
	pn.poll()
	if ap := pn.state("pc-a"); ap["state"] != "required" {
		t.Fatalf("after the accepted trust: %v", ap)
	}
}

// An approved shutdown: accepted by the agent's own verifier, for exactly the request the admin saw. The same
// approval is refused for another action, for another delay, on another machine, and a second time.
func TestE2EApprovedShutdownReplayAndSwap(t *testing.T) {
	pn := newApprovalPanel(t)
	pn.turnOn("pc-a", "pc-b")
	// without an approval the agent refuses, whoever asks
	if st, out := pn.a.direct(t, "/power", []byte(`{"action":"shutdown","delay_s":60}`)); st != 403 || out["code"] != agentapi.CodeApprovalRequired {
		t.Fatalf("a shutdown without an approval, straight to the agent: %d %v", st, out)
	}
	// the fresh sign-in does not count for a machine that checks itself
	st, out := pn.shutdown(pn.a, 60)
	id, machines, _, ok := asked(st, out)
	if !ok || len(machines) != 1 || machines[0] != "PC-A" {
		t.Fatalf("the shutdown must ask for the machine's approval: %d %v", st, out)
	}
	if d := out["detail"].(map[string]any); !strings.Contains(fmt.Sprint(d["action"]), "shutdown") || !strings.Contains(fmt.Sprint(d["message"]), "PC-A checks this approval itself") {
		t.Errorf("the dialog: %v", d)
	}
	if len(pn.a.power.Scheduled) != 0 {
		t.Fatal("something was scheduled before the approval")
	}
	if res := pn.approve(id); res["ok"] != true {
		t.Fatalf("the approved shutdown: %v", res)
	}
	if got := pn.a.power.Scheduled; len(got) != 1 || got[0] != "shutdown@1m0s" {
		t.Fatalf("the machine scheduled: %v", got)
	}
	sent := pn.a.sent("/power")
	good := sent[len(sent)-1]
	if !bytes.Contains(good, []byte(`"approval"`)) {
		t.Fatalf("the request the agent accepted carried no approval: %s", good)
	}
	// replay: the same request with the same approval, a second time
	if st, out := pn.a.direct(t, "/power", good); st != 403 || out["code"] != agentapi.CodeApprovalRefused {
		t.Fatalf("the replay: %d %v", st, out)
	}
	// cancel needs no approval (it only stops something), then a second approval that is kept from the agent
	if st, out := pn.call("POST", "/api/systems/pc-a/power", map[string]any{"action": "cancel"}); st != 200 {
		t.Fatalf("cancel: %d %v", st, out)
	}
	pn.a.mu.Lock()
	pn.a.hold = "/power"
	pn.a.mu.Unlock()
	st, out = pn.shutdown(pn.a, 60)
	if id, _, _, ok = asked(st, out); !ok {
		t.Fatalf("the second shutdown: %d %v", st, out)
	}
	if res := pn.approve(id); res["ok"] == true {
		t.Fatalf("the request was kept from the agent, and the panel says: %v", res)
	}
	swap := func(change func(m map[string]any)) []byte {
		m := map[string]any{}
		_ = json.Unmarshal(pn.a.held, &m)
		change(m)
		raw, _ := json.Marshal(m)
		return raw
	}
	for name, body := range map[string][]byte{
		"reboot instead of shutdown": swap(func(m map[string]any) { m["action"] = "reboot" }),
		"another delay":              swap(func(m map[string]any) { m["delay_s"] = 10 }),
		"sleep":                      swap(func(m map[string]any) { m["action"] = "sleep"; delete(m, "delay_s") }),
	} {
		if st, out := pn.a.direct(t, "/power", body); st != 403 || out["code"] != agentapi.CodeApprovalRefused {
			t.Errorf("%s under the approval for a shutdown in 60 s: %d %v", name, st, out)
		}
	}
	if st, out := pn.b.direct(t, "/power", pn.a.held); st != 403 || out["code"] != agentapi.CodeApprovalRefused {
		t.Errorf("pc-a's approval on pc-b: %d %v", st, out)
	}
	if st, out := pn.a.direct(t, "/fan/mode", swap(func(m map[string]any) {
		for k := range m {
			if k != "approval" {
				delete(m, k)
			}
		}
		m["mode"] = "full"
	})); st != 403 || out["code"] != agentapi.CodeApprovalRefused {
		t.Errorf("a fan change under the approval for a shutdown: %d %v", st, out)
	}
	if len(pn.a.power.Scheduled) != 1 || len(pn.b.power.Scheduled) != 0 {
		t.Fatalf("a swapped request was carried out: %v %v", pn.a.power.Scheduled, pn.b.power.Scheduled)
	}
	// and the approval was a good one all along: exactly the request it was given for is accepted, once
	if st, out := pn.a.direct(t, "/power", pn.a.held); st != 200 {
		t.Fatalf("the request the approval was given for: %d %v", st, out)
	}
	if st, out := pn.a.direct(t, "/power", pn.a.held); st == 200 {
		t.Fatalf("and a second time: %d %v", st, out)
	}
	if len(pn.a.power.Scheduled) != 2 {
		t.Fatalf("scheduled: %v", pn.a.power.Scheduled)
	}
	log, _ := os.ReadFile(pn.audit)
	if !strings.Contains(string(log), "auth.approval") || !strings.Contains(string(log), "power.shutdown") {
		t.Errorf("the audit log lacks the approval or the shutdown:\n%s", log)
	}
	pn.noLeak()
}

// Fans: the first change needs an approval; the machine then takes further fan changes for its fan window
// (30 s) without one, and asks again afterwards.
func TestE2EFanWindow(t *testing.T) {
	pn := newApprovalPanel(t)
	pn.turnOn("pc-a")
	st, out := pn.call("POST", "/api/systems/pc-a/fan/mode", map[string]any{"mode": "full"})
	id, _, _, ok := asked(st, out)
	if !ok {
		t.Fatalf("the first fan change must ask for the machine's approval: %d %v", st, out)
	}
	if mode, _ := pn.a.bmc.State(); mode != 2 {
		t.Fatalf("the fan mode changed before the approval: %d", mode)
	}
	if res := pn.approve(id); res["ok"] != true {
		t.Fatalf("the approved fan change: %v", res)
	}
	if mode, _ := pn.a.bmc.State(); mode != 1 {
		t.Fatalf("fan mode after the approved change: %d, want 1 (full)", mode)
	}
	tokens := len(pn.iss.Tokens())
	// inside the window: the next change goes through at once, and the agent takes it without an approval
	if st, out := pn.call("POST", "/api/systems/pc-a/fan/duty", map[string]any{"zone": 0, "percent": 60}); st != 200 {
		t.Fatalf("a second fan change inside the machine's window: %d %v", st, out)
	}
	if _, duty := pn.a.bmc.State(); duty[0] != 60 {
		t.Fatalf("duty: %v", duty)
	}
	duties := pn.a.sent("/fan/duty")
	if len(duties) != 1 || bytes.Contains(duties[0], []byte(`"approval"`)) || len(pn.iss.Tokens()) != tokens {
		t.Fatalf("the change inside the window carried an approval: %s", duties)
	}
	// a power action never rides on the fan window
	if st, out := pn.shutdown(pn.a, 60); st != 401 {
		t.Fatalf("a shutdown inside the fan window: %d %v", st, out)
	}
	// after the window, by the machine's clock: asked again, and nothing changes until then
	pn.a.later(31 * time.Second)
	st, out = pn.call("POST", "/api/systems/pc-a/fan/duty", map[string]any{"zone": 0, "percent": 80})
	if _, _, _, ok := asked(st, out); !ok {
		t.Fatalf("after the machine's fan window a fan change must ask again: %d %v", st, out)
	}
	if st, out := pn.a.direct(t, "/fan/duty", []byte(`{"zone":0,"percent":80}`)); st != 403 || out["code"] != agentapi.CodeApprovalRequired {
		t.Fatalf("straight to the agent after the window: %d %v", st, out)
	}
	if _, duty := pn.a.bmc.State(); duty[0] != 60 {
		t.Fatalf("duty changed without an approval: %v", duty)
	}
}

// Updates: one agent with its own approval, then "Update all" for two machines with ONE approval that each
// machine checks for its own entry.
func TestE2EUpdateAndUpdateAll(t *testing.T) {
	pn := newApprovalPanel(t)
	pn.turnOn("pc-a", "pc-b")
	wait := func(g *gated, n int32) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); g.applied.Load() < n && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		if g.applied.Load() != n {
			t.Fatalf("%s started its installer %d times, want %d", g.id, g.applied.Load(), n)
		}
	}
	if st, out := pn.a.direct(t, "/update", []byte(`{}`)); st != 403 || out["code"] != agentapi.CodeApprovalRequired {
		t.Fatalf("an update without an approval, straight to the agent: %d %v", st, out)
	}
	st, out := pn.call("POST", "/api/admin/updates/pc-a", map[string]any{})
	id, machines, _, ok := asked(st, out)
	if !ok || len(machines) != 1 {
		t.Fatalf("the update must ask for the machine's approval: %d %v", st, out)
	}
	if res := pn.approve(id); res["ok"] != true {
		t.Fatalf("the approved update: %v", res)
	}
	wait(pn.a, 1)
	if pn.b.applied.Load() != 0 {
		t.Fatal("pc-b was updated with pc-a's approval")
	}
	// the installer of the test does not replace the agent, so the same agent can be asked again once its
	// updater is idle
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		pn.poll()
		_, page := pn.call("GET", "/api/admin/updates", nil)
		idle := 0
		for _, r := range page["agents"].([]any) {
			if r.(map[string]any)["can_update"] == true {
				idle++
			}
		}
		if idle == 2 {
			break
		}
	}
	tokens := len(pn.iss.Tokens())
	st, out = pn.call("POST", "/api/admin/updates", map[string]any{})
	id, machines, items, ok := asked(st, out)
	if !ok || len(machines) != 2 {
		t.Fatalf("Update all: %d %v", st, out)
	}
	if !strings.Contains(items, "PC-A: update of its agent (checked on the machine)") || !strings.Contains(items, "PC-B: update of its agent (checked on the machine)") {
		t.Errorf("the dialog's lines: %s", items)
	}
	if res := pn.approve(id); res["ok"] != true {
		t.Fatalf("the approved Update all: %v", res)
	}
	wait(pn.a, 2)
	wait(pn.b, 1)
	if got := len(pn.iss.Tokens()) - tokens; got != 1 {
		t.Fatalf("%d approvals for one Update all of two machines", got)
	}
	// each machine got the whole list and found its own entry in it; the other machine's request with it is
	// refused
	ua, ub := pn.a.sent("/update"), pn.b.sent("/update")
	if st, out := pn.b.direct(t, "/update", ua[len(ua)-1]); st != 403 || out["code"] != agentapi.CodeApprovalRefused {
		t.Fatalf("pc-a's update request on pc-b: %d %v", st, out)
	}
	if st, out := pn.b.direct(t, "/update", ub[len(ub)-1]); st != 403 || out["code"] != agentapi.CodeApprovalRefused {
		t.Fatalf("pc-b's update request a second time: %d %v", st, out)
	}
	pn.noLeak()
}

// A machine whose trust file is damaged: shown as broken, refused with words that say what to do, nobody is
// sent to Pocket ID, and the agent itself refuses whatever it is sent. The other machine is not affected.
func TestE2EBrokenMachine(t *testing.T) {
	pn := newApprovalPanel(t)
	pn.turnOn("pc-a", "pc-b")
	if err := os.WriteFile(pn.a.trust, []byte("{ not a trust file"), 0o600); err != nil {
		t.Fatal(err)
	}
	pn.a.later(6 * time.Second) // the agent looks at the file again at most every 5 s
	pn.poll()
	if ap := pn.state("pc-a"); ap["state"] != "broken" || ap["error"] == "" {
		t.Fatalf("pc-a with a damaged trust file: %v", ap)
	}
	tokens := len(pn.iss.Tokens())
	st, out := pn.shutdown(pn.a, 60)
	if msg := fmt.Sprint(out["detail"]); st != 409 || !strings.Contains(msg, "cannot check approvals right now") {
		t.Fatalf("a shutdown on a broken machine: %d %v", st, out)
	}
	if st, out := pn.a.direct(t, "/power", []byte(`{"action":"shutdown","delay_s":60}`)); st != 503 || out["code"] != agentapi.CodeApprovalUnavailable {
		t.Fatalf("straight to the broken agent: %d %v", st, out)
	}
	if len(pn.a.power.Scheduled) != 0 || len(pn.iss.Tokens()) != tokens {
		t.Fatal("a broken machine: something was scheduled, or somebody was sent to Pocket ID")
	}
	// the run page keeps both out of "Update unpowered hosts"
	_, page := pn.call("GET", "/api/admin/wake-update", nil)
	for _, h := range page["hosts"].([]any) {
		m := h.(map[string]any)
		if m["choice"] != "" || m["approval"] == nil {
			t.Errorf("%v can be chosen for a run", m)
		}
	}
	// pc-b works as before
	st, out = pn.shutdown(pn.b, 60)
	id, _, _, ok := asked(st, out)
	if !ok {
		t.Fatalf("pc-b: %d %v", st, out)
	}
	if res := pn.approve(id); res["ok"] != true || len(pn.b.power.Scheduled) != 1 {
		t.Fatalf("pc-b's approved shutdown: %v %v", res, pn.b.power.Scheduled)
	}
}

// The machine decides by its own list. A token that Pocket ID really signed for this client and for the
// machine's own challenge, but for a person who is not on the machine's list, is refused by the agent, also
// when it is handed to it directly (as a panel that checked nothing would). The same request with a token for
// the admin is accepted: the only difference is who approved.
func TestE2EMachineDecidesByItsOwnList(t *testing.T) {
	pn := newApprovalPanel(t)
	pn.turnOn("pc-a")
	// token asks the issuer for an ID token for this nonce, the way the panel does (code flow with PKCE)
	token := func(nonce string) string {
		t.Helper()
		const verifier = "e2e-verifier-e2e-verifier-e2e-verifier-e2e-verifier"
		sum := sha256.Sum256([]byte(verifier))
		q := url.Values{"client_id": {approvalClient}, "nonce": {nonce}, "state": {"s"}, "response_type": {"code"},
			"redirect_uri": {"https://panel.example.com" + config.ApprovalCallbackPath}, "code_challenge_method": {"S256"},
			"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
		back := pn.iss.Approve(t, pn.iss.URL()+"/authorize?"+q.Encode())
		resp, err := pn.iss.Client().PostForm(pn.iss.URL()+"/token", url.Values{"grant_type": {"authorization_code"},
			"code": {back.Query().Get("code")}, "code_verifier": {verifier}, "redirect_uri": q["redirect_uri"]})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			IDToken string `json:"id_token"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.IDToken == "" {
			t.Fatal("no ID token from the issuer", err)
		}
		return out.IDToken
	}
	// shutdown asks the agent for a challenge, gets a token for it as sub, and sends the shutdown with it
	shutdown := func(sub string) (int, map[string]any) {
		t.Helper()
		st, ch := pn.a.direct(t, agentapi.PathApprovalChallenge, []byte(`{"verb":"power","action":"shutdown","delay_s":60}`))
		if st != 200 || ch["needed"] != true || ch["request"] != "power action=shutdown delay_s=60" {
			t.Fatalf("challenge: %d %v", st, ch)
		}
		entries := []agentapi.ApprovalEntry{{Challenge: fmt.Sprint(ch["challenge"]), Request: fmt.Sprint(ch["request"])}}
		nonce, err := agentapi.ApprovalNonce(entries)
		if err != nil {
			t.Fatal(err)
		}
		pn.iss.Set(func(i *testutil.Issuer) { i.Sub = sub })
		body, _ := json.Marshal(map[string]any{"action": "shutdown", "delay_s": 60, "approval": agentapi.Approval{Token: token(nonce), Entries: entries}})
		return pn.a.direct(t, "/power", body)
	}
	if st, out := shutdown("someone-else"); st != 403 || out["code"] != agentapi.CodeApprovalRefused {
		t.Fatalf("an approval by a person who is not on the machine's list: %d %v", st, out)
	}
	if len(pn.a.power.Scheduled) != 0 {
		t.Fatal("the machine scheduled a shutdown for a person who is not on its list")
	}
	if st, out := shutdown("u2"); st != 200 || len(pn.a.power.Scheduled) != 1 {
		t.Fatalf("the same with the admin's approval: %d %v", st, out)
	}
}
