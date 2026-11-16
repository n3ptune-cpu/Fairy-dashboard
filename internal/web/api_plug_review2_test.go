package web

import (
	"strings"
	"testing"
	"time"

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
