package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Remove of a host that "Edit host" made the sender of other machines' wake packets (R6, N3). The decision
// of such a machine is "these MACs, through that relay, on this port": without its relay it is no longer what
// the admin confirmed, so it goes whole, and config.yaml's Wake-on-LAN counts for the machine again. It used
// to lose only the relay: "those MACs through config.yaml's relay", a wake nobody ever confirmed. The Remove
// dialog says so before, the answer after, and the audit log and the panel's log name the machines.
func TestEditHostRemoveOfARelayTakesTheWholeWakeChangeOfOthers(t *testing.T) {
	logs := captureLog(t)
	cfg := hedConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// relay-m was added with Add host, and may send wake packets
	if err := managed.NewStore(cfg.DataDir).Put(managed.Host{ID: "relay-m", Name: "Relay M", URL: "http://relay-m", OS: "linux", AddedAt: time.Now().UTC(),
		Options: managed.Options{WolRelay: true}}, managed.Secret{Token: strings.Repeat("m", 32), ControlToken: strings.Repeat("c", 32)}); err != nil {
		t.Fatal(err)
	}
	b := hedBackend()
	b.Metrics["relay-m"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["hostname"] = "relay-m"
		delete(m, "power_sources")
		return m, nil
	}
	c := *cfg
	c.Agents, c.Systems, c.Proxmox = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems), slices.Clone(cfg.Proxmox)
	if _, skipped, err := managed.NewStore(cfg.DataDir).Merge(&c); err != nil || len(skipped) != 0 {
		t.Fatal(err, skipped)
	}
	r := hedStart(t, &c, b)
	managedRow := func() map[string]any {
		t.Helper()
		return hostRow(t, r.env, r.hdr[1], "relay-m")
	}
	if row := managedRow(); row == nil || row["wake_relay_of"] != nil {
		t.Fatalf("the rig: relay-m is an added host that nothing names yet: %v", row)
	}
	// pc-a: other MACs, through relay-m, port 7. pc-b gets a Wake-on-LAN through it. box-c stays config.yaml's.
	r.ok("pc-a", map[string]any{"name": "Gaming PC"})
	r.wol("pc-a", map[string]any{"macs": []string{"02:00:00:00:00:ee"}, "relay": "relay-m", "port": 7})
	r.wol("pc-b", map[string]any{"macs": []string{"02:00:00:00:00:b1"}, "relay": "relay-m"})
	r.wol("box-d", map[string]any{"port": 7}) // a decision that names no relay: not relay-m's
	if got := strs(managedRow()["wake_relay_of"]); !slices.Equal(got, []string{"Gaming PC", "PC-B"}) {
		t.Fatalf("what the Remove dialog is told: %v", got)
	}
	logs.take()

	p := r.do("DELETE", "/api/admin/hosts/relay-m", map[string]any{"confirm": "Relay M"}, append([]string{"Content-Type", "application/json"}, r.hdr...)...)
	want := "Relay M removed from the panel. Relay M was chosen in Edit host as the sender of the wake packets of Gaming PC, PC-B: " +
		"that change was taken out with it, and what config.yaml says about Wake-on-LAN counts there again"
	if p.code != 200 || p.body["message"] != want {
		t.Fatalf("Remove: %d %s", p.code, p.raw)
	}
	// whole: nothing of pc-a's decision is left (its MAC and its port went with the relay), its name stays
	if st := r.storedHost("pc-a"); st["wol"] != nil || st["name"] != "Gaming PC" {
		t.Errorf("pc-a: %v", st)
	}
	if st := r.storedHost("pc-b"); st != nil {
		t.Errorf("pc-b: %v", st)
	}
	if st, _ := r.storedHost("box-d")["wol"].(map[string]any); st["port"] != 7.0 {
		t.Errorf("box-d, whose decision names no relay: %v", r.storedHost("box-d"))
	}
	if w := r.host("pc-a", "wol"); !slices.Equal(strs(w["macs"]), []string{"02:00:00:00:00:a1"}) || w["relay"] != "relay-a" || w["port"] != 9.0 || w["from"] != "config" || w["unused"] != nil {
		t.Errorf("pc-a's Wake-on-LAN: %v", w)
	}
	if w := r.host("pc-b", "wol"); w["on"] != false || w["from"] != "config" || slices.Contains(strs(r.row("pc-b")["power_actions"]), "wake") {
		t.Errorf("pc-b's Wake-on-LAN: %v", w)
	}
	// a wake of pc-a is config.yaml's again, never the changed MAC through the relay of config.yaml
	r.off("pc-a")
	if w := r.wake("pc-a"); w.code != 200 {
		t.Fatalf("wake: %d %s", w.code, w.raw)
	}
	if got := strings.ToLower(strings.Join(r.wakes(), "\n")); !strings.Contains(got, "relay-a post /wol") || !strings.Contains(got, "02:00:00:00:00:a1") || strings.Contains(got, "02:00:00:00:00:ee") {
		t.Errorf("the wake that was sent: %s", got)
	}
	// the audit log: the lines in the host.options entry of the Remove, the names in the host.remove entry
	var lines []string
	for _, e := range r.auditEntries("", "host.options") {
		if pr, _ := e["params"].(map[string]any); e["target"] == "relay-m" && pr["because"] == "host removed" {
			lines = strs(pr["removed"])
		}
	}
	for _, id := range []string{"pc-a", "pc-b"} {
		if l := id + ": the Wake-on-LAN details changed for it (they named relay-m as the relay): config.yaml's count for it again"; !slices.Contains(lines, l) {
			t.Errorf("audit, host.options: %q lacks %q", lines, l)
		}
	}
	rm := r.auditEntries("", "host.remove")
	if pr, _ := rm[0]["params"].(map[string]any); len(rm) == 0 || rm[0]["outcome"] != "ok" || !slices.Equal(strs(pr["wake_changes_removed"]), []string{"Gaming PC", "PC-B"}) {
		t.Errorf("audit, host.remove: %v", rm)
	}
	// the panel's log: one WARN for each machine
	out := logs.take()
	for _, id := range []string{"pc-a", "pc-b"} {
		if n := strings.Count(out, "level=WARN msg=\"Edit host: a host's Wake-on-LAN details changed in the panel are taken out, because their relay is removed: config.yaml's count for it again\" host="+id+" relay=relay-m"); n != 1 {
			t.Errorf("%d WARN lines for %s:\n%s", n, id, out)
		}
	}
	if strings.Contains(out, "host=box-d") {
		t.Errorf("a WARN about box-d:\n%s", out)
	}
	// a Remove of a host nothing names says nothing of it
	if err := managed.NewStore(cfg.DataDir).Put(managed.Host{ID: "box-m", Name: "Box M", URL: "http://box-m", OS: "linux", AddedAt: time.Now().UTC()},
		managed.Secret{Token: strings.Repeat("n", 32)}); err != nil {
		t.Fatal(err)
	}
	if p := r.do("DELETE", "/api/admin/hosts/box-m", map[string]any{"confirm": "Box M"}, append([]string{"Content-Type", "application/json"}, r.hdr...)...); p.code != 200 || p.body["message"] != "Box M removed from the panel" {
		t.Errorf("Remove of a host that is nobody's relay: %d %s", p.code, p.raw)
	}
}

