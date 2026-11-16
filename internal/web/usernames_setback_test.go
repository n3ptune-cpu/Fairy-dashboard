package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// testdata/build16-setback is a data folder the code of build 16 wrote (generator_test.go.txt is the generator,
// run in the tree of build 16): users.json and notifications.json, kept as <name>.old like the files of
// testutil's v-before. Its accounts are made up: an admin with an e-mail address (sub-owner), an admin without
// one (sub-bare), two admins without one who were set back to "No access (waiting)" afterwards, on the Users
// page (sub-dana) and with `fairy admin users` (sub-cleo), a viewer (sub-vee) and an account that only waits
// (sub-wendy). Each of the four who were admins did everything build 16 wrote a sentence with a person for: 29
// sentences each, 116 events. Build 16 named the first by the address and the other three by their names.
const (
	setbackDir = "testdata/build16-setback"
	setbackDay = "2026-11-16" // the day the files were written
)

const setbackConfig = `
public_url: https://panel.example.com
request_timeout: 1
offline_after: 60
audit_log: %q
data_dir: %q
oidc: {issuer: "https://id.example.com", client_id: cid, allowed_groups: [viewers], control_groups: [controllers]}
proxmox:
  - {id: home, id_prefix: home-, cluster_name: ALPHA, standalone: true, urls: ["https://alpha:8006"],
     token_id: "fairy@pve!panel", token_secret_env: PVE_SECRET, verify_tls: false, site: Home}
agents:
  - {id: home-alpha-agent, name: ALPHA, url: "http://alpha", token_env: TOK_A, control_token_env: CTL_A, node: ALPHA, self_update: true, power_control: true, site: Home}
  - {id: home-deskpc-agent, name: DESKPC, url: "http://deskpc", token_env: TOK_B, control_token_env: CTL_A, self_update: true, power_control: true, site: Home}
`

var (
	setbackOwner = model.User{Sub: "sub-owner", Name: model.P("Olivia Ownerson"), Email: model.P("owner@example.com"), EmailVerified: true, Groups: []string{"viewers", "controllers"}}
	setbackBare  = model.User{Sub: "sub-bare", Name: model.P("Bare Nameson"), Groups: []string{"viewers", "controllers"}}
	setbackDana  = model.User{Sub: "sub-dana", Name: model.P("Dana Demotedson"), Groups: []string{"viewers", "controllers"}}
	setbackCleo  = model.User{Sub: "sub-cleo", Name: model.P("Cleo Commandline"), Groups: []string{"viewers", "controllers"}}
	setbackVee   = model.User{Sub: "sub-vee", Name: model.P("Vera Viewerson"), Email: model.P("vee@example.net"), EmailVerified: true, Groups: []string{"viewers"}}
)

