package web

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Tests for the third review of stage 1a (its findings 1 to 4, and the mutations of its own that survived).

// plugNodeRig is the plug rig with a second Proxmox node, PVE2, and what can sit on it. The socket of these
// tests is other/rack-bar/node ("Node" of "Rack bar", read by the agent "other", which runs on a machine of
// its own unless otherExtra says where).
//
//	lab-node-pve2  the node PVE2, with the guests nas-vm (lab-qemu-201), panel-vm (lab-qemu-202) and the
//	               container ct-box (lab-lxc-203); lost-vm (lab-qemu-301) and the container lost-ct
//	               (lab-lxc-302) are on a node the answer does not have
//	pve2agent      the agent of the node PVE2 (node: PVE2): one machine with the node, shown as the node's row
//	vmagent        an agent in nas-vm (links_to: lab-qemu-201); it has a row of its own
//	ctagent        an agent in ct-box (links_to: lab-lxc-203)
//	vm2agent       an agent in panel-vm (links_to: lab-qemu-202)
//	sideagent      a second agent of the node (links_to: lab-node-pve2, after pve2agent): a row of its own.
//	               With it the configuration does not say whose the node is (two agents name it), and the
//	               node has no "other id"; lone leaves it out, and the node's other id is pve2agent
//	lostagent      an agent in lost-vm (links_to: lab-qemu-301)
//	viaagent       an agent whose entry names another agent, vmagent (links_to: vmagent): two rows on the way
//	               to the node
//	via2agent      the same over lostagent (links_to: lostagent): the way ends at a guest without its node
type plugNodeRig struct {
	link       string // what other's rack-bar/node is linked to
	ha         string // other's home_assistant_system ("": desk-pc, a machine with nothing to do with PVE2)
	otherExtra string // more keys of other's entry: where the agent that reads the plug runs ("node: PVE2,")
	panelHosts string // panel_hosts ("": the rig's panel-box)
	guests     string // the status of every guest ("": running)
	down       string // an agent that answers at first and then stops: its row is "stale"
	lone       bool   // without sideagent: pve2agent is the only agent that names the node
}

const plugOtherEntry = "  - {id: other, name: OTHER, url: \"http://other\", token_env: TOK_B, control_token_env: CTL_A,\n     socket_links: {rack-bar/node: lab-node-pve}, home_assistant_system: lab-qemu-100}"

func (c plugNodeRig) env(t *testing.T) *plugEnv {
	t.Helper()
	agents := []string{"pve2agent", "vmagent", "ctagent", "vm2agent", "sideagent", "lostagent", "viaagent", "via2agent"}
	for i := range agents { // before one is left out: the entries name the tokens by their place in this list
		t.Setenv(fmt.Sprintf("TOK_N%d", i), testutil.ReadToken+string(rune('n'+i)))
	}
	if strings.Count(plugConfig, plugOtherEntry) != 1 || strings.Count(plugConfig, "\nagents:\n") != 1 || strings.Count(plugConfig, "panel_hosts: [panel-box]") != 1 {
		t.Fatal("the rig's config is not the one this fixture changes")
	}
	ha := c.ha
	if ha == "" {
		ha = "desk-pc"
	}
	yaml := strings.Replace(plugConfig, plugOtherEntry, fmt.Sprintf("  - {id: other, name: OTHER, url: \"http://other\", token_env: TOK_B, control_token_env: CTL_A, %s\n"+
		"     socket_links: {rack-bar/node: %s}, home_assistant_system: %s}", c.otherExtra, c.link, ha), 1)
	yaml = strings.Replace(yaml, "\nagents:\n", "\nagents:\n"+
		"  - {id: pve2agent, name: PVE2AGENT, url: \"http://pve2agent\", token_env: TOK_N0, node: PVE2}\n"+
		"  - {id: vmagent, name: VMAGENT, url: \"http://vmagent\", token_env: TOK_N1, links_to: lab-qemu-201}\n"+
		"  - {id: ctagent, name: CTAGENT, url: \"http://ctagent\", token_env: TOK_N2, links_to: lab-lxc-203}\n"+
		"  - {id: vm2agent, name: VM2AGENT, url: \"http://vm2agent\", token_env: TOK_N3, links_to: lab-qemu-202}\n"+
		"  - {id: sideagent, name: SIDEAGENT, url: \"http://sideagent\", token_env: TOK_N4, links_to: lab-node-pve2}\n"+
		"  - {id: lostagent, name: LOSTAGENT, url: \"http://lostagent\", token_env: TOK_N5, links_to: lab-qemu-301}\n"+
		"  - {id: viaagent, name: VIAAGENT, url: \"http://viaagent\", token_env: TOK_N6, links_to: vmagent}\n"+
		"  - {id: via2agent, name: VIA2AGENT, url: \"http://via2agent\", token_env: TOK_N7, links_to: lostagent}\n", 1)
	if c.lone {
		const side = "  - {id: sideagent, name: SIDEAGENT, url: \"http://sideagent\", token_env: TOK_N4, links_to: lab-node-pve2}\n"
		if strings.Count(yaml, side) != 1 {
			t.Fatal("the fixture: no entry of sideagent to leave out")
		}
		yaml = strings.Replace(yaml, side, "", 1)
		agents = slices.DeleteFunc(agents, func(id string) bool { return id == "sideagent" })
	}
	if c.panelHosts != "" {
		yaml = strings.Replace(yaml, "panel_hosts: [panel-box]", "panel_hosts: ["+c.panelHosts+"]", 1)
	}
	pe := newPlugEnvWith(t, yaml)
	down := false
	status := c.guests
	if status == "" {
		status = "running"
	}
	guest := func(kind string, vmid float64, name, node string) jsonx.M {
		return jsonx.M{"id": kind + "/" + name, "type": kind, "vmid": vmid, "name": name, "node": node, "status": status,
			"cpu": 0.0, "maxcpu": 2.0, "mem": 0.0, "maxmem": float64(4 << 30), "disk": 0.0, "maxdisk": float64(32 << 30), "template": 0.0}
	}
	pe.b.Mu.Lock()
	pe.b.PVEOf = func(string) ([]jsonx.M, error) {
		r := append(testutil.HostResources("PVE", "running"), jsonx.M{"id": "node/PVE2", "type": "node", "node": "PVE2", "status": "online", "cpu": 0.1,
			"maxcpu": 8.0, "mem": float64(8 << 30), "maxmem": float64(32 << 30), "disk": float64(10 << 30), "maxdisk": float64(100 << 30), "uptime": 30.0})
		return append(r, guest("qemu", 201, "nas-vm", "PVE2"), guest("qemu", 202, "panel-vm", "PVE2"), guest("lxc", 203, "ct-box", "PVE2"), guest("qemu", 301, "lost-vm", "GHOST"), guest("lxc", 302, "lost-ct", "GHOST")), nil
	}
	for _, id := range agents {
		pe.b.Metrics[id] = func() (jsonx.M, error) {
			if down && c.down == id {
				return nil, devices.Errf("connection failed")
			}
			m := testutil.AgentMetrics()
			m["hostname"] = id
			delete(m, "power_sources")
			return m, nil
		}
	}
	pe.b.Mu.Unlock()
	pe.poll()
	pe.poll()
	if c.down != "" {
		pe.b.Mu.Lock()
		down = true
		pe.b.Mu.Unlock()
		pe.poll()
	}
	// the fixture: the node's agent is one row with the node; every other agent has a row of its own
	if got := pe.s.Poller.ShownID("pve2agent"); got != "lab-node-pve2" {
		t.Fatalf("the fixture: pve2agent is shown as %q", got)
	}
	for _, id := range agents[1:] {
		x := pe.do("GET", "/api/systems/"+id, nil, "Cookie", pe.admin)
		want := "online"
		if c.down == id {
			want = "stale"
		}
		if x.code != 200 || x.body["kind"] != "agent" || x.body["status"] != want || pe.s.Poller.ShownID(id) != id {
			t.Fatalf("the fixture: the row of %s: %d %v", id, x.code, x.body)
		}
	}
	for id, kind := range map[string]string{"lab-node-pve2": "node", "lab-qemu-201": "qemu", "lab-qemu-202": "qemu", "lab-lxc-203": "lxc", "lab-qemu-301": "qemu", "lab-lxc-302": "lxc"} {
		if x := pe.do("GET", "/api/systems/"+id, nil, "Cookie", pe.admin); x.body["kind"] != kind {
			t.Fatalf("the fixture: %s is %v", id, x.body)
		}
	}
	// the node's other id: its agent's when the configuration says whose the node is, none when two agents name it
	if other := slices.Contains(pe.s.Poller.SameMachine("lab-node-pve2"), "pve2agent"); other != c.lone {
		t.Fatalf("the fixture: the ids of the node are %v", pe.s.Poller.SameMachine("lab-node-pve2"))
	}
	return pe
}

