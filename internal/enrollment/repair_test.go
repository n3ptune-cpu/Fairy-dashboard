package enrollment

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
)

// testConfig is a valid panel configuration without hosts (a Re-pair's answer is checked against the
// configuration, like a new host's).
func testConfig(t *testing.T) *config.Panel {
	t.Helper()
	cfg, err := config.Parse([]byte("public_url: https://panel.example.com\noidc: {issuer: \"https://id.example.com\", client_id: cid, allowed_groups: [homelab]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Re-pair keeps none of the previous pairing's secrets: the tokens file holds only the new tokens, the poller
// is told to forget the old ones before the new ones are added, and a node that sends no Proxmox grant this
// time loses its old CA file.
func TestRepairDropsTheOldTokens(t *testing.T) {
	store := managed.NewStore(t.TempDir())
	h := managed.Host{ID: "srv-pvea", Name: "srv-pvea", URL: "http://srv-pvea:9101", OS: "linux", ProxmoxWanted: true,
		Options: managed.Options{PowerControl: true}}
	old := managed.Secret{Token: "old-read-token-xxxxxxxxxxxxxxxxxx", ControlToken: "old-control-token-yyyyyyyyyyyyyyy", PVETokenSecret: "old-pve-secret-0000-1111"}
	if err := store.Put(h, old); err != nil {
		t.Fatal(err)
	}
	if err := store.PutCA(h.ID, []byte("-----BEGIN CERTIFICATE-----\nold\n-----END CERTIFICATE-----\n")); err != nil {
		t.Fatal(err)
	}
	var calls []string
	m := &Manager{Store: store, Now: time.Now, Interval: time.Millisecond, OnlineWait: time.Second, PanelAddr: "100.64.0.10",
		Scripts: Fixed(testInstallers), items: map[string]*Enrollment{},
		Hooks: Hooks{
			Config: func() *config.Panel { return testConfig(t) },
			Remove: func(id string) { calls = append(calls, "remove "+id) },
			Add:    func(e managed.Entry) error { calls = append(calls, "add "+e.Agent.Token); return nil },
			Online: func(string) (bool, string) { return true, "" },
			Event:  func(Enrollment, string, string) {},
		},
		Pair: func(context.Context, string, []byte) (int, enroll.Response, string, error) {
			return 200, enroll.Response{AgentVersion: "8-abcdef0", Token: "new-read-token-xxxxxxxxxxxxxxxxxx",
				ControlToken: "new-control-token-yyyyyyyyyyyyyyy"}, "", nil
		}}
	cr, err := m.Create(context.Background(), Request{}, &h, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if e, _ := m.Get(cr.ID); e.Done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e, _ := m.Get(cr.ID); e.State != Online {
		t.Fatalf("%+v", e)
	}
	sec, err := store.Secret(h.ID)
	if err != nil || sec.Token != "new-read-token-xxxxxxxxxxxxxxxxxx" || sec.ControlToken != "new-control-token-yyyyyyyyyyyyyyy" || sec.PVETokenSecret != "" {
		t.Fatalf("%+v %v", sec, err)
	}
	raw, _ := os.ReadFile(store.Path())
	files, _ := os.ReadDir(store.Path()[:strings.LastIndex(store.Path(), "/")] + "/secrets")
	for _, f := range files {
		b, _ := os.ReadFile(store.Path()[:strings.LastIndex(store.Path(), "/")] + "/secrets/" + f.Name())
		if strings.Contains(string(b), "old-") {
			t.Fatalf("old secret kept in %s", f.Name())
		}
	}
	if strings.Contains(string(raw), "old-") {
		t.Fatal("old secret in managed-hosts.yaml")
	}
	if _, err := os.Stat(store.CAPath(h.ID)); !os.IsNotExist(err) {
		t.Fatal("old CA kept", err)
	}
	if strings.Join(calls, ", ") != "remove srv-pvea, add new-read-token-xxxxxxxxxxxxxxxxxx" {
		t.Fatal(calls)
	}
}

// Remove while a Re-pair is pairing: the host does not come back. CancelHost either stops the pairing before it
// stores anything or waits until it has (and the caller then removes it).
func TestRemoveDuringRepairDoesNotBringTheHostBack(t *testing.T) {
	store := managed.NewStore(t.TempDir())
	h := managed.Host{ID: "srv-winpc", Name: "srv-winpc", URL: "http://srv-winpc:9101", OS: "linux"}
	if err := store.Put(h, managed.Secret{Token: "old-read-token-xxxxxxxxxxxxxxxxxx"}); err != nil {
		t.Fatal(err)
	}
	answering := make(chan struct{})
	release := make(chan struct{})
	var adds atomic.Int32
	m := &Manager{Store: store, Now: time.Now, Interval: time.Millisecond, OnlineWait: time.Second, PanelAddr: "100.64.0.10",
		Scripts: Fixed(testInstallers), items: map[string]*Enrollment{},
		Hooks: Hooks{
			Config: func() *config.Panel { return &config.Panel{} },
			Remove: func(string) {},
			Add:    func(managed.Entry) error { adds.Add(1); return nil },
			Online: func(string) (bool, string) { return true, "" },
			Event:  func(Enrollment, string, string) {},
		},
		Pair: func(context.Context, string, []byte) (int, enroll.Response, string, error) {
			close(answering) // the machine answers: the pairing is in flight
			<-release
			return 200, enroll.Response{Token: "new-read-token-xxxxxxxxxxxxxxxxxx"}, "", nil
		}}
	cr, err := m.Create(context.Background(), Request{}, &h, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-answering
	m.CancelHost(h.ID) // Remove
	_ = store.Remove(h.ID)
	close(release)
	time.Sleep(50 * time.Millisecond)
	if e, _ := m.Get(cr.ID); e.State != Cancelled || adds.Load() != 0 {
		t.Fatalf("%+v adds=%d", e, adds.Load())
	}
	if hosts, _ := store.Load(); len(hosts) != 0 {
		t.Fatal("the removed host came back", hosts)
	}
	if m.Cancel(cr.ID) {
		t.Fatal("cancelled twice")
	}
}

// L12: Re-pair runs the checks of Add host. A managed host whose id config.yaml also uses (config.yaml wins, the
// managed one is skipped at start) is refused before anything is stored or forgotten, like a new host would be.
func TestRepairOfAHostThatConfigYAMLShadowsIsRefused(t *testing.T) {
	store := managed.NewStore(t.TempDir())
	h := managed.Host{ID: "srv-winpc", Name: "srv-winpc", URL: "http://srv-winpc:9101", OS: "linux"}
	if err := store.Put(h, managed.Secret{Token: "old-read-token-xxxxxxxxxxxxxxxxxx"}); err != nil {
		t.Fatal(err)
	}
	var removed, added atomic.Int32
	m := &Manager{Store: store, Now: time.Now, Interval: time.Millisecond, OnlineWait: time.Second, PanelAddr: "100.64.0.10",
		Scripts: Fixed(testInstallers), items: map[string]*Enrollment{},
		Hooks: Hooks{
			Config: func() *config.Panel {
				c := testConfig(t)
				c.Agents = append(c.Agents, config.Agent{ID: "srv-winpc", URL: "http://10.0.0.5:9101", TokenEnv: "TOK"}, // config.yaml's
					config.Agent{ID: "other", URL: "http://other:9101", Managed: true, Token: "other-read-token-xxxxxxxxxxxxxxxx"})
				return c
			},
			Remove: func(string) { removed.Add(1) },
			Add:    func(managed.Entry) error { added.Add(1); return nil },
			Online: func(string) (bool, string) { return true, "" },
			Event:  func(Enrollment, string, string) {},
		},
		Pair: func(context.Context, string, []byte) (int, enroll.Response, string, error) {
			return 200, enroll.Response{Token: "new-read-token-xxxxxxxxxxxxxxxxxx"}, "", nil
		}}
	cr, err := m.Create(context.Background(), Request{}, &h, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	var e Enrollment
	for i := 0; i < 200 && !e.Done; i++ {
		time.Sleep(5 * time.Millisecond)
		e, _ = m.Get(cr.ID)
	}
	if e.State != Failed || !strings.Contains(e.Message, "an agent with id srv-winpc is already configured") || removed.Load() != 0 || added.Load() != 0 {
		t.Fatalf("%+v removed=%d added=%d", e, removed.Load(), added.Load())
	}
	if sec, err := store.Secret(h.ID); err != nil || sec.Token != "old-read-token-xxxxxxxxxxxxxxxxxx" {
		t.Fatalf("the stored tokens changed: %+v %v", sec, err)
	}
}

// withoutHost takes out exactly the host's own entries: other hosts, and entries of config.yaml with the same
// ids, stay.
func TestWithoutHost(t *testing.T) {
	prefix := "s-a-"
	cfg := &config.Panel{
		Agents:  []config.Agent{{ID: "a", Managed: true}, {ID: "b", Managed: true}, {ID: "c"}},
		Systems: []config.SystemOverride{{ID: "s-a-node-a"}, {ID: "b"}},
		Proxmox: []config.Proxmox{{ID: "s-a", IDPrefix: &prefix, Managed: true}, {ID: "s-b", Managed: true}, {ID: "yaml"}},
	}
	old := managed.Host{ID: "a", SystemID: "s-a-node-a", Proxmox: &managed.PVE{ID: "s-a"}, Wol: &config.Wol{MACs: []string{"aa:bb:cc:dd:ee:ff"}, Relay: "b"}}
	c := withoutHost(cfg, old)
	if len(c.Agents) != 2 || c.Agents[0].ID != "b" || len(c.Systems) != 1 || c.Systems[0].ID != "b" || len(c.Proxmox) != 2 || c.Proxmox[0].ID != "s-b" {
		t.Fatalf("%+v", c)
	}
	if len(cfg.Agents) != 3 || len(cfg.Systems) != 2 || len(cfg.Proxmox) != 3 {
		t.Fatal("the shared configuration was changed")
	}
	// the same ids, but written in config.yaml (not managed): they stay, and a host without Wake-on-LAN or
	// BMC power has no systems: entry to take out
	c = withoutHost(&config.Panel{Agents: []config.Agent{{ID: "a"}}, Systems: []config.SystemOverride{{ID: "a"}}, Proxmox: []config.Proxmox{{ID: "s-a"}}},
		managed.Host{ID: "a", Proxmox: &managed.PVE{ID: "s-a"}})
	if len(c.Agents) != 1 || len(c.Systems) != 1 || len(c.Proxmox) != 1 {
		t.Fatalf("%+v", c)
	}
}
