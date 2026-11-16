package admin

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `fairy admin config check` and the ids the panel follows down to its own machine. Each of these is a state
// in which the running panel fails closed, so each is a warning (exit code 1): an id in panel_hosts that no row
// can have; an agent whose id is exactly the id of a node the file itself names (the id names two things);
// links_to that leads from agent to agent further than the panel follows, or back. An id of the form a node or
// a guest has cannot be checked from a file and has no line; neither has an empty panel_hosts. An agent whose
// id only looks like a node's or a guest's has a note, and no warning: nothing is locked for a look.
func TestConfigCheckAndThePanelsOwnMachine(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	t.Setenv("TOK_R", "r")
	t.Setenv("PVE_S", "s")
	t.Setenv("FAIRY_SESSION_SECRET", strings.Repeat("no-secret-", 5)) // made here, for this test only
	head := "public_url: https://p.example\ndata_dir: " + dir + "/data\noidc: {issuer: \"https://id.example\", client_id: c, allowed_groups: [users], control_groups: [admins]}\n" +
		"proxmox:\n" +
		"  - {id: lab, id_prefix: lab-, urls: [\"https://pve:8006\"], token_id: \"fairy@pve!panel\", token_secret_env: PVE_S}\n" +
		"agents:\n"
	agent := func(id, more string) string {
		return fmt.Sprintf("  - {id: %s, url: \"http://%s:9101\", token_env: TOK_R%s}\n", id, id, more)
	}
	check := func(y string) (int, string) {
		t.Helper()
		if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb)
		if !strings.Contains(out.String(), "config "+cfg+": valid") {
			t.Fatalf("the fixture is not a valid config:\n%s%s", out.String(), errb.String())
		}
		return code, out.String()
	}
	noRow := func(id string) string {
		return "WARN  panel_hosts: " + id + " is no agent's id and not the id of a node or a guest of a proxmox: entry, so no row can have it. " +
			"While an id of panel_hosts has no row the panel cannot tell which machine it runs on: it switches no socket off, asks for the typed name before a reboot of any host, " +
			"and lets no host take part in \"Update unpowered hosts\". A typing mistake in the id?\n"
	}
	looks := func(id string) string {
		return "note  agent " + id + ": its id looks like the id of a node or a guest of a proxmox: entry (<id_prefix>node-<name>, <id_prefix>qemu-<number>, <id_prefix>lxc-<number>). " +
			"Nothing is locked for what an id looks like. But if that entry has a node or a guest with exactly this id, the id names two things, and from the moment its Proxmox API lists it " +
			"a socket that is linked to this id is locked, home_assistant_system with it switches no socket of that agent, and panel_hosts with it leaves the panel without a machine it knows as its own. " +
			"If there is one, give the agent another id\n"
	}
	twice := func(id, by string) string {
		return "WARN  agent " + id + ": its id is also the id of the Proxmox node that the entry of agent " + by + " names (node:). " +
			"The panel keeps one row for an id, so this agent and that node take each other's place, and the id names two things. The panel does not guess which is meant: " +
			"a socket that is linked to this id is locked, home_assistant_system with it switches no socket of that agent, and panel_hosts with it leaves the panel without a machine it knows as its own. " +
			"Give the agent another id\n"
	}
	chain := func(id string) string {
		return "WARN  agent " + id + ": links_to leads from agent to agent over more than 8 agents, or back to one of them."
	}

	// what is right as it is: the panel's machine named by an agent's id, by a guest's and by a node's
	code, got := check(head + agent("plain", "") + agent("in-vm", ", links_to: lab-qemu-105") + "panel_hosts: [plain, lab-qemu-105, lab-node-pve, in-vm]\n")
	if code != 0 || strings.Contains(got, "WARN") || strings.Contains(got, "panel_hosts:") {
		t.Errorf("panel_hosts with an agent, a guest, a node and an agent in a guest (exit %d):\n%s", code, got)
	}
	// ... and no panel_hosts at all
	if code, got = check(head + agent("plain", "")); code != 0 || strings.Contains(got, "WARN") {
		t.Errorf("no panel_hosts (exit %d):\n%s", code, got)
	}

	// an id no row can have: a typing mistake in an agent's id, and one in the form of a guest's id
	code, got = check(head + agent("plain", "") + "panel_hosts: [plian, lab-qmeu-105, lab-qemu-105x, lab-qemu-105]\n")
	if code != 1 || strings.Count(got, noRow("plian")) != 1 || strings.Count(got, noRow("lab-qmeu-105")) != 1 || strings.Count(got, noRow("lab-qemu-105x")) != 1 || strings.Count(got, "WARN  panel_hosts:") != 3 {
		t.Errorf("panel_hosts with ids no row can have (exit %d):\n%s", code, got)
	}

	// An agent whose id looks like a guest's, a container's or a node's: a note for each, no warning, exit code
	// 0. Whether there is such a guest or node is not in the file, and the running panel locks nothing for a
	// look (the agents lab-node-red and lab-node-pve-agent of a setup without nodes of those names had a
	// warning here, and their sockets locked). One that does not look like one has no line.
	code, got = check(head + agent("lab-qemu-100", "") + agent("lab-lxc-7", "") + agent("lab-node-pve", "") + agent("lab-node-red", "") + agent("lab-node-pve-agent", "") +
		agent("lab-qemu-box", "") + agent("lab-nodes", "") + agent("lab-pve-agent", "") + "panel_hosts: [lab-qemu-100]\n")
	for _, id := range []string{"lab-qemu-100", "lab-lxc-7", "lab-node-pve", "lab-node-red", "lab-node-pve-agent"} {
		if strings.Count(got, looks(id)) != 1 {
			t.Errorf("no note (or two) for the agent %s:\n%s", id, got)
		}
	}
	if code != 0 || strings.Count(got, "its id looks like") != 5 || strings.Contains(got, "WARN") {
		t.Errorf("agents with ids that look like a node's or a guest's (exit %d):\n%s", code, got)
	}
	// The one case the file itself decides: an agent's id is exactly the id of the node another agent's entry
	// names with node: (or its own). That is a warning, and the only one about the look of an id.
	code, got = check(head + agent("pve-agent", ", node: PVE") + agent("lab-node-pve", "") + agent("lab-node-red", "") + agent("lab-node-nas", ", node: NAS"))
	if code != 1 || strings.Count(got, twice("lab-node-pve", "pve-agent")) != 1 || strings.Count(got, twice("lab-node-nas", "lab-node-nas")) != 1 || strings.Count(got, "WARN  agent") != 2 ||
		strings.Count(got, looks("lab-node-red")) != 1 || strings.Count(got, "its id looks like") != 1 {
		t.Errorf("agents whose id is the id of a node the file names (exit %d):\n%s", code, got)
	}

	// links_to from agent to agent: a way over eight agents is followed, one over nine is not, as in the running
	// panel (the same count: every agent on the way that names where it runs); and a way that comes back
	for _, end := range []string{"lab-node-pve", "lab-qemu-100"} {
		for n, warned := range map[int][]string{1: nil, 8: nil, 9: {"c0"}, 10: {"c0", "c1"}, 12: {"c0", "c1", "c2", "c3"}} {
			var y strings.Builder
			for i := 0; i < n; i++ {
				to := fmt.Sprintf("c%d", i+1)
				if i == n-1 {
					to = end
				}
				y.WriteString(agent(fmt.Sprintf("c%d", i), ", links_to: "+to))
			}
			code, got = check(head + y.String() + "panel_hosts: [c0]\n")
			if (code == 1) != (len(warned) > 0) || strings.Count(got, "links_to leads from agent to agent") != len(warned) {
				t.Errorf("%d agents, each with links_to the next, the last to %s (exit %d): want a line for %v\n%s", n, end, code, warned, got)
			}
			for _, id := range warned {
				if strings.Count(got, chain(id)) != 1 {
					t.Errorf("%d agents down to %s: no line for %s:\n%s", n, end, id, got)
				}
			}
		}
	}
	// (the last agent names its node with node:, or names nothing and is a machine of its own: the first counts,
	// the second does not)
	var y strings.Builder
	for i := 0; i < 8; i++ {
		y.WriteString(agent(fmt.Sprintf("c%d", i), fmt.Sprintf(", links_to: c%d", i+1)))
	}
	if code, got = check(head + y.String() + agent("c8", ", node: PVE")); code != 1 || strings.Count(got, chain("c0")) != 1 || strings.Count(got, "links_to leads from agent to agent") != 1 {
		t.Errorf("nine agents, the last with node: (exit %d):\n%s", code, got)
	}
	if code, got = check(head + y.String() + agent("c8", "")); code != 0 || strings.Contains(got, "links_to leads from agent to agent") {
		t.Errorf("eight agents and a machine of its own (exit %d):\n%s", code, got)
	}
	code, got = check(head + agent("loop-a", ", links_to: loop-b") + agent("loop-b", ", links_to: loop-a") + agent("into", ", links_to: loop-a"))
	if code != 1 || strings.Count(got, chain("loop-a")) != 1 || strings.Count(got, chain("loop-b")) != 1 || strings.Count(got, chain("into")) != 1 || strings.Count(got, "links_to leads from agent to agent") != 3 {
		t.Errorf("links_to that leads back (exit %d):\n%s", code, got)
	}
}
