package web

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// POST /api/power/socket (PLUGS-DESIGN.md 4.4 to 4.8, the test plan's 7.3). The machines are
// testutil.Machine with the agent's rules for a socket and Fairy-agent's real gate: what "the machine got" is
// counted, and a switch request that got past the gate carried an approval the gate accepted.

// plugActor is how the panel names the admin of these tests (an address Pocket ID did not verify).
const plugActor = "n@example.com (unverified)"

// One switch off and one switch on, end to end: each needs its own passkey, each is one request for one
// socket in one direction, and the request names ids and a tag, never an entity, an address or a service.
func TestPlugSwitchOffAndOn(t *testing.T) {
	pe := newPlugEnv(t)
	logs := captureLog(t)
	var answers []string
	// an admin who signed in this second is asked all the same: the panel's own window never switches a socket
	fresh := pe.cookie(controller(), 0)
	r := pe.do("POST", "/api/power/socket", plugBody("reader/desk-bar", "ch1", "off"), "Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json")
	if _, _, ok := approvalAsked(r); !ok {
		t.Fatalf("inside the panel's own window: %d %s", r.code, r.raw)
	}
	for i, c := range []struct{ to, was, said string }{{"off", "on", "Left monitor is off."}, {"on", "off", "Left monitor is on."}} {
		r := pe.flip("reader/desk-bar", "ch1", c.to)
		answers = append(answers, string(r.raw))
		id, machines, ok := approvalAsked(r)
		if !ok || len(machines) != 1 || machines[0] != "READER" {
			t.Fatalf("%s: the dialog: %d %s", c.to, r.code, r.raw)
		}
		d := r.body["detail"].(map[string]any)
		if want := "switch " + c.to + " Left monitor (Monitor powerbar)"; d["signs"] != want || d["action"] != "switch a plug socket ("+want+")" {
			t.Fatalf("%s: the dialog says: signs %q, action %q", c.to, d["signs"], d["action"])
		}
		if n := len(plugSent(pe.reader)); n != i {
			t.Fatalf("%s: before the approval the machine got %d switch requests, want %d", c.to, n, i)
		}
		if cb := pe.approve(t, id); cb.code != 303 {
			t.Fatalf("%s: callback: %d %s", c.to, cb.code, cb.raw)
		}
		sent := plugSent(pe.reader)
		if len(sent) != i+1 {
			t.Fatalf("%s: the machine got %d switch requests, want %d", c.to, len(sent), i+1)
		}
		got := sent[i]
		// the request: the plug by its id ON THE AGENT, the socket, the direction in a word, the tag, who asked
		want := map[string]bool{"source": true, "socket": true, "to": true, "tag": true, "requested_by": true, "approval": true}
		for k := range got.Body {
			if !want[k] {
				t.Errorf("%s: the request has a member %q", c.to, k)
			}
		}
		if got.Body["source"] != "desk-bar" || got.Body["socket"] != "ch1" || got.Body["to"] != c.to || got.Body["tag"] != plugTag("desk-bar", "ch1") ||
			got.Body["requested_by"] != plugActor || !got.Approved {
			t.Fatalf("%s: the request: %+v", c.to, got)
		}
		if got.Wait != plugSwitchWait || plugSwitchWait.Seconds() != 25 {
			t.Errorf("%s: the panel waits %v for the answer, want 25 s", c.to, got.Wait)
		}
		// one approval is one socket and one direction: a list of exactly one entry, with the exact text
		entries := got.Body["approval"].(map[string]any)["entries"].([]any)
		if len(entries) != 1 || entries[0].(map[string]any)["request"] != "socket_set source=desk-bar socket=ch1 to="+c.to+" tag="+plugTag("desk-bar", "ch1") {
			t.Fatalf("%s: the approval's entries: %v", c.to, entries)
		}
		pr := pe.result(t)
		answers = append(answers, fmt.Sprint(pr))
		if pr["ok"] != true || pr["status"] != 200.0 || pr["message"] != c.said {
			t.Fatalf("%s: result: %v", c.to, pr)
		}
		au := pe.plugAudits()[0]
		p, _ := au["params"].(map[string]any)
		if au["action"] != "plug.switch" || au["target"] != "reader/desk-bar/ch1" || au["outcome"] != "ok" || au["actor"] != plugActor ||
			p["to"] != c.to || p["was"] != c.was || p["confirmed"] != true || p["approval"] != "sent" || p["name"] != "Left monitor" ||
			p["plug"] != "Monitor powerbar" || p["agent"] != "reader" || p["cut"] != nil || p["linked"] != nil {
			t.Fatalf("%s: the audit entry: %v", c.to, au)
		}
	}
	if len(pe.iss.Tokens()) != 2 {
		t.Errorf("%d passkey approvals for two switches", len(pe.iss.Tokens()))
	}
	// nothing but the two words reaches a machine as the direction, and no machine was asked about another socket
	for _, s := range pe.reader.Asked() {
		raw := fmt.Sprint(s.Body)
		if strings.Contains(raw, "toggle") || strings.Contains(raw, "switch.") || strings.Contains(raw, "entity") || strings.Contains(raw, "http") {
			t.Errorf("a request to the machine names more than ids: %s %v", s.Path, s.Body)
		}
	}
	if len(pe.other.Asked())+len(pe.third.Asked()) != 0 {
		t.Error("another machine was asked something")
	}
	// the notification, and no token anywhere
	notes := pe.do("GET", "/api/notifications", nil, "Cookie", pe.admin)
	if !strings.Contains(string(notes.raw), "Left monitor (Monitor powerbar) switched off by "+plugActor) {
		t.Errorf("no notification of the switch: %s", notes.raw)
	}
	audit, _ := os.ReadFile(pe.s.Cfg.AuditLog)
	pe.noLeak(t, "an answer to the browser", strings.Join(answers, "\n"))
	pe.noLeak(t, "the log", logs.take())
	pe.noLeak(t, "the audit log", string(audit))
}

// Who may switch: an admin in a browser. Not a viewer, not an API token of any scope, nobody while the kill
// switch is on, and no request without the page's header. None of them reaches a machine.
func TestPlugSwitchIsForAdminsInABrowser(t *testing.T) {
	pe := newPlugEnv(t)
	body := plugBody("reader/desk-bar", "ch1", "off")
	hdr := []string{"X-Requested-With", "fairy", "Content-Type", "application/json"}
	if r := pe.do("POST", "/api/power/socket", body, hdr...); r.code != 401 {
		t.Errorf("not signed in: %d", r.code)
	}
	if r := pe.do("POST", "/api/power/socket", body, append([]string{"Cookie", pe.viewerOf("Home")}, hdr...)...); r.code != 403 {
		t.Errorf("a viewer: %d %s", r.code, r.raw)
	}
	for _, scope := range []string{"read", "control"} {
		tok, _, _ := pe.s.Tokens.Create("t-"+scope, "system", scope, "cli:test", 0)
		if r := pe.do("POST", "/api/power/socket", body, "Authorization", "Bearer "+tok, "Content-Type", "application/json"); r.code != 403 {
			t.Errorf("a %s token: %d %s", scope, r.code, r.raw)
		}
	}
	if !sessionOnlyControl["POST /api/power/socket"] {
		t.Error("the route is not in the list of routes no API token may use")
	}
	if r := pe.do("POST", "/api/power/socket", body, "Cookie", pe.admin, "Content-Type", "application/json"); r.code != 403 {
		t.Errorf("without the page's header: %d", r.code)
	}
	if err := pe.s.Poller.Admin.SetControlDisabled(true, "test", "kill switch"); err != nil {
		t.Fatal(err)
	}
	for _, to := range []string{"off", "on"} {
		if r := pe.flip("reader/desk-bar", "ch1", to); r.code != 423 {
			t.Errorf("%s with the kill switch on: %d %s", to, r.code, r.raw)
		}
	}
	if n := len(pe.reader.Asked()); n != 0 {
		t.Errorf("the machine was asked %d things", n)
	}
	if n := len(pe.plugAudits()); n != 0 {
		t.Errorf("%d plug.switch entries for requests that were refused before the body was read", n)
	}
}

