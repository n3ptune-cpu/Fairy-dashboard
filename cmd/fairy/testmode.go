//go:build fairytest

package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/panelupdate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/web"
	releasekey "github.com/n3ptune-cpu/Fairy-dashboard/release"
)

// The test build: `go build -tags fairytest ./cmd/fairy` (TESTING-LOCAL.md). A panel for trying pages, Add host
// and control actions on one's own PC against throwaway machines, without Pocket ID: `fairy serve` signs the
// browser in as one built-in admin. A release is built without the tag and has none of this (testmode_off.go).
//
// Because it has no sign-in, it refuses to start unless everything says "a test, on this PC":
//   - it listens on 127.0.0.1, ::1 or localhost only, and public_url is such an address too;
//   - config.yaml has no oidc: block, and neither FAIRY_SESSION_SECRET nor FAIRY_OIDC_CLIENT_SECRET is set;
//   - its data folder is new, empty or one a test build made, with nobody in it but the test admin;
//   - no file it uses is in the real panel's folders (/etc/fairy, /var/lib/fairy, C:\ProgramData\Fairy).
//
// So putting this program in the place of an installed panel does not switch that panel's sign-in off: it
// does not start. Its own files are in the folder fairy-test next to the program.
const (
	testBuild     = true
	testNote      = "\nTEST BUILD (-tags fairytest): sign-in is off. It listens on 127.0.0.1 only. Never expose this."
	testUpdateOff = "this is a test build (-tags fairytest): it never updates itself (`fairy update` and the Update panel " +
		"button are off). Build it again from the source you want to try"
	testAdminOff = "this is a test build (-tags fairytest): it has no `fairy admin`. That command works on the files of " +
		"an installed panel, and a test build keeps to its own folder. Use the page"
	testMarker = "FAIRY-TEST-STATE.txt" // in the data folder: a test build made it
)

//go:embed fairytest-config.yaml
var testConfig []byte

func init() {
	version += " TEST BUILD" // `fairy version`, the Updates page, the start-up log
	if releaseKey == "" {    // Add host checks the Fairy-agent release with the real key, like a release does
		releaseKey = strings.Join(strings.Fields(releasekey.Pub), "")
	}
}

// testDir is where a test build keeps its config and data: the folder fairy-test next to the program.
func testDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "fairy-test"
	}
	return filepath.Join(filepath.Dir(exe), "fairy-test")
}

// testDefaults: no env file and no FAIRY_CONFIG_FILE unless given on the command line. The real panel's
// defaults (/etc/fairy/..., C:\ProgramData\Fairy\...) are never read.
func testDefaults(f *serveFlags) {
	if !f.envFileSet {
		f.envFile = ""
	}
	if f.cfgPath == "" {
		f.cfgPath = filepath.Join(testDir(), "config.yaml")
	}
}

// setupTest is setupPanel for a test build.
func setupTest(f serveFlags) (*panel, int) {
	cfg, secrets, err := testSetup(f, realDirs())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Refusing to start: %v\n(This is a test build, -tags fairytest: it has no sign-in. See TESTING-LOCAL.md.)\n", err)
		return nil, 2
	}
	return buildPanel(cfg, secrets)
}

// realDirs are the folders of an installed panel on this system.
func realDirs() []string {
	if runtime.GOOS == "windows" {
		return []string{`C:\ProgramData\Fairy`}
	}
	return []string{"/etc/fairy", "/var/lib/fairy"}
}

// testSetup reads the config a test build runs with and refuses everything it must not run with (see the top
// of this file). real are the installed panel's folders.
func testSetup(f serveFlags, real []string) (*config.Panel, config.Secrets, error) {
	none := config.Secrets{}
	if f.demo {
		return nil, none, errors.New("a test build has no --demo; build without -tags fairytest for the demo")
	}
	for _, k := range []string{"FAIRY_SESSION_SECRET", "FAIRY_OIDC_CLIENT_SECRET"} {
		if _, set := os.LookupEnv(k); set {
			return nil, none, fmt.Errorf("%s is set: that is a real panel's secret, and a test build has no sign-in to use it for", k)
		}
	}
	raw, err := os.ReadFile(f.cfgPath)
	if errors.Is(err, os.ErrNotExist) && f.cfgPath == filepath.Join(testDir(), "config.yaml") {
		raw = testConfig // first start: the built-in test config, written out so it can be changed
		if err = os.MkdirAll(testDir(), 0o700); err == nil {
			err = os.WriteFile(f.cfgPath, raw, 0o600)
		}
	}
	if err != nil {
		return nil, none, fmt.Errorf("config %s: %w", f.cfgPath, err)
	}
	doc := map[string]any{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, none, fmt.Errorf("config %s: %w", f.cfgPath, err)
	}
	if doc["oidc"] != nil {
		return nil, none, fmt.Errorf("%s has an oidc: block: that is a real panel's sign-in, and a test build has none. "+
			"Start from cmd/fairy/fairytest-config.yaml", f.cfgPath)
	}
	// What the real panel needs from oidc: is filled in here; the issuer is never contacted (.invalid).
	doc["oidc"] = map[string]any{"issuer": "https://fairytest.invalid", "client_id": "fairytest", "control_groups": []string{web.TestGroup}}
	ownURL := doc["public_url"] == nil
	if ownURL {
		doc["public_url"] = "http://127.0.0.1"
	}
	if doc["data_dir"] == nil {
		doc["data_dir"] = filepath.Join(testDir(), "data")
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, none, err
	}
	cfg, err := config.Parse(out)
	if err != nil {
		return nil, none, fmt.Errorf("%s: %w", f.cfgPath, err)
	}
	addr := f.listenAddr(cfg)
	if !loopback(addr) {
		return nil, none, fmt.Errorf("the listen address %s is not 127.0.0.1, ::1 or localhost: a panel without sign-in listens on this PC only", addr)
	}
	if ownURL {
		cfg.PublicURL = "http://" + addr
	} else if u, err := url.Parse(cfg.PublicURL); err != nil || !loopback(net.JoinHostPort(u.Hostname(), "1")) {
		return nil, none, fmt.Errorf("public_url %s is not an address of this PC (127.0.0.1, ::1, localhost): a test build is not put behind a proxy", cfg.PublicURL)
	}
	for _, p := range []string{f.cfgPath, cfg.DataDir, cfg.AuditLog} {
		for _, dir := range real {
			if inside(p, dir) {
				return nil, none, fmt.Errorf("%s is in %s, the folder of an installed panel: a test build keeps its files in %s", p, dir, testDir())
			}
		}
	}
	if err := testState(cfg.DataDir); err != nil {
		return nil, none, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, none, err
	}
	return cfg, config.Secrets{SessionSecret: hex.EncodeToString(secret), InsecureCookies: true,
		ProxmoxTokenSecret: os.Getenv("FAIRY_PROXMOX_TOKEN_SECRET")}, nil
}

