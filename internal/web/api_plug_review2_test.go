package web

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Tests for the second review of the fixes of stage 1a (its findings 1 to 6, and the mutations of its own that
// survived the suite). Each test names the finding it is for.

// Finding 3: a meter that reads below zero is no sign that the machine is off. The PC does not answer, and its
// own plug reads -30 W (a clamp that was put on backwards reads so for a PC that draws 30 W): "off" asked no
// question and went to the passkey. Only a reading from 0 W to under 5 W lets "off" through.
func TestPlugMeterBelowZeroIsNoSignOfOff(t *testing.T) {
	pe := newPlugEnv(t)
	pe.pcSilent()
	for _, c := range []struct {
		watts float64
		asks  bool
		said  string
	}{
		{-30, true, "DESK-PC does not answer, and its plug reads -30 W, which is no sign that it is off: it may still be running. Shut it down first, or look at it."},
		{-0.5, true, "DESK-PC does not answer, and its plug reads -0.5 W, which is no sign that it is off: it may still be running. Shut it down first, or look at it."},
		{0, false, ""},
		{4.99, false, ""},
		{5, true, "DESK-PC does not answer, and its plug still measures 5 W: it may still be running. Shut it down first, or look at it."},
	} {
		pe.measures("pc-plug", c.watts)
		r := pe.flip("reader/pc-plug", "main", "off")
		d, _ := r.body["detail"].(map[string]any)
		if _, _, passkey := approvalAsked(r); passkey == c.asks {
			t.Errorf("%v W: the passkey step came at once: %v (%d %s)", c.watts, passkey, r.code, r.raw)
			continue
		}
		if c.asks && (r.code != 409 || d["code"] != codeMachineRunning || d["state"] != "unknown" || d["machine"] != "DESK-PC" || d["message"] != c.said) {
			t.Errorf("%v W: %d %s\nwant machine_running, state unknown: %q", c.watts, r.code, r.raw, c.said)
		}
	}
	if n := len(plugSent(pe.reader)); n != 0 {
		t.Errorf("the machine got %d switch requests", n)
	}
}

// withGuests gives the rig's Proxmox a second node, PVE2, that carries nothing of the panel's (no agent that
// reads plugs, no Home Assistant, not the panel), and four more guests: spare-vm (lab-qemu-101) on PVE, the
// node that carries the reader and Home Assistant's VM; nas-vm (lab-qemu-201) and panel-vm (lab-qemu-202) on
// PVE2; lost-vm (lab-qemu-301) on a node the answer does not have. Each guest has the given status ("stopped", "running").
func (pe *plugEnv) withGuests(status string) {
	pe.t.Helper()
	guest := func(vmid float64, name, node string) jsonx.M {
		return jsonx.M{"id": "qemu/" + name, "type": "qemu", "vmid": vmid, "name": name, "node": node, "status": status,
			"cpu": 0.0, "maxcpu": 2.0, "mem": 0.0, "maxmem": float64(4 << 30), "disk": 0.0, "maxdisk": float64(32 << 30), "template": 0.0}
	}
	pe.b.Mu.Lock()
	pe.b.PVEOf = func(string) ([]jsonx.M, error) {
		r := append(testutil.HostResources("PVE", "running"), jsonx.M{"id": "node/PVE2", "type": "node", "node": "PVE2", "status": "online", "cpu": 0.1,
			"maxcpu": 8.0, "mem": float64(8 << 30), "maxmem": float64(32 << 30), "disk": float64(10 << 30), "maxdisk": float64(100 << 30), "uptime": 30.0})
		return append(r, guest(101, "spare-vm", "PVE"), guest(201, "nas-vm", "PVE2"), guest(202, "panel-vm", "PVE2"), guest(301, "lost-vm", "GHOST")), nil
	}
	pe.b.Mu.Unlock()
	pe.poll()
	pe.poll()
	want := "stopped"
	if status == "running" {
		want = "online"
	}
	for id, parent := range map[string]string{"lab-qemu-101": "lab-node-pve", "lab-qemu-201": "lab-node-pve2", "lab-qemu-301": "lab-node-ghost"} {
		x := pe.do("GET", "/api/systems/"+id, nil, "Cookie", pe.admin)
		if x.body["kind"] != "qemu" || x.body["status"] != want || x.body["parent_id"] != parent {
			pe.t.Fatalf("the fixture: %s is %v", id, x.body)
		}
	}
	if x := pe.do("GET", "/api/systems/lab-node-pve2", nil, "Cookie", pe.admin); x.body["kind"] != "node" || x.body["status"] != "online" {
		pe.t.Fatalf("the fixture: PVE2 is %v", x.body)
	}
	if x := pe.do("GET", "/api/systems/lab-node-ghost", nil, "Cookie", pe.admin); x.code != 404 {
		pe.t.Fatalf("the fixture: the node of lost-vm has a row: %d", x.code)
	}
}

// Finding 1: a socket feeds a machine, and a guest (a VM, a container) is not one: what a socket that is
// linked to a guest really feeds is the guest's node. The panel looked at the guest. With
// `socket_links: {desk-bar/ch7: lab-qemu-101}`, the guest stopped and its node online, carrying Home
// Assistant's VM and the agent that reads the plugs, the socket had no lock and "off" went to the passkey at
// once ("which powers spare-vm"); with the guest running, a cut needed the guest's name. Now a link to a guest
// is a link to its node, for every rule: here the node is the machine that reads the plugs, so the socket is
// locked, in both directions, whatever the guest does.
func TestPlugSocketLinkedToAGuestOfTheReadersNode(t *testing.T) {
	for _, status := range []string{"stopped", "running"} {
		pe := newPlugEnvWith(t, plugConfigWith(t, "desk-bar/ch7: old-pc", "desk-bar/ch7: lab-qemu-101"))
		pe.withGuests(status)
		const said = "Locked. This socket powers the machine that reads the plugs."
		s := pe.socketOf(pe.admin, "reader/desk-bar", "ch7")
		if s["switchable"] != false || s["lock"] != model.SocketLockAgent || s["why"] != model.SocketLockAgent || s["note"] != said || s["system_id"] != "lab-node-pve" {
			t.Errorf("guest %s: the socket for an admin: %v", status, s)
		}
		cut := plugBody("reader/desk-bar", "ch7", "off")
		cut["cut"], cut["confirm"] = true, "spare-vm"
		cutNode := plugBody("reader/desk-bar", "ch7", "off")
		cutNode["cut"], cutNode["confirm"] = true, "PVE"
		for what, body := range map[string]map[string]any{"off": plugBody("reader/desk-bar", "ch7", "off"), "on": plugBody("reader/desk-bar", "ch7", "on"),
			"a cut with the guest's name": cut, "a cut with the node's name": cutNode} {
			r := pe.post("/api/power/socket", body)
			if msg, _ := plugDetailOf(r); r.code != 409 || msg != said+" Nothing was switched." {
				t.Errorf("guest %s, %s: %d %s", status, what, r.code, r.raw)
			}
		}
		if au := pe.plugAudits()[0]; !strings.HasPrefix(au["outcome"].(string), "refused ("+model.SocketLockAgent+")") || au["params"].(map[string]any)["linked"] != "lab-node-pve" ||
			au["params"].(map[string]any)["guest"] != "lab-qemu-101" {
			t.Errorf("guest %s: the audit entry: %v", status, au)
		}
		if n := len(pe.reader.Asked()); n != 0 {
			t.Errorf("guest %s: the machine was asked %d things for a locked socket", status, n)
		}
	}
}

// Finding 1, the other half: a guest that Proxmox calls stopped says nothing about its node. A socket linked to
// a stopped guest on a node that carries nothing of the panel's and is online: "off" asks, the machine in the
// question is the NODE (its name is the one to type, and the one the passkey signs), and the sentence says
// which guest the link named. "Proxmox says the guest is stopped" was a sign that the machine on the socket is
// off; it is none any more.
func TestPlugOffForASocketLinkedToAGuestAsksAboutItsNode(t *testing.T) {
	for _, status := range []string{"stopped", "running"} {
		pe := newPlugEnvWith(t, plugConfigWith(t, "desk-bar/ch7: old-pc", "desk-bar/ch7: lab-qemu-201"))
		pe.withGuests(status)
		guests := "" // the node's sentence counts its guests that run
		if status == "running" {
			guests = ", with 2 guests running on it"
		}
		said := "This socket is linked to nas-vm, a guest of PVE2, and what it feeds is PVE2. PVE2 is still running" + guests + ". Shut it down first."
		s := pe.socketOf(pe.admin, "reader/desk-bar", "ch7")
		if s["switchable"] != true || s["lock"] != nil || s["system_id"] != "lab-node-pve2" {
			t.Fatalf("guest %s: the socket for an admin: %v", status, s)
		}
		r := pe.flip("reader/desk-bar", "ch7", "off")
		d, _ := r.body["detail"].(map[string]any)
		if r.code != 409 || d["code"] != codeMachineRunning || d["state"] != "running" || d["machine"] != "PVE2" || d["system_id"] != "lab-node-pve2" || d["guest"] != "nas-vm" || d["message"] != said {
			t.Fatalf("guest %s: off: %d %s\nwant machine_running for PVE2: %q", status, r.code, r.raw, said)
		}
		cut := func(confirm string) resp {
			b := plugBody("reader/desk-bar", "ch7", "off")
			b["cut"], b["confirm"] = true, confirm
			return pe.post("/api/power/socket", b)
		}
		if r := cut("nas-vm"); r.code != 422 || !strings.Contains(string(r.raw), "Type PVE2 to confirm") {
			t.Fatalf("guest %s: a cut with the guest's name typed: %d %s", status, r.code, r.raw)
		}
		if n := len(pe.reader.Asked()); n != 0 {
			t.Fatalf("guest %s: the machine was asked %d things before the node's name was typed", status, n)
		}
		r = cut("PVE2")
		id, _, ok := approvalAsked(r)
		const words = "cut the power of PVE2 while it is running: switch off Old PC (Monitor powerbar)"
		if !ok || r.body["detail"].(map[string]any)["signs"] != words {
			t.Fatalf("guest %s: a cut with the node's name typed: %d %s", status, r.code, r.raw)
		}
		if cb := pe.approve(t, id); cb.code != 303 {
			t.Fatal(cb.code)
		}
		if pr := pe.result(t); pr["ok"] != true || len(plugSent(pe.reader)) != 1 {
			t.Fatalf("guest %s: result %v, %d switch requests", status, pr, len(plugSent(pe.reader)))
		}
		au := pe.plugAudits()[0]
		if p := au["params"].(map[string]any); au["outcome"] != "ok" || p["cut"] != true || p["linked"] != "lab-node-pve2" || p["guest"] != "lab-qemu-201" {
			t.Errorf("guest %s: the audit entry of the cut: %v", status, au)
		}
		// "on" has no question; its words name the node
		r = pe.flip("reader/desk-bar", "ch7", "on")
		if _, _, ok := approvalAsked(r); !ok || r.body["detail"].(map[string]any)["signs"] != "switch on Old PC (Monitor powerbar), which powers PVE2" {
			t.Errorf("guest %s: on: %d %s", status, r.code, r.raw)
		}
	}
}

// Finding 1: a guest whose node the panel cannot find is locked like a link that names no row: which machine
// the socket feeds is then not known. And the rule for the panel's own machine reaches a socket that is linked
// to a guest: when the panel runs in another guest of that node, the socket feeds the panel.
func TestPlugSocketLinkedToAGuestWithoutItsNodeAndBesideThePanel(t *testing.T) {
	pe := newPlugEnvWith(t, plugConfigWith(t, "desk-bar/ch7: old-pc", "desk-bar/ch7: lab-qemu-301"))
	pe.withGuests("stopped")
	const said = "Locked. This socket is linked to lost-vm, a virtual machine or container, and the panel does not have the Proxmox node it runs on. A socket feeds the node: link it to the node."
	s := pe.socketOf(pe.admin, "reader/desk-bar", "ch7")
	if s["switchable"] != false || s["lock"] != model.SocketLockLinkUnknown || s["note"] != said || s["system_id"] != nil {
		t.Errorf("a guest without its node: %v", s)
	}
	for _, to := range []string{"off", "on"} {
		r := pe.flip("reader/desk-bar", "ch7", to)
		if msg, _ := plugDetailOf(r); r.code != 409 || msg != said+" Nothing was switched." {
			t.Errorf("%s: %d %s", to, r.code, r.raw)
		}
	}
	// The panel's own machine. bare reads bare-bar, its Home Assistant is elsewhere, and its one socket is linked
	// to nas-vm, a guest of PVE2. While the panel runs somewhere else the socket has none of the panel's locks;
	// when the panel runs in another guest of PVE2 (panel_hosts names that guest), the socket feeds the panel.
	yaml := strings.Replace(plugConfigWith(t, "desk-bar/ch7: old-pc", "desk-bar/ch7: lab-qemu-301"),
		"token_env: TOK_D, home_assistant_system: lab-qemu-100}", "token_env: TOK_D, socket_links: {bare-bar/lamp: lab-qemu-201}, home_assistant_system: desk-pc}", 1)
	if !strings.Contains(yaml, "bare-bar/lamp: lab-qemu-201") {
		t.Fatal("the rig's config was not changed")
	}
	pe = newPlugEnvWith(t, yaml)
	pe.withGuests("stopped")
	if s := pe.socketOf(pe.admin, "bare/bare-bar", "lamp"); s["lock"] != nil || s["why"] != model.SocketNoControl || s["system_id"] != "lab-node-pve2" {
		t.Fatalf("the fixture: %v", s)
	}
	pe.s.Cfg.PanelHosts = []string{"lab-qemu-202"} // panel-vm, the other guest of PVE2
	pe.s.Poller.Cfg.PanelHosts = []string{"lab-qemu-202"}
	if s := pe.socketOf(pe.admin, "bare/bare-bar", "lamp"); s["lock"] != model.SocketLockPanel || s["why"] != model.SocketLockPanel || s["note"] != "Locked. This socket powers the panel." {
		t.Errorf("a socket linked to a guest beside the panel's guest: %v", s)
	}
	pe.s.Cfg.PanelHosts = []string{"lab-qemu-100"} // a guest of the other node
	pe.s.Poller.Cfg.PanelHosts = []string{"lab-qemu-100"}
	if s := pe.socketOf(pe.admin, "bare/bare-bar", "lamp"); s["lock"] != nil {
		t.Errorf("the panel runs on another node: %v", s)
	}
}

// Finding 4: the mark "seen to go off" had no age. A run of "Update unpowered hosts" sets it, and only a sign of
// life of the machine ends it; a machine that is started by hand later, and whose agent does not answer, kept
// it for as long as the panel ran, and its powerbar socket was switched off with one passkey. For a socket the
// mark is a sign of "off" for 60 minutes after it was set (socketOffMarkFor). After that the machine "may still
// be running": the question, and the typed name. The mark itself, and the row, stay as they are.
func TestPlugSeenToGoOffIsASignForAnHour(t *testing.T) {
	if socketOffMarkFor != 60*time.Minute {
		t.Fatalf("the mark counts for %v", socketOffMarkFor)
	}
	pe := newPlugEnv(t)
	pe.pcOff()
	later := func(d time.Duration) {
		pe.s.Poller.Clock = func() time.Time { return time.Now().Add(d) }
		pe.poll()
		if x := pe.do("GET", "/api/systems/desk-pc", nil, "Cookie", pe.admin); x.body["status"] != "offline" || !pe.s.Poller.KnownOff("desk-pc") {
			t.Fatalf("%v later: the PC is %v, marked %v", d, x.body["status"], pe.s.Poller.KnownOff("desk-pc"))
		}
	}
	for _, d := range []time.Duration{0, 59 * time.Minute} {
		later(d)
		r := pe.flip("reader/desk-bar", "ch3", "off")
		if _, _, ok := approvalAsked(r); !ok || r.body["detail"].(map[string]any)["signs"] != "switch off Desk PC (Monitor powerbar), which powers DESK-PC" {
			t.Fatalf("%v after the PC was seen to go off: %d %s", d, r.code, r.raw)
		}
	}
	const said = "DESK-PC does not answer, which does not say that it is off: it may still be running. Shut it down first, or look at it."
	for _, d := range []time.Duration{61 * time.Minute, 30 * time.Hour} {
		later(d)
		r := pe.flip("reader/desk-bar", "ch3", "off")
		if dd, _ := r.body["detail"].(map[string]any); r.code != 409 || dd["code"] != codeMachineRunning || dd["state"] != "unknown" || dd["machine"] != "DESK-PC" || dd["message"] != said {
			t.Fatalf("%v after the PC was seen to go off: %d %s\nwant machine_running, state unknown", d, r.code, r.raw)
		}
		// a plain "off" is not enough any more; the cut needs the name, and signs that it may be running
		b := plugBody("reader/desk-bar", "ch3", "off")
		b["cut"], b["confirm"] = true, "DESK-PC"
		r = pe.post("/api/power/socket", b)
		if _, _, ok := approvalAsked(r); !ok || r.body["detail"].(map[string]any)["signs"] != "cut the power of DESK-PC while it may still be running: switch off Desk PC (Monitor powerbar)" {
			t.Fatalf("%v later, the cut: %d %s", d, r.code, r.raw)
		}
	}
	if n := len(plugSent(pe.reader)); n != 0 {
		t.Errorf("the machine got %d switch requests", n)
	}
}

// Finding 6: home_assistant_system that names no row. Without the key every socket of the agent is locked,
// because any of them could be the one that feeds Home Assistant; with a key that names nothing (a typing
// mistake) that is just as true, and only the sockets that were linked to a machine were locked: one that is
// linked to nothing had the "nothing is linked" question and no lock. Now every socket of such an agent is
// locked, on and off, with a sentence that names the id. It ends by itself when a row with that id is there.
func TestPlugHomeAssistantSystemThatNamesNoRow(t *testing.T) {
	// "third" reads spare-bar: pc is linked to the PC, lamp to nothing; its home_assistant_system names
	// lab-qemu-201, a VM the panel has not heard of yet
	pe := newPlugEnvWith(t, plugConfigWith(t, "home_assistant_system: lab-qemu-999}", "home_assistant_system: lab-qemu-201}"))
	said := strings.Replace(plugHAUnknown, "lab-qemu-999", "lab-qemu-201", 1)
	for _, socket := range []string{"pc", "lamp"} {
		s := pe.socketOf(pe.admin, "third/spare-bar", socket)
		if s["switchable"] != false || s["lock"] != model.SocketLockHAUnknown || s["why"] != model.SocketLockHAUnknown || s["note"] != said {
			t.Errorf("%s: %v\nwant the lock %s: %q", socket, s, model.SocketLockHAUnknown, said)
		}
		for _, to := range []string{"off", "on"} {
			b := plugBody("third/spare-bar", socket, to)
			if to == "off" {
				b["cut"], b["confirm"] = true, "DESK-PC" // no way round it
			}
			r := pe.post("/api/power/socket", b)
			if msg, _ := plugDetailOf(r); r.code != 409 || msg != said+" Nothing was switched." {
				t.Errorf("%s %s: %d %s", socket, to, r.code, r.raw)
			}
			if au := pe.plugAudits()[0]; au["target"] != "third/spare-bar/"+socket || au["outcome"] != "refused ("+model.SocketLockHAUnknown+"): "+said+" Nothing was switched." {
				t.Errorf("%s %s: the audit entry: %v", socket, to, au)
			}
		}
	}
	if n := len(pe.third.Asked()) + len(pe.iss.Asked()); n != 0 {
		t.Errorf("the machine or Pocket ID was asked %d things", n)
	}
	// the agents whose key names a row are not touched
	if s := pe.socketOf(pe.admin, "other/rack-bar", "fan"); s["switchable"] != true || s["lock"] != nil {
		t.Errorf("a socket of another agent: %v", s)
	}
	// somebody who is no admin gets no lock and no sentence
	if s := pe.socketOf(pe.viewerOf("Home"), "reader/desk-bar", "ch1"); s["lock"] != nil || s["note"] != nil {
		t.Errorf("a viewer: %v", s)
	}
	// the row is there (Proxmox lists the VM now): the lock is gone, and what is left are the rules of every socket
	pe.withGuests("running")
	if s := pe.socketOf(pe.admin, "third/spare-bar", "lamp"); s["switchable"] != true || s["lock"] != nil || s["note"] != nil {
		t.Errorf("lamp with the row there: %v", s)
	}
	if s := pe.socketOf(pe.admin, "third/spare-bar", "pc"); s["switchable"] != true || s["lock"] != nil || s["system_id"] != "desk-pc" {
		t.Errorf("pc with the row there: %v", s)
	}
	if _, _, ok := approvalAsked(pe.flip("third/spare-bar", "lamp", "on")); !ok {
		t.Error("on for the lamp with the row there is refused")
	}
}

// Finding 2 (the second review's fix "C" was not closed): somebody who may see the machines of some sites only
// gets no socket that feeds a row they may not see. That held only while the viewer also saw the agent that
// relays the plug. A viewer who sees the plug and not that agent got every socket, names and states, and no
// count of hidden ones: the viewer's copy of the source loses the agent (cleanPower), the sockets were worked
// out from that copy, could not be looked at without the agent, and a socket that cannot be looked at was
// shown. Two ways to such a viewer, each for a cookie and for a read token that is limited to the site, and
// each for GET /api/power and for the event stream:
//
//  1. the plug is put into a site by itself (the target "power:reader/desk-bar" of the Sites page): the viewer
//     of that site sees no row at all;
//  2. the plug follows its own link (power_links) to a row the viewer sees, and the agent that reads it is in
//     another site.
//
// The sockets are now worked out from the source as the panel has it. A socket is in the answer when it is
// linked to nothing (it is the plug's own) or to a row the viewer sees; every other one is left out and
// counted.
func TestPlugSocketsOfAPlugWhoseAgentTheViewerDoesNotSee(t *testing.T) {
	type want struct {
		shown  map[string]string // socket id -> the row it names ("": none)
		hidden int
		rows   []string // the viewer's rows
	}
	check := func(t *testing.T, pe *plugEnv, who string, hdr []string, w want) {
		t.Helper()
		rows := []string{}
		for _, x := range pe.do("GET", "/api/systems", nil, hdr...).body["systems"].([]any) {
			rows = append(rows, fmt.Sprint(x.(map[string]any)["id"]))
		}
		if fmt.Sprint(rows) != fmt.Sprint(w.rows) {
			t.Fatalf("%s: the fixture: the rows are %v, want %v", who, rows, w.rows)
		}
		power := pe.do("GET", "/api/power", nil, hdr...)
		if power.code != 200 {
			t.Fatalf("%s: GET /api/power: %d %s", who, power.code, power.raw)
		}
		found := false
		for _, x := range power.body["sources"].([]any) {
			src := x.(map[string]any)
			if src["id"] != "reader/desk-bar" {
				continue
			}
			found = true
			got := map[string]string{}
			list, _ := src["sockets"].([]any)
			for _, k := range list {
				m := k.(map[string]any)
				id, _ := m["system_id"].(string)
				got[fmt.Sprint(m["id"])] = id
				if m["switchable"] != false || m["why"] != model.SocketReadOnly || m["tag"] != nil || m["lock"] != nil || m["note"] != nil {
					t.Errorf("%s: the socket %v", who, m)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(w.shown) || src["sockets_hidden"] != float64(w.hidden) {
				t.Errorf("%s: GET /api/power has the sockets %v and sockets_hidden %v, want %v and %d", who, got, src["sockets_hidden"], w.shown, w.hidden)
			}
			if src["via"] != nil {
				t.Errorf("%s: the answer names the agent that relays the plug: %v", who, src["via"])
			}
		}
		if !found {
			t.Fatalf("%s: the fixture: the plug is not in the viewer's answer: %s", who, power.raw)
		}
		code, stream := pe.streamBody("/api/stream", hdr...)
		if code != 200 || !strings.Contains(stream, "Monitor powerbar") || !strings.Contains(stream, fmt.Sprintf(`"sockets_hidden":%d`, w.hidden)) {
			t.Errorf("%s: the stream (%d) has not the plug with sockets_hidden %d", who, code, w.hidden)
		}
		names := map[string]string{"ch1": "Left monitor", "ch2": "Lamp", "ch3": "Desk PC", "ch4": "Server", "ch5": "Home Assistant box", "ch6": "Panel box", "ch7": "Old PC", "ch8": "Printer"}
		for id, name := range names {
			_, shown := w.shown[id]
			for where, body := range map[string]string{"GET /api/power": string(power.raw), "the stream": stream} {
				if strings.Contains(body, name) != shown {
					t.Errorf("%s: %s names the socket %q: %v, want %v", who, where, name, !shown, shown)
				}
			}
		}
	}
	token := func(pe *plugEnv, site string) []string {
		tok, _, err := pe.s.Tokens.CreateForSites("t-"+site, "system", "read", "cli:test", 0, []string{site})
		if err != nil {
			t.Fatal(err)
		}
		return []string{"Authorization", "Bearer " + tok}
	}
	t.Run("the plug is in a site by itself", func(t *testing.T) {
		pe := newPlugEnv(t)
		_ = pe.s.Access.EnsureSites([]string{"Home", "Lab"})
		if _, err := pe.s.Access.Assign("power:reader/desk-bar", "Lab"); err != nil {
			t.Fatal(err)
		}
		w := want{shown: map[string]string{"ch1": "", "ch2": "", "ch8": ""}, hidden: 5, rows: []string{}}
		check(t, pe, "a viewer of the site", []string{"Cookie", pe.viewerOf("Lab")}, w)
		check(t, pe, "a read token of the site", token(pe, "Lab"), w)
	})
	t.Run("the plug follows its own link", func(t *testing.T) {
		yaml := strings.Replace(plugConfig, "node: PVE, site: Home,\n     power_links: {pc-plug: desk-pc},", "node: PVE, site: Lab,\n     power_links: {pc-plug: desk-pc, desk-bar: desk-pc},", 1)
		if yaml == plugConfig {
			t.Fatal("the rig's config was not changed")
		}
		pe := newPlugEnvWith(t, yaml)
		w := want{shown: map[string]string{"ch1": "", "ch2": "", "ch3": "desk-pc", "ch8": ""}, hidden: 4, rows: []string{"desk-pc"}}
		check(t, pe, "a viewer of the site", []string{"Cookie", pe.viewerOf("Home")}, w)
		check(t, pe, "a read token of the site", token(pe, "Home"), w)
	})
	// nothing of this is for an admin, or for a token that sees every site: all eight, nothing left out
	pe := newPlugEnv(t)
	all, _, err := pe.s.Tokens.Create("t-all", "system", "read", "cli:test", 0)
	if err != nil {
		t.Fatal(err)
	}
	for who, hdr := range map[string][]string{"an admin": {"Cookie", pe.admin}, "a read token without sites": {"Authorization", "Bearer " + all}} {
		for _, x := range pe.do("GET", "/api/power", nil, hdr...).body["sources"].([]any) {
			if src := x.(map[string]any); src["id"] == "reader/desk-bar" && (len(src["sockets"].([]any)) != 8 || src["sockets_hidden"] != nil) {
				t.Errorf("%s: %d sockets, sockets_hidden %v", who, len(src["sockets"].([]any)), src["sockets_hidden"])
			}
		}
	}
	// The rule alone, for somebody who is no admin. A socket that cannot be looked at (a copy of the source
	// without the agent that relays it) is left out for a viewer of some sites, and so is one whose link names
	// no row, whatever that viewer may see; a viewer of every site gets them, read-only.
	var bar model.PowerSourceState
	for _, st := range pe.s.Poller.Power.Overview().Sources {
		if st.ID == "reader/desk-bar" {
			bar = st
		}
	}
	if len(bar.Sockets) != 8 || bar.Via == nil {
		t.Fatalf("the fixture: %v", bar)
	}
	looks := pe.s.newPlugLooks(pe.s.Poller.Snapshot())
	every := func(string) bool { return true }
	if out, hidden := looks.sockets(bar, false, false, every); len(out) != 7 || hidden != 1 || slices.ContainsFunc(out, func(k model.PowerSocket) bool { return k.ID == "ch7" }) {
		t.Errorf("a viewer of some sites who sees every row there is: %d sockets, %d left out; want ch7 (linked to an id without a row) left out", len(out), hidden)
	}
	if out, hidden := looks.sockets(bar, false, true, every); len(out) != 8 || hidden != 0 {
		t.Errorf("a viewer of every site: %d sockets, %d left out", len(out), hidden)
	}
	blind := bar
	blind.Via = nil
	if out, hidden := looks.sockets(blind, false, false, every); len(out) != 0 || hidden != 8 {
		t.Errorf("a source without its agent, for a viewer of some sites: %d sockets shown, %d left out; want none and 8", len(out), hidden)
	}
	if out, hidden := looks.sockets(blind, false, true, every); len(out) != 8 || hidden != 0 || out[0].Why != model.SocketReadOnly || out[0].Tag != "" {
		t.Errorf("a source without its agent, for a viewer of every site: %d sockets, %d left out", len(out), hidden)
	}
}

// Finding 5: an approved switch that meets the limit on control requests in the panel's replay is on record as
// a plug.switch entry. The limit comes before anything is looked at, and a request over it is no attempt and
// leaves no plug.switch entry; but after somebody gave a passkey for a switch, "what became of it" must be in
// the log. Since the limit was moved to the top of the route such a replay left only an auth.denied line,
// which shares the account's budget of ten written refusals a minute: with that used up, the newest lines
// were "auth.approval ok ... runs: switch a plug socket (...)" and nothing after it, as if the switch had
// run and its entry were missing. One such entry needs one passkey, so it cannot fill the log.
func TestPlugApprovedReplayOverTheLimitIsOnRecord(t *testing.T) {
	pe := newPlugEnv(t)
	pe.s.control = newRateLimiter(2)
	id, _, ok := approvalAsked(pe.flip("reader/desk-bar", "ch2", "on")) // the first request of the two
	if !ok {
		t.Fatal("not asked")
	}
	for range 12 { // refused requests of the same admin (no page header): the account's budget of written refusals
		pe.do("POST", "/api/power/socket", plugBody("reader/desk-bar", "ch2", "on"), "Cookie", pe.admin, "Content-Type", "application/json")
	}
	if r := pe.flip("reader/desk-bar", "ch6", "off"); r.code != 409 { // the second: the limit is reached
		t.Fatalf("the fixture: %d %s", r.code, r.raw)
	}
	before := len(pe.plugAudits())
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
	entries := pe.plugAudits()
	if len(entries) != before+1 {
		t.Fatalf("%d plug.switch entries after the replay, %d before it: want one more", len(entries), before)
	}
	au := entries[0]
	p, _ := au["params"].(map[string]any)
	if au["target"] != "reader/desk-bar/ch2" || au["outcome"] != "refused (rate_limit): too many control requests" || p["to"] != "on" || p["approval"] != "not sent" || au["actor"] != plugActor {
		t.Errorf("the entry of the approved switch that was not sent: %v", au)
	}
	// it is the newest thing in the log about that switch: after the approval's own line
	all := pe.auditEntries("", "")
	if len(all) < 2 || all[0]["action"] != plugAudit || all[0]["target"] != "reader/desk-bar/ch2" {
		t.Errorf("the newest entry of the log: %v", all[0])
	}
	// a request over the limit that no passkey was given for is still no attempt: no entry
	if r := pe.flip("reader/desk-bar", "ch2", "on"); r.code != 429 {
		t.Fatalf("a request over the limit: %d", r.code)
	}
	if n := len(pe.plugAudits()); n != before+1 {
		t.Errorf("a request over the limit without an approval wrote a plug.switch entry (%d, want %d)", n, before+1)
	}
}

// The second review's mutation Q02 survived the suite: with the limit on control requests as ONE budget for all
// admins together, no test failed, because no plug test had two admins. The limit is each admin's own
// (s.control.allow(u.Sub)): one admin who uses theirs up, by mistake or on purpose, does not lock the route for
// the other.
func TestPlugControlLimitIsEachAdminsOwn(t *testing.T) {
	pe := newPlugEnv(t)
	pe.s.control = newRateLimiter(3)
	second := model.User{Sub: "u22", Name: model.P("Second"), Email: model.P("s@example.com"), Groups: []string{"viewers", "controllers"}}
	ck := pe.cookie(second, 0)
	if _, _, err := pe.s.Access.SetRole("u22", "admin", nil, "test", false); err != nil {
		t.Fatal(err)
	}
	other := func() resp {
		return pe.do("POST", "/api/power/socket", plugBody("reader/desk-bar", "ch6", "off"), "Cookie", ck, "X-Requested-With", "fairy", "Content-Type", "application/json")
	}
	for i := range 3 {
		if r := pe.flip("reader/desk-bar", "ch6", "off"); r.code != 409 {
			t.Fatalf("the first admin's request %d: %d %s", i+1, r.code, r.raw)
		}
	}
	if r := pe.flip("reader/desk-bar", "ch6", "off"); r.code != 429 {
		t.Fatalf("the first admin's fourth request: %d %s", r.code, r.raw)
	}
	for i := range 3 { // the second admin has all of their own
		if r := other(); r.code != 409 {
			t.Fatalf("the second admin's request %d, after the first used up theirs: %d %s", i+1, r.code, r.raw)
		}
	}
	if r := other(); r.code != 429 {
		t.Errorf("the second admin's fourth request: %d %s", r.code, r.raw)
	}
	if n := len(pe.plugAudits()); n != 6 {
		t.Errorf("%d plug.switch entries, want 3 of each admin", n)
	}
}

// The second review's mutation Q35 survived: how old a reading of a socket's own meter may be was tested in the
// rule alone, with the age set by hand, so the line that hands offline_after to the rule (look) was pinned by
// nothing: with ten times offline_after no test failed. Here the route is asked. The PC is silent and its own
// plug reads 2 W: fresh, that is a sign of "off", and "off" goes to the passkey. The same reading a little
// older than offline_after (the reader was not polled meanwhile; the plug stays "ok" with its last reading) is
// no sign any more, and "off" asks.
func TestPlugMeterReadingOlderThanOfflineAfterIsNoSign(t *testing.T) {
	pe := newPlugEnvWith(t, plugConfigWith(t, "offline_after: 60", "offline_after: 1"))
	pe.pcSilent()
	pe.measures("pc-plug", 2)
	r := pe.flip("reader/pc-plug", "main", "off")
	if _, _, ok := approvalAsked(r); !ok || r.body["detail"].(map[string]any)["signs"] != "switch off PC (PC plug), which powers DESK-PC" {
		t.Fatalf("a fresh reading of 2 W: %d %s", r.code, r.raw)
	}
	time.Sleep(1300 * time.Millisecond) // offline_after, and a little
	for _, x := range pe.do("GET", "/api/power", nil, "Cookie", pe.admin).body["sources"].([]any) {
		if src := x.(map[string]any); src["id"] == "reader/pc-plug" && src["status"] != "ok" {
			t.Fatalf("the fixture: the plug is %v", src["status"])
		}
	}
	r = pe.flip("reader/pc-plug", "main", "off")
	const said = "DESK-PC does not answer, which does not say that it is off: it may still be running. Shut it down first, or look at it."
	if d, _ := r.body["detail"].(map[string]any); r.code != 409 || d["code"] != codeMachineRunning || d["state"] != "unknown" || d["message"] != said {
		t.Fatalf("a reading of 2 W that is older than offline_after: %d %s\nwant machine_running, state unknown", r.code, r.raw)
	}
	// polled again, the reading is fresh again
	pe.poll()
	if _, _, ok := approvalAsked(pe.flip("reader/pc-plug", "main", "off")); !ok {
		t.Error("a fresh reading after the next poll: off is refused")
	}
}

// The second review's mutation Q18 survived: "seen to go off" is asked for the row the machine is shown as, not
// for the id that is written in the link, and in the rig the two were always the same id. Here they are not:
// the socket is linked to an agent (nas-box) that runs on a Proxmox node of a cluster, so the machine is shown
// as that node's row. A run saw the machine go off: both ids have the mark, and "off" needs no question. Then
// the cluster lists the node as offline, which ends the node's mark (the cluster says what the node is; the
// poller's rule) and leaves the agent's. What the page shows is the node's row, without a mark: the machine "may
// still be running", and "off" asks, whatever the mark under the agent's id says.
func TestPlugSeenToGoOffIsAskedOfTheRowThatIsShown(t *testing.T) {
	t.Setenv("TOK_G", testutil.ReadToken+"g")
	yaml := plugConfigWith(t, "desk-bar/ch7: old-pc", "desk-bar/ch7: nas-box")
	yaml = strings.Replace(yaml, "\nagents:\n", "\nagents:\n  - {id: nas-box, name: NAS-BOX, url: \"http://nas-box\", token_env: TOK_G, node: PVE2}\n", 1)
	if !strings.Contains(yaml, "id: nas-box") {
		t.Fatal("the rig's config was not changed")
	}
	pe := newPlugEnvWith(t, yaml)
	node, on := "online", true // what the cluster says of PVE2, and whether the agent on it answers
	pe.b.Mu.Lock()
	pe.b.PVEOf = func(string) ([]jsonx.M, error) {
		return append(testutil.HostResources("PVE", "running"), jsonx.M{"id": "node/PVE2", "type": "node", "node": "PVE2", "status": node, "cpu": 0.1,
			"maxcpu": 8.0, "mem": float64(8 << 30), "maxmem": float64(32 << 30), "disk": float64(10 << 30), "maxdisk": float64(100 << 30), "uptime": 30.0}), nil
	}
	pe.b.Metrics["nas-box"] = func() (jsonx.M, error) {
		if !on {
			return nil, devices.Errf("connection failed")
		}
		m := testutil.AgentMetrics()
		m["hostname"] = "nas-box"
		delete(m, "power_sources")
		return m, nil
	}
	pe.b.Mu.Unlock()
	pe.poll()
	pe.poll()
	if got := pe.s.Poller.ShownID("nas-box"); got != "lab-node-pve2" {
		t.Fatalf("the fixture: nas-box is shown as %q", got)
	}
	if s := pe.socketOf(pe.admin, "reader/desk-bar", "ch7"); s["switchable"] != true || s["system_id"] != "lab-node-pve2" {
		t.Fatalf("the fixture: %v", s)
	}
	// it runs: the question
	if r := pe.flip("reader/desk-bar", "ch7", "off"); r.code != 409 || r.body["detail"].(map[string]any)["state"] != "running" {
		t.Fatalf("off while the machine runs: %d %s", r.code, r.raw)
	}
	// it goes off, and a run saw it: the cluster lists the node as offline, the agent does not answer
	pe.b.Mu.Lock()
	node, on = "offline", false
	pe.b.Mu.Unlock()
	pe.poll()
	pe.s.Poller.MarkOff("nas-box")
	if !pe.s.Poller.KnownOff("nas-box") || !pe.s.Poller.KnownOff("lab-node-pve2") {
		t.Fatalf("the fixture: the marks after MarkOff: agent %v, node %v", pe.s.Poller.KnownOff("nas-box"), pe.s.Poller.KnownOff("lab-node-pve2"))
	}
	r := pe.flip("reader/desk-bar", "ch7", "off")
	if _, _, ok := approvalAsked(r); !ok || r.body["detail"].(map[string]any)["signs"] != "switch off Old PC (Monitor powerbar), which powers PVE2" {
		t.Fatalf("off for a machine that was seen to go off: %d %s", r.code, r.raw)
	}
	// the next poll of the cluster: the node's mark ends, the agent's stays
	pe.poll()
	if !pe.s.Poller.KnownOff("nas-box") || pe.s.Poller.KnownOff("lab-node-pve2") {
		t.Fatalf("the fixture: the marks after the cluster's poll: agent %v, node %v; want the agent's only", pe.s.Poller.KnownOff("nas-box"), pe.s.Poller.KnownOff("lab-node-pve2"))
	}
	if x := pe.do("GET", "/api/systems/lab-node-pve2", nil, "Cookie", pe.admin); x.body["status"] != "offline" {
		t.Fatalf("the fixture: PVE2 is %v", x.body["status"])
	}
	r = pe.flip("reader/desk-bar", "ch7", "off")
	if d, _ := r.body["detail"].(map[string]any); r.code != 409 || d["code"] != codeMachineRunning || d["state"] != "unknown" || d["machine"] != "PVE2" || d["system_id"] != "lab-node-pve2" {
		t.Fatalf("off when only the id in the link has the mark: %d %s\nwant machine_running, state unknown, for PVE2", r.code, r.raw)
	}
}
