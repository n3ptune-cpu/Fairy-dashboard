package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// A panel that cannot follow panel_hosts to a machine does not know which machine it runs on, and fails closed
// until it does.

// nasRig is the plug rig with a second Proxmox host, NAS, whose own agent is nas-box (node: NAS, decided by the
// configuration) and on which the panel runs in the VM 555: panel_hosts: [nas-qemu-555]. The socket
// other/rack-bar/node is linked to link. answers says whether the NAS's Proxmox API answers; until it does
// the panel has no row for the node and none for the VM.
func nasRig(t *testing.T, link, panelHosts string, answers *bool) *plugEnv {
	t.Helper()
	t.Setenv("TOK_G", testutil.ReadToken+"g")
	for _, want := range []string{"\nagents:\n", "socket_links: {rack-bar/node: lab-node-pve}", "panel_hosts: [panel-box]"} {
		if strings.Count(plugConfig, want) != 1 {
			t.Fatal("the rig's config is not the one this fixture changes")
		}
	}
	yaml := strings.Replace(plugConfig, "\nagents:\n", "\n  - {id: nas, id_prefix: nas-, cluster_name: NAS, standalone: true, urls: [\"https://nas:8006\"],\n"+
		"     token_id: \"fairy@pve!panel\", token_secret_env: PVE_SECRET, verify_tls: false}\nagents:\n"+
		"  - {id: nas-box, name: NAS-BOX, url: \"http://nas-box\", token_env: TOK_G, control_token_env: CTL_A, self_update: true, node: NAS}\n", 1)
	yaml = strings.Replace(yaml, "socket_links: {rack-bar/node: lab-node-pve}", "socket_links: {rack-bar/node: "+link+"}", 1)
	yaml = strings.Replace(yaml, "panel_hosts: [panel-box]", "panel_hosts: ["+panelHosts+"]", 1)
	// the node PVE can be rebooted through its Proxmox API (a power token and node_power)
	const tls = "token_id: \"fairy@pve!panel\", token_secret_env: PVE_SECRET, verify_tls: false}\nagents:"
	if strings.Count(yaml, tls) != 1 {
		t.Fatal("the fixture: no proxmox: entry to give a power token")
	}
	yaml = strings.Replace(yaml, "cluster_name: PVE, standalone: true, urls: [\"https://pve:8006\"],\n     token_id: \"fairy@pve!panel\", token_secret_env: PVE_SECRET, verify_tls: false}",
		"cluster_name: PVE, standalone: true, urls: [\"https://pve:8006\"],\n     token_id: \"fairy@pve!panel\", token_secret_env: PVE_SECRET, verify_tls: false,\n"+
			"     power_token_id: \"fairy@pve!power\", power_token_secret_env: PVE_SECRET, node_power: true}", 1)
	if !strings.Contains(yaml, "node_power: true") {
		t.Fatal("the fixture: the entry of PVE has no power token")
	}
	pe := newPlugEnvWith(t, yaml)
	pe.b.Mu.Lock()
	pe.b.PVEOf = func(cluster string) ([]jsonx.M, error) {
		if cluster == "lab" {
			return testutil.HostResources("PVE", "running"), nil
		}
		if *answers {
			return append(testutil.HostResources("NAS", "running"), jsonx.M{"id": "qemu/555", "type": "qemu", "vmid": 555.0, "name": "panel-vm", "node": "NAS", "status": "running",
				"cpu": 0.0, "maxcpu": 4.0, "mem": 0.0, "maxmem": float64(8 << 30), "disk": 0.0, "maxdisk": float64(64 << 30), "template": 0.0}), nil
		}
		return nil, devices.Errf("connection failed")
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
	return pe
}

func menuOf(pe *plugEnv, id string) resp {
	return pe.do("GET", "/api/systems/"+id+"/power", nil, "Cookie", pe.admin)
}

func runLine(t *testing.T, pe *plugEnv, id string) map[string]any {
	t.Helper()
	page := pe.do("GET", "/api/admin/wake-update", nil, "Cookie", pe.admin)
	hosts, _ := page.body["hosts"].([]any)
	for _, h := range hosts {
		if m := h.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("no line of %s on the page of Update unpowered hosts: %d %s", id, page.code, clipText(string(page.raw), 300))
	return nil
}

// The panel runs in a VM and panel_hosts names the VM. After a start of the panel, and for as long as that
// Proxmox API does not answer, the VM has no row, and the panel cannot go from it to its node. It took the id
// as it is written and nothing more: the node's socket had no lock, "off" for it only asked about the machine
// that still runs, the reboot of the node's agent was a click, and the node could join a run.
//
// Now the panel does not know its machine in that state, and says so: every socket that is on is locked for
// "off" (panel_unknown, with a sentence that says why and when it ends), a reboot of a host asks for the typed
// name, and no host takes part in "Update unpowered hosts". "On" for a socket, a guest that is not named, and
// what is no cut (cancel, wake) are as they are without it. When the row is there, the rule is the old one:
// the node and the VM are the panel's, and everything else is free again.
func TestPanelThatCannotFollowPanelHostsFailsClosed(t *testing.T) {
	warning, whyNot := panelUnknownSaid("nas-qemu-555")
	const said = "Locked. The panel cannot follow panel_hosts (nas-qemu-555) to a machine right now: no row has that id, or the node under it has none. " +
		"So it cannot tell which machine it runs on, and switches no socket off. This ends when the row is there (its Proxmox API answers) or panel_hosts is corrected."
	for _, link := range []string{"nas-box", "nas-node-nas"} {
		answers := false
		pe := nasRig(t, link, "nas-qemu-555", &answers)
		if x := pe.do("GET", "/api/systems/nas-qemu-555", nil, "Cookie", pe.admin); x.code != 404 {
			t.Fatalf("the fixture: the panel's VM has a row before its Proxmox API answered: %d", x.code)
		}
		// the socket of the panel's node, and every other socket that is on and has no lock of its own
		for _, sock := range [][2]string{{"other/rack-bar", "node"}, {"reader/desk-bar", "ch1"}, {"reader/desk-bar", "ch3"}, {"reader/pc-plug", "main"}} {
			s := pe.socketOf(pe.admin, sock[0], sock[1])
			if s["state"] != "on" || s["switchable"] != false || s["lock"] != model.SocketLockPanelUnknown || s["why"] != model.SocketLockPanelUnknown || s["note"] != said {
				t.Errorf("link %s, before the API answered: the socket %s/%s: %v", link, sock[0], sock[1], s)
			}
			r := pe.flip(sock[0], sock[1], "off")
			if msg, _ := plugDetailOf(r); r.code != 409 || msg != said+" Nothing was switched." {
				t.Errorf("link %s, before the API answered: off for %s/%s: %d %s", link, sock[0], sock[1], r.code, r.raw)
			}
		}
		// a socket with a lock of its own keeps that one (the nearer reason), in both directions
		if s := pe.socketOf(pe.admin, "reader/desk-bar", "ch4"); s["lock"] != model.SocketLockAgent {
			t.Errorf("link %s: the socket of the reader's node: %v", link, s)
		}
		if n := len(pe.other.Asked()) + len(pe.reader.Asked()); n != 0 {
			t.Errorf("link %s: a machine was asked %d things for a switch that was refused", link, n)
		}
		// power ON is not held back: a socket that is off has its switch, and "on" goes to the passkey as always
		for _, sock := range [][2]string{{"other/rack-bar", "fan"}, {"reader/desk-bar", "ch2"}} {
			s := pe.socketOf(pe.admin, sock[0], sock[1])
			if s["state"] != "off" || s["switchable"] != true || s["lock"] != nil || s["why"] != nil {
				t.Errorf("link %s, before the API answered: the socket %s/%s, which is off: %v", link, sock[0], sock[1], s)
			}
			if r := pe.flip(sock[0], sock[1], "on"); r.code != 401 || !stepUpRequired(r) {
				t.Errorf("link %s, before the API answered: on for %s/%s: %d %s; want the question for the passkey", link, sock[0], sock[1], r.code, clipText(string(r.raw), 300))
			}
		}
		// the power menu: every host may be the panel's
		for id, kind := range map[string]string{"nas-box": "agent", "desk-pc": "agent", "lab-node-pve": "node", "panel-box": "agent"} {
			r := menuOf(pe, id)
			if r.body["kills_panel"] != false || r.body["panel_unknown"] != true || r.body["warning"] != warning || powerAsksOf(r) != powerAsks(kind, true) {
				t.Errorf("link %s, before the API answered: the power menu of %s: kills_panel %v, panel_unknown %v, warning %v, asks %q", link, id, r.body["kills_panel"], r.body["panel_unknown"], r.body["warning"], powerAsksOf(r))
			}
		}
		// ... a guest is not: it is no host, and the panel's own guest is the one without a row
		if r := menuOf(pe, "lab-qemu-100"); r.body["kills_panel"] != false || r.body["panel_unknown"] != false || r.body["warning"] != nil || powerAsksOf(r) != powerAsks("qemu", false) {
			t.Errorf("link %s, before the API answered: the power menu of a guest: %s", link, r.raw)
		}
		// what the menu says is what a request is held to: a reboot of a host without its name is refused
		admin := freshAdmin(pe)
		for _, typed := range []any{nil, "", "NAS", "DESK-PC"} {
			if r := pe.do("POST", "/api/systems/lab-node-pve/power", map[string]any{"action": "reboot", "delay_s": 60, "confirm": typed}, admin...); r.code != 400 || r.body["detail"] != "type the system name (PVE) to confirm reboot" {
				t.Errorf("link %s, before the API answered: a reboot of PVE with %v typed: %d %s", link, typed, r.code, r.raw)
			}
		}
		if calls := pveCalls(pe); len(calls) != 0 {
			t.Errorf("link %s, before the API answered: Proxmox was asked before a name was typed: %v", link, calls)
		}
		// ... a guest's reboot is a click, as it was, and what is no cut asks nothing: a cancel is not asked for a name
		if r := pe.do("POST", "/api/systems/lab-qemu-100/power", map[string]any{"action": "reboot"}, admin...); r.code != 200 || r.body["via"] != "proxmox-api" {
			t.Errorf("link %s, before the API answered: a reboot of a guest with a click: %d %s", link, r.code, r.raw)
		}
		if r := pe.do("POST", "/api/systems/desk-pc/power", map[string]any{"action": "cancel"}, admin...); strings.Contains(string(r.raw), "type the system name") {
			t.Errorf("link %s, before the API answered: a cancel asks for the name: %d %s", link, r.code, r.raw)
		}
		// "Update unpowered hosts": no host takes part
		for _, id := range []string{"nas-box", "desk-pc"} {
			if line := runLine(t, pe, id); line["can_join"] != false || line["can_wake"] != false || line["why_not"] != whyNot {
				t.Errorf("link %s, before the API answered: %s on the page of Update unpowered hosts: %v", link, id, line)
			}
		}
		if r := pe.do("PUT", "/api/systems/nas-box/wake-update", map[string]any{"enabled": true}, admin...); r.code != 409 || !strings.Contains(fmt.Sprint(r.body["detail"]), "the panel may run on it") {
			t.Errorf("link %s, before the API answered: NAS-BOX switched on for runs: %d %s", link, r.code, r.raw)
		}
		if r := pe.do("POST", "/api/admin/wake-update", map[string]any{"update_only": []string{"nas-box"}}, admin...); r.code != 400 || !strings.Contains(fmt.Sprint(r.body["detail"]), "the panel may run on it") {
			t.Errorf("link %s, before the API answered: a run with NAS-BOX: %d %s", link, r.code, r.raw)
		}
		// somebody who is no admin is told nothing about where the panel runs
		viewer := pe.viewerOf("Home")
		if r := pe.do("GET", "/api/systems/desk-pc/power", nil, "Cookie", viewer); r.code != 200 || r.body["panel_unknown"] != false || r.body["warning"] != nil {
			t.Errorf("link %s: the power menu of a viewer: %d %s", link, r.code, r.raw)
		}

		// the NAS's Proxmox API answers: the rows are there, and the rule is the one of a panel that knows its machine
		answers = true
		pe.poll()
		pe.poll()
		if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["lock"] != model.SocketLockPanel || s["note"] != plugSaidPanel {
			t.Errorf("link %s, with the rows there: the socket of the panel's node: %v", link, s)
		}
		for _, sock := range [][2]string{{"reader/desk-bar", "ch1"}, {"reader/desk-bar", "ch3"}, {"reader/pc-plug", "main"}} {
			if s := pe.socketOf(pe.admin, sock[0], sock[1]); s["switchable"] != true || s["lock"] != nil {
				t.Errorf("link %s, with the rows there: the socket %s/%s: %v", link, sock[0], sock[1], s)
			}
		}
		for id, panel := range map[string]bool{"nas-node-nas": true, "nas-qemu-555": true, "desk-pc": false, "lab-node-pve": false, "panel-box": false} {
			r := menuOf(pe, id)
			var want any
			if panel {
				want = panelRunsHere
			}
			if r.code != 200 || r.body["kills_panel"] != panel || r.body["panel_unknown"] != false || r.body["warning"] != want {
				t.Errorf("link %s, with the rows there: the power menu of %s: %d kills_panel %v, panel_unknown %v, warning %v", link, id, r.code, r.body["kills_panel"], r.body["panel_unknown"], r.body["warning"])
			}
		}
		if asks := powerAsksOf(menuOf(pe, "desk-pc")); asks != powerAsks("agent", false) {
			t.Errorf("link %s, with the rows there: the power menu of DESK-PC asks %q", link, asks)
		}
		if r := pe.do("POST", "/api/systems/lab-node-pve/power", map[string]any{"action": "reboot", "delay_s": 60}, admin...); r.code != 200 || r.body["via"] != "proxmox-api" {
			t.Errorf("link %s, with the rows there: a reboot of PVE, where the panel does not run, with a click: %d %s", link, r.code, r.raw)
		}
		if line := runLine(t, pe, "nas-node-nas"); line["can_join"] != false || line["why_not"] != panelRunsOnIt {
			t.Errorf("link %s, with the rows there: the panel's node on the page of Update unpowered hosts: %v", link, line)
		}
		if line := runLine(t, pe, "desk-pc"); line["why_not"] == whyNot {
			t.Errorf("link %s, with the rows there: DESK-PC on the page of Update unpowered hosts: %v", link, line)
		}
	}
}

// An empty panel_hosts is not that state: a panel that was told nothing about its machine marks and locks
// nothing as its own, as before. And a panel_hosts that is followed to a machine holds nothing back elsewhere.
func TestPanelHostsEmptyOrFollowedHoldsNothingBack(t *testing.T) {
	for _, panelHosts := range []string{"", "panel-box", "nas-box"} {
		answers := false
		pe := nasRig(t, "lab-node-pve", panelHosts, &answers)
		if ids := pe.s.Poller.PanelUnknown(pe.s.Poller.Snapshot()); ids != nil {
			t.Fatalf("panel_hosts [%s]: the panel cannot follow %v", panelHosts, ids)
		}
		if s := pe.socketOf(pe.admin, "reader/desk-bar", "ch1"); s["switchable"] != true || s["lock"] != nil {
			t.Errorf("panel_hosts [%s]: a socket that is on: %v", panelHosts, s)
		}
		if r := menuOf(pe, "desk-pc"); r.body["panel_unknown"] != false || r.body["warning"] != nil || powerAsksOf(r) != powerAsks("agent", false) {
			t.Errorf("panel_hosts [%s]: the power menu of DESK-PC: %s", panelHosts, r.raw)
		}
		if line := runLine(t, pe, "desk-pc"); strings.Contains(fmt.Sprint(line["why_not"]), "the panel may run on it") {
			t.Errorf("panel_hosts [%s]: DESK-PC on the page of Update unpowered hosts: %v", panelHosts, line)
		}
	}
}

// An agent whose id is written like a guest's id (lab-qemu-100, while the VM 100 is there too) hides the
// guest: the panel keeps one row for an id. With that id in panel_hosts the row was marked and nothing else:
// the node under the VM had no mark and its socket no lock. Which of the two is meant cannot be told, so the
// id names two things, and every way that meets it ends there: the panel does not know its machine (the lock
// of every socket for "off", the typed name for a reboot of a host), a socket that is linked to the id is
// locked like one that is linked to no row, and home_assistant_system with it switches no socket of that
// agent.
func TestAnAgentIDThatIsAlsoAGuestsID(t *testing.T) {
	rig := func(link, ha, panelHosts string) *plugEnv {
		t.Setenv("TOK_G", testutil.ReadToken+"g")
		for _, want := range []string{"\nagents:\n", "socket_links: {rack-bar/node: lab-node-pve}, home_assistant_system: lab-qemu-100}", "panel_hosts: [panel-box]"} {
			if strings.Count(plugConfig, want) != 1 {
				t.Fatal("the rig's config is not the one this fixture changes")
			}
		}
		yaml := strings.Replace(plugConfig, "\nagents:\n", "\nagents:\n  - {id: lab-qemu-100, name: SAMEID, url: \"http://sameid\", token_env: TOK_G}\n", 1)
		yaml = strings.Replace(yaml, "socket_links: {rack-bar/node: lab-node-pve}, home_assistant_system: lab-qemu-100}", "socket_links: {rack-bar/node: "+link+"}, home_assistant_system: "+ha+"}", 1)
		pe := newPlugEnvWith(t, strings.Replace(yaml, "panel_hosts: [panel-box]", "panel_hosts: ["+panelHosts+"]", 1))
		pe.b.Mu.Lock()
		pe.b.Metrics["lab-qemu-100"] = func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			m["hostname"] = "sameid"
			delete(m, "power_sources")
			return m, nil
		}
		pe.b.Mu.Unlock()
		pe.poll()
		pe.poll()
		if !pe.s.Poller.NamesTwo("lab-qemu-100") || pe.s.Poller.NamesTwo("lab-qemu-101") || pe.s.Poller.NamesTwo("desk-pc") || pe.s.Poller.NamesTwo("lab-node-pve") {
			t.Fatal("the fixture: which ids name two things")
		}
		return pe
	}
	// panel_hosts with that id: the row is the panel's as it is written, and the panel does not know its machine
	pe := rig("lab-node-pve", "desk-pc", "lab-qemu-100")
	if r := menuOf(pe, "lab-qemu-100"); r.body["kills_panel"] != true {
		t.Errorf("panel_hosts names the id: the row with that id: %s", r.raw)
	}
	warning, _ := panelUnknownSaid("lab-qemu-100")
	if r := menuOf(pe, "lab-node-pve"); r.body["kills_panel"] != false || r.body["panel_unknown"] != true || r.body["warning"] != warning || powerAsksOf(r) != powerAsks("node", true) {
		t.Errorf("panel_hosts names the id: the power menu of the node under the VM: %s", r.raw)
	}
	s := pe.socketOf(pe.admin, "other/rack-bar", "node")
	if s["switchable"] != false || s["lock"] != model.SocketLockPanelUnknown {
		t.Errorf("panel_hosts names the id: the socket of the node under the VM: %v", s)
	}
	if msg, _ := plugDetailOf(pe.flip("other/rack-bar", "node", "off")); !strings.HasPrefix(msg, "Locked. The panel cannot follow panel_hosts (lab-qemu-100) to a machine right now") {
		t.Errorf("panel_hosts names the id: off for the socket of the node: %s", msg)
	}
	// a socket that is linked to the id: locked like a link to no row, on and off, with a sentence of its own
	pe = rig("lab-qemu-100", "desk-pc", "panel-box")
	const said = "Locked. This socket is linked to lab-qemu-100, and that id names two things: an agent, and a node or a guest of a Proxmox entry. The panel cannot tell which is meant. Give the agent another id."
	if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["switchable"] != false || s["lock"] != model.SocketLockLinkUnknown || s["note"] != said || s["system_id"] != nil {
		t.Errorf("a socket linked to the id: %v", s)
	}
	for _, to := range []string{"off", "on"} {
		r := pe.flip("other/rack-bar", "node", to)
		if msg, _ := plugDetailOf(r); r.code != 409 || msg != said+" Nothing was switched." {
			t.Errorf("a socket linked to the id, %s: %d %s", to, r.code, r.raw)
		}
	}
	// (and with nothing that names the id the panel knows its machine: nothing else is held back)
	if r := menuOf(pe, "desk-pc"); r.body["panel_unknown"] != false || powerAsksOf(r) != powerAsks("agent", false) {
		t.Errorf("the id is named nowhere the panel follows: the power menu of DESK-PC: %s", r.raw)
	}
	// home_assistant_system with the id: which machine Home Assistant runs on is not known, and no socket of
	// that agent is switched
	pe = rig("lab-node-pve", "lab-qemu-100", "panel-box")
	for _, sock := range []string{"node", "fan"} {
		if s := pe.socketOf(pe.admin, "other/rack-bar", sock); s["switchable"] != false || s["lock"] != model.SocketLockHAUnknown {
			t.Errorf("home_assistant_system names the id: the socket %s: %v", sock, s)
		}
	}
}
