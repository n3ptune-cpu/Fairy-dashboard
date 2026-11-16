package web

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Tests for the fourth round of stage 1a: the panel's own machine in the power menu and in "Update unpowered
// hosts" (third review, check of the third round, finding C).
//
// Since the third round the lock of a socket follows panel_hosts down to the machine. The power menu and
// "Update unpowered hosts" did not: they took the ids as they are written, and the node of a guest that is
// named. With an agent in a guest in panel_hosts, or the id of the node's own agent, the node's menu had no
// "The panel itself runs here", its reboot was a click, and the node could be switched on for a run. Now the
// three ask one rule (poller.PanelRows): the row that is named, the rows on the way down, and the machine at
// the end.

const (
	panelRunsHere = "The panel itself runs here: shutting this down takes Fairy offline until it is powered on again by other means."
	panelRunsOnIt = "the panel itself runs on it"
)

// panelWay is one way panel_hosts can name the machine the panel runs on, and the rows the panel then runs
// on: the row that is named, the rows on the way down from it, and the machine at the end. No other row: a
// guest beside the panel's guest goes down without the panel.
//
// What the rule does not do, as it never did: it goes down from what is named, and not up. An agent that runs
// in the panel's guest, or beside the node's own agent, and is not itself in panel_hosts keeps a row that is
// not marked (vm2agent while panel_hosts names lab-qemu-202; sideagent while it names the node), although a
// shutdown through that agent would take the panel's guest or node down. Such a row is among "every other
// row" below, so these tests hold it as it is.
type panelWay struct {
	name, panel string
	rows        []string
}

// panelWaysOnPVE2 are the ways to say in plugNodeRig that the panel runs on the node PVE2.
var panelWaysOnPVE2 = []panelWay{
	{"the node's id", "lab-node-pve2", []string{"lab-node-pve2"}},
	{"a VM of the node", "lab-qemu-202", []string{"lab-qemu-202", "lab-node-pve2"}},
	{"a container of the node", "lab-lxc-203", []string{"lab-lxc-203", "lab-node-pve2"}},
	{"an agent in a VM of the node", "vm2agent", []string{"vm2agent", "lab-qemu-202", "lab-node-pve2"}},
	{"an agent in a container of the node", "ctagent", []string{"ctagent", "lab-lxc-203", "lab-node-pve2"}},
	{"the node's own agent", "pve2agent", []string{"lab-node-pve2"}},
	{"an agent two steps away", "viaagent", []string{"viaagent", "vmagent", "lab-qemu-201", "lab-node-pve2"}},
	{"a second agent of the node", "sideagent", []string{"sideagent", "lab-node-pve2"}},
}

// powerAsks is what the power menu asks for each action of a row of this kind ("typed": the row's name;
// "click"; "none"), in the menu's order, on a row the panel runs on and on any other. The rule is
// powerctl's (guestCaps, hostCaps) and no test had it whole:
//
//	shut down, stop, sleep   the name typed, always
//	reboot, start (a guest)  the name typed on a row the panel runs on, a click on any other
//	cancel, wake             never asked
//
// (Power on through a BMC asks for the name always; no machine of the rig has one.)
func powerAsks(kind string, panel bool) string {
	ifPanel := "click"
	if panel {
		ifPanel = "typed"
	}
	switch kind {
	case "qemu", "lxc":
		return "start:" + ifPanel + " shutdown:typed reboot:" + ifPanel + " stop:typed"
	case "node": // a hypervisor is not put to sleep from the panel
		return "shutdown:typed reboot:" + ifPanel + " cancel:none wake:none"
	}
	return "shutdown:typed reboot:" + ifPanel + " sleep:typed cancel:none wake:none"
}

// powerAsksOf is what a power menu that was sent asks for each of its actions, as powerAsks writes it.
func powerAsksOf(menu resp) string {
	var asks []string
	list, _ := menu.body["actions"].([]any)
	for _, a := range list {
		m := a.(map[string]any)
		asks = append(asks, fmt.Sprint(m["action"], ":", m["confirm"]))
	}
	return strings.Join(asks, " ")
}

