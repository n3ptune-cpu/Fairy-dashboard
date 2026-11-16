package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// userinfoOfSize makes the provider's userinfo endpoint answer with n bytes of JSON for the account u1, with
// the user name Juniper and the groups given.
func userinfoOfSize(idp *fakeIDP, n int, groups string) {
	inner := idp.srv.Config.Handler
	idp.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
		if rq.URL.Path != "/userinfo" {
			inner.ServeHTTP(w, rq)
			return
		}
		head, tail := `{"sub":"u1","preferred_username":"Juniper","groups":[`+groups+`],"x":"`, `"}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(head + strings.Repeat("a", n-len(head)-len(tail)) + tail))
	})
}

// The userinfo answer is read up to 1 MiB and no further. The library read whatever came (64 MiB were read and
// accepted), and since the panel asks userinfo for the user name of every sign-in whose ID token has none, that
// is a request of every sign-in. A larger answer is a userinfo request that failed: asked for the user name
// alone, the sign-in goes on without one; asked for the groups, they could not be read and the sign-in is
// refused, as for every other failure.
func TestUserinfoAnswerIsReadUpToOneMiB(t *testing.T) {
	const mib = 1 << 20
	for _, c := range []struct {
		name     string
		size     int
		groups   any // the ID token's (nil: none, userinfo is asked for them)
		code     int
		username string
	}{
		{"exactly 1 MiB, asked for the user name", mib, []string{"viewers"}, 303, "Juniper"},
		{"one byte more, asked for the user name", mib + 1, []string{"viewers"}, 303, ""},
		{"3 MiB, asked for the user name", 3 * mib, []string{"viewers"}, 303, ""},
		{"exactly 1 MiB, asked for the groups", mib, nil, 303, "Juniper"},
		{"one byte more, asked for the groups", mib + 1, nil, 502, ""},
		{"3 MiB, asked for the groups", 3 * mib, nil, 502, ""},
	} {
		logs := captureLog(t)
		e := setup(t)
		idp := newFakeIDP(t)
		userinfoOfSize(idp, c.size, `"viewers"`)
		e.s.Cfg.OIDC.Issuer = idp.srv.URL
		idp.claims = map[string]any{"groups": c.groups, "preferred_username": nil, "email": "n@example.com", "email_verified": true}
		q, rt := idp.login(e, "/auth/login?next=/", "")
		r := e.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt)
		u := e.s.Access.Load().Users["u1"]
		if r.code != c.code || u.Username != c.username {
			t.Errorf("%s: callback %d (want %d), user name %q (want %q)", c.name, r.code, c.code, u.Username, c.username)
		}
		out := logs.take()
		if failed := strings.Contains(out, "OIDC userinfo request failed"); failed != (c.size > mib) {
			t.Errorf("%s: the log says the userinfo request failed: %v\n%s", c.name, failed, clipText(out, 600))
		}
		if c.size > mib && (!strings.Contains(out, "larger than 1 MiB") || strings.Contains(out, "n@example.com") || strings.Contains(out, "aaaa")) {
			t.Errorf("%s: the log line: %s", c.name, clipText(out, 600))
		}
		if c.code == 303 && c.groups != nil {
			if me := e.do("GET", "/api/me", nil, "Cookie", sessionCookie(e, r.hdr)); me.code != 200 || fmt.Sprint(me.body["groups"]) != "[viewers]" {
				t.Errorf("%s: /api/me %d %s", c.name, me.code, me.raw)
			}
		}
	}
}

// When userinfo is asked only because the ID token has no user name, the user name is all that is taken from
// its answer. The name, the display name, the picture and the e-mail address of that answer were taken too
// where the ID token had none: three more shown texts, and an address, from an answer that is not signed. When
// userinfo is asked for the groups, those are taken where the ID token has none, as before.
func TestUserinfoForTheUsernameAloneGivesTheUsernameAlone(t *testing.T) {
	all := map[string]any{"sub": "u1", "preferred_username": "Juniper", "name": "Other Name", "display_name": "D", "picture": "https://example.com/p.png",
		"email": "other@example.com", "email_verified": true, "groups": []string{"controllers", "admins"}}
	// the ID token has groups, and no user name, name, picture or address
	e, _, r := signInWith(t, map[string]any{"groups": []string{"viewers"}, "preferred_username": nil, "email": nil}, all)
	if r.code != 303 {
		t.Fatalf("callback: %d %s", r.code, r.raw)
	}
	if u := e.s.Access.Load().Users["u1"]; u.Username != "Juniper" || u.Name != "" || u.Email != "" || u.Verified() {
		t.Errorf("asked for the user name alone, users.json has: user name %q, name %q, address %q, verified %v", u.Username, u.Name, u.Email, u.Verified())
	}
	me := e.do("GET", "/api/me", nil, "Cookie", sessionCookie(e, r.hdr))
	if me.body["preferred_username"] != "Juniper" || me.body["name"] != nil || me.body["email"] != nil || me.body["picture"] != nil || fmt.Sprint(me.body["groups"]) != "[viewers]" {
		t.Errorf("asked for the user name alone, /api/me: %s", me.raw)
	}
	for _, private := range []string{"Other Name", "other@example.com", "p.png"} {
		if strings.Contains(string(me.raw), private) {
			t.Errorf("asked for the user name alone, /api/me holds %q of the userinfo answer", private)
		}
	}
	// the ID token has no groups: userinfo is asked for them, and what it says about the person is taken where
	// the ID token says nothing, as before (an address from it is never a verified one)
	e, _, r = signInWith(t, map[string]any{"groups": nil, "preferred_username": nil, "email": nil}, map[string]any{"sub": "u1", "preferred_username": "Juniper", "name": "Other Name",
		"picture": "https://example.com/p.png", "email": "other@example.com", "email_verified": true, "groups": []string{"viewers"}})
	if r.code != 303 {
		t.Fatalf("asked for the groups: callback %d %s", r.code, r.raw)
	}
	if u := e.s.Access.Load().Users["u1"]; u.Username != "Juniper" || u.Name != "Other Name" || u.Email != "other@example.com" || u.Verified() {
		t.Errorf("asked for the groups, users.json has: user name %q, name %q, address %q, verified %v", u.Username, u.Name, u.Email, u.Verified())
	}
}

type fixedBody struct{ n int }

func (f fixedBody) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("a", f.n)))}, nil
}

// The client that reads a bounded answer is the panel's own client for its provider (its timeout, its
// transport) with the bound around each answer's body: a body of the bound's size is read whole, one byte more
// is an error after the bound's bytes, and a context without a client gets a plain one.
func TestBoundedAnswerKeepsTheClient(t *testing.T) {
	for _, n := range []int{0, 9, 10, 11, 5000} {
		base := &http.Client{Timeout: 7 * time.Second, Transport: fixedBody{n}}
		c, _ := boundedAnswer(oidc.ClientContext(t.Context(), base), 10).Value(oauth2.HTTPClient).(*http.Client)
		if c == nil || c == base || c.Timeout != 7*time.Second || base.Transport != (fixedBody{n}) {
			t.Fatalf("the client of a bounded answer: %+v (the panel's own: %+v)", c, base)
		}
		resp, err := c.Get("http://provider.invalid/userinfo")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		if n <= 10 && (err != nil || len(body) != n) {
			t.Errorf("a body of %d bytes, bound 10: read %d, %v", n, len(body), err)
		}
		if n > 10 && (!errors.Is(err, errAnswerTooLarge) || len(body) != 10) {
			t.Errorf("a body of %d bytes, bound 10: read %d, %v; want the error after 10 bytes", n, len(body), err)
		}
	}
	if c, _ := boundedAnswer(t.Context(), 10).Value(oauth2.HTTPClient).(*http.Client); c == nil || c.Transport == nil {
		t.Errorf("a context without a client: %+v", c)
	}
}
