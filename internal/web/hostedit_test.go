package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// "Edit host", the machine itself (stage 1b): its name, its site and its Wake-on-LAN details, through the
// real handler chain: who may, what can be sent, where it is kept, what it moves and what it never can.

// hedRig is a panel of plain machines in two sites. relay-a (site-a) and relay-b (site-b) may send wake
// packets. pc-a (site-a) has a wol: through relay-a and reads a plug of its own; pc-b has no site and no
// wol:; box-c has a wol: with a target address, box-d one with a password.
type hedRig struct {
	*env
	hdr []string // an admin who signed in just now: the cookie and the page's header
	a   *armed   // an admin who signed in through the identity provider an hour ago (for "Confirm it's you")
}

func hedConfig(t *testing.T) *config.Panel {
	t.Helper()
	for _, v := range []string{"TOK_RA", "TOK_RB", "TOK_PA", "TOK_PB", "TOK_BC", "TOK_BD"} {
		t.Setenv(v, testutil.ReadToken+strings.ToLower(v[len(v)-2:]))
	}
	t.Setenv("CTL_ALL", testutil.ControlToken)
	t.Setenv("WOL_PASSWORD_D", "02:00:00:00:00:99")
	dir := t.TempDir()
	y := `
public_url: https://panel.example.com
request_timeout: 1
offline_after: 60
audit_log: ` + fmt.Sprintf("%q", dir+"/audit.log") + `
data_dir: ` + fmt.Sprintf("%q", dir+"/data") + `
oidc: {issuer: "https://id.example.com", client_id: cid, allowed_groups: [viewers], control_groups: [controllers]}
agents:
  - {id: relay-a, name: Relay A, url: "http://relay-a", token_env: TOK_RA, control_token_env: CTL_ALL, wol_relay: true, site: site-a}
  - {id: relay-b, name: Relay B, url: "http://relay-b", token_env: TOK_RB, control_token_env: CTL_ALL, wol_relay: true, site: site-b}
  - {id: pc-a, name: PC-A, url: "http://pc-a", token_env: TOK_PA, site: site-a}
  - {id: pc-b, name: PC-B, url: "http://pc-b", token_env: TOK_PB, control_token_env: CTL_ALL, power_control: true}
  - {id: box-c, name: Box C, url: "http://box-c", token_env: TOK_BC, site: site-a}
  - {id: box-d, name: Box D, url: "http://box-d", token_env: TOK_BD}
systems:
  - {id: pc-a, wol: {mac: "02:00:00:00:00:A1", relay: relay-a}}
  - {id: box-c, wol: {mac: "02:00:00:00:00:c1", relay: relay-a, broadcast: 10.20.30.255}}
  - {id: box-d, wol: {mac: "02:00:00:00:00:d1", relay: relay-a, password_env: WOL_PASSWORD_D}}
`
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func hedBackend() *testutil.Backend {
	b := testutil.NewBackend()
	for _, id := range []string{"relay-a", "relay-b", "pc-a", "pc-b", "box-c", "box-d"} {
		b.Metrics[id] = func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			m["hostname"] = id
			delete(m, "power_sources")
			if id == "pc-a" {
				m["power_sources"] = []any{jsonx.M{"id": "desk", "name": "Desk plug", "type": "homeassistant", "ok": true, "watts": 50.0, "system_hint": "self"}}
			}
			return m, nil
		}
	}
	return b
}

func hedSetup(t *testing.T) *hedRig { return hedStart(t, hedConfig(t), hedBackend()) }

func hedStart(t *testing.T, cfg *config.Panel, b *testutil.Backend) *hedRig {
	t.Helper()
	r := &hedRig{env: ehStart(t, cfg, b)}
	r.s.control = newRateLimiter(1000)
	r.a = &armed{e: r.env, idp: newFakeIDP(t), clk: r.fakeClock()}
	r.hdr = []string{"Cookie", r.cookie(controller(), 0), "X-Requested-With", "fairy"} // the first sign-in of a control group: an admin
	r.s.Cfg.OIDC.Issuer = r.a.idp.srv.URL
	r.a.idp.claims = map[string]any{"sub": "u5", "email": "five@example.com", "groups": []string{"viewers", "controllers"}, "auth_time": time.Now().Add(-time.Hour).Unix()}
	q, rt := r.a.idp.login(r.env, "/auth/login?next=/", "")
	r.a.sess = sessionCookie(r.env, r.do("GET", "/auth/callback?code=c&state="+q.Get("state"), nil, "Cookie", rt).hdr)
	if _, _, err := r.s.Access.SetRole("u5", access.RoleAdmin, nil, "test", true); err != nil { // the second admin is made one
		t.Fatal(err)
	}
	return r
}

func (r *hedRig) poll() { r.s.Poller.PollOnce(context.Background()) }

func (r *hedRig) options(id string) resp {
	r.t.Helper()
	g := r.do("GET", "/api/admin/hosts/"+id+"/options", nil, r.hdr[:2]...)
	if g.code != 200 {
		r.t.Fatalf("GET options of %s: %d %s", id, g.code, g.raw)
	}
	return g
}

// host is the "host" part of the dialog's data: its name, site or wol.
func (r *hedRig) host(id, part string) map[string]any {
	r.t.Helper()
	h, _ := r.options(id).body["host"].(map[string]any)
	m, _ := h[part].(map[string]any)
	if m == nil {
		r.t.Fatalf("the options of %s have no host.%s", id, part)
	}
	return m
}

func (r *hedRig) put(id string, body any) resp {
	r.t.Helper()
	return r.do("PUT", "/api/admin/hosts/"+id+"/options", body, append([]string{"Content-Type", "application/json"}, r.hdr...)...)
}

// ok is put for a change that the admin who just signed in may make at once.
func (r *hedRig) ok(id string, body any) resp {
	r.t.Helper()
	p := r.put(id, body)
	if p.code != 200 || p.body["ok"] != true {
		r.t.Fatalf("PUT options of %s %v: %d %s", id, body, p.code, p.raw)
	}
	return p
}

