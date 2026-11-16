package web

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// wayRig is the plug rig with more agents (their entries, as lines of the configuration; each answers as a PC
// without plugs), the link of the socket rack-bar/node, home_assistant_system of the agent "other", and
// panel_hosts.
func wayRig(t *testing.T, agents map[string]string, link, ha, panelHosts string) *plugEnv {
	t.Helper()
	return wayRigWith(t, agents, link, ha, panelHosts, nil)
}

// wayRigWith is wayRig with a second proxmox: entry (nas, id_prefix nas-, the node NAS) whose API answers while
// *nas is not empty, with the VM 555 on the node *nas names (nil: no such entry). With *nas empty it has not
// answered when the rig is handed out.
func wayRigWith(t *testing.T, agents map[string]string, link, ha, panelHosts string, nas *string) *plugEnv {
	t.Helper()
	for _, want := range []string{"\nagents:\n", "socket_links: {rack-bar/node: lab-node-pve}, home_assistant_system: lab-qemu-100}", "panel_hosts: [panel-box]"} {
		if strings.Count(plugConfig, want) != 1 {
			t.Fatal("the rig's config is not the one this fixture changes")
		}
	}
	ids := make([]string, 0, len(agents))
	for id := range agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var lines strings.Builder
	for i, id := range ids {
		env := fmt.Sprintf("TOK_W%d", i)
		t.Setenv(env, testutil.ReadToken+fmt.Sprintf("w%d", i))
		fmt.Fprintf(&lines, "  - {id: %s, name: %s, url: \"http://%s\", token_env: %s%s}\n", id, strings.ToUpper(id), id, env, agents[id])
	}
	entry := ""
	if nas != nil {
		entry = "\n  - {id: nas, id_prefix: nas-, cluster_name: NAS, standalone: true, urls: [\"https://nas:8006\"],\n" +
			"     token_id: \"fairy@pve!panel\", token_secret_env: PVE_SECRET, verify_tls: false}"
	}
	yaml := strings.Replace(plugConfig, "\nagents:\n", entry+"\nagents:\n"+lines.String(), 1)
	yaml = strings.Replace(yaml, "socket_links: {rack-bar/node: lab-node-pve}, home_assistant_system: lab-qemu-100}", "socket_links: {rack-bar/node: "+link+"}, home_assistant_system: "+ha+"}", 1)
	pe := newPlugEnvWith(t, strings.Replace(yaml, "panel_hosts: [panel-box]", "panel_hosts: ["+panelHosts+"]", 1))
	pe.b.Mu.Lock()
	pe.b.PVEOf = func(cluster string) ([]jsonx.M, error) {
		switch {
		case cluster == "lab":
			return testutil.HostResources("PVE", "running"), nil
		case nas != nil && *nas != "":
			return append(testutil.HostResources("NAS", "running"), jsonx.M{"id": "qemu/555", "type": "qemu", "vmid": 555.0, "name": "panel-vm", "node": *nas, "status": "running",
				"cpu": 0.0, "maxcpu": 4.0, "mem": 0.0, "maxmem": float64(8 << 30), "disk": 0.0, "maxdisk": float64(64 << 30), "template": 0.0}), nil
		}
		return nil, devices.Errf("connection failed")
	}
	for _, id := range ids {
		pe.b.Metrics[id] = func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			m["hostname"] = id
			delete(m, "power_sources")
			return m, nil
		}
	}
	pe.b.Mu.Unlock()
	pe.poll()
	pe.poll()
	return pe
}

