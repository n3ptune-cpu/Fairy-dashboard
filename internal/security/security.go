// Package security resolves the client IP behind the reverse proxy (for rate limiting and the audit log only).
package security

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

func trusted(addr string, nets []netip.Prefix) bool {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// PeerIP is the TCP peer.
func PeerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String()
	}
	return host
}

// hopAddr parses one X-Forwarded-For entry: "203.0.113.7", "203.0.113.7:4711", "[2001:db8::1]:443" or
// "2001:db8::1" (optionally quoted).
func hopAddr(h string) (netip.Addr, bool) {
	h = strings.Trim(strings.TrimSpace(h), `"`)
	if ip, err := netip.ParseAddr(h); err == nil {
		return ip.Unmap(), true
	}
	if ap, err := netip.ParseAddrPort(h); err == nil {
		return ap.Addr().Unmap(), true
	}
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		if ip, err := netip.ParseAddr(h[1 : len(h)-1]); err == nil {
			return ip.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

// ClientIP returns the client's address. X-Forwarded-For is only believed when the peer is a configured trusted
// proxy (nginx), and then the right-most address that is not itself a trusted proxy wins (entries a client
// sent itself are further left and never chosen while nginx appends or, as recommended, overwrites the
// header). An entry that cannot be parsed stops the walk: the trusted peer is used. Never used for
// authorisation.
//
// Recommended nginx (behind Cloudflare): real_ip_header CF-Connecting-IP with set_real_ip_from <Cloudflare
// ranges>, then proxy_set_header X-Forwarded-For $remote_addr (overwrite, do not append); trusted_proxies
// lists nginx's address. See deploy/nginx.example.conf.
func ClientIP(r *http.Request, nets []netip.Prefix) string {
	peer := PeerIP(r)
	if !trusted(peer, nets) {
		return peer
	}
	var hops []string
	for _, h := range strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",") {
		if h = strings.TrimSpace(h); h != "" {
			hops = append(hops, h)
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		ip, ok := hopAddr(hops[i])
		if !ok {
			return peer
		}
		if !trusted(ip.String(), nets) {
			return ip.String()
		}
	}
	return peer
}
