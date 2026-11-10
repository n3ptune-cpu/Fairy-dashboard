package enrollment

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// Pairing is only spoken over Tailscale: there the path is encrypted and the addresses are authenticated, so
// nobody can listen in or answer in a machine's place from outside it. Unless config.yaml says otherwise
// (enroll: {allow_non_tailscale: true}), the panel therefore sends a pairing request only to an address in
// Tailscale's ranges. The check is made where the connection is opened, on the address the name resolved to
// at that moment, so a name that points elsewhere (the local network, a stale record) is not contacted at all.

// IsTailscale reports whether a is a Tailscale address (100.64.0.0/10 or fd7a:115c:a1e0::/48).
func IsTailscale(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	return tailnet4.Contains(a) || tailnet6.Contains(a)
}

// NotTailscale is the refusal to open a pairing connection to an address outside Tailscale's ranges.
type NotTailscale struct {
	Addr string // the address the name resolved to (or the one that was typed)
}

func (e *NotTailscale) Error() string {
	return fmt.Sprintf("%s is not a Tailscale address (100.64.0.0/10 or fd7a:115c:a1e0::/48): not contacted", e.Addr)
}

// tailnetOnly is a net.Dialer Control function: it runs for every connection attempt, after the name was
// resolved and before the connection is made, with the address that would be connected to.
func tailnetOnly(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return &NotTailscale{Addr: address}
	}
	if !IsTailscale(ap.Addr()) {
		return &NotTailscale{Addr: ap.Addr().String()}
	}
	return nil
}

// newPairClient is the HTTP client of the pairing requests: never through a proxy (like every agent request),
// never following a redirect (a redirect could point anywhere), one connection per request (so every attempt
// resolves the name again and is checked again), and to Tailscale addresses only unless allowOther.
func newPairClient(allowOther bool) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	if !allowOther {
		d.Control = tailnetOnly
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: nil, DialContext: d.DialContext, DisableKeepAlives: true,
			ResponseHeaderTimeout: 10 * time.Second, MaxResponseHeaderBytes: 16 << 10},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

var (
	pairClientTailnet = newPairClient(false)
	pairClientAny     = newPairClient(true)
)

// literalOutsideTailscale returns the IP address a host URL names when that address is outside Tailscale's
// ranges ("" for a name, which is checked when it is resolved, and for a Tailscale address).
func literalOutsideTailscale(hostURL string) string {
	u, err := url.Parse(hostURL)
	if err != nil {
		return ""
	}
	a, err := netip.ParseAddr(u.Hostname())
	if err != nil || IsTailscale(a) {
		return ""
	}
	return a.String()
}
