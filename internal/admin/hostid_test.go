package admin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/health"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
)

// Audit 6, r4-p2-05: `fairy admin host <id> power-control disable` took any word as the id and answered "Power
// control for <word>: disabled." The switch is looked up by the system's id: with the id of a Proxmox node's
// agent, or with the node's name, the node's shutdown stayed on offer. The command now refuses an id that the
// panel does not have, says which id to use when it knows, and exits with 1; nothing is written.
func TestHostPowerControlRefusesAnIDThePanelDoesNotHave(t *testing.T) {
	run, read, auditLog := shapeCLI(t, `{}`)
	dir := filepath.Dir(read().StatePath)
	// the running panel's systems: a Proxmox node, its guest, an ordinary machine. The node has an agent of its
	// own, added in the panel under the id "pve-agent".
	if err := health.Write(dir, health.Report{SystemIDs: []string{"node-srv-pvea", "qemu-100", "pc-a"}}); err != nil {
		t.Fatal(err)
	}
	if err := managed.NewStore(dir).Put(managed.Host{ID: "pve-agent", Name: "PVE agent", URL: "http://pve-agent:9101", OS: "linux", Node: "SRV-PVEA", SystemID: "node-srv-pvea"},
		managed.Secret{Token: strings.Repeat("r", 32), ControlToken: strings.Repeat("c", 32)}); err != nil {
		t.Fatal(err)
	}
	entries := func() int {
		b, _ := os.ReadFile(auditLog)
		return strings.Count(string(b), "host.power_control")
	}
	for _, c := range []struct{ id, want string }{
		{"pve-agent", "pve-agent is the id of an agent that reports for the system node-srv-pvea: power control is switched by the system's id. Nothing was changed. " +
			"Use: fairy admin host node-srv-pvea power-control ..."},
		{"srv-pvea", "the panel has no system with the id srv-pvea, so nothing was changed."},
		{"SRV-PVEA", "the panel has no system with the id SRV-PVEA, so nothing was changed."},
		{"node-srv-pveb", "the panel has no system with the id node-srv-pveb, so nothing was changed."},
	} {
		for _, word := range []string{"disable", "enable"} {
			code, out := run("host", c.id, "power-control", word)
			if code != 1 || !strings.Contains(out, c.want) || strings.Contains(out, "Power control for") {
				t.Errorf("host %s power-control %s: exit %d, want 1 and %q:\n%s", c.id, word, code, c.want, out)
			}
		}
		if l, _ := read().PowerDisabledList(); len(l) != 0 || read().PowerDisabled("node-srv-pvea") || entries() != 0 {
			t.Errorf("after the refused %s: the list is %v, audit entries: %d", c.id, l, entries())
		}
	}
	// the system's own id is taken, and a guest's and an ordinary machine's
	for _, id := range []string{"node-srv-pvea", "qemu-100", "pc-a"} {
		if code, out := run("host", id, "power-control", "disable"); code != 0 || out != "Power control for "+id+": disabled.\n" || !read().PowerDisabled(id) {
			t.Errorf("host %s power-control disable: exit %d, switched off: %v:\n%s", id, code, read().PowerDisabled(id), out)
		}
	}
	if entries() != 3 {
		t.Errorf("%d audit entries, want 3", entries())
	}
	// a system that is gone from the panel can still be taken out of the list
	if err := health.Write(dir, health.Report{SystemIDs: []string{"node-srv-pvea", "pc-a"}}); err != nil {
		t.Fatal(err)
	}
	if code, out := run("host", "qemu-100", "power-control", "enable"); code != 0 || out != "Power control for qemu-100: enabled.\n" || read().PowerDisabled("qemu-100") {
		t.Errorf("enabling an id that is in the list and no longer in the panel: exit %d:\n%s", code, out)
	}
	if code, out := run("host", "qemu-100", "power-control", "disable"); code != 1 || !strings.Contains(out, "the panel has no system with the id qemu-100") {
		t.Errorf("disabling it again: exit %d:\n%s", code, out)
	}
}

// The panel has never run (no health.json): the command cannot check the id against the panel's systems. It
// takes it and says so; what the configuration shows to be another system's agent is refused all the same.
func TestHostPowerControlBeforeThePanelEverRan(t *testing.T) {
	run, read, _ := shapeCLI(t, `{}`)
	dir := filepath.Dir(read().StatePath)
	if err := managed.NewStore(dir).Put(managed.Host{ID: "pve-agent", Name: "PVE agent", URL: "http://pve-agent:9101", OS: "linux", Node: "SRV-PVEA", SystemID: "node-srv-pvea"},
		managed.Secret{Token: strings.Repeat("r", 32), ControlToken: strings.Repeat("c", 32)}); err != nil {
		t.Fatal(err)
	}
	code, out := run("host", "pc-a", "power-control", "disable")
	if code != 0 || !strings.HasPrefix(out, "Power control for pc-a: disabled.\n") || !strings.Contains(out, "Fairy could not check that pc-a is one of them") || !read().PowerDisabled("pc-a") {
		t.Errorf("exit %d, switched off: %v:\n%s", code, read().PowerDisabled("pc-a"), out)
	}
	if code, out := run("host", "pve-agent", "power-control", "disable"); code != 1 || !strings.Contains(out, "Use: fairy admin host node-srv-pvea power-control") || read().PowerDisabled("pve-agent") {
		t.Errorf("the agent's id: exit %d:\n%s", code, out)
	}
}
