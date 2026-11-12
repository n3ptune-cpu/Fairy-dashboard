package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// "Set model name": an admin gives a host a model name of the panel's own (PUT /api/systems/{id}/custom-model).
// These tests ask the real handler chain who may do that, what a name may be, where it is kept, and that it
// is text wherever it comes out again.

const reportedModel = "Lenovo ThinkPad T14" // what the laptop of these tests reports about itself

// modelPanel is a panel whose laptop reports a model (the plain fixture reports none; the tower never
// answers, so it has none), with an admin's cookie and the headers a page sends with a write.
func modelPanel(t *testing.T) (*env, []string) {
	t.Helper()
	e := setup(t)
	e.b.Mu.Lock()
	e.b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["hardware"] = jsonx.M{"system_vendor": "LENOVO", "system_model": "ThinkPad T14"}
		return m, nil
	}
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(context.Background())
	return e, []string{"Cookie", e.cookie(controller(), 0), "X-Requested-With", "fairy"}
}

// restart is the panel started again on the same folders: a new poller and server that read state.json,
// users.json and the audit log the first one left.
func (e *env) restart() *env {
	e.t.Helper()
	p, err := poller.New(e.s.Cfg, e.s.Secrets, e.b)
	if err != nil {
		e.t.Fatal(err)
	}
	p.PollOnce(context.Background())
	s := New(e.s.Cfg, e.s.Secrets, p, nil, "test")
	s.Enroll.Scripts = testInstallers
	srv := httptest.NewServer(s.Handler())
	e.t.Cleanup(srv.Close)
	return &env{t: e.t, s: s, srv: srv, b: e.b}
}

func (e *env) setModel(id string, name any, hdr ...string) resp {
	e.t.Helper()
	return e.do("PUT", "/api/systems/"+id+"/custom-model", map[string]any{"custom_model": name}, append([]string{"Content-Type", "application/json"}, hdr...)...)
}

// models returns a system's two model names as the API hands them out: the custom one and the reported one
// (nil: none).
func (e *env) models(id string, hdr ...string) (custom, reported any) {
	e.t.Helper()
	r := e.do("GET", "/api/systems/"+id, nil, hdr...)
	if r.code != 200 {
		e.t.Fatalf("GET %s: %d %s", id, r.code, r.raw)
	}
	if _, has := r.body["custom_model"]; !has {
		e.t.Fatalf("%s: the system has no custom_model key: %s", id, r.raw)
	}
	hw, _ := r.body["hardware"].(map[string]any)
	return r.body["custom_model"], hw["display"]
}

