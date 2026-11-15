package web

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Tests for rules of the socket route that the first review of stage 1a found right in the code and asked by
// no test: seven of its hand mutations survived the whole suite. Each test here names the mutation it is for
// (R03 ... R17 in the review's table). R10, the limit on control requests, is asked by
// TestPlugSwitchPassesTheControlLimitFirst.

// R03: the name that is typed for "Cut power anyway" is the MACHINE's, the one the dialog's title shows. Not
// the socket's own name (which often says the same thing in other words: "Desk PC" for DESK-PC), not the
// plug's, and not the row's id. The rig's PC is called DESK-PC under the id desk-pc, so that typing the id
// passed as the name without its case; here it has a name that is not its id.
func TestPlugCutTakesOnlyTheMachinesName(t *testing.T) {
	pe := newPlugEnvWith(t, plugConfigWith(t, "{id: desk-pc, name: DESK-PC,", "{id: desk-pc, name: Tower,"))
	cut := func(confirm string) resp {
		b := plugBody("reader/desk-bar", "ch3", "off")
		b["cut"], b["confirm"] = true, confirm
		return pe.post("/api/power/socket", b)
	}
	if d, _ := pe.flip("reader/desk-bar", "ch3", "off").body["detail"].(map[string]any); d["machine"] != "Tower" || d["code"] != "machine_running" {
		t.Fatalf("the fixture: %v", d)
	}
	for _, typed := range []string{"Desk PC", "desk pc", "desk-pc", "DESK-PC", "Monitor powerbar", "ch3", "reader/desk-bar/ch3", "Towe", "Tower 2", "READER"} {
		if r := cut(typed); r.code != 422 || r.body["detail"] != "Type Tower to confirm cutting its power. Nothing was switched." {
			t.Errorf("cut with %q typed: %d %s", typed, r.code, r.raw)
		}
	}
	if n := len(pe.reader.Asked()); n != 0 {
		t.Fatalf("the machine was asked %d things by cuts with a wrong name", n)
	}
	if _, _, ok := approvalAsked(cut("tower")); !ok {
		t.Error("the machine's name, typed, is refused")
	}
}

// R04: a lock does not depend on whether the locked machine runs. The socket of Home Assistant's machine, of
// its node and of the panel's host stay locked, on and off, while that machine is off by everything the panel
// knows: a guest Proxmox says is stopped, a host that was seen to go off. (They are the cases in which "off"
// would otherwise need no question at all, and "on" is the one thing that brings Home Assistant back: by
// hand, at the plug.) A machine that is off is the usual state of a locked socket's row after a power cut.
func TestPlugLocksHoldWhileTheLockedMachineIsOff(t *testing.T) {
	pe := newPlugEnv(t)
	// Home Assistant's VM is stopped, and the panel's host went off and was seen to
	pe.b.Mu.Lock()
	pe.b.PVEOf = func(string) ([]jsonx.M, error) { return testutil.HostResources("PVE", "stopped"), nil }
	pe.b.Metrics["panel-box"] = func() (jsonx.M, error) { return nil, fmt.Errorf("timeout") }
	pe.b.Mu.Unlock()
	pe.poll()
	pe.poll()
	pe.s.Poller.MarkOff("panel-box")
	rows := map[string]string{}
	for _, x := range pe.do("GET", "/api/systems", nil, "Cookie", pe.admin).body["systems"].([]any) {
		rows[fmt.Sprint(x.(map[string]any)["id"])] = fmt.Sprint(x.(map[string]any)["status"])
	}
	if rows["lab-qemu-100"] != "stopped" || rows["panel-box"] != "offline" || !pe.s.Poller.KnownOff("panel-box") {
		t.Fatalf("the fixture: Home Assistant's VM is %q, the panel's host %q", rows["lab-qemu-100"], rows["panel-box"])
	}
	for _, c := range []struct{ source, socket, lock, said string }{
		{"reader/desk-bar", "ch5", model.SocketLockHomeAssistant, "Locked. This socket powers Home Assistant."},
		{"other/rack-bar", "node", model.SocketLockHomeAssistant, "Locked. This socket powers Home Assistant."},
		{"reader/desk-bar", "ch6", model.SocketLockPanel, "Locked. This socket powers the panel."},
	} {
		if s := pe.socketOf(pe.admin, c.source, c.socket); s["switchable"] != false || s["lock"] != c.lock || s["note"] != c.said {
			t.Errorf("%s/%s while its machine is off: %v", c.source, c.socket, s)
		}
		for _, to := range []string{"off", "on"} {
			b := plugBody(c.source, c.socket, to)
			r := pe.post("/api/power/socket", b)
			if msg, _ := plugDetailOf(r); r.code != 409 || msg != c.said+" Nothing was switched." {
				t.Errorf("%s/%s %s while its machine is off: %d %s", c.source, c.socket, to, r.code, r.raw)
			}
		}
	}
	if n := len(pe.reader.Asked()) + len(pe.other.Asked()) + len(pe.iss.Asked()); n != 0 {
		t.Errorf("a machine or Pocket ID was asked %d things for locked sockets", n)
	}
}

