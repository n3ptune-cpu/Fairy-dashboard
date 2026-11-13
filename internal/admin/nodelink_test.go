package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `fairy admin config check` prints, as a WARN line, the warning the panel logs at its start for an agent on a
// Proxmox node whose node the configuration does not decide although a systems: entry is written under an id
// that node can have (review of fix13: two hosts whose node is called pve; the wol: of the second is not the
// first one's). The line says what to add. With links_to in the agent's entry, or without the systems: entry,
// there is no such line.
func TestConfigCheckWarnsAboutAnAgentWhoseNodeIsNotDecided(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	check := func(agent, systems string) string {
		t.Helper()
		y := "public_url: https://p.example\ndata_dir: " + dir + "/data\noidc: {issuer: \"https://id.example\", client_id: c}\n" +
			"proxmox:\n" +
			"  - {id: site1, id_prefix: site1-, standalone: true, urls: [\"https://pve1:8006\"], token_id: \"fairy@pve!panel\", token_secret_env: PVE_S}\n" +
			"  - {id: site2, id_prefix: site2-, standalone: true, urls: [\"https://pve2:8006\"], token_id: \"fairy@pve!panel\", token_secret_env: PVE_S}\n" +
			"agents:\n" +
			"  - {id: relay, url: \"http://relay:9101\", token_env: TOK_R, control_token_env: CTL_R, wol_relay: true}\n" +
			"  - {id: host1-agent, url: \"http://host1:9101\", token_env: TOK_H, " + agent + "}\n" + systems
		if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb)
		if !strings.Contains(out.String(), "config "+cfg+": valid") {
			t.Fatalf("the fixture is not a valid config:\n%s%s", out.String(), errb.String())
		}
		return out.String()
	}
	const wol = "systems:\n  - {id: site2-node-pve, wol: {mac: \"00:00:5e:00:53:72\", relay: relay}}\n"
	const line = "WARN  agent host1-agent (node: pve): its id begins with the id_prefix of none of the proxmox: entries, so the configuration does not say which node is the agent's " +
		"(it can be site1-node-pve or site2-node-pve). systems: has an entry for site2-node-pve: it is not used for this agent while it is shown under its own id " +
		"(after a restart of the panel with the machine off, until the node's Proxmox API answers). To say which node it is, write links_to: <the id of its node> " +
		"in the agent's entry (links_to: site2-node-pve if that entry is this machine's)\n"
	if out := check("node: pve", wol); !strings.Contains(out, line) || strings.Count(out, "host1-agent (node: pve)") != 1 {
		t.Errorf("no line (or more than one) for the agent whose node is not decided:\n%s", out)
	}
	for name, out := range map[string]string{
		"links_to names the node":        check("node: pve, links_to: site1-node-pve", wol),
		"no systems: entry for the node": check("node: pve", ""),
	} {
		if strings.Contains(out, "does not say which node") {
			t.Errorf("%s: a warning all the same:\n%s", name, out)
		}
	}
}

// Build 14: an agent whose id begins with the id_prefix of several proxmox: entries is given the entry with
// the longest one. `fairy admin config check` says every link that was worked out from an id_prefix, as a
// note (never a reason for exit code 1), so that nothing is guessed without a word; an agent with links_to,
// and a configuration with one proxmox: entry, have no such line.
func TestConfigCheckSaysEveryLinkInferredFromAnIDPrefix(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	t.Setenv("PVE_S", "s")
	t.Setenv("TOK_R", "r")
	t.Setenv("CTL_R", "c")
	t.Setenv("TOK_H", "h")
	t.Setenv("FAIRY_SESSION_SECRET", strings.Repeat("no-secret-", 5)) // made here, for this test only
	check := func(entries, agent string) (string, int) {
		t.Helper()
		y := "public_url: https://p.example\ndata_dir: " + dir + "/data\noidc: {issuer: \"https://id.example\", client_id: c, allowed_groups: [users], control_groups: [admins]}\n" +
			"proxmox:\n" + entries +
			"agents:\n" +
			"  - {id: relay, url: \"http://relay:9101\", token_env: TOK_R, control_token_env: CTL_R, wol_relay: true}\n" +
			"  - {id: alpha-b-box2-agent, url: \"http://box2:9101\", token_env: TOK_H, " + agent + "}\n" +
			"systems:\n  - {id: alpha-b-node-box-2, wol: {mac: \"00:00:5e:00:53:72\", relay: relay}}\n"
		if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb)
		if !strings.Contains(out.String(), "config "+cfg+": valid") {
			t.Fatalf("the fixture is not a valid config:\n%s%s", out.String(), errb.String())
		}
		return out.String(), code
	}
	entry := func(id, prefix string) string {
		return "  - {id: " + id + ", id_prefix: " + prefix + ", standalone: true, urls: [\"https://" + id + ":8006\"], token_id: \"fairy@pve!panel\", token_secret_env: PVE_S}\n"
	}
	three := entry("beta", "beta-") + entry("alpha", "alpha-") + entry("alpha-b", "alpha-b-")
	const line = "note  agent alpha-b-box2-agent (node: BOX-2) -> node alpha-b-node-box-2: inferred from id_prefix alpha-b- of the proxmox: entry alpha-b " +
		"(the longest of alpha-, alpha-b-, which the agent's id begins with); set links_to in the agent's entry to override\n"
	out, code := check(three, "node: BOX-2")
	if strings.Count(out, line) != 1 || strings.Count(out, "inferred from id_prefix") != 1 {
		t.Errorf("no line (or more than one) for the inferred link:\n%s", out)
	}
	if strings.Contains(out, "does not say which node") || strings.Contains(out, "WARN  agent") {
		t.Errorf("build 13's warning for an id that begins with two id_prefixes is still there:\n%s", out)
	}
	if code != 0 || strings.Contains(out, "WARN") {
		t.Errorf("a note made the exit code %d, or something else warns:\n%s", code, out)
	}
	for name, c := range map[string][2]string{
		"links_to names the node": {three, "node: BOX-2, links_to: alpha-b-node-box-2"},
		"one proxmox: entry":      {entry("alpha-b", "alpha-b-"), "node: BOX-2"},
	} {
		if out, _ := check(c[0], c[1]); strings.Contains(out, "inferred") {
			t.Errorf("%s: a line all the same:\n%s", name, out)
		}
	}
}
