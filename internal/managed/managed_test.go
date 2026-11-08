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
	for in, want := range map[string]string{"NSX-FURINA": "nsx-furina", "PROJECT REBUILD": "project-rebuild", "node-x": "host-node-x", "!!": ""} {
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
	furina := Host{ID: "nsx-furina", Name: "NSX-FURINA", URL: "http://nsx-furina:9101", Site: "Neptune", OS: "windows",
		Options: Options{PowerControl: true, CoreTemp: true}, Wol: &config.Wol{MACs: []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}, Relay: "relay-box"},
		AddedAt: time.Now().UTC()}
	if err := s.Put(furina, Secret{Token: tokA, ControlToken: tokB}); err != nil {
		t.Fatal(err)
	}
	// the same id as config.yaml's laptop: config.yaml wins
	if err := s.Put(Host{ID: "laptop", Name: "dup", URL: "http://x:9101", OS: "linux"}, Secret{Token: tokA}); err != nil {
		t.Fatal(err)
	}
	// invalid: Wake-on-LAN relay that is no relay agent
	if err := s.Put(Host{ID: "bad", Name: "bad", URL: "http://bad:9101", OS: "linux", Wol: &config.Wol{MAC: "aa:bb:cc:dd:ee:03", Relay: "cyrene"}}, Secret{Token: tokA}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(s.Path())
	if strings.Contains(string(b), tokA) || strings.Contains(string(b), tokB) {
		t.Fatal("tokens in managed-hosts.yaml")
	}
	if st, _ := os.Stat(s.secretPath("nsx-furina")); st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	n := len(cfg.Agents)
	added, skipped, err := s.Merge(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 || added[1].Agent.ID != "nsx-furina" || len(cfg.Agents) != n+2 || len(skipped) != 2 {
		t.Fatalf("added %d, skipped %v", len(added), skipped)
	}
	a := cfg.Agents[n+1]
	if !a.Managed || a.Token != tokA || a.ControlToken != tokB || !a.PowerControl || a.WolRelay || a.Site != "Neptune" {
		t.Fatalf("agent: %+v", a)
	}
	last := cfg.Systems[len(cfg.Systems)-1]
	if last.ID != "nsx-furina" || len(last.Wol.AllMACs()) != 2 {
		t.Fatalf("wol entry: %+v", last)
	}
	if !strings.Contains(strings.Join(skipped, "\n"), "laptop") || !strings.Contains(strings.Join(skipped, "\n"), "wol_relay") {
		t.Fatal(skipped)
	}
	if err := s.Remove("nsx-furina"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Secret("nsx-furina"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("secret kept after Remove:", err)
	}
	if err := s.Remove("nsx-furina"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if hosts, _ := s.Load(); len(hosts) != 3 {
		t.Fatal(hosts)
	}
}