// confirmed is a change that needs its own "Confirm it's you": the request is refused and kept, the admin
// confirms it at the identity provider, and the panel runs it. It returns what the run answered (GET
// /api/me: pending_result).
func (r *hedRig) confirmed(id string, body any) map[string]any {
	r.t.Helper()
	p := r.do("PUT", "/api/admin/hosts/"+id+"/options", body, "Content-Type", "application/json", "Cookie", r.a.sess, "X-Requested-With", "fairy")
	d, _ := p.body["detail"].(map[string]any)
	if !stepUpRequired(p) || d["pending"] != true {
		r.t.Fatalf("PUT options of %s %v: not kept for a confirmation: %d %s", id, body, p.code, p.raw)
	}
	if cb := r.a.confirm(pendingID(p), false); cb.code != 303 {
		r.t.Fatalf("the confirmation: %d %s", cb.code, cb.raw)
	}
	res, _ := r.do("GET", "/api/me", nil, "Cookie", r.a.sess).body["pending_result"].(map[string]any)
	if res == nil {
		r.t.Fatalf("the confirmed change of %s %v has no result", id, body)
	}
	return res
}

// wol is confirmed for a change that must be taken.
func (r *hedRig) wol(id string, w map[string]any) {
	r.t.Helper()
	if res := r.confirmed(id, map[string]any{"wol": w}); res["ok"] != true {
		r.t.Fatalf("Wake-on-LAN of %s %v: %v", id, w, res)
	}
}

func (r *hedRig) row(id string) map[string]any {
	r.t.Helper()
	g := r.do("GET", "/api/systems/"+id, nil, r.hdr[:2]...)
	if g.code != 200 {
		r.t.Fatalf("GET %s: %d %s", id, g.code, g.raw)
	}
	return g.body
}

// storedHost is one machine's decisions in state.json (nil: none).
func (e *env) storedHost(id string) map[string]any {
	e.t.Helper()
	h, _ := e.storedOptions()["hosts"].(map[string]any)
	m, _ := h[id].(map[string]any)
	return m
}

// lastChange is the newest audit entry host.options of the machine and its params.
func (r *hedRig) lastChange(id string) (entry, params map[string]any) {
	r.t.Helper()
	for _, e := range r.auditEntries("", "host.options") {
		if e["target"] == id {
			p, _ := e["params"].(map[string]any)
			return e, p
		}
	}
	r.t.Fatalf("no audit entry host.options for %s", id)
	return nil, nil
}

func changeOf(params map[string]any, field string) map[string]any {
	l, _ := params["changes"].([]any)
	for _, x := range l {
		if m, _ := x.(map[string]any); m["field"] == field {
			return m
		}
	}
	return nil
}

// wakes is the wake requests the relays were sent, one line each.
func (r *hedRig) wakes() []string {
	r.b.Mu.Lock()
	defer r.b.Mu.Unlock()
	var out []string
	for _, x := range r.b.Requests {
		if strings.Contains(x, "POST /wol") {
			out = append(out, x)
		}
	}
	return out
}

// off makes a machine silent (it is off): its row is offline and can be woken.
func (r *hedRig) off(id string) {
	r.b.Mu.Lock()
	r.b.Metrics[id] = func() (jsonx.M, error) { return nil, devices.Errf("connection failed") }
	r.b.Mu.Unlock()
	r.poll()
}

func (r *hedRig) wake(id string) resp {
	r.t.Helper()
	return r.do("POST", "/api/systems/"+id+"/power", map[string]any{"action": "wake"}, append([]string{"Content-Type", "application/json"}, r.hdr...)...)
}

// The dialog's data about the machine itself: what counts, where it comes from, the choices, and what is
// only shown. Nothing is changed by looking, and nothing secret is in it.
func TestEditHostShowsTheMachineItself(t *testing.T) {
	r := hedSetup(t)
	g := r.options("pc-a")
	name, site, wol := r.host("pc-a", "name"), r.host("pc-a", "site"), r.host("pc-a", "wol")
	if name["value"] != "PC-A" || name["from"] != "config" || name["config"] != "PC-A" {
		t.Errorf("name: %v", name)
	}
	if site["value"] != "site-a" || site["from"] != "config" || site["default"] != "site-a" || !slices.Equal(strs(site["choices"]), []string{"site-a", "site-b"}) {
		t.Errorf("site: %v", site)
	}
	plans, _ := site["plans"].(map[string]any)
	if b, _ := plans["site-b"].(map[string]any); b == nil || b["line"] != "Site: site-b (was site-a)" ||
		!slices.Equal(strs(b["notes"]), []string{"Viewers of site-b will see PC-A", "Viewers of site-a will no longer see PC-A", "Viewers of site-b will see plug Desk plug", "Viewers of site-a will no longer see plug Desk plug"}) {
		t.Errorf("what the site site-b would move: %v", plans["site-b"])
	}
	if _, has := plans["site-a"]; has || len(plans) != 1 {
		t.Errorf("a plan for the site it has, or for the default it has: %v", plans)
	}
	if wol["on"] != true || !slices.Equal(strs(wol["macs"]), []string{"02:00:00:00:00:a1"}) || wol["relay"] != "relay-a" || wol["relay_name"] != "Relay A" || wol["port"] != 9.0 ||
		wol["from"] != "config" || wol["target"] != nil || wol["password"] != false || wol["relay_fixed"] != nil || wol["unused"] != nil {
		t.Errorf("wol: %v", wol)
	}
	relays, _ := json.Marshal(wol["relays"])
	if string(relays) != `[{"id":"relay-a","name":"Relay A","online":true},{"id":"relay-b","name":"Relay B","online":true}]` {
		t.Errorf("the relays to choose from: %s", relays)
	}
	// a machine that may send wake packets is not among its own relays; one without a wol: has none
	if w := r.host("relay-a", "wol"); w["on"] != false || fmt.Sprint(w["relays"]) != "[map[id:relay-b name:Relay B online:true]]" || w["config"] != nil {
		t.Errorf("relay-a: %v", w)
	}
	// the target address is shown, the password only as "there is one", and its relay is fixed with the reason
	c, d := r.host("box-c", "wol"), r.host("box-d", "wol")
	if c["target"] != "10.20.30.255" || c["password"] != false || !strings.Contains(fmt.Sprint(c["relay_fixed"]), "target address") {
		t.Errorf("box-c: %v", c)
	}
	if d["target"] != nil || d["password"] != true || !strings.Contains(fmt.Sprint(d["relay_fixed"]), "password") {
		t.Errorf("box-d: %v", d)
	}
	for _, id := range []string{"pc-a", "box-c", "box-d"} {
		raw := string(r.options(id).raw)
		for _, secret := range []string{"WOL_PASSWORD_D", "02:00:00:00:00:99", testutil.ReadToken, testutil.ControlToken, "password_env", "token_env", "TOK_", "CTL_ALL", "http://"} {
			if strings.Contains(raw, secret) {
				t.Errorf("the options of %s hold %q", id, secret)
			}
		}
	}
	if g.body["name"] != "PC-A" || r.storedOptions() != nil || len(r.auditEntries("", "host.options")) != 0 {
		t.Error("looking changed something")
	}
}

