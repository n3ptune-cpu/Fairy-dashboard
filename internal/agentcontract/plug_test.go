package agentcontract_test

// Switching a plug's socket, end to end (PLUGS-DESIGN.md, stage 1a): the panel's real web API, poller and
// approval round trip talk over HTTP to the real fairy-agent handler with its real gate, its real keeper of
// sockets (pkg/plugs) and a fake Home Assistant that records every request it sees (pkg/plugs/hatest). The
// passkey is a token of the fake Pocket ID, signed at run time. Nothing of the check is scripted: a request
// that reaches the fake Home Assistant got past the agent's own verifier.
//
// What is not real here: the passkey itself (the issuer approves every request it is asked), Home Assistant
// and the plugs, and Windows (the agent checks for itself, as on Linux).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/agenttest"
	agentapi "github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/approval"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/plugs"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/plugs/hatest"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/web"
)

// The variable the agent of these tests keeps its one Home Assistant token in (reading and switching: the
// normal setup). Not a FAIRY_ name: those are refused.
const plugTokenEnv = "CONTRACT_HA_TOKEN"

// plugClock is the clock of the agent's keeper of sockets: a wait moves it on and returns at once, so the
// looks at a socket after a switch (half a second, then every second) take no time.
type plugClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *plugClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *plugClock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}
func (c *plugClock) Pause(_ context.Context, d time.Duration) bool { c.add(d); return true }

// plugMachine is the agent that reads the plugs: the real handler behind a recorder, its relay, its keeper of
// sockets, and the fake Home Assistant.
type plugMachine struct {
	*gated
	ha    *hatest.Server
	relay *agent.Relay
	plugs *plugs.Plugs
	clk   *plugClock
}

// read lets the agent read its meters and its sockets once, as its loops do every few seconds.
func (m *plugMachine) read() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m.relay.PollOnce(ctx)
	m.plugs.PollOnce(ctx)
}