// inside reports whether path is dir or below it (Windows: whatever the case of the letters).
func inside(path, dir string) bool {
	a, err1 := filepath.Abs(path)
	b, err2 := filepath.Abs(dir)
	if runtime.GOOS == "windows" {
		a, b = strings.ToLower(a), strings.ToLower(b)
	}
	rel, err3 := filepath.Rel(b, a)
	return err1 == nil && err2 == nil && err3 == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// testState accepts a data folder that does not exist yet or is empty (and marks it), or one a test build
// marked before that still has nobody in it but the test admin. Anything else may be a real panel's state.
func testState(dir string) error {
	marker := filepath.Join(dir, testMarker)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(entries) == 0) {
		if err = os.MkdirAll(dir, 0o700); err == nil {
			err = os.WriteFile(marker, []byte("This folder is the state of a Fairy TEST BUILD (sign-in off). Delete the folder to start again.\n"), 0o600)
		}
		return err
	}
	if err != nil {
		return err
	}
	if _, err := os.Stat(marker); err != nil {
		return fmt.Errorf("the data folder %s holds files and was not made by a test build (no %s): it may be a real panel's "+
			"state. Use a new, empty folder", dir, testMarker)
	}
	users := access.NewStore(dir)
	if err := users.Err(); err != nil {
		return err
	}
	for sub := range users.Load().Users {
		if sub != web.TestUser.Sub {
			return fmt.Errorf("users.json in %s has an account other than %s (%s): that is a real panel's state", dir, web.TestUser.Sub, sub)
		}
	}
	sessions, err := adminstate.New(dir).ListSessions()
	if err != nil {
		return err
	}
	for _, s := range sessions {
		if s.Sub != web.TestUser.Sub {
			return fmt.Errorf("sessions.json in %s has a session of %s, not of %s: that is a real panel's state", dir, s.Sub, web.TestUser.Sub)
		}
	}
	return nil
}

// testServer switches the server to test mode, turns the panel's own update off and says so three times.
func testServer(srv *web.Server, p *panel) {
	srv.EnableTestMode()
	srv.PanelRunner = testRunner{}
	// Without Tailscale between this PC and a test VM, the address agents pair with is this PC's address on
	// their network. Only when the config asks for other networks and names no address.
	if e := p.cfg.Enroll; e.AllowNonTailscale && e.PanelAddress == "" {
		if a := privateIPv4(); a != "" {
			srv.Enroll.PanelAddr, srv.Enroll.PanelSource, srv.Enroll.PanelVia = a, "auto", "test build: this PC's only private address"
		} else {
			slog.Warn("test build: this PC has no private IPv4 address, or more than one: set enroll: {panel_address: ...} for Add host")
		}
	}
	for i := 0; i < 3; i++ {
		slog.Warn("!!! " + web.TestBanner + " User " + web.TestUser.Sub + ", files in " + testDir() + " !!!")
	}
}

// privateIPv4 is this machine's private IPv4 address (10/8, 172.16/12, 192.168/16) when it has exactly one.
func privateIPv4() string {
	addrs, _ := net.InterfaceAddrs()
	found := ""
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.To4() == nil || !n.IP.IsPrivate() {
			continue
		}
		if found != "" {
			return ""
		}
		found = n.IP.String()
	}
	return found
}

// testRunner is the Updates page's "Update panel" in a test build: off.
type testRunner struct{}

func (testRunner) Ready() (bool, string)              { return false, testUpdateOff }
func (testRunner) Start(context.Context) error        { return errors.New(testUpdateOff) }
func (testRunner) Status() (panelupdate.Status, bool) { return panelupdate.Status{}, false }