// wayLook is what an admin is told in a rig, with the id of one agent written as "<id>": every socket (state,
// switch, lock, sentence, what it is linked to), every power menu, and every line of "Update unpowered hosts".
func wayLook(t *testing.T, pe *plugEnv, id string) []string {
	t.Helper()
	var out []string
	as := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, strings.ToUpper(id), "<ID>"), id, "<id>")
	}
	power := pe.do("GET", "/api/power", nil, "Cookie", pe.admin)
	if power.code != 200 {
		t.Fatalf("/api/power: %d %s", power.code, power.raw)
	}
	for _, src := range power.body["sources"].([]any) {
		m := src.(map[string]any)
		socks, _ := m["sockets"].([]any)
		for _, s := range socks {
			x := s.(map[string]any)
			out = append(out, as(fmt.Sprintf("socket %v/%v: state=%v switchable=%v lock=%v why=%v system_id=%v note=%v", m["id"], x["id"], x["state"], x["switchable"], x["lock"], x["why"], x["system_id"], x["note"])))
		}
	}
	look := panelLookOf(t, pe)
	for row := range look.menu {
		r := menuOf(pe, row)
		out = append(out, as(fmt.Sprintf("menu %s (%s): kills_panel=%v panel_unknown=%v warning=%v asks=%s", row, look.kinds[row], r.body["kills_panel"], r.body["panel_unknown"], r.body["warning"], powerAsksOf(r))))
	}
	for row, line := range look.run {
		out = append(out, as(fmt.Sprintf("run %s: can_join=%v can_wake=%v why_not=%v", row, line["can_join"], line["can_wake"], line["why_not"])))
	}
	sort.Strings(out)
	return out
}

// An id names two things only when it is exactly the id of two things that exist: an agent, and a node or a
// guest the Proxmox API lists. An agent whose id only begins like a node's id of a proxmox: entry
// (<id_prefix>node-...), or is written like a guest's id where there is no such guest, counted too: its
// sockets were locked after the update, with a sentence about a node that is not there, and panel_hosts or
// home_assistant_system with it failed closed.
//
// The agents here have ids as a home setup has them (<site>-<node>-agent next to the node <site>-node-<node>),
// and ids that look like a node's or a guest's. In every place such an id can stand (the link of a socket,
// home_assistant_system, panel_hosts) the panel says exactly what it says for an agent with a plain id.
func TestAnAgentIDThatOnlyLooksLikeAnotherIDLocksNothing(t *testing.T) {
	type place struct{ link, ha, panel string }
	places := map[string]func(id string) place{
		"a socket is linked to it":           func(id string) place { return place{id, "lab-qemu-100", "panel-box"} },
		"home_assistant_system names it":     func(id string) place { return place{"lab-node-pve", id, "panel-box"} },
		"panel_hosts names it":               func(id string) place { return place{"lab-node-pve", "lab-qemu-100", id} },
		"all three name it":                  func(id string) place { return place{id, id, id} },
		"an agent that runs on it is linked": func(id string) place { return place{"rides", "lab-qemu-100", "rides"} },
	}
	rig := func(id string, p place) *plugEnv {
		return wayRig(t, map[string]string{id: "", "rides": ", links_to: " + id}, p.link, p.ha, p.panel)
	}
	for what, at := range places {
		plain := wayLook(t, rig("plainbox", at("plainbox")), "plainbox")
		for _, l := range plain {
			if strings.Contains(l, "plainbox") || (strings.Contains(l, "<id>") && strings.Contains(l, "lock=link_unknown")) || strings.Contains(l, "names two things") {
				t.Fatalf("%s, the fixture: with a plain id the panel says: %s", what, l)
			}
		}
		for _, id := range []string{
			"lab-pve-agent",                                                 // as a home setup names the agent of the node PVE
			"lab-node-red",                                                  // begins like a node's id; there is no node "red"
			"lab-node-pve-agent",                                            // ... no node "pve-agent"
			"lab-node-pve-100",                                              // ... no node "pve-100"
			"lab-qemu-777",                                                  // written like a VM's id; there is no VM 777
			"lab-lxc-100",                                                   // written like a container's id; 100 is a VM
			"lab-qemu-1000",                                                 //
			"lab-qemu-box", "lab-nodered", "lab-qemu-100x", "xlab-qemu-100", // not written like one at all
		} {
			pe := rig(id, at(id))
			if pe.s.Poller.NamesTwo(id) {
				t.Errorf("%s: the agent %s counts as an id that names two things", what, id)
			}
			got := wayLook(t, pe, id)
			if len(got) != len(plain) {
				t.Errorf("%s: with the agent %s the panel has %d lines, with a plain id %d", what, id, len(got), len(plain))
				continue
			}
			for i := range got {
				if got[i] != plain[i] {
					t.Errorf("%s: with the agent %s the panel says\n      %s\n   with a plain id\n      %s", what, id, got[i], plain[i])
				}
			}
			if ids := pe.s.Poller.PanelUnknown(pe.s.Poller.Snapshot()); ids != nil {
				t.Errorf("%s: with the agent %s the panel cannot follow %v", what, id, ids)
			}
		}
	}
}

