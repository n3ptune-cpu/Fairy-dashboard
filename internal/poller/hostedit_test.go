package poller

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// "Edit host", the machine itself: where a name and Wake-on-LAN details changed in the panel meet the
// configuration (hostedit.go).

// plainHosts is a panel of plain agents: relay-a and relay-b may send wake packets; pc-a has a wol: through
// relay-a; pc-b has none; box-c has one with a target address and box-d one with a password.
func plainHosts(t *testing.T) (*Poller, *testutil.Backend) {
	t.Helper()
	for _, v := range []string{"TOK_RA", "TOK_RB", "TOK_PA", "TOK_PB", "TOK_BC", "TOK_BD"} {
		t.Setenv(v, testutil.ReadToken+strings.ToLower(v[len(v)-2:]))
	}
	t.Setenv("CTL_ALL", testutil.ControlToken)
	dir := t.TempDir()
	y := `
public_url: https://panel.example.com
request_timeout: 1
offline_after: 60
audit_log: ` + fmt.Sprintf("%q", dir+"/audit.log") + `
data_dir: ` + fmt.Sprintf("%q", dir+"/data") + `
oidc: {issuer: "https://id.example.com", client_id: cid, allowed_groups: [viewers], control_groups: [controllers]}
agents:
  - {id: relay-a, name: Relay A, url: "http://relay-a", token_env: TOK_RA, control_token_env: CTL_ALL, wol_relay: true}
  - {id: relay-b, name: Relay B, url: "http://relay-b", token_env: TOK_RB, control_token_env: CTL_ALL, wol_relay: true}
  - {id: pc-a, name: PC-A, url: "http://pc-a", token_env: TOK_PA}
  - {id: pc-b, url: "http://pc-b", token_env: TOK_PB}
  - {id: box-c, name: Box C, url: "http://box-c", token_env: TOK_BC}
  - {id: box-d, name: Box D, url: "http://box-d", token_env: TOK_BD}
systems:
  - {id: pc-a, wol: {mac: "02:00:00:00:00:a1", macs: ["02:00:00:00:00:a2"], relay: relay-a}}
  - {id: box-c, wol: {mac: "02:00:00:00:00:c1", relay: relay-a, broadcast: 10.20.30.255}}
  - {id: box-d, wol: {mac: "02:00:00:00:00:d1", relay: relay-a, password_env: WOL_PASSWORD_D}}
`
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	b := testutil.NewBackend()
	for _, id := range []string{"relay-a", "relay-b", "pc-a", "pc-b", "box-c", "box-d"} {
		b.Metrics[id] = func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			m["hostname"] = "host-of-" + id
			return m, nil
		}
	}
	p, err := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	return p, b
}

func setHost(t *testing.T, p *Poller, key string, e adminstate.HostEdit) {
	t.Helper()
	if _, _, err := p.Admin.EditHostOptions("admin", func(o *adminstate.HostOptions) error { o.SetHost(key, e); return nil }); err != nil {
		t.Fatal(err)
	}
	p.Bump()
}

func rowOf(t *testing.T, p *Poller, id string) model.System {
	t.Helper()
	for _, s := range p.Snapshot() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no row %s", id)
	return model.System{}
}

