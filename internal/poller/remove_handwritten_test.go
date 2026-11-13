package poller

import (
	"context"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Second review of build 14, R4. A host that was added with "Add host" WITHOUT Wake-on-LAN and is the agent of
// a node of config.yaml, whose wol: and timings are hand-written under the node's id. Remove took "the systems:
// entry under the id the host is shown as" out of the running panel: the node's own hand-written entry. Remove
// is also the first half of every Re-pair, so after a Re-pair of the host the node had no Wake-on-LAN (no
// Power on, "no Wake-on-LAN is set up for it" in a run) until the panel was restarted.
//
// Remove takes out the entries "Add host" made for that host, and no other.
func TestRemoveAndRePairLeaveAHandWrittenEntryAsItIs(t *testing.T) {
	nodes := map[string]string{"lab": "pve"}
	p, _ := linkPanel(t, [][2]string{{"lab", "lab-"}},
		"  - {id: relay, name: R, url: \"http://relay\", token_env: TOK_A, control_token_env: CTL_LAB, wol_relay: true}\n",
		"  - {id: lab-node-pve, name: Lab host, wol: {mac: \"00:00:5e:00:53:01\", relay: relay}, wake_update: {off_after_s: 600}}\n", nodes)
	a := config.Agent{ID: "pve-host", Name: "PVE", URL: "http://pve-host:9101", Node: "pve", Managed: true, Token: testutil.ReadToken}
	whole := func(when string) {
		t.Helper()
		o := p.Override("lab-node-pve")
		if o == nil || o.Wol == nil || o.Wol.MAC != "00:00:5e:00:53:01" || o.Name != "Lab host" || o.WakeUpdate == nil || o.WakeUpdate.OffAfterS != 600 || o.ManagedBy != "" {
			t.Fatalf("%s: the node's hand-written entry in the running panel: %+v", when, o)
		}
		n := 0
		for _, x := range p.SystemsConfig() {
			if x.ID == "lab-node-pve" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%s: %d systems: entries for the node, want the one of config.yaml", when, n)
		}
	}
	if err := p.AddAgent(a, nil); err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	if l := p.LinkOf(a); l != "lab-node-pve" {
		t.Fatalf("the fixture: the host is the agent of the node (%q)", l)
	}
	whole("after Add host")
	p.RemoveAgent("pve-host") // a Re-pair: Remove, then Add
	whole("after Remove")
	if err := p.AddAgent(a, nil); err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	whole("after the Re-pair")
	if o := p.PowerOverride("pve-host"); o == nil || o.Wol == nil {
		t.Errorf("after the Re-pair the host has no Wake-on-LAN: %+v", o)
	}
	p.RemoveAgent("pve-host")
	whole("after the last Remove")
}

// What "Add host" made for a host still goes with it: the entry with its Wake-on-LAN, under its own id or
// under the id of its node, and with it nothing of another host. The machine's own timings from config.yaml,
// which stand in that entry (AddAgent), stay.
func TestRemoveTakesOutTheEntryAddHostMade(t *testing.T) {
	nodes := map[string]string{"lab": "pve"}
	p, _ := linkPanel(t, [][2]string{{"lab", "lab-"}},
		"  - {id: relay, name: R, url: \"http://relay\", token_env: TOK_A, control_token_env: CTL_LAB, wol_relay: true}\n",
		"  - {id: desk-pc, wake_update: {off_after_s: 900}}\n  - {id: other-pc, wol: {mac: \"00:00:5e:00:53:09\", relay: relay}}\n", nodes)
	wol := func(id, mac string) *config.SystemOverride {
		return &config.SystemOverride{ID: id, Wol: &config.Wol{MAC: mac, Relay: "relay"}}
	}
	desk := config.Agent{ID: "desk-pc", Name: "Desk", URL: "http://desk:9101", Managed: true, Token: testutil.ReadToken}
	host := config.Agent{ID: "pve-host", Name: "PVE", URL: "http://pve-host:9101", Node: "pve", Managed: true, Token: testutil.ReadToken}
	if err := p.AddAgent(desk, wol("desk-pc", "00:00:5e:00:53:02")); err != nil {
		t.Fatal(err)
	}
	if err := p.AddAgent(host, wol("lab-node-pve", "00:00:5e:00:53:03")); err != nil { // Add host writes it under the node's id (managed.Host.Shown)
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	for id, by := range map[string]string{"desk-pc": "desk-pc", "lab-node-pve": "pve-host", "other-pc": ""} {
		if o := p.Override(id); o == nil || o.Wol == nil || o.ManagedBy != by {
			t.Fatalf("the fixture: the entry of %s: %+v, want a wol: made by %q", id, o, by)
		}
	}
	p.RemoveAgent("pve-host")
	if o := p.Override("lab-node-pve"); o != nil {
		t.Errorf("the entry Add host made for pve-host is still there after its Remove: %+v", o)
	}
	if o := p.Override("desk-pc"); o == nil || o.Wol == nil || o.WakeUpdate == nil {
		t.Errorf("the Remove of pve-host changed the entry of desk-pc: %+v", o)
	}
	p.RemoveAgent("desk-pc")
	if o := p.Override("desk-pc"); o == nil || o.Wol != nil || o.WakeUpdate == nil || o.WakeUpdate.OffAfterS != 900 || o.ManagedBy != "" {
		t.Errorf("after the Remove of desk-pc: %+v, want config.yaml's own timings and nothing else", o)
	}
	if o := p.Override("other-pc"); o == nil || o.Wol == nil {
		t.Errorf("a hand-written entry of another machine is gone: %+v", o)
	}
	if n := len(p.SystemsConfig()); n != 2 {
		t.Errorf("%d systems: entries are left, want config.yaml's two: %+v", n, p.SystemsConfig())
	}
}

// The same for a host that was added before the panel started: its entry comes from managed-hosts.yaml
// (managed.Store.Merge), marked as that host's, and its Remove takes out that entry and leaves the
// hand-written one of the node the host runs on.
func TestRemoveOfAHostFromBeforeTheStartTakesOutItsOwnEntryOnly(t *testing.T) {
	t.Setenv("TOK_A", testutil.ReadToken)
	t.Setenv("CTL_LAB", testutil.ControlToken)
	t.Setenv("PVE_SECRET", "pve-secret-uuid")
	cfg, err := config.Parse([]byte(`
public_url: https://panel.example.com
data_dir: "` + t.TempDir() + `"
oidc: {issuer: "https://id.example.com", client_id: cid, allowed_groups: [viewers], control_groups: [controllers]}
proxmox:
  - {id: lab, id_prefix: lab-, cluster_name: C, standalone: true, urls: ["https://pve0:8006"], token_id: "fairy@pve!panel", token_secret_env: PVE_SECRET, verify_tls: false}
agents:
  - {id: relay, name: R, url: "http://relay", token_env: TOK_A, control_token_env: CTL_LAB, wol_relay: true}
systems:
  - {id: lab-node-pve, wol: {mac: "00:00:5e:00:53:01", relay: relay}}
`))
	if err != nil {
		t.Fatal(err)
	}
	// what Merge adds for two hosts of managed-hosts.yaml: one with Wake-on-LAN of its own, one on the node
	cfg.Agents = append(cfg.Agents,
		config.Agent{ID: "desk-pc", Name: "Desk", URL: "http://desk:9101", Managed: true, Token: testutil.ReadToken},
		config.Agent{ID: "pve-host", Name: "PVE", URL: "http://pve-host:9101", Node: "pve", Managed: true, Token: testutil.ReadToken})
	cfg.Systems = append(cfg.Systems, config.SystemOverride{ID: "desk-pc", ManagedBy: "desk-pc", Wol: &config.Wol{MAC: "00:00:5e:00:53:02", Relay: "relay"}})
	p, err := New(cfg, config.Secrets{}, testutil.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	p.RemoveAgent("pve-host")
	if o := p.Override("lab-node-pve"); o == nil || o.Wol == nil {
		t.Errorf("the Remove of the node's agent took the node's hand-written entry: %+v", o)
	}
	if o := p.Override("desk-pc"); o == nil || o.Wol == nil {
		t.Errorf("the Remove of another host took the entry of desk-pc: %+v", o)
	}
	p.RemoveAgent("desk-pc")
	if o := p.Override("desk-pc"); o != nil {
		t.Errorf("the entry Add host made for desk-pc is still there after its Remove: %+v", o)
	}
	if n := len(p.SystemsConfig()); n != 1 {
		t.Errorf("%d systems: entries are left, want config.yaml's one: %+v", n, p.SystemsConfig())
	}
}