// plugNodeLocked asks everything of the socket other/rack-bar/node that a lock must refuse: off, on, and a cut
// with every name somebody could type. want is the lock, said its sentence.
func plugNodeLocked(t *testing.T, pe *plugEnv, what, want, said string) {
	t.Helper()
	s := pe.socketOf(pe.admin, "other/rack-bar", "node")
	if s["switchable"] != false || s["lock"] != want || s["why"] != want || s["note"] != said || s["system_id"] != "lab-node-pve2" {
		t.Errorf("%s: the socket for an admin: %v\nwant the lock %s on the row of the node", what, s, want)
	}
	tries := map[string]map[string]any{"off": plugBody("other/rack-bar", "node", "off"), "on": plugBody("other/rack-bar", "node", "on")}
	for _, name := range []string{"PVE2", "VMAGENT", "CTAGENT", "SIDEAGENT", "nas-vm", "ct-box", "PVE2AGENT"} {
		b := plugBody("other/rack-bar", "node", "off")
		b["cut"], b["confirm"] = true, name
		tries["a cut with "+name+" typed"] = b
	}
	for name, body := range tries {
		r := pe.post("/api/power/socket", body)
		if msg, _ := plugDetailOf(r); r.code != 409 || msg != said+" Nothing was switched." {
			t.Errorf("%s, %s: %d %s", what, name, r.code, r.raw)
		}
	}
	if n := len(pe.other.Asked()) + len(pe.iss.Asked()); n != 0 {
		t.Errorf("%s: the machine or Pocket ID was asked %d things for a locked socket", what, n)
	}
}

