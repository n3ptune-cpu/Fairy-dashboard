package enrollment

import "net"

var tailnet = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// DetectPanelAddress is this machine's Tailscale IPv4 address: the only 100.64.0.0/10 address of an interface
// that is up, preferring one on an interface named like "tailscale". "" when there is none or several.
func DetectPanelAddress() string {
	ifs, _ := net.Interfaces()
	var all, named []string
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil || !tailnet.Contains(n.IP) {
				continue
			}
			all = append(all, n.IP.String())
			if len(i.Name) >= 9 && i.Name[:9] == "tailscale" {
				named = append(named, n.IP.String())
			}
		}
	}
	switch {
	case len(named) == 1:
		return named[0]
	case len(all) == 1:
		return all[0]
	}
	return ""
}
