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

// ClientIP returns the client's address. X-Forwarded-For is only believed when the peer is a configured trusted
// proxy (nginx), and then the right-most address that is not itself a trusted proxy wins. Never used for
// authorisation.
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
		if !trusted(hops[i], nets) {
			if ip, err := netip.ParseAddr(hops[i]); err == nil {
				return ip.Unmap().String()
			}
			return peer
		}
	}
	return peer
}
