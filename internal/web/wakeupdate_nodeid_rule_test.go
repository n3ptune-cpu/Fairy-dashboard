package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// The review of fix13 (tip 0dfee9a). Its two proofs are tests here (and in internal/powerctl), with the
// report's own configuration from end to end and the lines of runOps that no test held.

// reportConfig is the shape of the configuration the report came from: three proxmox: entries (a cluster
// without an id prefix, and two standalone hosts with the prefixes yard-a- and yard-b-), the agent
// yard-a-relay-agent on the host that is always on (it sends the wake packets), and the agent
// yard-b-box-agent (node: BOX-B) on the host that is normally off, whose wol: is written under its
// node's id, yard-b-node-box-b. The MAC and the broadcast address are documentation values.
func reportConfig(t *testing.T) *config.Panel {
	t.Helper()
	t.Setenv("TOK_YARD_A", testutil.ReadToken)
	t.Setenv("TOK_YARD_B", testutil.ReadToken+"x")
	t.Setenv("TOK_PC", testutil.ReadToken+"y")
	t.Setenv("CTL_LAB", testutil.ControlToken)
	t.Setenv("PVE_SECRET", "pve-secret-uuid")
	dir := t.TempDir()
	y := `
public_url: https://panel.example.com
request_timeout: 1
offline_after: 60
audit_log: ` + fmt.Sprintf("%q", dir+"/audit.log") + `
data_dir: ` + fmt.Sprintf("%q", dir+"/data") + `
oidc: {issuer: "https://id.example.com", client_id: cid, allowed_groups: [viewers], control_groups: [controllers]}
proxmox:
  - {id: main, id_prefix: "", cluster_name: MAIN, urls: ["https://main:8006"], token_id: "fairy@pve!panel", token_secret_env: PVE_SECRET, verify_tls: false}
  - {id: yard-a, id_prefix: yard-a-, cluster_name: YARD-A, standalone: true, urls: ["https://box-a:8006"],
     token_id: "fairy@pve!panel", token_secret_env: PVE_SECRET, verify_tls: false}
  - {id: yard-b, id_prefix: yard-b-, cluster_name: YARD-B, standalone: true, urls: ["https://box-b:8006"],
     token_id: "fairy@pve!panel", token_secret_env: PVE_SECRET, verify_tls: false}
agents:
  - {id: yard-a-relay-agent, name: BOX-A, url: "http://box-a:9101", token_env: TOK_YARD_A, control_token_env: CTL_LAB, node: BOX-A, wol_relay: true}
  - id: yard-b-box-agent
    name: BOX-B
    url: http://box-b:9101
    token_env: TOK_YARD_B
    control_token_env: CTL_LAB
    node: BOX-B
    power_control: true
    self_update: true
  - {id: yard-pc-agent, name: PC, url: "http://pc:9101", token_env: TOK_PC, control_token_env: CTL_LAB, power_control: true, self_update: true}
systems:
  - id: yard-b-node-box-b
    wol: {mac: "00:00:5e:00:53:36", relay: yard-a-relay-agent, broadcast: 192.0.2.255}
  - id: yard-pc-agent
    wol: {mac: "00:00:5e:00:53:37", relay: yard-a-relay-agent, broadcast: 192.0.2.255}
`
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The report's configuration from end to end: the panel restarts while the box-b host is off. Its row is the
// agent's; the Power menu and the Updates page have its wake; a run wakes it with its own MAC through its own
// relay, follows it to its node's row, updates it and switches it off again. With a guest running it is left on.
func TestTheReportedConfigurationAfterAPanelRestartWithTheNodeOff(t *testing.T) {
	const agent, node = "yard-b-box-agent", "yard-b-node-box-b"
	for _, guest := range []string{"stopped", "running"} {
		t.Run("its guest is "+guest, func(t *testing.T) {
			h := setupNodeOff(t, reportConfig(t), &twoHosts{agent: agent, node: node, cluster: "yard-b", name: "BOX-B",
				others: map[string]string{"main": "pve1", "yard-a": "BOX-A"}}, "yard-a-relay-agent")
			e := h.e
			if w := e.s.Poller.NodeLinkWarnings(); len(w) != 0 {
				t.Errorf("warnings at the start: %q", w)
			}
			if id, cluster, ok := e.s.Poller.NodeOf(agent); !ok || id != node || cluster != "yard-b" {
				t.Fatalf("the node of %s by the configuration: %q %q %v", agent, id, cluster, ok)
			}
			if id, _, ok := e.s.Poller.NodeOf("yard-a-relay-agent"); !ok || id != "yard-a-node-box-a" {
				t.Errorf("the node of the relay's agent: %q %v", id, ok)
			}
			admin := e.cookie(controller(), 0)
			if got := h.shown(); got != agent {
				t.Fatalf("after the restart the host is shown as %s", got)
			}
			var wake map[string]any
			for _, a := range e.do("GET", "/api/systems/"+agent+"/power", nil, "Cookie", admin).body["actions"].([]any) {
				if m := a.(map[string]any); m["action"] == "wake" {
					wake = m
				}
			}
			if wake["available"] != true || wake["via"] != "wol:yard-a-relay-agent" {
				t.Fatalf("the Power menu: %v", wake)
			}
			h.guest = guest
			h.wake()
			if row := wuHostRow(t, e.do("GET", "/api/admin/wake-update", nil, "Cookie", admin), agent); row["can_wake"] != true || row["relay"] != "BOX-A" || row["opted_in"] != true {
				t.Errorf("the Updates page: %v", row)
			}
			host, sent := h.end()
			if got := h.shown(); got != node {
				t.Fatalf("after the run the host is shown as %s", got)
			}
			e.b.Mu.Lock()
			all := strings.Join(e.b.Requests, "\n")
			e.b.Mu.Unlock()
			if !strings.Contains(all, "yard-a-relay-agent POST /wol map[broadcast:192.0.2.255 mac:00:00:5e:00:53:36 port:9]") || strings.Contains(all, "00:00:5e:00:53:37") {
				t.Errorf("the wake packets:\n%s", all)
			}
			want, state := "yard-a-relay-agent /wol, "+agent+" /update, "+agent+" /power", wakerun.StOff
			if guest == "running" {
				want, state = "yard-a-relay-agent /wol, "+agent+" /update", wakerun.StLeftOn
			}
			if got := strings.Join(sent, ", "); got != want || host.State != state {
				t.Errorf("the run sent %q and the machine ended as %s (%s): %s", got, host.State, host.On, host.Text)
			}
			if guest == "running" && !strings.Contains(host.Text, "1 guest is running on it (win11)") {
				t.Errorf("left on: %s", host.Text)
			}
		})
	}
}

// Review of fix13, must-fix 1 (the reviewer's proof, as a test): two standalone hosts whose node is called
// pve; the agent is on host 1, the wol: is host 2's. On fix13's first rule the Updates page offered host 1 as
// a machine that can be woken, and a run "for host 1" sent host 2's MAC and woke host 2. Now host 1 has no
// Wake-on-LAN, a start with it is refused, and a run that is started all the same sends no packet.
func TestARunForAHostNeverWakesAnotherHostWithTheSameNodeName(t *testing.T) {
	h := setupNodeOff(t, testutil.SameNameConfig(t), &twoHosts{agent: "host1-agent", node: "site1-node-pve", cluster: "site1", name: "pve"}, "relay")
	e := h.e
	admin := e.cookie(controller(), 0)
	h.f.offline()
	h.f.optIn("host1-agent")
	row := wuHostRow(t, e.do("GET", "/api/admin/wake-update", nil, "Cookie", admin), "host1-agent")
	if row["can_wake"] != false || row["no_wake"] != "no Wake-on-LAN is set up for it" || row["relay"] != nil || row["choice"] != "" ||
		row["why"] != "it is off, and Fairy cannot wake it: no Wake-on-LAN is set up for it" {
		t.Fatalf("host 1 on the Updates page: %v", row)
	}
	if _, err := e.s.wuCheck(wuStartBody{Hosts: []string{"host1-agent"}}); err == nil || !strings.Contains(err.Error(), "cannot be woken and switched off again") {
		t.Fatalf("a start with host 1 as a machine to wake: %v", err)
	}
	// the run itself, started past the page's check
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"host1-agent"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	host := hostOf(h.f.finish(), "host1-agent")
	if host.State != wakerun.StGaveUp || host.WakeSentAt != nil || !strings.Contains(host.Text, "The wake could not be sent") {
		t.Errorf("host 1 ended as %s (%s): %s", host.State, host.On, host.Text)
	}
	e.b.Mu.Lock()
	defer e.b.Mu.Unlock()
	for _, q := range e.b.Requests {
		if strings.Contains(q, "/wol") || strings.Contains(q, "00:00:5e:00:53:72") {
			t.Errorf("WRONG MACHINE: the run for host 1 sent %s", q)
		}
	}
}
