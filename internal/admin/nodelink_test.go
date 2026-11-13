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
