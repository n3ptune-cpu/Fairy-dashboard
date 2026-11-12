package web

import (
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// httpIssuer puts the panel's approval issuer at a plain http address that is in order otherwise: it answers
// with its own address as "issuer" and with the keys of the test issuer. Only the scheme is wrong.
func httpIssuer(t *testing.T, a *approvalEnv) string {
	t.Helper()
	var plain *httptest.Server
	b64 := base64.RawURLEncoding.EncodeToString
	plain = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			u := plain.URL
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": u, "authorization_endpoint": u + "/authorize", "token_endpoint": u + "/token",
				"jwks_uri": u + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": a.iss.Kid, "use": "sig", "alg": "RS256",
				"n": b64(a.iss.Key.N.Bytes()), "e": b64(big.NewInt(int64(a.iss.Key.E)).Bytes())}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(plain.Close)
	if !strings.HasPrefix(plain.URL, "http://") {
		t.Fatal(plain.URL)
	}
	a.s.Cfg.Approval = config.Approval{ClientID: testApprovalClient, Issuer: plain.URL, RunApprovalWaitS: 900}
	a.s.SetIDPClient(plain.Client())
	return plain.URL
}

// An approval issuer that is not https (audit 6, r4-p3-09). A machine takes an https issuer and nothing else,
// so with `approval.issuer: http://...` no machine can be switched on. The approval page said "ready" for every
// machine and counted them ("1 machine is ready"), and Turn on then answered that what the machine would be
// told to trust "is not something a machine accepts". Now no machine is ready, with the short reason; Turn on
// says the same and asks nobody for a passkey. (The log at start: TestApprovalWarningsAtStart.)
func TestAnHTTPApprovalIssuerMakesNoMachineReady(t *testing.T) {
	a := newApprovalEnv(t)
	httpIssuer(t, a)

	page := a.do("GET", "/api/admin/approval", nil, "Cookie", a.admin)
	if page.code != 200 || page.body["keys_error"] != nil && page.body["keys_error"] != "" {
		t.Fatalf("the page (its keys must be readable: only the scheme is wrong): %d %s", page.code, page.raw)
	}
	if n, _ := page.body["ready"].(float64); n != 0 {
		t.Errorf("%v machines are ready with an http issuer, want 0", page.body["ready"])
	}
	machines, _ := page.body["machines"].([]any)
	if len(machines) < 2 {
		t.Fatalf("machines: %s", page.raw)
	}
	for _, x := range machines {
		m := x.(map[string]any)
		if m["ready"] != false || m["why"] != "the issuer must be https" {
			t.Errorf("%v: ready %v, why %q; want not ready, \"the issuer must be https\"", m["id"], m["ready"], m["why"])
		}
	}

	// Turn on says the same, asks for no passkey and sends nothing
	r := a.post("/api/admin/approval/turn-on", map[string]any{"hosts": []string{"laptop", "tower"}})
	if _, _, asked := approvalAsked(r); asked {
		t.Fatalf("Turn on asked for a passkey with an issuer no machine accepts: %d %s", r.code, r.raw)
	}
	if r.code != 409 || strings.Contains(string(r.raw), "is not something a machine accepts") || !strings.Contains(string(r.raw), "the issuer must be https") {
		t.Errorf("Turn on: %d %s", r.code, r.raw)
	}
	if n := len(a.laptop.Asked()) + len(a.tower.Asked()); n != 0 {
		t.Fatalf("something was sent to a machine: laptop %q, tower %q", sentPaths(a.laptop.Asked()), sentPaths(a.tower.Asked()))
	}
}

// With an https issuer a machine is ready as before.
func TestAnHTTPSApprovalIssuerIsReady(t *testing.T) {
	a := newApprovalEnv(t)
	page := a.do("GET", "/api/admin/approval", nil, "Cookie", a.admin)
	if n, _ := page.body["ready"].(float64); n != 2 {
		t.Fatalf("ready: %v, want both machines: %s", page.body["ready"], page.raw)
	}
}
