package web

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/audit"
)

// fakeIDP is a minimal OpenID provider (discovery, JWKS, token, userinfo) that signs RS256 ID tokens, so the
// whole Pocket ID login and step-up round trip can be tested without a real identity provider.
type fakeIDP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu            sync.Mutex
	challenge     string         // PKCE code_challenge from the last authorize URL
	nonce         string         // nonce to put into the next ID token
	claims        map[string]any // extra ID-token claims (groups, auth_time, ...)
	userinfo      map[string]any // userinfo response (nil = 404)
	verifierOK    bool           // the last token request carried the right PKCE verifier
	tokenRequests int
}

func newFakeIDP(t *testing.T) *fakeIDP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIDP{t: t, key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		u := f.srv.URL
		writeJSON(w, map[string]any{"issuer": u, "authorization_endpoint": u + "/authorize",
			"token_endpoint": u + "/token", "jwks_uri": u + "/jwks", "userinfo_endpoint": u + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		writeJSON(w, map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.tokenRequests++
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		f.verifierOK = base64.RawURLEncoding.EncodeToString(sum[:]) == f.challenge
		claims := map[string]any{"iss": f.srv.URL, "aud": "cid", "sub": "u1", "nonce": f.nonce,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "email": "n@example.com"}
		for k, v := range f.claims {
			claims[k] = v
		}
		writeJSON(w, map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 60,
			"id_token": f.sign(claims)})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.userinfo == nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, f.userinfo)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeIDP) sign(claims map[string]any) string {
	b64 := base64.RawURLEncoding.EncodeToString
	h, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	p, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(p)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return in + "." + b64(sig)
}

// login starts /auth/login (with the given cookie) and returns the redirect's query and the round-trip cookie.
func (f *fakeIDP) login(e *env, path, cookie string) (url.Values, string) {
	e.t.Helper()
	var hdr []string
	if cookie != "" {
		hdr = []string{"Cookie", cookie}
	}
	r := e.do("GET", path, nil, hdr...)
	loc, err := url.Parse(r.hdr.Get("Location"))
	if r.code != 303 || err != nil || !strings.HasPrefix(loc.String(), f.srv.URL+"/authorize?") {
		e.t.Fatalf("login redirect: %d %q", r.code, r.hdr.Get("Location"))
	}
	q := loc.Query()
	f.mu.Lock()
	f.challenge, f.nonce = q.Get("code_challenge"), q.Get("nonce")
	f.mu.Unlock()
	return q, sessionCookie(e, r.hdr)
}

func sessionCookie(e *env, h http.Header) string {
	for _, c := range h.Values("Set-Cookie") {
		if strings.HasPrefix(c, cookieName+"=") {
			return strings.Split(c, ";")[0]
		}
	}
	e.t.Fatal("no session cookie in", h)
	return ""
}

func TestPocketIDLoginAndStepUpRoundTrip(t *testing.T) {
	e := setup(t)
	idp := newFakeIDP(t)
	e.s.Cfg.OIDC.Issuer = idp.srv.URL
	idp.claims = map[string]any{"groups": []string{"viewers", "controllers"}, "auth_time": time.Now().Unix()}

	// Login: PKCE S256, state and nonce; the callback creates a registered session and redirects to `next`.
	q, rt := idp.login(e, "/auth/login?next=/%23/system/x", "")
	if q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "cid" ||
		q.Get("redirect_uri") != "https://panel.example.com/auth/callback" || q.Get("state") == "" {
		t.Fatal("authorize URL", q)
	}
	if r := e.do("GET", "/auth/callback?code=c&state=wrong", nil, "Cookie", rt); r.code != 400 {
		t.Fatal("state mismatch must fail", r.code)
	}
	r := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
	if r.code != 303 || r.hdr.Get("Location") != "/#/system/x" || !idp.verifierOK {
		t.Fatal("callback", r.code, r.hdr.Get("Location"), idp.verifierOK, string(r.raw))
	}
	sess := sessionCookie(e, r.hdr)
	me := e.do("GET", "/api/me", nil, "Cookie", sess)
	if me.code != 200 || me.body["sub"] != "u1" || me.body["can_control"] != true || me.body["auth"] != "session" {
		t.Fatal("me after login", me.code, string(me.raw))
	}
	if n := len(e.s.Poller.Admin.ListSessions()); n != 1 {
		t.Fatal("session not registered", n)
	}

	// Step-up: max_age=0 forces a fresh Pocket ID login; the same session gets a new auth_time.
	q, rt = idp.login(e, "/auth/login?step_up=1&next=/", sess)
	if q.Get("max_age") != "0" {
		t.Fatal("step-up must send max_age=0", q)
	}
	idp.claims["auth_time"] = time.Now().Add(-10 * time.Minute).Unix() // provider did not re-authenticate
	if r := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt); r.code != 400 {
		t.Fatal("stale auth_time must fail step-up", r.code)
	}
	q, rt = idp.login(e, "/auth/login?step_up=1&next=/", sess)
	idp.claims["auth_time"] = time.Now().Unix()
	r = e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
	if r.code != 303 || len(e.s.Poller.Admin.ListSessions()) != 1 {
		t.Fatal("step-up callback", r.code, string(r.raw))
	}
	if me := e.do("GET", "/api/me", nil, "Cookie", sessionCookie(e, r.hdr)); me.code != 200 || me.body["auth_age_s"].(float64) > 60 {
		t.Fatal("auth age after step-up", string(me.raw))
	}

	// Nonce mismatch and users outside allowed_groups are refused.
	q, rt = idp.login(e, "/auth/login", "")
	idp.mu.Lock()
	idp.nonce = "other"
	idp.mu.Unlock()
	if r := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt); r.code != 400 {
		t.Fatal("nonce mismatch must fail", r.code)
	}
	q, rt = idp.login(e, "/auth/login", "")
	idp.claims["groups"] = []string{"strangers"}
	if r := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt); r.code != 403 {
		t.Fatal("user outside allowed_groups must be refused", r.code)
	}

	// Groups missing from the ID token are taken from userinfo, but only for the same subject.
	delete(idp.claims, "groups")
	idp.userinfo = map[string]any{"sub": "someone-else", "groups": []string{"viewers"}}
	q, rt = idp.login(e, "/auth/login", "")
	if r := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt); r.code != 403 {
		t.Fatal("userinfo for another subject must be ignored", r.code)
	}
	idp.userinfo = map[string]any{"sub": "u1", "groups": []string{"viewers"}}
	q, rt = idp.login(e, "/auth/login", "")
	if r := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt); r.code != 303 {
		t.Fatal("groups from userinfo", r.code, string(r.raw))
	}

	entries := auditOutcomes(e.s.Cfg.AuditLog)
	for _, want := range []string{"auth.login ok", "auth.step_up ok", "auth.login failed: nonce mismatch",
		"auth.login denied: not in an allowed group", "auth.login failed: mismatching_state"} {
		if !strings.Contains(entries, want) {
			t.Errorf("audit log lacks %q", want)
		}
	}
}

// auditOutcomes returns the audit log as "action outcome" lines.
func auditOutcomes(path string) string {
	entries, _ := audit.Read(path, "", "", "", nil, 500)
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%v %v\n", e["action"], e["outcome"])
	}
	return b.String()
}
