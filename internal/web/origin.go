package web

import (
	"net"
	"net/url"
	"strings"
)

// Comparing the panel's own address (public_url) with what a browser sends.
//
// A browser leaves the scheme's own port out of Host and Origin: for https://panel.example.com:443 it sends
// "Host: panel.example.com" and "Origin: https://panel.example.com". The panel compared these with public_url
// as text. With `public_url: https://panel.example.com:443` (which the config check accepts) every write was
// refused as "cross-origin", and a demo or test build started on port 80 sent every page to its own address
// again, for ever (r3-p1-07). All such comparisons go through the two functions here, which fill in the
// scheme's port (80 for http, 443 for https) on both sides and take the host name without regard to case.
// Nothing else is equal: another port, another scheme and another name stay another origin.

// defaultPort is the port a scheme has when an address names none ("" for a scheme that is not http or https).
func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// originKey is what two addresses must share to be the same origin: scheme, host name and port, with the
// scheme's own port filled in. ok is false for anything that is not an http(s) address with a host.
func originKey(raw string) (key string, ok bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || defaultPort(u.Scheme) == "" || u.User != nil {
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = defaultPort(u.Scheme)
	}
	return strings.ToLower(u.Scheme) + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port), true
}

// sameOrigin reports whether two addresses (an Origin header, public_url) are the same origin.
func sameOrigin(a, b string) bool {
	ka, ok1 := originKey(a)
	kb, ok2 := originKey(b)
	return ok1 && ok2 && ka == kb
}

// hostIsOf reports whether a request's Host header names the address base (public_url): the same host name,
// and the same port once base's scheme has filled in a port the header leaves out.
func hostIsOf(host, base string) bool {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" {
		return false
	}
	return sameOrigin(u.Scheme+"://"+host, base)
}
