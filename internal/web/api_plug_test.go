package web

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
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
// socket in one direction, and the request names ids and a tag, never an entity, an address or a service. The
// socket is linked to nothing, so its "off" is the one the page sends after "nothing is linked" was confirmed
// (cut: TestPlugOffForASocketThatIsLinkedToNothing).
func TestPlugSwitchOffAndOn(t *testing.T) {
	pe := newPlugEnv(t)
	logs := captureLog(t)
	var answers []string
	// an admin who signed in this second is asked all the same: the panel's own window never switches a socket
	fresh := pe.cookie(controller(), 0)
	r := pe.do("POST", "/api/power/socket", plugBody("reader/desk-bar", "ch2", "on"), "Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json")
	if _, _, ok := approvalAsked(r); !ok {
		t.Fatalf("inside the panel's own window: %d %s", r.code, r.raw)
	}
	for i, c := range []struct{ to, was, said string }{{"off", "on", "Left monitor is off."}, {"on", "off", "Left monitor is on."}} {
		b := plugBody("reader/desk-bar", "ch1", c.to)
		if c.to == "off" {
			b["cut"] = true
		}
		r := pe.post("/api/power/socket", b)
		answers = append(answers, string(r.raw))
		id, machines, ok := approvalAsked(r)
		if !ok || len(machines) != 1 || machines[0] != "READER" {
			t.Fatalf("%s: the dialog: %d %s", c.to, r.code, r.raw)
		}
		d := r.body["detail"].(map[string]any)
		words := "switch " + c.to + " Left monitor (Monitor powerbar)"
		if c.to == "off" {
			words = "cut the power of whatever is plugged in: " + words
		}
		if d["signs"] != words || d["action"] != "switch a plug socket ("+words+")" {
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
			p["plug"] != "Monitor powerbar" || p["agent"] != "reader" || (p["cut"] == true) != (c.to == "off") || p["linked"] != nil {
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
		if r.code != 409 || d["code"] != "machine_running" || d["machine"] != machine || d["message"] != said || d["system_id"] != system || d["state"] != "running" {
			t.Fatalf("%d %s\nwant machine_running for %s: %q", r.code, r.raw, machine, said)
		}
	}
	// by socket_links, and by the plug's own link for its only socket
	running(pe.flip("reader/desk-bar", "ch3", "off"), "DESK-PC", "desk-pc", "DESK-PC is still running. Shut it down first.")
	running(pe.flip("reader/pc-plug", "main", "off"), "DESK-PC", "desk-pc", "DESK-PC is still running. Shut it down first.")
	if au := pe.plugAudits()[0]; au["outcome"] != "refused (machine_running): DESK-PC is still running. Shut it down first." || au["params"].(map[string]any)["linked"] != "desk-pc" {
		t.Errorf("the audit entry of the refusal: %v", au)
	}
	// a link to an id no row has is no case of this question any more: the socket is locked (TestPlugLinkToARowThePanelDoesNotHave)
	if r := pe.flip("reader/desk-bar", "ch7", "off"); r.code != 409 || strings.Contains(string(r.raw), "machine_running") {
		t.Errorf("a link to an id no row has: %d %s", r.code, r.raw)
	}
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
	if d := r.body["detail"].(map[string]any); d["signs"] != "cut the power of DESK-PC while it is running: switch off Desk PC (Monitor powerbar)" {
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
}

// When the machine on a socket counts as off for the question "this machine is still running". Not answering
// is never enough by itself, however long it lasts: a link that dropped, an agent that was stopped or hangs and
// a machine that installs updates while it shuts down all look like that (second review of stage 1a). It counts
// as off only with a sign of it: Proxmox says the guest is stopped, a run of "Update unpowered hosts" saw the
// machine go off (Poller.MarkOff), or the socket has a meter of its own (the only socket of a plug) and a fresh
// reading of it is under 5 W. A meter of its own that still measures a load says the opposite, whatever else is
// known. Everything in between is "unknown": the page says "may still be running" and asks for the typed name.
func TestPlugRunningRule(t *testing.T) {
	now := time.Now()
	meter := func(sockets int, watts *float64, age time.Duration, status string) model.PowerSourceState {
		st := model.PowerSourceState{Name: "PC plug", Status: status, Sockets: make([]model.PowerSocket, sockets)}
		st.Readings = []model.PowerReading{{Watts: watts, Timestamp: now.Add(-age)}}
		return st
	}
	w := func(v float64) *float64 { return &v }
	none := model.PowerSourceState{Name: "Monitor powerbar", Status: "ok", Sockets: make([]model.PowerSocket, 8), Readings: []model.PowerReading{{Watts: w(1), Timestamp: now}}}
	for _, c := range []struct {
		name     string
		status   string
		asleep   bool
		on       string // the row's sign that the machine itself answers (MachineOn)
		knownOff bool
		src      model.PowerSourceState
		want     string
	}{
		{"online", "online", false, "", false, none, "running"},
		{"stale: not heard from for a moment", "stale", false, "", false, none, "running"},
		{"paused", "paused", false, "", false, none, "running"},
		{"error", "error", false, "", false, none, "running"},
		{"offline, and nothing else is known", "offline", false, "", false, none, "unknown"},
		{"offline, sent to sleep by the panel", "offline", true, "", false, none, "asleep"},
		{"offline, and the machine itself answers", "offline", false, "the machine refused the connection", true, none, "running"},
		{"offline, seen to go off", "offline", false, "", true, none, ""},
		// "stopped" is what Proxmox says of a guest, and a guest is never the machine of a socket (a link to one
		// is a link to its node: look). It is no sign that anything is off. (Until the second review of the fixes
		// this case wanted "".)
		{"a guest Proxmox says is stopped", "stopped", false, "", false, none, "unknown"},
		{"stopped, its own plug reads 2 W", "stopped", false, "", false, meter(1, w(2), 5*time.Second, "ok"), ""},
		// the socket's own meter: the only socket of a plug, a fresh reading
		{"offline, its own plug reads 2 W", "offline", false, "", false, meter(1, w(2), 5*time.Second, "ok"), ""},
		{"offline, its own plug reads 4.9 W", "offline", false, "", false, meter(1, w(4.9), 5*time.Second, "ok"), ""},
		{"offline, its own plug reads 5 W", "offline", false, "", false, meter(1, w(5), 5*time.Second, "ok"), "unknown"},
		// only a reading from 0 W to under 5 W is a sign of "off": a meter that reads below zero (a clamp put on
		// backwards, a meter that also counts what is fed in) reads so for a load that runs, and a value that is
		// no number says nothing. Such a reading is no sign either way, also next to another sign.
		{"offline, its own plug reads 0 W", "offline", false, "", false, meter(1, w(0), 5*time.Second, "ok"), ""},
		{"offline, its own plug reads -0.5 W", "offline", false, "", false, meter(1, w(-0.5), 5*time.Second, "ok"), "unknown"},
		{"offline, its own plug reads -30 W", "offline", false, "", false, meter(1, w(-30), 5*time.Second, "ok"), "unknown"},
		{"offline, its own plug reads no number", "offline", false, "", false, meter(1, w(math.NaN()), 5*time.Second, "ok"), "unknown"},
		{"offline, its own plug reads +Inf", "offline", false, "", false, meter(1, w(math.Inf(1)), 5*time.Second, "ok"), "unknown"},
		{"offline, its own plug reads -Inf", "offline", false, "", false, meter(1, w(math.Inf(-1)), 5*time.Second, "ok"), "unknown"},
		{"seen to go off, and its own plug reads -30 W", "offline", false, "", true, meter(1, w(-30), 5*time.Second, "ok"), "unknown"},
		{"seen to go off, an old reading of -30 W", "offline", false, "", true, meter(1, w(-30), 5*time.Minute, "ok"), ""},
		{"seen to go off, and its own plug reads 40 W", "offline", false, "", true, meter(1, w(40), 5*time.Second, "ok"), "unknown"},
		{"stopped, and its own plug reads 40 W", "stopped", false, "", false, meter(1, w(40), 5*time.Second, "ok"), "unknown"},
		{"online, its own plug reads 0 W", "online", false, "", false, meter(1, w(0), 5*time.Second, "ok"), "running"},
		{"asleep, its own plug reads 1 W", "offline", true, "", false, meter(1, w(1), 5*time.Second, "ok"), "asleep"},
		// a reading that is old, of a plug that fails, without watts, or of a plug with more sockets says nothing
		{"offline, a reading of 2 W that is old", "offline", false, "", false, meter(1, w(2), 5*time.Minute, "ok"), "unknown"},
		{"offline, a reading of 2 W from the future", "offline", false, "", false, meter(1, w(2), -5*time.Minute, "ok"), "unknown"},
		{"offline, 2 W of a plug that is stale", "offline", false, "", false, meter(1, w(2), 5*time.Second, "stale"), "unknown"},
		{"offline, a reading without watts", "offline", false, "", false, meter(1, nil, 5*time.Second, "ok"), "unknown"},
		{"offline, 2 W of a powerbar", "offline", false, "", false, meter(2, w(2), 5*time.Second, "ok"), "unknown"},
		{"seen to go off, an old reading of 40 W", "offline", false, "", true, meter(1, w(40), 5*time.Minute, "ok"), ""},
		{"seen to go off, 40 W of a powerbar", "offline", false, "", true, meter(2, w(40), 5*time.Second, "ok"), ""},
	} {
		row := &model.System{ID: "desk-pc", Name: "DESK-PC", Kind: "agent", Status: c.status, Asleep: c.asleep, MachineOn: c.on}
		k := socketLook{src: c.src, link: "desk-pc", shown: "desk-pc", row: row, knownOff: c.knownOff, at: now, fresh: time.Minute}
		if got := k.runningState(); got != c.want {
			t.Errorf("%s: state %q, want %q", c.name, got, c.want)
		}
		if name, yes := k.running(); yes != (c.want != "") || name != "DESK-PC" {
			t.Errorf("%s: running %v (%s)", c.name, yes, name)
		}
	}
	// without the time of the look nothing is fresh
	k := socketLook{src: meter(1, w(2), 0, "ok"), link: "desk-pc", shown: "desk-pc", row: &model.System{ID: "desk-pc", Name: "DESK-PC", Status: "offline"}}
	if got := k.runningState(); got != "unknown" {
		t.Errorf("a look without a time: state %q", got)
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
	// the sentence for "not known to be off" says what the panel has
	off := &model.System{ID: "desk-pc", Name: "DESK-PC", Kind: "agent", Status: "offline"}
	for want, k := range map[string]socketLook{
		"DESK-PC does not answer, which does not say that it is off: it may still be running. Shut it down first, or look at it.":                         {src: none, link: "desk-pc", shown: "desk-pc", row: off, at: now, fresh: time.Minute},
		"DESK-PC does not answer, and its plug still measures 40 W: it may still be running. Shut it down first, or look at it.":                          {src: meter(1, w(40), time.Second, "ok"), link: "desk-pc", shown: "desk-pc", row: off, knownOff: true, at: now, fresh: time.Minute},
		"DESK-PC does not answer, and its plug reads -30 W, which is no sign that it is off: it may still be running. Shut it down first, or look at it.": {src: meter(1, w(-30), time.Second, "ok"), link: "desk-pc", shown: "desk-pc", row: off, at: now, fresh: time.Minute},
	} {
		if got := (&plugLooks{}).runningWords(k); got != want {
			t.Errorf("%q\nwant %q", got, want)
		}
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
			b := plugBody("reader/desk-bar", c.socket, "off")
			b["cut"] = c.socket == "ch1" // linked to nothing: the page asked "nothing is linked" first
			r := pe.post("/api/power/socket", b)
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
// direction in the replay gets none, and nothing is sent. Two things stand in the way, and each is asked here.
// The words: what the dialog would show for the changed request is not what it showed (the socket's name, the
// direction). And under them the approval itself, which is made for one request: a second socket with the SAME
// name on the same plug has the same words, and still gets no approval.
func TestPlugApprovalIsForTheExactRequest(t *testing.T) {
	other := func(b string) string {
		b = strings.Replace(b, `"socket":"ch2"`, `"socket":"ch1"`, 1)
		return strings.Replace(b, plugTag("desk-bar", "ch2"), plugTag("desk-bar", "ch1"), 1)
	}
	for name, c := range map[string]struct {
		prepare func(pe *plugEnv)
		edit    func(body string) string
		said    string
	}{
		// ("off" for this socket, which is linked to nothing, with the answer to the page's question in it)
		"another direction": {nil, func(b string) string { return strings.Replace(b, `"to":"on"`, `"to":"off","cut":true`, 1) },
			`Your passkey was asked for "switch on Lamp (Monitor powerbar)", and it would now be "cut the power of whatever is plugged in: switch off Lamp (Monitor powerbar)"`},
		"another socket": {nil, other,
			`Your passkey was asked for "switch on Lamp (Monitor powerbar)", and it would now be "switch on Left monitor (Monitor powerbar)"`},
		"another socket of the same name": {func(pe *plugEnv) {
			pe.set("reader/desk-bar/ch1", "name", "Lamp")
			pe.set("reader/desk-bar/ch1", "state", "off")
		}, other, "not the request that was approved"},
	} {
		t.Run(name, func(t *testing.T) {
			pe := newPlugEnv(t)
			if c.prepare != nil {
				c.prepare(pe)
			}
			id, _, ok := approvalAsked(pe.flip("reader/desk-bar", "ch2", "on"))
			if !ok {
				t.Fatal("not asked")
			}
			l := pe.s.codec.live
			l.mu.Lock()
			p := l.m[sidOf(pe.env, pe.admin)].pending
			changed := c.edit(p.Body)
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
			if pr["ok"] != false || !strings.Contains(fmt.Sprint(pr["message"]), c.said) {
				t.Fatalf("result: %v\nwant %q", pr, c.said)
			}
			if n := len(plugSent(pe.reader)); n != 0 {
				t.Fatalf("the machine got %d switch requests", n)
			}
			if out := fmt.Sprint(pe.plugAudits()[0]["outcome"]); !strings.Contains(out, c.said) {
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
		// linked to Home Assistant's VM: the reason is Home Assistant's, and the row the socket feeds is the VM's node
		{"reader/desk-bar", "ch5", "on", false, model.SocketLockHomeAssistant, model.SocketLockHomeAssistant, "lab-node-pve"},
		{"reader/desk-bar", "ch6", "on", false, model.SocketLockPanel, model.SocketLockPanel, "panel-box"},
		{"reader/desk-bar", "ch7", "on", false, model.SocketLockLinkUnknown, model.SocketLockLinkUnknown, ""}, // linked to an id no row has: locked, no row to name
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
	// a viewer of the site: the state and the name of a socket that is linked to nothing or to a row the viewer
	// sees, with that row; no tag, no switch, no reason
	ck := pe.viewerOf("Home")
	for _, c := range []struct{ socket, link string }{{"ch1", ""}, {"ch3", "desk-pc"}, {"ch4", "lab-node-pve"}, {"ch8", ""}} {
		s := pe.socketOf(ck, "reader/desk-bar", c.socket)
		link, _ := s["system_id"].(string)
		if s == nil || s["switchable"] != false || s["why"] != model.SocketReadOnly || s["tag"] != nil || s["lock"] != nil || s["note"] != nil || s["state"] != "on" || s["name"] == "" || link != c.link {
			t.Errorf("%s for a viewer: %v", c.socket, s)
		}
	}
	// A socket that feeds a row the viewer does not see is not in the viewer's answer at all: not its name
	// ("Panel box", "Old PC"), not its state. The plug says how many are left out. (Until the second review of
	// stage 1a this test wanted ch6 in the answer, without its link.)
	rows := map[string]bool{}
	for _, x := range pe.do("GET", "/api/systems", nil, "Cookie", ck).body["systems"].([]any) {
		rows[fmt.Sprint(x.(map[string]any)["id"])] = true
	}
	if !rows["desk-pc"] || rows["panel-box"] || rows["old-pc"] {
		t.Fatalf("the fixture: the viewer's rows are %v", rows)
	}
	power := pe.do("GET", "/api/power", nil, "Cookie", ck)
	for _, hidden := range []string{"ch6", "ch7"} {
		if s := pe.socketOf(ck, "reader/desk-bar", hidden); s != nil {
			t.Errorf("a viewer gets a socket that feeds a row they do not see: %v", s)
		}
	}
	for _, word := range []string{"Panel box", "Old PC", "panel-box", "old-pc"} {
		if strings.Contains(string(power.raw), word) {
			t.Errorf("the viewer's answer names %q", word)
		}
	}
	shown, left := 0, -1.0
	for _, x := range power.body["sources"].([]any) {
		if src := x.(map[string]any); src["id"] == "reader/desk-bar" {
			list, _ := src["sockets"].([]any)
			shown, left = len(list), 0
			if n, ok := src["sockets_hidden"].(float64); ok {
				left = n
			}
			for _, k := range list {
				id, _ := k.(map[string]any)["system_id"].(string)
				if id != "" && !rows[id] {
					t.Errorf("a socket in the viewer's answer feeds a row they do not see: %v", k)
				}
			}
		}
	}
	if shown+int(left) != 8 || left < 2 || shown < 5 {
		t.Errorf("the viewer's powerbar: %d sockets shown, %v left out; want 8 in all, ch6 and ch7 among the ones left out", shown, left)
	}
	// an admin's answer leaves nothing out and says so
	for _, x := range pe.do("GET", "/api/power", nil, "Cookie", pe.admin).body["sources"].([]any) {
		if src := x.(map[string]any); src["sockets_hidden"] != nil {
			t.Errorf("an admin's answer leaves sockets out: %v", src["id"])
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
		{socketLook{src: src, sock: sock, link: "desk-pc", shown: "desk-pc", row: off, knownOff: true}, "off", false, "switch off Left monitor (Monitor powerbar), which powers DESK-PC"},
		{socketLook{src: src, sock: sock, link: "desk-pc", shown: "desk-pc", row: pc}, "on", false, "switch on Left monitor (Monitor powerbar), which powers DESK-PC"},
		{socketLook{src: src, sock: sock, link: "desk-pc", shown: "desk-pc", row: pc}, "off", true, "cut the power of DESK-PC while it is running: switch off Left monitor (Monitor powerbar)"},
		// a machine that does not answer and is not known to be off, and one the panel sent to sleep: the words
		// of a cut say which it is
		{socketLook{src: src, sock: sock, link: "desk-pc", shown: "desk-pc", row: off}, "off", true, "cut the power of DESK-PC while it may still be running: switch off Left monitor (Monitor powerbar)"},
		{socketLook{src: src, sock: sock, link: "desk-pc", shown: "desk-pc", row: off}, "off", false, "switch off Left monitor (Monitor powerbar), which powers DESK-PC"},
		{socketLook{src: src, sock: sock, link: "desk-pc", shown: "desk-pc", row: &model.System{ID: "desk-pc", Name: "DESK-PC", Status: "offline", Asleep: true}}, "off", true,
			"cut the power of DESK-PC while it is asleep: switch off Left monitor (Monitor powerbar)"},
		{socketLook{src: src, sock: sock, link: "old-pc", shown: "old-pc"}, "off", true, "cut the power of old-pc while it may still be running: switch off Left monitor (Monitor powerbar)"},
	} {
		if got := plugWords(c.k, c.to, c.cut); got != c.want {
			t.Errorf("%q, want %q", got, c.want)
		}
	}
	// The dialog shows at most 200 bytes of a line (safeTarget), and cuts at the end. So the part that must be
	// read comes first, the machine and "while it is running", and the names are shortened inside the sentence:
	// with the longest names an agent can give (a socket's 48 bytes, a plug's 80) nothing is cut off the line
	// "Your passkey signs" or the line "Fairy will run" (review of stage 1a, finding 5).
	long := func(c string, n int) string { return strings.Repeat(c, n) }
	for name, c := range map[string]struct {
		sock, plug, machine string
		cut                 bool
		lead, tail          string
	}{
		"a cut, the longest names":        {long("S", 48), long("P", 80), "WORKSTATION-OF-THE-OFFICE-01", true, "cut the power of WORKSTATION-OF-THE-OFFICE-01 while it is running: switch off SSSS", ")"},
		"a cut, a very long machine name": {long("S", 48), long("P", 80), long("M", 150), true, "cut the power of " + long("M", 61) + "… while it is running: switch off SSSS", ")"},
		"off, the longest names":          {long("S", 48), long("P", 80), "WORKSTATION-OF-THE-OFFICE-01", false, "switch off SSSS", "), which powers WORKSTATION-OF-THE-OFFICE-01"},
		"names of two-byte letters":       {long("é", 24), long("ü", 40), long("ö", 60), true, "cut the power of " + long("ö", 30) + "… while it is running: switch off éééé", ")"},
		"a short plug and a long socket":  {long("S", 48), "Monitor powerbar", long("M", 64), true, "cut the power of " + long("M", 64) + " while it is running: switch off " + long("S", 34) + "… (Monitor powerbar)", ")"},
		"a long plug and a short socket":  {"ch1", long("P", 80), long("M", 64), true, "cut the power of " + long("M", 64) + " while it is running: switch off ch1 (" + long("P", 47) + "…)", ")"},
		"control characters in a name":    {"a\nb", "p\x1b[31m", "m\u202e", true, "cut the power of m%E2%80%AE while it is running: switch off a%0Ab (p%1B[31m)", ")"},
	} {
		k := socketLook{src: model.PowerSourceState{Name: c.plug}, sock: model.PowerSocket{Name: c.sock}, link: "desk-pc", shown: "desk-pc",
			row: &model.System{ID: "desk-pc", Name: c.machine, Status: map[bool]string{true: "online", false: "offline"}[c.cut]}}
		w := plugWords(k, "off", c.cut)
		signs := truncate(safeTarget(w), maxActionText)                               // approval.go: "Your passkey signs"
		action := truncate(safeTarget("switch a plug socket ("+w+")"), maxActionText) // pending.go: "Fairy will run"
		if signs != w || action != "switch a plug socket ("+w+")" || len(action) > maxTarget {
			t.Errorf("%s: the dialog cuts the words (%d bytes):\n%s\n%s", name, len(w), signs, action)
		}
		if !strings.HasPrefix(w, c.lead) || !strings.HasSuffix(w, c.tail) || !utf8.ValidString(w) {
			t.Errorf("%s: %q\nwant it to begin %q and to end %q", name, w, c.lead, c.tail)
		}
		if c.cut && strings.Index(w, "while it is running") > strings.Index(w, "switch off") {
			t.Errorf("%s: the names come before what is dangerous: %q", name, w)
		}
	}
	if got := requestWords(api.SocketSetRequest("desk-bar", "ch1", "off", plugTag("desk-bar", "ch1"))); got != "switch off the socket ch1 of desk-bar" {
		t.Errorf("the words made from the request alone: %q", got)
	}
	if _, ok := actionText["POST /api/power/socket"]; !ok {
		t.Error("the kept request has no description")
	}
	// every reason has a sentence of its own, and none of them is the fallback
	for _, why := range []string{model.SocketLockAgent, model.SocketLockHomeAssistant, model.SocketLockHAUnknown, model.SocketLockHAUnset, model.SocketLockPanel, model.SocketLockLinkUnknown, api.WhyFeedsSelf,
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
	// the fixture: desk-bar/ch7 is linked to old-pc, an id no row has: locked for that, and not as the panel's
	if s := pe.socketOf(pe.admin, "reader/desk-bar", "ch7"); s["lock"] != model.SocketLockLinkUnknown {
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
	bare.HomeAssistantSystem = "desk-pc" // its Home Assistant is not on this node (the rig's runs in a guest of it)
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

// A link that names no row the panel has (a typing mistake in socket_links, or a row that is not there yet)
// locks the socket, in both directions, as home_assistant_system does for the same mistake. Until the review
// of stage 1a (finding 3) such a socket had none of the panel's three locks: a mistyped id of Home Assistant's
// machine, of the reader's or of the panel's host was switched off with the wrong id typed and a passkey.
func TestPlugLinkToARowThePanelDoesNotHave(t *testing.T) {
	pe := newPlugEnv(t)
	const said = "Locked. This socket is linked to a machine the panel does not have: correct socket_links."
	s := pe.socketOf(pe.admin, "reader/desk-bar", "ch7") // socket_links: desk-bar/ch7: old-pc
	if s["switchable"] != false || s["lock"] != model.SocketLockLinkUnknown || s["why"] != model.SocketLockLinkUnknown || s["note"] != said || s["system_id"] != nil {
		t.Errorf("a socket that is linked to no row: %v", s)
	}
	try := func(pe *plugEnv, source, socket, said string, more map[string]any) {
		t.Helper()
		for _, to := range []string{"off", "on"} {
			b := plugBody(source, socket, to)
			if to == "off" {
				for k, v := range more {
					b[k] = v
				}
			}
			r := pe.post("/api/power/socket", b)
			if msg, _ := plugDetailOf(r); r.code != 409 || msg != said+" Nothing was switched." {
				t.Errorf("%s/%s %s %v: %d %s", source, socket, to, more, r.code, r.raw)
			}
			if au := pe.plugAudits()[0]; au["target"] != source+"/"+socket || au["outcome"] != "refused (link_unknown): "+said+" Nothing was switched." {
				t.Errorf("%s/%s %s: the audit entry: %v", source, socket, to, au)
			}
		}
	}
	try(pe, "reader/desk-bar", "ch7", said, nil)
	// what got "off" through before: "cut" with the id typed
	try(pe, "reader/desk-bar", "ch7", said, map[string]any{"cut": true, "confirm": "old-pc"})
	// the only socket of a plug takes the plug's link: a plug that is linked to no row is locked the same way,
	// and the sentence names the plug's link
	pe2 := newPlugEnvWith(t, plugConfigWith(t, "power_links: {pc-plug: desk-pc}", "power_links: {pc-plug: desk-pv}"))
	const said2 = "Locked. This plug is linked to a machine the panel does not have: correct its link (power_links, or Edit host)."
	if s := pe2.socketOf(pe2.admin, "reader/pc-plug", "main"); s["switchable"] != false || s["lock"] != model.SocketLockLinkUnknown || s["note"] != said2 {
		t.Errorf("the only socket of a plug that is linked to no row: %v", s)
	}
	try(pe2, "reader/pc-plug", "main", said2, nil)
	if n := len(pe.reader.Asked()) + len(pe2.reader.Asked()) + len(pe.iss.Asked()) + len(pe2.iss.Asked()); n != 0 {
		t.Errorf("a machine or Pocket ID was asked %d things for a locked socket", n)
	}
	// the more telling locks come first: the same link, once the id is in panel_hosts, is the panel's
	// (TestPlugLockOfThePanelsMachineWithoutARow), and a link to a row that is there is not locked for this
	if s := pe.socketOf(pe.admin, "reader/desk-bar", "ch3"); s["lock"] != nil || s["switchable"] != true {
		t.Errorf("a socket that is linked to a row: %v", s)
	}
}

// The case the second review of stage 1a asked for: the PC runs, and its agent has not answered for longer than
// offline_after (a link that dropped, an update, a hang). Its row is "offline", and that is no sign that the
// machine is off: "off" for its socket is refused as "may still be running", and goes on only with the
// machine's name typed, under a passkey line that says the cut.
func TestPlugOffForAMachineThatDoesNotAnswer(t *testing.T) {
	pe := newPlugEnv(t)
	pe.pcSilent()
	const said = "DESK-PC does not answer, which does not say that it is off: it may still be running. Shut it down first, or look at it."
	unknown := func(r resp, said string) {
		t.Helper()
		d, _ := r.body["detail"].(map[string]any)
		if r.code != 409 || d["code"] != "machine_running" || d["state"] != "unknown" || d["machine"] != "DESK-PC" || d["message"] != said || d["system_id"] != "desk-pc" {
			t.Fatalf("%d %s\nwant machine_running, state unknown: %q", r.code, r.raw, said)
		}
	}
	unknown(pe.flip("reader/desk-bar", "ch3", "off"), said)
	if au := pe.plugAudits()[0]; au["outcome"] != "refused (machine_running): "+said {
		t.Errorf("the audit entry: %v", au)
	}
	// however long it has not answered: an hour more changes nothing
	pe.s.Poller.Clock = func() time.Time { return time.Now().Add(70 * time.Minute) }
	pe.poll()
	unknown(pe.flip("reader/desk-bar", "ch3", "off"), said)
	if n := len(pe.reader.Asked()); n != 0 {
		t.Fatalf("the machine was asked %d things", n)
	}
	// "on" has no such question
	if _, _, ok := approvalAsked(pe.flip("reader/desk-bar", "ch3", "on")); !ok {
		t.Error("on is refused for a socket whose machine does not answer")
	}
	// Cut power anyway: the name typed, and the passkey line says what is cut and that it may be running
	cut := func(confirm string) resp {
		b := plugBody("reader/desk-bar", "ch3", "off")
		b["cut"], b["confirm"] = true, confirm
		return pe.post("/api/power/socket", b)
	}
	if r := cut("Desk PC"); r.code != 422 {
		t.Fatalf("a cut with the socket's name typed: %d %s", r.code, r.raw)
	}
	r := cut("DESK-PC")
	id, _, ok := approvalAsked(r)
	if !ok {
		t.Fatalf("a cut with the name typed: %d %s", r.code, r.raw)
	}
	const words = "cut the power of DESK-PC while it may still be running: switch off Desk PC (Monitor powerbar)"
	if d := r.body["detail"].(map[string]any); d["signs"] != words || d["action"] != "switch a plug socket ("+words+")" {
		t.Errorf("the dialog of the cut: signs %q, action %q", d["signs"], d["action"])
	}
	if cb := pe.approve(t, id); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if pr := pe.result(t); pr["ok"] != true {
		t.Fatalf("result: %v", pr)
	}
	if au := pe.plugAudits()[0]; au["outcome"] != "ok" || au["params"].(map[string]any)["cut"] != true {
		t.Errorf("the audit entry of the cut: %v", au)
	}
	if notes := pe.do("GET", "/api/notifications", nil, "Cookie", pe.admin); !strings.Contains(string(notes.raw), "power cut while DESK-PC may still have been running") {
		t.Errorf("the notification of the cut: %s", notes.raw)
	}

	// the PC's own plug (one socket, a meter of its own) still measures a load: the same answer, with the watts
	unknown(pe.flip("reader/pc-plug", "main", "off"), "DESK-PC does not answer, and its plug still measures 40 W: it may still be running. Shut it down first, or look at it.")
	// under 5 W in a fresh reading it is off: no question, and the passkey line names what the plug powers
	pe.measures("pc-plug", 2)
	r = pe.flip("reader/pc-plug", "main", "off")
	if _, _, ok := approvalAsked(r); !ok || r.body["detail"].(map[string]any)["signs"] != "switch off PC (PC plug), which powers DESK-PC" {
		t.Fatalf("off for a plug that measures 2 W: %d %s", r.code, r.raw)
	}
	// the powerbar has no meter for one socket: the PC's socket there still has the question
	unknown(pe.flip("reader/desk-bar", "ch3", "off"), said)

	// a machine that was seen to go off (a run of "Update unpowered hosts" tells the poller): no question on the
	// powerbar; and its own plug, back at 40 W, says the opposite and wins
	pe = newPlugEnv(t)
	pe.pcOff()
	if _, _, ok := approvalAsked(pe.flip("reader/desk-bar", "ch3", "off")); !ok {
		t.Error("off is refused for a socket whose machine was seen to go off")
	}
	unknown(pe.flip("reader/pc-plug", "main", "off"), "DESK-PC does not answer, and its plug still measures 40 W: it may still be running. Shut it down first, or look at it.")
}

// An agent whose entry has no home_assistant_system: the panel does not know which machine its Home Assistant
// runs on, so it cannot lock the socket that feeds it, and it switches NO socket of that agent, on or off,
// until the key is set. Before the review of stage 1a (finding 1) such an agent's sockets had no lock and no
// question at all: "off" for the powerbar socket of Home Assistant's box, of the reader's own node and of a
// running PC went to the passkey at once. A host added with Add host has no entry in config.yaml and cannot
// carry the key: its sockets are locked the same way, and the sentence says that.
func TestPlugAgentWithoutHomeAssistantSystem(t *testing.T) {
	pe := newPlugEnvWith(t, plugConfigWith(t, "desk-bar/ch7: old-pc},\n     home_assistant_system: lab-qemu-100}", "desk-bar/ch7: old-pc}}"))
	const said = "The panel does not know which machine runs Home Assistant for this plug. Set home_assistant_system for this agent in config.yaml."
	for _, c := range []struct{ source, socket, lock, note string }{
		{"reader/desk-bar", "ch1", model.SocketLockHAUnset, said}, // linked to nothing
		{"reader/desk-bar", "ch2", model.SocketLockHAUnset, said},
		{"reader/desk-bar", "ch3", model.SocketLockHAUnset, said}, // a PC that runs
		{"reader/desk-bar", "ch8", model.SocketLockHAUnset, said}, // not allowed on the machine either: the panel's reason comes first
		{"reader/pc-plug", "main", model.SocketLockHAUnset, said},
		// what the panel does know about a socket is still said first
		{"reader/desk-bar", "ch4", model.SocketLockAgent, "Locked. This socket powers the machine that reads the plugs."},
		// ch5 is linked to a VM (the one Home Assistant runs on, which the panel is not told). A link to a guest is
		// a link to its node, and that node is the reader's: until the second review of the fixes this socket had
		// the lock of the missing key
		{"reader/desk-bar", "ch5", model.SocketLockAgent, "Locked. This socket powers the machine that reads the plugs."},
		{"reader/own-plug", "main", model.SocketLockAgent, "Locked. This socket powers the machine that reads the plugs."},
		{"reader/desk-bar", "ch6", model.SocketLockPanel, "Locked. This socket powers the panel."},
		{"reader/desk-bar", "ch7", model.SocketLockLinkUnknown, "Locked. This socket is linked to a machine the panel does not have: correct socket_links."},
	} {
		s := pe.socketOf(pe.admin, c.source, c.socket)
		if s["switchable"] != false || s["lock"] != c.lock || s["why"] != c.lock || s["note"] != c.note {
			t.Errorf("%s/%s: %v\nwant the lock %s: %q", c.source, c.socket, s, c.lock, c.note)
		}
		for _, to := range []string{"off", "on"} {
			b := plugBody(c.source, c.socket, to)
			if to == "off" {
				b["cut"], b["confirm"] = true, "DESK-PC" // no way round it
			}
			r := pe.post("/api/power/socket", b)
			if msg, _ := plugDetailOf(r); r.code != 409 || msg != c.note+" Nothing was switched." {
				t.Errorf("%s/%s %s: %d %s", c.source, c.socket, to, r.code, r.raw)
			}
			if au := pe.plugAudits()[0]; au["target"] != c.source+"/"+c.socket || au["outcome"] != "refused ("+c.lock+"): "+c.note+" Nothing was switched." {
				t.Errorf("%s/%s %s: the audit entry: %v", c.source, c.socket, to, au)
			}
		}
	}
	if n := len(pe.reader.Asked()) + len(pe.iss.Asked()); n != 0 {
		t.Errorf("the machine or Pocket ID was asked %d things for sockets of an agent without home_assistant_system", n)
	}
	// an agent whose entry has the key is not touched by its neighbour's lack of it
	if s := pe.socketOf(pe.admin, "other/rack-bar", "fan"); s["switchable"] != true || s["lock"] != nil {
		t.Errorf("a socket of another agent: %v", s)
	}
	// somebody who is no admin gets no lock and no sentence, as for every socket
	if s := pe.socketOf(pe.viewerOf("Home"), "reader/desk-bar", "ch1"); s["lock"] != nil || s["note"] != nil || s["why"] != model.SocketReadOnly {
		t.Errorf("a viewer: %v", s)
	}

	// a host added with Add host (managed-hosts.yaml): no entry in config.yaml, so nowhere to write the key
	pe = newPlugEnv(t)
	pe.b.Mu.Lock()
	pe.b.Metrics["added"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["hostname"] = "added"
		m["power_sources"] = []any{jsonx.M{"id": "new-bar", "name": "New bar", "type": "homeassistant", "ok": true, "watts": 3.0, "sockets": []any{
			jsonx.M{"id": "ch1", "name": "Lamp", "state": "off", "switchable": true, "why": nil, "tag": plugTag("new-bar", "ch1"), "error": nil},
			jsonx.M{"id": "ch2", "name": "NAS", "state": "on", "switchable": true, "why": nil, "tag": plugTag("new-bar", "ch2"), "error": nil}}}}
		return m, nil
	}
	pe.b.Mu.Unlock()
	if err := pe.s.Poller.AddAgent(config.Agent{ID: "added", Name: "ADDED", URL: "http://added", Managed: true, Token: testutil.ReadToken + "x", ControlToken: testutil.ControlToken}, nil); err != nil {
		t.Fatal(err)
	}
	pe.poll()
	const added = "The panel does not know which machine runs Home Assistant for this plug. A machine added with Add host cannot be given that setting yet."
	for _, c := range []struct{ socket, to string }{{"ch1", "on"}, {"ch2", "off"}} {
		s := pe.socketOf(pe.admin, "added/new-bar", c.socket)
		if s == nil || s["switchable"] != false || s["lock"] != model.SocketLockHAUnset || s["note"] != added {
			t.Fatalf("a socket of a host added with Add host: %v", s)
		}
		r := pe.flip("added/new-bar", c.socket, c.to)
		if msg, _ := plugDetailOf(r); r.code != 409 || msg != added+" Nothing was switched." {
			t.Errorf("%s %s: %d %s", c.socket, c.to, r.code, r.raw)
		}
		if au := pe.plugAudits()[0]; au["outcome"] != "refused (home_assistant_unset): "+added+" Nothing was switched." {
			t.Errorf("the audit entry: %v", au)
		}
	}
	if n := len(pe.iss.Asked()); n != 0 {
		t.Errorf("Pocket ID was asked %d times", n)
	}
}

// A socket that is linked to nothing: the panel does not know what is plugged in. It stays switchable, but
// "off" is never sent on the strength of one click: the first request is refused with a word of its own
// (nothing_linked), the page asks "If a machine is plugged in here, it loses power now", and only the same
// request with cut goes on to the passkey, whose line then says the cut. No name is typed: there is none.
// "On" has no such question. Before the review of stage 1a (finding 1) "off" for such a socket went to the
// passkey at once, under the words "switch off Channel 4 (Monitor powerbar)".
func TestPlugOffForASocketThatIsLinkedToNothing(t *testing.T) {
	pe := newPlugEnvWith(t, plugConfigWith(t, "power_links: {pc-plug: desk-pc},", ""))
	const said = "Nothing is linked to this socket, so the panel does not know what is plugged in. If a machine is plugged in here, it loses power now. Nothing was switched."
	for _, c := range []struct{ source, socket, name, plug string }{
		{"reader/desk-bar", "ch1", "Left monitor", "Monitor powerbar"}, // a powerbar's socket without a line in socket_links
		{"reader/pc-plug", "main", "PC", "PC plug"},                    // the only socket of a plug that is linked to nothing
	} {
		if s := pe.socketOf(pe.admin, c.source, c.socket); s["switchable"] != true || s["lock"] != nil || s["system_id"] != nil {
			t.Fatalf("%s/%s: the fixture: %v", c.source, c.socket, s)
		}
		asked, passkeys := len(pe.reader.Asked()), len(pe.iss.Asked())
		r := pe.flip(c.source, c.socket, "off")
		d, _ := r.body["detail"].(map[string]any)
		if r.code != 409 || d["code"] != "nothing_linked" || d["message"] != said || d["machine"] != nil {
			t.Fatalf("%s/%s off: %d %s", c.source, c.socket, r.code, r.raw)
		}
		if au := pe.plugAudits()[0]; au["target"] != c.source+"/"+c.socket || au["outcome"] != "refused (nothing_linked): "+said || au["params"].(map[string]any)["cut"] != nil {
			t.Errorf("%s/%s: the audit entry of the refusal: %v", c.source, c.socket, au)
		}
		if len(pe.reader.Asked()) != asked || len(pe.iss.Asked()) != passkeys {
			t.Fatalf("%s/%s: the machine or Pocket ID was asked before the question was answered", c.source, c.socket)
		}
		// the answer: the same request with cut. No name is asked for, and one that is sent is not looked at
		b := plugBody(c.source, c.socket, "off")
		b["cut"] = true
		r = pe.post("/api/power/socket", b)
		id, _, ok := approvalAsked(r)
		if !ok {
			t.Fatalf("%s/%s off with cut: %d %s", c.source, c.socket, r.code, r.raw)
		}
		words := "cut the power of whatever is plugged in: switch off " + c.name + " (" + c.plug + ")"
		if d := r.body["detail"].(map[string]any); d["signs"] != words || d["action"] != "switch a plug socket ("+words+")" {
			t.Errorf("%s/%s: the dialog of the cut: signs %q, action %q", c.source, c.socket, d["signs"], d["action"])
		}
		sent := len(plugSent(pe.reader))
		if cb := pe.approve(t, id); cb.code != 303 {
			t.Fatal(cb.code)
		}
		if pr := pe.result(t); pr["ok"] != true || pr["message"] != c.name+" is off." {
			t.Fatalf("%s/%s: result: %v", c.source, c.socket, pr)
		}
		got := plugSent(pe.reader)
		if len(got) != sent+1 || got[sent].Body["cut"] != nil || got[sent].Body["confirm"] != nil || got[sent].Body["to"] != "off" {
			t.Fatalf("%s/%s: what the machine got: %+v", c.source, c.socket, got)
		}
		au := pe.plugAudits()[0]
		if p := au["params"].(map[string]any); au["outcome"] != "ok" || p["cut"] != true || p["linked"] != nil || p["approval"] != "sent" {
			t.Errorf("%s/%s: the audit entry of the switch: %v", c.source, c.socket, au)
		}
	}
	if notes := pe.do("GET", "/api/notifications", nil, "Cookie", pe.admin); !strings.Contains(string(notes.raw), "Left monitor (Monitor powerbar) switched off by "+plugActor+": power cut with nothing linked to the socket") {
		t.Errorf("the notification: %s", notes.raw)
	}
	// "on" is not asked about
	if _, _, ok := approvalAsked(pe.flip("reader/desk-bar", "ch2", "on")); !ok {
		t.Error("on for a socket that is linked to nothing is refused")
	}
	// and the question is asked again every time, also right after it was answered once
	pe.set("reader/desk-bar/ch2", "state", "on")
	if r := pe.flip("reader/desk-bar", "ch2", "off"); r.code != 409 || !strings.Contains(string(r.raw), `"code":"nothing_linked"`) {
		t.Errorf("a second socket: %d %s", r.code, r.raw)
	}
	// a socket that is linked has its own rules, not this one: a machine that runs is named
	if r := pe.flip("reader/desk-bar", "ch3", "off"); !strings.Contains(string(r.raw), `"code":"machine_running"`) {
		t.Errorf("a linked socket: %d %s", r.code, r.raw)
	}
}

// What the passkey was asked for is what is switched. The kept request is replayed by the panel after the round
// trip to Pocket ID, with the machine as it is THEN; when the words the dialog would show now are not the words
// it showed, nothing is sent, the approval is not used, and the result says that the situation changed (second
// review of stage 1a: a request that carried "cut" ahead was kept for a machine that was off, the machine was
// started meanwhile, and the replay cut its power under a passkey line that did not say "cut").
func TestPlugReplayRefusesWhatWasNotSigned(t *testing.T) {
	changed := func(t *testing.T, pe *plugEnv, id, signed, now string) {
		t.Helper()
		if cb := pe.approve(t, id); cb.code != 303 {
			t.Fatal(cb.code)
		}
		pr := pe.result(t)
		want := "This changed while you confirmed. Your passkey was asked for \"" + signed + "\", and it would now be \"" + now + "\". Nothing was switched. Look again."
		if pr["ok"] != false || pr["status"] != 409.0 || pr["code"] != "situation_changed" || pr["message"] != want {
			t.Fatalf("result: %v\nwant %q", pr, want)
		}
		if n := len(plugSent(pe.reader)); n != 0 {
			t.Fatalf("the machine got %d switch requests", n)
		}
		au := pe.plugAudits()[0]
		if p := au["params"].(map[string]any); au["outcome"] != "refused (situation_changed): "+want || p["approval"] != nil {
			t.Errorf("the audit entry: %v", au)
		}
	}
	const off, cut, maybe = "switch off Desk PC (Monitor powerbar), which powers DESK-PC",
		"cut the power of DESK-PC while it is running: switch off Desk PC (Monitor powerbar)",
		"cut the power of DESK-PC while it may still be running: switch off Desk PC (Monitor powerbar)"

	// 1. the review's case. The machine is off; a request with "cut" ahead cannot be kept any more (3), so the
	// kept request is given it here, as if it had been. The machine is started; the replay would cut its power.
	t.Run("the machine was started, and the kept request says cut", func(t *testing.T) {
		pe := newPlugEnv(t)
		pe.pcOff()
		r := pe.flip("reader/desk-bar", "ch3", "off")
		id, _, ok := approvalAsked(r)
		if !ok || r.body["detail"].(map[string]any)["signs"] != off {
			t.Fatalf("the first request: %d %s", r.code, r.raw)
		}
		l := pe.s.codec.live
		l.mu.Lock()
		p := l.m[sidOf(pe.env, pe.admin)].pending
		body := strings.Replace(p.Body, `"to":"off"`, `"to":"off","cut":true,"confirm":"DESK-PC"`, 1)
		if body == p.Body {
			l.mu.Unlock()
			t.Fatal("the test did not change the kept body:", p.Body)
		}
		p.Body = body
		l.mu.Unlock()
		pe.pcIs("online")
		changed(t, pe, id, off, cut)
	})
	// 2. a cut that was signed for a machine that "may still be running": it answers again meanwhile. The words
	// differ, so the person is asked again with the ones that are true now.
	t.Run("the machine answers again after a cut was signed for one that may be running", func(t *testing.T) {
		pe := newPlugEnv(t)
		pe.pcSilent()
		b := plugBody("reader/desk-bar", "ch3", "off")
		b["cut"], b["confirm"] = true, "DESK-PC"
		r := pe.post("/api/power/socket", b)
		id, _, ok := approvalAsked(r)
		if !ok || r.body["detail"].(map[string]any)["signs"] != maybe {
			t.Fatalf("the first request: %d %s", r.code, r.raw)
		}
		pe.pcIs("online")
		changed(t, pe, id, maybe, cut)
	})
	// 3. "cut" where the panel asks no question is not taken as a plain switch, and so cannot be kept ahead of
	// one: the machine on the socket counts as off. Nobody is asked for a passkey.
	t.Run("cut where nothing asks for it", func(t *testing.T) {
		pe := newPlugEnv(t)
		pe.pcOff()
		b := plugBody("reader/desk-bar", "ch3", "off")
		b["cut"], b["confirm"] = true, "DESK-PC"
		r := pe.post("/api/power/socket", b)
		d, _ := r.body["detail"].(map[string]any)
		const want = "\"Cut power anyway\" answers a question the panel does not ask for this socket now: DESK-PC counts as off. Nothing was switched. Look again."
		if r.code != 409 || d["code"] != "situation_changed" || d["message"] != want {
			t.Fatalf("%d %s", r.code, r.raw)
		}
		if au := pe.plugAudits()[0]; au["outcome"] != "refused (situation_changed): "+want {
			t.Errorf("the audit entry: %v", au)
		}
		if n := len(pe.reader.Asked()) + len(pe.iss.Asked()); n != 0 {
			t.Errorf("the machine or Pocket ID was asked %d things", n)
		}
	})
	// 4. nothing changed: the same words, and the switch is sent (a cut, the longest way through the route)
	t.Run("nothing changed", func(t *testing.T) {
		pe := newPlugEnv(t)
		b := plugBody("reader/desk-bar", "ch3", "off")
		b["cut"], b["confirm"] = true, "DESK-PC"
		id, _, ok := approvalAsked(pe.post("/api/power/socket", b))
		if !ok {
			t.Fatal("not asked")
		}
		pe.poll() // the panel looks again in between, and sees the same
		if cb := pe.approve(t, id); cb.code != 303 {
			t.Fatal(cb.code)
		}
		if pr := pe.result(t); pr["ok"] != true || pr["code"] != nil || len(plugSent(pe.reader)) != 1 {
			t.Fatalf("result: %v", pr)
		}
	})
}
