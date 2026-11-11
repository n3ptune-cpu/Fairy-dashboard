package web

import (
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// T-69: users.json keeps what the identity provider said about a person at their last sign-in: the e-mail
// address and whether the provider verified it (`fairy admin users set-role <address>` finds an account by
// its address only when it is verified). The panel also notes every few minutes that a signed-in user was
// seen, and it did that with everything the session cookie carries, so an older session of the same account
// wrote its older address and "verified" back over what a later sign-in had recorded. Now only a sign-in
// records an identity; being seen moves "last seen" and nothing else.
func TestAnOlderSessionDoesNotWriteItsIdentityBack(t *testing.T) {
	e := setup(t)
	before := model.User{Sub: "u8", Name: model.P("Owner"), Email: model.P("owner@example.com"), EmailVerified: true, Groups: []string{"viewers"}}
	old := e.cookie(before, 0) // the first sign-in: owner@example.com, verified
	rec := func() (string, bool, string, time.Time) {
		u := e.s.Access.Load().Users["u8"]
		return u.Email, u.EmailVerified != nil && *u.EmailVerified, u.Name, model.Deref(u.LastSeen)
	}
	if email, verified, _, _ := rec(); email != "owner@example.com" || !verified {
		t.Fatalf("after the first sign-in: %s verified=%v", email, verified)
	}
	// the provider now says something else about the same account: another address, not verified, another name
	after := before
	after.Email, after.EmailVerified, after.Name = model.P("other@example.com"), false, model.P("Renamed")
	e.signIn(after)
	if email, verified, name, _ := rec(); email != "other@example.com" || verified || name != "Renamed" {
		t.Fatalf("after the second sign-in: %s verified=%v name=%s", email, verified, name)
	}
	_, _, _, seenAt := rec()

	// a request with the OLDER session's cookie, at a moment the panel notes "seen" again
	time.Sleep(5 * time.Millisecond)
	e.s.seenMu.Lock()
	delete(e.s.seen, "u8")
	e.s.seenMu.Unlock()
	if r := e.do("GET", "/api/me", nil, "Cookie", old); r.code != 200 {
		t.Fatal(r.code)
	}
	email, verified, name, seenNow := rec()
	if email != "other@example.com" || verified || name != "Renamed" {
		t.Fatalf("the older session wrote its identity back: %s verified=%v name=%s", email, verified, name)
	}
	if !seenNow.After(seenAt) {
		t.Fatal("last seen did not move")
	}
}
