package web

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/enrollment"
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
	var keySeen atomic.Value
	attempts := atomic.Int32{}
	e.s.Enroll.Pair = func(_ context.Context, url string, key []byte) (int, enroll.Response, string, error) {
		keySeen.Store(string(key))
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
	// the Windows line: the script is checked and run from memory, the code is set in the window's environment
	// (on no command line), the fingerprint and the pairing v2 option are passed
	if _, err := enroll.Normalize(code); err != nil || !strings.Contains(cmd, "; $env:FAIRY_JOIN_CODE='"+code+"'; & (") ||
		!strings.HasSuffix(cmd, " -ReleaseUrl $u -SumsSha256 "+testInstallers.Sums+" -PanelAddress 100.88.109.47 -PairV2 -PowerControl -SelfUpdate -CoreTemp") ||
		!strings.HasPrefix(cmd, "$u='https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-7'; ") ||
		!strings.Contains(cmd, " -ne '"+testInstallers.PS1+"') { throw ") || strings.Count(cmd, code) != 1 ||
		strings.Contains(cmd, "-Join") || strings.Contains(cmd, "$env:TEMP") || strings.Contains(cmd, "latest") {
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
	// the panel pairs with the pairing v2 key of the code it showed, and with nothing else
	want, _ := enroll.PairingKey(code)
	if keySeen.Load() != string(want) {
		t.Fatal("the panel must pair with the pairing v2 key of the code")
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
	e.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) { // nobody ran the new command
		return 0, enroll.Response{}, "", errors.New("dial tcp: connection refused")
	}
	if r := e.do("POST", "/api/admin/hosts/nsx-furina/repair", map[string]any{"confirm": "nsx-furina"}, append([]string{"Cookie", c}, csrf...)...); r.code != 201 ||
		!strings.Contains(r.body["command"].(string), "$env:FAIRY_JOIN_CODE='"+r.body["code"].(string)+"'") || r.body["code"] == code {
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
	if pg["agent_release"] != "build-7 (fairy-agent 7-6155fe6)" {
		t.Fatal("release shown in the dialog:", pg["agent_release"])
	}
	// a panel build without the release key cannot check the install script: no code, no command (503)
	e.s.Enroll.PanelAddr = "100.88.109.47"
	e.s.Enroll.Scripts = enrollment.PinnedVerifier("n3ptune-cpu/Fairy-agent", "", nil)
	r = e.do("POST", "/api/admin/hosts", body, "Cookie", c, "X-Requested-With", "fairy")
	if r.code != 503 || !strings.Contains(string(r.raw), "release signing key") || r.body["command"] != nil || len(e.s.Enroll.List()) != 0 {
		t.Fatal(r.code, string(r.raw))
	}
	if !strings.Contains(auditOutcomes(e.s.Cfg.AuditLog), "host.add refused: cannot check the Fairy-agent install script") {
		t.Fatal(auditOutcomes(e.s.Cfg.AuditLog))
	}
}

// Add host for a new Proxmox VE node (agent v0.4.2 --proxmox): the command asks the node for its read-only
// token, the panel keeps the token secret (0600) and the node's CA, polls the node through the Proxmox API at
// once, attaches the agent to it, and Remove takes both away and says how to delete the token on the node.
func TestAddHostProxmoxNode(t *testing.T) {
	e := setup(t)
	e.s.Enroll.Interval = 20 * time.Millisecond
	e.s.control = newRateLimiter(100)
	e.s.Enroll.PanelAddr = "100.88.109.47"
	ca := testCA(t)
	e.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
		return 200, enroll.Response{AgentVersion: "43-abc", Platform: "linux", Token: "node-read-token-xxxxxxxxxxxxxxxxxxxx",
			ControlToken: "node-control-token-yyyyyyyyyyyyyyyyy", Proxmox: &enroll.ProxmoxGrant{TokenID: "fairy@pve!panel-nsx-kachina",
				TokenSecret: "0b7c4e2a-1111-2222-3333-444455556666", CAPEM: string(ca), Node: "NSX-KACHINA",
				URL: "https://nsx-kachina.tailnet.ts.net:8006", ServerName: "NSX-KACHINA"}}, "", nil
	}
	e.b.Mu.Lock()
	e.b.Metrics["nsx-kachina-2"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	e.b.Mu.Unlock()
	c := e.cookie(controller(), 0)
	csrf := []string{"X-Requested-With", "fairy"}
	bad := map[string]any{"name": "nsx-kachina-2", "os": "windows", "proxmox_new": true}
	if r := e.do("POST", "/api/admin/hosts", bad, append([]string{"Cookie", c}, csrf...)...); r.code != 400 {
		t.Fatal("a Proxmox node is Linux", r.code)
	}
	body := map[string]any{"name": "nsx-kachina-2", "os": "linux", "site": "Neptune", "proxmox_new": true, "options": map[string]any{"power_control": true}}
	r := e.do("POST", "/api/admin/hosts", body, append([]string{"Cookie", c}, csrf...)...)
	if r.code != 201 {
		t.Fatal(r.code, string(r.raw))
	}
	cmd := r.body["command"].(string)
	if !strings.Contains(cmd, `echo "`+testInstallers.SH+`  $f" | sha256sum -c - && printf '%s' '`+r.body["code"].(string)+
		`' | env FAIRY_RELEASE_URL=https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-7 sh "$f" --sums-sha256 `+testInstallers.Sums+` --join-stdin --pair-v2 --panel `) ||
		!strings.HasSuffix(cmd, " --power-control --proxmox") || strings.Contains(cmd, "--join "+r.body["code"].(string)) {
		t.Fatal(cmd)
	}
	id := r.body["id"].(string)
	var st string
	for i := 0; i < 200 && st != "online"; i++ {
		time.Sleep(20 * time.Millisecond)
		e.s.Poller.PollOnce(context.Background())
		st, _ = e.do("GET", "/api/admin/enrollments/"+id, nil, "Cookie", c).body["state"].(string)
	}
	if st != "online" {
		t.Fatal("state", st, e.do("GET", "/api/admin/enrollments/"+id, nil, "Cookie", c).body["message"])
	}
	var pc *config.Proxmox
	for _, x := range e.s.Poller.Clusters() {
		if x.Managed {
			x := x
			pc = &x
		}
	}
	if pc == nil || pc.ID != "neptune-nsx-kachina" || pc.Prefix() != "neptune-nsx-kachina-" || pc.TokenID != "fairy@pve!panel-nsx-kachina" ||
		pc.URLs[0] != "https://nsx-kachina-2:8006" || pc.TLSServerName != "NSX-KACHINA" || !pc.Standalone || pc.Site != "Neptune" {
		t.Fatalf("%+v", pc)
	}
	if b, _ := os.ReadFile(pc.CAFile); string(b) != string(ca) {
		t.Fatal("CA not kept", pc.CAFile)
	}
	a, _ := e.s.Poller.AgentByID("nsx-kachina-2")
	if a.LinksTo != "neptune-nsx-kachina-node-nsx-kachina" {
		t.Fatalf("agent not attached to its node: %+v", a)
	}
	if g := e.do("GET", "/api/systems/neptune-nsx-kachina-node-nsx-kachina", nil, "Cookie", c); g.code != 200 || g.body["kind"] != "node" {
		t.Fatal("node not polled", g.code, string(g.raw))
	}
	sec := filepath.Join(e.s.Cfg.DataDir, "secrets", "agent-nsx-kachina-2.json")
	sb, _ := os.ReadFile(sec)
	if fi, _ := os.Stat(sec); !strings.Contains(string(sb), "0b7c4e2a-1111-2222-3333-444455556666") || fi.Mode().Perm() != 0o600 {
		t.Fatal("token secret", string(sb))
	}
	y, _ := os.ReadFile(filepath.Join(e.s.Cfg.DataDir, "managed-hosts.yaml"))
	if strings.Contains(string(y), "0b7c4e2a") || !strings.Contains(string(y), "fairy@pve!panel-nsx-kachina") {
		t.Fatal("managed-hosts.yaml", string(y))
	}
	// Remove: node and agent gone, CA and secret deleted, the token command shown
	rm := e.do("DELETE", "/api/admin/hosts/nsx-kachina-2", map[string]any{"confirm": "nsx-kachina-2"}, append([]string{"Cookie", c}, csrf...)...)
	if rm.code != 200 || !strings.Contains(rm.body["message"].(string), "pveum user token remove fairy@pve panel-nsx-kachina") {
		t.Fatal(rm.code, string(rm.raw))
	}
	for _, x := range e.s.Poller.Clusters() {
		if x.Managed {
			t.Fatal("cluster kept", x.ID)
		}
	}
	if _, err := os.Stat(pc.CAFile); !os.IsNotExist(err) {
		t.Fatal("CA kept", err)
	}
	if g := e.do("GET", "/api/systems/neptune-nsx-kachina-node-nsx-kachina", nil, "Cookie", c); g.code != 404 {
		t.Fatal("node still shown", g.code)
	}
}

func testCA(t *testing.T) []byte {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Proxmox Virtual Environment"}, IsCA: true,
		BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
