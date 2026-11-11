//go:build fairytest

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/web"
)

// testCase is one start of a test build: a config file, flags, the environment and what is in its data folder.
type testCase struct {
	t    *testing.T
	dir  string   // the test's own folder: config.yaml and data are in it
	real []string // what counts as an installed panel's folders
	f    serveFlags
}

func newCase(t *testing.T, config string) *testCase {
	t.Helper()
	for _, k := range []string{"FAIRY_SESSION_SECRET", "FAIRY_OIDC_CLIENT_SECRET", "FAIRY_LISTEN", "FAIRY_CONFIG_FILE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	dir := t.TempDir()
	c := &testCase{t: t, dir: dir, real: []string{filepath.Join(dir, "etc-fairy"), filepath.Join(dir, "var-lib-fairy")}}
	c.f.cfgPath = filepath.Join(dir, "config.yaml")
	c.write("config.yaml", "data_dir: "+filepath.ToSlash(c.data())+"\n"+config)
	return c
}

func (c *testCase) data() string { return filepath.Join(c.dir, "data") }

func (c *testCase) write(name, content string) {
	c.t.Helper()
	p := filepath.Join(c.dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		c.t.Fatal(err)
	}
}

// refused: the start is refused, and the reason names what is wrong.
func (c *testCase) refused(why string) {
	c.t.Helper()
	cfg, _, err := testSetup(c.f, c.real)
	if err == nil || cfg != nil || !strings.Contains(err.Error(), why) {
		c.t.Fatalf("want a refusal that says %q, got: %v", why, err)
	}
}

// Rule 2: any listen address but loopback is refused, wherever it comes from.
func TestTestBuildListensOnLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "192.168.1.20:8080", "[::]:8080", "panel.example.com:8080", "100.64.0.10:8080"} {
		c := newCase(t, "")
		c.f.listen = addr
		c.refused("not 127.0.0.1, ::1 or localhost")
		c = newCase(t, "")
		t.Setenv("FAIRY_LISTEN", addr)
		c.refused("not 127.0.0.1, ::1 or localhost")
		newCase(t, "listen: \""+addr+"\"\n").refused("not 127.0.0.1, ::1 or localhost")
	}
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080"} {
		c := newCase(t, "")
		c.f.listen = addr
		cfg, _, err := testSetup(c.f, c.real)
		if err != nil || cfg.PublicURL != "http://"+addr {
			t.Fatalf("%s must be accepted, with the page at that address: %v", addr, err)
		}
	}
	// the real start: nothing is listened on, exit code 2
	c := newCase(t, "")
	if code := serve([]string{"--config", c.f.cfgPath, "--listen", "0.0.0.0:0"}); code != 2 {
		t.Fatal("serve must refuse with exit code 2, got", code)
	}
	newCase(t, "public_url: https://fairy.example.com\n").refused("public_url")
	newCase(t, "public_url: http://192.168.1.20:8080\n").refused("public_url")
}

// Rule 3: a real panel's sign-in settings or secrets.
func TestTestBuildRefusesARealPanelsSignIn(t *testing.T) {
	newCase(t, "oidc: {issuer: \"https://id.example.com\", client_id: fairy}\n").refused("oidc: block")
	newCase(t, "oidc:\n  allowed_groups: [homelab]\n").refused("oidc: block")
	for _, k := range []string{"FAIRY_SESSION_SECRET", "FAIRY_OIDC_CLIENT_SECRET"} {
		c := newCase(t, "")
		t.Setenv(k, strings.Repeat("x", 40)+"y")
		c.refused(k + " is set")
		// also from an env file given on the command line (a real panel's fairy.env)
		c = newCase(t, "")
		c.write("fairy.env", k+"="+strings.Repeat("x", 40)+"y\n")
		c.f.envFile, c.f.envFileSet = filepath.Join(c.dir, "fairy.env"), true
		testDefaults(&c.f)
		if err := c.f.loadEnv(); err != nil {
			t.Fatal(err)
		}
		c.refused(k + " is set")
		os.Unsetenv(k)
	}
	c := newCase(t, "")
	c.f.demo = true
	c.refused("no --demo")
}

