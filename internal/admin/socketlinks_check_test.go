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
// and what the panel does with it. A socket that is linked to an id that is no machine's is a warning (exit code
// 1): the panel locks that socket until the link names a row. An id of the form a node or a guest has cannot be
// checked here, and has no line. home_assistant_system with such an id is a warning too: the panel switches no
// socket of that agent. A line of the wrong form is neither: the file does not load with it.
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
		"WARN  agent relay: socket_links: socket desk-bar/ch4 is linked to plian, which is no agent's id and not the id of a node or a guest of a proxmox: entry. " +
			"The panel locks that socket (it is switched neither on nor off from the panel) until the link names a row. A typing mistake in the id?\n",
		"WARN  agent other: home_assistant_system: lab-qmeu-104 is no agent's id and not the id of a node or a guest of a proxmox: entry. " +
			"While no row has this id the panel cannot tell which socket feeds Home Assistant, and switches no socket of the plugs this agent relays, on or off. A typing mistake in the id?\n",
	} {
		if strings.Count(got, want) != 1 {
			t.Errorf("the output lacks (or has twice) %q", want)
		}
	}
	if n := strings.Count(got, "agent relay: socket_links"); n != 1 {
		t.Errorf("%d lines about the relay's socket links, want 1 (none for a link to an agent, a node or a guest):\n%s", n, got)
	}
	if strings.Contains(got, "agent plain: sockets") {
		t.Errorf("a line for an agent with nothing set:\n%s", got)
	}
	if code != 1 || strings.Count(got, "WARN  agent relay") != 1 || strings.Count(got, "WARN  agent other") != 1 || strings.Count(got, "WARN  ") != 2 {
		t.Errorf("two warnings, for the link to no machine and for the home_assistant_system that names none (exit %d):\n%s", code, got)
	}
	// with the link put right one warning is left (until the second review of the fixes the mistyped
	// home_assistant_system was a note, and this was exit 0) ...
	y = strings.Replace(y, "desk-bar/ch4: plian", "desk-bar/ch4: plain", 1)
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb); code != 1 || strings.Count(out.String(), "WARN  ") != 1 || !strings.Contains(out.String(), "WARN  agent other: home_assistant_system: lab-qmeu-104") {
		t.Errorf("the link put right (exit %d):\n%s", code, out.String())
	}
	// ... and with the id put right none, and the exit code says so. An id of the form a guest has is not checked
	// here: only the running panel knows the guests.
	if err := os.WriteFile(cfg, []byte(strings.Replace(y, "home_assistant_system: lab-qmeu-104", "home_assistant_system: lab-qemu-104", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb); code != 0 || strings.Contains(out.String(), "WARN  ") {
		t.Errorf("the links and the id put right (exit %d):\n%s", code, out.String())
	}

	// Three things the panel needs to be told before it switches sockets, as far as a file can show them. The
	// check reads files and asks no machine: it does not know which agent reports sockets, so it goes by the two
	// keys. (1) socket_links without home_assistant_system: the panel switches NO socket of that agent, a
	// warning. (2) home_assistant_system without socket_links: every socket of a powerbar is then linked to
	// nothing, a note. (3) no panel_hosts while an entry has one of the keys: the panel locks no socket as its
	// own, a note. None of them for an agent, or a file, without the keys.
	lacks := head +
		"  - {id: relay, url: \"http://relay:9101\", token_env: TOK_R, socket_links: {desk-bar/ch3: plain}}\n" +
		"  - {id: other, url: \"http://other:9101\", token_env: TOK_R, home_assistant_system: plain}\n" +
		"  - {id: plain, url: \"http://plain:9101\", token_env: TOK_R}\n"
	check := func(y string) (int, string) {
		t.Helper()
		if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb)
		return code, out.String() + errb.String()
	}
	code, got = check(lacks)
	const (
		noHA = "WARN  agent relay: socket_links is set and home_assistant_system is not: the panel does not know which machine this agent's Home Assistant runs on, " +
			"and switches no socket of the plugs it relays, on or off, until home_assistant_system names that machine's row\n"
		noLinks = "note  agent other: home_assistant_system is set and socket_links is not: every socket of a powerbar this agent relays is linked to nothing " +
			"(the only socket of a plug takes the plug's own link). The panel then asks before each \"off\" whether a machine is plugged in, " +
			"and cannot lock a socket for what it feeds. Name the machines with socket_links\n"
		noPanel = "note  panel_hosts is not set while an agent's entry has socket_links or home_assistant_system: the panel does not know which machine it runs on, " +
			"so it locks no socket as its own. If a socket feeds the panel's machine, name that machine's row in panel_hosts\n"
	)
	for _, want := range []string{noHA, noLinks, noPanel} {
		if strings.Count(got, want) != 1 {
			t.Errorf("the output lacks (or has twice) %q\n%s", want, got)
		}
	}
	if code != 1 || strings.Count(got, "WARN  ") != 1 || strings.Contains(got, "agent plain: ") {
		t.Errorf("one warning, and no line for an agent without the keys (exit %d):\n%s", code, got)
	}
	// with all three said, nothing is left; and a file without the keys has none of the lines
	code, got = check(strings.Replace(lacks, "public_url:", "panel_hosts: [plain]\npublic_url:", 1) +
		"  - {id: full, url: \"http://full:9101\", token_env: TOK_R, socket_links: {bar/ch1: plain}, home_assistant_system: plain}\n")
	if code != 1 || strings.Contains(got, "panel_hosts is not set") || strings.Contains(got, "agent full: socket_links is set") || strings.Contains(got, "agent full: home_assistant_system is set") {
		t.Errorf("an entry with both keys, and panel_hosts (exit %d):\n%s", code, got)
	}
	code, got = check(head + "  - {id: plain, url: \"http://plain:9101\", token_env: TOK_R}\n")
	if code != 0 || strings.Contains(got, "panel_hosts is not set") || strings.Contains(got, "home_assistant_system") {
		t.Errorf("a file without the keys (exit %d):\n%s", code, got)
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