// Who may. Nobody but an admin's browser session gets the dialog or changes anything: not somebody who may
// see the machine's site, not somebody waiting for access, not an API token, and no request without the
// page's header. An admin whose sign-in is old is asked to confirm, and nothing is stored meanwhile. (There
// is no admin who is limited to a site: an admin sees and manages every site.)
func TestEditHostTheMachineItselfWhoMay(t *testing.T) {
	r := hedSetup(t)
	vc := r.cookie(viewer, 0)
	if _, _, err := r.s.Access.SetRole(viewer.Sub, access.RoleViewer, []string{"site-a"}, "test", true); err != nil {
		t.Fatal(err)
	}
	if g := r.do("GET", "/api/systems/pc-a", nil, "Cookie", vc); g.code != 200 {
		t.Fatalf("the viewer of site-a does not see pc-a: %d", g.code)
	}
	waiting := model.User{Sub: "u9", Name: model.P("Waiting"), Email: model.P("w@example.com"), Groups: []string{"viewers"}}
	wc := r.cookie(waiting, 0)
	ctl, _, _ := r.s.Tokens.Create("ops", "system", "control", "cli:test", 0)
	bodies := []map[string]any{{"name": "Mine now"}, {"site": "site-b"}, {"wol": map[string]any{"macs": []string{"02:00:00:00:00:ee"}}}, {"wol": map[string]any{"off": true}},
		{"reset": []any{map[string]any{"field": "name"}}}}
	for who, hdr := range map[string][]string{
		"a viewer of the machine's site": {"Cookie", vc, "X-Requested-With", "fairy"},
		"somebody waiting for access":    {"Cookie", wc, "X-Requested-With", "fairy"},
		"an API token with control":      {"Authorization", "Bearer " + ctl},
		"nobody":                         {"X-Requested-With", "fairy"},
		"an admin without the header":    {"Cookie", r.hdr[1]},
	} {
		if g := r.do("GET", "/api/admin/hosts/pc-a/options", nil, hdr...); who != "an admin without the header" && (g.code != 403 && g.code != 401 || strings.Contains(string(g.raw), "relay")) {
			t.Errorf("%s gets the dialog: %d %s", who, g.code, g.raw)
		}
		for _, body := range bodies {
			p := r.do("PUT", "/api/admin/hosts/pc-a/options", body, append([]string{"Content-Type", "application/json"}, hdr...)...)
			if p.code != 403 && p.code != 401 {
				t.Errorf("%s, %v: %d %s", who, body, p.code, p.raw)
			}
			if stepUpRequired(p) {
				t.Errorf("%s, %v: asked to confirm", who, body)
			}
		}
	}
	// an admin whose sign-in is an hour old: asked, kept, nothing stored
	for _, body := range bodies {
		p := r.do("PUT", "/api/admin/hosts/pc-a/options", body, "Content-Type", "application/json", "Cookie", r.a.sess, "X-Requested-With", "fairy")
		if d, _ := p.body["detail"].(map[string]any); !stepUpRequired(p) || d["pending"] != true {
			t.Errorf("an old sign-in, %v: %d %s", body, p.code, p.raw)
		}
	}
	if r.storedOptions() != nil || len(r.s.Access.Load().Assign) != 0 || r.row("pc-a")["name"] != "PC-A" {
		t.Errorf("something was stored: %v, %v", r.storedOptions(), r.s.Access.Load().Assign)
	}
	for _, e := range r.auditEntries("", "host.options") {
		t.Errorf("an audit entry of a change: %v", e)
	}
}