// A body that is not one socket in one named direction is refused before anything is looked up: no toggle,
// no list, no entity, no member the request does not have.
func TestPlugSwitchBody(t *testing.T) {
	pe := newPlugEnv(t)
	good := plugBody("reader/desk-bar", "ch1", "off")
	with := func(k string, v any) map[string]any {
		b := map[string]any{}
		for kk, vv := range good {
			b[kk] = vv
		}
		if v == nil {
			delete(b, k)
		} else {
			b[k] = v
		}
		return b
	}
	for name, b := range map[string]map[string]any{
		"toggle":              with("to", "toggle"),
		"no direction":        with("to", nil),
		"a direction in caps": with("to", "OFF"),
		"true for on":         with("to", true),
		"two sockets":         with("socket", []string{"ch1", "ch2"}),
		"a socket id of 17":   with("socket", "this-is-seventeen"),
		"no socket":           with("socket", nil),
		"no source":           with("source", nil),
		"a tag of 21":         with("tag", plugTag("desk-bar", "ch1")[:21]),
		"no tag":              with("tag", nil),
		"an entity":           with("entity", "switch.example_desk_bar_ch1"),
		"a service":           with("service", "turn_off"),
		"an approval":         with("approval", map[string]any{"token": "x", "entries": []any{}}),
		"cut for on":          func() map[string]any { b := with("to", "on"); b["cut"] = true; return b }(),
		"a long confirmation": with("confirm", strings.Repeat("x", 201)),
	} {
		if r := pe.post("/api/power/socket", b); r.code != 422 {
			t.Errorf("%s: %d %s", name, r.code, r.raw)
		}
	}
	if r := pe.do("POST", "/api/power/socket", nil, pe.csrf...); r.code != 422 && r.code != 400 {
		t.Errorf("no body: %d", r.code)
	}
	if n := len(pe.reader.Asked()); n != 0 {
		t.Errorf("the machine was asked %d things", n)
	}
}

// What the panel itself refuses, with the socket as it sees it now. Nothing of it reaches the machine, not
// even the question about an approval, and every refusal is in the audit log.
func TestPlugSwitchRefusedByThePanel(t *testing.T) {
	pe := newPlugEnv(t)
	type want struct {
		source, socket string
		code           int
		word, said     string
	}
	cases := []want{
		{"reader/desk-bar", "ch9", 404, "", "there is no such socket"},
		{"reader/no-bar", "ch1", 404, "", "there is no such socket"},
		{"nobody/desk-bar", "ch1", 404, "", "there is no such socket"},
		// the agent's own config does not let it be switched
		{"reader/desk-bar", "ch8", 409, api.WhyNotAllowed, "Not switchable. It is not in the allowed list on the machine that reads this plug. Nothing was switched."},
		// the panel's locks, by what the socket is linked to: the node the reading agent runs on ...
		{"reader/desk-bar", "ch4", 409, model.SocketLockAgent, "Locked. This socket powers the machine that reads the plugs. Nothing was switched."},
		// ... the agent's own plug (its only socket takes the plug's link, here system_hint: self) ...
		{"reader/own-plug", "main", 409, model.SocketLockAgent, "Locked. This socket powers the machine that reads the plugs. Nothing was switched."},
		// ... the VM Home Assistant runs on, and for another agent the node of that VM ...
		{"reader/desk-bar", "ch5", 409, model.SocketLockHomeAssistant, "Locked. This socket powers Home Assistant. Nothing was switched."},
		{"other/rack-bar", "node", 409, model.SocketLockHomeAssistant, "Locked. This socket powers Home Assistant. Nothing was switched."},
		// ... the machine the panel runs on ...
		{"reader/desk-bar", "ch6", 409, model.SocketLockPanel, "Locked. This socket powers the panel. Nothing was switched."},
		// ... and a socket that is linked to a machine while home_assistant_system names no row
		{"third/spare-bar", "pc", 409, model.SocketLockHAUnknown, "Locked. The panel cannot tell which machine Home Assistant runs on (home_assistant_system names no row). Nothing was switched."},
		// the panel has no control token for the agent
		{"bare/bare-bar", "lamp", 409, model.SocketNoControl, "The panel has no control token for BARE. Nothing was switched."},
	}
	for _, c := range cases {
		for _, to := range []string{"off", "on"} { // a lock holds in both directions
			r := pe.flip(c.source, c.socket, to)
			msg, _ := plugDetailOf(r)
			if r.code != c.code || msg != c.said {
				t.Errorf("%s/%s %s: %d %q, want %d %q", c.source, c.socket, to, r.code, msg, c.code, c.said)
			}
			au := pe.plugAudits()[0]
			wantOut := "error 404: there is no such socket"
			if c.word != "" {
				wantOut = "refused (" + c.word + "): " + c.said
			}
			if au["target"] != c.source+"/"+c.socket || au["outcome"] != wantOut || au["params"].(map[string]any)["to"] != to {
				t.Errorf("%s/%s %s: the audit entry: %v", c.source, c.socket, to, au)
			}
		}
	}
	// a socket that is linked to nothing is not locked by a home_assistant_system that names no row
	if _, _, ok := approvalAsked(pe.flip("third/spare-bar", "lamp", "on")); !ok {
		t.Error("a socket that feeds no machine is refused because of home_assistant_system")
	}
	// the tag the page showed is not the socket's any more
	b := plugBody("reader/desk-bar", "ch1", "off")
	b["tag"] = plugTag("desk-bar", "ch2")
	if r := pe.post("/api/power/socket", b); r.code != 409 || r.body["detail"] != "This socket was changed on the machine. Nothing was switched. Look again." {
		t.Errorf("another tag: %d %s", r.code, r.raw)
	}
	// Home Assistant has no state for it
	for _, state := range []string{"unavailable", "unknown"} {
		pe.set("reader/desk-bar/ch1", "state", state)
		if r := pe.flip("reader/desk-bar", "ch1", "off"); r.code != 409 || r.body["detail"] != "Home Assistant has no state for this socket right now. Nothing was switched." {
			t.Errorf("state %s: %d %s", state, r.code, r.raw)
		}
	}
	if asked := len(pe.reader.Asked()) + len(pe.other.Asked()); asked != 0 {
		t.Errorf("the machines were asked %d things by requests the panel refuses itself", asked)
	}
	// the agent does not answer: its sockets have no state and cannot be switched
	pe.b.Mu.Lock()
	pe.b.Metrics["other"] = func() (jsonx.M, error) { return nil, errors.New("connection failed") }
	pe.b.Mu.Unlock()
	pe.poll()
	if r := pe.flip("other/rack-bar", "fan", "on"); r.code != 409 || r.body["detail"] != "OTHER does not answer. Nothing was switched." {
		t.Errorf("an agent that does not answer: %d %s", r.code, r.raw)
	}
}

