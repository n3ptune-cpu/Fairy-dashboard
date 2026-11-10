package web

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/power"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/ups"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Upgrade in place with hosts that the old release added with Add host (fixture "v-before-managed": written by
// the code of commit 05b1019, which paired with the old exchange). After the upgrade they load, none is
// skipped, they are polled with their old tokens and shown as "not verified"; nothing in the files is changed
// by starting the new panel. Re-pair replaces one such pairing with a verified one and leaves the other host
// alone.
func TestUpgradeOldManagedHostsLoadAndCanBeRepaired(t *testing.T) {
	v := testutil.NewVBefore(t)
	testutil.AddVBeforeManaged(t, v.DataDir)
	yamlBefore := string(must(os.ReadFile(filepath.Join(v.DataDir, "managed-hosts.yaml"))))
	if strings.Contains(yamlBefore, "pairing:") {
		t.Fatal("the fixture must be an old file, without the pairing field")
	}
	cfg := v.Load(t)
	if err := power.ValidateConfigs(cfg.PowerSources); err != nil {
		t.Fatal(err)
	}
	if err := ups.ValidateConfigs(cfg.UPS); err != nil {
		t.Fatal(err)
	}
	// as `fairy serve` does at start
	added, skipped, err := managed.NewStore(cfg.DataDir).Merge(cfg)
	if err != nil || len(skipped) != 0 || len(added) != 2 {
		t.Fatal(added, skipped, err)
	}
	b := testutil.NewBackend()
	b.Mu.Lock()
	for _, id := range []string{"srv-winpc", "srv-pvea"} {
		b.Metrics[id] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	}
	b.Mu.Unlock()
	p, err := poller.New(cfg, v.Secrets, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	s := New(cfg, v.Secrets, p, nil, "test")
	rel := testInstallers // the release the new panel installs: newer than the hosts' agent 13
	rel.Tag, rel.Version, rel.Base = "build-20", "20-abcdef0", strings.Replace(rel.Base, "build-7", "build-20", 1)
	s.Enroll.Scripts, s.Enroll.PanelAddr, s.Enroll.Interval = rel, "100.64.0.10", 10*time.Millisecond
	s.control = newRateLimiter(100)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e := &env{t: t, s: s, srv: srv, b: b}
	idp := newFakeIDP(t)
	s.Cfg.OIDC.Issuer = idp.srv.URL

	// polled with the tokens the old release stored, the node with its Proxmox entry and CA
	fur, ok := p.AgentByID("srv-winpc")
	if !ok || !fur.Managed || fur.Token != "old-winpc-read-token-xxxxxxxxxxxxxxx" || fur.ControlToken != "old-winpc-control-token-yyyyyyyyyy" || !fur.PowerControl || !fur.SelfUpdate {
		t.Fatalf("%+v", fur)
	}
	kach, ok := p.AgentByID("srv-pvea")
	if !ok || kach.Token != "old-pvea-read-token-xxxxxxxxxxxxxx" || kach.LinksTo != "alice-srv-pvea-node-srv-pvea" {
		t.Fatalf("%+v", kach)
	}
	var node bool
	for _, c := range p.Clusters() {
		if c.Managed && c.ID == "alice-srv-pvea" && c.TokenSecret == "0b7c4e2a-1111-2222-3333-444455556666" && c.TLSServerName == "SRV-PVEA" {
			ca, err := os.ReadFile(c.CAFile)
			node = err == nil && strings.Contains(string(ca), "BEGIN CERTIFICATE")
		}
	}
	if !node {
		t.Fatalf("the managed Proxmox node did not load: %+v", p.Clusters())
	}

	r := idp.signInAs(e, map[string]any{"sub": v.OwnerSub, "name": "Owner", "email": "owner@example.com", "email_verified": true})
	if r.code != 303 {
		t.Fatal("sign-in", r.code, string(r.raw))
	}
	ck := sessionCookie(e, r.hdr)
	wr := []string{"Cookie", ck, "X-Requested-With", "fairy"}
	if g := e.do("GET", "/api/systems/srv-winpc", nil, "Cookie", ck); g.code != 200 || g.body["status"] != "online" {
		t.Fatal("the old host is not shown online:", g.code, string(g.raw))
	}
	page := e.do("GET", "/api/admin/hosts", nil, "Cookie", ck)
	rows, _ := page.body["managed"].([]any)
	if page.code != 200 || len(rows) != 2 || page.body["skipped"] != nil {
		t.Fatal(page.code, string(page.raw))
	}
	for _, x := range rows {
		row := x.(map[string]any)
		if _, has := row["pairing"]; has || row["active"] != true || row["agent_version"] != "13-d43fdaf" {
			t.Fatalf("an old host must load as it was, without a verified mark: %v", row)
		}
	}
	if got := string(must(os.ReadFile(filepath.Join(v.DataDir, "managed-hosts.yaml")))); got != yamlBefore {
		t.Fatalf("starting the new panel changed managed-hosts.yaml:\n%s", got)
	}

	// Re-pair SRV-WINPC: the machine answers a verified pairing (the exchange itself: internal/agentcontract)
	s.Enroll.Pair = func(_ context.Context, url string, key []byte) (int, enroll.Response, string, error) {
		if url != "http://srv-winpc:9101" || len(key) != enroll.KeySize {
			t.Error("pairing request for", url)
		}
		return 200, enroll.Response{AgentVersion: "20-abcdef0", Hostname: "SRV-WINPC", Platform: "windows", Token: "new-winpc-read-token-xxxxxxxxxxxxxxx",
			ControlToken: "new-winpc-control-token-yyyyyyyyyy"}, "", nil
	}
	rp := e.do("POST", "/api/admin/hosts/srv-winpc/repair", map[string]any{"confirm": "SRV-WINPC"}, wr...)
	if rp.code != 201 || !strings.Contains(rp.body["command"].(string), "$env:FAIRY_JOIN_CODE='"+rp.body["code"].(string)+"'") ||
		!strings.Contains(rp.body["command"].(string), " -PairV2 -PowerControl -SelfUpdate -CoreTemp") || rp.body["warning"] != nil {
		t.Fatal(rp.code, string(rp.raw))
	}
	var st map[string]any
	for i := 0; i < 300; i++ {
		time.Sleep(15 * time.Millisecond)
		p.PollOnce(context.Background())
		if st = e.do("GET", "/api/admin/enrollments/"+rp.body["id"].(string), nil, "Cookie", ck).body; st["done"] == true {
			break
		}
	}
	if st["state"] != "online" || st["pairing"] != "v2" {
		t.Fatal(st)
	}
	hosts, err := s.Managed.Load()
	if err != nil || len(hosts) != 2 {
		t.Fatal(hosts, err)
	}
	for _, h := range hosts {
		switch h.ID {
		case "srv-winpc":
			if h.Pairing != managed.PairingV2 || h.AgentVersion != "20-abcdef0" || !h.Options.PowerControl || !h.Options.CoreTemp || h.Site != "Alice" || h.OS != "windows" {
				t.Fatalf("%+v", h)
			}
		case "srv-pvea": // not touched
			if h.Pairing != "" || h.AgentVersion != "13-d43fdaf" || h.Proxmox == nil || h.Proxmox.ID != "alice-srv-pvea" {
				t.Fatalf("%+v", h)
			}
		default:
			t.Fatal(h.ID)
		}
	}
	if sec, err := s.Managed.Secret("srv-winpc"); err != nil || sec.Token != "new-winpc-read-token-xxxxxxxxxxxxxxx" {
		t.Fatal(sec, err)
	}
	if b := string(must(os.ReadFile(filepath.Join(v.DataDir, "secrets", "agent-srv-winpc.json")))); strings.Contains(b, "old-winpc") {
		t.Fatal("the old tokens are still on disk")
	}
	if sec, err := s.Managed.Secret("srv-pvea"); err != nil || sec.Token != "old-pvea-read-token-xxxxxxxxxxxxxx" || sec.PVETokenSecret == "" {
		t.Fatal(sec, err)
	}
	if a, _ := p.AgentByID("srv-winpc"); a.Token != "new-winpc-read-token-xxxxxxxxxxxxxxx" {
		t.Fatalf("the poller still uses the old token: %+v", a)
	}
	// the file the new panel wrote: one host marked, the other as before; and a second start reads it
	y := string(must(os.ReadFile(filepath.Join(v.DataDir, "managed-hosts.yaml"))))
	if strings.Count(y, "pairing: v2") != 1 {
		t.Fatalf("%s", y)
	}
	cfg2 := v.Load(t)
	if added, skipped, err := managed.NewStore(cfg2.DataDir).Merge(cfg2); err != nil || len(skipped) != 0 || len(added) != 2 {
		t.Fatal(added, skipped, err)
	}
}
