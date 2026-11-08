package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Add host end to end (with a fake machine): a control action (step-up), one command with a 160-bit code, the
// panel pairs by itself, stores the tokens in its own state (not config.yaml), polls the host at once; Remove
// and Re-pair for hosts added this way only.
func TestAddHostPairsAndPolls(t *testing.T) {
	e := setup(t)
	e.s.Enroll.Interval = 20 * time.Millisecond
	e.s.control = newRateLimiter(100)
	e.s.Enroll.PanelAddr = "100.88.109.47"
	var proofSeen atomic.Value
	attempts := atomic.Int32{}
	e.s.Enroll.Pair = func(_ context.Context, url, proof string) (int, enroll.Response, string, error) {
		proofSeen.Store(proof)
		if attempts.Add(1) < 3 {
			return 0, enroll.Response{}, "", errors.New("dial tcp: connection refused")
		}
		return 200, enroll.Response{AgentVersion: "42-abc", Platform: "linux", Token: "furina-read-token-xxxxxxxxxxxxxxxx",
			ControlToken: "furina-control-token-yyyyyyyyyyyyyy"}, "", nil
	}
	e.b.Mu.Lock()
	e.b.Metrics["nsx-furina"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	e.b.Mu.Unlock()
	csrf := []string{"X-Requested-With", "fairy"}
	body := map[string]any{"name": "NSX-FURINA", "os": "windows", "site": "Neptune",
		"options": map[string]any{"power_control": true, "coretemp": true, "self_update": true}}
	if r := e.do("POST", "/api/admin/hosts", body, append([]string{"Cookie", e.cookie(viewer, 0)}, csrf...)...); r.code != 403 {
		t.Fatal("viewer", r.code)
	}
	if r := e.do("POST", "/api/admin/hosts", body, append([]string{"Cookie", e.cookie(controller(), 10*time.Minute)}, csrf...)...); r.code != 401 {
		t.Fatal("step-up", r.code)
	}
	c := e.cookie(controller(), 0)
	bad := map[string]any{"name": "bad name!", "os": "linux"}
	if r := e.do("POST", "/api/admin/hosts", bad, append([]string{"Cookie", c}, csrf...)...); r.code != 400 {
		t.Fatal("invalid name", r.code, string(r.raw))
	}
	r := e.do("POST", "/api/admin/hosts", body, append([]string{"Cookie", c}, csrf...)...)
	if r.code != 201 {
		t.Fatal(r.code, string(r.raw))
	}
	code, cmd := r.body["code"].(string), r.body["command"].(string)
	if _, err := enroll.Normalize(code); err != nil || !strings.Contains(cmd, "-Join "+code+" -PanelAddress 100.88.109.47") ||
		!strings.Contains(cmd, "-PowerControl -SelfUpdate -CoreTemp") || !strings.HasPrefix(cmd, "iwr -useb https://github.com/n3ptune-cpu/Fairy-agent/releases/latest/download/install.ps1") {
		t.Fatal(code, cmd)
	}
	id := r.body["id"].(string)
	var st string
	for i := 0; i < 200 && st != "online"; i++ {
		time.Sleep(20 * time.Millisecond)
		e.s.Poller.PollOnce(context.Background())
		st, _ = e.do("GET", "/api/admin/enrollments/"+id, nil, "Cookie", c).body["state"].(string)
	}
	if st != "online" {
		t.Fatal("state", st)
	}
	want, _ := enroll.Proof(code)
	if proofSeen.Load() != want {
		t.Fatal("the panel must send the code's proof")
	}
	if g := e.do("GET", "/api/systems/nsx-furina", nil, "Cookie", c); g.code != 200 || g.body["status"] != "online" {
		t.Fatal("host not shown", g.code, string(g.raw))
	}
	a, ok := e.s.Poller.AgentByID("nsx-furina")
	if !ok || !a.Managed || !a.PowerControl || !a.SelfUpdate || a.Site != "Neptune" {
		t.Fatalf("%+v", a)
	}
	y, _ := os.ReadFile(filepath.Join(e.s.Cfg.DataDir, "managed-hosts.yaml"))
	if !strings.Contains(string(y), "nsx-furina") || strings.Contains(string(y), "furina-read-token") || strings.Contains(string(y), code) {
		t.Fatal("managed-hosts.yaml", string(y))
	}
	if strings.Contains(string(must(os.ReadFile(e.s.Cfg.AuditLog))), "furina-read-token") || strings.Contains(string(must(os.ReadFile(e.s.Cfg.AuditLog))), code) {
		t.Fatal("secret in the audit log")
	}
	page := e.do("GET", "/api/admin/hosts", nil, "Cookie", c)
	if m := page.body["managed"].([]any); len(m) != 1 || m[0].(map[string]any)["online"] != true {
		t.Fatal(string(page.raw))
	}
	// a second Add host with the same name is refused
	if r := e.do("POST", "/api/admin/hosts", body, append([]string{"Cookie", c}, csrf...)...); r.code != 400 {
		t.Fatal("duplicate", r.code, string(r.raw))
	}
	// config.yaml's own hosts cannot be removed or re-paired here
	if r := e.do("DELETE", "/api/admin/hosts/laptop", nil, append([]string{"Cookie", c}, csrf...)...); r.code != 404 {
		t.Fatal("remove config host", r.code)
	}
	// Re-pair and Remove need the host's name typed (checked by the panel)
	if r := e.do("POST", "/api/admin/hosts/nsx-furina/repair", map[string]any{}, append([]string{"Cookie", c}, csrf...)...); r.code != 400 {
		t.Fatal("repair without the typed name", r.code)
	}
	if r := e.do("DELETE", "/api/admin/hosts/nsx-furina", map[string]any{"confirm": "nsx-fontaine"}, append([]string{"Cookie", c}, csrf...)...); r.code != 400 {
		t.Fatal("remove with a wrong name", r.code)
	}
	if r := e.do("POST", "/api/admin/hosts/nsx-furina/repair", map[string]any{"confirm": "nsx-furina"}, append([]string{"Cookie", c}, csrf...)...); r.code != 201 ||
		!strings.Contains(r.body["command"].(string), "-Join ") || r.body["code"] == code {
		t.Fatal("repair", r.code, string(r.raw))
	} else if !e.s.Enroll.Cancel(r.body["id"].(string)) {
		t.Fatal("cancel")
	}
	if r := e.do("DELETE", "/api/admin/hosts/nsx-furina", map[string]any{"confirm": " NSX-FURINA "}, append([]string{"Cookie", c}, csrf...)...); r.code != 200 {
		t.Fatal("remove", r.code, string(r.raw))
	}
	e.s.Poller.PollOnce(context.Background())
	if g := e.do("GET", "/api/systems/nsx-furina", nil, "Cookie", c); g.code != 404 {
		t.Fatal("still shown after Remove", g.code)
	}
	if _, err := os.Stat(filepath.Join(e.s.Cfg.DataDir, "secrets", "agent-nsx-furina.json")); !os.IsNotExist(err) {
		t.Fatal("tokens kept after Remove", err)
	}
	for _, want := range []string{"host.add join code made", "host.pair online", "host.remove refused: 400: type the host", "host.repair refused", "host.remove ok"} {
		if !strings.Contains(auditOutcomes(e.s.Cfg.AuditLog), want) {
			t.Errorf("audit lacks %q in %s", want, auditOutcomes(e.s.Cfg.AuditLog))
		}
	}
	n := e.do("GET", "/api/notifications?type=enroll", nil, "Cookie", c)
	if evs := n.body["events"].([]any); len(evs) < 2 {
		t.Fatal("notifications", string(n.raw))
	}
	if v := e.do("GET", "/api/notifications?type=enroll", nil, "Cookie", e.cookie(viewer, 0)); v.code == 200 && len(v.body["events"].([]any)) != 0 {
		t.Fatal("viewer sees admin-only events", string(v.raw))
	}
}

// The Add host dialog shows the panel's own address and where it came from; an IPv6-only detection is shown but
// cannot make a command (agents pair over Tailscale IPv4).
func TestAddHostPanelAddressSource(t *testing.T) {
	e := setup(t)
	e.s.control = newRateLimiter(100)
	e.s.Enroll.PanelAddr, e.s.Enroll.PanelSource, e.s.Enroll.PanelVia = "100.88.109.47", "auto", "Tailscale LocalAPI"
	c := e.cookie(controller(), 0)
	pg := e.do("GET", "/api/admin/hosts", nil, "Cookie", c).body
	if pg["panel_address"] != "100.88.109.47" || pg["panel_address_source"] != "auto" || pg["panel_address_via"] != "Tailscale LocalAPI" {
		t.Fatal(pg)
	}
	e.s.Enroll.PanelAddr = "fd7a:115c:a1e0::5"
	body := map[string]any{"name": "NSX-FURINA", "os": "linux"}
	r := e.do("POST", "/api/admin/hosts", body, "Cookie", c, "X-Requested-With", "fairy")
	if r.code != 400 || !strings.Contains(string(r.raw), "IPv4") {
		t.Fatal(r.code, string(r.raw))
	}
}
