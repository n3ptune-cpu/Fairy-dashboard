package enrollment

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
)

// M3, second layer: which addresses a pairing request may go to.
func TestIsTailscale(t *testing.T) {
	for addr, want := range map[string]bool{
		"100.64.0.0":            true,
		"100.88.109.47":         true,
		"100.127.255.255":       true,
		"::ffff:100.64.0.1":     true, // IPv4 written as IPv6
		"fd7a:115c:a1e0::1":     true,
		"fd7a:115c:a1e0:ab12::": true,
		"100.63.255.255":        false,
		"100.128.0.0":           false,
		"fd7a:115c:a1e1::1":     false,
		"fd7a:115c:a1df:ffff::": false,
		"127.0.0.1":             false,
		"::1":                   false,
		"10.0.0.5":              false,
		"172.16.3.4":            false,
		"192.168.1.50":          false,
		"169.254.10.10":         false,
		"fe80::1%eth0":          false,
		"0.0.0.0":               false,
		"::":                    false,
		"8.8.8.8":               false,
		"2001:db8::1":           false,
		"::ffff:192.168.1.50":   false,
	} {
		if got := IsTailscale(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
	if IsTailscale(netip.Addr{}) {
		t.Error("the zero address")
	}
}

func TestTailnetOnlyDialControl(t *testing.T) {
	for _, ok := range []string{"100.88.109.47:9101", "[fd7a:115c:a1e0::5]:9101", "[::ffff:100.64.0.1]:9101"} {
		if err := tailnetOnly("tcp", ok, nil); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"192.168.1.50:9101", "127.0.0.1:9101", "[::1]:9101", "[fe80::1%eth0]:9101", "100.128.0.1:9101", "nsx-furina:9101", "", "100.88.109.47"} {
		var nt *NotTailscale
		if err := tailnetOnly("tcp", bad, nil); !errors.As(err, &nt) {
			t.Errorf("%q was not refused: %v", bad, err)
		}
	}
}

var testKey = []byte(strings.Repeat("k", enroll.KeySize))

// accepts counts the TCP connections a loopback listener gets (the check must stop the connection itself, not
// just the HTTP request).
func accepts(t *testing.T) (addr string, n *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	n = &atomic.Int32{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			c.Close()
		}
	}()
	return ln.Addr().String(), n
}

// The pairing request is not sent, and no connection is opened, to an address outside Tailscale: for an
// address, and for a name at the moment it resolves (localhost stands in for a name that points at the local
// network).
func TestPairingDoesNotConnectOutsideTailscale(t *testing.T) {
	addr, n := accepts(t)
	_, port, _ := net.SplitHostPort(addr)
	for _, host := range []string{"127.0.0.1", "localhost"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		status, resp, code, err := HTTPPair(ctx, "http://"+net.JoinHostPort(host, port), testKey)
		cancel()
		var nt *NotTailscale
		if !errors.As(err, &nt) || status != 0 || code != "" || resp.Token != "" {
			t.Fatalf("%s: %d %q %v", host, status, code, err)
		}
		if a, perr := netip.ParseAddr(nt.Addr); perr != nil || !a.IsLoopback() {
			t.Fatalf("%s: the refusal must name the address the name led to: %q", host, nt.Addr)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n.Load() != 0 {
		t.Fatal("connections were opened:", n.Load())
	}
	// with enroll.allow_non_tailscale the same request goes out
	hit := atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Add(1)
		http.Error(w, `{"error":"x","code":"pairing_inactive"}`, 404)
	}))
	defer srv.Close()
	if status, _, code, err := httpPair(context.Background(), pairClientAny, srv.URL, testKey); err != nil || status != 404 || code != "pairing_inactive" || hit.Load() != 1 {
		t.Fatal(status, code, err, hit.Load())
	}
}

// A redirect is never followed: the answer is just a status the panel does not act on.
func TestPairingDoesNotFollowRedirects(t *testing.T) {
	second := atomic.Int32{}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { second.Add(1) }))
	defer other.Close()
	for _, st := range []int{301, 302, 307, 308} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL+"/pair", st) }))
		status, resp, _, err := httpPair(context.Background(), pairClientAny, srv.URL, testKey)
		srv.Close()
		if err != nil || status != st || resp.Token != "" {
			t.Fatal(st, status, err)
		}
	}
	if second.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