// startPlugMachine starts the agent that reads the plugs. realClock: its keeper of sockets waits for real, as
// on a machine (a switch whose socket does not follow then takes about 10 s); else a wait takes no time.
func startPlugMachine(t *testing.T, id string, realClock bool) *plugMachine {
	t.Helper()
	m := &plugMachine{ha: hatest.New(agenttest.PlugStates()), clk: &plugClock{now: time.Unix(1794000000, 0)}}
	t.Cleanup(m.ha.Close)
	m.ha.SetSensor(agenttest.PlugBarSensor, "95.5", "W")
	m.ha.SetSensor(agenttest.PlugCornerSens, "410", "W")
	m.ha.SetSensor(agenttest.PlugOwnSensor, "61.2", "W")
	t.Setenv(plugTokenEnv, hatest.Token())
	cfg, err := agent.Parse([]byte("listen: 127.0.0.1:9101\ncontrol_rate_per_min: 60\npower_control: {enabled: true}\n"+
		agenttest.PlugConfig(m.ha.URL, agenttest.PlugOptions{ReadEnv: plugTokenEnv, SwitchEnv: plugTokenEnv, Switching: true})),
		filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if m.relay, err = cfg.NewRelay(); err != nil {
		t.Fatal(err)
	}
	po, ok, err := cfg.PlugOptions(m.relay)
	if err != nil || !ok {
		t.Fatalf("the agent's config has no sockets: %v", err)
	}
	if !realClock {
		po.Now, po.Pause = m.clk.Now, m.clk.Pause
	}
	m.plugs = plugs.New(po)
	g := &gated{id: id, power: &agenttest.PowerExec{}, trust: filepath.Join(t.TempDir(), approval.TrustFileName)}
	g.gate = approval.NewGate(approval.Options{Files: approval.Files{Path: g.trust}, EnforcedBy: agentapi.EnforcedByAgent,
		Now: g.now, Uptime: func() (time.Duration, error) { return time.Hour, nil }})
	m.gated = g
	m.read()
	h := agent.New(cfg, agent.Tokens{Read: readTok, Control: ctlTok}, agent.Deps{Collector: agenttest.NewCollector(), GPU: agenttest.GPU{},
		Version: "contract", Approval: g.gate, Relay: m.relay, Plugs: m.plugs, Power: agent.NewPower(g.power, cfg.PowerControl.MinDelayS)}).Handler()
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Header.Get("X-Direct") == "" {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			g.mu.Lock()
			g.posts = append(g.posts, post{r.URL.Path, body})
			g.mu.Unlock()
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return m
}

// plugPanel is the panel in front of the machine that reads the plugs (pc-a) and a PC that hangs on one of its
// sockets (pc-b): the first socket of the powerbar is linked to that PC.
type plugPanel struct {
	*approvalPanel
	m *plugMachine
}

func newPlugPanel(t *testing.T) *plugPanel { return newPlugPanelWith(t, false) }

func newPlugPanelWith(t *testing.T, realClock bool) *plugPanel {
	t.Helper()
	m := startPlugMachine(t, "pc-a", realClock)
	pn := &approvalPanel{t: t, iss: testutil.NewIssuer(t), a: m.gated, b: startGated(t, "pc-b"), dir: t.TempDir()}
	t.Setenv("E2E_READ", readTok)
	t.Setenv("E2E_CONTROL", ctlTok)
	pn.audit = filepath.Join(pn.dir, "audit.log")
	timeout := 2 // request_timeout: the panel's usual wait for an agent is this + 8 s
	if realClock {
		timeout = 1
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
public_url: https://panel.example.com
request_timeout: %d
control_rate_per_min: 60
data_dir: %q
audit_log: %q
oidc: {issuer: %q, client_id: cid, allowed_groups: [homelab], control_groups: [admins]}
approval: {client_id: %s}
agents:
  - {id: pc-a, name: PC-A, url: %q, token_env: E2E_READ, control_token_env: E2E_CONTROL, power_control: true,
     socket_links: {%s/ch1: pc-b}}
  - {id: pc-b, name: PC-B, url: %q, token_env: E2E_READ, control_token_env: E2E_CONTROL, power_control: true}
`, timeout, filepath.Join(pn.dir, "data"), pn.audit, pn.iss.URL(), approvalClient, m.srv.URL, agenttest.PlugBar, pn.b.srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	secrets := config.Secrets{SessionSecret: strings.Repeat("s", 48), InsecureCookies: true}
	if pn.p, err = poller.New(cfg, secrets, nil); err != nil {
		t.Fatal(err)
	}
	pn.ws = web.New(cfg, secrets, pn.p, nil, "test")
	pn.ws.SetIDPClient(pn.iss.Client())
	pn.srv = httptest.NewServer(pn.ws.Handler())
	t.Cleanup(pn.srv.Close)
	pn.poll()
	pn.signIn()
	return &plugPanel{approvalPanel: pn, m: m}
}

// look lets the agent read its sockets and the panel ask the agent, so the panel's view is the plugs' now.
func (pp *plugPanel) look() {
	pp.m.read()
	pp.poll()
}

// socket is one socket as GET /api/power gives it to the admin.
func (pp *plugPanel) socket(plug, id string) map[string]any {
	pp.t.Helper()
	_, ov := pp.call("GET", "/api/power", nil)
	srcs, _ := ov["sources"].([]any)
	for _, s := range srcs {
		src, _ := s.(map[string]any)
		if src["id"] != "pc-a/"+plug {
			continue
		}
		socks, _ := src["sockets"].([]any)
		for _, k := range socks {
			if x, _ := k.(map[string]any); x["id"] == id {
				return x
			}
		}
	}
	pp.t.Fatalf("GET /api/power has no socket %s/%s", plug, id)
	return nil
}

// flip asks the panel to switch a socket, with the tag the panel's own answer gave for it.
func (pp *plugPanel) flip(plug, id, to string, more map[string]any) (int, map[string]any) {
	pp.t.Helper()
	body := map[string]any{"source": "pc-a/" + plug, "socket": id, "to": to, "tag": pp.socket(plug, id)["tag"]}
	for k, v := range more {
		body[k] = v
	}
	return pp.call("POST", "/api/power/socket", body)
}

// haPosts are the requests that change something which the fake Home Assistant saw, as "<path> <body>".
func (pp *plugPanel) haPosts() []string {
	var out []string
	for _, q := range pp.m.ha.Posts() {
		out = append(out, q.Path+" "+q.Body)
	}
	return out
}

func detailOf(out map[string]any) (msg, code string) {
	switch d := out["detail"].(type) {
	case string:
		return d, ""
	case map[string]any:
		msg, _ = d["message"].(string)
		code, _ = d["code"].(string)
	}
	return msg, code
}

func TestE2EPlugSwitch(t *testing.T) {
	pp := newPlugPanel(t)
	ha := pp.m.ha

	// 1. Before the machine has the passkey check: its sockets are shown, and none can be switched. A switch
	// request is refused by the panel itself, and the agent would refuse it too (409 approval_off).
	if s := pp.socket(agenttest.PlugBar, "ch2"); s["state"] != "off" || s["switchable"] != false || s["why"] != agentapi.WhyApprovalOff || s["name"] != "ch2" {
		t.Fatalf("a socket of a machine without the check: %v", s)
	}
	if st, out := pp.flip(agenttest.PlugBar, "ch2", "on", nil); st != 409 || !strings.Contains(fmt.Sprint(out["detail"]), "PC-A has no passkey check. Plugs are switched only with one.") {
		t.Fatalf("a switch on a machine without the check: %d %v", st, out)
	}
	tag := agentapi.SocketTag(agenttest.PlugBar, "ch2", "ch2", agenttest.PlugBar2)
	raw, _ := json.Marshal(map[string]any{"source": agenttest.PlugBar, "socket": "ch2", "to": "on", "tag": tag})
	if st, out := pp.m.direct(t, agentapi.PathPlugSwitch, raw); st != 409 || out["code"] != agentapi.CodeApprovalOff {
		t.Fatalf("the agent itself, without the check: %d %v", st, out)
	}
	if n := len(ha.Posts()); n != 0 {
		t.Fatalf("Home Assistant got %d requests that change something", n)
	}

	// 2. The check is switched on from the panel (one passkey), for both machines.
	if res := pp.turnOn("pc-a", "pc-b"); res["ok"] != true {
		t.Fatalf("Turn on: %v", res)
	}
	pp.look()
	// what the panel now says about each socket: the agent's own rules, and the panel's link
	for _, c := range []struct {
		plug, id, state string
		can             bool
		why, link       string
	}{
		{agenttest.PlugBar, "ch1", "on", true, "", "pc-b"},                     // allowed on the machine; the panel links it to PC-B
		{agenttest.PlugBar, "ch2", "off", true, "", ""},                        // allowed, linked to nothing
		{agenttest.PlugBar, "ch3", "on", false, agentapi.WhyNotAllowed, ""},    // not in switch_allow
		{agenttest.PlugCorner, "main", "on", false, agentapi.WhyFeedsSelf, ""}, // in feeds_self
		{agenttest.PlugOwn, "main", "on", false, "reads_plugs", "pc-a"},        // system_hint: self: the agent locks it, and so does the panel
	} {
		s := pp.socket(c.plug, c.id)
		why, _ := s["why"].(string)
		link, _ := s["system_id"].(string)
		if s["state"] != c.state || s["switchable"] != c.can || why != c.why || link != c.link {
			t.Errorf("%s/%s: %v", c.plug, c.id, s)
		}
	}
	// the panel's answer carries no entity id and no address of Home Assistant
	_, ov := pp.call("GET", "/api/power", nil)
	if all := fmt.Sprint(ov); strings.Contains(all, "switch.") || strings.Contains(all, strings.TrimPrefix(ha.URL, "http://")) {
		t.Errorf("GET /api/power names an entity or Home Assistant's address")
	}

	// 3. A plain switch: on, for a socket that feeds no machine. One passkey, one request to Home Assistant.
	st, out := pp.flip(agenttest.PlugBar, "ch2", "on", nil)
	id, machines, _, ok := asked(st, out)
	if !ok || len(machines) != 1 || machines[0] != "PC-A" {
		t.Fatalf("switch on: %d %v", st, out)
	}
	if d := out["detail"].(map[string]any); d["signs"] != "switch on ch2 (Monitor powerbar)" {
		t.Errorf("the dialog says: %v", d["signs"])
	}
	if n := len(ha.Posts()); n != 0 {
		t.Fatalf("before the approval Home Assistant got %d requests that change something", n)
	}
	res := pp.approve(id)
	if res["ok"] != true || res["message"] != "ch2 is on." {
		t.Fatalf("result: %v", res)
	}
	if got := pp.haPosts(); len(got) != 1 || got[0] != `/api/services/switch/turn_on {"entity_id":"`+agenttest.PlugBar2+`"}` {
		t.Fatalf("Home Assistant got: %v", got)
	}
	if ha.State(agenttest.PlugBar2) != "on" {
		t.Fatal("the socket is not on")
	}
	// the request the agent got from the panel: ids, the direction, the tag, who, the approval; nothing else
	sent := pp.m.sent(agentapi.PathPlugSwitch)
	var body map[string]any
	_ = json.Unmarshal(sent[len(sent)-1], &body)
	for k := range body {
		if !map[string]bool{"source": true, "socket": true, "to": true, "tag": true, "requested_by": true, "approval": true}[k] {
			t.Errorf("the panel's request has a member %q", k)
		}
	}
	if body["source"] != agenttest.PlugBar || body["socket"] != "ch2" || body["to"] != "on" || body["tag"] != tag {
		t.Errorf("the panel's request: %v", body)
	}
	pp.look()
	if s := pp.socket(agenttest.PlugBar, "ch2"); s["state"] != "on" {
		t.Errorf("the panel shows %v after the switch", s["state"])
	}

	// 4. The same approval again, sent straight to the agent by whoever holds the control token: refused, and
	// Home Assistant is asked nothing.
	ha.Forget()
	if st, out := pp.m.direct(t, agentapi.PathPlugSwitch, sent[len(sent)-1]); st != 403 || out["code"] != agentapi.CodeApprovalRefused {
		t.Fatalf("the used approval again: %d %v", st, out)
	}
	if n := ha.Count(); n != 0 {
		t.Fatalf("Home Assistant was asked %d things for a used approval", n)
	}

	// 5. Off for the socket PC-B hangs on, while PC-B runs: refused by the panel, nothing is asked of anybody.
	n := len(pp.m.sent(agentapi.PathApprovalChallenge))
	st, out = pp.flip(agenttest.PlugBar, "ch1", "off", nil)
	if msg, code := detailOf(out); st != 409 || code != "machine_running" || msg != "PC-B is still running. Shut it down first." {
		t.Fatalf("off for a socket whose machine runs: %d %v", st, out)
	}
	if st, out := pp.flip(agenttest.PlugBar, "ch1", "off", map[string]any{"cut": true, "confirm": "PC-A"}); st != 422 {
		t.Fatalf("cut with another machine's name: %d %v", st, out)
	}
	if got := len(pp.m.sent(agentapi.PathApprovalChallenge)); got != n || ha.Count() != 0 {
		t.Fatalf("the agent was asked %d questions and Home Assistant %d things for refused requests", got-n, ha.Count())
	}
	// "Cut power anyway", with PC-B's name typed: the passkey, then exactly one turn_off
	st, out = pp.flip(agenttest.PlugBar, "ch1", "off", map[string]any{"cut": true, "confirm": "PC-B"})
	id, _, _, ok = asked(st, out)
	if !ok || out["detail"].(map[string]any)["signs"] != "switch off Left monitor (Monitor powerbar): cut the power of PC-B while it is running" {
		t.Fatalf("cut power anyway: %d %v", st, out)
	}
	if res := pp.approve(id); res["ok"] != true || res["message"] != "Left monitor is off." {
		t.Fatalf("result: %v", res)
	}
	if got := pp.haPosts(); len(got) != 1 || got[0] != `/api/services/switch/turn_off {"entity_id":"`+agenttest.PlugBar1+`"}` {
		t.Fatalf("Home Assistant got: %v", got)
	}

	// 6. On again at once: the machine's own gap between two switches of one socket. The panel says how long
	// to wait; Home Assistant gets nothing.
	pp.look()
	ha.Forget()
	st, out = pp.flip(agenttest.PlugBar, "ch1", "on", nil)
	if id, _, _, ok = asked(st, out); !ok {
		t.Fatalf("on again: %d %v", st, out)
	}
	res = pp.approve(id)
	if res["ok"] != false || !strings.HasPrefix(fmt.Sprint(res["message"]), "Wait ") || !strings.HasSuffix(fmt.Sprint(res["message"]), " s before you switch Left monitor again.") {
		t.Fatalf("inside the gap: %v", res)
	}
	if got := pp.haPosts(); len(got) != 0 {
		t.Fatalf("inside the gap Home Assistant got: %v", got)
	}
	pp.m.clk.add(time.Minute) // the gap is over

	// 7. Home Assistant answers the switch request with an error: said as it is, the approval is used up, and
	// nothing is sent a second time.
	ha.Forget()
	ha.AnswerSwitch(500)
	st, out = pp.flip(agenttest.PlugBar, "ch1", "on", nil)
	if id, _, _, ok = asked(st, out); !ok {
		t.Fatalf("on: %d %v", st, out)
	}
	res = pp.approve(id)
	if res["ok"] != false || !strings.HasPrefix(fmt.Sprint(res["message"]), "Home Assistant refused the request, so Left monitor was most likely not switched.") ||
		!strings.Contains(fmt.Sprint(res["message"]), "The passkey approval is used up.") {
		t.Fatalf("a refused switch request: %v", res)
	}
	if got := pp.haPosts(); len(got) != 1 {
		t.Fatalf("Home Assistant got %d switch requests, want exactly one: %v", len(got), got)
	}
	ha.Honest()
	pp.m.clk.add(time.Minute)

	// 8. Home Assistant does not reach the socket: the agent's row says so, and the panel refuses before
	// anybody is asked for a passkey.
	ha.Set(agenttest.PlugBar2, "unavailable")
	pp.look()
	ha.Forget()
	if st, out := pp.flip(agenttest.PlugBar, "ch2", "off", nil); st != 409 || out["detail"] != "Home Assistant has no state for this socket right now. Nothing was switched." {
		t.Fatalf("a socket Home Assistant does not reach: %d %v", st, out)
	}
	// and what the machine's own config forbids is refused by the panel from the agent's row alone
	for _, c := range []struct{ plug, id, want string }{
		{agenttest.PlugBar, "ch3", "Not switchable. It is not in the allowed list on the machine that reads this plug. Nothing was switched."},
		{agenttest.PlugCorner, "main", "Locked. On PC-A this socket is marked as one that is never switched. Nothing was switched."},
		{agenttest.PlugOwn, "main", "Locked. This socket powers the machine that reads the plugs. Nothing was switched."},
	} {
		for _, to := range []string{"on", "off"} {
			if st, out := pp.flip(c.plug, c.id, to, nil); st != 409 || out["detail"] != c.want {
				t.Errorf("%s/%s %s: %d %v", c.plug, c.id, to, st, out)
			}
		}
	}
	if n := len(ha.Posts()); n != 0 {
		t.Fatalf("Home Assistant got %d requests that change something for refused switches", n)
	}

	// 9. Everything Home Assistant ever saw: only state reads and the two services, with the right token, and
	// never "toggle" (the fake's own checks).
	if p := ha.Problems(); p != "" {
		t.Errorf("the fake Home Assistant: %s", p)
	}
	// 10. The record: every attempt, with who and what; and no token anywhere.
	log, _ := os.ReadFile(pp.audit)
	for _, want := range []string{
		`"action":"plug.switch"`, `"target":"pc-a/desk-bar/ch2"`, `"outcome":"ok"`, `"cut":true`, `"linked":"pc-b"`,
		`refused (machine_running): PC-B is still running`, `refused (too_soon): Wait `, `failed (ha_refused): Home Assistant refused the request`,
		// (of the refusals the panel makes itself, the first ten of a minute are written for one admin; the last
		// ones of this test are past that, and are counted in the minute's "audit.suppressed" entry)
		`refused (approval_off): PC-A has no passkey check`, `refused (socket_unavailable)`, `refused (not_allowed)`,
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("the audit log lacks %s", want)
		}
	}
	pp.noLeak()
	if strings.Contains(string(log), hatest.Token()) {
		t.Error("the Home Assistant token is in the panel's audit log")
	}
	pp.mu.Lock()
	all := strings.Join(pp.bodies, "\n")
	pp.mu.Unlock()
	if strings.Contains(all, hatest.Token()) || strings.Contains(all, "switch.desk_bar") {
		t.Error("an answer to the browser has the Home Assistant token or an entity id")
	}
}

// The wait: the agent works on a switch for up to 18 s (a socket whose state never follows is watched for
// 10 s), and the panel's usual wait for an agent is request_timeout + 8 s, 9 s in this test. The agent here
// waits for real, its Home Assistant answers each state read a little late, and the socket does not follow:
// the agent's answer comes after more than 9 s. The panel still gets it (it waits 25 s for this one request),
// and says it as it is: sent, not confirmed. With the usual wait this was "no answer".
func TestE2EPlugSwitchThatIsNotConfirmed(t *testing.T) {
	pp := newPlugPanelWith(t, true)
	if res := pp.turnOn("pc-a", "pc-b"); res["ok"] != true {
		t.Fatalf("Turn on: %v", res)
	}
	pp.look()
	pp.m.ha.IgnoreSwitch(true) // Home Assistant takes the request and the socket never shows the new state
	pp.m.ha.SlowState(100 * time.Millisecond)
	st, out := pp.flip(agenttest.PlugBar, "ch2", "on", nil)
	id, _, _, ok := asked(st, out)
	if !ok {
		t.Fatalf("switch on: %d %v", st, out)
	}
	start := time.Now()
	res := pp.approve(id)
	took := time.Since(start)
	if res["ok"] != true || res["message"] != "Sent. ch2 does not show on yet." {
		t.Fatalf("result after %v: %v", took, res)
	}
	if usual := 9 * time.Second; took <= usual {
		t.Fatalf("the agent answered after %v: the test does not show that the panel waits longer than its usual %v", took, usual)
	}
	if got := pp.haPosts(); len(got) != 1 {
		t.Fatalf("Home Assistant got %d switch requests, want exactly one", len(got))
	}
	log, _ := os.ReadFile(pp.audit)
	if !strings.Contains(string(log), `"outcome":"ok (not confirmed)"`) || !strings.Contains(string(log), `"confirmed":false`) {
		t.Errorf("the audit entry of a switch that was not confirmed: %s", log)
	}
}
