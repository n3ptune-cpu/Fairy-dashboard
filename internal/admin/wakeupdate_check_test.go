package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Build 14: `fairy admin config check` lists the timings of "Update unpowered hosts" that are not what a
// reader expects: the panel's off_after_s when it is not the default, and every machine with timings of its
// own. A time below the default after which a machine counts as off is allowed, and is said with what it
// risks, as a note: no WARN, and the exit code stays 0.
func TestConfigCheckListsTheRunTimingsThatAreNotTheDefault(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	t.Setenv("TOK_R", "r")
	t.Setenv("CTL_R", "c")
	t.Setenv("FAIRY_SESSION_SECRET", strings.Repeat("no-secret-", 5)) // made here, for this test only
	check := func(more string) (string, int) {
		t.Helper()
		y := "public_url: https://p.example\ndata_dir: " + dir + "/data\noidc: {issuer: \"https://id.example\", client_id: c, allowed_groups: [users], control_groups: [admins]}\n" +
			"agents:\n  - {id: relay, url: \"http://relay:9101\", token_env: TOK_R, control_token_env: CTL_R, wol_relay: true}\n" +
			"  - {id: pc-a, url: \"http://pc-a:9101\", token_env: TOK_R}\n  - {id: pc-b, url: \"http://pc-b:9101\", token_env: TOK_R}\n" +
			"  - {id: pc-c, url: \"http://pc-c:9101\", token_env: TOK_R}\n  - {id: pc-d, url: \"http://pc-d:9101\", token_env: TOK_R}\n" + more
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
	// nothing set: nothing said
	out, code := check("")
	if strings.Contains(out, "off_after_s") || strings.Contains(out, "wake_update") || code != 0 {
		t.Fatalf("the defaults (exit %d):\n%s", code, out)
	}
	if out, _ := check("wake_update: {off_after_s: 900, shutdown_delay_s: 60}\n"); strings.Contains(out, "off_after_s") {
		t.Fatalf("off_after_s written with its default:\n%s", out)
	}
	// the panel's own, below the default; one machine's own, below it too; one machine with a countdown only;
	// one with a longer time (no risk to name)
	out, code = check("wake_update: {off_after_s: 360}\nsystems:\n" +
		"  - {id: pc-a, wol: {mac: \"00:00:5e:00:53:01\", relay: relay}, wake_update: {off_after_s: 300, shutdown_delay_s: 30}}\n" +
		"  - {id: pc-b, wake_update: {shutdown_delay_s: 45}}\n" +
		"  - {id: pc-c, wake_update: {off_after_s: 1830}}\n" +
		"  - {id: pc-d, wol: {mac: \"00:00:5e:00:53:04\", relay: relay}}\n")
	for _, want := range []string{
		"ok    wake_update.off_after_s: 360 (a machine counts as off after 6 min without an answer; without the line it is 900)\n",
		"ok    system pc-a: wake_update of its own: off_after_s 300 (it counts as off after 5 min without an answer), shutdown_delay_s 30 (the countdown of its shutdown in a run)\n",
		"ok    system pc-b: wake_update of its own: shutdown_delay_s 45 (the countdown of its shutdown in a run)\n",
		"ok    system pc-c: wake_update of its own: off_after_s 1830 (it counts as off after 1830 s without an answer)\n",
		"note  wake_update.off_after_s is 360 (the default is 900): a machine that restarts for longer than 6 min can be taken for off by a run, which then wakes it, updates it and switches it off as a machine it woke\n",
		"note  system pc-a: wake_update.off_after_s is 300 (the default is 900): a machine that restarts for longer than 5 min can be taken for off by a run, which then wakes it, updates it and switches it off as a machine it woke\n",
	} {
		if strings.Count(out, want) != 1 {
			t.Errorf("the output lacks (or has twice) %q", want)
		}
	}
	if n := strings.Count(out, "can be taken for off by a run"); n != 2 {
		t.Errorf("%d notes about a short off_after_s, want 2 (not for pc-b, pc-c, pc-d)", n)
	}
	if strings.Contains(out, "system pc-d") || strings.Contains(out, "WARN") || code != 0 {
		t.Errorf("exit code %d, or a WARN, or a line for a machine without the block:\n%s", code, out)
	}
	// a value outside its range is no valid file at all
	bad := "public_url: https://p.example\ndata_dir: " + dir + "/data\noidc: {issuer: \"https://id.example\", client_id: c}\nwake_update: {off_after_s: 120}\n"
	if err := os.WriteFile(cfg, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	var o, eb bytes.Buffer
	if code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &o, &eb); code == 0 || !strings.Contains(o.String()+eb.String(), "wake_update.off_after_s must be 300-3600") {
		t.Errorf("off_after_s: 120: exit %d\n%s%s", code, o.String(), eb.String())
	}
}

// A systems: entry whose id is no machine's (a typing mistake in the id) is read and counts for nothing: the
// machine it was written for has the panel's own timings, and nothing said so. One note per such entry; an
// entry under an agent's id, or under an id of the form a node or guest of a proxmox: entry has, has none.
func TestConfigCheckNotesASystemsEntryThatIsNoMachines(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	t.Setenv("TOK_R", "r")
	t.Setenv("CTL_R", "c")
	t.Setenv("PVE_S", "s")
	t.Setenv("FAIRY_SESSION_SECRET", strings.Repeat("no-secret-", 5)) // made here, for this test only
	y := "public_url: https://p.example\ndata_dir: " + dir + "/data\noidc: {issuer: \"https://id.example\", client_id: c, allowed_groups: [users], control_groups: [admins]}\n" +
		"proxmox:\n  - {id: lab, id_prefix: lab-, urls: [\"https://pve:8006\"], token_id: \"fairy@pve!panel\", token_secret_env: PVE_S}\n" +
		"agents:\n  - {id: relay, url: \"http://relay:9101\", token_env: TOK_R, control_token_env: CTL_R, wol_relay: true}\n" +
		"  - {id: lab-agent, url: \"http://lab:9101\", token_env: TOK_R, node: pve-b}\n" +
		"systems:\n" +
		"  - {id: lab-node-pve-b, wake_update: {off_after_s: 3600}}\n" + // the node of lab-agent
		"  - {id: lab-node-pveb, wake_update: {off_after_s: 3600}}\n" + // of a node's form: it may be a node the panel has not seen
		"  - {id: lab-nod-pve-b, wake_update: {off_after_s: 3600}}\n" + // a typing mistake
		"  - {id: relay, name: Relay}\n" + // an agent
		"  - {id: lab-qemu-100, name: Win}\n" + // a guest
		"  - {id: old-pc, name: Old}\n" // nobody
	if err := os.WriteFile(cfg, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "config", "check"}, &out, &errb)
	if !strings.Contains(out.String(), "config "+cfg+": valid") {
		t.Fatalf("the fixture is not a valid config:\n%s%s", out.String(), errb.String())
	}
	for _, want := range []string{
		"note  system lab-nod-pve-b: no agent has this id, and it is not the id of a node or a guest of a proxmox: entry (<id_prefix>node-<name>, <id_prefix>qemu-<vmid>, <id_prefix>lxc-<vmid>): " +
			"the entry and its wake_update: block count for no machine (the machine it was written for has the panel's own timings). A typing mistake in the id?\n",
		"note  system old-pc: no agent has this id, and it is not the id of a node or a guest of a proxmox: entry (<id_prefix>node-<name>, <id_prefix>qemu-<vmid>, <id_prefix>lxc-<vmid>): " +
			"the entry counts for no machine. A typing mistake in the id?\n",
	} {
		if strings.Count(out.String(), want) != 1 {
			t.Errorf("the output lacks (or has twice) %q", want)
		}
	}
	if n := strings.Count(out.String(), "no agent has this id"); n != 2 {
		t.Errorf("%d notes, want 2:\n%s", n, out.String())
	}
	if code != 0 || strings.Contains(out.String(), "WARN") {
		t.Errorf("a note is no warning (exit %d):\n%s", code, out.String())
	}
}