// A machine without the passkey check is sent nothing: not the switch, and not the question either. The
// panel's own record decides that, whatever the agent's row says about the socket.
func TestPlugSwitchNeedsTheMachinesOwnCheck(t *testing.T) {
	pe := newPlugEnv(t)
	// "bare" has no trust file; give the panel a control token for it by making its sockets the reader's kind:
	// the reader's check is taken away on the machine instead (its trust file is removed)
	if err := os.Remove(pe.reader.TrustPath); err != nil {
		t.Fatal(err)
	}
	pe.reader.Gate.StatusNow()
	pe.poll()
	n := len(pe.reader.Asked())
	for _, to := range []string{"on", "off"} {
		r := pe.flip("reader/desk-bar", "ch2", to)
		msg, _ := plugDetailOf(r)
		if r.code != 409 || !strings.Contains(msg, "Nothing was switched") || strings.Contains(msg, "Confirm") {
			t.Errorf("%s on a machine whose check is gone: %d %s", to, r.code, r.raw)
		}
	}
	if got := len(pe.reader.Asked()); got != n {
		t.Errorf("a machine without the check was asked %d things", got-n)
	}
	// and a machine that says, to the question, that a switch needs no approval is not believed
	pe2 := newPlugEnv(t)
	for _, reason := range []string{"off", "fan_window", "boot_shutdown", ""} {
		pe2.gb.Lie("reader", func(path string) (int, jsonx.M) {
			if path != api.PathApprovalChallenge {
				return 0, nil
			}
			return 200, jsonx.M{"needed": false, "reason": reason,
				"request": "socket_set source=desk-bar socket=ch2 to=on tag=" + plugTag("desk-bar", "ch2")}
		})
		r := pe2.flip("reader/desk-bar", "ch2", "on")
		msg, _ := plugDetailOf(r)
		if r.code != 409 || !strings.Contains(msg, "a socket is switched only with a passkey approval. Nothing was sent.") {
			t.Errorf("\"needs no approval (%s)\": %d %s", reason, r.code, r.raw)
		}
		if n := len(plugSent(pe2.reader)); n != 0 {
			t.Errorf("\"needs no approval (%s)\": the machine got %d switch requests", reason, n)
		}
		if out := fmt.Sprint(pe2.plugAudits()[0]["outcome"]); !strings.HasPrefix(out, "error 409: READER answered that switching a socket needs no approval") {
			t.Errorf("\"needs no approval (%s)\": audit %q", reason, out)
		}
	}
	// also inside the panel's own window, where a fan change would go on without an approval
	fresh := pe2.cookie(controller(), 0)
	r := pe2.do("POST", "/api/power/socket", plugBody("reader/desk-bar", "ch2", "on"), "Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json")
	if r.code != 409 || len(plugSent(pe2.reader)) != 0 {
		t.Errorf("inside the panel's window: %d %s", r.code, r.raw)
	}
}

// "Still running": a socket whose machine runs is not switched off. The refusal names the machine and has a
// code, so the page can offer "Shut down" and "Cut power anyway". With cut and the machine's name typed the
// request goes on to the passkey like every other switch.
func TestPlugSwitchOffAMachineThatRuns(t *testing.T) {
	pe := newPlugEnv(t)
	running := func(r resp, machine, system, said string) {
		t.Helper()
		d, _ := r.body["detail"].(map[string]any)
		state := "running" // a link to an id no row has: the panel cannot tell, and says so
		if system == "" {
			state = "unknown"
		}
		if r.code != 409 || d["code"] != "machine_running" || d["machine"] != machine || d["message"] != said || (system != "" && d["system_id"] != system) ||
			(system == "" && d["system_id"] != nil) || d["state"] != state {
			t.Fatalf("%d %s\nwant machine_running for %s: %q", r.code, r.raw, machine, said)
		}
	}
	// by socket_links, and by the plug's own link for its only socket
	running(pe.flip("reader/desk-bar", "ch3", "off"), "DESK-PC", "desk-pc", "DESK-PC is still running. Shut it down first.")
	running(pe.flip("reader/pc-plug", "main", "off"), "DESK-PC", "desk-pc", "DESK-PC is still running. Shut it down first.")
	if au := pe.plugAudits()[0]; au["outcome"] != "refused (machine_running): DESK-PC is still running. Shut it down first." || au["params"].(map[string]any)["linked"] != "desk-pc" {
		t.Errorf("the audit entry of the refusal: %v", au)
	}
	// a link to an id no row has: not known to be off, named by the id
	running(pe.flip("reader/desk-bar", "ch7", "off"), "old-pc", "", "This socket is linked to old-pc, and the panel has no such row right now, so it cannot tell whether that machine is off.")
	// switching ON has no such rule
	if _, _, ok := approvalAsked(pe.flip("reader/desk-bar", "ch3", "on")); !ok {
		t.Error("on is refused for a socket whose machine runs")
	}
	if n := len(pe.reader.Asked()); n != 1 || pe.reader.Asked()[0].Path != api.PathApprovalChallenge {
		t.Errorf("the machine was asked %d things, want only the one question for \"on\"", n)
	}

	// cut power anyway: the machine's name, typed
	cut := func(confirm string) resp {
		b := plugBody("reader/desk-bar", "ch3", "off")
		b["cut"], b["confirm"] = true, confirm
		return pe.post("/api/power/socket", b)
	}
	for _, bad := range []string{"", "DESK", "desk-pc-2", "READER", "Left monitor"} {
		if r := cut(bad); r.code != 422 || r.body["detail"] != "Type DESK-PC to confirm cutting its power. Nothing was switched." {
			t.Errorf("cut with %q typed: %d %s", bad, r.code, r.raw)
		}
	}
	if n := len(plugSent(pe.reader)); n != 0 {
		t.Fatalf("the machine got %d switch requests", n)
	}
	r := cut(" desk-pc ") // as the other typed names: letter case and spaces around it do not count
	id, _, ok := approvalAsked(r)
	if !ok {
		t.Fatalf("cut with the name typed: %d %s", r.code, r.raw)
	}
	if d := r.body["detail"].(map[string]any); d["signs"] != "switch off Desk PC (Monitor powerbar): cut the power of DESK-PC while it is running" {
		t.Errorf("the dialog of a cut says: %q", d["signs"])
	}
	if cb := pe.approve(t, id); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if pr := pe.result(t); pr["ok"] != true || pr["message"] != "Desk PC is off." {
		t.Fatalf("result: %v", pr)
	}
	sent := plugSent(pe.reader)
	if len(sent) != 1 || sent[0].Body["cut"] != nil || sent[0].Body["confirm"] != nil || sent[0].Body["to"] != "off" || sent[0].Body["socket"] != "ch3" {
		t.Fatalf("what the machine got: %+v", sent)
	}
	au := pe.plugAudits()[0]
	if p := au["params"].(map[string]any); au["outcome"] != "ok" || p["cut"] != true || p["linked"] != "desk-pc" || p["approval"] != "sent" {
		t.Errorf("the audit entry of a cut: %v", au)
	}
	if notes := pe.do("GET", "/api/notifications", nil, "Cookie", pe.admin); !strings.Contains(string(notes.raw), "power cut while DESK-PC was running") {
		t.Errorf("the notification of a cut: %s", notes.raw)
	}

	// a machine that has not answered for a moment ("stale") is not off: it still counts as running
	pe.pcIs("down")
	if x := pe.do("GET", "/api/systems/desk-pc", nil, "Cookie", pe.admin); x.body["status"] != "stale" {
		t.Fatalf("the fixture: the PC is %v, want stale", x.body["status"])
	}
	running(pe.flip("reader/desk-bar", "ch3", "off"), "DESK-PC", "desk-pc", "DESK-PC is still running. Shut it down first.")
	// a machine that is off: no question, and the dialog says what the socket powers
	pe.pcOff()
	r = pe.flip("reader/desk-bar", "ch3", "off")
	if _, _, ok := approvalAsked(r); !ok || r.body["detail"].(map[string]any)["signs"] != "switch off Desk PC (Monitor powerbar), which powers DESK-PC" {
		t.Fatalf("off for a socket whose machine is off: %d %s", r.code, r.raw)
	}
	// "cut" for a socket that nothing hangs on is refused: there is no machine to name
	b := plugBody("reader/desk-bar", "ch1", "off")
	b["cut"], b["confirm"] = true, "Left monitor"
	if r := pe.post("/api/power/socket", b); r.code != 422 || !strings.Contains(fmt.Sprint(r.body["detail"]), "there is no machine whose power would be cut") {
		t.Errorf("cut for a socket that feeds nothing: %d %s", r.code, r.raw)
	}
}