// What can never be sent. A request that names anything but the closed set is refused as a whole (422),
// before anybody is asked to confirm, and nothing is stored: where a machine is, its tokens, what the panel
// may do to it, and of a wake packet the address it is sent to and its password.
func TestEditHostTheMachineItselfNeverEditable(t *testing.T) {
	r := hedSetup(t)
	wolWith := func(k string, v any) map[string]any {
		return map[string]any{"wol": map[string]any{"macs": []string{"02:00:00:00:00:ee"}, k: v}}
	}
	never := map[string]map[string]any{
		"a target address":          wolWith("broadcast", "10.9.8.7"),
		"a target under other name": wolWith("target", "10.9.8.7"),
		"an address":                wolWith("address", "10.9.8.7"),
		"a host":                    wolWith("host", "pc-b"),
		"a password variable":       wolWith("password_env", "FAIRY_SESSION_SECRET"),
		"a password":                wolWith("password", "02:00:00:00:00:99"),
		"a relay switch":            wolWith("wol_relay", true),
		"mac beside macs":           wolWith("mac", "02:00:00:00:00:ef"),
		"Broadcast spelled up":      wolWith("Broadcast", "10.9.8.7"),
	}
	for _, k := range []string{"id", "url", "token_env", "control_token_env", "verify_tls", "node", "links_to", "fan_control", "power_control", "self_update", "wol_relay",
		"owner", "bmc_power", "group", "tags", "dwservice_url", "broadcast", "password_env", "token", "control_token", "allow_non_tailscale", "host", "hosts", "agents"} {
		never["the key "+k] = map[string]any{"name": "Fine name", k: "x"}
		never["the key "+k+" as a switch"] = map[string]any{"name": "Fine name", k: true}
	}
	for what, body := range never {
		for who, hdr := range map[string][]string{"signed in just now": r.hdr, "signed in an hour ago": {"Cookie", r.a.sess, "X-Requested-With", "fairy"}} {
			p := r.do("PUT", "/api/admin/hosts/pc-a/options", body, append([]string{"Content-Type", "application/json"}, hdr...)...)
			if p.code != 422 {
				t.Errorf("%s (%s): %d %s", what, who, p.code, p.raw)
			}
		}
	}
	// ... and what is of the closed set, with a value it cannot have
	form := map[string]map[string]any{
		"a relay that is an address":  {"wol": map[string]any{"relay": "http://10.9.8.7:9101"}},
		"a relay with a port":         {"wol": map[string]any{"relay": "relay-a:9"}},
		"port 80":                     {"wol": map[string]any{"port": 80}},
		"port 0":                      {"wol": map[string]any{"port": 0}},
		"a port as a text":            {"wol": map[string]any{"port": "9"}},
		"a MAC that is an address":    {"wol": map[string]any{"macs": []string{"10.9.8.7"}}},
		"a MAC with something behind": {"wol": map[string]any{"macs": []string{"02:00:00:00:00:ee;reboot"}}},
		"no MAC":                      {"wol": map[string]any{"macs": []string{}}},
		"nine MACs":                   {"wol": map[string]any{"macs": []string{"02:00:00:00:00:01", "02:00:00:00:00:02", "02:00:00:00:00:03", "02:00:00:00:00:04", "02:00:00:00:00:05", "02:00:00:00:00:06", "02:00:00:00:00:07", "02:00:00:00:00:08", "02:00:00:00:00:09"}}},
		"the broadcast MAC":           {"wol": map[string]any{"macs": []string{"ff:ff:ff:ff:ff:ff"}}},
		"a MAC twice":                 {"wol": map[string]any{"macs": []string{"02:00:00:00:00:ee", "02-00-00-00-00-EE"}}},
		"macs as a text":              {"wol": map[string]any{"macs": "02:00:00:00:00:ee"}},
		"an empty wol":                {"wol": map[string]any{}},
		"off: false":                  {"wol": map[string]any{"off": false}},
		"off with a MAC":              {"wol": map[string]any{"off": true, "macs": []string{"02:00:00:00:00:ee"}}},
		"wol and its reset":           {"wol": map[string]any{"port": 7}, "reset": []any{map[string]any{"field": "wol"}}},
		"a name of two lines":         {"name": "PC\nA"},
		"a name with an escape":       {"name": "PC\x1b[31mA"},
		"a name that turns the text":  {"name": "PC‮A"},
		"a name of 65 characters":     {"name": strings.Repeat("n", 65)},
		"an empty name":               {"name": "  "},
		"a name as a number":          {"name": 7},
		"name and its reset":          {"name": "Fine name", "reset": []any{map[string]any{"field": "name"}}},
		"a reset of name with an id":  {"reset": []any{map[string]any{"field": "name", "id": "pc-b"}}},
		"a reset of name twice":       {"reset": []any{map[string]any{"field": "name"}, map[string]any{"field": "name"}}},
		"a reset of the site":         {"reset": []any{map[string]any{"field": "site"}}},
		"a site with a line break":    {"site": "site-a\nsite-b"},
		"a site as a list":            {"site": []string{"site-a", "site-b"}},
	}
	for what, body := range form {
		p := r.do("PUT", "/api/admin/hosts/pc-a/options", body, "Content-Type", "application/json", "Cookie", r.a.sess, "X-Requested-With", "fairy")
		if p.code != 422 { // also for the old sign-in: nobody is asked to confirm what can never be taken
			t.Errorf("%s: %d %s", what, p.code, p.raw)
		}
	}
	if r.storedOptions() != nil || len(r.s.Access.Load().Assign) != 0 {
		t.Errorf("something was stored: %v, %v", r.storedOptions(), r.s.Access.Load().Assign)
	}
	// What only the panel can know is refused when the change is looked at, after the confirmation: the relay
	// must be an agent that may send wake packets already, and not the machine itself; the relay of an entry
	// with a target address or a password stays config.yaml's; a name another host has; a site there is none of.
	late := map[string]struct {
		id   string
		body map[string]any
		want string
	}{
		"a relay that is no agent":           {"pc-a", map[string]any{"wol": map[string]any{"relay": "relay-z"}}, "is no agent of this panel"},
		"a relay that may not send":          {"pc-a", map[string]any{"wol": map[string]any{"relay": "pc-b"}}, "does not send wake packets"},
		"itself":                             {"relay-a", map[string]any{"wol": map[string]any{"relay": "relay-a", "macs": []string{"02:00:00:00:00:ee"}}}, "cannot send its own wake packet"},
		"another relay, target address":      {"box-c", map[string]any{"wol": map[string]any{"relay": "relay-b"}}, "target address"},
		"another relay, password":            {"box-d", map[string]any{"wol": map[string]any{"relay": "relay-b"}}, "password"},
		"MACs without a relay, no wol:":      {"pc-b", map[string]any{"wol": map[string]any{"macs": []string{"02:00:00:00:00:ee"}}}, "no relay"},
		"a relay without MACs, no wol:":      {"pc-b", map[string]any{"wol": map[string]any{"relay": "relay-a"}}, "no MAC"},
		"a port alone, no wol:":              {"pc-b", map[string]any{"wol": map[string]any{"port": 7}}, "no MAC"},
		"the relay and the MAC of a refusal": {"box-c", map[string]any{"wol": map[string]any{"relay": "relay-b", "macs": []string{"02:00:00:00:00:ee"}}}, "target address"},
	}
	for what, c := range late {
		res := r.confirmed(c.id, c.body)
		if res["ok"] == true || res["status"] != 422.0 || !strings.Contains(fmt.Sprint(res["message"]), c.want) {
			t.Errorf("%s: %v (want 422 with %q)", what, res, c.want)
		}
	}
	for what, c := range map[string]struct {
		body map[string]any
		want string
	}{
		"the name of another host":      {map[string]any{"name": "pc-b"}, "another host is called PC-B already"},
		"a site there is none of":       {map[string]any{"site": "site-z"}, "no such site"},
		"a name next to a site of none": {map[string]any{"name": "Fine name", "site": "site-z"}, "no such site"},
	} {
		if p := r.put("pc-a", c.body); p.code != 422 || !strings.Contains(string(p.raw), c.want) {
			t.Errorf("%s: %d %s", what, p.code, p.raw)
		}
	}
	if r.storedOptions() != nil || len(r.s.Access.Load().Assign) != 0 || len(r.wakes()) != 0 {
		t.Errorf("something was stored or sent: %v, %v, %v", r.storedOptions(), r.s.Access.Load().Assign, r.wakes())
	}
	// every refusal after the guard is in the audit log, none of them as a change
	for _, e := range r.auditEntries("", "host.options") {
		if out := fmt.Sprint(e["outcome"]); !strings.HasPrefix(out, "error 422") {
			t.Errorf("audit: %v", e)
		}
	}
}