// Rule 3: a data folder that may be a real panel's state.
func TestTestBuildRefusesRealState(t *testing.T) {
	// files, but not made by a test build
	c := newCase(t, "")
	c.write("data/users.json", `{"users":{}}`)
	c.refused("was not made by a test build")
	c = newCase(t, "")
	c.write("data/audit.log", "")
	c.refused("was not made by a test build")

	// a test build's folder is taken again, with its own admin and sessions in it
	c = newCase(t, "")
	if _, _, err := testSetup(c.f, c.real); err != nil {
		t.Fatal("a new folder must be accepted:", err)
	}
	if _, err := os.Stat(filepath.Join(c.data(), testMarker)); err != nil {
		t.Fatal("the folder was not marked:", err)
	}
	users, sessions := access.NewStore(c.data()), adminstate.New(c.data())
	if _, _, err := users.SeenIdentity(access.Identity{Sub: web.TestUser.Sub, Name: "Test admin"}, access.RoleAdmin, "test"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.AddSession("sid-of-the-test-admin", adminstate.NewSession{Sub: web.TestUser.Sub}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := testSetup(c.f, c.real); err != nil {
		t.Fatal("a test build's own state must be accepted:", err)
	}
	// ... and the sessions of the earlier start are gone: their cookies were signed with that start's secret, so
	// they could only sit on the Sessions page as browsers that are not there
	if left, err := sessions.ListSessions(); err != nil || len(left) != 0 {
		t.Fatal("the sessions of an earlier start must leave sessions.json:", left, err)
	}
	if u := users.Load().Users; len(u) != 1 || u[web.TestUser.Sub].Role != access.RoleAdmin {
		t.Fatal("the test admin stays:", u)
	}
	// ... but not with anybody else's session or account in it, marked or not
	if err := sessions.AddSession("sid-of-somebody", adminstate.NewSession{Sub: "pocket-id-user-1"}); err != nil {
		t.Fatal(err)
	}
	c.refused("sessions.json")
	if _, err := sessions.RevokeUserSessions("pocket-id-user-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := users.SeenIdentity(access.Identity{Sub: "pocket-id-user-1", Email: "owner@example.com"}, access.RoleAdmin, "test"); err != nil {
		t.Fatal(err)
	}
	c.refused("users.json")
	// a users.json that cannot be read is not taken for an empty one
	c = newCase(t, "")
	c.write("data/"+testMarker, "")
	c.write("data/users.json", "{not json")
	if _, _, err := testSetup(c.f, c.real); err == nil {
		t.Fatal("an unreadable users.json must stop the start")
	}
}

// Rule 3: nothing of an installed panel is used, and the defaults are the test build's own folder.
func TestTestBuildKeepsToItsOwnFolder(t *testing.T) {
	c := newCase(t, "")
	c.write("etc-fairy/config.yaml", "listen: 127.0.0.1:8080\n")
	c.f.cfgPath = filepath.Join(c.dir, "etc-fairy", "config.yaml")
	c.refused("the folder of an installed panel")
	c = newCase(t, "")
	c.write("config.yaml", "data_dir: "+filepath.ToSlash(filepath.Join(c.dir, "var-lib-fairy"))+"\n")
	c.refused("the folder of an installed panel")
	c = newCase(t, "")
	c.write("config.yaml", "data_dir: "+filepath.ToSlash(c.data())+"\naudit_log: "+filepath.ToSlash(filepath.Join(c.dir, "var-lib-fairy", "audit.log"))+"\n")
	c.refused("the folder of an installed panel")
	for _, dir := range realDirs() {
		if !inside(filepath.Join(dir, "config.yaml"), dir) || !inside(dir, dir) || inside(dir+"-test", dir) || inside(filepath.Dir(dir), dir) {
			t.Fatal("inside:", dir)
		}
	}

	f, _ := parseServeFlags(nil)
	testDefaults(&f)
	if f.envFile != "" || f.cfgPath != filepath.Join(testDir(), "config.yaml") || filepath.Base(testDir()) != "fairy-test" {
		t.Fatalf("defaults: env file %q, config %q", f.envFile, f.cfgPath)
	}
	for _, p := range []string{f.cfgPath, testDir()} {
		for _, dir := range realDirs() {
			if inside(p, dir) {
				t.Fatalf("the default %s is in %s", p, dir)
			}
		}
	}
	t.Setenv("FAIRY_CONFIG_FILE", "/etc/fairy/config.yaml") // a real panel's unit sets it: not followed
	f, _ = parseServeFlags(nil)
	testDefaults(&f)
	f.resolve()
	if f.cfgPath != filepath.Join(testDir(), "config.yaml") {
		t.Fatal("FAIRY_CONFIG_FILE must not lead a test build to a real config:", f.cfgPath)
	}
}

// The config a test build starts with (the file in this folder) is accepted as it is, makes the test admin an
// admin, and allows pairing with a test VM that is not on Tailscale.
func TestTestBuildConfig(t *testing.T) {
	c := newCase(t, "")
	c.write("config.yaml", "data_dir: "+filepath.ToSlash(c.data())+"\n"+string(testConfig))
	cfg, secrets, err := testSetup(c.f, c.real)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enroll.AllowNonTailscale || cfg.Listen != "127.0.0.1:8080" || cfg.PublicURL != "http://127.0.0.1:8080" {
		t.Fatalf("test config: %+v listen %s url %s", cfg.Enroll, cfg.Listen, cfg.PublicURL)
	}
	if len(cfg.OIDC.ControlGroups) != 1 || cfg.OIDC.ControlGroups[0] != web.TestGroup || !strings.HasSuffix(cfg.OIDC.Issuer, ".invalid") {
		t.Fatalf("oidc: %+v", cfg.OIDC)
	}
	if len(secrets.SessionSecret) < 32 || !secrets.InsecureCookies || secrets.OIDCClientSecret != "" {
		t.Fatal("the test build makes its own session secret for each start")
	}
	if _, s2, _ := testSetup(c.f, c.real); s2.SessionSecret == secrets.SessionSecret {
		t.Fatal("the session secret must be new at every start")
	}
	for _, word := range []string{"token_env", "password", "_SECRET"} {
		if strings.Contains(string(testConfig), word) {
			t.Errorf("fairytest-config.yaml names a secret (%s)", word)
		}
	}
}

// Rules 4 and 7: the program says what it is, checks releases with the real key, and never updates itself.
func TestTestBuildSaysSoAndDoesNotUpdateItself(t *testing.T) {
	if !testBuild || !strings.HasSuffix(version, "TEST BUILD") || !strings.Contains(testNote, "sign-in is off") {
		t.Fatal("version:", version, testNote)
	}
	pub, err := os.ReadFile("../../release/signing.pub")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := release.ParsePublicKey(releaseKey); err != nil || releaseKey != strings.TrimSpace(string(pub)) {
		t.Fatal("a test build checks releases with release/signing.pub:", err)
	}
	if code := update(nil); code != 1 {
		t.Fatal("fairy update must refuse, got exit code", code)
	}
	if code := update([]string{"--check"}); code != 1 {
		t.Fatal("fairy update --check must refuse too, got exit code", code)
	}
	var r testRunner
	if ready, why := r.Ready(); ready || !strings.Contains(why, "test build") {
		t.Fatal("the Update panel button must be off and say why:", why)
	}
	if err := r.Start(context.Background()); err == nil {
		t.Fatal("the update must not start")
	}
	if _, ran := r.Status(); ran {
		t.Fatal("no update ever ran")
	}
}