// "Stale" is not off: a machine the panel has not heard from for a moment counts as running. Only "offline"
// and "stopped" let a switch-off through without the typed name; a machine that sleeps counts as running.
func TestPlugRunningRule(t *testing.T) {
	for _, c := range []struct {
		status  string
		asleep  bool
		running bool
	}{{"online", false, true}, {"stale", false, true}, {"paused", false, true}, {"error", false, true},
		{"offline", false, false}, {"stopped", false, false}, {"offline", true, true}} {
		row := &model.System{ID: "desk-pc", Name: "DESK-PC", Kind: "agent", Status: c.status, Asleep: c.asleep}
		k := socketLook{link: "desk-pc", shown: "desk-pc", row: row}
		if name, yes := k.running(); yes != c.running || name != "DESK-PC" {
			t.Errorf("status %s (asleep %v): running %v, want %v", c.status, c.asleep, yes, c.running)
		}
		// which kind of "not off" the page's dialog names
		want := map[bool]string{false: "", true: map[bool]string{false: "running", true: "asleep"}[c.asleep]}[c.running]
		if got := k.runningState(); got != want {
			t.Errorf("status %s (asleep %v): state %q, want %q", c.status, c.asleep, got, want)
		}
	}
	if got := (socketLook{link: "old-pc", shown: "old-pc"}).runningState(); got != "unknown" {
		t.Errorf("a link to an id without a row: state %q", got)
	}
	if got := (socketLook{}).runningState(); got != "" {
		t.Errorf("a socket that is linked to nothing: state %q", got)
	}
	if name, yes := (socketLook{}).running(); yes || name != "" {
		t.Error("a socket that is linked to nothing has a machine that runs")
	}
	if name, yes := (socketLook{link: "old-pc", shown: "old-pc"}).running(); !yes || name != "old-pc" {
		t.Error("a link to an id without a row counts as off")
	}
	// a Proxmox node: the sentence says how many of its guests run
	node := model.System{ID: "lab-node-pve", Name: "PVE", Kind: "node", Status: "online", ClusterID: model.P("lab")}
	guest := func(id, status string) model.System {
		return model.System{ID: id, Kind: "qemu", Status: status, ParentID: model.P("lab-node-pve"), ClusterID: model.P("lab")}
	}
	for n, want := range map[int]string{0: "PVE is still running. Shut it down first.", 1: "PVE is still running, with 1 guest running on it. Shut it down first.",
		3: "PVE is still running, with 3 guests running on it. Shut it down first."} {
		snap := []model.System{node, guest("lab-qemu-900", "stopped"), {ID: "other-qemu-1", Kind: "qemu", Status: "online", ParentID: model.P("other-node"), ClusterID: model.P("lab")}}
		for i := range n {
			snap = append(snap, guest(fmt.Sprintf("lab-qemu-%d", 100+i), "online"))
		}
		l := &plugLooks{snap: snap}
		if got := l.runningWords(socketLook{link: node.ID, shown: node.ID, row: &snap[0]}); got != want {
			t.Errorf("%d guests: %q", n, got)
		}
	}
	asleep := &model.System{ID: "desk-pc", Name: "DESK-PC", Kind: "agent", Status: "offline", Asleep: true}
	if got := (&plugLooks{}).runningWords(socketLook{link: "desk-pc", shown: "desk-pc", row: asleep}); got != "DESK-PC is asleep, not shut down: what is open on it is lost when its power is cut. Shut it down first." {
		t.Errorf("a machine that sleeps: %q", got)
	}
}

// Everything the panel looks at before the approval is looked at again after it, with what is true then: the
// person was at Pocket ID meanwhile. The machine is then sent nothing, and the approval is not used.
func TestPlugSwitchLooksAgainAfterTheApproval(t *testing.T) {
	for name, c := range map[string]struct {
		socket string
		change func(pe *plugEnv)
		said   string
	}{
		"the PC was started": {"ch3", func(pe *plugEnv) { pe.pcIs("online") }, "DESK-PC is still running. Shut it down first."},
		"the socket was changed on the machine": {"ch1", func(pe *plugEnv) { pe.set("reader/desk-bar/ch1", "tag", plugTag("desk-bar", "other")) },
			"This socket was changed on the machine. Nothing was switched. Look again."},
		"the machine's config no longer allows it": {"ch1", func(pe *plugEnv) { pe.set("reader/desk-bar/ch1", "why", api.WhyNotAllowed) },
			"Not switchable. It is not in the allowed list on the machine that reads this plug. Nothing was switched."},
		"the socket is gone": {"ch1", func(pe *plugEnv) { pe.set("reader/desk-bar/ch1", "gone", "") }, "there is no such socket"},
		"Home Assistant lost the socket": {"ch1", func(pe *plugEnv) { pe.set("reader/desk-bar/ch1", "state", "unavailable") },
			"Home Assistant has no state for this socket right now. Nothing was switched."},
		"the kill switch was turned on": {"ch1", func(pe *plugEnv) { _ = pe.s.Poller.Admin.SetControlDisabled(true, "test", "stop") }, "control is disabled by an administrator (test): stop"},
	} {
		t.Run(name, func(t *testing.T) {
			pe := newPlugEnv(t)
			pe.pcOff()
			r := pe.flip("reader/desk-bar", c.socket, "off")
			id, _, ok := approvalAsked(r)
			if !ok {
				t.Fatalf("the first request: %d %s", r.code, r.raw)
			}
			c.change(pe)
			if cb := pe.approve(t, id); cb.code != 303 {
				t.Fatal(cb.code)
			}
			pr := pe.result(t)
			if pr["ok"] != false || pr["message"] != c.said {
				t.Fatalf("result: %v\nwant %q", pr, c.said)
			}
			if n := len(plugSent(pe.reader)); n != 0 {
				t.Fatalf("the machine got %d switch requests", n)
			}
		})
	}
}

// The approval is handed out only for exactly the request that was approved: another socket or another
// direction in the replay gets none, and nothing is sent.
func TestPlugApprovalIsForTheExactRequest(t *testing.T) {
	for name, edit := range map[string]func(body string) string{
		"another direction": func(b string) string { return strings.Replace(b, `"to":"on"`, `"to":"off"`, 1) },
		"another socket": func(b string) string {
			b = strings.Replace(b, `"socket":"ch2"`, `"socket":"ch1"`, 1)
			return strings.Replace(b, plugTag("desk-bar", "ch2"), plugTag("desk-bar", "ch1"), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			pe := newPlugEnv(t)
			id, _, ok := approvalAsked(pe.flip("reader/desk-bar", "ch2", "on"))
			if !ok {
				t.Fatal("not asked")
			}
			l := pe.s.codec.live
			l.mu.Lock()
			p := l.m[sidOf(pe.env, pe.admin)].pending
			changed := edit(p.Body)
			if changed == p.Body {
				l.mu.Unlock()
				t.Fatal("the test did not change the kept body:", p.Body)
			}
			p.Body = changed
			l.mu.Unlock()
			if cb := pe.approve(t, id); cb.code != 303 {
				t.Fatalf("callback: %d %s", cb.code, cb.raw)
			}
			pr := pe.result(t)
			if pr["ok"] != false || !strings.Contains(fmt.Sprint(pr["message"]), "not the request that was approved") {
				t.Fatalf("result: %v", pr)
			}
			if n := len(plugSent(pe.reader)); n != 0 {
				t.Fatalf("the machine got %d switch requests", n)
			}
			if out := fmt.Sprint(pe.plugAudits()[0]["outcome"]); !strings.Contains(out, "not the request that was approved") {
				t.Errorf("audit: %q", out)
			}
		})
	}
}

