package testutil

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// Issuer is a made-up Pocket ID for tests of the whole round trip: discovery, its signing key, an authorize
// endpoint that approves at once (as if the person had touched the passkey) and a token endpoint with PKCE.
// It serves https, as a machine's trust wants its issuer; Client trusts its certificate. Every token is made
// and signed when the test runs: none is written down anywhere.
type Issuer struct {
	Srv *httptest.Server
	Key *rsa.PrivateKey
	Kid string

	mu sync.Mutex
	// Sub is who signs in. Edit, when set, may change the claims of the next tokens (by client id).
	Sub    string
	Edit   func(clientID string, claims map[string]any)
	Now    func() time.Time
	codes  map[string]issuedCode
	tokens []string // every ID token handed out
	asked  []url.Values
}

type issuedCode struct {
	clientID, nonce, challenge, redirect string
}

// NewIssuer starts the issuer.
func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &Issuer{Key: key, Kid: "issuer-key-1", Sub: "u2", Now: time.Now, codes: map[string]issuedCode{}}
	b64 := base64.RawURLEncoding.EncodeToString
	js := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		u := i.Srv.URL
		js(w, map[string]any{"issuer": u, "authorization_endpoint": u + "/authorize", "token_endpoint": u + "/token",
			"jwks_uri": u + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		i.mu.Lock()
		defer i.mu.Unlock()
		js(w, map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": i.Kid, "use": "sig", "alg": "RS256",
			"n": b64(i.Key.N.Bytes()), "e": b64(big.NewInt(int64(i.Key.E)).Bytes())}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := make([]byte, 18)
		_, _ = rand.Read(code)
		i.mu.Lock()
		i.codes[b64(code)] = issuedCode{q.Get("client_id"), q.Get("nonce"), q.Get("code_challenge"), q.Get("redirect_uri")}
		i.asked = append(i.asked, q)
		i.mu.Unlock()
		back, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || back.Host == "" {
			http.Error(w, "redirect_uri", 400)
			return
		}
		v := back.Query()
		v.Set("code", b64(code))
		v.Set("state", q.Get("state"))
		back.RawQuery = v.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		i.mu.Lock()
		defer i.mu.Unlock()
		c, ok := i.codes[r.PostForm.Get("code")]
		delete(i.codes, r.PostForm.Get("code")) // a code is good once
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || b64(sum[:]) != c.challenge || r.PostForm.Get("redirect_uri") != c.redirect {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		now := i.Now()
		claims := map[string]any{"iss": i.Srv.URL, "aud": c.clientID, "sub": i.Sub, "nonce": c.nonce, "iat": now.Unix(),
			"exp": now.Add(time.Hour).Unix(), "auth_time": now.Unix(), "amr": []string{"phr"}, "preferred_username": "neptune",
			"email": "n@example.com", "groups": []string{"viewers", "controllers", "homelab", "admins"}}
		if i.Edit != nil {
			i.Edit(c.clientID, claims)
		}
		tok := i.sign(claims)
		i.tokens = append(i.tokens, tok)
		js(w, map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 60, "id_token": tok})
	})
	i.Srv = httptest.NewTLSServer(mux)
	t.Cleanup(i.Srv.Close)
	return i
}

func (i *Issuer) sign(claims map[string]any) string {
	b64 := base64.RawURLEncoding.EncodeToString
	h, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": i.Kid, "typ": "JWT"})
	p, _ := json.Marshal(claims)
	in := b64(h) + "." + b64(p)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, i.Key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return in + "." + b64(sig)
}

// URL is the issuer's address (its "iss").
func (i *Issuer) URL() string { return i.Srv.URL }

// Client is an HTTP client that trusts the issuer's certificate and follows no redirect by itself.
func (i *Issuer) Client() *http.Client {
	c := i.Srv.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.Timeout = 10 * time.Second
	return c
}

// Approve plays the person at Pocket ID: it opens the authorization address the panel answered with and
// returns where Pocket ID sends the browser back to (the panel's callback, with code and state).
func (i *Issuer) Approve(t testing.TB, authURL string) *url.URL {
	t.Helper()
	resp, err := i.Client().Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	back, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil {
		t.Fatalf("the issuer's authorize endpoint: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	return back
}

// Tokens are all ID tokens handed out so far (to check that none of them shows up where it must not).
func (i *Issuer) Tokens() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]string(nil), i.tokens...)
}

// Asked are the queries of the authorization requests so far.
func (i *Issuer) Asked() []url.Values {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]url.Values(nil), i.asked...)
}

// Set changes the issuer under its lock (Sub, Edit, Now, Key, Kid).
func (i *Issuer) Set(fn func(i *Issuer)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	fn(i)
}