// R05: the panel's locks are looked at again in the replay after the passkey, like everything else before the
// approval (PLUGS-DESIGN.md 4.4, step 9). A socket that became locked while the person was at Pocket ID is not
// switched: here the machine on it is named as the panel's host meanwhile. The words of the switch are the
// same before and after ("on" for a socket of a machine), so only the lock stands in the way.
func TestPlugLocksAreLookedAtAgainAfterTheApproval(t *testing.T) {
	pe := newPlugEnv(t)
	r := pe.flip("reader/desk-bar", "ch3", "on")
	id, _, ok := approvalAsked(r)
	if !ok {
		t.Fatalf("the first request: %d %s", r.code, r.raw)
	}
	pe.s.Cfg.PanelHosts = append(slices.Clone(pe.s.Cfg.PanelHosts), "desk-pc")
	if s := pe.socketOf(pe.admin, "reader/desk-bar", "ch3"); s["lock"] != model.SocketLockPanel {
		t.Fatalf("the fixture: the socket is not locked after the change: %v", s)
	}
	if cb := pe.approve(t, id); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if pr := pe.result(t); pr["ok"] != false || pr["status"] != 409.0 || pr["message"] != "Locked. This socket powers the panel. Nothing was switched." {
		t.Fatalf("the replay for a socket that is locked now: %v", pr)
	}
	if n := len(plugSent(pe.reader)); n != 0 {
		t.Fatalf("the machine got %d switch requests", n)
	}
	if au := pe.plugAudits()[0]; au["outcome"] != "refused (panel): Locked. This socket powers the panel. Nothing was switched." {
		t.Errorf("the audit entry: %v", au)
	}
}