// plugNodeAsks asks "off" for the socket other/rack-bar/node where nothing locks it and the node PVE2 runs:
// the question is about the node, whoever the link names (named, with the words that say what it is); only the
// node's name cuts, and the passkey signs the node.
func plugNodeAsks(t *testing.T, pe *plugEnv, what, named, how string, guestsRunning int) {
	t.Helper()
	s := pe.socketOf(pe.admin, "other/rack-bar", "node")
	if s["switchable"] != true || s["lock"] != nil || s["system_id"] != "lab-node-pve2" {
		t.Fatalf("%s: the socket for an admin: %v", what, s)
	}
	said := fmt.Sprintf("This socket is linked to %s, %s PVE2, and what it feeds is PVE2. PVE2 is still running, with %d guests running on it. Shut it down first.", named, how, guestsRunning)
	if guestsRunning == 0 {
		said = fmt.Sprintf("This socket is linked to %s, %s PVE2, and what it feeds is PVE2. PVE2 is still running. Shut it down first.", named, how)
	}
	r := pe.flip("other/rack-bar", "node", "off")
	d, _ := r.body["detail"].(map[string]any)
	if r.code != 409 || d["code"] != codeMachineRunning || d["state"] != "running" || d["machine"] != "PVE2" || d["system_id"] != "lab-node-pve2" || d["guest"] != named || d["message"] != said {
		t.Fatalf("%s: off: %d %s\nwant machine_running for PVE2: %q", what, r.code, r.raw, said)
	}
	cut := func(confirm string) resp {
		b := plugBody("other/rack-bar", "node", "off")
		b["cut"], b["confirm"] = true, confirm
		return pe.post("/api/power/socket", b)
	}
	for _, name := range []string{"VMAGENT", "CTAGENT", "SIDEAGENT", "nas-vm", "ct-box", "PVE2AGENT", named} {
		if r := cut(name); r.code != 422 || !strings.Contains(string(r.raw), "Type PVE2 to confirm") {
			t.Fatalf("%s: a cut with %s typed: %d %s", what, name, r.code, r.raw)
		}
	}
	if n := len(pe.other.Asked()); n != 0 {
		t.Fatalf("%s: the machine was asked %d things before the node's name was typed", what, n)
	}
	r = cut("PVE2")
	if _, _, ok := approvalAsked(r); !ok || r.body["detail"].(map[string]any)["signs"] != "cut the power of PVE2 while it is running: switch off Node (Rack bar)" {
		t.Fatalf("%s: a cut with the node's name typed: %d %s", what, r.code, r.raw)
	}
	r = pe.flip("other/rack-bar", "node", "on")
	if _, _, ok := approvalAsked(r); !ok || r.body["detail"].(map[string]any)["signs"] != "switch on Node (Rack bar), which powers PVE2" {
		t.Errorf("%s: on: %d %s", what, r.code, r.raw)
	}
	if n := len(plugSent(pe.other)); n != 0 {
		t.Errorf("%s: the machine got %d switch requests", what, n)
	}
}

const (
	plugSaidReader = "Locked. This socket powers the machine that reads the plugs."
	plugSaidHA     = "Locked. This socket powers Home Assistant."
	plugSaidPanel  = "Locked. This socket powers the panel."
)

// Finding 1 of the third review: "a link to a guest is a link to its node" stopped one step short. An agent
// that runs in a guest has a row of its own, of the kind "agent", and a socket that was linked to that agent
// was judged by that row: none of the three locks in any arrangement in which the node is or carries Home
// Assistant, the reader or the panel, and a cut that took the agent's name ("cut the power of VMAGENT while it
// is running"). What such a socket can feed is the node under the guest. Whatever a socket is linked to is now
// followed down to that machine (socketFeeds), and every rule uses it.
//
// The arrangements are the review's: Home Assistant on the node itself, in another guest of it, in the node's
// agent, in an agent in another guest; the reader on the node, in another guest; the panel on the node, in
// another guest. (The panel in the node's agent, and in an agent in another guest, are the review's finding 3,
// which is about panel_hosts and not about the link.) For an agent in a VM and for one in a container.
func TestPlugSocketLinkedToAnAgentInAGuest(t *testing.T) {
	for _, link := range []string{"vmagent", "ctagent"} {
		for _, c := range []struct {
			name       string
			rig        plugNodeRig
			lock, said string
		}{
			{"Home Assistant on the node", plugNodeRig{ha: "lab-node-pve2"}, model.SocketLockHomeAssistant, plugSaidHA},
			{"Home Assistant in another guest", plugNodeRig{ha: "lab-qemu-202"}, model.SocketLockHomeAssistant, plugSaidHA},
			{"Home Assistant in the node's agent", plugNodeRig{ha: "pve2agent"}, model.SocketLockHomeAssistant, plugSaidHA},
			{"Home Assistant in an agent in another guest", plugNodeRig{ha: "vm2agent"}, model.SocketLockHomeAssistant, plugSaidHA},
			{"the reader on the node", plugNodeRig{otherExtra: "links_to: lab-node-pve2,"}, model.SocketLockAgent, plugSaidReader},
			{"the reader in another guest", plugNodeRig{otherExtra: "links_to: lab-qemu-202,"}, model.SocketLockAgent, plugSaidReader},
			{"the panel on the node", plugNodeRig{panelHosts: "lab-node-pve2"}, model.SocketLockPanel, plugSaidPanel},
			{"the panel in another guest", plugNodeRig{panelHosts: "lab-qemu-202"}, model.SocketLockPanel, plugSaidPanel},
		} {
			c.rig.link = link
			plugNodeLocked(t, c.rig.env(t), link+", "+c.name, c.lock, c.said)
		}
		// nothing of the panel's on the node, and the node runs: the question is about the node
		named := map[string]string{"vmagent": "VMAGENT", "ctagent": "CTAGENT"}[link]
		plugNodeAsks(t, plugNodeRig{link: link}.env(t), link+", nothing on the node", named, "which runs on", 3)
	}
}

// Finding 1, the case in which "off" went out with one passkey: the agent in the VM does not answer and was
// seen to go off (what a run of "Update unpowered hosts" sets), and the node under it runs. That the agent in a
// guest went off says that the guest went off. It is never a sign that the node is off: the question is asked,
// about the node; and where the node carries Home Assistant's VM the socket is locked.
func TestPlugAgentInAGuestSeenToGoOffSaysNothingAboutItsNode(t *testing.T) {
	mark := func(pe *plugEnv) {
		t.Helper()
		pe.s.Poller.MarkOff("vmagent")
		if x := pe.do("GET", "/api/systems/vmagent", nil, "Cookie", pe.admin); x.body["status"] != "offline" || !pe.s.Poller.KnownOff("vmagent") {
			t.Fatalf("the fixture: vmagent is %v, marked %v", x.body["status"], pe.s.Poller.KnownOff("vmagent"))
		}
		if x := pe.do("GET", "/api/systems/lab-node-pve2", nil, "Cookie", pe.admin); x.body["status"] != "online" || pe.s.Poller.KnownOff("lab-node-pve2") {
			t.Fatalf("the fixture: PVE2 is %v, marked %v", x.body["status"], pe.s.Poller.KnownOff("lab-node-pve2"))
		}
	}
	pe := plugNodeRig{link: "vmagent", down: "vmagent"}.env(t)
	mark(pe)
	plugNodeAsks(t, pe, "the agent in the VM was seen to go off", "VMAGENT", "which runs on", 3)
	pe = plugNodeRig{link: "vmagent", down: "vmagent", ha: "lab-qemu-202"}.env(t)
	mark(pe)
	plugNodeLocked(t, pe, "seen to go off, Home Assistant in another guest", model.SocketLockHomeAssistant, plugSaidHA)
}

