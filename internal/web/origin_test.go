package web

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// r3-p1-07 (audit 5, round 3): public_url written with the scheme's own port. A browser sends "Host:
// panel.example.com" and "Origin: https://panel.example.com" for https://panel.example.com:443; the panel
// compared them with public_url as text.
func TestTheSchemesOwnPortIsTheSameOrigin(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"https://panel.example.com", "https://panel.example.com:443", true},
		{"https://panel.example.com:443", "https://panel.example.com", true},
		{"http://127.0.0.1", "http://127.0.0.1:80", true},
		{"http://[::1]", "http://[::1]:80", true},
		{"https://Panel.Example.com", "https://panel.example.com/", true},
		{"https://panel.example.com/", "https://panel.example.com", true},
		{"http://127.0.0.1:8090", "http://127.0.0.1:8090", true},
		// everything else stays another origin
		{"https://panel.example.com:444", "https://panel.example.com", false},
		{"https://panel.example.com:80", "https://panel.example.com", false},
		{"http://panel.example.com:443", "https://panel.example.com:443", false},
		{"http://panel.example.com", "https://panel.example.com", false},
		{"https://panel.example.com.evil.example", "https://panel.example.com", false},
		{"https://evil.example", "https://panel.example.com:443", false},
		{"https://user@panel.example.com", "https://panel.example.com", false},
		{"http://127.0.0.1:8091", "http://127.0.0.1:8090", false},
		{"http://localhost:80", "http://127.0.0.1:80", false},
		{"null", "https://panel.example.com", false},
		{"", "", false},
		{"panel.example.com", "https://panel.example.com", false},
		{"ftp://panel.example.com", "ftp://panel.example.com", false},
	} {
		if got := sameOrigin(c.a, c.b); got != c.same {
			t.Errorf("sameOrigin(%q, %q) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
	for _, c := range []struct {
		host, base string
		is         bool
	}{
		{"127.0.0.1", "http://127.0.0.1:80", true}, {"127.0.0.1:80", "http://127.0.0.1", true}, {"panel.example.com", "https://panel.example.com:443", true},
		{"[::1]", "http://[::1]:80", true}, {"127.0.0.1:8090", "http://127.0.0.1:8090", true}, {"LOCALHOST:8090", "http://localhost:8090", true},
		{"localhost", "http://127.0.0.1:80", false}, {"127.0.0.1", "http://127.0.0.1:8090", false}, {"127.0.0.1:443", "http://127.0.0.1", false},
		{"panel.example.com", "http://panel.example.com:443", false}, {"", "http://127.0.0.1", false}, {"a b", "http://127.0.0.1", false},
	} {
		if got := hostIsOf(c.host, c.base); got != c.is {
			t.Errorf("hostIsOf(%q, %q) = %v, want %v", c.host, c.base, got, c.is)
		}
	}
	// one function for every such comparison: nothing in the package compares a Host or an Origin with public_url as text
	for file, bad := range map[string][]string{"guards.go": {"ou.Host != bu.Host"}, "auth.go": {`strings.TrimRight(o, "/") != c.s.Cfg.BaseURL()`},
		"server.go": {"r.Host == u.Host"}, "testmode.go": {"strings.EqualFold(r.Host, u.Host)"}} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, b := range bad {
			if strings.Contains(src, b) {
				t.Errorf("%s compares an address as text again: %s", file, b)
			}
		}
		if !strings.Contains(src, "sameOrigin(") && !strings.Contains(src, "hostIsOf(") {
			t.Errorf("%s does not use the comparison of origin.go", file)
		}
	}
}

// A real panel whose public_url names 443: writes from its own page pass, as does the sign-out; other origins
// are refused as before.
func TestPublicURLWithTheDefaultPort(t *testing.T) {
	e := setup(t)
	e.s.Cfg.PublicURL = "https://panel.example.com:443"
	ck := e.cookie(controller(), 0)
	w := func(origin string) []string {
		return []string{"Cookie", ck, "X-Requested-With", "fairy", "Content-Type", "application/json", "Origin", origin}
	}
	for _, own := range []string{"https://panel.example.com", "https://panel.example.com:443"} {
		if r := e.do("POST", "/api/me/active", nil, w(own)...); r.code != 204 {
			t.Errorf("a write from the panel's own page (Origin %s): %d %s (before: 403 cross-origin request refused)", own, r.code, r.raw)
		}
	}
	for _, other := range []string{"https://panel.example.com:444", "http://panel.example.com", "https://evil.example", "https://panel.example.com.evil.example", "null"} {
		if r := e.do("POST", "/api/me/active", nil, w(other)...); r.code != 403 || r.body["detail"] != "cross-origin request refused" {
			t.Errorf("a write with Origin %s: %d %s", other, r.code, r.raw)
		}
		if r := e.do("POST", "/auth/logout", nil, w(other)...); r.code != 403 {
			t.Errorf("a sign-out with Origin %s: %d", other, r.code)
		}
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 200 {
		t.Fatal("still signed in:", r.code)
	}
	if r := e.do("POST", "/auth/logout", nil, append(w("https://panel.example.com"), "Accept", "application/json")...); r.code != 204 {
		t.Errorf("the sign-out from the panel's own page: %d %s", r.code, r.raw)
	}
}

// The demo started on port 80 (`fairy serve --demo --listen 127.0.0.1:80` makes public_url
// http://127.0.0.1:80): the browser asks with "Host: 127.0.0.1". It was sent to its own address again, for
// ever, and every write was refused.
func TestDemoOnPort80DoesNotRedirectToItself(t *testing.T) {
	e := setup(t)
	u := viewer
	u.Auth = "demo"
	e.s.DemoUser = &u
	e.s.Cfg.PublicURL = "http://127.0.0.1:80"
	ask := func(method, host, path string, hdr ...string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, e.srv.URL+path, nil)
		req.Host = host
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}
	for _, host := range []string{"127.0.0.1", "127.0.0.1:80"} {
		if res := ask("GET", host, "/"); res.StatusCode != 200 {
			t.Errorf("GET / with Host %s: %d to %q (a redirect to its own address never ends)", host, res.StatusCode, res.Header.Get("Location"))
		}
	}
	if res := ask("POST", "127.0.0.1", "/api/me/active", "X-Requested-With", "fairy", "Origin", "http://127.0.0.1"); res.StatusCode != 204 {
		t.Errorf("a write from the demo's own page: %d (before: 403 cross-origin request refused)", res.StatusCode)
	}
	// another loopback name is still sent to the one address, and a write from it is still refused
	if res := ask("GET", "localhost", "/"); res.StatusCode != 307 || res.Header.Get("Location") != "http://127.0.0.1:80/" {
		t.Errorf("GET / as localhost: %d to %q", res.StatusCode, res.Header.Get("Location"))
	}
	if res := ask("POST", "127.0.0.1", "/api/me/active", "X-Requested-With", "fairy", "Origin", "http://localhost"); res.StatusCode != 403 {
		t.Errorf("a write from another origin: %d", res.StatusCode)
	}
}