// What the machine answers to the question about an approval, before anybody is sent to Pocket ID: its own
// reasons not to switch are said in the panel's words, and nobody is asked for a passkey for nothing.
func TestPlugChallengeRefusals(t *testing.T) {
	pe := newPlugEnv(t)
	for _, c := range []struct {
		name   string
		make   func()
		status int
		said   string
	}{
		{"switching was turned off on the machine", func() { pe.reader.Plugs.Off = true }, 409, "Switching is turned off on READER. Nothing was switched."},
		{"the socket is in feeds_self there", func() {
			pe.reader.Plugs.Off = false
			pe.reader.Plugs.Sockets["desk-bar/ch2"].Refuse = api.CodeFeedsSelf
		}, 409,
			"Locked. On READER this socket is marked as one that is never switched. Nothing was switched."},
		{"the socket is not allowed there", func() { pe.reader.Plugs.Sockets["desk-bar/ch2"].Refuse = api.CodeNotAllowed }, 409,
			"Not switchable. It is not in the allowed list on the machine that reads this plug. Nothing was switched."},
		{"there is no token to switch with", func() { pe.reader.Plugs.Sockets["desk-bar/ch2"].Refuse = api.CodeNoSwitchToken }, 409,
			"READER has no Home Assistant token to switch with. Nothing was switched. (READER says: the machine's own sentence for no_token)"},
		{"the tag is another one on the machine", func() {
			pe.reader.Plugs.Sockets["desk-bar/ch2"].Refuse = ""
			pe.reader.Plugs.Sockets["desk-bar/ch2"].Tag = plugTag("desk-bar", "other")
		}, 409, "This socket was changed on the machine. Nothing was switched. Look again."},
		{"the machine has no such socket", func() { delete(pe.reader.Plugs.Sockets, "desk-bar/ch2") }, 409, "READER has no such socket any more. Nothing was switched. Look again."},
		{"the wait after a refused token is on", func() {
			pe.reader.Plugs.Sockets["desk-bar/ch2"] = &testutil.MachineSocket{Tag: plugTag("desk-bar", "ch2"), State: "off"}
			pe.reader.Plugs.HeldS = 240
		}, 424, "READER does not ask Home Assistant right now: its token was refused there, and it waits before it asks again (240 s left; a restart of its agent ends the wait). " +
			"Nothing was switched. (READER says: the machine's own sentence for ha_unreachable)"},
	} {
		c.make()
		r := pe.flip("reader/desk-bar", "ch2", "on")
		if msg, _ := plugDetailOf(r); r.code != c.status || msg != c.said {
			t.Errorf("%s: %d %q\nwant %d %q", c.name, r.code, msg, c.status, c.said)
		}
		if out := fmt.Sprint(pe.plugAudits()[0]["outcome"]); !strings.HasPrefix(out, "refused (") || !strings.Contains(out, strings.TrimSuffix(c.said[:40], " ")) {
			t.Errorf("%s: audit %q", c.name, out)
		}
	}
	if n := len(plugSent(pe.reader)); n != 0 {
		t.Errorf("the machine got %d switch requests", n)
	}
	if n := len(pe.iss.Asked()); n != 0 {
		t.Errorf("Pocket ID was asked %d times", n)
	}
}

// The machine's answers to the switch request itself, each in the panel's words: whether something was
// switched, and whether the approval is used up. "No answer" is never "no".
func TestPlugSwitchAnswers(t *testing.T) {
	no := func(status int, code string, more ...any) func(api.PlugSwitch) (int, jsonx.M) {
		return func(api.PlugSwitch) (int, jsonx.M) {
			out := jsonx.M{"error": "the machine's own sentence for " + code, "code": code}
			if code == "" {
				delete(out, "code")
			}
			for i := 0; i+1 < len(more); i += 2 {
				out[more[i].(string)] = more[i+1]
			}
			return status, out
		}
	}
	for _, c := range []struct {
		name    string
		answer  func(api.PlugSwitch) (int, jsonx.M)
		status  float64
		said    string
		outcome string
	}{
		{"Home Assistant refused the switch request", no(502, api.CodeHARefused), 424,
			"Home Assistant refused the request, so Lamp was most likely not switched. READER did not look at it again: look at the socket. The passkey approval is used up. " +
				"(READER says: the machine's own sentence for ha_refused)", "failed (ha_refused)"},
		{"Home Assistant did not answer the switch request", no(502, api.CodeNoAnswer), 424,
			"No answer from Home Assistant. It may have been switched: look at the socket. The passkey approval is used up.", "failed (no_answer)"},
		{"the socket was switched a moment ago", no(429, api.CodeTooSoon, "retry_after_s", 7.0), 429, "Wait 7 s before you switch Lamp again.", "refused (too_soon)"},
		{"another socket is being switched", no(409, api.CodeSwitchInProgress), 409, "READER is switching another socket right now. Nothing was switched: try again in a moment.", "refused (switch_in_progress)"},
		{"Home Assistant lost the socket meanwhile", no(409, api.CodeSocketUnavailable), 409, "Home Assistant cannot reach this socket right now. Nothing was switched.", "refused (socket_unavailable)"},
		{"the state could not be read", no(502, api.CodeHAUnreachable), 424,
			"READER cannot reach Home Assistant. Nothing was switched. (READER says: the machine's own sentence for ha_unreachable)", "refused (ha_unreachable)"},
		{"the machine's check went away", no(409, api.CodeApprovalOff), 409, "READER has no passkey check. Plugs are switched only with one. Nothing was switched.", "refused (approval_off)"},
		{"an agent from before v0.10.0", func(api.PlugSwitch) (int, jsonx.M) { return 404, jsonx.M{"error": "not found"} }, 409,
			"The agent on READER is too old to switch plugs. Update it.", "refused (agent_too_old)"},
		{"the control token is refused", func(api.PlugSwitch) (int, jsonx.M) { return 401, jsonx.M{"error": "unauthorized"} }, 424,
			"agent rejected the control token (check control_token_env). Nothing was switched.", "refused (control_token)"},
		// An answer the panel does not know is never "refused" (which says that nothing was sent to Home
		// Assistant): the panel cannot tell. A code it has no sentence for, an error without a code, and a 2xx that
		// is not 200, whatever its body says (review of stage 1a, finding 6).
		{"a code this panel does not know", no(409, "moon_phase"), 502,
			"READER gave an answer this panel does not know (moon_phase: the machine's own sentence for moon_phase). It is not known whether the socket was switched: look at the socket.", "failed (moon_phase)"},
		{"an error without a code", func(api.PlugSwitch) (int, jsonx.M) { return 500, jsonx.M{"error": "boom"} }, 502,
			"READER gave an answer this panel does not know (http_500: boom). It is not known whether the socket was switched: look at the socket.", "failed (http_500)"},
		{"a code that is long and not a word", func(api.PlugSwitch) (int, jsonx.M) {
			return 409, jsonx.M{"error": "odd", "code": "<b>" + strings.Repeat("x", 300)}
		}, 502,
			"READER gave an answer this panel does not know (<b>" + strings.Repeat("x", 37) + "…: odd). It is not known whether the socket was switched: look at the socket.",
			"failed (<b>" + strings.Repeat("x", 37) + "…)"},
		{"a 201 with the answer of a switch", func(b api.PlugSwitch) (int, jsonx.M) {
			return 201, jsonx.M{"source": b.Source, "socket": b.Socket, "to": b.To, "was": "off", "state": "on", "confirmed": true}
		}, 502, "READER answered with HTTP 201, which is no answer to a switch. It is not known whether the socket was switched: look at the socket.", "failed (http_201)"},
		{"a 202 with a code of a refusal", no(202, api.CodeNotAllowed), 502,
			"READER answered with HTTP 202, which is no answer to a switch. It is not known whether the socket was switched: look at the socket.", "failed (http_202)"},
		// no answer of the agent: a gateway's 502 and 504, an answer that is not the agent's
		{"a gateway's 502", func(api.PlugSwitch) (int, jsonx.M) { return 502, jsonx.M{"error": "bad gateway"} }, 502,
			"No answer from READER. It may have been switched: look at the socket.", "failed (no_answer_agent)"},
		{"a gateway's 504 with the agent's code", no(504, api.CodeNoAnswer), 502, "No answer from READER. It may have been switched: look at the socket.", "failed (no_answer_agent)"},
		{"a 200 without the agent's answer", func(api.PlugSwitch) (int, jsonx.M) { return 200, jsonx.M{} }, 502,
			"No answer from READER. It may have been switched: look at the socket.", "failed (no_answer_agent)"},
		{"a 200 for another socket", func(b api.PlugSwitch) (int, jsonx.M) {
			return 200, jsonx.M{"source": b.Source, "socket": "ch1", "to": b.To, "was": "off", "state": "on", "confirmed": true}
		}, 502, "READER answered for another request than the one sent. It may have been switched: look at the socket.", "failed (other_answer)"},
		{"a 200 for the other direction", func(b api.PlugSwitch) (int, jsonx.M) {
			return 200, jsonx.M{"source": b.Source, "socket": b.Socket, "to": "off", "was": "on", "state": "off", "confirmed": true}
		}, 502, "READER answered for another request than the one sent. It may have been switched: look at the socket.", "failed (other_answer)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pe := newPlugEnv(t)
			pe.reader.Plugs.Answer = c.answer
			id, _, ok := approvalAsked(pe.flip("reader/desk-bar", "ch2", "on"))
			if !ok {
				t.Fatal("not asked")
			}
			if cb := pe.approve(t, id); cb.code != 303 {
				t.Fatal(cb.code)
			}
			pr := pe.result(t)
			if pr["ok"] != false || pr["status"] != c.status || pr["message"] != c.said {
				t.Fatalf("result: %v\nwant %v %q", pr, c.status, c.said)
			}
			if n := len(plugSent(pe.reader)); n != 1 {
				t.Errorf("the machine got %d switch requests, want exactly 1 (never a second one)", n)
			}
			au := pe.plugAudits()[0]
			if out := fmt.Sprint(au["outcome"]); !strings.HasPrefix(out, c.outcome+": ") || au["params"].(map[string]any)["approval"] != "sent" {
				t.Errorf("audit: %v", au)
			}
		})
	}
	// the agent could not be reached at all, or its connection broke: "no answer", and never a second request
	t.Run("the request got no answer", func(t *testing.T) {
		pe := newPlugEnv(t)
		id, _, ok := approvalAsked(pe.flip("reader/desk-bar", "ch2", "on"))
		if !ok {
			t.Fatal("not asked")
		}
		pe.gb.Fail("reader", errors.New("read tcp: connection reset by peer"))
		if cb := pe.approve(t, id); cb.code != 303 {
			t.Fatal(cb.code)
		}
		pr := pe.result(t)
		if pr["ok"] != false || pr["message"] != "No answer from READER. It may have been switched: look at the socket." {
			t.Fatalf("result: %v", pr)
		}
		if out := fmt.Sprint(pe.plugAudits()[0]["outcome"]); !strings.HasPrefix(out, "failed (no_answer_agent): ") {
			t.Errorf("audit: %q", out)
		}
	})
	// sent, and the socket does not show the new state yet: said as it is, in the answer and in the audit log
	t.Run("not confirmed", func(t *testing.T) {
		pe := newPlugEnv(t)
		pe.reader.Plugs.Unconfirmed = true
		id, _, ok := approvalAsked(pe.flip("reader/desk-bar", "ch2", "on"))
		if !ok {
			t.Fatal("not asked")
		}
		if cb := pe.approve(t, id); cb.code != 303 {
			t.Fatal(cb.code)
		}
		pr := pe.result(t)
		if pr["ok"] != true || pr["warn"] != true || pr["message"] != "Sent. Lamp does not show on yet." {
			t.Fatalf("result: %v", pr)
		}
		au := pe.plugAudits()[0]
		if p := au["params"].(map[string]any); au["outcome"] != "ok (not confirmed)" || p["confirmed"] != false || p["was"] != "off" {
			t.Errorf("audit: %v", au)
		}
	})
	// a machine that says "confirmed" with a state that is not the one asked for is not believed
	t.Run("confirmed with another state", func(t *testing.T) {
		pe := newPlugEnv(t)
		pe.reader.Plugs.Answer = func(b api.PlugSwitch) (int, jsonx.M) {
			return 200, jsonx.M{"source": b.Source, "socket": b.Socket, "to": b.To, "was": "off", "state": "off", "confirmed": true, "took_ms": 3.0}
		}
		id, _, _ := approvalAsked(pe.flip("reader/desk-bar", "ch2", "on"))
		pe.approve(t, id)
		if pr := pe.result(t); pr["ok"] != true || pr["message"] != "Sent. Lamp does not show on yet." {
			t.Fatalf("result: %v", pr)
		}
	})
}

