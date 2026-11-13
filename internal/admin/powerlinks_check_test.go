package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `fairy admin config check` and the plugs an agent relays (power_links, power_exclude_from_total in the
// panel's entry of that agent): one ok line per agent with what is set, and a note for what cannot work: a
// device id no device can have, a link to an id that is no machine's, a link to a node whose agent the
// configuration does not decide. A link to an agent's id, to its node's id (either id of one machine) or to a
// guest has none. Whether the agent has a device of that id is the running panel's to say (its log); the ok
// line says so. Notes are no warnings: exit code 0.
func TestConfigCheckAndTheLinksOfRelayedPlugs(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	t.Setenv("TOK_R", "r")
	t.Setenv("PVE_S", "s")
	t.Setenv("FAIRY_SESSION_SECRET", strings.Repeat("no-secret-", 5)) // made here, for this test only
	y := "public_url: https://p.example\ndata_dir: " + dir + "/data\noidc: {issuer: \"https://id.example\", client_id: c, allowed_groups: [users], control_groups: [admins]}\n" +
		"proxmox:\n" +
		"  - {id: lab, id_prefix: lab-, urls: [\"https://pve:8006\"], token_id: \"fairy@pve!panel\", token_secret_env: PVE_S}\n" +
		"  - {id: site, id_prefix: site-, urls: [\"https://pve2:8006\"], token_id: \"fairy@pve!panel\", token_secret_env: PVE_S}\n" +
		"agents:\n" +
		"  - {id: relay, url: \"http://relay:9101\", token_env: TOK_R,\n" +
		"     power_links: {plug-agent: lab-b-agent, plug-node: lab-node-pve-b, plug-guest: lab-qemu-100, plug-typo: lab-b-agnet, plug-undecided: site-node-pve, Bad_ID: relay},\n" +
		"     power_exclude_from_total: [corner-plug, plug-node]}\n" +
		"  - {id: lab-b-agent, url: \"http://lab-b:9101\", token_env: TOK_R, node: pve-b}\n" +
		"  - {id: lone-agent, url: \"http://lone:9101\", token_env: TOK_R, node: pve}\n" + // its id begins with no id_prefix: not decided
		"  - {id: plain, url: \"http://plain:9101\", token_env: TOK_R}\n"
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb)
	got := out.String()
	if !strings.Contains(got, "config "+cfg+": valid") {
		t.Fatalf("the fixture is not a valid config:\n%s%s", got, errb.String())
	}
	for _, want := range []string{
		"ok    agent relay: devices it relays: linked Bad_ID -> relay, plug-agent -> lab-b-agent, plug-guest -> lab-qemu-100, plug-node -> lab-node-pve-b, plug-typo -> lab-b-agnet, plug-undecided -> site-node-pve; " +
			"not in the total: corner-plug, plug-node (the device ids are the agent's own: the running panel says in its log when the agent reports no device with such an id)\n",
		"note  agent relay: \"Bad_ID\" in power_links / power_exclude_from_total is not an id a device can have (lowercase letters, digits and dashes, as in the agent's own power_sources): nothing is linked or left out for it\n",
		"note  agent relay: power_links: device plug-typo is linked to lab-b-agnet, which is no agent's id and not the id of a node or a guest of a proxmox: entry: its watts are on no row. A typing mistake in the id?\n",
		"note  agent relay: power_links: device plug-undecided is linked to the node site-node-pve, and the configuration does not say which agent's machine that node is (it can be the node of lone-agent): " +
			"the watts are on the node's row while it is there, and on no row after a restart of the panel with the machine off. To say whose node it is, write links_to: site-node-pve in that agent's entry\n",
	} {
		if strings.Count(got, want) != 1 {
			t.Errorf("the output lacks (or has twice) %q", want)
		}
	}
	if n := strings.Count(got, "note  agent relay:"); n != 3 {
		t.Errorf("%d notes about the relay's links, want 3 (none for a link to an agent, to its node, to a guest):\n%s", n, got)
	}
	if strings.Contains(got, "agent plain: devices") || strings.Contains(got, "agent lab-b-agent: devices") {
		t.Errorf("a line for an agent with nothing set:\n%s", got)
	}
	if code != 0 || strings.Contains(got, "WARN  agent relay") {
		t.Errorf("notes are no warnings (exit %d):\n%s", code, got)
	}
}