// The name. It is on the row at once, config.yaml's name is kept for Reset, the audit entry has the old and
// the new name, and a name equal to config.yaml's is no decision.
func TestEditHostName(t *testing.T) {
	r := hedSetup(t)
	p := r.ok("pc-a", map[string]any{"name": "  Gaming   PC "})
	if !slices.Equal(strs(p.body["changed"]), []string{"Name: Gaming PC (was PC-A)"}) || p.body["message"] != "changes saved" || p.body["name"] != "Gaming PC" {
		t.Fatalf("the answer: %s", p.raw)
	}
	if got := r.row("pc-a")["name"]; got != "Gaming PC" {
		t.Errorf("the row: %v", got)
	}
	if st := r.storedHost("pc-a"); st["name"] != "Gaming PC" || st["by"] != ehAdmin || st["wol"] != nil {
		t.Errorf("stored: %v", st)
	}
	if n := r.host("pc-a", "name"); n["value"] != "Gaming PC" || n["from"] != "panel" || n["config"] != "PC-A" || n["by"] != ehAdmin {
		t.Errorf("the dialog: %v", n)
	}
	e, params := r.lastChange("pc-a")
	if c := changeOf(params, "name"); e["outcome"] != "ok" || c == nil || c["before"] != "PC-A" || c["after"] != "Gaming PC" || c["before_from"] != "config.yaml" || c["after_from"] != "panel" || c["host"] != "pc-a" {
		t.Errorf("audit: %v", e)
	}
	// the typed confirmation of a power action asks for the name the row has now
	r.off("pc-a")
	if got := r.row("pc-a")["name"]; got != "Gaming PC" {
		t.Errorf("the row while the machine is silent: %v", got)
	}
	// the same name again: nothing to change; another host cannot take it, in any case
	if p := r.ok("pc-a", map[string]any{"name": "Gaming PC"}); p.body["message"] != "nothing to change" {
		t.Errorf("the same name again: %s", p.raw)
	}
	if p := r.put("pc-b", map[string]any{"name": "gaming pc"}); p.code != 422 || !strings.Contains(string(p.raw), "another host is called Gaming PC already") {
		t.Errorf("a second host of that name: %d %s", p.code, p.raw)
	}
	// config.yaml's own name: the decision goes
	if p := r.ok("pc-a", map[string]any{"name": "PC-A"}); !slices.Equal(strs(p.body["changed"]), []string{"Name: PC-A (was Gaming PC)"}) || r.storedOptions()["hosts"] != nil {
		t.Errorf("back to config.yaml's name by typing it: %s, stored %v", p.raw, r.storedOptions())
	}
	r.ok("pc-a", map[string]any{"name": "Den"})
	if p := r.ok("pc-a", map[string]any{"reset": []any{map[string]any{"field": "name"}}}); !slices.Equal(strs(p.body["changed"]), []string{"Name: back to config.yaml: PC-A"}) ||
		r.row("pc-a")["name"] != "PC-A" || r.storedHost("pc-a") != nil {
		t.Errorf("Reset: %s", p.raw)
	}
	if p := r.ok("pc-a", map[string]any{"reset": []any{map[string]any{"field": "name"}}}); p.body["message"] != "nothing to change" {
		t.Errorf("Reset of nothing: %s", p.raw)
	}
	// a name and a link in one request: one entry, both in it, and the links' own message stays for links alone
	p = r.ok("pc-a", map[string]any{"name": "Den", "plugs": []any{map[string]any{"id": "pc-a/desk", "in_total": false}}})
	if !slices.Equal(strs(p.body["changed"]), []string{"Name: Den (was PC-A)", "Plug Desk plug (read by PC-A): not in the total"}) {
		t.Errorf("a name and a link: %s", p.raw)
	}
	if p := r.ok("pc-a", map[string]any{"plugs": []any{map[string]any{"id": "pc-a/desk", "in_total": true}}}); p.body["message"] != "links saved" {
		t.Errorf("a link alone: %s", p.raw)
	}
}

