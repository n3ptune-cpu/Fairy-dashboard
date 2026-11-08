package security

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientIPTrustsOnlyConfiguredProxies(t *testing.T) {
	nets := []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("100.64.0.0/10")}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9, 100.64.0.2")
	if got := ClientIP(r, nets); got != "203.0.113.9" { // right-most non-proxy hop
		t.Fatal(got)
	}
	r.RemoteAddr = "198.51.100.1:5555" // untrusted peer: XFF ignored
	if got := ClientIP(r, nets); got != "198.51.100.1" {
		t.Fatal(got)
	}
	// nginx in its own container (10.0.0.5) overwriting X-Forwarded-For with Cloudflare's CF-Connecting-IP
	nginx := []netip.Prefix{netip.MustParsePrefix("10.0.0.5/32")}
	r.RemoteAddr = "10.0.0.5:40000"
	for xff, want := range map[string]string{
		"203.0.113.7":            "203.0.113.7",
		"203.0.113.7:4711":       "203.0.113.7",
		"[2001:db8::7]:443":      "2001:db8::7",
		"2001:db8::7":            "2001:db8::7",
		"1.1.1.1, 203.0.113.7":   "203.0.113.7", // a client-sent entry left of nginx's is ignored
		"203.0.113.7, 10.0.0.5":  "203.0.113.7", // trusted hops are skipped
		"::ffff:203.0.113.7":     "203.0.113.7",
		"203.0.113.7, not-an-ip": "10.0.0.5",
	} {
		r.Header.Set("X-Forwarded-For", xff)
		if got := ClientIP(r, nginx); got != want {
			t.Errorf("XFF %q: %s, want %s", xff, got, want)
		}
	}
	r.RemoteAddr = "10.0.0.6:1" // another LAN host: not trusted, its XFF is ignored
	if got := ClientIP(r, nginx); got != "10.0.0.6" {
		t.Fatal(got)
	}
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Forwarded-For", "garbage")
	if got := ClientIP(r, nets); got != "127.0.0.1" {
		t.Fatal(got)
	}
}