// Who may set it: an admin from the panel's own page, with a browser session. Nobody else, and nothing a
// refused request sent is stored.
func TestCustomModelWhoMaySetIt(t *testing.T) {
	e, admin := modelPanel(t)
	csrf := []string{"X-Requested-With", "fairy"}
	body := map[string]any{"custom_model": "Gaming PC"}
	put := func(hdr ...string) resp {
		return e.do("PUT", "/api/systems/laptop/custom-model", body, append([]string{"Content-Type", "application/json"}, hdr...)...)
	}
	// nobody is signed in: 401 as JSON, never a redirect, with or without the page's header
	for _, hdr := range [][]string{nil, csrf} {
		if r := put(hdr...); r.code != 401 || r.body["detail"] != "not authenticated" || r.hdr.Get("Location") != "" {
			t.Fatalf("not signed in: %d %s", r.code, r.raw)
		}
	}
	// signed in, not an admin: a viewer (who sees the laptop) and a user who waits for access
	v := model.User{Sub: "u7", Name: model.P("Vee"), Email: model.P("v@example.com"), Groups: []string{"viewers"}}
	vc := e.cookie(v, 0)
	_ = e.s.Access.EnsureSites([]string{"Home"})
	if _, err := e.s.Access.Assign("laptop", "Home"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.Access.SetRole("u7", "viewer", []string{"Home"}, "test", false); err != nil {
		t.Fatal(err)
	}
	if e.do("GET", "/api/systems/laptop", nil, "Cookie", vc).code != 200 {
		t.Fatal("the viewer of this test sees the laptop")
	}
	if r := put(append([]string{"Cookie", vc}, csrf...)...); r.code != 403 || r.body["detail"] != "only admins can set a host's model name" {
		t.Fatalf("a viewer: %d %s", r.code, r.raw)
	}
	pend := e.cookie(model.User{Sub: "u8", Name: model.P("Pat"), Groups: []string{"viewers"}}, 0)
	if r := put(append([]string{"Cookie", pend}, csrf...)...); r.code != 403 {
		t.Fatalf("a user waiting for access: %d %s", r.code, r.raw)
	}
	// API tokens, of either scope: refused, like on the other settings an admin makes in the browser
	for _, scope := range []string{"read", "control"} {
		tok, _, err := e.s.Tokens.Create("t-"+scope, "system", scope, "cli:test", 0)
		if err != nil {
			t.Fatal(err)
		}
		if r := put("Authorization", "Bearer "+tok); r.code != 403 || r.body["detail"] != "API tokens are not accepted on this endpoint" {
			t.Fatalf("a %s token: %d %s", scope, r.code, r.raw)
		}
		if r := put("Authorization", "Bearer "+tok, admin[0], admin[1], "X-Requested-With", "fairy"); r.code != 403 {
			t.Fatalf("a %s token next to an admin's cookie: %d %s", scope, r.code, r.raw)
		}
	}
	if r := put("Authorization", "Bearer fy_not-a-token"); refused(r) == false {
		t.Fatalf("a token the panel does not know: %d", r.code)
	}
	// an admin's cookie, but not from the panel's own page
	ck := admin[:2]
	for name, hdr := range map[string][]string{
		"without the CSRF header":    ck,
		"from another origin":        append([]string{"Origin", "https://evil.example"}, admin...),
		"an origin that looks alike": append([]string{"Origin", "https://panel.example.com.evil.example"}, admin...),
		"http instead of https":      append([]string{"Origin", "http://panel.example.com"}, admin...),
		"Sec-Fetch-Site: cross-site": append([]string{"Sec-Fetch-Site", "cross-site"}, admin...),
		"Sec-Fetch-Site: same-site":  append([]string{"Sec-Fetch-Site", "same-site"}, admin...),
		"the wrong header value":     append([]string{"X-Requested-With", "XMLHttpRequest"}, ck...),
	} {
		if r := put(hdr...); r.code != 403 {
			t.Errorf("an admin's cookie %s: %d %s (want 403)", name, r.code, r.raw)
		}
	}
	if c, _ := e.models("laptop", ck...); c != nil {
		t.Fatalf("a refused request set the name: %v", c)
	}
	if n := len(e.auditEntries("", "host.model")); n != 0 {
		t.Fatalf("%d host.model entries for refused requests", n)
	}
	// the admin, from the panel's page (its own origin named, as a browser does)
	if r := put(append([]string{"Origin", "https://panel.example.com", "Sec-Fetch-Site", "same-origin"}, admin...)...); r.code != 200 {
		t.Fatalf("the admin: %d %s", r.code, r.raw)
	}
	if c, _ := e.models("laptop", ck...); c != "Gaming PC" {
		t.Fatalf("after the admin's request: %v", c)
	}
	// it is a label: an admin whose sign-in is an hour old is not asked to confirm, and the kill switch,
	// which stops device control, does not stop it
	body = map[string]any{"custom_model": "Office PC"}
	old := e.staleCookie(controller(), time.Hour)
	if r := put("Cookie", old, "X-Requested-With", "fairy"); r.code != 200 || stepUpRequired(r) {
		t.Fatalf("an admin whose sign-in is an hour old: %d %s", r.code, r.raw)
	}
	if err := e.s.Poller.Admin.SetControlDisabled(true, "test", "kill switch"); err != nil {
		t.Fatal(err)
	}
	body = map[string]any{"custom_model": "Gaming PC"}
	if r := put(admin...); r.code != 200 {
		t.Fatalf("with the kill switch on: %d %s", r.code, r.raw)
	}
	// who did it is on record: the admin, the host, the old and the new name
	got := e.auditEntries("", "host.model")
	if len(got) != 3 {
		t.Fatalf("want 3 host.model entries, got %v", got)
	}
	last, _ := got[0]["params"].(map[string]any) // newest first
	if got[0]["actor"] != "n@example.com"+unverifiedMark || got[0]["sub"] != "u2" || got[0]["target"] != "laptop" || got[0]["outcome"] != "ok" ||
		got[0]["source"] != "ui" || last["model"] != "Gaming PC" || last["before"] != "Office PC" {
		t.Fatalf("the audit entry: %v", got[0])
	}
	// many changes in a minute: the limit of an admin's changes answers, the name stays
	e.s.adminRL = newRateLimiter(2)
	codes := ""
	for i := 0; i < 4; i++ {
		codes += " " + strings.TrimSpace(string(rune('0'+put(admin...).code/100)))
	}
	if codes != " 2 2 4 4" {
		t.Fatalf("the rate limit: %s", codes)
	}
}

// What a name may be is decided by the panel, whatever the page sent. Each refusal is a 422 with a sentence,
// in the shape every route's error has, and leaves the name as it was.
func TestCustomModelIsCheckedByThePanel(t *testing.T) {
	e, admin := modelPanel(t)
	e.s.adminRL = newRateLimiter(1000)
	put := func(id, rawBody string) resp {
		t.Helper()
		r := e.doRaw("PUT", "/api/systems/"+id+"/custom-model", rawBody, admin...)
		_ = json.Unmarshal(r.raw, &r.body)
		return r
	}
	if r := put("laptop", `{"custom_model": "Gaming PC"}`); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	long := strings.Repeat("x", 65)
	for name, body := range map[string]string{
		// the name
		"65 characters":             `{"custom_model": "` + long + `"}`,
		"65 characters and spaces":  `{"custom_model": "   ` + strings.Repeat("й", 65) + `   "}`,
		"a line feed":               `{"custom_model": "Gaming\nPC"}`,
		"a carriage return":         `{"custom_model": "Gaming\rPC"}`,
		"a tab":                     `{"custom_model": "Gaming\tPC"}`,
		"NUL":                       `{"custom_model": "Gaming\u0000PC"}`,
		"escape (ANSI)":             `{"custom_model": "\u001b[31mred"}`,
		"DEL":                       `{"custom_model": "Gaming\u007fPC"}`,
		"a C1 control":              `{"custom_model": "Gaming\u0085PC"}`,
		"the line separator":        `{"custom_model": "Gaming\u2028PC"}`,
		"the paragraph separator":   `{"custom_model": "Gaming\u2029PC"}`,
		"right-to-left override":    `{"custom_model": "PC \u202egnimaG"}`,
		"the override, as it is":    "{\"custom_model\": \"PC \xe2\x80\xaegnimaG\"}",
		"left-to-right embedding":   `{"custom_model": "\u202aGaming PC"}`,
		"direction isolates":        `{"custom_model": "\u2066Gaming\u2069 PC"}`,
		"right-to-left mark":        `{"custom_model": "Gaming\u200fPC"}`,
		"Arabic letter mark":        `{"custom_model": "Gaming\u061cPC"}`,
		"zero-width space":          `{"custom_model": "Gaming\u200bPC"}`,
		"zero-width joiner":         `{"custom_model": "Gaming\u200dPC"}`,
		"byte order mark":           `{"custom_model": "\ufeffGaming PC"}`,
		"soft hyphen":               `{"custom_model": "Gam\u00ading PC"}`,
		"a tag character":           `{"custom_model": "Gaming PC\udb40\udc41"}`,
		"a private-use character":   `{"custom_model": "Gaming \ue000 PC"}`,
		"a filler that shows blank": `{"custom_model": "\u3164"}`,
		"half a surrogate pair":     `{"custom_model": "Gaming \ud83c PC"}`,
		"the replacement character": `{"custom_model": "Gaming \ufffd PC"}`,
		"bytes that are not UTF-8":  "{\"custom_model\": \"Gaming \xff\xfe PC\"}",
		"a cut-off UTF-8 sequence":  "{\"custom_model\": \"Gaming PC \xe6\xb8\"}",
		"Latin-1 bytes":             "{\"custom_model\": \"B\xfcro-PC\"}",
		// the request
		"no field":            `{}`,
		"null":                `{"custom_model": null}`,
		"a number":            `{"custom_model": 5}`,
		"a list":              `{"custom_model": ["Gaming PC"]}`,
		"an object":           `{"custom_model": {"name": "Gaming PC"}}`,
		"another field":       `{"custom_model": "Gaming PC", "reported_model": "x"}`,
		"the field twice":     `{"custom_model": "A", "custom_model": "B"}`,
		"another spelling":    `{"custom_model": "A", "CUSTOM_MODEL": "B"}`,
		"only another case":   `{"Custom_Model": "B"}`,
		"something after it":  `{"custom_model": "A"} {"custom_model": "B"}`,
		"not JSON":            `custom_model=Gaming+PC`,
		"an empty body":       ``,
		"a body over 64 KiB":  `{"custom_model": "` + strings.Repeat("x", 70<<10) + `"}`,
		"spaces, then 64 KiB": strings.Repeat(" ", 64<<10) + `{"custom_model": "B"}`,
	} {
		r := put("laptop", body)
		detail, isText := r.body["detail"].(string)
		if r.code != 422 || !isText || detail == "" || len(r.body) != 1 {
			t.Errorf("%s: %d %.200s (want 422 with a detail text)", name, r.code, r.raw)
		}
		if strings.Contains(string(r.raw), "\xff") || strings.Contains(string(r.raw), long) {
			t.Errorf("%s: the answer repeats what was sent: %.200s", name, r.raw)
		}
	}
	if c, _ := e.models("laptop", admin[:2]...); c != "Gaming PC" {
		t.Fatalf("a refused name replaced the one that was set: %v", c)
	}
	if n := len(e.auditEntries("", "host.model")); n != 1 {
		t.Fatalf("refused names are not changes: %d host.model entries", n)
	}
	// what the dialog shows for the common mistakes
	for body, want := range map[string]string{
		`{"custom_model": "` + long + `"}`:                          "model name too long: at most 64 characters",
		`{"custom_model": "Gaming\nPC"}`:                            "model name must be one line, without control characters",
		`{"custom_model": "PC \u202egnimaG"}`:                       "model name has an invisible character: type it again",
		`{"custom_model": "\u0301\u0302"}`:                          "model name needs a letter, a digit or a sign",
		`{"custom_model": "Z` + strings.Repeat(`\u0351`, 20) + `"}`: "model name has too many accents on one character",
		"{\"custom_model\": \"Gaming \xff PC\"}":                    "invalid request body: not valid UTF-8",
		`{"custom_model": "Gaming \ud83c PC"}`:                      "model name is not valid text",
		`{"custom_model": null}`:                                    "custom_model must be a text",
		`{"custom_model": "A", "extra": "field"}`:                   "invalid request body",
		`{"custom_model": "A", "custom_model": 1}`:                  "invalid request body",
	} {
		if r := put("laptop", body); r.body["detail"] != want {
			t.Errorf("%.60s: %q, want %q", body, r.body["detail"], want)
		}
	}
	// what is accepted, and how it is stored
	for body, want := range map[string]any{
		`{"custom_model": "  Gaming   PC  "}`:                                 "Gaming PC",
		`{"custom_model": "Gaming\u00a0\u3000PC"}`:                            "Gaming PC",
		`{"custom_model": "` + strings.Repeat("x", 64) + `"}`:                 strings.Repeat("x", 64),
		`{"custom_model": "` + strings.Repeat("й", 64) + `"}`:                 strings.Repeat("й", 64), // 128 bytes, 64 characters
		`{"custom_model": "  ` + strings.Repeat("x", 64) + `      "}`:         strings.Repeat("x", 64),
		`{"custom_model": "Büro-PC №2 (Keller) 游戏电脑"}`:                        "Büro-PC №2 (Keller) 游戏电脑",
		`{"custom_model": "Gaming PC \ud83c\udfae"}`:                          "Gaming PC 🎮",
		` { "custom_model" : "ThinkCentre M720q \"Tiny\" 10T7-001" } ` + "\n": `ThinkCentre M720q "Tiny" 10T7-001`,
		`{"custom_model": "   "}`:                                             nil,
	} {
		r := put("laptop", body)
		if c, _ := e.models("laptop", admin[:2]...); r.code != 200 || r.body["custom_model"] != want || c != want {
			t.Errorf("%.70s: %d %s; stored %v, want %v", body, r.code, r.raw, c, want)
		}
	}
	// which system: an unknown one is 404 like everywhere, a VM or container has no model name, a node has
	if r := put("nope", `{"custom_model": "X"}`); r.code != 404 || r.body["detail"] != "system not found" {
		t.Fatalf("an unknown system: %d %s", r.code, r.raw)
	}
	if r := put("..%2Flaptop", `{"custom_model": "X"}`); r.code != 404 {
		t.Fatalf("an id that is a path: %d %s", r.code, r.raw)
	}
	for _, id := range []string{"qemu-100", "lxc-101"} {
		if r := put(id, `{"custom_model": "X"}`); r.code != 409 || r.body["detail"] != "only hosts have a model name" {
			t.Fatalf("%s: %d %s", id, r.code, r.raw)
		}
	}
	if r := put("node-srv-pvea", `{"custom_model": "Dell R730"}`); r.code != 200 {
		t.Fatalf("a Proxmox node: %d %s", r.code, r.raw)
	}
	if c, _ := e.models("node-srv-pvea", admin[:2]...); c != "Dell R730" {
		t.Fatal("the node's name:", c)
	}
	if st := string(must(os.ReadFile(filepath.Join(e.s.Cfg.DataDir, "state.json")))); strings.Contains(st, "nope") || strings.Contains(st, "qemu-100") {
		t.Fatalf("state.json holds a name for something that is not a host: %s", st)
	}
}

// The custom name is shown in place of the reported one and never replaces it: the API carries both, a host
// that reports nothing can have one, and removing it brings back what the machine reports (or nothing).
func TestCustomModelShowsInPlaceOfTheReportedOne(t *testing.T) {
	e, admin := modelPanel(t)
	ck := admin[:2]
	if c, rep := e.models("laptop", ck...); c != nil || rep != reportedModel {
		t.Fatalf("before: custom %v, reported %v", c, rep)
	}
	if c, rep := e.models("tower", ck...); c != nil || rep != nil {
		t.Fatalf("the tower reports nothing: custom %v, reported %v", c, rep)
	}
	r := e.setModel("laptop", "Gaming PC", admin...)
	if r.code != 200 || r.body["ok"] != true || r.body["custom_model"] != "Gaming PC" || r.body["reported_model"] != reportedModel ||
		r.body["message"] != "laptop: model name set" {
		t.Fatalf("set: %d %s", r.code, r.raw)
	}
	if c, rep := e.models("laptop", ck...); c != "Gaming PC" || rep != reportedModel {
		t.Fatalf("after set: custom %v, reported %v (the reported model must stay)", c, rep)
	}
	// the list, the stream every open page reads, and an API token that only reads carry it too
	list := e.do("GET", "/api/systems?kind=agent", nil, ck...)
	n := 0
	for _, x := range list.body["systems"].([]any) {
		m := x.(map[string]any)
		if _, has := m["custom_model"]; !has {
			t.Fatalf("%v has no custom_model key", m["id"])
		}
		if m["id"] == "laptop" && m["custom_model"] == "Gaming PC" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the list: %s", list.raw)
	}
	if code, body := e.streamBody("/api/stream", ck...); code != 200 || !strings.Contains(body, `"custom_model":"Gaming PC"`) {
		t.Fatalf("the stream: %d %.300s", code, body)
	}
	tok, _, _ := e.s.Tokens.Create("ha", "system", "read", "cli:test", 0)
	if c, rep := e.models("laptop", "Authorization", "Bearer "+tok); c != "Gaming PC" || rep != reportedModel {
		t.Fatalf("a read token: %v, %v", c, rep)
	}
	// other hosts are not touched; a host without a reported model can have a name
	if c, _ := e.models("tower", ck...); c != nil {
		t.Fatal("the tower got the laptop's name")
	}
	if r := e.setModel("tower", "Supermicro X11", admin...); r.code != 200 || r.body["custom_model"] != "Supermicro X11" || r.body["reported_model"] != nil {
		t.Fatalf("the tower: %d %s", r.code, r.raw)
	}
	// the same name again is no change: no second audit entry
	if r := e.setModel("laptop", " Gaming  PC ", admin...); r.code != 200 || len(e.auditEntries("", "host.model")) != 2 {
		t.Fatalf("the same name again: %d, %d entries", r.code, len(e.auditEntries("", "host.model")))
	}
	// removing it: the reported model is what is left; for the tower nothing
	r = e.setModel("laptop", "", admin...)
	if r.code != 200 || r.body["custom_model"] != nil || r.body["reported_model"] != reportedModel ||
		r.body["message"] != "laptop: model name removed" {
		t.Fatalf("remove: %d %s", r.code, r.raw)
	}
	if c, rep := e.models("laptop", ck...); c != nil || rep != reportedModel {
		t.Fatalf("after remove: custom %v, reported %v", c, rep)
	}
	if r := e.setModel("tower", "", admin...); r.code != 200 || r.body["message"] != "tower: model name removed" {
		t.Fatalf("remove on the tower: %d %s", r.code, r.raw)
	}
	if c, rep := e.models("tower", ck...); c != nil || rep != nil {
		t.Fatalf("the tower after remove: %v, %v", c, rep)
	}
	if r := e.setModel("laptop", "", admin...); r.code != 200 || len(e.auditEntries("", "host.model")) != 4 {
		t.Fatalf("removing none is not a change: %d, %d entries", r.code, len(e.auditEntries("", "host.model")))
	}
	// the audit log, newest first: what was removed and what was set, with the name before
	var lines []string
	for _, en := range e.auditEntries("", "host.model") {
		p, _ := en["params"].(map[string]any)
		lines = append(lines, en["target"].(string)+": "+p["before"].(string)+" -> "+p["model"].(string))
	}
	if got := strings.Join(lines, "; "); got != "tower: Supermicro X11 -> ; laptop: Gaming PC -> ; tower:  -> Supermicro X11; laptop:  -> Gaming PC" {
		t.Fatalf("the audit log: %s", got)
	}
	// the Audit page's "Host admin" filter lists them
	if r := e.do("GET", "/api/audit?action=host", nil, ck...); r.code != 200 || len(r.body["entries"].([]any)) != 4 {
		t.Fatalf("the Audit page: %d %s", r.code, r.raw)
	}
}

// The name is the panel's and is kept in state.json: a panel that starts again shows it. A state.json from
// before the feature, or with something else where the names should be, never keeps the panel from starting.
func TestCustomModelSurvivesARestart(t *testing.T) {
	e, admin := modelPanel(t)
	if r := e.setModel("laptop", "Gaming PC", admin...); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	if r := e.setModel("tower", "Supermicro X11", admin...); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	path := filepath.Join(e.s.Cfg.DataDir, "state.json")
	if st := string(must(os.ReadFile(path))); !strings.Contains(st, `"host_models"`) || !strings.Contains(st, `"laptop": "Gaming PC"`) {
		t.Fatalf("state.json: %s", st)
	}
	e2 := e.restart()
	ck := []string{"Cookie", e2.cookie(controller(), 0)}
	if c, rep := e2.models("laptop", ck...); c != "Gaming PC" || rep != reportedModel {
		t.Fatalf("after a restart: custom %v, reported %v", c, rep)
	}
	if c, _ := e2.models("tower", ck...); c != "Supermicro X11" {
		t.Fatalf("the tower after a restart: %v", c)
	}
	// state.json as a panel from before this feature left it
	for name, content := range map[string]string{
		"a state.json without the field":  `{"poll_overrides": {}, "power_control_disabled": [], "wake_update_hosts": ["laptop"]}`,
		"host_models is a number":         `{"host_models": 7, "wake_update_hosts": ["laptop"]}`,
		"host_models is a list":           `{"host_models": ["laptop", "Gaming PC"], "wake_update_hosts": ["laptop"]}`,
		"names that are not names":        `{"host_models": {"laptop": 5, "tower": "bad\nname", "nas": {"a": 1}}, "wake_update_hosts": ["laptop"]}`,
		"one bad name next to a good one": `{"host_models": {"laptop": "PC \u202egnimaG", "tower": "Supermicro X11"}, "wake_update_hosts": ["laptop"]}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		e3 := e.restart()
		ck := []string{"Cookie", e3.cookie(controller(), 0), "X-Requested-With", "fairy"}
		me := e3.do("GET", "/api/me", nil, ck[:2]...)
		if me.code != 200 || me.body["control_disabled"] != nil || !e3.s.Poller.Admin.WakeUpdateEnabled("laptop") {
			t.Fatalf("%s: the panel must start and read the rest of its state: %d %s", name, me.code, me.raw)
		}
		c, rep := e3.models("laptop", ck[:2]...)
		if c != nil || rep != reportedModel {
			t.Errorf("%s: the laptop shows custom %v, reported %v (want the reported model alone)", name, c, rep)
		}
		wantTower := any(nil)
		if name == "one bad name next to a good one" {
			wantTower = "Supermicro X11"
		}
		if c, _ := e3.models("tower", ck[:2]...); c != wantTower {
			t.Errorf("%s: the tower shows %v, want %v", name, c, wantTower)
		}
		if r := e3.setModel("laptop", "Gaming PC", ck...); r.code != 200 {
			t.Errorf("%s: a name can be set afterwards: %d %s", name, r.code, r.raw)
		}
		if c, _ := e3.models("laptop", ck[:2]...); c != "Gaming PC" || !e3.s.Poller.Admin.WakeUpdateEnabled("laptop") {
			t.Errorf("%s: after setting one: %v", name, c)
		}
	}
}

// Remove host takes the host's custom model name with it. A host's id is made from its name, so the next
// machine added under that name would otherwise show the model somebody typed for the old one.
func TestRemoveHostForgetsItsCustomModel(t *testing.T) {
	e, admin := modelPanel(t)
	e.s.Enroll.Interval = 20 * time.Millisecond
	e.s.control = newRateLimiter(100)
	e.s.Enroll.PanelAddr = "100.64.0.10"
	e.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
		return 200, enroll.Response{AgentVersion: "42-abc", Platform: "linux", Token: "winpc-read-token-xxxxxxxxxxxxxxxx",
			ControlToken: "winpc-control-token-yyyyyyyyyyyyyy"}, "", nil
	}
	e.b.Mu.Lock()
	e.b.Metrics["srv-winpc"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["hardware"] = jsonx.M{"system_vendor": "ASUS", "system_model": "ROG Strix G15"}
		return m, nil
	}
	e.b.Mu.Unlock()
	add := func() {
		t.Helper()
		r := e.do("POST", "/api/admin/hosts", map[string]any{"name": "SRV-WINPC", "os": "windows"}, admin...)
		if r.code != 201 {
			t.Fatal(r.code, string(r.raw))
		}
		id, st := r.body["id"].(string), ""
		for i := 0; i < 200 && st != "online"; i++ {
			time.Sleep(20 * time.Millisecond)
			e.s.Poller.PollOnce(context.Background())
			st, _ = e.do("GET", "/api/admin/enrollments/"+id, nil, admin[:2]...).body["state"].(string)
		}
		if st != "online" {
			t.Fatal("the host did not pair:", st)
		}
	}
	add()
	if c, rep := e.models("srv-winpc", admin[:2]...); c != nil || rep != "ASUS ROG Strix G15" {
		t.Fatalf("a new host: custom %v, reported %v", c, rep)
	}
	if r := e.setModel("srv-winpc", "Old gaming PC", admin...); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	if r := e.setModel("laptop", "Gaming PC", admin...); r.code != 200 { // another host's name must stay
		t.Fatal(r.code, string(r.raw))
	}
	if r := e.do("DELETE", "/api/admin/hosts/srv-winpc", map[string]any{"confirm": "SRV-WINPC"}, admin...); r.code != 200 {
		t.Fatal("remove", r.code, string(r.raw))
	}
	if got := e.s.Poller.Admin.HostModels(); len(got) != 1 || got["laptop"] != "Gaming PC" {
		t.Fatalf("the names after Remove: %v", got)
	}
	if st := string(must(os.ReadFile(filepath.Join(e.s.Cfg.DataDir, "state.json")))); strings.Contains(st, "srv-winpc") || strings.Contains(st, "Old gaming PC") {
		t.Fatalf("state.json after Remove: %s", st)
	}
	// on record: the name going because of the removal, and the removal saying that there was one
	found := false
	for _, en := range e.auditEntries("", "host.model") {
		p, _ := en["params"].(map[string]any)
		found = found || (en["target"] == "srv-winpc" && p["model"] == "" && p["before"] == "Old gaming PC" && p["because"] == "host removed")
	}
	rm := e.auditEntries("", "host.remove")
	if p, _ := rm[0]["params"].(map[string]any); !found || len(rm) != 1 || p["custom_model_removed"] != true {
		t.Fatalf("audit: name entry %v; host.remove %v", found, rm)
	}
	// the same name again (another machine, for all the panel knows): it shows what it reports
	add()
	if c, rep := e.models("srv-winpc", admin[:2]...); c != nil || rep != "ASUS ROG Strix G15" {
		t.Fatalf("the host added again under the same name: custom %v, reported %v", c, rep)
	}
	// a host without a custom name: Remove writes no name entry
	n := len(e.auditEntries("", "host.model"))
	if r := e.do("DELETE", "/api/admin/hosts/srv-winpc", map[string]any{"confirm": "SRV-WINPC"}, admin...); r.code != 200 {
		t.Fatal("remove", r.code, string(r.raw))
	}
	rm = e.auditEntries("", "host.remove")
	if p, _ := rm[0]["params"].(map[string]any); len(e.auditEntries("", "host.model")) != n || len(rm) != 2 || p["custom_model_removed"] != false {
		t.Fatalf("second removal: %v", rm)
	}
	if c, _ := e.models("laptop", admin[:2]...); c != "Gaming PC" {
		t.Fatal("removing a host took another host's name:", c)
	}
}

// A name that is markup is a text like any other: the panel stores and hands out exactly what was typed, as
// JSON text (never as HTML), and the page puts it into the document only through esc() or as a property.
func TestCustomModelIsTextNotMarkup(t *testing.T) {
	e, admin := modelPanel(t)
	ck := admin[:2]
	const evil = `<img src=x onerror=alert(1)>"'&`
	r := e.setModel("laptop", evil, admin...)
	if r.code != 200 || r.body["custom_model"] != evil {
		t.Fatalf("set: %d %s", r.code, r.raw)
	}
	one := e.do("GET", "/api/systems/laptop", nil, ck...)
	list := e.do("GET", "/api/systems", nil, ck...)
	aud := e.do("GET", "/api/audit?action=host", nil, ck...)
	_, stream := e.streamBody("/api/stream", ck...)
	if one.body["custom_model"] != evil {
		t.Fatalf("the system: %v", one.body["custom_model"])
	}
	inList := false
	for _, x := range list.body["systems"].([]any) {
		m := x.(map[string]any)
		inList = inList || (m["id"] == "laptop" && m["custom_model"] == evil)
	}
	entry := aud.body["entries"].([]any)[0].(map[string]any)
	if p, _ := entry["params"].(map[string]any); !inList || p["model"] != evil || entry["target"] != "laptop" {
		t.Fatalf("the list (%v) or the audit entry: %v", inList, entry)
	}
	// on the wire every answer is JSON, and none of them holds the tag as it would be in a page
	for name, a := range map[string]resp{"the answer to the change": r, "the system": one, "the list": list, "the audit page": aud} {
		if ct := a.hdr.Get("Content-Type"); ct != "application/json" || a.hdr.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: Content-Type %q", name, ct)
		}
		if strings.Contains(string(a.raw), "<img") || strings.Contains(string(a.raw), "onerror=alert(1)>") {
			t.Errorf("%s holds the tag unescaped: %.300s", name, a.raw)
		}
	}
	if strings.Contains(stream, "<img") || !strings.Contains(stream, `"custom_model":"\u003cimg src=x onerror=alert(1)\u003e\"'\u0026"`) {
		t.Errorf("the stream: %.400s", stream)
	}
	// state.json and the audit log hold the text as it was typed, as JSON
	st := map[string]any{}
	if err := json.Unmarshal(must(os.ReadFile(filepath.Join(e.s.Cfg.DataDir, "state.json"))), &st); err != nil {
		t.Fatal(err)
	}
	if m, _ := st["host_models"].(map[string]any); m["laptop"] != evil {
		t.Fatalf("state.json: %v", st["host_models"])
	}
	// it comes back the same after a restart, and a second change names it as the name before
	e2 := e.restart()
	admin2 := []string{"Cookie", e2.cookie(controller(), 0), "X-Requested-With", "fairy"}
	if c, _ := e2.models("laptop", admin2[:2]...); c != evil {
		t.Fatalf("after a restart: %v", c)
	}
	if r := e2.setModel("laptop", "", admin2...); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	if p, _ := e2.auditEntries("", "host.model")[0]["params"].(map[string]any); p["before"] != evil || p["model"] != "" {
		t.Fatalf("the audit entry of the removal: %v", p)
	}

	// The page. Every place that shows a host's model takes it from model(s) or from the system's own
	// fields, and inserts it through esc(); the dialog sets it as a property, never as markup.
	js := appJS(t)
	for _, want := range []string{
		`const model = (s) => s.custom_model || s.hardware?.display || null;`,          // the custom name first, else the reported one
		`<div class="host-model" title="${esc(modelTitle(s))}">${esc(model(s))}</div>`, // Overview: the line under the name and its tooltip
		`title="${esc(about)}"`,                            // Overview: the name's tooltip (subLine)
		`<div class="sub">${esc(sub)}`,                     // the host's page: the line under its name (subLine)
		`<span>Model</span><span>${esc(model(s))}${bios ?`, // the host's page: the Model row, reported model
		`<span>Model</span><span title="${esc(modelTip(s))}">${esc(s.custom_model)}${infoBtn("host-model", "Model name",`, // ... a custom name alone
		"`<p>${esc(modelTip(s))}${bios ? ` BIOS ${esc(bios)}.` : \"\"}</p>`, { label: modelTip(s) })",                     // ... what is behind it: the info button
		"const modelTip = (s) => `Set by hand. ${s.hardware?.display ? `Reported by the host: ${s.hardware.display}.` : \"The host reports no model.\"}`;",
		`input.value = custom; input.placeholder = reported;`,                     // the dialog: properties
		"input.ariaLabel = `Model name for ${s.name}`;",                           // the dialog: the field's name, a property
		"clear.title = reported ? `Show the reported model again: ${reported}` :", // what Clear does: its tooltip, a property
		`err.textContent = x.message;`,                              // the panel's sentence, as text
		`body: JSON.stringify({ custom_model: name })`,              // sent as JSON
		`${esc(Object.entries(e.params).map(`,                       // the Audit page: an entry's values
		`if (!isHost(s) || !state.me?.can_manage_users) return "";`, // the menu entry: admins, hosts
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// modelTip and modelTitle make plain sentences out of the two names. They are the only places that put a
	// name into a text first; what they return goes into the page through esc() (infoBtn escapes its label).
	page := js
	for _, def := range []string{"const modelTip = (s) => ", "const modelTitle = (s) => "} {
		i := strings.Index(page, def)
		if i < 0 {
			t.Fatalf("app.js lacks %q", def)
		}
		page = page[:i] + page[i+strings.Index(page[i:], "\n"):]
	}
	for _, use := range regexp.MustCompile(`(esc\(|label: |.)model(?:Tip|Title)\(s\)`).FindAllStringSubmatch(page, -1) {
		if use[1] != "esc(" && use[1] != "label: " {
			t.Errorf("app.js uses a model sentence without esc(): %q", use[0])
		}
	}
	for _, want := range []string{`aria-label="${esc(o.label || `, "title=\"${esc(o.label)}\""} {
		if !strings.Contains(js, want) {
			t.Errorf("infoBtn no longer escapes its label: app.js lacks %q", want)
		}
	}
	// no template inserts a model name as it is
	if m := regexp.MustCompile(`\$\{\s*(model\(s\)|modelTip\(s\)|modelTitle\(s\)|bios|s\.custom_model|s\.hardware\??\.display|r\.custom_model|r\.reported_model|subLine\(s\)|sub)\s*\}`).FindAllString(page, -1); len(m) != 0 {
		t.Errorf("app.js inserts a model name without esc(): %v", m)
	}
	// the dialog's markup is fixed text: nothing is filled into it
	start := strings.Index(js, "function openModelName(s) {")
	end := strings.Index(js[start:], "</form>`;")
	if start < 0 || end < 0 {
		t.Fatal("the dialog is not where this test looks for it")
	}
	dialog := js[start : start+end]
	i := strings.Index(dialog, "m.innerHTML = `")
	if i < 0 || strings.Contains(dialog[i:], "${") {
		t.Errorf("the model name dialog's markup must hold no inserted value: %s", dialog[max(i, 0):])
	}
	// and it is a title, a field and buttons: no paragraph to read, no label text; the menu entry is three words
	if strings.Contains(dialog, "<p") || strings.Contains(dialog, "<span") || !strings.Contains(js, `data-act="model-name">Set model name</button>`) {
		t.Errorf("the model name dialog and its menu entry carry no explanation: %s", dialog[max(i, 0):])
	}
	fn := js[start:]
	fn = fn[:strings.Index(fn, "\ndocument.addEventListener")]
	if strings.Count(fn, "innerHTML") != 1 || strings.Contains(fn, "insertAdjacentHTML") || strings.Contains(fn, "outerHTML") {
		t.Error("the model name dialog writes markup in one place only, the fixed form")
	}
}