// The site. The dialog changes what the Users page changes, in the Users page's own store: who sees the
// machine moves with it, the answer and the audit entry say whose view moves, and nothing is kept in
// state.json.
func TestEditHostSite(t *testing.T) {
	r := hedSetup(t)
	vb := model.User{Sub: "u7", Name: model.P("Bea"), Email: model.P("b@example.com"), Groups: []string{"viewers"}}
	vc := r.cookie(vb, 0)
	if _, _, err := r.s.Access.SetRole(vb.Sub, access.RoleViewer, []string{"site-b"}, "test", true); err != nil {
		t.Fatal(err)
	}
	sees := func(id string) bool { return r.do("GET", "/api/systems/"+id, nil, "Cookie", vc).code == 200 }
	if sees("pc-a") || !sees("relay-b") {
		t.Fatal("the rig: a viewer of site-b sees relay-b and not pc-a")
	}
	p := r.ok("pc-a", map[string]any{"site": "site-b"})
	want := []string{"Viewers of site-b will see PC-A", "Viewers of site-a will no longer see PC-A", "Viewers of site-b will see plug Desk plug", "Viewers of site-a will no longer see plug Desk plug"}
	if !slices.Equal(strs(p.body["changed"]), []string{"Site: site-b (was site-a)"}) || !slices.Equal(strs(p.body["visibility"]), want) || p.body["site"] != "site-b" {
		t.Fatalf("the answer: %s", p.raw)
	}
	if got := r.s.Access.Load().Assign; len(got) != 1 || got["pc-a"] != "site-b" {
		t.Errorf("the Users page's store: %v", got)
	}
	if r.storedOptions() != nil {
		t.Errorf("state.json: %v", r.storedOptions())
	}
	if !sees("pc-a") {
		t.Error("the viewer of site-b does not see pc-a")
	}
	if s := r.host("pc-a", "site"); s["value"] != "site-b" || s["from"] != "panel" || s["default"] != "site-a" {
		t.Errorf("the dialog: %v", s)
	}
	e, params := r.lastChange("pc-a")
	if c := changeOf(params, "site"); e["outcome"] != "ok" || c == nil || c["before"] != "site-a" || c["after"] != "site-b" || c["before_from"] != "config.yaml" || c["after_from"] != "panel" ||
		!slices.Equal(strs(params["visibility"]), want) {
		t.Errorf("audit: %v", e)
	}
	// the Users page shows the same assignment (one store)
	found := false
	for _, x := range r.do("GET", "/api/admin/users", nil, r.hdr[:2]...).body["targets"].([]any) {
		if m := x.(map[string]any); m["target"] == "pc-a" {
			found = m["site"] == "site-b" && m["assigned"] == "site-b"
		}
	}
	if !found {
		t.Error("the Users page does not show the assignment")
	}
	// the same again: nothing; the default by its name: the assignment goes; "": the same
	if p := r.ok("pc-a", map[string]any{"site": "site-b"}); p.body["message"] != "nothing to change" {
		t.Errorf("the same site again: %s", p.raw)
	}
	if p := r.ok("pc-a", map[string]any{"site": "site-a"}); !slices.Equal(strs(p.body["changed"]), []string{"Site: site-a (was site-b)"}) || len(r.s.Access.Load().Assign) != 0 || sees("pc-a") {
		t.Errorf("the default by its name: %s, %v", p.raw, r.s.Access.Load().Assign)
	}
	r.ok("pc-a", map[string]any{"site": "site-b"})
	if p := r.ok("pc-a", map[string]any{"site": ""}); !slices.Equal(strs(p.body["changed"]), []string{"Site: back to the default: site-a (was site-b)"}) || len(r.s.Access.Load().Assign) != 0 {
		t.Errorf("back to the default: %s", p.raw)
	}
	// a machine without a site gets one, and loses it again: then only admins see it, and the line says so
	if p := r.ok("pc-b", map[string]any{"site": "site-b"}); !slices.Equal(strs(p.body["changed"]), []string{"Site: site-b (was none (admins only))"}) || !sees("pc-b") {
		t.Errorf("a first site: %s", p.raw)
	}
	if p := r.ok("pc-b", map[string]any{"site": ""}); !slices.Equal(strs(p.body["visibility"]), []string{"Viewers of site-b will no longer see PC-B", "Only admins will see PC-B"}) || sees("pc-b") {
		t.Errorf("no site again: %s", p.raw)
	}
	// a site and a name in one request, and the request as "Confirm it's you" lists it for an old sign-in
	old := r.do("PUT", "/api/admin/hosts/pc-a/options", map[string]any{"site": "site-b", "name": "Den"}, "Content-Type", "application/json", "Cookie", r.a.sess, "X-Requested-With", "fairy")
	d, _ := old.body["detail"].(map[string]any)
	if !stepUpRequired(old) || d["per_action"] == true || !slices.Equal(strs(d["items"]), append([]string{"Site: site-b (was site-a)", "Name: Den (was PC-A)"}, want...)) {
		t.Errorf("what an old sign-in is asked to confirm: %s", old.raw)
	}
	if !strings.HasPrefix(fmt.Sprint(d["action"]), "edit the host pc-a") {
		t.Errorf("the action: %v", d["action"])
	}
	p = r.ok("pc-a", map[string]any{"site": "site-b", "name": "Den"})
	if !slices.Equal(strs(p.body["changed"]), []string{"Site: site-b (was site-a)", "Name: Den (was PC-A)"}) || r.row("pc-a")["name"] != "Den" || r.s.Access.Load().Assign["pc-a"] != "site-b" {
		t.Errorf("a site and a name: %s", p.raw)
	}
}