// The same approval is good for one switch: a second request with it is the machine's "no".
func TestPlugApprovalIsUsedUp(t *testing.T) {
	pe := newPlugEnv(t)
	id, _, _ := approvalAsked(pe.flip("reader/desk-bar", "ch2", "on"))
	if cb := pe.approve(t, id); cb.code != 303 {
		t.Fatal(cb.code)
	}
	sent := plugSent(pe.reader)
	if len(sent) != 1 || !sent[0].Approved {
		t.Fatalf("%+v", sent)
	}
	// whoever holds the panel's control token sends the very same body again
	st, data := pe.gb.Forward("reader", api.PathPlugSwitch, sent[0].Body)
	if st != 403 || data["code"] != api.CodeApprovalRefused {
		t.Fatalf("the same approval again: %d %v", st, data)
	}
	if n := len(pe.reader.Passed()); n != 1 {
		t.Errorf("the gate let %d requests through", n)
	}
}

// What GET /api/power gives of a socket: an admin gets whether it can be switched from here, why not, what it
// is linked to and the tag; everybody else its name and its state.
func TestPlugSocketsInThePowerAnswer(t *testing.T) {
	pe := newPlugEnv(t)
	for _, c := range []struct {
		source, socket, state string
		can                   bool
		why, lock, link       string
	}{
		{"reader/desk-bar", "ch1", "on", true, "", "", ""},
		{"reader/desk-bar", "ch2", "off", true, "", "", ""},
		{"reader/desk-bar", "ch3", "on", true, "", "", "desk-pc"},
		{"reader/desk-bar", "ch4", "on", false, model.SocketLockAgent, model.SocketLockAgent, "lab-node-pve"},
		{"reader/desk-bar", "ch5", "on", false, model.SocketLockHomeAssistant, model.SocketLockHomeAssistant, "lab-qemu-100"},
		{"reader/desk-bar", "ch6", "on", false, model.SocketLockPanel, model.SocketLockPanel, "panel-box"},
		{"reader/desk-bar", "ch7", "on", true, "", "", ""}, // linked to an id no row has: no row to name
		{"reader/desk-bar", "ch8", "on", false, api.WhyNotAllowed, "", ""},
		{"reader/pc-plug", "main", "on", true, "", "", "desk-pc"},
		{"reader/own-plug", "main", "on", false, model.SocketLockAgent, model.SocketLockAgent, "lab-node-pve"},
		{"other/rack-bar", "node", "on", false, model.SocketLockHomeAssistant, model.SocketLockHomeAssistant, "lab-node-pve"},
		{"other/rack-bar", "fan", "off", true, "", "", ""},
		{"third/spare-bar", "pc", "on", false, model.SocketLockHAUnknown, model.SocketLockHAUnknown, "desk-pc"},
		{"third/spare-bar", "lamp", "off", true, "", "", ""},
		{"bare/bare-bar", "lamp", "off", false, model.SocketNoControl, "", ""},
	} {
		s := pe.socketOf(pe.admin, c.source, c.socket)
		_, plug, _ := strings.Cut(c.source, "/")
		get := func(k string) string { v, _ := s[k].(string); return v }
		if s == nil || s["state"] != c.state || s["switchable"] != c.can || get("why") != c.why || get("lock") != c.lock || get("system_id") != c.link ||
			s["tag"] != plugTag(plug, c.socket) {
			t.Errorf("%s/%s for an admin: %v", c.source, c.socket, s)
		}
		// the sentence behind the lock on the page: there for every socket that cannot be switched, and only there
		machine := "PVE"
		if strings.HasPrefix(c.source, "bare/") {
			machine = "BARE"
		}
		if (get("note") != "") != (c.why != "") || get("note") != socketWhyTextOr(c.why, machine) {
			t.Errorf("%s/%s: the sentence for %q is %q", c.source, c.socket, c.why, get("note"))
		}
	}
	// a viewer of the site: the state and the name, the row it feeds when the viewer sees that row; no tag,
	// no switch, no reason
	ck := pe.viewerOf("Home")
	for _, c := range []struct{ socket, link string }{{"ch1", ""}, {"ch3", "desk-pc"}, {"ch4", "lab-node-pve"}, {"ch6", ""}, {"ch8", ""}} {
		s := pe.socketOf(ck, "reader/desk-bar", c.socket)
		link, _ := s["system_id"].(string)
		if s == nil || s["switchable"] != false || s["why"] != model.SocketReadOnly || s["tag"] != nil || s["lock"] != nil || s["note"] != nil || s["state"] != "on" || s["name"] == "" || link != c.link {
			t.Errorf("%s for a viewer: %v", c.socket, s)
		}
	}
	if s := pe.socketOf(ck, "other/rack-bar", "fan"); s != nil {
		t.Errorf("a viewer of another site sees %v", s)
	}
	// a machine without the check: its sockets are shown and not switchable, for the reason the panel's record gives
	if err := os.Remove(pe.reader.TrustPath); err != nil {
		t.Fatal(err)
	}
	pe.reader.Gate.StatusNow()
	pe.poll()
	if s := pe.socketOf(pe.admin, "reader/desk-bar", "ch1"); s["switchable"] != false || (s["why"] != api.WhyApprovalOff && s["why"] != api.WhyApprovalUnavailable) {
		t.Errorf("a socket of a machine whose check is gone: %v", s)
	}
	// the manager's own rows are never written to by an answer
	for _, st := range pe.s.Poller.Power.Overview().Sources {
		for _, k := range st.Sockets {
			if k.SystemID != nil || k.Lock != "" || k.Why == model.SocketReadOnly {
				t.Fatalf("the kept row of %s/%s was changed by an answer: %+v", st.ID, k.ID, k)
			}
		}
	}
}