// A name given in the panel is on the row that is shown, at once, and on the rows the other pages are handed.
// The poller's own entry keeps the configuration's name, and Reset shows that again.
func TestPanelNameIsOnTheShownRow(t *testing.T) {
	p, _ := plainHosts(t)
	if a, b := rowOf(t, p, "pc-a"), rowOf(t, p, "pc-b"); a.Name != "PC-A" || b.Name != "host-of-pc-b" {
		t.Fatalf("before: %q %q", a.Name, b.Name)
	}
	setHost(t, p, "pc-a", adminstate.HostEdit{Name: "Gaming PC"})
	setHost(t, p, "relay-a", adminstate.HostEdit{Name: "Closet box"})
	if got := rowOf(t, p, "pc-a").Name; got != "Gaming PC" {
		t.Errorf("the row: %q", got)
	}
	if got := rowOf(t, p, "pc-b").Name; got != "host-of-pc-b" {
		t.Errorf("another host's row: %q", got)
	}
	if got := p.BaseName("pc-a"); got != "PC-A" {
		t.Errorf("the configuration's name: %q", got)
	}
	if s := p.ShownSystem("pc-a"); s == nil || s.Name != "Gaming PC" {
		t.Errorf("ShownSystem: %+v", s)
	}
	if s := p.RelaySystem("relay-a"); s == nil || s.Name != "Closet box" {
		t.Errorf("RelaySystem: %+v", s)
	}
	if key, e, ok := p.HostEdit("pc-a"); !ok || key != "pc-a" || e.Name != "Gaming PC" {
		t.Errorf("HostEdit: %q %+v %v", key, e, ok)
	}
	p.PollOnce(context.Background()) // a poll does not put the configuration's name back
	if got := rowOf(t, p, "pc-a").Name; got != "Gaming PC" {
		t.Errorf("after a poll: %q", got)
	}
	setHost(t, p, "pc-a", adminstate.HostEdit{})
	if got := rowOf(t, p, "pc-a").Name; got != "PC-A" {
		t.Errorf("after Reset: %q", got)
	}
	// a name kept under an id that is no machine's names nothing
	setHost(t, p, "pc-z", adminstate.HostEdit{Name: "Nobody"})
	for _, s := range p.Snapshot() {
		if s.Name == "Nobody" {
			t.Errorf("%s is shown under a name kept for another id", s.ID)
		}
	}
}

// A Proxmox node with an agent has two ids and is shown under one of them at a time. What was changed about
// the machine is the machine's under either: kept with the agent's id, it is on the node's row, and on the
// agent's row after a restart of the panel with the machine off. A guest's row never gets a host's name.
func TestPanelNameAndWolFollowAMachineWithTwoIDs(t *testing.T) {
	cfg := testutil.TwoHostsConfig(t, "")
	b := testutil.NewBackend()
	up := true
	b.PVEOf = func(cluster string) ([]jsonx.M, error) {
		if cluster == "lab-a" || up {
			return testutil.HostResources(map[string]string{"lab-a": "PVE-A", "lab-b": "PVE-B"}[cluster], "stopped"), nil
		}
		return nil, devices.Errf("connection failed")
	}
	for _, id := range []string{"lab-a-agent", "lab-b-agent"} {
		b.Metrics[id] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	}
	start := func() *Poller {
		p, err := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, b)
		if err != nil {
			t.Fatal(err)
		}
		p.PollOnce(context.Background())
		p.PollOnce(context.Background())
		return p
	}
	p := start()
	const node, agent = "lab-b-node-pve-b", "lab-b-agent"
	if got := p.HostKey(node); got != agent {
		t.Fatalf("a change about the node's row is kept under %q, not under its agent's id", got)
	}
	setHost(t, p, agent, adminstate.HostEdit{Name: "Cellar", Wol: &adminstate.WolEdit{MACs: []string{"02:00:00:00:00:b9"}}})
	if got := rowOf(t, p, node).Name; got != "Cellar" {
		t.Errorf("the node's row: %q", got)
	}
	if o := p.PowerOverride(node); o == nil || o.Wol == nil || !slices.Equal(o.Wol.AllMACs(), []string{"02:00:00:00:00:b9"}) || o.Wol.Relay != "lab-a-agent" || o.Wol.Broadcast != "192.0.2.255" {
		t.Errorf("the node's Wake-on-LAN: %+v", o)
	}
	guest := ""
	for _, s := range p.Snapshot() {
		if s.Kind != "node" && s.Name == "Cellar" {
			t.Errorf("the row %s (%s) has the host's name", s.ID, s.Kind)
		}
		if s.Kind == "qemu" || s.Kind == "lxc" {
			guest = s.ID
		}
	}
	// ... and a name kept under a guest's id (a file edited by hand) names nothing: only hosts have one here
	if guest == "" {
		t.Fatal("the rig has no guest")
	}
	was := rowOf(t, p, guest).Name
	setHost(t, p, guest, adminstate.HostEdit{Name: "Not a host"})
	if got := rowOf(t, p, guest).Name; got != was {
		t.Errorf("the guest %s is shown as %q", guest, got)
	}
	setHost(t, p, guest, adminstate.HostEdit{})
	// the panel starts again while the machine is off: no node row, the agent's row stands for the machine
	b.Mu.Lock()
	up = false
	b.Metrics[agent] = func() (jsonx.M, error) { return nil, devices.Errf("connection failed") }
	b.Mu.Unlock()
	p = start()
	if got := rowOf(t, p, agent).Name; got != "Cellar" {
		t.Errorf("the agent's row after a restart with the machine off: %q", got)
	}
	o := p.PowerOverride(agent)
	if o == nil || o.Wol == nil || !slices.Equal(o.Wol.AllMACs(), []string{"02:00:00:00:00:b9"}) {
		t.Errorf("its Wake-on-LAN there: %+v", o)
	}
	if acts := rowOf(t, p, agent).PowerActions; !slices.Contains(acts, "wake") {
		t.Errorf("its actions there: %v", acts)
	}
	// and one kept under the node's id (a file edited by hand) counts under both as well
	setHost(t, p, agent, adminstate.HostEdit{})
	setHost(t, p, node, adminstate.HostEdit{Name: "Under the node's id"})
	if got := rowOf(t, p, agent).Name; got != "Under the node's id" {
		t.Errorf("kept under the node's id, shown under the agent's: %q", got)
	}
}

