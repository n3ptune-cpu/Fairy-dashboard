package web

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// M3, second layer, through the API: Add host for an address outside Tailscale is refused (400, audited, no
// code made) unless config.yaml has enroll: {allow_non_tailscale: true}.
func TestAddHostRefusesAddressesOutsideTailscale(t *testing.T) {
	nobody := func(context.Context, string, string) (int, enroll.Response, string, error) {
		return 0, enroll.Response{}, "", errors.New("dial tcp: connection refused")
	}
	e := setup(t)
	e.s.control = newRateLimiter(100)
	e.s.Enroll.PanelAddr, e.s.Enroll.Pair = "100.88.109.47", nobody
	if e.s.Enroll.AllowNonTailscale {
		t.Fatal("other networks must be off by default")
	}
	hdr := []string{"Cookie", e.cookie(controller(), 0), "X-Requested-With", "fairy"}
	for _, addr := range []string{"192.168.1.50", "10.1.2.3", "127.0.0.1", "::1", "100.128.0.1"} {
		r := e.do("POST", "/api/admin/hosts", map[string]any{"name": "lan-box", "address": addr, "os": "linux"}, hdr...)
		if r.code != 400 || !strings.Contains(string(r.raw), "is not a Tailscale address") || r.body["code"] != nil || r.body["command"] != nil {
			t.Fatal(addr, r.code, string(r.raw))
		}
	}
	if len(e.s.Enroll.List()) != 0 {
		t.Fatal("an enrollment was made")
	}
	if !strings.Contains(auditOutcomes(e.s.Cfg.AuditLog), "host.add refused: 192.168.1.50 is not a Tailscale address") {
		t.Fatal(auditOutcomes(e.s.Cfg.AuditLog))
	}
	if r := e.do("POST", "/api/admin/hosts", map[string]any{"name": "ts-box", "address": "100.101.102.103", "os": "linux"}, hdr...); r.code != 201 {
		t.Fatal(r.code, string(r.raw))
	}

	// the switch in config.yaml
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://cyrene")
	cfg.Enroll.AllowNonTailscale = true
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, testutil.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, p, nil, "test")
	s.Enroll.Scripts, s.Enroll.PanelAddr, s.Enroll.Pair = testInstallers, "100.88.109.47", nobody
	s.control = newRateLimiter(100)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	e2 := &env{t: t, s: s, srv: srv}
	if !s.Enroll.AllowNonTailscale {
		t.Fatal("enroll.allow_non_tailscale did not reach Add host")
	}
	r := e2.do("POST", "/api/admin/hosts", map[string]any{"name": "lan-box", "address": "192.168.1.50", "os": "linux"},
		"Cookie", e2.cookie(controller(), 0), "X-Requested-With", "fairy")
	if r.code != 201 || r.body["url"] != "http://192.168.1.50:9101" {
		t.Fatal(r.code, string(r.raw))
	}
	s.Enroll.Cancel(r.body["id"].(string))
}
