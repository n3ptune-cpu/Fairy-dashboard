package web

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

var (
	tagRe      = regexp.MustCompile(`<(script|link)\b[^>]*>`)
	attrRe     = regexp.MustCompile(`\b(src|href|integrity|crossorigin|rel)="([^"]*)"`)
	absURLRe   = regexp.MustCompile(`https?://[^\s"'<>)]+`)
	pinnedRe   = regexp.MustCompile(`^https://cdn\.jsdelivr\.net/npm/swagger-ui-dist@\d+\.\d+\.\d+/[a-z-]+\.(js|css)$`)
	inlineRe   = regexp.MustCompile(`<script>([^<]*)</script>`)
	cspPartsRe = regexp.MustCompile(`\s*;\s*`)
)

func cspDirectives(t *testing.T, policy string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, d := range cspPartsRe.Split(strings.TrimSpace(policy), -1) {
		f := strings.Fields(d)
		if len(f) == 0 {
			continue
		}
		if _, dup := out[f[0]]; dup {
			t.Fatalf("directive %s twice in %q", f[0], policy)
		}
		out[f[0]] = f[1:]
	}
	return out
}

// L14: /docs loaded Swagger UI from a CDN without an integrity hash and was the one page without a script
// policy. Now both files carry the SHA-384 of the pinned release, the page has its own policy that allows
// exactly those two files and its one inline line, and it is served to users with a role only.
func TestDocsPageIsPinnedAndHasItsOwnPolicy(t *testing.T) {
	e := setup(t)
	admin := e.cookie(controller(), 0)
	r := e.do("GET", "/docs", nil, "Cookie", admin)
	if r.code != 200 || !strings.HasPrefix(r.hdr.Get("Content-Type"), "text/html") {
		t.Fatal(r.code, r.hdr)
	}
	page := string(r.raw)

	// every script and stylesheet: an exact release on the CDN, a SHA-384, and crossorigin (needed for the check)
	var srcs []string
	for _, tag := range tagRe.FindAllString(page, -1) {
		a := map[string]string{}
		for _, m := range attrRe.FindAllStringSubmatch(tag, -1) {
			a[m[1]] = m[2]
		}
		u := a["src"] + a["href"]
		if u == "" { // the inline script
			continue
		}
		srcs = append(srcs, u)
		if !pinnedRe.MatchString(u) {
			t.Errorf("not pinned to one release: %s", tag)
		}
		sum, ok := strings.CutPrefix(a["integrity"], "sha384-")
		if raw, err := base64.StdEncoding.DecodeString(sum); !ok || err != nil || len(raw) != sha512.Size384 {
			t.Errorf("no SHA-384 integrity hash: %s", tag)
		}
		if a["crossorigin"] != "anonymous" {
			t.Errorf("crossorigin=\"anonymous\" missing (the browser cannot check the hash without it): %s", tag)
		}
	}
	if len(srcs) != 2 || srcs[0] != swaggerCSS || srcs[1] != swaggerJS {
		t.Fatal("the page must load exactly the two pinned files", srcs)
	}
	for _, u := range absURLRe.FindAllString(page, -1) { // nothing else from anywhere
		if u != swaggerCSS && u != swaggerJS {
			t.Errorf("another outside address in the page: %s", u)
		}
	}
	if strings.Count(page, "<script") != 2 || strings.Contains(strings.ToLower(page), "javascript:") || regexp.MustCompile(`\son\w+=`).MatchString(page) {
		t.Fatal("unexpected script in the page", page)
	}

	// the policy of exactly this page
	policy := r.hdr.Get("Content-Security-Policy")
	if policy != docsCSP || len(r.hdr.Values("Content-Security-Policy")) != 1 {
		t.Fatal(r.hdr.Values("Content-Security-Policy"))
	}
	d := cspDirectives(t, policy)
	inline := inlineRe.FindStringSubmatch(page)
	if inline == nil {
		t.Fatal("inline script not found")
	}
	sum := sha256.Sum256([]byte(inline[1]))
	wantScript := []string{swaggerJS, "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"}
	if got := d["script-src"]; len(got) != 2 || got[0] != wantScript[0] || got[1] != wantScript[1] {
		t.Fatalf("script-src %v, want exactly %v", got, wantScript)
	}
	if got := d["default-src"]; len(got) != 1 || got[0] != "'none'" {
		t.Fatal("default-src", got)
	}
	if got := d["connect-src"]; len(got) != 1 || got[0] != "'self'" {
		t.Fatal("connect-src", got)
	}
	if got := d["style-src"]; len(got) != 2 || got[0] != swaggerCSS {
		t.Fatal("style-src", got)
	}
	for _, name := range []string{"base-uri", "form-action", "frame-ancestors"} {
		if got := d[name]; len(got) != 1 || got[0] != "'none'" {
			t.Fatal(name, got)
		}
	}
	for name, vals := range d {
		for _, v := range vals {
			if v == "*" || v == "https:" || v == "http:" || v == "'unsafe-eval'" || v == "'strict-dynamic'" ||
				(v == "'unsafe-inline'" && name != "style-src") || (strings.Contains(v, "://") && v != swaggerJS && v != swaggerCSS) {
				t.Errorf("%s allows %s", name, v)
			}
		}
	}
	if !strings.Contains(inline[1], "validatorUrl: null") {
		t.Fatal("the validator badge would send the panel's address to validator.swagger.io")
	}
	if r.hdr.Get("X-Frame-Options") != "DENY" || r.hdr.Get("X-Content-Type-Options") != "nosniff" || r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatal(r.hdr)
	}

	// every other page keeps the panel's own policy: nothing from other hosts
	for _, p := range []string{"/", "/api/openapi.json", "/api/me"} {
		if got := e.do("GET", p, nil, "Cookie", admin).hdr.Get("Content-Security-Policy"); got != CSP {
			t.Errorf("%s: policy %q", p, got)
		}
	}
	if strings.Contains(CSP, "jsdelivr") {
		t.Fatal("the CDN must be allowed on /docs only")
	}
}