// setbackStored are the sentences of the folder as build 16 stored them, in the order of the file.
func setbackStored(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(setbackDir, "notifications.json.old"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Events []struct {
			Text string `json:"text"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range file.Events {
		out = append(out, e.Text)
	}
	return out
}

// newSetbackEnv is this panel started on a copy of that folder, as after the update from build 16. Only the day
// in the stored times is changed, to yesterday: the Notifications list keeps a month.
func newSetbackEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02")
	for _, name := range []string{"users.json", "notifications.json"} {
		raw, err := os.ReadFile(filepath.Join(setbackDir, name+".old"))
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.ReplaceAll(raw, []byte(`"`+setbackDay+`T`), []byte(`"`+yesterday+`T`))
		if err := os.WriteFile(filepath.Join(data, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TOK_A", testutil.ReadToken+"a")
	t.Setenv("TOK_B", testutil.ReadToken+"b")
	t.Setenv("CTL_A", testutil.ControlToken)
	t.Setenv("PVE_SECRET", "pve-secret-uuid")
	cfg, err := config.Parse([]byte(fmt.Sprintf(setbackConfig, filepath.Join(dir, "audit.log"), data)))
	if err != nil {
		t.Fatal(err)
	}
	b := testutil.NewBackend()
	b.PVEOf = func(string) ([]jsonx.M, error) { return testutil.HostResources("ALPHA", "running"), nil }
	plain := func(host string) func() (jsonx.M, error) {
		return func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			m["hostname"] = host
			delete(m, "power_sources")
			return m, nil
		}
	}
	b.Metrics = map[string]func() (jsonx.M, error){"home-alpha-agent": plain("alpha"), "home-deskpc-agent": plain("deskpc")}
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	s := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, p, nil, "test")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, s: s, srv: srv, b: b}
}

// setbackList is the Notifications list as the owner of a session reads it: the sentences, sorted.
func setbackList(e *env, cookie string) []string {
	e.t.Helper()
	x := e.do("GET", "/api/notifications?limit=200", nil, "Cookie", cookie)
	if x.code != 200 {
		e.t.Fatal(x.code, string(x.raw))
	}
	var out []string
	for _, ev := range x.body["events"].([]any) {
		out = append(out, fmt.Sprint(ev.(map[string]any)["text"]))
	}
	sort.Strings(out)
	return out
}

// An admin who has no e-mail address did things under build 16, which wrote the name the account gives for the
// person into its sentences ("shutdown requested by Dana Demotedson"). Then the account was set back to "No
// access (waiting)". After the update the panel left every account that waits out of the reading of stored
// text, and with that the name of this one was readable again in all of those sentences, for every viewer, in
// the list and on the stream.
//
// An account that was set back is not one that only waits: its name stays hidden in what it wrote while it had
// its role ("(name hidden)", and its user name once the panel knows it), exactly as for the admin without an
// address who still is one. That holds for an account set back on the Users page and for one set back on the
// command line.
func TestANameBuild16WroteStaysHiddenAfterTheAccountWasSetBack(t *testing.T) {
	e := newSetbackEnv(t)
	stored := setbackStored(t)
	if len(stored) != 116 {
		t.Fatalf("the folder has %d sentences", len(stored))
	}
	d := e.s.Access.Load()
	for sub, want := range map[string]string{"sub-owner": "admin via control_groups", "sub-bare": "admin via panel by owner@example.com", "sub-dana": "pending via panel by owner@example.com",
		"sub-cleo": "pending via cli by root", "sub-vee": "viewer via panel by owner@example.com", "sub-wendy": "pending via first sign-in"} {
		if u := d.Users[sub]; u.Role+" via "+u.Via != want || u.Username != "" {
			t.Fatalf("the folder: %s is %q with the user name %q, want %q and none", sub, u.Role+" via "+u.Via, u.Username, want)
		}
	}
	private := []string{"Dana", "Demotedson", "Cleo", "Commandline", "Bare Nameson", "Nameson", "Olivia", "Ownerson", "@"}
	// as reads what build 16 stored with one text for each of the four: the whole list an admin gets, sorted
	as := func(owner, bare, dana, cleo string) []string {
		var out []string
		for _, s := range stored {
			out = append(out, strings.NewReplacer("owner@example.com", owner, "Bare Nameson", bare, "Dana Demotedson", dana, "Cleo Commandline", cleo).Replace(s))
		}
		sort.Strings(out)
		return out
	}
	check := func(phase string, cookies map[string]string, want []string) {
		t.Helper()
		for who, ck := range cookies {
			got := setbackList(e, ck)
			for _, s := range got {
				for _, p := range private {
					if strings.Contains(s, p) {
						t.Errorf("%s: %s reads %q", phase, who, s)
					}
				}
			}
			switch who {
			case "the admin": // every sentence, with exactly these words for the four
				if strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf("%s: the admin reads %d sentences, and not the %d expected ones; the first that differs:", phase, len(got), len(want))
					for i := range min(len(got), len(want)) {
						if got[i] != want[i] {
							t.Errorf("  got  %q\n  want %q", got[i], want[i])
							break
						}
					}
				}
			default: // a viewer of the site: all but the four sentences about a run that have no machine
				if len(got) != 112 {
					t.Errorf("%s: %s reads %d sentences, want 112", phase, who, len(got))
				}
				in := map[string]int{}
				for _, s := range want {
					in[s]++
				}
				for _, s := range got {
					if in[s]--; in[s] < 0 {
						t.Errorf("%s: %s reads %q, which the admin does not", phase, who, s)
					}
				}
			}
			_, stream := e.streamBody("/api/stream", "Cookie", ck)
			for _, p := range private {
				if strings.Contains(stream, p) {
					t.Errorf("%s: the stream of %s holds %q", phase, who, p)
				}
			}
		}
	}
	// 1. right after the update: the sessions of before, nobody has a user name yet
	old := map[string]string{"the admin": e.staleCookie(setbackOwner, 0), "a viewer": e.staleCookie(setbackVee, 0)}
	check("right after the update", old, as(access.HiddenAddress, access.HiddenName, access.HiddenName, access.HiddenName))
	// 2. everybody signs in again and brings a user name: each of the four is the only account a stored text
	// can mean, and is named by the user name
	named := func(u model.User, username string) model.User { u.PreferredUsername = model.P(username); return u }
	e.cookie(named(setbackBare, "bare"), 0)
	e.cookie(named(setbackDana, "dd"), 0)
	e.cookie(named(setbackCleo, "cc"), 0)
	now := map[string]string{"the admin": e.cookie(named(setbackOwner, "juniper"), 0), "a viewer": e.cookie(named(setbackVee, "vee"), 0)}
	check("after the sign-ins", now, as("juniper", "bare", "dd", "cc"))
	d = e.s.Access.Load()
	for _, sub := range []string{"sub-dana", "sub-cleo"} {
		if u := d.Users[sub]; u.Role != access.RolePending {
			t.Errorf("%s has the role %q after its sign-in", sub, u.Role)
		}
	}

	// 3. And an account that signs in and is given no role still takes part in nothing, whatever it calls
	// itself: the admin's user name, a word of the sentences, the name or the user name of the account that was
	// set back. Every sentence reads as before, also one this version writes.
	e.s.Notes.Add(notifications.Event{Kind: "setback_now", Type: "power", Severity: notifications.Warning, SystemID: "home-deskpc-agent", Host: "DESKPC",
		Text: "Kettle (Den bar) switched off by " + e.s.whoShown(&model.User{Sub: "sub-owner", Auth: "session"})})
	want := append(as("juniper", "bare", "dd", "cc"), "Kettle (Den bar) switched off by juniper")
	sort.Strings(want)
	for i, w := range []model.User{
		{Name: model.P("juniper")},
		{Name: model.P("Juniper"), PreferredUsername: model.P("wanda")},
		{Name: model.P("off")},
		{Name: model.P("switched"), PreferredUsername: model.P("off")},
		{Name: model.P("Dana Demotedson")},
		{Name: model.P("dd")},
		{Name: model.P("Bare Nameson"), PreferredUsername: model.P("dd")},
		{Name: model.P("bare"), Email: model.P("owner@example.com")},
	} {
		w.Sub = fmt.Sprintf("9a1b2c3d-waits-%d", i)
		ck := e.cookie(w, 0)
		if u := e.s.Access.Load().Users[w.Sub]; u.Role != access.RolePending || u.Via != "first sign-in" {
			t.Fatalf("%s signed in and is %q via %q", w.Sub, u.Role, u.Via)
		}
		if x := e.do("GET", "/api/notifications?limit=200", nil, "Cookie", ck); x.code != 403 {
			t.Errorf("an account that waits reads the list: %d", x.code)
		}
		what := fmt.Sprintf("with an account that only waits and calls itself %q (user name %q)", model.Deref(w.Name), model.Deref(w.PreferredUsername))
		// (the viewer's count is one more: the sentence of now is about a machine of the site)
		if got := setbackList(e, now["the admin"]); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: the admin's list changed", what)
			for i := range min(len(got), len(want)) {
				if got[i] != want[i] {
					t.Errorf("  got  %q\n  want %q", got[i], want[i])
					break
				}
			}
		}
		if got := setbackList(e, now["a viewer"]); len(got) != 113 {
			t.Errorf("%s: a viewer reads %d sentences", what, len(got))
		} else {
			for _, s := range got {
				if strings.Contains(s, access.HiddenName) || strings.Contains(s, access.HiddenAddress) || strings.Contains(s, "Dana") || strings.Contains(s, "Nameson") || strings.Contains(s, "@") {
					t.Errorf("%s: a viewer reads %q", what, s)
				}
			}
		}
		if got := shownAs(e, now["the admin"]); got != "juniper" {
			t.Errorf("%s: the admin is shown as %q", what, got)
		}
	}
}