// A request that changes the site and something that is refused changes neither store; and when state.json
// cannot be written, the site is put back.
func TestEditHostSiteIsNotLeftChangedByARequestThatFails(t *testing.T) {
	r := hedSetup(t)
	if p := r.put("pc-a", map[string]any{"site": "site-b", "name": "pc-b"}); p.code != 422 {
		t.Fatalf("a site with a name that is taken: %d %s", p.code, p.raw)
	}
	if got := r.s.Access.Load().Assign; len(got) != 0 {
		t.Fatalf("the site was changed by a refused request: %v", got)
	}
	// state.json cannot be written: its lock is a folder
	if err := os.MkdirAll(filepath.Join(r.s.Cfg.DataDir, "state.json.lock"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := r.put("pc-a", map[string]any{"site": "site-b", "name": "Den"})
	if p.code == 200 {
		t.Fatalf("saved without state.json: %s", p.raw)
	}
	if got := r.s.Access.Load().Assign; len(got) != 0 {
		t.Errorf("the site stayed changed after the rest could not be saved: %v", got)
	}
	if e, _ := r.lastChange("pc-a"); e["outcome"] != "failed: not saved" {
		t.Errorf("audit: %v", e)
	}
}

// Wake-on-LAN. Every change needs its own confirmation, also right after a sign-in. What is decided is what
// a wake sends: the MAC addresses and the port, through the chosen relay. The address a packet goes to and
// the password are config.yaml's in every request.
func TestEditHostWakeOnLAN(t *testing.T) {
	r := hedSetup(t)
	// signed in just now: still asked, with the lines, and the confirmation is for this one request
	p := r.put("pc-a", map[string]any{"wol": map[string]any{"macs": []string{"02-00-00-00-00-EE"}}})
	d, _ := p.body["detail"].(map[string]any)
	if !stepUpRequired(p) || d["per_action"] != true || !slices.Equal(strs(d["items"]), []string{"Wake-on-LAN: to 02:00:00:00:00:ee through Relay A, port 9 (was to 02:00:00:00:00:a1 through Relay A, port 9)"}) {
		t.Fatalf("a change of the MAC right after a sign-in: %d %s", p.code, p.raw)
	}
	if r.storedOptions() != nil {
		t.Fatal("stored before the confirmation")
	}
	if p := r.put("pc-a", map[string]any{"reset": []any{map[string]any{"field": "wol"}}}); !stepUpRequired(p) {
		t.Errorf("a reset of Wake-on-LAN right after a sign-in: %d", p.code)
	}
	// confirmed: stored as the parts that differ from config.yaml, in one spelling
	r.wol("pc-a", map[string]any{"macs": []string{"02-00-00-00-00-EE", "02:00:00:00:00:ef"}, "relay": "relay-b", "port": 7})
	st, _ := r.storedHost("pc-a")["wol"].(map[string]any)
	if !slices.Equal(strs(st["macs"]), []string{"02:00:00:00:00:ee", "02:00:00:00:00:ef"}) || st["relay"] != "relay-b" || st["port"] != 7.0 || len(st) != 3 {
		t.Fatalf("stored: %v", r.storedHost("pc-a"))
	}
	w := r.host("pc-a", "wol")
	if w["from"] != "panel" || w["relay"] != "relay-b" || w["port"] != 7.0 || !slices.Equal(strs(w["macs"]), []string{"02:00:00:00:00:ee", "02:00:00:00:00:ef"}) ||
		fmt.Sprint(w["config"]) != "map[macs:[02:00:00:00:00:a1] port:9 relay:relay-a relay_name:Relay A]" {
		t.Errorf("the dialog: %v", w)
	}
	_, params := r.lastChange("pc-a")
	c := changeOf(params, "wol")
	if c == nil || fmt.Sprint(c["before"]) != "map[macs:[02:00:00:00:00:a1] port:9 relay:relay-a relay_name:Relay A]" ||
		fmt.Sprint(c["after"]) != "map[macs:[02:00:00:00:00:ee 02:00:00:00:00:ef] port:7 relay:relay-b relay_name:Relay B]" || c["before_from"] != "config.yaml" || c["after_from"] != "panel" {
		t.Errorf("audit: %v", params)
	}
	// the wake: both cards, through relay-b, port 7, no target address, no password
	r.off("pc-a")
	if k := r.wake("pc-a"); k.code != 200 {
		t.Fatalf("wake: %d %s", k.code, k.raw)
	}
	if got := r.wakes(); len(got) != 2 || got[0] != "relay-b POST /wol map[broadcast:<nil> mac:02:00:00:00:00:ee port:7]" || got[1] != "relay-b POST /wol map[broadcast:<nil> mac:02:00:00:00:00:ef port:7]" {
		t.Errorf("the wake packets: %q", got)
	}
	// config.yaml's values typed back: the decisions go, one by one
	r.wol("pc-a", map[string]any{"relay": "relay-a", "port": 9})
	if st, _ := r.storedHost("pc-a")["wol"].(map[string]any); len(st) != 1 || st["macs"] == nil {
		t.Errorf("after the relay and the port of config.yaml: %v", st)
	}
	r.wol("pc-a", map[string]any{"macs": []string{"02:00:00:00:00:a1"}})
	if r.storedOptions()["hosts"] != nil {
		t.Errorf("after the MAC of config.yaml: %v", r.storedOptions())
	}
	// off, and back by Reset
	r.wol("pc-a", map[string]any{"off": true})
	if w := r.host("pc-a", "wol"); w["on"] != false || w["from"] != "panel" {
		t.Errorf("off: %v", w)
	}
	if k := r.wake("pc-a"); k.code == 200 || len(r.wakes()) != 2 {
		t.Errorf("a wake of a machine whose Wake-on-LAN is off: %d %s", k.code, k.raw)
	}
	if res := r.confirmed("pc-a", map[string]any{"reset": []any{map[string]any{"field": "wol"}}}); res["ok"] != true {
		t.Fatalf("Reset: %v", res)
	}
	if w := r.host("pc-a", "wol"); w["on"] != true || w["from"] != "config" || r.storedOptions()["hosts"] != nil {
		t.Errorf("after Reset: %v", w)
	}
	// a machine without a wol: gets one here, MACs and a relay together
	r.wol("pc-b", map[string]any{"macs": []string{"02:00:00:00:00:b1"}, "relay": "relay-b"})
	r.off("pc-b")
	if k := r.wake("pc-b"); k.code != 200 {
		t.Fatalf("wake of pc-b: %d %s", k.code, k.raw)
	}
	if got := r.wakes(); len(got) != 3 || got[2] != "relay-b POST /wol map[broadcast:<nil> mac:02:00:00:00:00:b1 port:9]" {
		t.Errorf("the wake of pc-b: %q", got)
	}
	// "off" for a machine that has none: nothing to change
	if res := r.confirmed("box-d", map[string]any{"wol": map[string]any{"port": 9}}); res["ok"] != true || !strings.Contains(fmt.Sprint(res["message"]), "nothing to change") {
		t.Errorf("config.yaml's own port: %v", res)
	}
	// With a target address (box-c) and a password (box-d) in config.yaml: another MAC can be decided, and the
	// address and the password go out as config.yaml has them, through config.yaml's relay.
	r.wol("box-c", map[string]any{"macs": []string{"02:00:00:00:00:c9"}})
	r.wol("box-d", map[string]any{"macs": []string{"02:00:00:00:00:d9"}})
	r.off("box-c")
	r.off("box-d")
	if c, d := r.wake("box-c"), r.wake("box-d"); c.code != 200 || d.code != 200 {
		t.Fatalf("wake of box-c %d, box-d %d", c.code, d.code)
	}
	got := r.wakes()
	if len(got) != 5 || got[3] != "relay-a POST /wol map[broadcast:10.20.30.255 mac:02:00:00:00:00:c9 port:9]" ||
		got[4] != "relay-a POST /wol map[broadcast:<nil> mac:02:00:00:00:00:d9 password:02:00:00:00:00:99 port:9]" {
		t.Errorf("the wakes of box-c and box-d: %q", got[3:])
	}
}

// A name and a site move nothing a machine is asked: the same panel with and without them sends the same
// requests for a wake and a shutdown. A Wake-on-LAN decision changes the wake packet's MAC, relay and port,
// and nothing else that goes to a machine.
func TestEditHostWolIsTheOneThingAControlPathReads(t *testing.T) {
	look := func(change func(r *hedRig)) (requests string) {
		r := hedSetup(t)
		change(r)
		r.off("pc-a")
		r.b.Mu.Lock()
		r.b.Requests = nil
		r.b.Mu.Unlock()
		name := fmt.Sprint(r.row("pc-b")["name"])
		w := r.wake("pc-a")
		s := r.do("POST", "/api/systems/pc-b/power", map[string]any{"action": "shutdown", "delay_s": 60, "confirm": name}, append([]string{"Content-Type", "application/json"}, r.hdr...)...)
		if w.code != 200 || s.code != 200 {
			t.Fatalf("wake %d %s, shutdown %d %s", w.code, w.raw, s.code, s.raw)
		}
		r.b.Mu.Lock()
		defer r.b.Mu.Unlock()
		return strings.Join(r.b.Requests, "\n")
	}
	plain := look(func(*hedRig) {})
	if !strings.Contains(plain, "relay-a POST /wol map[broadcast:<nil> mac:02:00:00:00:00:A1 port:9]") || !strings.Contains(plain, "pc-b POST /power") {
		t.Fatalf("the plain panel: %q", plain)
	}
	labels := look(func(r *hedRig) {
		r.ok("pc-a", map[string]any{"name": "Gaming PC", "site": "site-b"})
		r.ok("pc-b", map[string]any{"name": "Den", "site": "site-a"})
		r.ok("relay-a", map[string]any{"name": "Closet", "site": "site-b"})
	})
	if labels != plain {
		t.Errorf("with other names and sites the machines are asked something else:\n%s\n%s", plain, labels)
	}
	decided := look(func(r *hedRig) {
		r.wol("pc-a", map[string]any{"macs": []string{"02:00:00:00:00:ee"}, "relay": "relay-b", "port": 7})
	})
	if want := strings.Replace(plain, "relay-a POST /wol map[broadcast:<nil> mac:02:00:00:00:00:A1 port:9]", "relay-b POST /wol map[broadcast:<nil> mac:02:00:00:00:00:ee port:7]", 1); decided != want {
		t.Errorf("with a Wake-on-LAN decision:\n%s\nwant\n%s", decided, want)
	}
}

// A state.json as build 15 left it: the panel starts, the link decisions count, and a change to a machine
// itself adds "hosts" next to them without touching them. A key of a newer build stays through that save.
func TestEditHostTheMachineItselfOnABuild15StateFile(t *testing.T) {
	cfg := hedConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	b15 := `{"wake_update_hosts": ["pc-b"], "host_models": {"pc-a": "Tower"},
		"host_options": {"rev": 7, "later": {"x": 1}, "agents": {"pc-a": {"power_exclude_from_total": {"desk": true}, "by": "someone", "at": "2026-11-14T05:00:00Z", "newer": [1, 2]}}}}`
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "state.json"), []byte(b15), 0o600); err != nil {
		t.Fatal(err)
	}
	r := hedStart(t, cfg, hedBackend())
	g := r.options("pc-a")
	if desk := ehEntry(t, g, "plugs", "pc-a/desk"); desk["in_total"] != false || desk["total_from"] != "panel" {
		t.Fatalf("the link decision of the file: %v", desk)
	}
	if g.body["rev"] != 7.0 || r.host("pc-a", "name")["from"] != "config" {
		t.Fatalf("the file: %s", g.raw)
	}
	r.ok("pc-a", map[string]any{"name": "Gaming PC"})
	o := r.storedOptions()
	agent := r.storedAgent("pc-a")
	if o["rev"] != 8.0 || o["later"] == nil || agent["newer"] == nil || agent["by"] != "someone" || fmt.Sprint(agent["power_exclude_from_total"]) != "map[desk:true]" || r.storedHost("pc-a")["name"] != "Gaming PC" {
		t.Errorf("after a change of the name: %v", o)
	}
	st := map[string]any{}
	raw, _ := os.ReadFile(filepath.Join(cfg.DataDir, "state.json"))
	if err := json.Unmarshal(raw, &st); err != nil || fmt.Sprint(st["wake_update_hosts"]) != "[pc-b]" || fmt.Sprint(st["host_models"]) != "map[pc-a:Tower]" {
		t.Errorf("the rest of state.json: %s", raw)
	}
}