// Who gets the page: a browser session of a user with a role. Not a user waiting for access (they get no data
// anywhere else either), not an API token, not a visitor.
func TestDocsPageNeedsARole(t *testing.T) {
	e := setup(t)
	if r := e.do("GET", "/docs", nil); r.code != 303 || r.hdr.Get("Location") != "/auth/login?next=/docs" ||
		r.hdr.Get("Content-Security-Policy") != CSP {
		t.Fatal("visitor", r.code, r.hdr)
	}
	pending := e.cookie(viewer, 0)
	r := e.do("GET", "/docs", nil, "Cookie", pending)
	if r.code != 403 || r.hdr.Get("Content-Security-Policy") != CSP || strings.Contains(string(r.raw), "jsdelivr") ||
		strings.Contains(string(r.raw), "<script") || !strings.Contains(string(r.raw), "Waiting for access") {
		t.Fatal("a user waiting for access must not get the page", r.code, r.hdr.Get("Content-Security-Policy"), string(r.raw))
	}
	if r := e.do("GET", "/api/openapi.json", nil, "Cookie", pending); r.code != 403 {
		t.Fatal("openapi.json for a pending user", r.code)
	}
	if _, _, err := e.s.Access.SetRole(viewer.Sub, access.RoleViewer, []string{}, "test", true); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/docs", nil, "Cookie", pending); r.code != 200 || r.hdr.Get("Content-Security-Policy") != docsCSP {
		t.Fatal("a viewer gets the page", r.code)
	}
	raw, _, err := e.s.Tokens.Create("reader", "system", "read", "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/docs", nil, "Authorization", "Bearer "+raw); r.code != 403 || strings.Contains(string(r.raw), "jsdelivr") ||
		r.hdr.Get("Content-Security-Policy") != CSP {
		t.Fatal("API token", r.code, string(r.raw))
	}
	if r := e.do("GET", "/docs", nil, "Authorization", "Bearer fy_bogus"); r.code != 403 || strings.Contains(string(r.raw), "jsdelivr") {
		t.Fatal("bogus token", r.code, string(r.raw))
	}
	if r := e.do("GET", "/docs", nil, "Authorization", "Bearer "+raw, "Cookie", e.cookie(controller(), 0)); r.code != 403 {
		t.Fatal("a token next to a session", r.code)
	}
}

// enable_docs is off by default: then there is no /docs and no openapi.json at all.
func TestDocsAreOffByDefault(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://cyrene")
	cfg.EnableDocs = false
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, testutil.NewBackend())
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	s := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, p, nil, "test")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e := &env{t: t, s: s, srv: srv}
	c := e.cookie(controller(), 0)
	for _, path := range []string{"/docs", "/api/openapi.json"} {
		if r := e.do("GET", path, nil, "Cookie", c); r.code != 404 || strings.Contains(string(r.raw), "jsdelivr") {
			t.Fatal(path, r.code, string(r.raw))
		}
	}
	def, err := config.Parse([]byte("public_url: https://p.example\noidc: {issuer: https://id.example, client_id: c}\n"))
	if err != nil || def.EnableDocs {
		t.Fatal("enable_docs must default to off", err)
	}
}

// The hashes in docs.go against the files the CDN serves today. Needs the network, so it only runs when asked:
//
//	FAIRY_TEST_CDN=1 go test ./internal/web -run TestSwaggerHashesMatchTheCDN -count=1
func TestSwaggerHashesMatchTheCDN(t *testing.T) {
	if os.Getenv("FAIRY_TEST_CDN") == "" {
		t.Skip("set FAIRY_TEST_CDN=1 to download the two pinned files and compare their SHA-384")
	}
	for u, want := range map[string]string{swaggerCSS: swaggerCSSHash, swaggerJS: swaggerJSHash} {
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			t.Fatal(u, resp.StatusCode, err)
		}
		sum := sha512.Sum384(body)
		if got := "sha384-" + base64.StdEncoding.EncodeToString(sum[:]); got != want {
			t.Errorf("%s: the CDN serves %s (%d bytes), docs.go pins %s", u, got, len(body), want)
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: no Access-Control-Allow-Origin: the browser could not check the hash", u)
		}
	}
}