// panelLook is what an admin is told about every row: its power menu as it is sent, and its line on the page
// of "Update unpowered hosts" (nil: it is no machine with an agent of its own).
type panelLook struct {
	kinds map[string]string
	menu  map[string]string
	run   map[string]map[string]any
}

func panelLookOf(t *testing.T, pe *plugEnv) panelLook {
	t.Helper()
	k := panelLook{kinds: map[string]string{}, menu: map[string]string{}, run: map[string]map[string]any{}}
	for _, x := range pe.s.Poller.Snapshot() {
		r := pe.do("GET", "/api/systems/"+x.ID+"/power", nil, "Cookie", pe.admin)
		if r.code != 200 {
			t.Fatalf("the power menu of %s: %d %s", x.ID, r.code, r.raw)
		}
		k.kinds[x.ID], k.menu[x.ID] = x.Kind, string(r.raw)
	}
	page := pe.do("GET", "/api/admin/wake-update", nil, "Cookie", pe.admin)
	hosts, _ := page.body["hosts"].([]any)
	if page.code != 200 || len(hosts) == 0 {
		t.Fatalf("the page of Update unpowered hosts: %d %s", page.code, page.raw)
	}
	for _, h := range hosts {
		m := h.(map[string]any)
		k.run[m["id"].(string)] = m
	}
	return k
}

// panelRowsAre asks the power menu and the page of "Update unpowered hosts" about every row there is. On the
// rows the panel runs on (rows): kills_panel, the warning, and each action asks what it asks on a panel host;
// the row cannot take part in a run, with the reason of a panel host. Every other row: its menu and its line
// are, byte for byte, what they are in the same rig with a panel that runs on none of its rows (plain).
func panelRowsAre(t *testing.T, what string, pe *plugEnv, rows []string, plain panelLook) {
	t.Helper()
	got := panelLookOf(t, pe)
	for _, id := range rows {
		if got.menu[id] == "" {
			t.Fatalf("%s: the fixture: no row %s", what, id)
		}
	}
	if len(got.menu) != len(plain.menu) || len(got.run) != len(plain.run) {
		t.Fatalf("%s: the fixture: %d rows and %d lines, without the panel %d and %d", what, len(got.menu), len(got.run), len(plain.menu), len(plain.run))
	}
	for id, kind := range got.kinds {
		on := slices.Contains(rows, id)
		r := pe.do("GET", "/api/systems/"+id+"/power", nil, "Cookie", pe.admin)
		var warning any
		if on {
			warning = panelRunsHere
		}
		if asks := powerAsksOf(r); r.body["kills_panel"] != on || r.body["warning"] != warning || asks != powerAsks(kind, on) {
			t.Errorf("%s: the power menu of %s (%s): kills_panel %v, warning %v, asks %q;\nwant %v, %v, %q",
				what, id, kind, r.body["kills_panel"], r.body["warning"], asks, on, warning, powerAsks(kind, on))
		}
		if !on && got.menu[id] != plain.menu[id] {
			t.Errorf("%s: the power menu of %s, a row the panel does not run on, is not what it is without the panel:\n%s\n%s", what, id, got.menu[id], plain.menu[id])
		}
		line := got.run[id]
		switch {
		case (line == nil) != (plain.run[id] == nil):
			t.Errorf("%s: %s on the page of Update unpowered hosts: %v, without the panel %v", what, id, line, plain.run[id])
		case line == nil:
		case on:
			if line["can_join"] != false || line["why_not"] != panelRunsOnIt || line["why"] != panelRunsOnIt || line["can_wake"] != false || line["choice"] != "" {
				t.Errorf("%s: %s, a row the panel runs on, on the page of Update unpowered hosts: %v", what, id, line)
			}
		case !reflect.DeepEqual(line, plain.run[id]):
			t.Errorf("%s: %s, a row the panel does not run on, on the page of Update unpowered hosts:\n%v\nwithout the panel:\n%v", what, id, line, plain.run[id])
		}
	}
}