// What is kept about a machine that is no machine of this panel any more has no dialog to reset it in: it is
// counted with the other leftovers, used for nothing, and "Remove" takes it out.
func TestEditHostLeftoversOfMachinesThemselves(t *testing.T) {
	cfg := hedConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	state := `{"host_options": {"rev": 2, "hosts": {"old-pc": {"name": "Old", "wol": {"macs": ["02:00:00:00:00:0d"], "relay": "relay-a"}}, "pc-a": {"name": "Gaming PC"}}}}`
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "state.json"), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	r := hedStart(t, cfg, hedBackend())
	g := r.options("pc-b")
	if g.body["leftover_count"] != 3.0 || !slices.Equal(strs(g.body["leftovers"]), []string{"3 stored changes kept with old-pc, which is no machine of this panel"}) {
		t.Fatalf("the leftovers: %s", g.raw)
	}
	for _, s := range r.s.Poller.Snapshot() {
		if s.Name == "Old" || slices.Contains(s.PowerActions, "wake") && s.ID != "pc-a" && s.ID != "box-c" && s.ID != "box-d" {
			t.Errorf("%s uses what is kept for old-pc: %q %v", s.ID, s.Name, s.PowerActions)
		}
	}
	p := r.ok("pc-b", map[string]any{"leftovers": true})
	if !slices.Equal(strs(p.body["changed"]), []string{"Removed: 3 stored changes kept with old-pc, which is no machine of this panel"}) || p.body["leftover_count"] != 0.0 {
		t.Errorf("Remove: %s", p.raw)
	}
	if r.storedHost("old-pc") != nil || r.storedHost("pc-a")["name"] != "Gaming PC" {
		t.Errorf("stored: %v", r.storedOptions())
	}
}

// Remove of a host (and an Add under an id something is still kept for) takes what was changed about the
// machine itself with it, and the relay decisions of other machines that name it.
func TestEditHostForgetTakesTheMachineItself(t *testing.T) {
	r := hedSetup(t)
	r.ok("pc-b", map[string]any{"name": "Den"})
	r.wol("pc-a", map[string]any{"relay": "relay-b", "port": 7})
	gone, _, err := r.s.forgetHostOptions("test", "relay-b")
	if err != nil || len(gone) != 1 || !strings.Contains(gone[0], "pc-a: it is no longer the relay") {
		t.Fatalf("Remove of relay-b: %q %v", gone, err)
	}
	if st, _ := r.storedHost("pc-a")["wol"].(map[string]any); st["relay"] != nil || st["port"] != 7.0 {
		t.Errorf("pc-a after its relay was removed: %v", st)
	}
	if w := r.host("pc-a", "wol"); w["relay"] != "relay-a" || w["port"] != 7.0 || w["unused"] != nil {
		t.Errorf("pc-a's Wake-on-LAN after it: %v", w)
	}
	gone, _, err = r.s.forgetHostOptions("test", "pc-b")
	if err != nil || len(gone) != 1 || r.storedHost("pc-b") != nil || r.row("pc-b")["name"] != "PC-B" {
		t.Errorf("Remove of pc-b: %q %v, row %v", gone, err, r.row("pc-b")["name"])
	}
}