// PowerOverride is what the Wake button, the wake and "Update unpowered hosts" ask. The panel's decisions lie
// over the configuration's wol:, each over its own part; the entry of the configuration is never changed; the
// target address and the password are the configuration's in every case.
func TestPanelWolIsWhatPowerOverrideReturns(t *testing.T) {
	logs := &syncBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	p, _ := plainHosts(t)
	wol := func(id string) *config.Wol {
		t.Helper()
		o := p.PowerOverride(id)
		if o == nil {
			return nil
		}
		return o.Wol
	}
	base := []string{"02:00:00:00:00:a1", "02:00:00:00:00:a2"}
	if w := wol("pc-a"); w == nil || !slices.Equal(w.AllMACs(), base) || w.Relay != "relay-a" || wol("pc-b") != nil {
		t.Fatalf("before: %+v %+v", w, wol("pc-b"))
	}
	setHost(t, p, "pc-a", adminstate.HostEdit{Wol: &adminstate.WolEdit{MACs: []string{"02:00:00:00:00:a9"}, Relay: "relay-b", Port: 7}})
	if w := wol("pc-a"); !slices.Equal(w.AllMACs(), []string{"02:00:00:00:00:a9"}) || w.Relay != "relay-b" || w.Port != 7 || w.Broadcast != "" || w.PasswordEnv != "" {
		t.Errorf("MACs, relay and port decided: %+v", w)
	}
	if c := p.Override("pc-a").Wol; !slices.Equal(c.AllMACs(), base) || c.Relay != "relay-a" {
		t.Errorf("the configuration's entry was changed: %+v", c)
	}
	if c := p.ConfigWol("pc-a"); !slices.Equal(c.AllMACs(), base) || c.Relay != "relay-a" {
		t.Errorf("ConfigWol: %+v", c)
	}
	// off: no Wake-on-LAN, and no Wake among the row's actions
	setHost(t, p, "pc-a", adminstate.HostEdit{Wol: &adminstate.WolEdit{Off: true}})
	if w := wol("pc-a"); w != nil || slices.Contains(rowOf(t, p, "pc-a").PowerActions, "wake") {
		t.Errorf("off: %+v, actions %v", w, rowOf(t, p, "pc-a").PowerActions)
	}
	setHost(t, p, "pc-a", adminstate.HostEdit{})
	if w := wol("pc-a"); w == nil || !slices.Equal(w.AllMACs(), base) || !slices.Contains(rowOf(t, p, "pc-a").PowerActions, "wake") {
		t.Errorf("after Reset: %+v", w)
	}
	// a machine without a wol: gets one from the panel: MACs and a relay
	setHost(t, p, "pc-b", adminstate.HostEdit{Wol: &adminstate.WolEdit{MACs: []string{"02:00:00:00:00:b1"}, Relay: "relay-b"}})
	if w := wol("pc-b"); w == nil || w.Relay != "relay-b" || !slices.Equal(w.AllMACs(), []string{"02:00:00:00:00:b1"}) || !slices.Contains(rowOf(t, p, "pc-b").PowerActions, "wake") {
		t.Errorf("a new Wake-on-LAN: %+v", w)
	}
	// Decisions that cannot be used change nothing, and each is said once in the log.
	unused := map[string]adminstate.WolEdit{
		"a relay that is no agent":            {Relay: "relay-z"},
		"a relay that sends no wake packets":  {Relay: "pc-b"},
		"another relay with a target address": {Relay: "relay-b"},
		"another relay with a password":       {Relay: "relay-b"},
		"itself as its relay":                 {Relay: "relay-a", MACs: []string{"02:00:00:00:00:f1"}},
		"MACs alone without a wol:":           {MACs: []string{"02:00:00:00:00:f2"}},
	}
	host := map[string]string{"a relay that is no agent": "pc-a", "a relay that sends no wake packets": "pc-a", "another relay with a target address": "box-c",
		"another relay with a password": "box-d", "itself as its relay": "relay-a", "MACs alone without a wol:": "relay-b"}
	for what, e := range unused {
		id := host[what]
		before := wol(id)
		setHost(t, p, id, adminstate.HostEdit{Wol: &e})
		n := logs.warns("Wake-on-LAN decisions of a host are not used")
		after := wol(id)
		_ = rowOf(t, p, id) // a second look says nothing more
		_ = wol(id)
		switch {
		case (before == nil) != (after == nil):
			t.Errorf("%s: before %+v, after %+v", what, before, after)
		case before != nil && (!slices.Equal(before.AllMACs(), after.AllMACs()) || before.Relay != after.Relay || before.Broadcast != after.Broadcast || before.PasswordEnv != after.PasswordEnv):
			t.Errorf("%s: before %+v, after %+v", what, before, after)
		}
		if got := logs.warns("Wake-on-LAN decisions of a host are not used"); got != n+1 && got != n {
			t.Errorf("%s: %d warnings for one decision", what, got-n)
		}
		if _, why := p.WolWith(id, p.ConfigWol(id), e); why == "" {
			t.Errorf("%s: WolWith says it is used", what)
		}
		setHost(t, p, id, adminstate.HostEdit{})
	}
	if n := logs.warns("Wake-on-LAN decisions of a host are not used"); n != len(unused) {
		t.Errorf("%d decisions that cannot be used, %d warnings", len(unused), n)
	}
	// With a target address or a password in config.yaml, other MACs and the port can be decided, and the
	// address and the password stay where they are.
	setHost(t, p, "box-c", adminstate.HostEdit{Wol: &adminstate.WolEdit{MACs: []string{"02:00:00:00:00:c9"}, Port: 7}})
	if w := wol("box-c"); !slices.Equal(w.AllMACs(), []string{"02:00:00:00:00:c9"}) || w.Relay != "relay-a" || w.Broadcast != "10.20.30.255" || w.Port != 7 {
		t.Errorf("box-c: %+v", w)
	}
	setHost(t, p, "box-d", adminstate.HostEdit{Wol: &adminstate.WolEdit{MACs: []string{"02:00:00:00:00:d9"}}})
	if w := wol("box-d"); !slices.Equal(w.AllMACs(), []string{"02:00:00:00:00:d9"}) || w.Relay != "relay-a" || w.PasswordEnv != "WOL_PASSWORD_D" {
		t.Errorf("box-d: %+v", w)
	}
	if got := p.WolRelays(); len(got) != 2 || got[0].ID != "relay-a" || got[1].ID != "relay-b" {
		t.Errorf("the relays: %+v", got)
	}
}