func addrManager(t *testing.T) *Manager {
	t.Helper()
	m := New(managed.NewStore(t.TempDir()), Hooks{
		Config: func() *config.Panel { return testConfig(t) },
		Remove: func(string) {}, Add: func(managed.Entry) error { return nil },
		Online: func(string) (bool, string) { return true, "" }, Event: func(Enrollment, string, string) {},
	}, "100.88.109.47", Fixed(testInstallers))
	m.Interval = 5 * time.Millisecond
	return m
}

// Add host and Re-pair refuse an address outside Tailscale at once, unless config.yaml allows other networks.
// A name is not resolved here: it is checked on every connection.
func TestCreateRefusesAddressesOutsideTailscale(t *testing.T) {
	none := func(string) string { return "" }
	for _, tc := range []struct {
		addr  string
		allow bool
		ok    bool
	}{
		{"192.168.1.50", false, false},
		{"10.0.0.5", false, false},
		{"127.0.0.1", false, false},
		{"100.128.0.1", false, false},
		{"8.8.8.8", false, false},
		{"::1", false, false},
		{"fd7a:115c:a1e1::1", false, false},
		{"100.88.1.2", false, true},
		{"fd7a:115c:a1e0::5", false, true},
		{"nsx-furina.tailnet.ts.net", false, true},
		{"", false, true}, // the name itself
		{"192.168.1.50", true, true},
		{"::1", true, true},
	} {
		m := addrManager(t)
		m.AllowNonTailscale = tc.allow
		m.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
			return 0, enroll.Response{}, "", errors.New("nobody there")
		}
		cr, err := m.Create(context.Background(), Request{Name: "nsx-furina", Address: tc.addr, OS: "linux"}, nil, "neptune", none)
		var v *Validation
		switch {
		case tc.ok && err != nil:
			t.Errorf("%q allow=%v: %v", tc.addr, tc.allow, err)
		case !tc.ok && (!errors.As(err, &v) || !strings.Contains(v.Msg, "is not a Tailscale address") || !strings.Contains(v.Msg, "allow_non_tailscale")):
			t.Errorf("%q allow=%v was not refused: %v", tc.addr, tc.allow, err)
		case !tc.ok && len(m.List()) != 0:
			t.Errorf("%q: an enrollment was made", tc.addr)
		}
		if err == nil {
			m.Cancel(cr.ID)
		}
	}
	// Re-pair of a host that was added at a local-network address before this rule
	m := addrManager(t)
	old := managed.Host{ID: "old-lan", Name: "old-lan", URL: "http://192.168.1.50:9101", OS: "linux"}
	var v *Validation
	if _, err := m.Create(context.Background(), Request{}, &old, "neptune", none); !errors.As(err, &v) || !strings.Contains(v.Msg, "192.168.1.50 is not a Tailscale address") {
		t.Fatal(err)
	}
}

// The real pairing loop against a name that leads outside Tailscale: the dialog says so, nothing is sent,
// and the enrollment keeps waiting (the name may be corrected in DNS; the code is not used up).
func TestPairingLoopSaysWhyItDoesNotConnect(t *testing.T) {
	addr, n := accepts(t)
	_, port, _ := net.SplitHostPort(addr)
	m := addrManager(t)
	old := managed.Host{ID: "box", Name: "box", URL: "http://localhost:" + port, OS: "linux"}
	cr, err := m.Create(context.Background(), Request{}, &old, "neptune", nil)
	if err != nil {
		t.Fatal(err)
	}
	var e Enrollment
	for i := 0; i < 400 && e.Attempts < 3; i++ {
		time.Sleep(5 * time.Millisecond)
		e, _ = m.Get(cr.ID)
	}
	if e.State != Waiting || e.Done || !strings.Contains(e.Message, "not contacted: localhost leads to") || !strings.Contains(e.Message, "not a Tailscale address") {
		t.Fatalf("%+v", e)
	}
	if !m.Cancel(cr.ID) {
		t.Fatal("cancel")
	}
	if n.Load() != 0 {
		t.Fatal("connections were opened:", n.Load())
	}
}