// Finding 1, a second agent of a node: an agent whose entry names a node (links_to: the node's id) while the
// node's row is another agent's. The poller's own words for it are "one in a guest with its host's node:". It
// has a row of its own, and a socket linked to it feeds the node.
func TestPlugSocketLinkedToASecondAgentOfANode(t *testing.T) {
	for _, c := range []struct {
		name       string
		rig        plugNodeRig
		lock, said string
	}{
		{"Home Assistant on the node", plugNodeRig{ha: "lab-node-pve2"}, model.SocketLockHomeAssistant, plugSaidHA},
		{"Home Assistant in a guest of the node", plugNodeRig{ha: "lab-qemu-202"}, model.SocketLockHomeAssistant, plugSaidHA},
		{"the reader on the node", plugNodeRig{otherExtra: "links_to: lab-node-pve2,"}, model.SocketLockAgent, plugSaidReader},
		{"the panel on the node", plugNodeRig{panelHosts: "lab-node-pve2"}, model.SocketLockPanel, plugSaidPanel},
	} {
		c.rig.link = "sideagent"
		plugNodeLocked(t, c.rig.env(t), "sideagent, "+c.name, c.lock, c.said)
	}
	plugNodeAsks(t, plugNodeRig{link: "sideagent"}.env(t), "sideagent, nothing on the node", "SIDEAGENT", "which runs on", 3)
}

// Finding 1: a link that cannot be followed to a machine the panel has a row for is a link to no row: locked,
// on and off. An agent in a guest whose node the answer of Proxmox does not have; and, with the rule alone,
// every way a chain of rows can end.
func TestPlugLinkThatCannotBeFollowedToAMachine(t *testing.T) {
	pe := plugNodeRig{link: "lostagent"}.env(t)
	const said = "Locked. This socket is linked to LOSTAGENT, which does not run on a machine of its own, and the panel cannot find the Proxmox node under it. A socket feeds the node: link it to the node."
	s := pe.socketOf(pe.admin, "other/rack-bar", "node")
	if s["switchable"] != false || s["lock"] != model.SocketLockLinkUnknown || s["note"] != said || s["system_id"] != nil {
		t.Errorf("an agent in a guest without its node: %v", s)
	}
	for _, to := range []string{"off", "on"} {
		r := pe.flip("other/rack-bar", "node", to)
		if msg, _ := plugDetailOf(r); r.code != 409 || msg != said+" Nothing was switched." {
			t.Errorf("%s: %d %s", to, r.code, r.raw)
		}
	}
	if n := len(pe.other.Asked()); n != 0 {
		t.Errorf("the machine was asked %d things", n)
	}
	// what the panel does know on the way is still said first: Home Assistant runs in that guest
	pe = plugNodeRig{link: "lostagent", ha: "lab-qemu-301"}.env(t)
	if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["lock"] != model.SocketLockHomeAssistant || s["note"] != plugSaidHA || s["system_id"] != nil {
		t.Errorf("an agent in the guest Home Assistant runs in, without its node: %v", s)
	}

	// The rule alone (socketFeeds), over rows and links made up here.
	row := func(id, kind, parent string) *model.System {
		r := &model.System{ID: id, Name: strings.ToUpper(id), Kind: kind}
		if parent != "" {
			r.ParentID = model.P(parent)
		}
		return r
	}
	rows := map[string]*model.System{}
	for _, r := range []*model.System{
		row("node-1", "node", ""), row("vm-1", "qemu", "node-1"), row("ct-1", "lxc", "node-1"), row("vm-lost", "qemu", "node-9"), row("vm-bare", "qemu", ""),
		row("vm-odd", "qemu", "pc"), // its parent is a row, and no node
		row("pc", "agent", ""), row("in-vm", "agent", ""), row("in-ct", "agent", ""), row("second", "agent", ""), row("in-lost", "agent", ""),
		row("to-nothing", "agent", ""), row("nodes-own", "agent", ""), row("self", "agent", ""),
		row("loop-a", "agent", ""), row("loop-b", "agent", ""), row("loop-c", "agent", ""), row("via-agent", "agent", ""),
		row("c1", "agent", ""), row("c2", "agent", ""), row("c3", "agent", ""), row("c4", "agent", ""), row("c5", "agent", ""), row("c6", "agent", ""), row("c7", "agent", ""), row("c8", "agent", ""), row("c9", "agent", ""),
	} {
		rows[r.ID] = r
	}
	runs := map[string]string{"in-vm": "vm-1", "in-ct": "ct-1", "second": "node-1", "in-lost": "vm-lost", "to-nothing": "vm-404", "nodes-own": "node-404", "self": "self",
		"loop-a": "loop-b", "loop-b": "loop-c", "loop-c": "loop-a", "via-agent": "in-vm",
		"c1": "c2", "c2": "c3", "c3": "c4", "c4": "c5", "c5": "c6", "c6": "c7", "c7": "c8", "c8": "c9", "c9": "node-1"}
	asked := 0
	feeds := func(start string) (string, string) {
		m, over := socketFeeds(start, func(id string) *model.System { return rows[id] }, func(id string) (string, bool) {
			if asked++; asked > 1000 {
				t.Fatal("the rule does not end")
			}
			return runs[id], id == "nodes-own"
		})
		ids := []string{}
		for _, r := range over {
			ids = append(ids, r.ID)
		}
		if m == nil {
			return "", strings.Join(ids, " ")
		}
		return m.ID, strings.Join(ids, " ")
	}
	for _, c := range []struct{ start, machine, over, what string }{
		{"node-1", "node-1", "", "a node"},
		{"pc", "pc", "", "a machine with an agent that runs on nothing else"},
		{"vm-1", "node-1", "vm-1", "a VM"},
		{"ct-1", "node-1", "ct-1", "a container"},
		{"in-vm", "node-1", "in-vm vm-1", "an agent in a VM"},
		{"in-ct", "node-1", "in-ct ct-1", "an agent in a container"},
		{"second", "node-1", "second", "a second agent of a node"},
		{"via-agent", "node-1", "via-agent in-vm vm-1", "an agent that runs where another agent runs"},
		{"self", "self", "", "an agent whose entry names itself"},
		{"nodes-own", "nodes-own", "", "a node's own agent while the node has no row: the agent's row is the machine"},
		{"nowhere", "", "", "an id without a row"},
		{"vm-lost", "", "vm-lost", "a guest whose node has no row"},
		{"vm-bare", "", "vm-bare", "a guest without a node"},
		{"vm-odd", "", "vm-odd", "a guest whose parent is no node"},
		{"in-lost", "", "in-lost vm-lost", "an agent in a guest whose node has no row"},
		{"to-nothing", "", "to-nothing", "an agent that runs on an id without a row, which is not its own machine"},
		{"loop-a", "", "loop-a loop-b loop-c", "links that lead back to the first row"},
		{"loop-b", "", "loop-b loop-c loop-a", "the same, entered at another row"},
		{"c1", "", "c1 c2 c3 c4 c5 c6 c7 c8", "a chain longer than any real one: not followed to its end"},
		{"c2", "node-1", "c2 c3 c4 c5 c6 c7 c8 c9", "a chain of the longest length that is followed"},
	} {
		if m, over := feeds(c.start); m != c.machine || over != c.over {
			t.Errorf("%s (%s): the machine is %q over %q, want %q over %q", c.what, c.start, m, over, c.machine, c.over)
		}
	}
}