// freshAdmin is the headers of an admin who signed in just now: a power action and a switch for "Update
// unpowered hosts" ask nothing more of them.
func freshAdmin(pe *plugEnv) []string {
	return []string{"Cookie", pe.cookie(controller(), 0), "X-Requested-With", "fairy", "Content-Type", "application/json"}
}

func pveCalls(pe *plugEnv) []string {
	pe.b.Mu.Lock()
	defer pe.b.Mu.Unlock()
	return slices.Clone(pe.b.PVECalls)
}

// However panel_hosts names the machine the panel runs on, the power menu says so on the node and on the
// guest the panel is in, and asks there what it asks on a panel host; and no other row is touched: the other
// guests of the node, the other node, every other machine.
//
// Before the change this failed for an agent in a guest (a VM, a container, two steps away), for the node's
// own agent and for a second agent of the node: the node's menu had kills_panel false and no warning, its
// reboot was a click, and the guest the agent runs in was not marked either.
func TestPowerMenuSaysThePanelRunsHereHoweverPanelHostsNamesIt(t *testing.T) {
	plain := map[bool]panelLook{}
	for _, lone := range []bool{false, true} {
		plain[lone] = panelLookOf(t, plugNodeRig{link: "lab-node-pve2", panelHosts: "no-such-box", lone: lone, power: true}.env(t))
		for id, menu := range plain[lone].menu { // the fixture: without the panel no row is marked
			if !strings.Contains(menu, `"kills_panel":false`) || strings.Contains(menu, "The panel itself") {
				t.Fatalf("the fixture: the menu of %s with a panel that runs on no row: %s", id, menu)
			}
		}
	}
	for _, lone := range []bool{false, true} {
		for _, w := range panelWaysOnPVE2 {
			if lone && (w.panel == "sideagent") {
				continue // lone is the rig without the second agent
			}
			what := fmt.Sprintf("panel_hosts names %s (%s), lone %v", w.panel, w.name, lone)
			pe := plugNodeRig{link: "lab-node-pve2", panelHosts: w.panel, lone: lone, power: true}.env(t)
			panelRowsAre(t, what, pe, w.rows, plain[lone])
			// said out for the rows the finding is about: the node, and every guest on it
			var guest string // the guest of PVE2 the panel is in ("": on the node itself)
			for _, g := range []string{"lab-qemu-201", "lab-qemu-202", "lab-lxc-203"} {
				if slices.Contains(w.rows, g) {
					guest = g
				}
			}
			for _, id := range []string{"lab-node-pve2", "lab-qemu-201", "lab-qemu-202", "lab-lxc-203", "lab-node-pve", "lab-qemu-100"} {
				r := pe.do("GET", "/api/systems/"+id+"/power", nil, "Cookie", pe.admin)
				if want := id == "lab-node-pve2" || id == guest; r.body["kills_panel"] != want {
					t.Errorf("%s: kills_panel of %s is %v, want %v", what, id, r.body["kills_panel"], want)
				}
			}
			// the socket of the node is the panel's in every one of them: the lock and the menu ask one rule
			if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["lock"] != model.SocketLockPanel {
				t.Errorf("%s: the socket of PVE2: %v", what, s)
			}

			// What the menu says is what the request is held to. Reboot of the node: no name, another name, and
			// nothing was sent; with its name it goes out.
			admin := freshAdmin(pe)
			reboot := func(id string, confirm any) resp {
				return pe.do("POST", "/api/systems/"+id+"/power", map[string]any{"action": "reboot", "delay_s": 60, "confirm": confirm}, admin...)
			}
			for _, typed := range []any{nil, "", "PVE", "panel-vm", "VM2AGENT", "PVE2AGENT"} {
				if r := reboot("lab-node-pve2", typed); r.code != 400 || r.body["detail"] != "type the system name (PVE2) to confirm reboot" {
					t.Errorf("%s: reboot of PVE2 with %v typed: %d %s", what, typed, r.code, r.raw)
				}
			}
			if guest != "" {
				if r := reboot(guest, nil); r.code != 400 || !strings.Contains(string(r.raw), "to confirm reboot") {
					t.Errorf("%s: reboot of %s, the guest the panel is in, without its name: %d %s", what, guest, r.code, r.raw)
				}
			}
			if calls := pveCalls(pe); len(calls) != 0 {
				t.Errorf("%s: Proxmox was asked before a name was typed: %v", what, calls)
			}
			// the rows beside it are a click, as they were: another guest of the node, and the other node
			beside := "lab-qemu-201"
			if guest == beside {
				beside = "lab-qemu-202"
			}
			for _, id := range []string{beside, "lab-node-pve"} {
				if r := reboot(id, nil); r.code != 200 || r.body["via"] != "proxmox-api" {
					t.Errorf("%s: reboot of %s, where the panel does not run, with a click: %d %s", what, id, r.code, r.raw)
				}
			}
			before := len(pveCalls(pe))
			if r := reboot("lab-node-pve2", "PVE2"); r.code != 200 || r.body["via"] != "proxmox-api" {
				t.Errorf("%s: reboot of PVE2 with its name typed: %d %s", what, r.code, r.raw)
			}
			if calls := pveCalls(pe); len(calls) != before+1 || !strings.Contains(calls[len(calls)-1], "PVE2") || !strings.Contains(calls[len(calls)-1], "command=reboot") {
				t.Errorf("%s: reboot of PVE2 with its name typed: Proxmox calls %v", what, calls)
			}
		}
	}
}

