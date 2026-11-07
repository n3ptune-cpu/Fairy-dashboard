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
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Forwarded-For", "garbage")
	if got := ClientIP(r, nets); got != "127.0.0.1" {
		t.Fatal(got)
	}
}
