package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
)

// oldUsersJSON is a users.json as the release before the email_verified field wrote it: Alice is the admin.
const oldUsersJSON = `{
 "assign": {},
 "removed": {},
 "sites": [],
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
   "role": "pending",
   "sites": [],
   "sub": "sub-s",
   "via": "first sign-in"
  }
 }
}
`

type cliEnv struct {
	t    *testing.T
	dir  string
	cfg  string
	st   *access.Store
	data string
}

func newCLI(t *testing.T) *cliEnv {
	t.Helper()
	dir := t.TempDir()
	e := &cliEnv{t: t, dir: dir, cfg: filepath.Join(dir, "config.yaml"), data: filepath.Join(dir, "data")}
	_ = os.WriteFile(e.cfg, []byte("public_url: https://p.example\ndata_dir: "+e.data+"\noidc: {issuer: https://id.example, client_id: c}\n"), 0o600)
	e.st = access.NewStore(e.data)
	t.Cleanup(func() { stdin = os.Stdin })
	return e
}

// run runs `fairy admin <args>` with answer as what the operator types ("" = no terminal: end of input).
func (e *cliEnv) run(answer string, args ...string) (int, string) {
	stdin = strings.NewReader(answer)
	var out, errb bytes.Buffer
	code := Main(append([]string{"--config", e.cfg, "--env-file", e.dir + "/none.env"}, args...), &out, &errb)
	return code, out.String() + errb.String()
}

func (e *cliEnv) seen(sub, email string, verified bool, name string) {
	e.t.Helper()
	if _, _, err := e.st.SeenIdentity(access.Identity{Sub: sub, Email: email, EmailVerified: verified, Name: name},
		access.RolePending, "first sign-in"); err != nil {
		e.t.Fatal(err)
	}
}

// role reads users.json afresh (a Store caches for a second; the CLI wrote through its own).
func (e *cliEnv) role(sub string) string { return access.NewStore(e.data).Load().Users[sub].Role }

// L4: `fairy admin users set-role <email> admin` matched whatever account claimed that address and did not say
// which. An impostor who put the owner's address on their own account at the identity provider was promoted
// when the owner had not signed in yet.
func TestSetRoleByEmailNeedsAVerifiedAddress(t *testing.T) {
	e := newCLI(t)
	// only the impostor's account exists, claiming the owner's address without verification
	e.seen("sub-impostor", "owner@example.com", false, "Owner")
	code, out := e.run("yes\n", "users", "set-role", "owner@example.com", "admin")
	if code != 1 || e.role("sub-impostor") != access.RolePending {
		t.Fatal("an unverified address gave a role", code, out)
	}
	for _, w := range []string{"1 account(s) have the e-mail owner@example.com", "subject sub-impostor", "owner@example.com (unverified)",
		"users set-role <subject> admin", "error: no user has that email verified"} {
		if !strings.Contains(out, w) {
			t.Fatalf("missing %q in\n%s", w, out)
		}
	}
	if code, out = e.run("", "users", "sites", "owner@example.com", "-"); code != 1 || e.role("sub-impostor") != access.RolePending {
		t.Fatal("users sites by an unverified address", code, out)
	}
	if code, out = e.run("", "users", "remove", "owner@example.com"); code != 1 || e.role("sub-impostor") == "" {
		t.Fatal("removed by an unverified address", code, out)
	}
	if code, out = e.run("", "users", "list"); code != 0 || !strings.Contains(out, "owner@example.com (unverified)") ||
		!strings.Contains(out, "name such an account by its SUBJECT") {
		t.Fatal(code, out)
	}

	// both present: the owner signs in with the address verified. It finds the owner; the impostor is named
	// as not changed.
	e.seen("sub-owner", "Owner@Example.com", true, "Owner")
	code, out = e.run("yes\n", "users", "set-role", "owner@example.com", "admin")
	if code != 0 || e.role("sub-owner") != access.RoleAdmin || e.role("sub-impostor") != access.RolePending {
		t.Fatal(code, out)
	}
	for _, w := range []string{"Account: subject sub-owner, e-mail Owner@Example.com, name Owner, pending (first sign-in)",
		"Note: 1 other account(s) claim this e-mail without verification and are not changed:", "  subject sub-impostor, e-mail owner@example.com (unverified)",
		"Make this account an admin?", "pending -> admin"} {
		if !strings.Contains(out, w) {
			t.Fatalf("missing %q in\n%s", w, out)
		}
	}
	if code, out = e.run("", "users", "list"); code != 0 || strings.Contains(out, "Owner@Example.com (unverified)") {
		t.Fatal(code, out)
	}
	// the subject always works, also for an account with an unverified address: its line says so
	code, out = e.run("", "users", "set-role", "sub-impostor", "viewer")
	if code != 0 || e.role("sub-impostor") != access.RoleViewer || !strings.Contains(out, "Account: subject sub-impostor, e-mail owner@example.com (unverified)") {
		t.Fatal(code, out)
	}
	// two accounts with the same verified address: refused, both listed
	e.seen("sub-twin", "owner@example.com", true, "Twin")
	code, out = e.run("yes\n", "users", "set-role", "owner@example.com", "pending")
	if code != 1 || !strings.Contains(out, "3 account(s) have the e-mail") || !strings.Contains(out, "subject sub-twin") ||
		!strings.Contains(out, "several users have that email") {
		t.Fatal(code, out)
	}
	// the audit entry names the subject that was changed
	entries, _ := audit.Read(filepath.Join(e.data, "audit.log"), "", "user.set_role", "", nil, 10)
	if len(entries) != 2 || entries[1]["params"].(map[string]any)["sub"] != "sub-owner" || entries[0]["params"].(map[string]any)["sub"] != "sub-impostor" {
		t.Fatal(entries)
	}
}