// The true case stays: an agent whose id is exactly the id of a VM the Proxmox API lists (lab-qemu-100, the VM
// 100 on PVE), and one whose id is exactly the node's (lab-node-pve). A socket linked to it is locked with a
// sentence that is true, and panel_hosts with it leaves the panel without a machine it knows, with the reason
// said. Before that Proxmox API has answered there is one thing with the id, the agent: nothing is locked
// for the look of the id.
func TestAnIDOfTwoThingsThatExist(t *testing.T) {
	for _, id := range []string{"lab-qemu-100", "lab-node-pve"} {
		pe := wayRig(t, map[string]string{id: ""}, id, "desk-pc", "panel-box")
		if !pe.s.Poller.NamesTwo(id) {
			t.Fatalf("the fixture: %s is an agent's id and a listed node's or guest's, and does not count as two things", id)
		}
		said := "Locked. This socket is linked to " + id + ", and that id names two things: an agent, and a node or a guest of a Proxmox entry. The panel cannot tell which is meant. Give the agent another id."
		if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["switchable"] != false || s["lock"] != model.SocketLockLinkUnknown || s["note"] != said {
			t.Errorf("a socket linked to %s: %v", id, s)
		}
		// an agent whose links_to names that id: the way ends at it, and the sentence names both ids
		pe = wayRig(t, map[string]string{id: "", "rides": ", links_to: " + id}, "rides", "desk-pc", "panel-box")
		said = "Locked. This socket is linked to rides, and rides leads to " + id + ", which is the id of an agent and of a node or a guest the Proxmox API lists, and the panel cannot tell which is meant. Give the agent " + id + " another id."
		if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["switchable"] != false || s["lock"] != model.SocketLockLinkUnknown || s["note"] != said {
			t.Errorf("a socket linked to an agent that runs on %s: %v", id, s)
		}
		// ... and one step further: the way ends at the id as it is written, before any row is asked
		pe = wayRig(t, map[string]string{id: "", "rides": ", links_to: " + id, "far": ", links_to: rides"}, "far", "desk-pc", "panel-box")
		said = strings.Replace(said, "linked to rides, and rides leads", "linked to far, and far leads", 1)
		if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["switchable"] != false || s["lock"] != model.SocketLockLinkUnknown || s["note"] != said {
			t.Errorf("a socket linked to an agent two steps from %s: %v", id, s)
		}
		// panel_hosts with it: the state, with the reason that is true for it and what ends it
		pe = wayRig(t, map[string]string{id: ""}, "desk-pc", "panel-box", id)
		reason := id + " is the id of an agent and of a node or a guest the Proxmox API lists, and the panel cannot tell which is meant"
		ends := "config.yaml is corrected (give the agent " + id + " another id)"
		warning, whyNot := panelUnknownSaidFor(id, reason, ends)
		if r := menuOf(pe, "desk-pc"); r.body["panel_unknown"] != true || r.body["warning"] != warning {
			t.Errorf("panel_hosts names %s: the power menu of another host: %s", id, r.raw)
		}
		lock := "Locked. The panel cannot follow panel_hosts (" + id + ") to a machine right now: " + reason + ". So it cannot tell which machine it runs on, and switches no socket off. This ends when " + ends + "."
		if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["state"] != "on" || s["lock"] != model.SocketLockPanelUnknown || s["note"] != lock {
			t.Errorf("panel_hosts names %s: a socket that is on: %v", id, s)
		}
		if line := panelLookOf(t, pe).run["desk-pc"]; line["can_join"] != false || line["why_not"] != whyNot {
			t.Errorf("panel_hosts names %s: the line of another host in Update unpowered hosts: %v", id, line)
		}
	}
	// An agent with the id of the VM 100 that is a part of the node's row (node: PVE: the first agent of the
	// configuration that names the node): the id is shown as the node's row, and names two things all the same.
	// A way that meets it ends there, and does not go on to the node.
	pe := wayRig(t, map[string]string{"lab-qemu-100": ", node: PVE", "rides": ", links_to: lab-qemu-100", "far": ", links_to: rides"}, "far", "desk-pc", "panel-box")
	if x := pe.s.Poller.ShownID("lab-qemu-100"); x != "lab-node-pve" || !pe.s.Poller.NamesTwo("lab-qemu-100") {
		t.Fatalf("the fixture: the agent lab-qemu-100 is shown as %s, names two things: %v", x, pe.s.Poller.NamesTwo("lab-qemu-100"))
	}
	if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["lock"] != model.SocketLockLinkUnknown || s["system_id"] != nil ||
		s["note"] != "Locked. This socket is linked to far, and far leads to lab-qemu-100, which is the id of an agent and of a node or a guest the Proxmox API lists, and the panel cannot tell which is meant. Give the agent lab-qemu-100 another id." {
		t.Errorf("a socket linked to an agent two steps from an id of two things that is shown as its node's row: %v", s)
	}
	// A Proxmox API that has not answered since the panel started: the VM 555 of NAS is not known, and the id
	// nas-qemu-555 is the agent's alone. Nothing is locked for how the id is written. When the API lists the VM,
	// the id names two things.
	answers := ""
	pe = wayRigWith(t, map[string]string{"nas-qemu-555": ""}, "nas-qemu-555", "desk-pc", "nas-qemu-555", &answers)
	if x := pe.do("GET", "/api/systems/nas-qemu-555", nil, "Cookie", pe.admin); x.code != 200 || x.body["kind"] != "agent" {
		t.Fatalf("the fixture: the row nas-qemu-555 before the API of NAS answered: %d %s", x.code, clipText(string(x.raw), 200))
	}
	if pe.s.Poller.NamesTwo("nas-qemu-555") {
		t.Error("before the Proxmox API answered, an agent's id counts as two things by how it is written")
	}
	if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["lock"] != model.SocketLockPanel || strings.Contains(fmt.Sprint(s["note"]), "two things") {
		t.Errorf("before the Proxmox API answered, a socket linked to the agent that panel_hosts names: %v", s)
	}
	if ids := pe.s.Poller.PanelUnknown(pe.s.Poller.Snapshot()); ids != nil {
		t.Errorf("before the Proxmox API answered: the panel cannot follow %v", ids)
	}
	if r := menuOf(pe, "desk-pc"); r.body["panel_unknown"] != false || r.body["warning"] != nil {
		t.Errorf("before the Proxmox API answered: the power menu of another host: %s", r.raw)
	}
	pe.b.Mu.Lock()
	answers = "NAS"
	pe.b.Mu.Unlock()
	pe.poll()
	pe.poll()
	if !pe.s.Poller.NamesTwo("nas-qemu-555") {
		t.Error("the Proxmox API lists the VM 555, and the agent's id nas-qemu-555 does not count as two things")
	}
	// (the socket that is linked to the id panel_hosts names stays the panel's, as the id is written)
	if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["lock"] != model.SocketLockPanel || s["switchable"] != false {
		t.Errorf("the Proxmox API lists the VM: a socket linked to the id: %v", s)
	}
	if r := menuOf(pe, "desk-pc"); r.body["panel_unknown"] != true || !strings.Contains(fmt.Sprint(r.body["warning"]), "nas-qemu-555 is the id of an agent and of a node or a guest the Proxmox API lists") {
		t.Errorf("the Proxmox API lists the VM: the power menu of another host: %s", r.raw)
	}
	if ids := pe.s.Poller.PanelUnknown(pe.s.Poller.Snapshot()); len(ids) != 1 || ids[0] != "nas-qemu-555" {
		t.Errorf("the Proxmox API lists the VM: the ids the panel cannot follow: %v", ids)
	}
}

