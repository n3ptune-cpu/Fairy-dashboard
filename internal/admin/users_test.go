package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/tokens"
)

func TestUsersAndSitesCLI(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfg, []byte("public_url: https://p.example\ndata_dir: "+dir+"/data\noidc: {issuer: https://id.example, client_id: c}\n"+
		"agents: [{id: bob-pc, url: 'http://10.0.0.2:9101', token_env: T, site: Bob}]\n"), 0o600)
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(append([]string{"--config", cfg, "--env-file", dir + "/none.env"}, args...), &out, &errb)
		return code, out.String() + errb.String()
	}
	if code, out := run("users", "list"); code != 0 || !strings.Contains(out, "nobody becomes admin") {
		t.Fatal(code, out)
	}
	st := access.NewStore(dir + "/data")
	// both addresses are verified by the identity provider, so they find their accounts (identity_test.go has the rest)
	_, _, _ = st.SeenIdentity(access.Identity{Sub: "sub-n", Email: "alice@example.com", EmailVerified: true, Name: "Alice"},
		access.RolePending, "first sign-in") // locked out
	_, _, _ = st.SeenIdentity(access.Identity{Sub: "sub-s", Email: "bob@example.com", EmailVerified: true, Name: "Bob"},
		access.RolePending, "first sign-in")
	if code, out := run("users", "set-role", "Alice@Example.com", "admin", "--yes"); code != 0 || !strings.Contains(out, "pending -> admin") {
		t.Fatal("recovery: make yourself admin", code, out)
	}
	if code, out := run("users", "sites", "bob@example.com", "Bob"); code != 0 || !strings.Contains(out, "viewer of Bob") {
		t.Fatal("config sites exist without creating them", code, out)
	}
	if code, out := run("users", "sites", "bob@example.com", "Mars"); code != 1 || !strings.Contains(out, "sites create") {
		t.Fatal("unknown site", code, out)
	}
	if code, out := run("users", "set-role", "sub-n", "viewer", "--sites", "Bob"); code != 1 || !strings.Contains(out, "last admin") {
		t.Fatal("last admin", code, out)
	}
	if code, _ := run("users", "set-role", "nobody@example.com", "admin"); code != 1 {
		t.Fatal("unknown user")
	}
	if code, out := run("sites", "assign", "power:rack-plug", "Alice"); code != 0 || !strings.Contains(out, `Created site "Alice"`) {
		t.Fatal(code, out)
	}
	if code, out := run("sites", "rename", "Bob", "S"); code != 1 || !strings.Contains(out, "config.yaml") {
		t.Fatal("config site renamed", code, out)
	}
	if code, out := run("sites", "rename", "Alice", "Home"); code != 0 {
		t.Fatal(code, out)
	}
	code, out := run("sites", "list")
	if code != 0 || !strings.Contains(out, "power:rack-plug") || !strings.Contains(out, "agent bob-pc") || !strings.Contains(out, "bob@example.com") {
		t.Fatal(code, out)
	}
	if code, out := run("users", "list"); code != 0 || !strings.Contains(out, "viewer") || !strings.Contains(out, "cli by") {
		t.Fatal(out)
	}
	if code, _ := run("tokens", "create", "--name", "ha", "--owner", "sub-s", "--sites", "Bob"); code != 0 {
		t.Fatal("site token")
	}
	if code, _ := run("tokens", "create", "--name", "x", "--owner", "system", "--sites", "Nowhere"); code != 2 {
		t.Fatal("token for unknown site")
	}
	if r, _ := tokens.NewStore(dir + "/data").List(); len(r) != 1 || strings.Join(r[0].Sites, ",") != "Bob" {
		t.Fatal(r)
	}
	if code, out := run("tokens", "list"); !strings.Contains(out, "Bob") || code != 0 {
		t.Fatal(out)
	}
	if code, out := run("users", "remove", "sub-n"); code != 1 || !strings.Contains(out, "--force") {
		t.Fatal("remove last admin", code, out)
	}
	if code, _ := run("users", "remove", "sub-n", "--force"); code != 0 {
		t.Fatal("forced")
	}
	if code, _ := run("sites", "delete", "Home"); code != 0 || st.Load().HasSite("Home") {
		t.Fatal("delete site")
	}
	entries, _ := audit.Read(filepath.Join(dir, "data", "audit.log"), "", "", "", nil, 100)
	var actions []string
	for _, e := range entries {
		if e["source"] != "cli" {
			t.Fatal("source", e)
		}
		actions = append(actions, e["action"].(string))
	}
	for _, want := range []string{"user.set_role", "user.remove", "site.assign", "site.create", "site.rename", "site.delete", "token.create"} {
		if !strings.Contains(strings.Join(actions, " "), want) {
			t.Errorf("%s not audited: %v", want, actions)
		}
	}
}
