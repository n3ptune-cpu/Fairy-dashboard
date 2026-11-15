package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `fairy admin config check` and the sockets of the plugs an agent relays (socket_links and
// home_assistant_system in the panel's entry of that agent, build 17): one ok line per agent with what is set
// and what the panel does with it, and a note for a row id that is no machine's. A link to an agent, a node or
// a guest has none. Notes are no warnings: exit code 0. A line of the wrong form is not a note: the file does
// not load with it.
func TestConfigCheckAndTheSocketsOfRelayedPlugs(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	t.Setenv("TOK_R", "r")
	t.Setenv("PVE_S", "s")
	t.Setenv("FAIRY_SESSION_SECRET", strings.Repeat("no-secret-", 5)) // made here, for this test only
	head := "public_url: https://p.example\ndata_dir: " + dir + "/data\noidc: {issuer: \"https://id.example\", client_id: c, allowed_groups: [users], control_groups: [admins]}\n" +
		"proxmox:\n" +
		"  - {id: lab, id_prefix: lab-, urls: [\"https://pve:8006\"], token_id: \"fairy@pve!panel\", token_secret_env: PVE_S}\n" +
		"agents:\n"
	y := head +
		"  - {id: relay, url: \"http://relay:9101\", token_env: TOK_R,\n" +
		"     socket_links: {desk-bar/ch3: plain, desk-bar/ch1: lab-node-pve-b, desk-bar/ch2: lab-qemu-100, desk-bar/ch4: plian},\n" +
		"     home_assistant_system: lab-qemu-104}\n" +
		"  - {id: other, url: \"http://other:9101\", token_env: TOK_R, home_assistant_system: lab-qmeu-104}\n" +
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
	tail := " (the panel asks before such a socket is switched off while its machine runs, and never switches the socket of the machine that reads the plugs, " +
		"of Home Assistant's machine or of its own; whether a socket can be switched at all is set in the agent's own config.yaml. " +
		"The running panel says in its log when the agent reports no socket with such a name)\n"
	for _, want := range []string{
		"ok    agent relay: sockets of the plugs it relays: desk-bar/ch1 -> lab-node-pve-b, desk-bar/ch2 -> lab-qemu-100, desk-bar/ch3 -> plain, desk-bar/ch4 -> plian; " +
			"its Home Assistant runs on lab-qemu-104" + tail,
		"ok    agent other: sockets of the plugs it relays; its Home Assistant runs on lab-qmeu-104" + tail,
		"note  agent relay: socket_links: socket desk-bar/ch4 is linked to plian, which is no agent's id and not the id of a node or a guest of a proxmox: entry. " +
			"The panel then cannot tell whether that machine runs: to switch the socket off, the id has to be typed. A typing mistake in the id?\n",
		"note  agent other: home_assistant_system: lab-qmeu-104 is no agent's id and not the id of a node or a guest of a proxmox: entry. " +
			"While no row has this id the panel cannot tell which socket feeds Home Assistant, and switches no socket of this agent that is linked to a machine. A typing mistake in the id?\n",
	} {
		if strings.Count(got, want) != 1 {
			t.Errorf("the output lacks (or has twice) %q", want)
		}
	}
	if n := strings.Count(got, "note  agent relay: socket_links"); n != 1 {
		t.Errorf("%d notes about the relay's socket links, want 1 (none for a link to an agent, a node or a guest):\n%s", n, got)
	}
	if strings.Contains(got, "agent plain: sockets") {
		t.Errorf("a line for an agent with nothing set:\n%s", got)
	}
	if code != 0 || strings.Contains(got, "WARN  agent relay") || strings.Contains(got, "WARN  agent other") {
		t.Errorf("notes are no warnings (exit %d):\n%s", code, got)
	}

	// a line that can never work: the file does not load, and the check says which agent and which line
	bad := head + "  - {id: relay, url: \"http://relay:9101\", token_env: TOK_R, socket_links: {desk-bar: plain}}\n"
	if err := os.WriteFile(cfg, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	code = Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb)
	if all := out.String() + errb.String(); code == 0 || !strings.Contains(all, `agent relay: socket_links: "desk-bar" is not "<device id>/<socket id>"`) {
		t.Errorf("a socket_links line of the wrong form: exit %d\n%s", code, all)
	}
}