// Finding 1, what must stay as it was: a node's own agent is one machine with the node. While the node has no
// row (the panel was started and that node's Proxmox API has not answered since), the machine is shown as the
// agent's row, and a socket linked to it is that row's: not a link the panel cannot follow.
func TestPlugSocketLinkedToANodesAgentWhileTheNodeHasNoRow(t *testing.T) {
	t.Setenv("TOK_G", testutil.ReadToken+"g")
	yaml := plugConfigWith(t, "desk-bar/ch7: old-pc", "desk-bar/ch7: nas-box")
	yaml = strings.Replace(yaml, "agents:\n", "  - {id: nas, id_prefix: nas-, cluster_name: NAS, standalone: true, urls: [\"https://nas:8006\"],\n"+
		"     token_id: \"fairy@pve!panel\", token_secret_env: PVE_SECRET, verify_tls: false}\nagents:\n"+
		"  - {id: nas-box, name: NAS-BOX, url: \"http://nas-box\", token_env: TOK_G, node: NAS}\n", 1)
	if !strings.Contains(yaml, "id: nas-box") {
		t.Fatal("the rig's config was not changed")
	}
	pe := newPlugEnvWith(t, yaml)
	pe.b.Mu.Lock()
	pe.b.PVEOf = func(cluster string) ([]jsonx.M, error) {
		if cluster == "lab" {
			return testutil.HostResources("PVE", "running"), nil
		}
		return nil, devices.Errf("connection failed") // the NAS's own Proxmox API has not answered since the panel started
	}
	pe.b.Metrics["nas-box"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["hostname"] = "nas-box"
		delete(m, "power_sources")
		return m, nil
	}
	pe.b.Mu.Unlock()
	pe.poll()
	pe.poll()
	if x := pe.do("GET", "/api/systems/nas-node-nas", nil, "Cookie", pe.admin); x.code != 404 {
		t.Fatalf("the fixture: the node has a row: %d %v", x.code, x.body)
	}
	if x := pe.do("GET", "/api/systems/nas-box", nil, "Cookie", pe.admin); x.body["kind"] != "agent" || x.body["status"] != "online" {
		t.Fatalf("the fixture: the row of nas-box: %v", x.body)
	}
	if got := pe.s.Poller.SameMachine("nas-box"); fmt.Sprint(got) != "[nas-box nas-node-nas]" {
		t.Fatalf("the fixture: the ids of the machine are %v", got)
	}
	s := pe.socketOf(pe.admin, "reader/desk-bar", "ch7")
	if s["switchable"] != true || s["lock"] != nil || s["system_id"] != "nas-box" {
		t.Fatalf("the socket for an admin: %v", s)
	}
	r := pe.flip("reader/desk-bar", "ch7", "off")
	if d, _ := r.body["detail"].(map[string]any); r.code != 409 || d["code"] != codeMachineRunning || d["machine"] != "NAS-BOX" || d["system_id"] != "nas-box" || d["guest"] != nil ||
		d["message"] != "NAS-BOX is still running. Shut it down first." {
		t.Errorf("off: %d %s", r.code, r.raw)
	}
}

// Finding 2 of the third review: three changes of its own to the lines of the second round passed the whole
// suite. The code was right in each; no test asked. A test for each (the fourth, a read of the state file's
// version without its lock, is in internal/adminstate).

