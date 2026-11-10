package access

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oldUsersJSON is a users.json exactly as the release before the email_verified field wrote it (generated with
// that code): an admin, a viewer, a removed user, a site and an assignment.
const oldUsersJSON = `{
 "assign": {
  "power:rack": "Bob"
 },
 "removed": {
  "sub-x": {
   "at": "2026-11-10T13:42:32.607569699Z",
   "by": "cli by fairy",
   "label": "x@example.com"
  }
 },
 "sites": [
  "Bob"
 ],
 "users": {
  "sub-n": {
   "created_at": "2026-11-10T13:42:32.605339371Z",
   "email": "alice@example.com",
   "last_seen": "2026-11-10T13:42:32.605339371Z",
   "name": "Alice",
   "role": "admin",
   "sites": [],
   "sub": "sub-n",
   "via": "control_groups"
  },
  "sub-s": {
   "created_at": "2026-11-10T13:42:32.605660873Z",
   "email": "bob@example.com",
   "last_seen": "2026-11-10T13:42:32.605660873Z",
   "name": "Bob",
   "role": "viewer",
   "sites": [
    "Bob"
   ],
   "sub": "sub-s",
   "via": "panel by alice@example.com"
  }
 }
}
`

func verified(sub, email, name string) Identity {
	return Identity{Sub: sub, Email: email, EmailVerified: true, Name: name}
}

func claimed(sub, email, name string) Identity {
	return Identity{Sub: sub, Email: email, Name: name}
}

// L4: an e-mail address finds an account only when the identity provider verified it for that account.
func TestFindByEmailNeedsAVerifiedAddress(t *testing.T) {
	s := NewStore(t.TempDir())
	seen := func(id Identity) User {
		t.Helper()
		u, _, err := s.SeenIdentity(id, RolePending, "first sign-in")
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	// verified: found by the address, in any case
	if u := seen(verified("sub-owner", "Owner@example.com", "Owner")); !u.Verified() || u.EmailVerified == nil || !*u.EmailVerified {
		t.Fatal(u)
	}
	if u, err := s.Load().Find("owner@EXAMPLE.com"); err != nil || u.Sub != "sub-owner" {
		t.Fatal("a verified address finds its account", u, err)
	}
	// unverified only: an impostor's account claims an address nobody has verified
	if u := seen(claimed("sub-impostor", "boss@example.com", "Boss")); u.Verified() || u.EmailVerified == nil || *u.EmailVerified {
		t.Fatal(u)
	}
	if _, err := s.Load().Find("boss@example.com"); status(err) != 409 || !IsEmailNotVerified(err) {
		t.Fatal("an address that is only claimed must not find an account", err)
	}
	if _, _, err := s.SetRole("boss@example.com", RoleAdmin, nil, "test", false); !IsEmailNotVerified(err) {
		t.Fatal("role given by a claimed address", err)
	}
	if _, err := s.Remove("boss@example.com", false, "test"); !IsEmailNotVerified(err) {
		t.Fatal("removed by a claimed address", err)
	}
	if u := s.Load().Users["sub-impostor"]; u.Role != RolePending {
		t.Fatal(u)
	}
	if u, err := s.Load().Find("sub-impostor"); err != nil || u.Email != "boss@example.com" {
		t.Fatal("the subject always finds its account", u, err)
	}
	// both present: the impostor claims the owner's verified address. The verified account is the one found.
	seen(claimed("sub-impostor", "owner@example.com", "Owner"))
	u, err := s.Load().Find("owner@example.com")
	if err != nil || u.Sub != "sub-owner" {
		t.Fatal("the verified account must be the one found", u, err)
	}
	v, c := s.Load().ByEmail("OWNER@example.com")
	if len(v) != 1 || v[0].Sub != "sub-owner" || len(c) != 1 || c[0].Sub != "sub-impostor" {
		t.Fatal(v, c)
	}
	if _, after, err := s.SetRole("owner@example.com", RoleAdmin, nil, "test", false); err != nil || after.Sub != "sub-owner" {
		t.Fatal(after, err)
	}
	if u := s.Load().Users["sub-impostor"]; u.Role != RolePending {
		t.Fatal("the impostor was promoted", u)
	}
	// two accounts with the same verified address: refused, as before
	seen(verified("sub-twin", "owner@example.com", "Twin"))
	if _, err := s.Load().Find("owner@example.com"); status(err) != 409 || IsEmailNotVerified(err) {
		t.Fatal(err)
	}
	// nobody
	if _, err := s.Load().Find("nobody@example.com"); status(err) != 404 {
		t.Fatal(err)
	}
	if _, err := s.Load().Find(""); status(err) != 404 {
		t.Fatal("an empty identifier must not match users without an address", err)
	}
	// labels
	d := s.Load()
	if got := d.Users["sub-owner"].Account(); got != "Owner@example.com (subject sub-owner)" {
		t.Fatal(got)
	}
	if got := d.Users["sub-impostor"].Account(); got != "owner@example.com (e-mail not verified, subject sub-impostor)" {
		t.Fatal(got)
	}
	if got := (User{Sub: "s"}).Account(); got != "subject s" {
		t.Fatal(got)
	}
}

// What a sign-in says replaces what is stored; the old Seen (no knowledge of verification) never makes an
// address verified.
func TestEmailVerifiedFollowsTheSignIn(t *testing.T) {
	s := NewStore(t.TempDir())
	get := func() User { return s.Load().Users["sub-a"] }
	if _, _, err := s.SeenIdentity(verified("sub-a", "a@example.com", "A"), RolePending, "first sign-in"); err != nil || !get().Verified() {
		t.Fatal(err, get())
	}
	// the provider stops vouching for it (or the user changed it to an unverified one)
	_, _, _ = s.SeenIdentity(claimed("sub-a", "boss@example.com", "A"), RolePending, "")
	if u := get(); u.Verified() || u.Email != "boss@example.com" {
		t.Fatal(u)
	}
	_, _, _ = s.SeenIdentity(verified("sub-a", "a@example.com", "A"), RolePending, "")
	// a sign-in without any address keeps the stored one, no longer verified
	_, _, _ = s.SeenIdentity(Identity{Sub: "sub-a", EmailVerified: true, Name: "A"}, RolePending, "")
	if u := get(); u.Verified() || u.Email != "a@example.com" || u.EmailVerified == nil {
		t.Fatal(u)
	}
	_, _, _ = s.SeenIdentity(verified("sub-a", "a@example.com", "A"), RolePending, "")
	// Seen: the same address keeps the stored answer, a new one is "not known"
	_, _, _ = s.Seen("sub-a", "a@example.com", "A", RolePending, "")
	if u := get(); !u.Verified() {
		t.Fatal(u)
	}
	_, _, _ = s.Seen("sub-a", "boss@example.com", "A", RolePending, "")
	if u := get(); u.Verified() || u.EmailVerified != nil {
		t.Fatal(u)
	}
	_, _, _ = s.Seen("sub-b", "b@example.com", "B", RolePending, "first sign-in")
	if u := s.Load().Users["sub-b"]; u.Verified() || u.EmailVerified != nil {
		t.Fatal(u)
	}
	// a claimed address without one stored: "verified" alone is not an address
	_, _, _ = s.SeenIdentity(Identity{Sub: "sub-c", EmailVerified: true}, RolePending, "first sign-in")
	if u := s.Load().Users["sub-c"]; u.Verified() || u.EmailVerified != nil || u.Account() != "subject sub-c" {
		t.Fatal(u)
	}
}

// A users.json written before the field existed keeps loading: roles, sites, assignments and removals are as
// they were, the admin is still the admin, and the addresses count as not verified until the next sign-in.
func TestOldUsersFileStillLoads(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "users.json"), []byte(oldUsersJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	d := s.Load()
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	n, st := d.Users["sub-n"], d.Users["sub-s"]
	if len(d.Users) != 2 || n.Role != RoleAdmin || d.Admins() != 1 || st.Grant().String() != "viewer:Bob" ||
		d.Assign["power:rack"] != "Bob" || d.Removed["sub-x"].Label != "x@example.com" || !d.HasSite("Bob") {
		t.Fatalf("%+v", d)
	}
	if n.EmailVerified != nil || n.Verified() || n.Email != "alice@example.com" {
		t.Fatal("an old entry's address is not known to be verified", n)
	}
	// by address: refused with the reason; by subject: works. Nobody is locked out.
	if _, err := d.Find("alice@example.com"); !IsEmailNotVerified(err) {
		t.Fatal(err)
	}
	if _, _, err := s.SetRole("sub-s", RoleAdmin, nil, "cli by fairy", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetRole("sub-n", RolePending, nil, "cli by fairy", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SetRole("sub-s", RolePending, nil, "cli by fairy", false); status(err) != 409 {
		t.Fatal("the last admin can still not be demoted", err)
	}
	// the write keeps everything and adds the field; the next sign-in fills it in
	if _, _, err := s.SeenIdentity(verified("sub-n", "alice@example.com", "Alice"), RoleAdmin, "control_groups"); err != nil {
		t.Fatal(err)
	}
	d = NewStore(dir).Load()
	if u, err := d.Find("alice@example.com"); err != nil || u.Sub != "sub-n" || u.Role != RolePending || u.Via != "cli by fairy" {
		t.Fatal(u, err)
	}
	if d.Users["sub-s"].EmailVerified != nil || d.Removed["sub-x"].By != "cli by fairy" || d.Assign["power:rack"] != "Bob" {
		t.Fatalf("%+v", d)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "users.json"))
	var file struct {
		Users map[string]map[string]any `json:"users"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.Users["sub-n"]["email_verified"] != true || file.Users["sub-s"]["email_verified"] != nil {
		t.Fatal(string(raw))
	}
	for _, k := range []string{"created_at", "email", "last_seen", "name", "role", "sites", "sub", "via"} { // nothing renamed
		if _, ok := file.Users["sub-s"][k]; !ok {
			t.Fatal("field missing after the upgrade:", k, string(raw))
		}
	}
	// an older panel reads the new file (rollback): unknown fields are ignored by encoding/json
	var old struct {
		Users map[string]struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"users"`
	}
	if err := json.Unmarshal(raw, &old); err != nil || old.Users["sub-s"].Role != RoleAdmin || !strings.Contains(string(raw), `"email_verified"`) {
		t.Fatal(err, old)
	}
}
