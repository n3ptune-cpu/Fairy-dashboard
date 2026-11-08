package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

var (
	tailnet4 = netip.MustParsePrefix("100.64.0.0/10")       // Tailscale's CGNAT range
	tailnet6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48") // Tailscale's ULA range
)

// TailscaleSocket is where tailscaled serves its LocalAPI on Linux.
const TailscaleSocket = "/var/run/tailscale/tailscaled.sock"

// Iface is one network interface as the detection sees it (a seam for tests).
type Iface struct {
	Name  string
	Up    bool
	Addrs []netip.Addr
}

// Detected is the panel's own Tailscale address and where it was found ("" Addr: not found).
type Detected struct {
	Addr string // e.g. 100.88.109.47
	Via  string // "interface tailscale0", "Tailscale LocalAPI"
}

// Detector finds the panel's Tailscale address: first on the local interfaces, else (tailscaled with
// --tun=userspace-networking has no tailscale0 interface) from the Tailscale LocalAPI.
type Detector struct {
	Interfaces func() ([]Iface, error)
	// LocalAPI returns Self.TailscaleIPs from tailscaled ("GET /localapi/v0/status"); nil skips it.
	LocalAPI func(ctx context.Context) ([]netip.Addr, error)
	Timeout  time.Duration
}

// DefaultDetector reads the real interfaces and tailscaled's LocalAPI socket (2 s timeout).
func DefaultDetector() Detector {
	return Detector{Interfaces: SystemInterfaces, Timeout: 2 * time.Second,
		LocalAPI: func(ctx context.Context) ([]netip.Addr, error) { return LocalAPIAddrs(ctx, TailscaleSocket) }}
}

// DetectPanelAddress is this machine's Tailscale address as found by DefaultDetector.
func DetectPanelAddress() Detected { return DefaultDetector().Detect(context.Background()) }

// Detect returns, in this order: the Tailscale IPv4 of the interfaces (one on an interface named tailscale* wins
// over others; several elsewhere are ambiguous), the IPv4 tailscaled reports, then the same for IPv6
// (fd7a:115c:a1e0::/48) only when there is no Tailscale IPv4 at all. Errors just mean "not found that way".
func (d Detector) Detect(ctx context.Context) Detected {
	var ifs []Iface
	if d.Interfaces != nil {
		ifs, _ = d.Interfaces()
	}
	if a, name := pickAddr(ifs, tailnet4); a != "" {
		return Detected{a, "interface " + name}
	}
	var api []netip.Addr
	if d.LocalAPI != nil {
		t := d.Timeout
		if t <= 0 {
			t = 2 * time.Second
		}
		c, cancel := context.WithTimeout(ctx, t)
		api, _ = d.LocalAPI(c)
		cancel()
	}
	for _, p := range []netip.Prefix{tailnet4, tailnet6} {
		if p == tailnet6 {
			if a, name := pickAddr(ifs, tailnet6); a != "" {
				return Detected{a, "interface " + name}
			}
		}
		for _, a := range api {
			if p.Contains(a.Unmap()) {
				return Detected{a.Unmap().String(), "Tailscale LocalAPI"}
			}
		}
	}
	return Detected{}
}

// pickAddr returns the one address in p on an up interface: the first one on an interface named tailscale*, else
// the only one; "" when there is none, or several and none on a tailscale* interface.
func pickAddr(ifs []Iface, p netip.Prefix) (addr, iface string) {
	var all []string
	var allIf []string
	for _, i := range ifs {
		if !i.Up {
			continue
		}
		for _, a := range i.Addrs {
			a = a.Unmap()
			if !p.Contains(a) {
				continue
			}
			if strings.HasPrefix(i.Name, "tailscale") {
				return a.String(), i.Name
			}
			all, allIf = append(all, a.String()), append(allIf, i.Name)
		}
	}
	if len(all) == 1 {
		return all[0], allIf[0]
	}
	return "", ""
}

// SystemInterfaces lists this machine's interfaces and their addresses.
func SystemInterfaces() ([]Iface, error) {
	nifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Iface, 0, len(nifs))
	for _, ni := range nifs {
		i := Iface{Name: ni.Name, Up: ni.Flags&net.FlagUp != 0}
		addrs, _ := ni.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok {
					i.Addrs = append(i.Addrs, ip.Unmap())
				}
			}
		}
		out = append(out, i)
	}
	return out, nil
}

// LocalAPIAddrs asks tailscaled for this node's Tailscale addresses (GET /localapi/v0/status?peers=false, the
// Self.TailscaleIPs field) over its unix socket. On Linux the socket is 0666 and tailscaled grants every local
// user read-only LocalAPI access (status included), so the panel's own user needs no `tailscale set --operator`.
func LocalAPIAddrs(ctx context.Context, socket string) ([]netip.Addr, error) {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://local-tailscaled.sock/localapi/v0/status?peers=false", nil)
	if err != nil {
		return nil, err
	}
	res, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tailscaled LocalAPI: %s", res.Status)
	}
	return parseStatus(io.LimitReader(res.Body, 4<<20))
}

// parseStatus reads Self.TailscaleIPs from a LocalAPI status reply.
func parseStatus(r io.Reader) ([]netip.Addr, error) {
	var st struct {
		Self *struct {
			TailscaleIPs []string
		}
	}
	if err := json.NewDecoder(r).Decode(&st); err != nil {
		return nil, err
	}
	if st.Self == nil {
		return nil, errors.New("tailscaled LocalAPI: no Self in status")
	}
	var out []netip.Addr
	for _, s := range st.Self.TailscaleIPs {
		if a, err := netip.ParseAddr(s); err == nil {
			out = append(out, a)
		}
	}
	return out, nil
}