// M03: a container is a guest as a VM is. No test linked a socket to a container, or had the reader or Home
// Assistant in one. What a socket that is linked to a container feeds is the container's node; and the node of
// a container that carries Home Assistant, the reader or the panel is locked like the node of such a VM.
func TestPlugAContainerIsAGuestLikeAVM(t *testing.T) {
	plugNodeAsks(t, plugNodeRig{link: "lab-lxc-203"}.env(t), "linked to a container", "ct-box", "a guest of", 3)
	for _, c := range []struct {
		name       string
		rig        plugNodeRig
		lock, said string
	}{
		{"linked to a container, Home Assistant in a VM of its node", plugNodeRig{link: "lab-lxc-203", ha: "lab-qemu-202"}, model.SocketLockHomeAssistant, plugSaidHA},
		{"linked to the node, Home Assistant in a container", plugNodeRig{link: "lab-node-pve2", ha: "lab-lxc-203"}, model.SocketLockHomeAssistant, plugSaidHA},
		{"linked to a VM, Home Assistant in a container", plugNodeRig{link: "lab-qemu-201", ha: "lab-lxc-203"}, model.SocketLockHomeAssistant, plugSaidHA},
		{"linked to the node, the reader in a container", plugNodeRig{link: "lab-node-pve2", otherExtra: "links_to: lab-lxc-203,"}, model.SocketLockAgent, plugSaidReader},
		{"linked to a VM, the reader in a container", plugNodeRig{link: "lab-qemu-201", otherExtra: "links_to: lab-lxc-203,"}, model.SocketLockAgent, plugSaidReader},
		{"linked to a container, the panel in a VM of its node", plugNodeRig{link: "lab-lxc-203", panelHosts: "lab-qemu-202"}, model.SocketLockPanel, plugSaidPanel},
		{"linked to a VM, the panel in a container", plugNodeRig{link: "lab-qemu-201", panelHosts: "lab-lxc-203"}, model.SocketLockPanel, plugSaidPanel},
	} {
		plugNodeLocked(t, c.rig.env(t), c.name, c.lock, c.said)
	}
	// a container whose node the panel does not have: locked, with the sentence for a guest
	pe := plugNodeRig{link: "lab-lxc-302"}.env(t)
	const said = "Locked. This socket is linked to lost-ct, a virtual machine or container, and the panel does not have the Proxmox node it runs on. A socket feeds the node: link it to the node."
	if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["switchable"] != false || s["lock"] != model.SocketLockLinkUnknown || s["note"] != said || s["system_id"] != nil {
		t.Errorf("a container without its node: %v", s)
	}
	for _, to := range []string{"off", "on"} {
		r := pe.flip("other/rack-bar", "node", to)
		if msg, _ := plugDetailOf(r); r.code != 409 || msg != said+" Nothing was switched." {
			t.Errorf("a container without its node, %s: %d %s", to, r.code, r.raw)
		}
	}
	if n := len(pe.other.Asked()); n != 0 {
		t.Errorf("a container without its node: the machine was asked %d things", n)
	}
}

// M05: Home Assistant's lock sees the node of a linked guest. The second round's test for "a guest beside Home
// Assistant's VM on one node" used the node that also carries the reader, so the reader's lock came first and
// hid a missing one. Here the reader runs on a machine of its own, and nothing but Home Assistant's VM is on
// the node: the lock is Home Assistant's, for a link to a VM, to a container and to the node.
func TestPlugGuestBesideHomeAssistantsVMOnANodeWithoutTheReader(t *testing.T) {
	for _, link := range []string{"lab-qemu-201", "lab-lxc-203", "lab-node-pve2"} {
		pe := plugNodeRig{link: link, ha: "lab-qemu-202"}.env(t)
		if own := pe.s.Poller.ShownID("other"); own != "other" {
			t.Fatalf("the fixture: the reader is shown as %s", own)
		}
		plugNodeLocked(t, pe, "linked to "+link+", Home Assistant in lab-qemu-202", model.SocketLockHomeAssistant, plugSaidHA)
	}
}

// M06: the node's other id is on the linked side. A node with an agent is one machine with two ids, the node's
// and the agent's, and panel_hosts can name either. With the agent's id there, the socket of the node is the
// panel's: linked to the node, to a guest of it, to an agent in such a guest, or to the agent's id itself.
// (Since finding 3 it is the panel's side that holds both ids, panelRows, and the line of the linked side that
// M06 took out is gone. What this test asks is the same.)
func TestPlugPanelHostsThatNamesTheNodesOwnAgent(t *testing.T) {
	for _, link := range []string{"lab-node-pve2", "lab-qemu-201", "lab-lxc-203", "vmagent", "pve2agent"} {
		pe := plugNodeRig{link: link, panelHosts: "pve2agent", lone: true}.env(t)
		plugNodeLocked(t, pe, "linked to "+link+", panel_hosts names the node's agent", model.SocketLockPanel, plugSaidPanel)
	}
}

// Finding 3 of the third review (old): the panel's own lock went by the ids in panel_hosts as they are written,
// and by the guests of a node that are named there. An agent that runs in a guest is no guest: with its id in
// panel_hosts the socket of the node under it had no lock, and a cut with the node's name typed went to the
// passkey. The reader's side and Home Assistant's were widened to the machine under an agent; the panel's is
// now too, by the same rule as a link (socketFeeds).
//
// With the two arrangements that finding 1 left for here: the panel in the node's own agent while a second
// agent names the node (so that the node has no "other id"), and the panel in an agent in another guest.
func TestPlugPanelHostsThatNamesAnAgent(t *testing.T) {
	for _, c := range []struct{ panel, what string }{
		{"vm2agent", "an agent in a VM of the node"},
		{"ctagent", "an agent in a container of the node"},
		{"pve2agent", "the node's own agent, where a second agent names the node too"},
		{"sideagent", "a second agent of the node"},
		{"viaagent", "an agent whose entry names an agent in a VM of the node"},
	} {
		for _, link := range []string{"lab-node-pve2", "lab-qemu-201", "lab-qemu-202", "lab-lxc-203", "vmagent", "ctagent", "vm2agent", "sideagent", "pve2agent", "viaagent"} {
			pe := plugNodeRig{link: link, panelHosts: c.panel}.env(t)
			plugNodeLocked(t, pe, "panel_hosts names "+c.what+", linked to "+link, model.SocketLockPanel, plugSaidPanel)
		}
	}
	// The panel in an agent in a guest whose node the panel does not have. The socket of that guest is the
	// panel's, said before "the link cannot be followed"; the node PVE2 is not that node, and its socket only
	// asks the question every "off" for a running machine asks.
	pe := plugNodeRig{link: "lab-qemu-301", panelHosts: "lostagent"}.env(t)
	if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["switchable"] != false || s["lock"] != model.SocketLockPanel || s["note"] != plugSaidPanel {
		t.Errorf("the panel in an agent in a guest without its node, linked to that guest: %v", s)
	}
	for _, to := range []string{"off", "on"} {
		r := pe.flip("other/rack-bar", "node", to)
		if msg, _ := plugDetailOf(r); r.code != 409 || msg != plugSaidPanel+" Nothing was switched." {
			t.Errorf("the panel in an agent in a guest without its node, %s: %d %s", to, r.code, r.raw)
		}
	}
	pe = plugNodeRig{link: "lab-node-pve2", panelHosts: "lostagent"}.env(t)
	if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["switchable"] != true || s["lock"] != nil || s["system_id"] != "lab-node-pve2" {
		t.Errorf("the panel in an agent in a guest of another node, the socket of PVE2: %v", s)
	}
	if r := pe.flip("other/rack-bar", "node", "off"); r.code != 409 || r.body["detail"].(map[string]any)["code"] != codeMachineRunning {
		t.Errorf("the panel in an agent in a guest of another node, off for PVE2: %d %s", r.code, r.raw)
	}
	// and a machine that has nothing to do with the panel's node keeps its socket: PVE, with the panel on PVE2
	for _, panel := range []string{"vm2agent", "ctagent", "pve2agent", "sideagent", "viaagent"} {
		pe := plugNodeRig{link: "lab-node-pve", panelHosts: panel}.env(t)
		if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["switchable"] != true || s["lock"] != nil || s["system_id"] != "lab-node-pve" {
			t.Errorf("panel_hosts names %s, the socket of the other node: %v", panel, s)
		}
	}
}

