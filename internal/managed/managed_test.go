package managed

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

const tokA, tokB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestIDFor(t *testing.T) {
	for in, want := range map[string]string{"SRV-WINPC": "srv-winpc", "OFFICE PC": "office-pc", "node-x": "host-node-x", "!!": ""} {
		if got := IDFor(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestStoreAndMerge(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve.example:8006"}, "http://127.0.0.1:1", "http://127.0.0.1:2")
	s := NewStore(cfg.DataDir)
	if hosts, err := s.Load(); err != nil || hosts != nil {
		t.Fatal(hosts, err)
	}
	if err := s.Put(Host{ID: "relay-box", Name: "relay-box", URL: "http://relay-box:9101", OS: "linux", Options: Options{WolRelay: true}},
		Secret{Token: tokA + "r", ControlToken: tokB + "r"}); err != nil {
		t.Fatal(err)
	}
	winpc := Host{ID: "srv-winpc", Name: "SRV-WINPC", URL: "http://srv-winpc:9101", Site: "Alice", OS: "windows",
		Options: Options{PowerControl: true, CoreTemp: true}, Wol: &config.Wol{MACs: []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}, Relay: "relay-box"},
		AddedAt: time.Now().UTC()}
	if err := s.Put(winpc, Secret{Token: tokA, ControlToken: tokB}); err != nil {
		t.Fatal(err)
	}
	// the same id as config.yaml's laptop: config.yaml wins
	if err := s.Put(Host{ID: "laptop", Name: "dup", URL: "http://x:9101", OS: "linux"}, Secret{Token: tokA}); err != nil {
		t.Fatal(err)
	}
	// invalid: Wake-on-LAN relay that is no relay agent
	if err := s.Put(Host{ID: "bad", Name: "bad", URL: "http://bad:9101", OS: "linux", Wol: &config.Wol{MAC: "aa:bb:cc:dd:ee:03", Relay: "tower"}}, Secret{Token: tokA}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(s.Path())
	if strings.Contains(string(b), tokA) || strings.Contains(string(b), tokB) {
		t.Fatal("tokens in managed-hosts.yaml")
	}
	if st, _ := os.Stat(s.secretPath("srv-winpc")); st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	n := len(cfg.Agents)
	added, skipped, err := s.Merge(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 || added[1].Agent.ID != "srv-winpc" || len(cfg.Agents) != n+2 || len(skipped) != 2 {
		t.Fatalf("added %d, skipped %v", len(added), skipped)
	}
	a := cfg.Agents[n+1]
	if !a.Managed || a.Token != tokA || a.ControlToken != tokB || !a.PowerControl || a.WolRelay || a.Site != "Alice" {
		t.Fatalf("agent: %+v", a)
	}
	last := cfg.Systems[len(cfg.Systems)-1]
	if last.ID != "srv-winpc" || len(last.Wol.AllMACs()) != 2 {
		t.Fatalf("wol entry: %+v", last)
	}
	if !strings.Contains(strings.Join(skipped, "\n"), "laptop") || !strings.Contains(strings.Join(skipped, "\n"), "wol_relay") {
		t.Fatal(skipped)
	}
	if err := s.Remove("srv-winpc"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Secret("srv-winpc"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("secret kept after Remove:", err)
	}
	if err := s.Remove("srv-winpc"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if hosts, _ := s.Load(); len(hosts) != 3 {
		t.Fatal(hosts)
	}
}

// A Proxmox node added with Add host comes back at start as a proxmox: entry (its secret and CA from the
// panel's state); config.yaml wins on the same id, and a clashing id_prefix is refused.
func TestManagedProxmoxNode(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve.example:8006"}, "http://127.0.0.1:1", "http://127.0.0.1:2")
	s := NewStore(cfg.DataDir)
	node := Host{ID: "mini-i7", Name: "mini-i7", URL: "http://mini-i7:9101", Site: "Bob", OS: "linux", Node: "MINI-i7",
		SystemID: "bob-mini-i7-node-mini-i7", ProxmoxWanted: true,
		Proxmox: &PVE{ID: "bob-mini-i7", IDPrefix: "bob-mini-i7-", Node: "MINI-i7", URL: "https://mini-i7:8006", ServerName: "MINI-i7",
			TokenID: "fairy@pve!panel-mini-i7", CA: true}}
	sec := Secret{Token: tokA, ControlToken: tokB, PVETokenSecret: "0b7c4e2a-1111-2222-3333-444455556666"}
	if err := s.PutCA(node.ID, []byte("-----BEGIN CERTIFICATE-----\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(node, sec); err != nil {
		t.Fatal(err)
	}
	if node.Proxmox.TokenName() != "panel-mini-i7" {
		t.Fatal(node.Proxmox.TokenName())
	}
	c := *cfg
	added, skipped, err := s.Merge(&c)
	if err != nil || len(added) != 1 || len(skipped) != 0 {
		t.Fatal(added, skipped, err)
	}
	px := c.Proxmox[len(c.Proxmox)-1]
	if !px.Managed || px.TokenSecret != sec.PVETokenSecret || px.CAFile != s.CAPath("mini-i7") || px.TLSServerName != "MINI-i7" ||
		px.Prefix() != "bob-mini-i7-" || *px.Group != "Bob" {
		t.Fatalf("%+v", px)
	}
	if a := c.Agents[len(c.Agents)-1]; a.LinksTo != "bob-mini-i7-node-mini-i7" || a.Node != "" {
		t.Fatalf("%+v", a)
	}
	// a hand-written cluster with the same id prefix: refused
	clash := *cfg
	p := "bob-mini-i7-"
	clash.Proxmox = append(clash.Proxmox[:len(clash.Proxmox):len(clash.Proxmox)], config.Proxmox{ID: "bob-by-hand", IDPrefix: &p,
		URLs: []string{"https://x:8006"}, TokenID: "a@pve!b"})
	if err := Check(&clash, s.EntryOf(node, sec)); err == nil || !strings.Contains(err.Error(), "id prefix") {
		t.Fatal(err)
	}
	// config.yaml has a proxmox entry with the same id: it wins, the managed node is skipped at start
	same := *cfg
	same.Proxmox = append(same.Proxmox[:len(same.Proxmox):len(same.Proxmox)], config.Proxmox{ID: "bob-mini-i7", URLs: []string{"https://x:8006"}, TokenID: "a@pve!b"})
	if _, skipped, _ := s.Merge(&same); len(skipped) != 1 || !strings.Contains(skipped[0], "already configured") {
		t.Fatal(skipped)
	}
	// Remove deletes the CA too
	if err := s.Remove("mini-i7"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.CAPath("mini-i7")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("CA kept", err)
	}
}

// T-48: what an older release left in managed-hosts.yaml after a Re-pair that was answered without a Proxmox
// token: proxmox_wanted, no proxmox entry, and still the node name the machine gave itself and the id of the
// node it was. Such a host is a plain host: its agent is linked to no node, not by id and not by name.
func TestAHostThatLostItsNodeIsLinkedToNothing(t *testing.T) {
	stale := Host{ID: "friend-box", Name: "friend-box", URL: "http://friend-box:9101", OS: "linux", Site: "Friend",
		ProxmoxWanted: true, Node: "pve", SystemID: "friend-pve-node-pve"}
	a := stale.Agent(Secret{Token: "read-token-xxxxxxxxxxxxxxxxxxxxxxxx"})
	if a.Node != "" || a.LinksTo != "" {
		t.Fatalf("the agent of a host without a node entry is linked: node=%q links_to=%q", a.Node, a.LinksTo)
	}
	if stale.Shown() != "friend-box" {
		t.Fatalf("it shows as %q", stale.Shown())
	}
	// unchanged: a node of its own is linked by id; an agent the admin put on an existing node, by name
	own := stale
	own.Proxmox = &PVE{ID: "friend-pve", IDPrefix: "friend-pve-", Node: "pve"}
	if a := own.Agent(Secret{}); a.Node != "" || a.LinksTo != "friend-pve-node-pve" || own.Shown() != "friend-pve-node-pve" {
		t.Fatalf("a host that is its own node: node=%q links_to=%q shown=%q", a.Node, a.LinksTo, own.Shown())
	}
	chosen := Host{ID: "box", Name: "box", URL: "http://box:9101", OS: "linux", Node: "pve", SystemID: "node-pve"}
	if a := chosen.Agent(Secret{}); a.Node != "pve" || a.LinksTo != "" || chosen.Shown() != "node-pve" {
		t.Fatalf("an agent on a node the admin chose: node=%q links_to=%q shown=%q", a.Node, a.LinksTo, chosen.Shown())
	}
}