// Making an admin asks first; only yes goes on. Other roles and --yes do not ask.
func TestMakingAnAdminAsksFirst(t *testing.T) {
	e := newCLI(t)
	e.seen("sub-a", "a@example.com", true, "A")
	for _, answer := range []string{"", "\n", "no\n", "maybe\n", "yes please\n"} {
		code, out := e.run(answer, "users", "set-role", "a@example.com", "admin")
		if code != 1 || e.role("sub-a") != access.RolePending || !strings.Contains(out, "Make this account an admin?") ||
			!strings.Contains(out, "Nothing changed") || !strings.Contains(out, "--yes") {
			t.Fatalf("answer %q: %d\n%s", answer, code, out)
		}
		if strings.Count(out, "\n") < 2 || !strings.Contains(out, "Account: subject sub-a") {
			t.Fatalf("answer %q: the account must be printed before the question:\n%s", answer, out)
		}
	}
	if entries, _ := audit.Read(filepath.Join(e.data, "audit.log"), "", "user.set_role", "", nil, 10); len(entries) != 0 {
		t.Fatal("a refused question is not a role change", entries)
	}
	for _, answer := range []string{"yes\n", "Y\n", " YES \r\n", "yes"} {
		if _, _, err := e.st.SetRole("sub-a", access.RolePending, nil, "test", true); err != nil {
			t.Fatal(err)
		}
		if code, out := e.run(answer, "users", "set-role", "a@example.com", "admin"); code != 0 || e.role("sub-a") != access.RoleAdmin {
			t.Fatalf("answer %q: %d\n%s", answer, code, out)
		}
	}
	// already an admin: nothing to ask
	if code, out := e.run("", "users", "set-role", "sub-a", "admin"); code != 0 || strings.Contains(out, "Make this account") {
		t.Fatal(code, out)
	}
	e.seen("sub-b", "b@example.com", true, "B")
	for _, args := range [][]string{{"users", "set-role", "sub-b", "viewer"}, {"users", "set-role", "sub-b", "pending"},
		{"users", "set-role", "sub-b", "admin", "--yes"}, {"users", "set-role", "--yes", "b@example.com", "admin"}} {
		if code, out := e.run("", args...); code != 0 || strings.Contains(out, "Make this account") || !strings.Contains(out, "Account: subject sub-b") {
			t.Fatal(args, code, out)
		}
	}
	if e.role("sub-b") != access.RoleAdmin {
		t.Fatal(e.role("sub-b"))
	}
}

// Upgrade in place: a users.json from before the field loads, the admin stays the admin, and the owner can
// still recover with the CLI. The old address is "not known to be verified", so the CLI says to use the subject.
func TestOldUsersFileAndTheCLI(t *testing.T) {
	e := newCLI(t)
	if err := os.MkdirAll(e.data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.data, "users.json"), []byte(oldUsersJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := e.run("", "users", "list")
	if code != 0 || !strings.Contains(out, "alice@example.com (unverified)") || !strings.Contains(out, "admin") ||
		strings.Contains(out, "there is no admin") {
		t.Fatal(code, out)
	}
	if e.role("sub-n") != access.RoleAdmin {
		t.Fatal("the admin of an old file must stay admin")
	}
	code, out = e.run("yes\n", "users", "set-role", "bob@example.com", "admin")
	if code != 1 || !strings.Contains(out, "subject sub-s") || !strings.Contains(out, "users set-role <subject> admin") {
		t.Fatal(code, out)
	}
	if code, out = e.run("yes\n", "users", "set-role", "sub-s", "admin"); code != 0 || e.role("sub-s") != access.RoleAdmin {
		t.Fatal("recovery by subject", code, out)
	}
	// after the next sign-in the address is verified and works again
	e.seen("sub-n", "alice@example.com", true, "Alice")
	if code, out = e.run("", "users", "set-role", "alice@example.com", "viewer"); code != 0 || e.role("sub-n") != access.RoleViewer {
		t.Fatal(code, out)
	}
	if code, out = e.run("", "users", "remove", "sub-s"); code != 1 || !strings.Contains(out, "last admin") {
		t.Fatal("the last admin is still protected", code, out)
	}
}

// A corrupt users.json is reported as such, not as "no such user".
func TestSetRoleReportsACorruptUsersFile(t *testing.T) {
	e := newCLI(t)
	if err := os.MkdirAll(e.data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.data, "users.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := e.run("yes\n", "users", "set-role", "sub-n", "admin")
	if code == 0 || strings.Contains(out, "no such user") {
		t.Fatal(code, out)
	}
}