// Every side of a lock goes down to the machine in the same way. The reader's side and Home Assistant's took one
// step from an agent (to what its entry names) and one from a guest (to its node); the link of a socket is
// followed as far as it goes since finding 1. An entry that names another agent, which runs in a guest, is two
// steps: the sides take them too now (machine), so that such a reader, such a Home Assistant and such a panel
// lock the socket of the node, and a socket that is linked to such an agent is the node's.
func TestPlugEverySideOfALockGoesDownToTheMachine(t *testing.T) {
	plugNodeAsks(t, plugNodeRig{link: "viaagent"}.env(t), "linked to an agent whose entry names an agent in a VM", "VIAAGENT", "which runs on", 3)
	for _, c := range []struct {
		name       string
		rig        plugNodeRig
		lock, said string
	}{
		{"linked to such an agent, Home Assistant in another guest", plugNodeRig{link: "viaagent", ha: "lab-qemu-202"}, model.SocketLockHomeAssistant, plugSaidHA},
		{"linked to the node, Home Assistant in such an agent", plugNodeRig{link: "lab-node-pve2", ha: "viaagent"}, model.SocketLockHomeAssistant, plugSaidHA},
		{"linked to a container, Home Assistant in such an agent", plugNodeRig{link: "lab-lxc-203", ha: "viaagent"}, model.SocketLockHomeAssistant, plugSaidHA},
		{"linked to the node, the reader's entry names such an agent", plugNodeRig{link: "lab-node-pve2", otherExtra: "links_to: viaagent,"}, model.SocketLockAgent, plugSaidReader},
		{"linked to another guest, the reader's entry names such an agent", plugNodeRig{link: "lab-qemu-202", otherExtra: "links_to: viaagent,"}, model.SocketLockAgent, plugSaidReader},
	} {
		plugNodeLocked(t, c.rig.env(t), c.name, c.lock, c.said)
	}
	// What has no row right now still counts as it is written, on each side. The reader's entry names a guest
	// the panel does not have, and the socket is linked to that id; Home Assistant, and the panel, run in a guest
	// whose node the panel does not have, and the socket is linked to that node's id. Each is a link the panel
	// cannot follow, and the lock says the nearer reason.
	for _, c := range []struct {
		name       string
		rig        plugNodeRig
		lock, said string
	}{
		{"the reader in a guest without a row, linked to that id", plugNodeRig{link: "lab-qemu-999", otherExtra: "links_to: lab-qemu-999,"}, model.SocketLockAgent, plugSaidReader},
		{"Home Assistant in a guest whose node has no row, linked to that node's id", plugNodeRig{link: "lab-node-ghost", ha: "lab-qemu-301"}, model.SocketLockHomeAssistant, plugSaidHA},
		{"the panel in a guest whose node has no row, linked to that node's id", plugNodeRig{link: "lab-node-ghost", panelHosts: "lab-qemu-301"}, model.SocketLockPanel, plugSaidPanel},
		// two agents on the way to a guest whose node has no row: the rows on the way count
		{"Home Assistant's entry names an agent in a guest without its node, linked to that guest", plugNodeRig{link: "lab-qemu-301", ha: "via2agent"}, model.SocketLockHomeAssistant, plugSaidHA},
		{"the panel's entry names an agent in a guest without its node, linked to that guest", plugNodeRig{link: "lab-qemu-301", panelHosts: "via2agent"}, model.SocketLockPanel, plugSaidPanel},
	} {
		pe := c.rig.env(t)
		if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["switchable"] != false || s["lock"] != c.lock || s["note"] != c.said || s["system_id"] != nil {
			t.Errorf("%s: %v", c.name, s)
		}
		for _, to := range []string{"off", "on"} {
			r := pe.flip("other/rack-bar", "node", to)
			if msg, _ := plugDetailOf(r); r.code != 409 || msg != c.said+" Nothing was switched." {
				t.Errorf("%s, %s: %d %s", c.name, to, r.code, r.raw)
			}
		}
		if n := len(pe.other.Asked()); n != 0 {
			t.Errorf("%s: the machine was asked %d things", c.name, n)
		}
	}
}

