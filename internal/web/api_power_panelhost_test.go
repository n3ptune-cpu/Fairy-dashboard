package web

import (
	"encoding/json"
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
// three ask one rule (poller.PanelRows): the row that is named, the rows on the way down, the machine at the
// end, and the row of every agent that is the same machine as one of those.

const (
	panelRunsHere = "The panel itself runs here: shutting this down takes Fairy offline until it is powered on again by other means."
	panelRunsOnIt = "the panel itself runs on it"
)

// panelWay is one way panel_hosts can name the machine the panel runs on, and the rows the panel then runs
// on:
//
//   - the row that is named, the rows on the way down from it, and the machine at the end;
//   - every agent's row that is the same machine as one of those: an agent that runs in the guest the panel
//     is in (a reboot through it is a reboot of the panel's guest), an agent whose entry names such an agent,
//     and an agent that runs on the node itself beside the node's own agent.
//
// No other row. A guest beside the panel's guest goes down without the panel, and so does an agent that runs
// in such a guest.
type panelWay struct {
	name, panel string
	rows        []string
}

// panelWaysOnPVE2 are the ways to say in plugNodeRig that the panel runs on the node PVE2. In the rig with
// the second agent of the node, sideagent, that agent's row is one of the rows in every one of them: it runs
// on the node (rowsIn).
var panelWaysOnPVE2 = []panelWay{
	{"the node's id", "lab-node-pve2", []string{"lab-node-pve2"}},
	{"a VM of the node, with an agent in it", "lab-qemu-202", []string{"lab-qemu-202", "lab-node-pve2", "vm2agent"}},
	{"a container of the node, with an agent in it", "lab-lxc-203", []string{"lab-lxc-203", "lab-node-pve2", "ctagent"}},
	{"a VM with an agent in it, and an agent whose entry names that agent", "lab-qemu-201", []string{"lab-qemu-201", "lab-node-pve2", "vmagent", "viaagent"}},
	{"an agent in a VM of the node", "vm2agent", []string{"vm2agent", "lab-qemu-202", "lab-node-pve2"}},
	{"an agent in a container of the node", "ctagent", []string{"ctagent", "lab-lxc-203", "lab-node-pve2"}},
	{"the node's own agent", "pve2agent", []string{"lab-node-pve2"}},
	{"an agent two steps away", "viaagent", []string{"viaagent", "vmagent", "lab-qemu-201", "lab-node-pve2"}},
	{"a second agent of the node", "sideagent", []string{"sideagent", "lab-node-pve2"}},
}

// rowsIn are the rows of a way in the rig with the second agent of the node (lone false) or without it.
func (w panelWay) rowsIn(lone bool) []string {
	if lone || slices.Contains(w.rows, "sideagent") {
		return w.rows
	}
	return append(slices.Clone(w.rows), "sideagent")
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
		plain[lone] = panelLookOf(t, plugNodeRig{link: "lab-node-pve2", noPanel: true, lone: lone, power: true}.env(t)) // (a panel that was told nothing marks no row)
		for id, menu := range plain[lone].menu {                                                                        // the fixture: without the panel no row is marked
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
			panelRowsAre(t, what, pe, w.rowsIn(lone), plain[lone])
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
	if line := panelLookOf(t, rig("panel-box")).run["lab-node-pve2"]; line["can_join"] != true || line["why_not"] != nil {
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
	// An agent's row that is the same machine as a row the panel runs on is left out like that row: the agent in
	// the panel's VM, and the second agent of the node. An agent in another guest of the node is not the panel's.
	for _, c := range []struct{ panel, agent, name string }{
		{"lab-qemu-202", "vm2agent", "VM2AGENT"}, {"lab-qemu-202", "sideagent", "SIDEAGENT"}, {"lab-node-pve2", "sideagent", "SIDEAGENT"},
		{"lab-lxc-203", "ctagent", "CTAGENT"}, {"lab-qemu-201", "viaagent", "VIAAGENT"},
	} {
		pe := rig(c.panel)
		admin := freshAdmin(pe)
		said := c.name + " cannot take part: the panel itself runs on it"
		if r := pe.do("PUT", "/api/systems/"+c.agent+"/wake-update", map[string]any{"enabled": true}, admin...); r.code != 409 || r.body["detail"] != said {
			t.Errorf("panel_hosts names %s: %s switched on for runs: %d %s", c.panel, c.agent, r.code, r.raw)
		}
		if r := pe.do("POST", "/api/admin/wake-update", map[string]any{"update_only": []string{c.agent}}, admin...); r.code != 400 || r.body["detail"] != said {
			t.Errorf("panel_hosts names %s: a run with %s: %d %s", c.panel, c.agent, r.code, r.raw)
		}
		other := pe.do("PUT", "/api/systems/vmagent/wake-update", map[string]any{"enabled": true}, admin...)
		if inPanelsGuest := c.panel == "lab-qemu-201"; (other.body["detail"] == "VMAGENT cannot take part: the panel itself runs on it") != inPanelsGuest || other.code != 409 {
			t.Errorf("panel_hosts names %s: vmagent, an agent in nas-vm, switched on for runs: %d %s", c.panel, other.code, other.raw)
		}
	}
}

// panelUnknownSaid are the two sentences of a panel that cannot follow panel_hosts (ids) to a machine: the
// warning of a host's power menu, and why a host cannot take part in a run.
func panelUnknownSaid(ids string) (warning, whyNot string) {
	why := "the panel cannot follow panel_hosts (" + ids + ") to a machine right now: no row has that id, or the node under it has none"
	return "This may be the machine the panel runs on: " + why + ". Until the row is there or panel_hosts is corrected, a reboot of a host asks for its name.",
		"the panel may run on it (" + why + ")"
}

// panelUnknownRowsAre is panelRowsAre for a panel that cannot follow panel_hosts (ids) to a machine. The rows
// that are named, and the rows the way went over, are the panel's as before (rows). Every other HOST (a node's
// row, an agent's row) may be the panel's machine: its menu says so (panel_unknown, the warning), a reboot of
// it asks for the typed name, and it cannot take part in a run. What is no cut is not touched: cancel and wake
// ask nothing, as on every row. A guest that is not named is, byte for byte, what it is without the panel.
func panelUnknownRowsAre(t *testing.T, what string, pe *plugEnv, rows []string, plain panelLook, ids string) {
	t.Helper()
	got := panelLookOf(t, pe)
	warning, whyNot := panelUnknownSaid(ids)
	if len(got.menu) != len(plain.menu) || len(got.run) != len(plain.run) {
		t.Fatalf("%s: the fixture: %d rows and %d lines, without the panel %d and %d", what, len(got.menu), len(got.run), len(plain.menu), len(plain.run))
	}
	hosts := 0
	for id, kind := range got.kinds {
		r := pe.do("GET", "/api/systems/"+id+"/power", nil, "Cookie", pe.admin)
		asks, line := powerAsksOf(r), got.run[id]
		switch {
		case slices.Contains(rows, id):
			if r.body["kills_panel"] != true || r.body["panel_unknown"] != false || r.body["warning"] != panelRunsHere || asks != powerAsks(kind, true) {
				t.Errorf("%s: the power menu of %s (%s), a row that is named: kills_panel %v, panel_unknown %v, warning %v, asks %q", what, id, kind, r.body["kills_panel"], r.body["panel_unknown"], r.body["warning"], asks)
			}
			if line != nil && (line["can_join"] != false || line["why_not"] != panelRunsOnIt) {
				t.Errorf("%s: %s, a row that is named, on the page of Update unpowered hosts: %v", what, id, line)
			}
		case kind == "node" || kind == "agent":
			hosts++
			if r.body["kills_panel"] != false || r.body["panel_unknown"] != true || r.body["warning"] != warning || asks != powerAsks(kind, true) {
				t.Errorf("%s: the power menu of %s (%s), a host that may be the panel's: kills_panel %v, panel_unknown %v, warning %v, asks %q;\nwant false, true, %q, %q",
					what, id, kind, r.body["kills_panel"], r.body["panel_unknown"], r.body["warning"], asks, warning, powerAsks(kind, true))
			}
			// nothing but what a reboot asks is another menu than without the panel
			if !menusDifferInRebootOnly(plain.menu[id], got.menu[id]) {
				t.Errorf("%s: the power menu of %s differs from the one without the panel in more than the reboot's confirmation:\n%s\n%s", what, id, got.menu[id], plain.menu[id])
			}
			if (line == nil) != (plain.run[id] == nil) || (line != nil && (line["can_join"] != false || line["can_wake"] != false || line["why_not"] != whyNot)) {
				t.Errorf("%s: %s, a host that may be the panel's, on the page of Update unpowered hosts: %v", what, id, line)
			}
		default: // a guest that is not named
			if r.body["kills_panel"] != false || r.body["panel_unknown"] != false || r.body["warning"] != nil || asks != powerAsks(kind, false) || got.menu[id] != plain.menu[id] {
				t.Errorf("%s: the power menu of the guest %s is not what it is without the panel:\n%s\n%s", what, id, got.menu[id], plain.menu[id])
			}
		}
	}
	if hosts < 5 {
		t.Fatalf("%s: the fixture: %d hosts that are not named", what, hosts)
	}
}

// menusDifferInRebootOnly reports whether two power menus are the same but for panel_unknown, the warning and
// what a reboot asks ("click" in the one, "typed" in the other).
func menusDifferInRebootOnly(plain, unknown string) bool {
	var a, b map[string]any
	if json.Unmarshal([]byte(plain), &a) != nil || json.Unmarshal([]byte(unknown), &b) != nil {
		return false
	}
	delete(a, "panel_unknown")
	delete(b, "panel_unknown")
	delete(a, "warning")
	delete(b, "warning")
	for _, m := range []map[string]any{a, b} {
		list, _ := m["actions"].([]any)
		for _, x := range list {
			if act, _ := x.(map[string]any); act["action"] == "reboot" {
				delete(act, "confirm")
			}
		}
	}
	return reflect.DeepEqual(a, b)
}

// panel_hosts that cannot be followed to a machine: an id no row has, a guest or an agent whose node has no
// row, links that lead back. What is named counts as it is written, with the rows the way went over before it
// ended: those rows say "The panel itself runs here", as before.
//
// And the panel does not know which machine it runs on. It used to guess nothing for that and mark nothing
// more ("an id of panel_hosts needs no row"), which left the node under the panel without a mark and its
// socket without a lock for as long as the row was missing: a panel in a VM, named by the VM's id, in the
// minutes after a start, or for as long as that Proxmox API does not answer. Now it fails closed for every
// host until the row is there or panel_hosts is corrected: a reboot of a node's or an agent's row asks for the
// typed name, no host takes part in a run, and no socket is switched off. A guest that is not named is not
// touched, and neither is anything that is no cut (TestPanelThatCannotFollowPanelHostsFailsClosed).
func TestPanelHostsThatCannotBeFollowedToAMachine(t *testing.T) {
	plain := panelLookOf(t, plugNodeRig{link: "lab-node-pve2", noPanel: true, power: true}.env(t))
	for _, w := range []panelWay{
		// via2agent's entry names lostagent, which runs in lost-vm: the three are one machine, whichever is named
		{"an agent in a guest whose node has no row", "lostagent", []string{"lostagent", "lab-qemu-301", "via2agent"}},
		{"an agent two steps from such a guest", "via2agent", []string{"via2agent", "lostagent", "lab-qemu-301"}},
		{"a VM whose node has no row", "lab-qemu-301", []string{"lab-qemu-301", "lostagent", "via2agent"}},
		{"a container whose node has no row", "lab-lxc-302", []string{"lab-lxc-302"}},
		{"an id no row has", "old-box", nil},
		{"the id of a guest that is not there", "lab-qemu-999", nil},
	} {
		what := fmt.Sprintf("panel_hosts names %s (%s)", w.panel, w.name)
		pe := plugNodeRig{link: "lab-node-pve2", panelHosts: w.panel, power: true}.env(t)
		panelUnknownRowsAre(t, what, pe, w.rows, plain, w.panel)
		// the socket of the node says the same: it may be the panel's, and is not switched off
		if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["lock"] != model.SocketLockPanelUnknown || s["switchable"] != false {
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
		pe := newPlugEnvWith(t, strings.Replace(yaml, "panel_hosts: [panel-box]", "panel_hosts: ["+panelHosts+"]", 1)) // ("": the panel was told nothing)
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
	plainLoop := panelLookOf(t, loop(""))
	for _, w := range []panelWay{
		{"an agent whose links lead back to it", "loopa", []string{"loopa", "loopb"}},
		{"the other agent of the two", "loopb", []string{"loopa", "loopb"}},
	} {
		panelUnknownRowsAre(t, fmt.Sprintf("panel_hosts names %s (%s)", w.panel, w.name), loop(w.panel), w.rows, plainLoop, w.panel)
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
	for _, panel := range []string{"nas-box", "nas-node-nas", "panel-box"} { // (the last: a panel that runs on another machine)
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
		on := panel != "panel-box"
		look := panelLookOf(t, pe)
		for id, menu := range look.menu {
			if want := (on && id == "nas-box") || id == panel; strings.Contains(menu, `"kills_panel":true`) != want || strings.Contains(menu, panelRunsHere) != want || strings.Contains(menu, `"panel_unknown":true`) {
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

// A panel that was not told where it runs (no panel_hosts) marks no row: no menu has kills_panel, panel_unknown
// or the warning, no line says "the panel itself runs on it", and every answer is, byte for byte, the one of a
// panel that runs on a machine beside these (the rig's panel-box, whose own row is the one that differs). An
// empty panel_hosts is not "the panel does not know its machine": nothing fails closed for it, as before.
func TestPowerMenuWithoutPanelHostsMarksNoRow(t *testing.T) {
	for _, lone := range []bool{false, true} {
		none := panelLookOf(t, plugNodeRig{link: "lab-node-pve2", noPanel: true, lone: lone, power: true, nodeAgent: "self_update: true, control_token_env: CTL_A,"}.env(t))
		elsewhere := panelLookOf(t, plugNodeRig{link: "lab-node-pve2", panelHosts: "panel-box", lone: lone, power: true, nodeAgent: "self_update: true, control_token_env: CTL_A,"}.env(t))
		if len(none.menu) < 15 || len(none.run) < 9 {
			t.Fatalf("the fixture: %d rows, %d of them machines with an agent", len(none.menu), len(none.run))
		}
		for id, menu := range none.menu {
			if !strings.Contains(menu, `"kills_panel":false`) || !strings.Contains(menu, `"panel_unknown":false`) || !strings.Contains(menu, `"warning":null`) || strings.Contains(menu, "The panel itself") {
				t.Errorf("lone %v, no panel_hosts: the power menu of %s: %s", lone, id, menu)
			}
			if id == "panel-box" {
				continue
			}
			if menu != elsewhere.menu[id] {
				t.Errorf("lone %v, no panel_hosts: the power menu of %s is not the one of a panel that runs elsewhere:\n%s\n%s", lone, id, menu, elsewhere.menu[id])
			}
		}
		for id, line := range none.run {
			if id == "panel-box" {
				continue
			}
			if line["why_not"] == panelRunsOnIt || line["why"] == panelRunsOnIt || !reflect.DeepEqual(line, elsewhere.run[id]) {
				t.Errorf("lone %v, no panel_hosts: %s on the page of Update unpowered hosts: %v, with a panel that runs elsewhere %v", lone, id, line, elsewhere.run[id])
			}
		}
	}
}

// An agent whose entry names a guest the panel has no row for (links_to: lab-qemu-999) runs in that guest for
// all the panel can know. With that id in panel_hosts the agent's row is the panel's, as it is written: no row
// says which guest that is, and none is needed.
func TestPowerMenuOfAnAgentInThePanelsGuestWithoutARow(t *testing.T) {
	rig := func(panelHosts string) *plugEnv {
		return plugNodeRig{link: "lab-node-pve2", otherExtra: "links_to: lab-qemu-999,", panelHosts: panelHosts, noPanel: panelHosts == "", power: true}.env(t)
	}
	plain := panelLookOf(t, rig(""))
	if plain.kinds["other"] != "agent" || plain.kinds["lab-qemu-999"] != "" {
		t.Fatalf("the fixture: the row of other is %q, of lab-qemu-999 %q", plain.kinds["other"], plain.kinds["lab-qemu-999"])
	}
	// (which node that guest is on the panel cannot tell: every other host may be the panel's machine)
	panelUnknownRowsAre(t, "panel_hosts names lab-qemu-999, the guest without a row that other runs in", rig("lab-qemu-999"), []string{"other"}, plain, "lab-qemu-999")
	panelUnknownRowsAre(t, "panel_hosts names lab-qemu-998, another guest without a row", rig("lab-qemu-998"), nil, plain, "lab-qemu-998")
}

// An agent whose entry names the node's own agent (links_to: pve2agent) runs where that agent runs, on the
// node. The node's agent has no row of its own while the node's row is there, so the id that is written is no
// row's, and (with a second agent beside it) not one of the node's ids either: the way goes on to the node
// that agent's entry names.
func TestPowerMenuOfAnAgentWhoseEntryNamesTheNodesOwnAgent(t *testing.T) {
	rig := func(panelHosts string) *plugEnv {
		return plugNodeRig{link: "lab-node-pve2", otherExtra: "links_to: pve2agent,", panelHosts: panelHosts, noPanel: panelHosts == "", power: true}.env(t)
	}
	plain := panelLookOf(t, rig(""))
	if plain.kinds["other"] != "agent" || plain.kinds["pve2agent"] != "" {
		t.Fatalf("the fixture: the row of other is %q, of pve2agent %q", plain.kinds["other"], plain.kinds["pve2agent"])
	}
	panelRowsAre(t, "panel_hosts names the node, other's entry names the node's agent", rig("lab-node-pve2"), []string{"lab-node-pve2", "sideagent", "other"}, plain)
	panelRowsAre(t, "panel_hosts names a VM of the node, other's entry names the node's agent", rig("lab-qemu-202"), []string{"lab-qemu-202", "lab-node-pve2", "vm2agent", "sideagent", "other"}, plain)
	panelRowsAre(t, "panel_hosts names the other node, other's entry names this node's agent", rig("lab-node-pve"), []string{"lab-node-pve"}, plain)
}

// The lock of a socket did not need the agents' rows: a socket that is linked to an agent is followed down to
// the machine under it, and that machine is one of the rows the panel runs on. Held here for the rows the
// power menu now marks too, and for one it does not mark: a socket linked to an agent in ANOTHER guest of the
// panel's node feeds that node, and is the panel's, while that agent's power menu is not.
func TestPlugSocketLinkedToAnAgentOnThePanelsMachine(t *testing.T) {
	for _, c := range []struct{ name, link, panel string }{
		{"an agent in the panel's VM", "vm2agent", "lab-qemu-202"},
		{"an agent in the panel's container", "ctagent", "lab-lxc-203"},
		{"an agent whose entry names an agent in the panel's VM", "viaagent", "lab-qemu-201"},
		{"the second agent of the panel's node", "sideagent", "lab-node-pve2"},
		{"the second agent of the node, the panel in a VM of it", "sideagent", "lab-qemu-202"},
		{"an agent in another guest of the panel's node", "vmagent", "lab-qemu-202"},
	} {
		plugNodeLocked(t, plugNodeRig{link: c.link, panelHosts: c.panel}.env(t), "linked to "+c.name, model.SocketLockPanel, plugSaidPanel)
	}
	// the panel on the other node: the same links are no lock
	for _, link := range []string{"vm2agent", "sideagent"} {
		pe := plugNodeRig{link: link, panelHosts: "lab-qemu-100"}.env(t)
		if s := pe.socketOf(pe.admin, "other/rack-bar", "node"); s["lock"] != nil || s["switchable"] != true || s["system_id"] != "lab-node-pve2" {
			t.Errorf("linked to %s, the panel in a guest of the other node: %v", link, s)
		}
	}

	// One sentence of a lock is another one now, of a socket that was locked before: it is linked to an agent
	// whose entry names a guest the panel has no row for, and panel_hosts has that guest's id. The panel
	// cannot follow the link to a machine (link_unknown, as before, with a panel that runs elsewhere); with
	// the id in panel_hosts the agent's row is the panel's as it is written, and the lock says the nearer
	// reason, as it does when panel_hosts has the agent's id and the link the guest's.
	ghost := func(panelHosts string) map[string]any {
		t.Setenv("TOK_G", testutil.ReadToken+"g")
		if strings.Count(plugConfig, "socket_links: {rack-bar/node: lab-node-pve}") != 1 || strings.Count(plugConfig, "\nagents:\n") != 1 || strings.Count(plugConfig, "panel_hosts: [panel-box]") != 1 {
			t.Fatal("the rig's config is not the one this fixture changes")
		}
		yaml := strings.Replace(plugConfig, "socket_links: {rack-bar/node: lab-node-pve}", "socket_links: {rack-bar/node: ghostagent}", 1)
		yaml = strings.Replace(yaml, "\nagents:\n", "\nagents:\n  - {id: ghostagent, name: GHOSTAGENT, url: \"http://ghostagent\", token_env: TOK_G, links_to: lab-qemu-999}\n", 1)
		pe := newPlugEnvWith(t, strings.Replace(yaml, "panel_hosts: [panel-box]", "panel_hosts: ["+panelHosts+"]", 1))
		pe.b.Mu.Lock()
		pe.b.Metrics["ghostagent"] = func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			m["hostname"] = "ghostagent"
			delete(m, "power_sources")
			return m, nil
		}
		pe.b.Mu.Unlock()
		pe.poll()
		pe.poll()
		if x := pe.do("GET", "/api/systems/ghostagent", nil, "Cookie", pe.admin); x.code != 200 || x.body["kind"] != "agent" {
			t.Fatalf("the fixture: the row of ghostagent: %d %v", x.code, x.body)
		}
		for _, to := range []string{"off", "on"} {
			if r := pe.flip("other/rack-bar", "node", to); r.code != 409 {
				t.Errorf("panel_hosts names %s: %s for the socket linked to ghostagent: %d %s", panelHosts, to, r.code, r.raw)
			}
		}
		return pe.socketOf(pe.admin, "other/rack-bar", "node")
	}
	if s := ghost("panel-box"); s["switchable"] != false || s["lock"] != model.SocketLockLinkUnknown {
		t.Errorf("linked to an agent in a guest without a row, the panel elsewhere: %v", s)
	}
	if s := ghost("lab-qemu-999"); s["switchable"] != false || s["lock"] != model.SocketLockPanel || s["note"] != plugSaidPanel {
		t.Errorf("linked to an agent in a guest without a row, that guest in panel_hosts: %v", s)
	}
}