// chainOf are the entries of n agents chain0 ... chain<n-1>, each with links_to the next, the last one to end.
func chainOf(n int, end string) map[string]string {
	agents := map[string]string{}
	for i := 0; i < n; i++ {
		to := fmt.Sprintf("chain%d", i+1)
		if i == n-1 {
			to = end
		}
		agents[fmt.Sprintf("chain%d", i)] = ", links_to: " + to
	}
	return agents
}

// links_to that names a node's own agent by the agent's id (not the node by the node's) left the panel without
// a machine it knows, for good and with every row there: the way went on at the id as it is written, where no
// row is (a node's own agent is a part of its node's row), while the rows the panel marks as its own, which are
// looked up by what an id is shown as, had the node. Both go by what an id is shown as now: the way is followed
// to the node, over at most eight agents, whether the last one names a node or a guest.
func TestAWayOverANodesOwnAgentIsFollowedToTheNode(t *testing.T) {
	for _, end := range []string{"lab-node-pve", "lab-qemu-100"} {
		for _, n := range []int{1, 2, 3, 7, 8, 9, 10} {
			what := fmt.Sprintf("%d agents, each with links_to the next, the last to %s", n, end)
			// (chain<n-1> is the first agent of the configuration that names the node: the node's row is its own)
			pe := wayRig(t, chainOf(n, end), "chain0", "desk-pc", "chain0")
			if end == "lab-node-pve" {
				if x := pe.do("GET", fmt.Sprintf("/api/systems/chain%d", n-1), nil, "Cookie", pe.admin); x.code != 404 {
					t.Fatalf("%s, the fixture: the node's own agent has a row of its own: %d", what, x.code)
				}
			}
			snap := pe.s.Poller.Snapshot()
			unknown, rows := pe.s.Poller.PanelUnknown(snap), pe.s.Poller.PanelRows(snap)
			node, sock, pc := menuOf(pe, "lab-node-pve"), pe.socketOf(pe.admin, "other/rack-bar", "node"), menuOf(pe, "desk-pc")
			if n <= 8 {
				if unknown != nil || !rows["lab-node-pve"] {
					t.Errorf("%s: the panel cannot follow %v; it marks the node: %v", what, unknown, rows["lab-node-pve"])
				}
				if node.body["kills_panel"] != true || pc.body["panel_unknown"] != false || pc.body["kills_panel"] != false {
					t.Errorf("%s: the power menu of the node: %s; of a PC: %s", what, node.raw, pc.raw)
				}
				if sock["lock"] != model.SocketLockPanel || sock["system_id"] != "lab-node-pve" {
					t.Errorf("%s: the socket linked to chain0: %v", what, sock)
				}
				continue
			}
			reason := "links_to leads from chain0 over more than 8 agents"
			ends := "config.yaml is corrected (write the node or the guest the agent runs on in links_to)"
			warning, whyNot := panelUnknownSaidFor("chain0", reason, ends)
			if len(unknown) != 1 || unknown[0] != "chain0" || rows["lab-node-pve"] {
				t.Errorf("%s: the panel cannot follow %v; it marks the node: %v", what, unknown, rows["lab-node-pve"])
			}
			if pc.body["panel_unknown"] != true || pc.body["warning"] != warning {
				t.Errorf("%s: the power menu of a PC: %s", what, pc.raw)
			}
			if line := panelLookOf(t, pe).run["desk-pc"]; line["can_join"] != false || line["why_not"] != whyNot {
				t.Errorf("%s: the line of a PC in Update unpowered hosts: %v", what, line)
			}
			// a socket that is linked to such a way, in a rig whose panel knows its machine
			pe = wayRig(t, chainOf(n, end), "chain0", "desk-pc", "panel-box")
			said := "Locked. This socket is linked to chain0, and links_to leads from chain0 over more than 8 agents: the panel does not follow that to a machine. " +
				"Write the node or the guest the agent runs on in links_to, or link the socket to the node."
			if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["lock"] != model.SocketLockLinkUnknown || s["note"] != said || s["system_id"] != nil {
				t.Errorf("%s: a socket linked to chain0: %v", what, s)
			}
		}
	}
	// the bound counts the agents that name where they run: eight of them before a machine of its own are
	// followed (the ninth agent is that machine), nine are not
	for n, followed := range map[int]bool{8: true, 9: false} {
		agents := chainOf(n, "lone-pc")
		agents["lone-pc"] = ""
		pe := wayRig(t, agents, "chain0", "desk-pc", "chain0")
		snap := pe.s.Poller.Snapshot()
		if unknown := pe.s.Poller.PanelUnknown(snap); (unknown == nil) != followed || pe.s.Poller.PanelRows(snap)["lone-pc"] != followed {
			t.Errorf("%d agents before a machine of its own: the panel cannot follow %v; it marks that machine: %v", n, unknown, pe.s.Poller.PanelRows(snap)["lone-pc"])
		}
	}
	// a socket that is linked to an agent whose links lead back
	loop := wayRig(t, map[string]string{"loop-a": ", links_to: loop-b", "loop-b": ", links_to: loop-a"}, "loop-a", "desk-pc", "panel-box")
	if s := loop.socketOf(loop.admin, "other/rack-bar", "node"); s["lock"] != model.SocketLockLinkUnknown || s["note"] != "Locked. This socket is linked to loop-a, and links_to leads from loop-a back to loop-a: "+
		"the panel does not follow that to a machine. Write the node or the guest the agent runs on in links_to, or link the socket to the node." {
		t.Errorf("a socket linked to an agent whose links lead back: %v", s)
	}
	// the lock of a socket goes the same way: linked to an agent that names the node's own agent, it is the
	// node's socket, with the node's locks (the node carries the VM Home Assistant runs in)
	pe := wayRig(t, chainOf(2, "lab-node-pve"), "chain0", "lab-qemu-100", "panel-box")
	if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["lock"] != model.SocketLockHomeAssistant || s["system_id"] != "lab-node-pve" {
		t.Errorf("a socket linked to an agent whose links_to names the node's own agent: %v", s)
	}
}