// "Update unpowered hosts" leaves the node under the panel out however panel_hosts names it, exactly as it
// leaves out a panel host that is named by its own id: the same line on the page, the same refusal of the
// switch and of a start. (panelRowsAre asks the page for every row; this one asks the two requests, and
// compares the node's line with the line it has when panel_hosts has the node's id.)
//
// The node's agent can be updated from the panel here (self_update and a control token), so that nothing but
// the panel stands in the way: with a panel that runs elsewhere the node can take part.
//
// Before the change the node could be switched on for runs when panel_hosts named an agent in a guest of it,
// its own agent, or a second agent.
func TestUpdateUnpoweredHostsLeavesOutTheNodeUnderThePanelHoweverItIsNamed(t *testing.T) {
	rig := func(panelHosts string) *plugEnv {
		return plugNodeRig{link: "lab-node-pve2", panelHosts: panelHosts, power: true, nodeAgent: "self_update: true, control_token_env: CTL_A,"}.env(t)
	}
	if line := panelLookOf(t, rig("no-such-box")).run["lab-node-pve2"]; line["can_join"] != true || line["why_not"] != nil {
		t.Fatalf("the fixture: the line of PVE2 with a panel that runs elsewhere: %v", line)
	}
	var direct map[string]any
	for _, w := range panelWaysOnPVE2 {
		what := fmt.Sprintf("panel_hosts names %s (%s)", w.panel, w.name)
		pe := rig(w.panel)
		line := panelLookOf(t, pe).run["lab-node-pve2"]
		if direct == nil { // the first way is the node's own id
			if w.panel != "lab-node-pve2" || line["can_join"] != false || line["why_not"] != panelRunsOnIt {
				t.Fatalf("the fixture: the line of a panel host named by its id: %v", line)
			}
			direct = line
		}
		if !reflect.DeepEqual(line, direct) {
			t.Errorf("%s: the line of PVE2:\n%v\nwith the node's id in panel_hosts:\n%v", what, line, direct)
		}
		admin := freshAdmin(pe)
		const said = "PVE2 cannot take part: the panel itself runs on it"
		if r := pe.do("PUT", "/api/systems/lab-node-pve2/wake-update", map[string]any{"enabled": true}, admin...); r.code != 409 || r.body["detail"] != said {
			t.Errorf("%s: PVE2 switched on for runs: %d %s", what, r.code, r.raw)
		}
		for _, body := range []map[string]any{{"update_only": []string{"lab-node-pve2"}}, {"hosts": []string{"lab-node-pve2"}}} {
			if r := pe.do("POST", "/api/admin/wake-update", body, admin...); r.code != 400 || r.body["detail"] != said {
				t.Errorf("%s: a run with PVE2 (%v): %d %s", what, body, r.code, r.raw)
			}
		}
		if pe.s.Poller.Admin.WakeUpdateEnabled("lab-node-pve2") {
			t.Errorf("%s: PVE2 is switched on for runs", what)
		}
	}
	// and a node the panel does not run on is refused for its own reason, not for the panel's
	pe := rig("vm2agent")
	r := pe.do("PUT", "/api/systems/lab-node-pve/wake-update", map[string]any{"enabled": true}, freshAdmin(pe)...)
	if r.code != 409 || !strings.HasPrefix(fmt.Sprint(r.body["detail"]), "PVE cannot take part: its agent cannot be updated from the panel") {
		t.Errorf("the other node switched on for runs: %d %s", r.code, r.raw)
	}
}