// Finding 4 of the third review (old): the note of a switched socket has the system of the machine ON the
// socket, which is what decides who sees it, and as `host` the name of the machine that READS the plug. A
// viewer of the site of the first who may not see the second got that name: of a machine of another site.
// Such a viewer, and a read token that is limited the same way, now gets the name of the machine on the socket
// there. An admin, and everybody who sees every site, gets what they got.
func TestPlugSwitchNoteDoesNotNameTheReaderToWhoCannotSeeIt(t *testing.T) {
	// the reader is in the site Lab; the PC on its socket ch3 is in Home
	yaml := strings.Replace(plugConfig, "node: PVE, site: Home,\n     power_links: {pc-plug: desk-pc},", "node: PVE, site: Lab,\n     power_links: {pc-plug: desk-pc},", 1)
	if yaml == plugConfig {
		t.Fatal("the rig's config was not changed")
	}
	pe := newPlugEnvWith(t, yaml)
	b := plugBody("reader/desk-bar", "ch3", "off")
	b["cut"], b["confirm"] = true, "DESK-PC"
	id, _, ok := approvalAsked(pe.post("/api/power/socket", b))
	if !ok {
		t.Fatal("the fixture: no approval was asked for the cut")
	}
	if cb := pe.approve(t, id); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if pr := pe.result(t); pr["ok"] != true {
		t.Fatalf("the fixture: the switch: %v", pr)
	}
	token := func(sites ...string) []string {
		tok, _, err := pe.s.Tokens.CreateForSites("t"+strings.Join(sites, "-"), "system", "read", "cli:test", 0, sites)
		if err != nil {
			t.Fatal(err)
		}
		return []string{"Authorization", "Bearer " + tok}
	}
	// the note as somebody gets it from GET /api/notifications (nil: it is not in the answer), and the answer
	note := func(hdr []string) (map[string]any, string) {
		t.Helper()
		r := pe.do("GET", "/api/notifications", nil, hdr...)
		if r.code != 200 {
			t.Fatalf("GET /api/notifications: %d %s", r.code, r.raw)
		}
		for _, x := range r.body["events"].([]any) {
			if e := x.(map[string]any); e["kind"] == "plug_switched" {
				return e, string(r.raw)
			}
		}
		return nil, string(r.raw)
	}
	const text = "Desk PC (Monitor powerbar) switched off by " + plugActor + ": power cut while DESK-PC was running"
	home := pe.viewerOf("Home")
	for who, hdr := range map[string][]string{"a viewer of the site of the PC": {"Cookie", home}, "a read token of the site of the PC": token("Home")} {
		if x := pe.do("GET", "/api/systems/reader", nil, hdr...); x.code != 404 {
			t.Fatalf("the fixture: %s sees the reader: %d", who, x.code)
		}
		e, raw := note(hdr)
		if e == nil || e["system_id"] != "desk-pc" || e["text"] != text {
			t.Fatalf("%s: the note of the switch: %v\n%s", who, e, raw)
		}
		if e["host"] != "DESK-PC" || strings.Contains(raw, "READER") {
			t.Errorf("%s: the note has host %v, want the machine on the socket and the reader's name nowhere:\n%s", who, e["host"], raw)
		}
	}
	// admins, and a read token that sees every site: as before, the machine that reads the plug
	all, _, err := pe.s.Tokens.Create("t-all", "system", "read", "cli:test", 0)
	if err != nil {
		t.Fatal(err)
	}
	for who, hdr := range map[string][]string{"an admin": {"Cookie", pe.admin}, "a read token without sites": {"Authorization", "Bearer " + all}} {
		if e, raw := note(hdr); e == nil || e["host"] != "READER" || e["system_id"] != "desk-pc" || e["text"] != text {
			t.Errorf("%s: the note of the switch: %v\n%s", who, e, raw)
		}
	}
	// somebody who sees the reader and not the PC does not get the note at all (it is the PC's)
	if e, raw := note(token("Lab")); e != nil {
		t.Errorf("a read token of the site of the reader has the note of a socket of another site: %s", raw)
	}

	// The stream sends new notes too, the ones that are warnings or worse. A switch is none, so no note of a
	// switch is on it today; the name is taken out there all the same, should one become urgent. A note of the
	// kind, made here as a warning, for a stream that is opened again after the last note it had:
	e, _ := note([]string{"Cookie", pe.admin})
	last := fmt.Sprint(int64(e["id"].(float64)))
	pe.s.Notes.Add(notifications.Event{Type: "power", Severity: notifications.Warning, Kind: "plug_switched", Host: "READER", SystemID: "desk-pc", Text: "made for the stream"})
	code, stream := pe.streamBody("/api/stream", "Cookie", home, "Last-Event-ID", last)
	if code != 200 || !strings.Contains(stream, "made for the stream") {
		t.Fatalf("the fixture: the viewer's stream (%d) has not the note", code)
	}
	if !strings.Contains(stream, `"host":"DESK-PC","text":"made for the stream"`) || strings.Contains(stream, "READER") {
		t.Errorf("the viewer's stream names the reader, or not the machine on the socket, in the note:\n%s", stream[strings.Index(stream, "event: notes_new"):])
	}
	if _, stream = pe.streamBody("/api/stream", "Cookie", pe.admin, "Last-Event-ID", last); !strings.Contains(stream, `"host":"READER","text":"made for the stream"`) {
		t.Errorf("the admin's stream has not the note with the reader's name")
	}
	// and a note of another kind keeps its host for the viewer: it is the name of the note's own machine
	pe.s.Notes.Add(notifications.Event{Type: "host", Severity: notifications.Warning, Kind: "temp_high", Host: "DESK-PC as it was called", SystemID: "desk-pc", Text: "another kind"})
	r := pe.do("GET", "/api/notifications", nil, "Cookie", home)
	if !strings.Contains(string(r.raw), `"host":"DESK-PC as it was called"`) {
		t.Errorf("a note of another kind lost its host for the viewer: %s", r.raw)
	}
	if _, stream = pe.streamBody("/api/stream", "Cookie", home, "Last-Event-ID", last); !strings.Contains(stream, `"host":"DESK-PC as it was called","text":"another kind"`) {
		t.Errorf("the viewer's stream has not the other note with its host")
	}
}
