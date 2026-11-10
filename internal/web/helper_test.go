package web

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

var helperAll = []string{"coretemp", "ipmi", "fans", "power"}

// helperPoll makes the agent report its helper in this state and polls once.
func (e *env) helperPoll(id, state string, features ...string) {
	e.b.Mu.Lock()
	e.b.Metrics[id] = func() (jsonx.M, error) {
		m := testutil.HelperMetrics(state, features...)
		m["hostname"] = id
		return m, nil
	}
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(context.Background())
}

func (e *env) agentRequests() string {
	e.b.Mu.Lock()
	defer e.b.Mu.Unlock()
	return strings.Join(e.b.Requests, "\n")
}

func (e *env) helperAudit(c string) []map[string]any {
	var out []map[string]any
	for _, x := range e.do("GET", "/api/audit?action=helper", nil, "Cookie", c).body["entries"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

// Stop / Start of the elevated helper: the same gate as a power action (admins only, CSRF, step-up, kill switch,
// typed name, rate limit), through the agent with its control token, audited as helper_stop / helper_start.
func TestHelperStopStartThroughTheAgent(t *testing.T) {
	e := setup(t)
	csrf := []string{"X-Requested-With", "fairy"}
	c := e.cookie(controller(), 0)
	ok := map[string]any{"confirm": "LAPTOP"}
	// a Linux agent (no helper block)
	if r := e.do("POST", "/api/systems/laptop/helper/stop", ok, append([]string{"Cookie", c}, csrf...)...); r.code != 409 ||
		!strings.Contains(r.raw2(), "no elevated helper") {
		t.Fatalf("no helper: %d %s", r.code, r.raw)
	}
	e.helperPoll("laptop", "running", helperAll...)
	// who may: not a viewer, not without the CSRF header, not with an old sign-in
	v := e.cookie(viewer, 0)
	if r := e.do("POST", "/api/systems/laptop/helper/stop", ok, append([]string{"Cookie", v}, csrf...)...); r.code != 403 {
		t.Fatalf("viewer: %d %s", r.code, r.raw)
	}
	if r := e.do("POST", "/api/systems/laptop/helper/stop", ok, "Cookie", c); r.code != 403 {
		t.Fatalf("csrf: %d %s", r.code, r.raw)
	}
	old := e.cookie(controller(), 10*time.Minute)
	r := e.do("POST", "/api/systems/laptop/helper/stop", ok, append([]string{"Cookie", old}, csrf...)...)
	if d, _ := r.body["detail"].(map[string]any); r.code != 401 || d["code"] != "step_up_required" {
		t.Fatalf("step-up: %d %s", r.code, r.raw)
	}
	_ = e.s.Poller.Admin.SetControlDisabled(true, "root (cli)", "maintenance")
	if r := e.do("POST", "/api/systems/laptop/helper/stop", ok, append([]string{"Cookie", c}, csrf...)...); r.code != 423 {
		t.Fatalf("kill switch: %d %s", r.code, r.raw)
	}
	_ = e.s.Poller.Admin.SetControlDisabled(false, "root", "")
	// the typed name
	for _, body := range []any{nil, map[string]any{}, map[string]any{"confirm": "laptops"}} {
		if r := e.do("POST", "/api/systems/laptop/helper/stop", body, append([]string{"Cookie", c}, csrf...)...); r.code != 400 ||
			!strings.Contains(r.raw2(), "type the host's name (laptop)") {
			t.Fatalf("typed name %v: %d %s", body, r.code, r.raw)
		}
	}
	if strings.Contains(e.agentRequests(), "/helper/") {
		t.Fatal("the agent was called before every check passed:", e.agentRequests())
	}
	// stop
	e.b.Reply = func(_, path string) (int, jsonx.M, error) {
		switch path {
		case "/helper/stop":
			return 200, jsonx.M{"state": "stopped"}, nil
		case "/helper/start":
			return 200, jsonx.M{"state": "starting"}, nil
		}
		return 0, nil, nil
	}
	r = e.do("POST", "/api/systems/laptop/helper/stop", ok, append([]string{"Cookie", c}, csrf...)...)
	if r.code != 200 || r.body["state"] != "stopped" || r.body["action"] != "stop" || !strings.Contains(r.raw2(), "BMC") {
		t.Fatalf("stop: %d %s", r.code, r.raw)
	}
	if reqs := e.agentRequests(); !strings.Contains(reqs, "laptop POST /helper/stop map[]") {
		t.Fatal("agent call:", reqs)
	}
	e.helperPoll("laptop", "stopped", helperAll...)
	r = e.do("POST", "/api/systems/laptop/helper/start", map[string]any{"confirm": " laptop "}, append([]string{"Cookie", c}, csrf...)...)
	if r.code != 200 || r.body["state"] != "starting" {
		t.Fatalf("start: %d %s", r.code, r.raw)
	}
	// audit: newest first, with the host and the user
	a := e.helperAudit(c)
	if len(a) < 2 {
		t.Fatal("audit:", a)
	}
	for i, want := range []struct{ action, state string }{{"helper_start", "starting"}, {"helper_stop", "stopped"}} {
		p, _ := a[i]["params"].(map[string]any)
		if a[i]["action"] != want.action || a[i]["target"] != "laptop" || a[i]["outcome"] != "ok" || a[i]["actor"] != "n@example.com (unverified)" ||
			a[i]["source"] != "ui" || p["host"] != "laptop" || p["state"] != want.state {
			t.Fatalf("audit %d: %v", i, a[i])
		}
	}
	// refused attempts are audited too (here: the typed name)
	found := false
	for _, x := range a {
		if x["action"] == "helper_stop" && strings.HasPrefix(fmt.Sprint(x["outcome"]), "error 400") {
			found = true
		}
	}
	if !found {
		t.Fatal("refused stop not audited:", a)
	}
	// the stop is a notification for everyone who sees the host
	if n := e.do("GET", "/api/notifications", nil, "Cookie", c); !strings.Contains(n.raw2(), "helper_stopped") {
		t.Fatal("notification:", n.raw2())
	}
}

func TestHelperAgentAnswers(t *testing.T) {
	e := setup(t)
	e.s.control = newRateLimiter(100)
	c := e.cookie(controller(), 0)
	hdr := []string{"Cookie", c, "X-Requested-With", "fairy"}
	e.helperPoll("laptop", "running", helperAll...)
	var reply struct {
		st   int
		body jsonx.M
		err  error
	}
	e.b.Reply = func(string, string) (int, jsonx.M, error) { return reply.st, reply.body, reply.err }
	for _, tc := range []struct {
		name   string
		st     int
		body   jsonx.M
		err    error
		want   int
		detail string
	}{
		{"not installed", 409, jsonx.M{"error": "not_installed"}, nil, 409, "not installed"},
		{"older agent", 404, jsonx.M{"error": "not found"}, nil, 409, "v0.5.0"},
		{"wrong control token", 401, nil, nil, 424, "control token"},
		{"agent failure", 500, jsonx.M{"error": "OpenService: access denied"}, nil, 424, "access denied"},
		{"unreachable", 0, nil, devices.Errf("connection refused"), 502, "agent unreachable"},
		{"odd state", 200, jsonx.M{"state": "<b>"}, nil, 200, ""},
	} {
		reply.st, reply.body, reply.err = tc.st, tc.body, tc.err
		r := e.do("POST", "/api/systems/laptop/helper/stop", map[string]any{"confirm": "laptop"}, hdr...)
		if r.code != tc.want || !strings.Contains(fmt.Sprint(r.body["detail"]), tc.detail) {
			t.Fatalf("%s: %d %s", tc.name, r.code, r.raw)
		}
		if tc.want == 200 && r.body["state"] != "unknown" {
			t.Fatalf("%s: %s", tc.name, r.raw)
		}
	}
	a := e.helperAudit(c)
	if len(a) != 6 || a[5]["outcome"] != "error 409: the elevated helper is not installed on this host" {
		t.Fatal("every answer is audited:", a)
	}
	// a helper the agent reports as not installed is refused without calling the agent
	e.b.Reply = nil
	m := testutil.HelperMetrics("running", helperAll...)
	m["helper"].(jsonx.M)["state"] = "not_installed"
	m["hostname"] = "laptop"
	e.b.Mu.Lock()
	e.b.Requests = nil
	e.b.Metrics["laptop"] = func() (jsonx.M, error) { return m, nil }
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(context.Background())
	if r := e.do("POST", "/api/systems/laptop/helper/start", map[string]any{"confirm": "laptop"}, hdr...); r.code != 409 ||
		strings.Contains(e.agentRequests(), "/helper/") {
		t.Fatalf("not installed: %d %s %s", r.code, r.raw, e.agentRequests())
	}
}

// Sites: a viewer cannot stop the helper of any host, their own included; an admin can stop any host's. A host
// without a control token cannot be driven at all.
func TestHelperPermissionsFollowPowerActions(t *testing.T) {
	e := setupSites(t)
	csrf := []string{"X-Requested-With", "fairy"}
	e.helperPoll("tower", "running", helperAll...)      // Bob's
	e.helperPoll("secret-box", "running", helperAll...) // admins only, and no control token
	for _, sub := range []string{subBob, subAlice, subPending} {
		c := e.cookie(person(sub), 0)
		for _, id := range []string{"tower", "secret-box", "laptop"} {
			r := e.do("POST", "/api/systems/"+id+"/helper/stop", map[string]any{"confirm": id}, append([]string{"Cookie", c}, csrf...)...)
			if r.code != 403 {
				t.Errorf("%s stopping %s: %d %s", sub, id, r.code, r.raw)
			}
			assertHidden(t, sub, "helper stop", r.raw2(), map[string][]string{subBob: hiddenFromBob, subAlice: hiddenFromAlice, subPending: hiddenFromAll}[sub])
		}
	}
	a := e.cookie(person(subAdmin), 0)
	if r := e.do("POST", "/api/systems/tower/helper/stop", map[string]any{"confirm": "tower"}, append([]string{"Cookie", a}, csrf...)...); r.code != 200 {
		t.Fatalf("admin: %d %s", r.code, r.raw)
	}
	if r := e.do("POST", "/api/systems/secret-box/helper/stop", map[string]any{"confirm": "secret-box"}, append([]string{"Cookie", a}, csrf...)...); r.code != 409 ||
		!strings.Contains(r.raw2(), "control_token_env") {
		t.Fatalf("no control token: %d %s", r.code, r.raw)
	}
	if r := e.do("POST", "/api/systems/nope/helper/stop", map[string]any{"confirm": "nope"}, append([]string{"Cookie", a}, csrf...)...); r.code != 404 {
		t.Fatalf("unknown host: %d %s", r.code, r.raw)
	}
	if r := e.do("POST", "/api/systems/qemu-100/helper/stop", map[string]any{"confirm": "win11"}, append([]string{"Cookie", a}, csrf...)...); r.code != 404 {
		t.Fatalf("a guest: %d %s", r.code, r.raw)
	}
	// viewers see the helper's state of their own hosts (the badge), never the other sites'
	s := e.cookie(person(subBob), 0)
	if r := e.do("GET", "/api/systems/tower", nil, "Cookie", s); r.code != 200 || r.body["helper"] == nil {
		t.Fatalf("Bob's own host: %d %s", r.code, r.raw)
	}
}

// While the helper is stopped the BMC is out of reach: the panel refuses fan writes itself (audited) and the
// fan status says why, without asking the agent.
func TestFansPausedWhileHelperStopped(t *testing.T) {
	e := setup(t)
	csrf := []string{"X-Requested-With", "fairy"}
	c := e.cookie(controller(), 0)
	e.helperPoll("tower", "running", helperAll...)
	if r := e.do("POST", "/api/systems/tower/fan/mode", map[string]any{"mode": "full"}, append([]string{"Cookie", c}, csrf...)...); r.code != 200 {
		t.Fatalf("running: %d %s", r.code, r.raw)
	}
	e.helperPoll("tower", "stopped", helperAll...)
	sys := e.do("GET", "/api/systems/tower", nil, "Cookie", c)
	h, _ := sys.body["helper"].(map[string]any)
	if sys.body["fan_control"] != true || fmt.Sprint(h["paused"]) != "[coretemp ipmi fans]" {
		t.Fatalf("stopped: %s", sys.raw)
	}
	e.b.Mu.Lock()
	e.b.Requests = nil
	e.b.Mu.Unlock()
	if r := e.do("POST", "/api/systems/tower/fan/mode", map[string]any{"mode": "full"}, append([]string{"Cookie", c}, csrf...)...); r.code != 409 ||
		!strings.Contains(r.raw2(), "helper is stopped") {
		t.Fatalf("fan write: %d %s", r.code, r.raw)
	}
	if r := e.do("GET", "/api/systems/tower/fan", nil, "Cookie", c); r.code != 200 || !strings.Contains(fmt.Sprint(r.body["error"]), "helper is stopped") {
		t.Fatalf("fan status: %d %s", r.code, r.raw)
	}
	if strings.Contains(e.agentRequests(), "/fan/") {
		t.Fatal("agent asked:", e.agentRequests())
	}
	r := e.do("GET", "/api/audit?action=fan", nil, "Cookie", c)
	if first := r.body["entries"].([]any)[0].(map[string]any); !strings.HasPrefix(fmt.Sprint(first["outcome"]), "error 409") {
		t.Fatal("refused fan write not audited:", first)
	}
}

// Power actions of a Windows host go through its helper: while it is stopped they are offered as unavailable
// ("start the helper first"), and an agent that refuses with 503 helper_stopped (stopped since the last poll)
// gets the same calm answer, never "agent error".
func TestPowerAndFansWaitForTheHelper(t *testing.T) {
	e := setup(t)
	e.s.control = newRateLimiter(100)
	c := e.cookie(controller(), 0)
	hdr := []string{"Cookie", c, "X-Requested-With", "fairy"}
	capOf := func(action string) map[string]any {
		for _, a := range e.do("GET", "/api/systems/laptop/power", nil, "Cookie", c).body["actions"].([]any) {
			if m := a.(map[string]any); m["action"] == action {
				return m
			}
		}
		return nil
	}
	e.helperPoll("laptop", "running", helperAll...)
	if a := capOf("shutdown"); a["available"] != true {
		t.Fatal("running:", a)
	}
	e.helperPoll("laptop", "stopped", helperAll...)
	for _, action := range []string{"shutdown", "reboot", "sleep"} {
		if a := capOf(action); a["available"] != false || !strings.Contains(fmt.Sprint(a["reason"]), "start the helper first") {
			t.Fatal("stopped:", action, a)
		}
	}
	r := e.do("POST", "/api/systems/laptop/power", map[string]any{"action": "reboot", "delay_s": 60, "confirm": "laptop"}, hdr...)
	if r.code != 409 || !strings.Contains(r.raw2(), "start the helper first") || strings.Contains(e.agentRequests(), "/power") {
		t.Fatalf("power while stopped: %d %s", r.code, r.raw)
	}
	e.helperPoll("laptop", "error", helperAll...)
	if a := capOf("shutdown"); a["available"] != false || !strings.Contains(fmt.Sprint(a["reason"]), "not answering") {
		t.Fatal("error:", a)
	}
	// the helper stops between the poll and the click: the agent answers 503 helper_stopped
	e.helperPoll("laptop", "running", helperAll...)
	e.helperPoll("tower", "running", helperAll...)
	e.b.Reply = func(_, path string) (int, jsonx.M, error) {
		if path == "/power" || strings.HasPrefix(path, "/fan/") && path != "/fan/status" {
			return 503, jsonx.M{"error": "helper stopped: start fairy-agent-helper", "code": "helper_stopped"}, nil
		}
		if path == "/fan/status" {
			return 200, jsonx.M{"enabled": true, "vendor": "supermicro", "mode": nil, "error": nil, "duty": jsonx.M{}, "fans": []any{},
				"events": []any{}, "available": false, "reason": "helper_stopped"}, nil
		}
		return 0, nil, nil
	}
	if r := e.do("POST", "/api/systems/laptop/power", map[string]any{"action": "reboot", "delay_s": 60, "confirm": "laptop"}, hdr...); r.code != 409 ||
		!strings.Contains(r.raw2(), "start the helper first") {
		t.Fatalf("power 503: %d %s", r.code, r.raw)
	}
	if r := e.do("POST", "/api/systems/tower/fan/mode", map[string]any{"mode": "full"}, hdr...); r.code != 409 || !strings.Contains(r.raw2(), "helper is stopped") {
		t.Fatalf("fan 503: %d %s", r.code, r.raw)
	}
	if r := e.do("GET", "/api/systems/tower/fan", nil, hdr...); r.code != 200 || r.body["available"] != false || r.body["reason"] != "helper_stopped" {
		t.Fatalf("fan status: %d %s", r.code, r.raw)
	}
}