// The Remove dialog's sentence about it: only for a host that is such a relay, with the machines by name,
// each name through esc().
func TestUIRemoveDialogSaysWhoseWakeChangeGoes(t *testing.T) {
	js := appJS(t)
	lines := regexp.MustCompile(`(?m)^    const wakeOf = .*\n    const wakeNote = .*$`).FindString(js)
	esc := regexp.MustCompile(`(?m)^const esc = .*$`).FindString(js)
	if lines == "" || esc == "" {
		t.Fatal("app.js has no sentence about the wake packets in the Remove dialog any more")
	}
	if !strings.Contains(js, "The agent keeps running on the machine until you uninstall it there.${wakeNote}`, h.name, \"Remove\"") {
		t.Error("the Remove dialog does not say it any more")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own functions are not run here")
	}
	file := filepath.Join(t.TempDir(), "remove-note.js")
	body := `"use strict";
` + esc + `
const note = (h) => {
` + lines + `
  return wakeNote;
};
const want = " It was chosen in Edit host as the sender of the wake packets of <b>Gaming PC</b>, <b>&lt;b&gt;x</b>: that change is taken out with it, and what config.yaml says about Wake-on-LAN counts there again.";
const got = note({ name: "Relay M", wake_relay_of: ["Gaming PC", "<b>x"] });
if (got !== want) { console.log("FAIL " + got); process.exitCode = 1; }
if (note({ name: "Box M" }) !== "" || note({ name: "Box M", wake_relay_of: [] }) !== "") { console.log("FAIL a sentence for a host that is nobody's relay"); process.exitCode = 1; }
if (!process.exitCode) console.log("ok");
`
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, file).CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