// R07: a switch request has six members and no other, and none of them is a time. How long the panel waits for
// the machine is a constant of the panel (plugSwitchWait); a body cannot set it, because there is nothing in a
// body that could. A new member of the request has to be put into this list, by somebody who looked at it.
func TestPlugRequestHasNoMemberButItsSix(t *testing.T) {
	var got []string
	typ := reflect.TypeOf(plugSwitchReq{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		got = append(got, name+" "+f.Type.String())
	}
	if want := []string{"source string", "socket string", "to string", "tag string", "cut bool", "confirm string"}; !slices.Equal(got, want) {
		t.Errorf("the members of a switch request: %v\nwant %v", got, want)
	}
	pe := newPlugEnv(t)
	for _, member := range []string{"wait_s", "wait", "timeout", "timeout_s", "within_s", "delay_s", "deadline"} {
		for _, v := range []any{600, "600", 600.5} {
			b := plugBody("reader/desk-bar", "ch2", "on")
			b[member] = v
			if r := pe.post("/api/power/socket", b); r.code != 422 {
				t.Errorf("a body with %s: %v: %d %s", member, v, r.code, r.raw)
			}
		}
	}
	if n := len(pe.reader.Asked()); n != 0 {
		t.Errorf("the machine was asked %d things", n)
	}
	// and the wait the machine's request gets is the panel's own, 25 s
	id, _, ok := approvalAsked(pe.flip("reader/desk-bar", "ch2", "on"))
	if !ok {
		t.Fatal("not asked")
	}
	pe.approve(t, id)
	if sent := plugSent(pe.reader); len(sent) != 1 || sent[0].Wait != 25*time.Second {
		t.Errorf("what the machine got: %+v", sent)
	}
}

// R14: only an admin gets what a switch needs. Seeing every row is not being an admin: an API token with read
// scope and no list of sites sees every site (the grant "viewer of all", access.Grant.All), and it gets no tag,
// no reason, no lock and nothing switchable, whoever owns it. It does get every socket: nothing is left out
// for somebody who sees every row. (Users have no such grant: a viewer has a list of sites.)
func TestPlugSocketsForAReadTokenThatSeesEverySite(t *testing.T) {
	pe := newPlugEnv(t)
	for _, owner := range []string{"system", "u2"} { // the panel's own, and the admin of these tests
		tok, _, err := pe.s.Tokens.Create("t-read-"+owner, owner, "read", "cli:test", 0)
		if err != nil {
			t.Fatal(err)
		}
		who := "a read token of " + owner
		r := pe.do("GET", "/api/power", nil, "Authorization", "Bearer "+tok)
		if r.code != 200 {
			t.Fatalf("%s: GET /api/power: %d %s", who, r.code, r.raw)
		}
		sockets := 0
		for _, x := range r.body["sources"].([]any) {
			src := x.(map[string]any)
			if src["sockets_hidden"] != nil {
				t.Errorf("%s: %v leaves sockets out", who, src["id"])
			}
			list, _ := src["sockets"].([]any)
			for _, k := range list {
				s := k.(map[string]any)
				sockets++
				if s["switchable"] != false || s["why"] != model.SocketReadOnly || s["tag"] != nil || s["note"] != nil || s["lock"] != nil {
					t.Errorf("%s: %v/%v: %v", who, src["id"], s["id"], s)
				}
			}
		}
		if sockets != 8+1+1+2+2+1 {
			t.Errorf("%s gets %d sockets, want all 15 of the rig", who, sockets)
		}
		raw, _ := json.Marshal(r.body)
		for _, word := range []string{`"tag"`, `"note"`, `"lock"`, `"switchable":true`, "Locked.", plugTag("desk-bar", "ch1")} {
			if strings.Contains(string(raw), word) {
				t.Errorf("%s: the answer has %s", who, word)
			}
		}
		// a row it sees is named for a socket that feeds it, as for every viewer
		if s := pe.socketIn(r, "reader/desk-bar", "ch6"); s == nil || s["system_id"] != "panel-box" {
			t.Errorf("%s: the socket of a row it sees: %v", who, s)
		}
		if r := pe.do("POST", "/api/power/socket", plugBody("reader/desk-bar", "ch1", "off"), "Authorization", "Bearer "+tok, "Content-Type", "application/json"); r.code != 403 {
			t.Errorf("%s posts a switch: %d", who, r.code)
		}
	}
	if n := len(pe.reader.Asked()); n != 0 {
		t.Errorf("the machine was asked %d things", n)
	}
}

// R17: the typed name is compared for every kind of "not off", also for a machine the panel sent to sleep (and
// for one that does not answer: TestPlugOffForAMachineThatDoesNotAnswer). A machine that sleeps still holds
// what is open on it; the words of its cut say that it is asleep.
func TestPlugCutOfAMachineThatIsAsleepNeedsItsName(t *testing.T) {
	pe := newPlugEnv(t)
	at := time.Now().Add(-10 * time.Second)
	pe.s.Poller.NoteSleep("desk-pc", &at)
	pe.pcIs("down")
	r := pe.flip("reader/desk-bar", "ch3", "off")
	d, _ := r.body["detail"].(map[string]any)
	if r.code != 409 || d["code"] != "machine_running" || d["state"] != "asleep" ||
		d["message"] != "DESK-PC is asleep, not shut down: what is open on it is lost when its power is cut. Shut it down first." {
		t.Fatalf("off for the socket of a machine that sleeps: %d %s", r.code, r.raw)
	}
	cut := func(confirm string) resp {
		b := plugBody("reader/desk-bar", "ch3", "off")
		b["cut"], b["confirm"] = true, confirm
		return pe.post("/api/power/socket", b)
	}
	for _, typed := range []string{"", "x", "Desk PC", "READER"} {
		if r := cut(typed); r.code != 422 || r.body["detail"] != "Type DESK-PC to confirm cutting its power. Nothing was switched." {
			t.Errorf("cut of a sleeping machine with %q typed: %d %s", typed, r.code, r.raw)
		}
	}
	if n := len(pe.reader.Asked()); n != 0 {
		t.Fatalf("the machine was asked %d things", n)
	}
	r = cut("DESK-PC")
	if _, _, ok := approvalAsked(r); !ok || r.body["detail"].(map[string]any)["signs"] != "cut the power of DESK-PC while it is asleep: switch off Desk PC (Monitor powerbar)" {
		t.Errorf("the cut with the name typed: %d %s", r.code, r.raw)
	}
}