// Each state in which the panel cannot follow panel_hosts to a machine has a sentence that is true for it: in
// the lock of a socket, in the power menu, and in the refusal of a run. They said "no row has that id, or the
// node under it has none" for all of them. The run's sentence also says what the state holds back: the whole
// run, with every machine, also one that would only be woken or updated.
func TestTheUnknownStateSaysItsTrueReason(t *testing.T) {
	const missing = "the row is there (its Proxmox API answers) or panel_hosts is corrected"
	const links = "config.yaml is corrected (write the node or the guest the agent runs on in links_to)"
	down, elsewhere := "", "ELSEWHERE"
	for _, c := range []struct {
		what, panel  string
		agents       map[string]string
		nas          *string
		reason, ends string
	}{
		{"the Proxmox API has not answered since the panel started", "nas-qemu-555", nil, &down, "no row has the id nas-qemu-555", missing},
		{"an agent in that VM", "inner", map[string]string{"inner": ", links_to: nas-qemu-555"}, &down, "inner leads to nas-qemu-555, and no row has that id", missing},
		{"the VM is on a node the API does not list", "nas-qemu-555", nil, &elsewhere, "nas-qemu-555 is a guest, and the node it runs on has no row", missing},
		{"an agent in such a VM", "inner", map[string]string{"inner": ", links_to: nas-qemu-555"}, &elsewhere, "inner leads to nas-qemu-555, a guest, and the node it runs on has no row", missing},
		{"an id nothing has", "nowhere", nil, nil, "no row has the id nowhere", missing},
		{"links_to that leads back", "loop-a", map[string]string{"loop-a": ", links_to: loop-b", "loop-b": ", links_to: loop-a"}, nil, "links_to leads from loop-a back to loop-a", links},
		{"links_to that leads back further on", "start", map[string]string{"start": ", links_to: loop-a", "loop-a": ", links_to: loop-b", "loop-b": ", links_to: loop-a"}, nil,
			"links_to leads from start back to loop-a", links},
		{"an agent that runs on an id of two things", "rides", map[string]string{"rides": ", links_to: lab-qemu-100", "lab-qemu-100": ""}, nil,
			"rides leads to lab-qemu-100, which is the id of an agent and of a node or a guest the Proxmox API lists, and the panel cannot tell which is meant",
			"config.yaml is corrected (give the agent lab-qemu-100 another id)"},
		{"two ids, a row that is missing and links that lead back", "nowhere, loop-a", map[string]string{"loop-a": ", links_to: loop-b", "loop-b": ", links_to: loop-a"}, nil,
			"no row has the id nowhere; links_to leads from loop-a back to loop-a", links},
	} {
		pe := wayRigWith(t, c.agents, "desk-pc", "panel-box", c.panel, c.nas)
		warning, whyNot := panelUnknownSaidFor(c.panel, c.reason, c.ends)
		lock := "Locked. The panel cannot follow panel_hosts (" + c.panel + ") to a machine right now: " + c.reason +
			". So it cannot tell which machine it runs on, and switches no socket off. This ends when " + c.ends + "."
		if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["state"] != "on" || s["lock"] != model.SocketLockPanelUnknown || s["note"] != lock {
			t.Errorf("%s: a socket that is on:\n got  %v\n want %s", c.what, s["note"], lock)
		}
		if r := pe.flip("other/rack-bar", "node", "off"); r.code != 409 {
			t.Errorf("%s: off for a socket: %d %s", c.what, r.code, r.raw)
		} else if msg, _ := plugDetailOf(r); msg != lock+" Nothing was switched." {
			t.Errorf("%s: off for a socket is refused with: %s", c.what, msg)
		}
		if r := menuOf(pe, "desk-pc"); r.body["panel_unknown"] != true || r.body["warning"] != warning {
			t.Errorf("%s: the power menu of a PC:\n got  %v\n want %s", c.what, r.body["warning"], warning)
		}
		if line := panelLookOf(t, pe).run["desk-pc"]; line["can_join"] != false || line["can_wake"] == true || line["why_not"] != whyNot {
			t.Errorf("%s: the line of a PC in Update unpowered hosts:\n got  %v\n want %s", c.what, line["why_not"], whyNot)
		}
		// the whole run is refused, with the sentence: a run that would only update a machine that is on
		start := pe.do("POST", "/api/admin/wake-update", map[string]any{"hosts": []string{}, "update_only": []string{"desk-pc"}}, pe.csrf...)
		if msg, _ := start.body["detail"].(string); start.code != 400 || msg != "DESK-PC cannot take part: "+whyNot {
			t.Errorf("%s: a run that would only update a machine that is on: %d %s", c.what, start.code, clipText(string(start.raw), 700))
		}
	}
}
