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
// can have; an agent whose id is written like a node's or a guest's (the id names two things); links_to that
// leads from agent to agent further than the panel follows, or back. An id of the form a node or a guest has
// cannot be checked from a file and has no line; neither has an empty panel_hosts.
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
	twice := func(id string) string {
		return "WARN  agent " + id + ": its id is written like the id of a node or a guest of a proxmox: entry"
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

	// an agent whose id is written like a guest's, a container's or a node's; one that only begins like it is none
	code, got = check(head + agent("lab-qemu-100", "") + agent("lab-lxc-7", "") + agent("lab-node-pve", "") + agent("lab-qemu-box", "") + agent("lab-nodes", "") + "panel_hosts: [lab-qemu-100]\n")
	for _, id := range []string{"lab-qemu-100", "lab-lxc-7", "lab-node-pve"} {
		if strings.Count(got, twice(id)) != 1 {
			t.Errorf("no warning (or two) for the agent %s:\n%s", id, got)
		}
	}
	if code != 1 || strings.Count(got, "its id is written like") != 3 || strings.Contains(got, "WARN  panel_hosts:") ||
		!strings.Contains(got, "a socket that is linked to this id is locked, home_assistant_system with it switches no socket of that agent, and panel_hosts with it leaves the panel without a machine it knows as its own. Give the agent another id\n") {
		t.Errorf("agents with ids of the form a node or a guest has (exit %d):\n%s", code, got)
	}

	// links_to from agent to agent: eight agents on the way are followed, nine are not; and a way that comes back
	var long, ok strings.Builder
	for i := 0; i < 10; i++ {
		to := fmt.Sprintf("c%d", i+1)
		if i == 9 {
			to = "lab-node-pve"
		}
		long.WriteString(agent(fmt.Sprintf("c%d", i), ", links_to: "+to))
	}
	for i := 0; i < 9; i++ {
		to := fmt.Sprintf("d%d", i+1)
		if i == 8 {
			to = "lab-node-pve"
		}
		ok.WriteString(agent(fmt.Sprintf("d%d", i), ", links_to: "+to))
	}
	code, got = check(head + long.String() + ok.String() + agent("loop-a", ", links_to: loop-b") + agent("loop-b", ", links_to: loop-a"))
	if code != 1 || strings.Count(got, chain("c0")) != 1 || strings.Count(got, chain("loop-a")) != 1 || strings.Count(got, chain("loop-b")) != 1 || strings.Count(got, "links_to leads from agent to agent") != 3 {
		t.Errorf("links_to from agent to agent (exit %d):\n%s", code, got)
	}
}