// The words the dialogs and the kept action use for a switch, and the ones made from the request alone.
func TestPlugWords(t *testing.T) {
	src := model.PowerSourceState{Name: "Monitor powerbar"}
	sock := model.PowerSocket{Name: "Left monitor"}
	pc := &model.System{ID: "desk-pc", Name: "DESK-PC", Status: "online"}
	off := &model.System{ID: "desk-pc", Name: "DESK-PC", Status: "offline"}
	for _, c := range []struct {
		k    socketLook
		to   string
		cut  bool
		want string
	}{
		{socketLook{src: src, sock: sock}, "off", false, "switch off Left monitor (Monitor powerbar)"},
		{socketLook{src: src, sock: sock}, "on", false, "switch on Left monitor (Monitor powerbar)"},
		{socketLook{src: src, sock: sock, link: "desk-pc", shown: "desk-pc", row: off}, "off", false, "switch off Left monitor (Monitor powerbar), which powers DESK-PC"},
		{socketLook{src: src, sock: sock, link: "desk-pc", shown: "desk-pc", row: pc}, "on", false, "switch on Left monitor (Monitor powerbar), which powers DESK-PC"},
		{socketLook{src: src, sock: sock, link: "desk-pc", shown: "desk-pc", row: pc}, "off", true, "switch off Left monitor (Monitor powerbar): cut the power of DESK-PC while it is running"},
		{socketLook{src: src, sock: sock, link: "old-pc", shown: "old-pc"}, "off", true, "switch off Left monitor (Monitor powerbar): cut the power of old-pc while it is running"},
	} {
		if got := plugWords(c.k, c.to, c.cut); got != c.want {
			t.Errorf("%q, want %q", got, c.want)
		}
	}
	if got := requestWords(api.SocketSetRequest("desk-bar", "ch1", "off", plugTag("desk-bar", "ch1"))); got != "switch off the socket ch1 of desk-bar" {
		t.Errorf("the words made from the request alone: %q", got)
	}
	if _, ok := actionText["POST /api/power/socket"]; !ok {
		t.Error("the kept request has no description")
	}
	// every reason has a sentence of its own, and none of them is the fallback
	for _, why := range []string{model.SocketLockAgent, model.SocketLockHomeAssistant, model.SocketLockHAUnknown, model.SocketLockPanel, api.WhyFeedsSelf,
		api.WhyNotAllowed, api.WhySwitchingOff, api.WhyNoToken, api.WhyApprovalOff, api.WhyApprovalUnavailable, model.SocketAgentUnreachable,
		model.SocketNoControl, model.SocketNoTag} {
		if got := socketWhyText(why, "READER"); got == "This socket cannot be switched." || got == "" {
			t.Errorf("no sentence for %s", why)
		}
	}
	if got := socketWhyText(model.SocketNotSwitchable, "READER"); got != "This socket cannot be switched." {
		t.Errorf("%q", got)
	}
}

// Every attempt is in the audit log under its own name, also while the admin's budget of refused requests
// ("auth.denied", ten a minute) is used up: the route has no share in that budget (review of stage 1a, finding
// 2: ten refused requests of the same account hid which sockets were tried). What bounds the entries is the
// limit on control requests, which the route applies before it looks at anything.
func TestPlugAttemptsAreAllInTheAuditLog(t *testing.T) {
	pe := newPlugEnv(t)
	pe.nextMinute()
	// ten refused requests of the admin in this minute (no page header): ten auth.denied, and the budget is gone
	noHeader := func() resp {
		return pe.do("POST", "/api/power/socket", plugBody("reader/desk-bar", "ch1", "off"), "Cookie", pe.admin, "Content-Type", "application/json")
	}
	for range deniedPerSub + 3 {
		if r := noHeader(); r.code != 403 {
			t.Fatal(r.code, string(r.raw))
		}
	}
	if n := len(pe.auditEntries("u2", deniedAction)); n != deniedPerSub {
		t.Fatalf("the fixture: %d auth.denied entries, want %d (the budget is used up)", n, deniedPerSub)
	}
	// "off" for Home Assistant's socket, the reader's, the panel's and a running PC's: each leaves its own entry
	tried := []struct{ socket, outcome string }{
		{"ch5", "refused (home_assistant): Locked. This socket powers Home Assistant. Nothing was switched."},
		{"ch4", "refused (reads_plugs): Locked. This socket powers the machine that reads the plugs. Nothing was switched."},
		{"ch6", "refused (panel): Locked. This socket powers the panel. Nothing was switched."},
		{"ch3", "refused (machine_running): DESK-PC is still running. Shut it down first."},
		{"ch9", "error 404: there is no such socket"},
	}
	for i, c := range tried {
		if r := pe.flip("reader/desk-bar", c.socket, "off"); r.code != 409 && r.code != 404 {
			t.Fatalf("%s: %d %s", c.socket, r.code, r.raw)
		}
		got := pe.plugAudits()
		if len(got) != i+1 {
			t.Fatalf("%s: %d plug.switch entries, want %d: the attempt is not on record", c.socket, len(got), i+1)
		}
		if got[0]["target"] != "reader/desk-bar/"+c.socket || got[0]["outcome"] != c.outcome || got[0]["params"].(map[string]any)["to"] != "off" {
			t.Fatalf("%s: the entry %v\nwant %q", c.socket, got[0], c.outcome)
		}
	}
	// the other way round: many attempts on sockets take nothing from the budget of auth.denied
	pe.nextMinute()
	for range 3 * deniedPerSub {
		if r := pe.flip("reader/desk-bar", "ch8", "on"); r.code != 409 {
			t.Fatal(r.code, string(r.raw))
		}
	}
	if n := len(pe.plugAudits()); n != len(tried)+3*deniedPerSub {
		t.Fatalf("%d plug.switch entries, want %d: one for every attempt", n, len(tried)+3*deniedPerSub)
	}
	before := len(pe.auditEntries("u2", deniedAction))
	if r := noHeader(); r.code != 403 {
		t.Fatal(r.code)
	}
	if n := len(pe.auditEntries("u2", deniedAction)); n != before+1 {
		t.Errorf("refused switches used up the budget of auth.denied: %d entries, want %d", n, before+1)
	}
	// nothing of all this reached a machine
	if n := len(pe.reader.Asked()); n != 0 {
		t.Errorf("the machine was asked %d things", n)
	}
	pe.nextMinute()
	for _, e := range pe.auditEntries("u2", "audit.suppressed") {
		if strings.Contains(fmt.Sprint(e["outcome"]), "under another action") {
			t.Errorf("a summary still counts refusals under another action: %v", e["outcome"])
		}
	}
}

