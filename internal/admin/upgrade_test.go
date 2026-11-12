package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Upgrade in place: the break-glass CLI against the files of the release before the security fixes (the
// "v-before" fixture: an old users.json without email_verified, old sessions, tokens and audit log, and a
// config.yaml without the new keys). Whatever else goes wrong after an upgrade, `fairy admin` on the server must
// still be able to look at these files and to make the owner admin again.
func TestBreakGlassOnTheOldReleasesFiles(t *testing.T) {
	v := testutil.NewVBefore(t)
	// fairy.env, for `config check`: a random value, as a real install has (the fixture signs its cookies with
	// one character repeated, which `config check` and `fairy serve` refuse: T-05)
	t.Setenv("FAIRY_SESSION_SECRET", "b0pV3m2x9QmJt8sYw1Zr5uKc7NfHd4Ea6LgTi0Oq2WsXy3Rv8BnMk1Jh5GfDc7Vb")
	t.Cleanup(func() { stdin = os.Stdin })
	run := func(answer string, args ...string) (int, string) {
		stdin = strings.NewReader(answer)
		var out, errb bytes.Buffer
		code := Main(append([]string{"--config", v.ConfigPath, "--env-file", filepath.Join(t.TempDir(), "none.env")}, args...), &out, &errb)
		return code, out.String() + errb.String()
	}
	st := access.NewStore(v.DataDir)
	role := func(sub string) string {
		t.Helper()
		if err := st.Err(); err != nil {
			t.Fatal(err)
		}
		return access.NewStore(v.DataDir).Load().Users[sub].Role
	}
	const owner, viewer, waiting = "3f6c1d2e-owner-0001", "9a1b77c0-bob-0002", "5d2e9f10-carol-0003"
	if v.OwnerSub != owner || v.ViewerSub != viewer {
		t.Fatal("fixture changed")
	}

	// looking: everything the old release wrote can be read
	code, out := run("", "users", "list")
	if code != 0 || !strings.Contains(out, owner) || !strings.Contains(out, "owner@example.com (unverified)") ||
		!strings.Contains(out, "bootstrap_admins") || !strings.Contains(out, "name such an account by its SUBJECT") ||
		strings.Contains(out, "there is no admin") {
		t.Fatalf("users list: %d\n%s", code, out)
	}
	// the old release's three sessions ended with the upgrade (NEW-6): they are not listed as sessions
	if code, out := run("", "sessions", "list"); code != 0 || strings.Contains(out, "@example.com") || !strings.Contains(out, "No active sessions") {
		t.Fatalf("sessions list: %d\n%s", code, out)
	}
	if code, out := run("", "tokens", "list"); code != 0 || !strings.Contains(out, "home-assistant") || !strings.Contains(out, "bob-script") {
		t.Fatalf("tokens list: %d\n%s", code, out)
	}
	if code, out := run("", "sites", "list"); code != 0 || !strings.Contains(out, "Bob") || !strings.Contains(out, "power:desk-plug") {
		t.Fatalf("sites list: %d\n%s", code, out)
	}
	if code, out := run("", "audit", "tail"); code != 0 || !strings.Contains(out, "user.first_sign_in") || !strings.Contains(out, "control.enable") {
		t.Fatalf("audit tail: %d\n%s", code, out)
	}
	if code, out := run("", "config", "check"); code != 0 || !strings.Contains(out, "valid") || !strings.Contains(out, "admins in users.json: 1") {
		t.Fatalf("config check on the old config.yaml: %d\n%s", code, out)
	} else if !strings.Contains(out, "ok    control_rate_per_min: 6 control requests a minute for one admin (set in config.yaml; without the line it is 20)") {
		// the old example file set 6: a config copied from it keeps 6 after the update, and the check says so
		t.Fatalf("config check must say that the old file keeps its limit of 6:\n%s", out)
	}

	// locked out: the owner is not admin any more (here: demoted with --force). The way back in:
	if code, out := run("", "users", "set-role", owner, "pending", "--force"); code != 0 || role(owner) != access.RolePending {
		t.Fatal(code, out)
	}
	// 1. By e-mail the old file's account is NOT found (nobody recorded that the address is verified), and the
	// refusal names the account's subject and the command to use.
	code, out = run("yes\n", "users", "set-role", "owner@example.com", "admin", "--yes")
	if code != 1 || role(owner) != access.RolePending || !strings.Contains(out, "subject "+owner) ||
		!strings.Contains(out, "set-role <subject> admin") {
		t.Fatalf("by e-mail on an old file: %d\n%s", code, out)
	}
	// 2. By subject, without a terminal and without --yes: nothing changes, and the output says what to add.
	code, out = run("", "users", "set-role", owner, "admin")
	if code != 1 || role(owner) != access.RolePending || !strings.Contains(out, "Account: subject "+owner) || !strings.Contains(out, "--yes") {
		t.Fatalf("no answer: %d\n%s", code, out)
	}
	// 3. By subject, answering the question.
	code, out = run("yes\n", "users", "set-role", owner, "admin")
	if code != 0 || role(owner) != access.RoleAdmin || !strings.Contains(out, "pending -> admin") {
		t.Fatalf("by subject, answered: %d\n%s", code, out)
	}
	// 4. By subject with --yes (a script, or sudo without a terminal).
	if code, out := run("", "users", "set-role", owner, "pending", "--force"); code != 0 {
		t.Fatal(code, out)
	}
	code, out = run("", "users", "set-role", owner, "admin", "--yes")
	if code != 0 || role(owner) != access.RoleAdmin {
		t.Fatalf("by subject with --yes: %d\n%s", code, out)
	}
	// 5. Once the owner has signed in to the new release with an address Pocket ID verified, the e-mail finds
	// the account again.
	if _, _, err := st.SeenIdentity(access.Identity{Sub: owner, Email: "owner@example.com", EmailVerified: true, Name: "Owner"}, access.RolePending, ""); err != nil {
		t.Fatal(err)
	}
	if code, out := run("", "users", "set-role", owner, "pending", "--force"); code != 0 {
		t.Fatal(code, out)
	}
	code, out = run("", "users", "set-role", "Owner@Example.com", "admin", "--yes")
	if code != 0 || role(owner) != access.RoleAdmin {
		t.Fatalf("by verified e-mail: %d\n%s", code, out)
	}

	// the other commands that change the old files
	if code, out := run("", "users", "sites", waiting, "Bob"); code != 0 || role(waiting) != access.RoleViewer {
		t.Fatal(code, out)
	}
	code, out = run("", "users", "remove", viewer)
	if code != 0 || role(viewer) != "" || !strings.Contains(out, "Revoked 1 session(s) and 1 API token(s)") {
		t.Fatalf("remove: %d\n%s", code, out)
	}
	if code, out := run("", "users", "remove", owner); code != 1 || !strings.Contains(out, "last admin") || role(owner) != access.RoleAdmin {
		t.Fatalf("the last admin is still protected: %d\n%s", code, out)
	}
	if code, out := run("", "sessions", "revoke-all"); code != 0 {
		t.Fatal(code, out)
	}
	if code, out := run("", "control", "disable", "--reason", "upgrade"); code != 0 {
		t.Fatal(code, out)
	}
	if code, out := run("", "control", "enable"); code != 0 {
		t.Fatal(code, out)
	}

	// users.json is still a whole file: sites, assignments and the removed list of the old release are there
	d := access.NewStore(v.DataDir).Load()
	if err := access.NewStore(v.DataDir).Err(); err != nil || d.Admins() != 1 || len(d.Removed) != 2 || d.Assign["tower"] != "Bob" || !d.HasSite("Alice") {
		t.Fatalf("%v %+v", err, d)
	}
}