// panel_hosts that cannot be followed to a machine. The power menu and "Update unpowered hosts" do what the
// lock of a socket does (they ask the same rule): what is named counts as it is written, with the rows the
// way went over before it ended, and no row beyond that is guessed. So the rows on the way say "The panel
// itself runs here", and no node does: the panel cannot tell which one.
//
// It is not turned into "the panel may run here" for every row. An id of panel_hosts needs no row (the
// panel's machine need not be one the panel shows; after a start the guests are not there until Proxmox has
// answered), and then every machine would be marked, each reboot would ask for the name and no machine could
// take part in a run, for as long as that id has no row.
func TestPanelHostsThatCannotBeFollowedToAMachine(t *testing.T) {
	plain := panelLookOf(t, plugNodeRig{link: "lab-node-pve2", panelHosts: "no-such-box", power: true}.env(t))
	for _, w := range []panelWay{
		{"an agent in a guest whose node has no row", "lostagent", []string{"lostagent", "lab-qemu-301"}},
		{"an agent two steps from such a guest", "via2agent", []string{"via2agent", "lostagent", "lab-qemu-301"}},
		{"a VM whose node has no row", "lab-qemu-301", []string{"lab-qemu-301"}},
		{"a container whose node has no row", "lab-lxc-302", []string{"lab-lxc-302"}},
		{"an id no row has", "old-box", nil},
		{"the id of a guest that is not there", "lab-qemu-999", nil},
	} {
		what := fmt.Sprintf("panel_hosts names %s (%s)", w.panel, w.name)
		pe := plugNodeRig{link: "lab-node-pve2", panelHosts: w.panel, power: true}.env(t)
		panelRowsAre(t, what, pe, w.rows, plain)
		// the socket of the node says the same: it is not the panel's
		if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["lock"] != nil || s["switchable"] != true {
			t.Errorf("%s: the socket of PVE2: %v", what, s)
		}
	}

	// links that lead back to themselves: two agents that each name the other
	loop := func(panelHosts string) *plugEnv {
		t.Setenv("TOK_L0", testutil.ReadToken+"x")
		t.Setenv("TOK_L1", testutil.ReadToken+"y")
		if strings.Count(plugConfig, "\nagents:\n") != 1 || strings.Count(plugConfig, "panel_hosts: [panel-box]") != 1 {
			t.Fatal("the rig's config is not the one this fixture changes")
		}
		yaml := strings.Replace(plugConfig, "\nagents:\n", "\nagents:\n"+
			"  - {id: loopa, name: LOOPA, url: \"http://loopa\", token_env: TOK_L0, links_to: loopb}\n"+
			"  - {id: loopb, name: LOOPB, url: \"http://loopb\", token_env: TOK_L1, links_to: loopa}\n", 1)
		pe := newPlugEnvWith(t, strings.Replace(yaml, "panel_hosts: [panel-box]", "panel_hosts: ["+panelHosts+"]", 1))
		pe.b.Mu.Lock()
		for _, id := range []string{"loopa", "loopb"} {
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
		for _, id := range []string{"loopa", "loopb"} {
			if x := pe.do("GET", "/api/systems/"+id, nil, "Cookie", pe.admin); x.code != 200 || x.body["kind"] != "agent" || x.body["status"] != "online" {
				t.Fatalf("the fixture: the row of %s: %d %v", id, x.code, x.body)
			}
		}
		return pe
	}
	plainLoop := panelLookOf(t, loop("no-such-box"))
	for _, w := range []panelWay{
		{"an agent whose links lead back to it", "loopa", []string{"loopa", "loopb"}},
		{"the other agent of the two", "loopb", []string{"loopa", "loopb"}},
	} {
		panelRowsAre(t, fmt.Sprintf("panel_hosts names %s (%s)", w.panel, w.name), loop(w.panel), w.rows, plainLoop)
	}
	ids := make([]string, 0, len(plainLoop.menu))
	for id := range plainLoop.menu {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if !slices.Contains(ids, "lab-node-pve") || !slices.Contains(ids, "desk-pc") {
		t.Fatalf("the fixture: the rows of the rig with the two agents: %v", ids)
	}
}

// A node's own agent is one machine with the node. While the node has no row (the panel was started and that
// node's Proxmox API has not answered since) the machine is shown as the agent's row, and that row is the
// panel's machine whether panel_hosts has the agent's id or the node's.
//
// Before the change the node's id in panel_hosts marked no row in that state: the id was compared with the
// id of the row as it is written.
func TestPowerMenuOfTheNodesAgentWhileTheNodeHasNoRow(t *testing.T) {
	for _, panel := range []string{"nas-box", "nas-node-nas", "no-such-box"} {
		t.Setenv("TOK_G", testutil.ReadToken+"g")
		if strings.Count(plugConfig, "\nagents:\n") != 1 || strings.Count(plugConfig, "panel_hosts: [panel-box]") != 1 {
			t.Fatal("the rig's config is not the one this fixture changes")
		}
		yaml := strings.Replace(plugConfig, "\nagents:\n", "\n  - {id: nas, id_prefix: nas-, cluster_name: NAS, standalone: true, urls: [\"https://nas:8006\"],\n"+
			"     token_id: \"fairy@pve!panel\", token_secret_env: PVE_SECRET, verify_tls: false}\nagents:\n"+
			"  - {id: nas-box, name: NAS-BOX, url: \"http://nas-box\", token_env: TOK_G, node: NAS}\n", 1)
		pe := newPlugEnvWith(t, strings.Replace(yaml, "panel_hosts: [panel-box]", "panel_hosts: ["+panel+"]", 1))
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
		if got := pe.s.Poller.SameMachine("nas-box"); fmt.Sprint(got) != "[nas-box nas-node-nas]" {
			t.Fatalf("the fixture: the ids of the machine are %v", got)
		}
		on := panel != "no-such-box"
		look := panelLookOf(t, pe)
		for id, menu := range look.menu {
			if want := on && id == "nas-box"; strings.Contains(menu, `"kills_panel":true`) != want || strings.Contains(menu, panelRunsHere) != want {
				t.Errorf("panel_hosts names %s: the power menu of %s: %s", panel, id, menu)
			}
		}
		if asks := powerAsksOf(pe.do("GET", "/api/systems/nas-box/power", nil, "Cookie", pe.admin)); asks != powerAsks("agent", on) {
			t.Errorf("panel_hosts names %s: the power menu of NAS-BOX asks %q, want %q", panel, asks, powerAsks("agent", on))
		}
		if line := look.run["nas-box"]; (line["why_not"] == panelRunsOnIt) != on || line["can_join"] != false {
			t.Errorf("panel_hosts names %s: NAS-BOX on the page of Update unpowered hosts: %v", panel, line)
		}
	}
}