// The limit on control requests (control_rate_per_min, for one admin) is the first thing the route asks after
// who is calling: a request over the limit is answered 429 with the time to wait, like the other control
// routes, before the body is read and before anything is looked up. So it is no attempt on a socket and
// leaves no plug.switch entry, and the entries one admin can write in a minute are bounded by the limit. The
// panel's own replay after the passkey passes the same limit, and is sent nowhere when it is over it.
func TestPlugSwitchPassesTheControlLimitFirst(t *testing.T) {
	pe := newPlugEnv(t)
	pe.s.control = newRateLimiter(3)
	for i := range 3 {
		if r := pe.flip("reader/desk-bar", "ch4", "off"); r.code != 409 {
			t.Fatalf("request %d inside the limit: %d %s", i+1, r.code, r.raw)
		}
	}
	bodies := map[string]map[string]any{
		"a locked socket":           plugBody("reader/desk-bar", "ch4", "off"),
		"a socket that can be":      plugBody("reader/desk-bar", "ch1", "off"),
		"a socket that is not here": plugBody("reader/desk-bar", "ch9", "off"),
		"a body that is not one":    {"to": "toggle"},
	}
	for name, b := range bodies {
		r := pe.post("/api/power/socket", b)
		if msg, _ := plugDetailOf(r); r.code != 429 || !strings.HasPrefix(msg, "too many control requests; try again in ") || r.hdr.Get("Retry-After") == "" {
			t.Errorf("%s, over the limit: %d %s (Retry-After %q)", name, r.code, r.raw, r.hdr.Get("Retry-After"))
		}
	}
	if n := len(pe.plugAudits()); n != 3 {
		t.Errorf("%d plug.switch entries, want 3: a request over the limit is not looked at", n)
	}
	if n := len(pe.reader.Asked()); n != 0 {
		t.Errorf("the machine was asked %d things", n)
	}
	if n := len(pe.auditEntries("u2", deniedAction)); n != len(bodies) {
		t.Errorf("%d auth.denied entries for %d requests over the limit", n, len(bodies))
	}

	// the replay after the passkey: the question was the one request the limit allowed
	pe = newPlugEnv(t)
	pe.s.control = newRateLimiter(1)
	id, _, ok := approvalAsked(pe.flip("reader/desk-bar", "ch2", "on"))
	if !ok {
		t.Fatal("not asked")
	}
	if cb := pe.approve(t, id); cb.code != 303 {
		t.Fatal(cb.code)
	}
	pr := pe.result(t)
	if pr["ok"] != false || pr["status"] != 429.0 || !strings.HasPrefix(fmt.Sprint(pr["message"]), "too many control requests; try again in ") {
		t.Fatalf("the replay over the limit: %v", pr)
	}
	if n := len(plugSent(pe.reader)); n != 0 {
		t.Fatalf("the machine got %d switch requests from a replay over the limit", n)
	}
	// with room for both, the switch is sent: the question and the replay are one request each
	pe = newPlugEnv(t)
	pe.s.control = newRateLimiter(2)
	id, _, _ = approvalAsked(pe.flip("reader/desk-bar", "ch2", "on"))
	if cb := pe.approve(t, id); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if pr := pe.result(t); pr["ok"] != true || len(plugSent(pe.reader)) != 1 {
		t.Fatalf("a switch inside the limit: %v", pr)
	}
	if r := pe.flip("reader/desk-bar", "ch2", "off"); r.code != 429 {
		t.Errorf("a third request with a limit of two: %d", r.code)
	}
}

// socketWhyTextOr is the sentence for a reason, "" for no reason.
func socketWhyTextOr(why, machine string) string {
	if why == "" {
		return ""
	}
	return socketWhyText(why, machine)
}

// The lock of the panel's own machine holds also while that machine has no row: it is named in panel_hosts,
// and the panel has not heard of it or it was taken out. The link alone locks the socket then, in both
// directions (the rule of the power menu, which looks at a row, cannot say it).
func TestPlugLockOfThePanelsMachineWithoutARow(t *testing.T) {
	pe := newPlugEnv(t)
	// the fixture: desk-bar/ch7 is linked to old-pc, an id no row has, and can be switched
	if s := pe.socketOf(pe.admin, "reader/desk-bar", "ch7"); s["switchable"] != true || s["lock"] != nil {
		t.Fatalf("the fixture: %v", s)
	}
	pe.s.Cfg.PanelHosts = append(pe.s.Cfg.PanelHosts, "old-pc")
	s := pe.socketOf(pe.admin, "reader/desk-bar", "ch7")
	if s["switchable"] != false || s["lock"] != model.SocketLockPanel || s["why"] != model.SocketLockPanel || s["note"] != "Locked. This socket powers the panel." {
		t.Errorf("a socket of a panel host without a row: %v", s)
	}
	for _, to := range []string{"off", "on"} {
		r := pe.flip("reader/desk-bar", "ch7", to)
		if msg, _ := plugDetailOf(r); r.code != 409 || msg != "Locked. This socket powers the panel. Nothing was switched." {
			t.Errorf("%s: %d %s", to, r.code, r.raw)
		}
	}
	if n := len(pe.reader.Asked()); n != 0 {
		t.Errorf("the machine was asked %d times for a locked socket", n)
	}
}

// The power menu's rule for the panel's machine (powerctl.KillsPanel) is the socket's rule too: the panel runs
// in a guest, panel_hosts names the guest, and a socket feeds that guest's node. The node is in no list. It is
// locked all the same, and not before the panel is said to run there.
func TestPlugLockOfTheNodeUnderThePanelsGuest(t *testing.T) {
	pe := newPlugEnv(t)
	snap := pe.s.Poller.Snapshot()
	l := pe.s.newPlugLooks(snap)
	node := l.rows["lab-node-pve"]
	if node == nil || node.Kind != "node" || l.rows["lab-qemu-100"] == nil {
		t.Fatalf("the fixture: the node %v and its guest %v", node, l.rows["lab-qemu-100"])
	}
	// a socket that feeds the node, read by an agent that runs elsewhere and has no Home Assistant of its own on it
	bare, ok := pe.s.Poller.AgentByID("bare")
	if !ok {
		t.Fatal("the fixture: no agent bare")
	}
	k := socketLook{agent: bare, link: node.ID, shown: node.ID, row: node}
	if got := l.lock(k); got != "" {
		t.Fatalf("the fixture: the socket is locked (%s) while the panel does not run on the node", got)
	}
	pe.s.Cfg.PanelHosts = []string{"lab-qemu-100"}
	pe.s.Poller.Cfg.PanelHosts = []string{"lab-qemu-100"}
	if got := pe.s.newPlugLooks(snap).lock(k); got != model.SocketLockPanel {
		t.Errorf("a socket of the node under the panel's guest: lock %q, want %q", got, model.SocketLockPanel)
	}
}
