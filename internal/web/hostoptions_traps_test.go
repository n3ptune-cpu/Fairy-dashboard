package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// The traps of "Edit host" that the design names, one test each (trap (a), the reading agent, and (e), the
// restart, are in hostoptions_test.go).

// Who may. The matrix (routes_test.go) asks the route's guard in every way it asks the others; this is what
// it does not ask: the dialog's data is for admins with a browser session too, an old sign-in is asked to
// confirm with the panel's own lines of what will change, and the kept change then runs once.
func TestEditHostWhoMay(t *testing.T) {
	a := signedIn(t) // an admin whose sign-in is an hour old
	e := a.e
	csrf := []string{"X-Requested-With", "fairy", "Content-Type", "application/json"}
	body := map[string]any{"plugs": []any{map[string]any{"id": "laptop/desk", "in_total": false}}}
	const path = "/api/admin/hosts/laptop/options"
	// the dialog's data: nobody, a viewer, a token of either scope
	if r := e.do("GET", path, nil); r.code != 401 {
		t.Fatalf("GET, not signed in: %d", r.code)
	}
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
	if r := e.do("GET", path, nil, "Cookie", vc); r.code != 403 || r.body["detail"] != "only admins can edit a host" {
		t.Fatalf("GET, a viewer: %d %s", r.code, r.raw)
	}
	if r := e.do("PUT", path, body, append([]string{"Cookie", vc}, csrf...)...); r.code != 403 || r.body["detail"] != "only admins can edit a host" {
		t.Fatalf("PUT, a viewer: %d %s", r.code, r.raw)
	}
	for _, scope := range []string{"read", "control"} {
		tok, _, err := e.s.Tokens.Create("t-"+scope, "system", scope, "cli:test", 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, method := range []string{"GET", "PUT"} {
			if r := e.do(method, path, body, "Authorization", "Bearer "+tok, "Content-Type", "application/json"); r.code != 403 || r.body["detail"] != "API tokens are not accepted on this endpoint" {
				t.Fatalf("%s with a %s token: %d %s", method, scope, r.code, r.raw)
			}
			// ... also next to an admin's cookie
			if r := e.do(method, path, body, append([]string{"Authorization", "Bearer " + tok, "Cookie", a.sess}, csrf...)...); r.code != 403 {
				t.Fatalf("%s with a %s token next to an admin's cookie: %d", method, scope, r.code)
			}
		}
	}
	// the admin may look (reading asks for no confirmation) ...
	if r := e.do("GET", path, nil, "Cookie", a.sess); r.code != 200 || r.body["name"] == nil {
		t.Fatalf("GET, the admin: %d %s", r.code, r.raw)
	}
	// ... and is asked to confirm a change: nothing is stored, and the dialog gets the panel's own lines
	r := e.do("PUT", path, body, append([]string{"Cookie", a.sess, "X-Fairy-Action", "Edit%20host%20laptop"}, csrf...)...)
	d, _ := r.body["detail"].(map[string]any)
	if !stepUpRequired(r) || d["action"] != "change the links of laptop" || d["pending"] != true ||
		!slices.Equal(strs(d["items"]), []string{"Plug Desk plug (read by laptop): not in the total"}) ||
		!strings.Contains(fmt.Sprint(d["message"]), "changes to a host need a Pocket ID sign-in") {
		t.Fatalf("an old sign-in: %d %s", r.code, r.raw)
	}
	if e.storedOptions() != nil {
		t.Fatal("stored before it was confirmed")
	}
	// a request that can never be taken is refused for what it is, before anybody is asked to confirm anything:
	// the admin with the old sign-in is not sent to Pocket ID for a request the panel then answers with 422
	asked := len(e.auditEntries("", stepUpAction))
	var many []any
	for i := 0; i <= maxHostOptionEntries; i++ {
		many = append(many, map[string]any{"id": fmt.Sprintf("laptop/u%d", i), "on": true})
	}
	for name, c := range map[string]struct {
		body map[string]any
		want string
	}{
		"nothing":                                {map[string]any{}, "nothing to change"},
		"empty lists":                            {map[string]any{"ups": []any{}, "plugs": []any{}, "reset": []any{}, "leftovers": false}, "nothing to change"},
		"a UPS without on":                       {map[string]any{"ups": []any{map[string]any{"id": "laptop/rack"}}}, `ups: "on" is missing`},
		"a plug with nothing to do":              {map[string]any{"plugs": []any{map[string]any{"id": "laptop/desk"}}}, "neither"},
		"a plug twice":                           {map[string]any{"plugs": []any{map[string]any{"id": "laptop/desk", "linked": true}, map[string]any{"id": "laptop/desk", "in_total": true}}}, "twice"},
		"a reset of no field":                    {map[string]any{"reset": []any{map[string]any{"id": "laptop/desk", "field": "url"}}}, "no such field"},
		"a reset and a tick of a line":           {map[string]any{"reset": []any{map[string]any{"id": "laptop/desk", "field": "total"}}, "plugs": []any{map[string]any{"id": "laptop/desk", "in_total": true}}}, "twice"},
		"more entries than one request may have": {map[string]any{"ups": many}, "too many entries in one request"},
	} {
		r := e.do("PUT", path, c.body, append([]string{"Cookie", a.sess}, csrf...)...)
		if d, _ := r.body["detail"].(string); r.code != 422 || !strings.Contains(d, c.want) {
			t.Errorf("%s, with an old sign-in: %d %s (want 422 with %q, and no confirmation)", name, r.code, r.raw, c.want)
		}
	}
	if n := len(e.auditEntries("", stepUpAction)); n != asked || e.storedOptions() != nil {
		t.Errorf("a request that cannot be taken asked for a confirmation: %d step-up entries, before %d", n, asked)
	}
	// a body with two readings is refused before anything is kept or described
	if r := e.doRaw("PUT", path, `{"plugs":[{"id":"laptop/desk","in_total":true}],"PLUGS":[{"id":"laptop/desk","in_total":false}]}`,
		append([]string{"Cookie", a.sess}, csrf...)...); r.code != 422 {
		t.Fatalf("two readings: %d %s", r.code, r.raw)
	}
	r = e.do("PUT", path, body, append([]string{"Cookie", a.sess, "X-Fairy-Action", "Edit%20host%20laptop"}, csrf...)...)
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal("the confirmation:", cb.code)
	}
	if ex := fmt.Sprint(e.storedAgent("laptop")["power_exclude_from_total"]); ex != "map[desk:true]" {
		t.Fatalf("after the confirmation the change is stored: %v", e.storedOptions())
	}
	if n := len(e.auditEntries("", "host.options")); n != 1 {
		t.Fatalf("%d audit entries, want 1", n)
	}
	// what the page shows afterwards is the kept result: the label and the panel's sentence
	me := e.do("GET", "/api/me", nil, "Cookie", a.sess)
	pr, _ := me.body["pending_result"].(map[string]any)
	if pr["ok"] != true || pr["message"] != "links saved" || pr["label"] != "Edit host laptop" {
		t.Fatalf("the kept result: %v", me.body["pending_result"])
	}
	// inside the window the next change needs no confirmation; the kill switch does not stop it (no device is controlled)
	if err := e.s.Poller.Admin.SetControlDisabled(true, "test", "kill switch"); err != nil {
		t.Fatal(err)
	}
	if r := e.do("PUT", path, map[string]any{"plugs": []any{map[string]any{"id": "laptop/desk", "in_total": true}}}, append([]string{"Cookie", a.sess}, csrf...)...); r.code != 200 {
		t.Fatalf("inside the window, kill switch on: %d %s", r.code, r.raw)
	}
	// the rate limit of admin changes counts here: 429 with the time to wait
	e.s.adminRL = newRateLimiter(1)
	_ = e.do("PUT", path, body, append([]string{"Cookie", a.sess}, csrf...)...)
	r = e.do("PUT", path, body, append([]string{"Cookie", a.sess}, csrf...)...)
	if r.code != 429 || r.hdr.Get("Retry-After") == "" || !strings.HasPrefix(fmt.Sprint(r.body["detail"]), "too many changes; try again in ") {
		t.Fatalf("over the limit: %d %s", r.code, r.raw)
	}
}

// Who may is asked before what was sent is looked at. A request that can never be taken (422) is answered so
// only to an admin's browser session that sent the page's header: everybody else gets the answer about who
// they are (401, 403), whatever the body is, and learns nothing about the form of the API from it. The form
// is still checked before an admin is asked to confirm (TestEditHostWhoMay).
func TestEditHostWhoMayIsAskedBeforeWhatWasSent(t *testing.T) {
	a := signedIn(t)
	e := a.e
	const path = "/api/admin/hosts/laptop/options"
	v := model.User{Sub: "u7", Name: model.P("Vee"), Email: model.P("v@example.com"), Groups: []string{"viewers"}}
	vc := e.cookie(v, 0)
	tok, _, err := e.s.Tokens.Create("t-control", "system", "control", "cli:test", 0)
	if err != nil {
		t.Fatal(err)
	}
	csrf := []string{"X-Requested-With", "fairy"}
	for name, body := range map[string]string{
		"nothing to change":   `{}`,
		"an unknown field":    `{"url": "http://elsewhere"}`,
		"a name misspelled":   `{"ups": [{"ID": "laptop/rack", "on": true}]}`,
		"not JSON":            `{"ups": [`,
		"a reset of no field": `{"reset": [{"id": "laptop/desk", "field": "url"}]}`,
	} {
		if r := e.doRaw("PUT", path, body, append([]string{"Cookie", a.sess}, csrf...)...); r.code != 422 && r.code != 400 {
			t.Fatalf("the rig: %s is refused for its form when an admin sends it: %d %s", name, r.code, r.raw)
		}
		denied := len(e.auditEntries("", "auth.denied"))
		for who, c := range map[string]struct {
			hdr    []string
			code   int
			detail string
		}{
			"nobody":                               {csrf, 401, ""},
			"a viewer":                             {append([]string{"Cookie", vc}, csrf...), 403, "only admins can edit a host"},
			"an admin's cookie without the header": {[]string{"Cookie", a.sess}, 403, "missing X-Requested-With: fairy header"},
			"a token":                              {[]string{"Authorization", "Bearer " + tok}, 403, "API tokens are not accepted on this endpoint"},
		} {
			r := e.doRaw("PUT", path, body, c.hdr...)
			if got := string(r.raw); r.code != c.code || !strings.Contains(got, c.detail) || strings.Contains(got, "nothing to change") || strings.Contains(got, "invalid request body") || strings.Contains(got, "no such field") {
				t.Errorf("%s, sent by %s: %d %s (want %d %q, and nothing about the body)", name, who, r.code, r.raw, c.code, c.detail)
			}
		}
		if n := len(e.auditEntries("", "auth.denied")) - denied; n != 2 {
			t.Errorf("%s: %d auth.denied entries for the viewer and the missing header, want 2", name, n)
		}
	}
}

// Trap (b). A line that was changed in the panel is used INSTEAD of what config.yaml says about that line: when
// config.yaml is edited afterwards, its word about that line does not count while the change is there. The
// dialog says where the value comes from and what config.yaml says now; Reset makes config.yaml's (new) word
// count. Everything nobody changed in the panel follows config.yaml, also for the same reading agent, the same
// UPS and another machine: a change made on PVE-B's page freezes nothing of what config.yaml says about PVE-A.
func TestEditHostAChangedLineShadowsConfigYAMLAndEveryOtherLineFollowsIt(t *testing.T) {
	r := ehSetup(t)
	r.ok(ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/desk", true)}})  // PVE-B's page: on the desk UPS
	r.ok(ehNodeA, map[string]any{"ups": []any{ehUPS("nas-agent/rack", false)}}) // PVE-A's page: off the rack UPS, where config.yaml has it
	if got := fmt.Sprint(r.storedAgent(ehNAS)["ups_powers"]); got != "map[desk:map["+ehNodeB+":true] rack:map["+ehNodeA+":false]]" {
		t.Fatalf("state.json holds the two decisions and nothing else: %v", got)
	}
	// config.yaml is edited, and the panel restarted: the desk UPS is given to PVE-A, the rack UPS to PVE-A (as
	// before) and to the NAS itself
	r2 := r.again(ehConfig(t, `ups_powers: {desk: [lab-a-node-pve-a], rack: [lab-a-node-pve-a, nas-agent]}, power_links: {a-plug: lab-a-agent}`))
	ups := func(id string) []string { return slices.Sorted(slices.Values(strs(r2.row(id)["ups"]))) }
	if a, b, n := ups(ehNodeA), ups(ehNodeB), ups(ehNAS); !slices.Equal(a, []string{"nas-agent/desk"}) || !slices.Equal(b, []string{"nas-agent/desk"}) || !slices.Equal(n, []string{"nas-agent/rack"}) {
		t.Fatalf("after config.yaml changed: PVE-A %v, PVE-B %v, NAS %v\n(want PVE-A on desk by config.yaml's new line and still off rack by the panel's change; "+
			"PVE-B on desk by the panel's change; the NAS on rack by config.yaml's new line)", a, b, n)
	}
	g := r2.options(ehNodeA)
	rack, desk := ehEntry(t, g, "ups", "nas-agent/rack"), ehEntry(t, g, "ups", "nas-agent/desk")
	if rack["on"] != false || rack["from"] != "panel" || rack["config_on"] != true {
		t.Fatalf("PVE-A's dialog, the line that was changed (what counts, and what config.yaml says now): %v", rack)
	}
	if desk["on"] != true || desk["from"] != "config" || desk["by"] != nil {
		t.Fatalf("PVE-A's dialog, a line nobody changed for PVE-A: it follows config.yaml and says so: %v", desk)
	}
	// the plug links were not changed in the panel: config.yaml's value counts for them
	if ap := ehEntry(t, g, "plugs", "nas-agent/a-plug"); ap["from"] != "config" || ap["linked"] != true {
		t.Fatalf("a line without a change: %v", ap)
	}
	// Reset of PVE-A's line: config.yaml's word about PVE-A and the rack UPS counts; PVE-B's change is not touched
	p := r2.ok(ehNodeA, map[string]any{"reset": []any{map[string]any{"id": "nas-agent/rack", "field": "ups"}}})
	if got := strs(p.body["changed"]); !slices.Equal(got, []string{"UPS Rack UPS (read by NAS): back to config.yaml: powers PVE-A"}) {
		t.Fatalf("changed: %v", got)
	}
	r2.poll()
	if a, b := ups(ehNodeA), ups(ehNodeB); !slices.Equal(a, []string{"nas-agent/desk", "nas-agent/rack"}) || !slices.Equal(b, []string{"nas-agent/desk"}) {
		t.Fatalf("after Reset of PVE-A's line: PVE-A %v, PVE-B %v", a, b)
	}
	if got := fmt.Sprint(r2.storedAgent(ehNAS)["ups_powers"]); got != "map[desk:map["+ehNodeB+":true]]" {
		t.Fatalf("state.json after the Reset: %v", got)
	}
}

// A Reset is about one line of one machine's page, and says what it does. On PVE-B's page it changes nothing
// of PVE-A's links, whatever was changed for PVE-A with the same UPS of the same reading agent; the dialog's
// lines, the lines of "Confirm it's you", the answer and the audit entry all say the same one thing. The one
// change that reaches another machine is a plug's: it has one place, and the line names the machine it goes
// to or comes back from.
func TestEditHostResetIsAboutOneLineOfOneMachine(t *testing.T) {
	r := ehSetup(t)
	reset := func(id, field string) map[string]any {
		return map[string]any{"reset": []any{map[string]any{"id": id, "field": field}}}
	}
	// PVE-A's page: off the rack UPS (config.yaml has it on), on the desk UPS. PVE-B's page: on the desk UPS.
	r.ok(ehNodeA, map[string]any{"ups": []any{ehUPS("nas-agent/rack", false), ehUPS("nas-agent/desk", true)}})
	r.ok(ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/desk", true)}})
	want := []string{"UPS Desk UPS (read by NAS): back to config.yaml: no longer powers PVE-B"}
	// what an admin with an old sign-in is shown before the Reset is kept
	old := r.do("PUT", "/api/admin/hosts/"+ehNodeB+"/options", reset("nas-agent/desk", "ups"), "Cookie", r.staleCookie(controller(), time.Hour), "X-Requested-With", "fairy", "Content-Type", "application/json")
	if d, _ := old.body["detail"].(map[string]any); !stepUpRequired(old) || !slices.Equal(strs(d["items"]), want) {
		t.Fatalf("the lines of \"Confirm it's you\": %d %s", old.code, old.raw)
	}
	p := r.ok(ehNodeB, reset("nas-agent/desk", "ups"))
	if got := strs(p.body["changed"]); !slices.Equal(got, want) {
		t.Fatalf("changed: %v", got)
	}
	r.poll()
	if a, b := strs(r.row(ehNodeA)["ups"]), strs(r.row(ehNodeB)["ups"]); !slices.Equal(a, []string{"nas-agent/desk"}) || len(b) != 0 {
		t.Fatalf("after the Reset on PVE-B's page: PVE-A %v (want its own two changes untouched: desk, not rack), PVE-B %v", a, b)
	}
	if got := fmt.Sprint(r.storedAgent(ehNAS)["ups_powers"]); got != "map[desk:map["+ehNodeA+":true] rack:map["+ehNodeA+":false]]" {
		t.Fatalf("state.json: %v", got)
	}
	en := r.auditEntries("", "host.options")[0]["params"].(map[string]any)
	ch := en["changes"].([]any)
	if c := ch[0].(map[string]any); !slices.Equal(strs(en["lines"]), want) || len(ch) != 1 || c["agent"] != ehNAS || c["field"] != "ups_powers" ||
		!slices.Equal(strs(c["before"].(map[string]any)["desk"]), []string{ehNodeA, ehNodeB}) || !slices.Equal(strs(c["after"].(map[string]any)["desk"]), []string{ehNodeA}) {
		t.Fatalf("the audit entry: %v", en)
	}
	// a line PVE-B's page did not change cannot be reset there: PVE-A's change of the rack UPS stays
	if p := r.ok(ehNodeB, reset("nas-agent/rack", "ups")); p.body["message"] != "nothing to change" || len(strs(r.row(ehNodeA)["ups"])) != 1 {
		t.Fatalf("a Reset of PVE-A's line asked on PVE-B's page: %s", p.raw)
	}
	// a tick that says what config.yaml says takes the change out: nothing is left in state.json for that line
	r.ok(ehNodeA, map[string]any{"ups": []any{ehUPS("nas-agent/rack", true), ehUPS("nas-agent/desk", false)}})
	if r.storedAgent(ehNAS) != nil {
		t.Fatalf("every line is as config.yaml has it again, and state.json still holds: %v", r.storedOptions())
	}
	if rack := ehEntry(t, r.options(ehNodeA), "ups", "nas-agent/rack"); rack["on"] != true || rack["from"] != "config" {
		t.Fatalf("the line after a tick back to config.yaml's value: %v", rack)
	}

	// a plug: config.yaml links a-plug to PVE-A. PVE-B's page takes it; both pages show the line as changed
	r.ok(ehNodeB, map[string]any{"plugs": []any{map[string]any{"id": "nas-agent/a-plug", "linked": true}}})
	onA, onB := ehEntry(t, r.options(ehNodeA), "plugs", "nas-agent/a-plug"), ehEntry(t, r.options(ehNodeB), "plugs", "nas-agent/a-plug")
	if onA["linked"] != false || onA["other"] != "PVE-B" || onA["from"] != "panel" || onA["config_linked"] != true || onA["config_other"] != nil {
		t.Fatalf("a-plug on PVE-A's page: %v", onA)
	}
	if onB["linked"] != true || onB["from"] != "panel" || onB["config_linked"] != false || onB["config_other"] != "PVE-A" {
		t.Fatalf("a-plug on PVE-B's page: %v", onB)
	}
	// Reset on PVE-B's page: the line says where the plug goes back to
	p = r.ok(ehNodeB, reset("nas-agent/a-plug", "plugs"))
	if got := strs(p.body["changed"]); !slices.Equal(got, []string{"Plug A plug (read by NAS): back to config.yaml: no longer linked to PVE-B (on PVE-A again)"}) {
		t.Fatalf("changed: %v", got)
	}
	// ... and the same Reset made on PVE-A's page says where it comes back from
	r.ok(ehNodeB, map[string]any{"plugs": []any{map[string]any{"id": "nas-agent/a-plug", "linked": true}}})
	p = r.ok(ehNodeA, reset("nas-agent/a-plug", "plugs"))
	if got := strs(p.body["changed"]); !slices.Equal(got, []string{"Plug A plug (read by NAS): back to config.yaml: linked to PVE-A (was on PVE-B)"}) {
		t.Fatalf("changed: %v", got)
	}
	r.poll()
	if a, b := r.row(ehNodeA)["power_w"], r.row(ehNodeB)["power_w"]; a != 100.0 || b != nil || r.storedAgent(ehNAS) != nil {
		t.Fatalf("after the Reset: PVE-A %v, PVE-B %v, state.json %v", a, b, r.storedOptions())
	}
	// a decision about a plug that is neither given to this machine nor taken from it is not this page's: PVE-B's
	// page shows b-plug, which the NAS's page gave to the NAS, as "from config.yaml", and cannot reset it
	r.ok(ehNAS, map[string]any{"plugs": []any{map[string]any{"id": "nas-agent/b-plug", "linked": true}}})
	if bp := ehEntry(t, r.options(ehNodeB), "plugs", "nas-agent/b-plug"); bp["linked"] != false || bp["other"] != "NAS" || bp["from"] != "config" {
		t.Fatalf("b-plug on PVE-B's page: %v", bp)
	}
	if p := r.ok(ehNodeB, reset("nas-agent/b-plug", "plugs")); p.body["message"] != "nothing to change" {
		t.Fatalf("a Reset of the NAS's line asked on PVE-B's page: %s", p.raw)
	}
	// the total: one plug's place in it
	r.ok(ehNAS, map[string]any{"plugs": []any{map[string]any{"id": "nas-agent/corner", "in_total": true}, map[string]any{"id": "nas-agent/b-plug", "in_total": false}}})
	p = r.ok(ehNAS, reset("nas-agent/corner", "total"))
	if got := strs(p.body["changed"]); !slices.Equal(got, []string{"Plug Corner plug (read by NAS): back to config.yaml: not in the total"}) {
		t.Fatalf("changed: %v", got)
	}
	if got := fmt.Sprint(r.storedAgent(ehNAS)["power_exclude_from_total"]); got != "map[b-plug:true]" {
		t.Fatalf("state.json after the Reset of one plug's total: %v", got)
	}
	// a change that says what config.yaml says by now: config.yaml is edited to leave b-plug out of the total and
	// to link it to the NAS itself. The lines stay "changed here" until Reset, which then changes nothing that shows.
	r2 := r.again(ehConfig(t, `ups_powers: {rack: [lab-a-node-pve-a]}, power_links: {a-plug: lab-a-agent, b-plug: nas-agent}, power_exclude_from_total: [corner, b-plug]`))
	bp := ehEntry(t, r2.options(ehNAS), "plugs", "nas-agent/b-plug")
	if bp["in_total"] != false || bp["total_from"] != "panel" || bp["config_in_total"] != false || bp["linked"] != true || bp["from"] != "panel" || bp["config_linked"] != true {
		t.Fatalf("b-plug after config.yaml caught up: %v", bp)
	}
	n := len(r2.auditEntries("", "host.options"))
	p = r2.ok(ehNAS, map[string]any{"reset": []any{map[string]any{"id": "nas-agent/b-plug", "field": "plugs"}, map[string]any{"id": "nas-agent/b-plug", "field": "total"}}})
	if got := strs(p.body["changed"]); !slices.Equal(got, []string{"Plug B plug (read by NAS): back to config.yaml: nothing changes now", "Plug B plug (read by NAS): back to config.yaml: nothing changes now"}) ||
		p.body["message"] != "links saved" || r2.storedAgent(ehNAS) != nil || len(r2.auditEntries("", "host.options")) != n+1 {
		t.Fatalf("a Reset of changes that say what config.yaml says: %s", p.raw)
	}
}

// Traps (c) and (d), with a host added in the panel. Re-pair keeps what was changed for it (the same machine,
// the same id). Remove takes it all out: the overrides of its own agent, and every link to its row in the
// overrides of other agents, so that the next machine added under the same name starts with nothing.
func TestEditHostRepairKeepsAndRemoveForgets(t *testing.T) {
	e := setup(t)
	e.s.Enroll.Interval = 10 * time.Millisecond
	e.s.control, e.s.adminRL = newRateLimiter(1000), newRateLimiter(1000)
	e.s.Enroll.PanelAddr = "100.64.0.10"
	e.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
		return 200, enroll.Response{AgentVersion: "42-abc", Platform: "linux", Token: "winpc-read-token-xxxxxxxxxxxxxxxx",
			ControlToken: "winpc-control-token-yyyyyyyyyyyyyy"}, "", nil
	}
	e.b.Mu.Lock()
	e.b.Metrics["srv-winpc"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil } // it relays a plug of its own, "desk"
	e.b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["ups"] = []any{jsonx.M{"name": "rack", "state": "online", "description": "Rack UPS"}}
		m["power_sources"] = append(m["power_sources"].([]any), jsonx.M{"id": "spare", "name": "Spare plug", "type": "shelly", "ok": true, "watts": 55.0})
		return m, nil
	}
	e.b.Mu.Unlock()
	c := e.cookie(controller(), 0)
	hdr := []string{"Cookie", c, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	run := func(path string, body map[string]any) {
		t.Helper()
		p := e.do("POST", path, body, hdr...)
		if p.code != 201 {
			t.Fatal(path, p.code, string(p.raw))
		}
		id := p.body["id"].(string)
		for i := 0; i < 300; i++ {
			time.Sleep(10 * time.Millisecond)
			e.s.Poller.PollOnce(context.Background())
			if g := e.do("GET", "/api/admin/enrollments/"+id, nil, "Cookie", c); g.body["done"] == true {
				if g.body["state"] != "online" {
					t.Fatal(path, g.body["state"], g.body["message"])
				}
				return
			}
		}
		t.Fatal("the enrollment never finished", path)
	}
	add := func() { run("/api/admin/hosts", map[string]any{"name": "SRV-WINPC", "os": "linux", "site": "Alice"}) }
	sys := func(id string) map[string]any { return e.do("GET", "/api/systems/"+id, nil, "Cookie", c).body }
	put := func(id string, body any) {
		t.Helper()
		if r := e.do("PUT", "/api/admin/hosts/"+id+"/options", body, hdr...); r.code != 200 || r.body["ok"] != true {
			t.Fatalf("PUT %s %v: %d %s", id, body, r.code, r.raw)
		}
	}
	add()
	// three things about the new host: the laptop's UPS powers it, the laptop's spare plug is its plug, and its
	// own plug is left out of the total; and one thing that is not about it (the laptop's own total)
	put("srv-winpc", map[string]any{"ups": []any{ehUPS("laptop/rack", true)},
		"plugs": []any{map[string]any{"id": "laptop/spare", "linked": true}, map[string]any{"id": "srv-winpc/desk", "in_total": false}}})
	put("laptop", map[string]any{"ups": []any{ehUPS("laptop/rack", true)}, "plugs": []any{map[string]any{"id": "laptop/desk", "in_total": false}}})
	e.s.Poller.PollOnce(context.Background())
	has := func() (ups []string, watts any) { x := sys("srv-winpc"); return strs(x["ups"]), x["power_w"] }
	if ups, w := has(); !slices.Equal(ups, []string{"laptop/rack"}) || w != 135.0 {
		t.Fatalf("the new host: ups %v, power_w %v (its own plug 80 W and the spare one 55 W)", ups, w)
	}
	storedBefore := fmt.Sprint(e.storedOptions()["agents"])

	// (d) Re-pair: new tokens, the same machine. Everything stays, in the file and on the page.
	run("/api/admin/hosts/srv-winpc/repair", map[string]any{"confirm": "SRV-WINPC"})
	if now := fmt.Sprint(e.storedOptions()["agents"]); now != storedBefore {
		t.Fatalf("Re-pair changed the overrides:\n%s\n%s", storedBefore, now)
	}
	e.s.Poller.PollOnce(context.Background())
	if ups, w := has(); !slices.Equal(ups, []string{"laptop/rack"}) || w != 135.0 {
		t.Fatalf("after Re-pair: ups %v, power_w %v", ups, w)
	}
	if ex := fmt.Sprint(e.storedAgent("srv-winpc")["power_exclude_from_total"]); ex != "map[desk:true]" {
		t.Fatalf("after Re-pair the host's own override: %v", e.storedAgent("srv-winpc"))
	}

	// (c) Remove: its own overrides and every link to it are gone; what is about other machines stays. (Before
	// it, the host's own plug is given to the laptop: a link kept with the host itself, about a device that
	// goes with it.)
	put("laptop", map[string]any{"plugs": []any{map[string]any{"id": "srv-winpc/desk", "linked": true}}})
	if fmt.Sprint(e.storedAgent("srv-winpc")["power_links"]) != "map[desk:laptop]" {
		t.Fatalf("the rig: the host's own plug is linked to the laptop: %v", e.storedAgent("srv-winpc"))
	}
	if r := e.do("DELETE", "/api/admin/hosts/srv-winpc", map[string]any{"confirm": "SRV-WINPC"}, hdr...); r.code != 200 {
		t.Fatal("remove", r.code, string(r.raw))
	}
	if e.storedAgent("srv-winpc") != nil {
		t.Fatalf("the removed host's overrides are still there: %v", e.storedOptions())
	}
	lap := e.storedAgent("laptop")
	if raw := fmt.Sprint(e.storedOptions()); strings.Contains(raw, "srv-winpc") {
		t.Fatalf("state.json still names the removed host: %s", raw)
	}
	if fmt.Sprint(lap["ups_powers"]) != "map[rack:map[laptop:true]]" || lap["power_links"] != nil || fmt.Sprint(lap["power_exclude_from_total"]) != "map[desk:true]" {
		t.Fatalf("the laptop's overrides after the removal (its own UPS link and total stay): %v", lap)
	}
	var forgot map[string]any
	for _, en := range e.auditEntries("", "host.options") {
		if p, _ := en["params"].(map[string]any); p["because"] == "host removed" && en["target"] == "srv-winpc" {
			forgot = p
		}
	}
	if got := strs(forgot["removed"]); !slices.Equal(got, []string{"the overrides of srv-winpc", "laptop: UPS rack no longer powers it", "laptop: device spare is no longer linked to it"}) {
		t.Fatalf("the audit entry of what Remove took out: %v", forgot)
	}
	// ... and whose view of a device that stays it moved: the host was of the site Alice, and the laptop's UPS
	// and spare plug were seen by Alice's viewers through their links to it. Nothing is said about the host's
	// own plug: it goes with the host
	if got := strs(forgot["visibility"]); !slices.Equal(got, []string{"Viewers of Alice will no longer see UPS Rack UPS", "Viewers of Alice will no longer see plug Spare plug"}) {
		t.Fatalf("the audit entry of what Remove took out, who sees what: %q", got)
	}
	// the same name again (another machine, for all the panel knows): it inherits nothing
	add()
	e.s.Poller.PollOnce(context.Background())
	if ups, w := has(); len(ups) != 0 || w != 80.0 {
		t.Fatalf("the host added again under the same name: ups %v, power_w %v (want no UPS, and its own plug alone)", ups, w)
	}
	g := e.do("GET", "/api/admin/hosts/srv-winpc/options", nil, "Cookie", c)
	if own := ehEntry(t, g, "plugs", "srv-winpc/desk"); own["in_total"] != true || own["total_from"] != "config" {
		t.Fatalf("its own plug is in the total again: %v", own)
	}
	if spare := ehEntry(t, g, "plugs", "laptop/spare"); spare["linked"] != false {
		t.Fatalf("the spare plug: %v", spare)
	}
	// a Remove of a host that has nothing writes no entry about overrides
	n := len(e.auditEntries("", "host.options"))
	if r := e.do("DELETE", "/api/admin/hosts/srv-winpc", map[string]any{"confirm": "SRV-WINPC"}, hdr...); r.code != 200 {
		t.Fatal("remove", r.code, string(r.raw))
	}
	if len(e.auditEntries("", "host.options")) != n {
		t.Fatal("a Remove with nothing to forget wrote a host.options entry")
	}
}

// Remove takes out what was about the host, and only that. A host that was added on a Proxmox node the panel
// already had (a `proxmox:` entry of config.yaml) is shown as that node's row, and that row stays when the
// host is removed: the UPS that powers the node still powers it. What goes is what names the host's own id (a
// plug's link is written with it) and the changes kept with the host's own agent.
func TestEditHostRemoveKeepsWhatIsAboutARowThatStays(t *testing.T) {
	e := setup(t)
	e.s.Enroll.Interval = 10 * time.Millisecond
	e.s.control, e.s.adminRL = newRateLimiter(1000), newRateLimiter(1000)
	e.s.Enroll.PanelAddr = "100.64.0.10"
	e.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
		return 200, enroll.Response{AgentVersion: "42-abc", Platform: "linux", Token: "boxa-read-token-xxxxxxxxxxxxxxxxx",
			ControlToken: "boxa-control-token-yyyyyyyyyyyyyyy"}, "", nil
	}
	e.b.Mu.Lock()
	e.b.Metrics["box-a"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil } // it relays a plug of its own, "desk"
	e.b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["ups"] = []any{jsonx.M{"name": "rack", "state": "online", "description": "Rack UPS"}}
		m["power_sources"] = append(m["power_sources"].([]any), jsonx.M{"id": "spare", "name": "Spare plug", "type": "shelly", "ok": true, "watts": 55.0})
		return m, nil
	}
	e.b.Mu.Unlock()
	c := e.cookie(controller(), 0)
	hdr := []string{"Cookie", c, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	const node = "node-srv-pvea" // a node of the config.yaml proxmox: entry of this fixture
	p := e.do("POST", "/api/admin/hosts", map[string]any{"name": "BOX-A", "os": "linux", "site": "Alice", "node": "SRV-PVEA"}, hdr...)
	if p.code != 201 {
		t.Fatal(p.code, string(p.raw))
	}
	for i := 0; ; i++ {
		time.Sleep(10 * time.Millisecond)
		e.s.Poller.PollOnce(context.Background())
		if g := e.do("GET", "/api/admin/enrollments/"+p.body["id"].(string), nil, "Cookie", c); g.body["done"] == true && g.body["state"] == "online" {
			break
		} else if i > 300 {
			t.Fatal("the enrollment never finished", g.body["state"], g.body["message"])
		}
	}
	h, ok := e.s.managedHost("box-a")
	if !ok || h.Shown() != node || h.Proxmox != nil {
		t.Fatalf("the rig: BOX-A is not a host on the node of config.yaml: %+v", h)
	}
	put := func(id string, body any) {
		t.Helper()
		if r := e.do("PUT", "/api/admin/hosts/"+id+"/options", body, hdr...); r.code != 200 || r.body["ok"] != true || r.body["id"] != node {
			t.Fatalf("PUT %s %v: %d %s", id, body, r.code, r.raw)
		}
	}
	// on the host's page (its node's row): the laptop's UPS powers it, the laptop's spare plug is its plug, its own
	// plug is left out of the total
	put("box-a", map[string]any{"ups": []any{ehUPS("laptop/rack", true)},
		"plugs": []any{map[string]any{"id": "laptop/spare", "linked": true}, map[string]any{"id": "box-a/desk", "in_total": false}}})
	if lap := e.storedAgent("laptop"); fmt.Sprint(lap["ups_powers"]) != "map[rack:map["+node+":true]]" || fmt.Sprint(lap["power_links"]) != "map[spare:box-a]" || e.storedAgent("box-a") == nil {
		t.Fatalf("stored before the removal: %v", e.storedOptions())
	}
	e.s.Poller.PollOnce(context.Background())
	if r := e.do("DELETE", "/api/admin/hosts/box-a", map[string]any{"confirm": "BOX-A"}, hdr...); r.code != 200 {
		t.Fatal("remove", r.code, string(r.raw))
	}
	lap := e.storedAgent("laptop")
	if fmt.Sprint(lap["ups_powers"]) != "map[rack:map["+node+":true]]" {
		t.Errorf("the UPS link of the node's row, which stays, was taken out with the host: %v", e.storedOptions())
	}
	if lap["power_links"] != nil || e.storedAgent("box-a") != nil {
		t.Errorf("what names the removed host's own id is still there: %v", e.storedOptions())
	}
	var forgot []string
	for _, en := range e.auditEntries("", "host.options") {
		if p, _ := en["params"].(map[string]any); p["because"] == "host removed" && en["target"] == "box-a" {
			forgot = strs(p["removed"])
		}
	}
	if !slices.Equal(forgot, []string{"the overrides of box-a", "laptop: device spare is no longer linked to it"}) {
		t.Errorf("the audit entry of what Remove took out: %q", forgot)
	}
	// the node's row is there as before, and on the UPS with the next poll
	e.s.Poller.PollOnce(context.Background())
	if x := e.do("GET", "/api/systems/"+node, nil, "Cookie", c); x.code != 200 || x.body["kind"] != "node" || !slices.Equal(strs(x.body["ups"]), []string{"laptop/rack"}) {
		t.Errorf("the node's row after the removal: %d kind %v ups %v", x.code, x.body["kind"], x.body["ups"])
	}
}

// rowsOf: which rows go away with a host. Its own node's row (the proxmox: entry came with the host), never
// the row of a node the panel had before, and nothing while the node's token is lost (the host is then shown
// under its agent's id, which Forget takes anyway).
func TestEditHostRowsThatGoWithAHost(t *testing.T) {
	own := &managed.PVE{ID: "site-box", IDPrefix: "site-box-", Node: "box"}
	for name, c := range map[string]struct {
		h    managed.Host
		want []string
	}{
		"a plain host":                    {managed.Host{ID: "box-a"}, nil},
		"a host on a node of config.yaml": {managed.Host{ID: "box-a", Node: "SRV-PVEA", SystemID: "node-srv-pvea"}, nil},
		"a node added with Add host":      {managed.Host{ID: "box-a", Node: "box", SystemID: "site-box-node-box", Proxmox: own, ProxmoxWanted: true}, []string{"site-box-node-box"}},
		"a node whose token is lost":      {managed.Host{ID: "box-a", SystemID: "site-box-node-box", ProxmoxWanted: true}, nil},
	} {
		if got := rowsOf(c.h); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// Stored changes that have no line in any machine's dialog can still be taken out. A change is such a leftover
// when the machine that reads the device answers and does not relay the device any more, when it is kept
// with an id that is no agent's (a host that was taken out of config.yaml), or when it names a row that is
// no machine of this panel. Every dialog says how many there are and what they are, and removes them, behind
// the same guard as every other change, with its lines in "Confirm it's you" and in the audit log. The
// changes of a machine that is only silent are not leftovers: they have their lines again when it answers.
func TestEditHostStoredChangesWithoutALineCanBeRemoved(t *testing.T) {
	left := func(r *ehRig, id string) ([]string, any) {
		t.Helper()
		g := r.options(id)
		return strs(g.body["leftovers"]), g.body["leftover_count"]
	}
	// away is what the dialog's data says about devices that are not relayed right now: how many entries, how many changes
	away := func(r *ehRig, id string) (int, any) {
		t.Helper()
		g := r.options(id)
		l, _ := g.body["unrelayed"].([]any)
		return len(l), g.body["unrelayed_count"]
	}
	t.Run("the reading machine does not relay the devices", func(t *testing.T) {
		r := ehSetup(t)
		r.ok(ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/rack", true)}, "plugs": []any{map[string]any{"id": "nas-agent/b-plug", "linked": true, "in_total": false}}})
		if l, n := left(r, ehNodeA); len(l) != 0 || n != 0.0 {
			t.Fatalf("changes that have their lines: %v %v", l, n)
		}
		// silent: its devices are not gone
		r.b.Mu.Lock()
		r.b.Metrics[ehNAS] = func() (jsonx.M, error) { return nil, fmt.Errorf("down") }
		r.b.Mu.Unlock()
		r.poll()
		if l, n := left(r, ehNodeA); len(l) != 0 || n != 0.0 {
			t.Fatalf("while the NAS is silent: %v %v", l, n)
		}
		if l, n := away(r, ehNodeA); l != 0 || n != 0.0 {
			t.Fatalf("while the NAS is silent, not relayed: %v %v", l, n)
		}
		// ... also not after a restart of the panel, when it knows of no device of the NAS at all: what the NAS
		// reads is not known then, and nothing is offered for removal
		r = r.again(nil)
		if g := r.options(ehNodeA); len(g.body["ups"].([]any)) != 0 || len(g.body["plugs"].([]any)) != 0 || len(strs(g.body["leftovers"])) != 0 || g.body["leftover_count"] != 0.0 ||
			len(g.body["unrelayed"].([]any)) != 0 || g.body["unrelayed_count"] != 0.0 {
			t.Fatalf("after a restart with the NAS silent: %s", g.raw)
		}
		for _, body := range []map[string]any{{"leftovers": true}, {"unrelayed": []any{map[string]any{"id": "nas-agent/rack", "kind": "ups"}, map[string]any{"id": "nas-agent/b-plug", "kind": "plug"}}}} {
			if p := r.ok(ehNodeA, body); p.body["message"] != "nothing to change" || r.storedAgent(ehNAS) == nil {
				t.Fatalf("a removal asked for while the NAS is silent (%v): %s", body, p.raw)
			}
		}
		// it answers, without a UPS and without a plug: the lines are gone, and what is stored is said as what it
		// is: about devices that are not relayed right now (TestEditHostADeviceThatIsNotRelayedRightNowIsNotGone)
		r.b.Mu.Lock()
		r.b.Metrics[ehNAS] = func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			delete(m, "power_sources")
			delete(m, "ups")
			return m, nil
		}
		r.b.Mu.Unlock()
		r.poll()
		g := r.options(ehNodeB)
		texts := func(g resp) (out []string) {
			l, _ := g.body["unrelayed"].([]any)
			for _, x := range l {
				out = append(out, fmt.Sprint(x.(map[string]any)["text"]))
			}
			return out
		}
		want := []string{"1 stored change about the UPS rack, which NAS does not relay right now", "2 stored changes about the plug b-plug, which NAS does not relay right now"}
		if len(g.body["ups"].([]any)) != 0 || len(g.body["plugs"].([]any)) != 0 || !slices.Equal(texts(g), want) || g.body["unrelayed_count"] != 3.0 || g.body["leftover_count"] != 0.0 {
			t.Fatalf("the dialog when the NAS relays nothing (no lines, and what is stored said): %s", g.raw)
		}
		if got := texts(r.options(ehNodeA)); !slices.Equal(got, want) {
			t.Fatalf("the same on another machine's page: %v", got)
		}
		if x := r.put(ehNodeA, map[string]any{"leftovers": false}); x.code != 422 {
			t.Fatalf("leftovers: false: %d %s", x.code, x.raw)
		}
		// both named: both go
		both := map[string]any{"unrelayed": []any{map[string]any{"id": "nas-agent/rack", "kind": "ups"}, map[string]any{"id": "nas-agent/b-plug", "kind": "plug"}}}
		removed := []string{"Removed: " + want[0], "Removed: " + want[1]}
		p := r.ok(ehNodeA, both)
		if !slices.Equal(strs(p.body["changed"]), removed) || p.body["message"] != "links saved" || r.storedAgent(ehNAS) != nil || len(texts(p)) != 0 {
			t.Fatalf("removed: %s\nstate.json: %v", p.raw, r.storedOptions())
		}
		en := r.auditEntries("", "host.options")[0]
		if lines := strs(en["params"].(map[string]any)["lines"]); !slices.Equal(lines, removed) || en["target"] != ehNodeA || en["outcome"] != "ok" {
			t.Fatalf("the audit entry: %v", en)
		}
		if p := r.ok(ehNodeA, both); p.body["message"] != "nothing to change" {
			t.Fatalf("nothing left: %s", p.raw)
		}
	})
	t.Run("an id that is no agent's, a row that is no machine's", func(t *testing.T) {
		cfg := ehConfig(t, ehNASLinks)
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.DataDir, "state.json"), []byte(`{"host_options": {"rev": 1, "agents": {
			"old-pc": {"power_exclude_from_total": {"desk": true}, "ups_powers": {"attic": {"old-pc": true}}},
			"nas-agent": {"ups_powers": {"rack": {"old-pc": true, "older-pc": false, "lab-b-node-pve-b": true}}, "power_links": {"b-plug": "old-pc", "corner": "lab-b-agent"}}}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		r := ehSetupWith(t, cfg, true)
		want := []string{"UPS Rack UPS (read by NAS): a stored change about old-pc, which is no machine of this panel",
			"UPS Rack UPS (read by NAS): a stored change about older-pc, which is no machine of this panel",
			"Plug B plug (read by NAS): a stored link to old-pc, which is no machine of this panel",
			"2 stored changes kept with old-pc, which is no machine of this panel"}
		if l, n := left(r, ehNodeB); !slices.Equal(l, want) || n != 5.0 {
			t.Fatalf("leftovers: %q %v", l, n)
		}
		// a save for a machine does not touch them, and does not drop them
		r.ok(ehNodeA, map[string]any{"ups": []any{ehUPS("nas-agent/desk", true)}})
		if l, n := left(r, ehNodeA); !slices.Equal(l, want) || n != 5.0 || r.storedAgent("old-pc") == nil {
			t.Fatalf("after another save: %q %v", l, n)
		}
		// removed together with a change of this machine, in one request
		p := r.ok(ehNodeB, map[string]any{"leftovers": true, "ups": []any{ehUPS("nas-agent/desk", true)}})
		if got := strs(p.body["changed"]); len(got) != 5 || got[0] != "Removed: "+want[0] || got[3] != "Removed: "+want[3] || got[4] != "UPS Desk UPS (read by NAS): powers PVE-B" {
			t.Fatalf("changed: %q", got)
		}
		nas := r.storedAgent(ehNAS)
		if r.storedAgent("old-pc") != nil || fmt.Sprint(nas["ups_powers"]) != "map[desk:map["+ehNodeA+":true "+ehNodeB+":true] rack:map["+ehNodeB+":true]]" || fmt.Sprint(nas["power_links"]) != "map[corner:lab-b-agent]" {
			t.Fatalf("state.json afterwards (what has a line stays): %v", r.storedOptions())
		}
		if l, n := left(r, ehNodeB); len(l) != 0 || n != 0.0 {
			t.Fatalf("leftovers afterwards: %q %v", l, n)
		}
	})
	t.Run("many", func(t *testing.T) {
		cfg := ehConfig(t, ehNASLinks)
		_ = os.MkdirAll(cfg.DataDir, 0o700)
		var agents []string
		for i := 0; i < 30; i++ {
			agents = append(agents, fmt.Sprintf(`"gone-%02d": {"power_exclude_from_total": {"x": true}}`, i))
		}
		if err := os.WriteFile(filepath.Join(cfg.DataDir, "state.json"), []byte(`{"host_options": {"agents": {`+strings.Join(agents, ",")+`}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		r := ehSetupWith(t, cfg, true)
		l, n := left(r, ehNodeB)
		if len(l) != 21 || n != 30.0 || l[0] != "1 stored change kept with gone-00, which is no machine of this panel" || l[20] != "10 more of the same kind" {
			t.Fatalf("thirty leftovers are said as twenty lines and how many more: %d lines, count %v: %q", len(l), n, l)
		}
		p := r.ok(ehNodeB, map[string]any{"leftovers": true})
		if got := strs(p.body["changed"]); len(got) != 21 || got[20] != "Removed: 10 more of the same kind" || len(r.storedOptions()["agents"].(map[string]any)) != 0 {
			t.Fatalf("removed: %q", got)
		}
		// the audit entry has the same twenty-one lines, and the number of all that went
		if en := r.auditEntries("", "host.options")[0]["params"].(map[string]any); len(strs(en["lines"])) != 21 || en["removed_count"] != 30.0 {
			t.Fatalf("the audit entry of thirty removed changes: %d lines, removed_count %v", len(strs(en["lines"])), en["removed_count"])
		}
	})
}

// A host that is ADDED under an id the panel still has stored changes for starts with none of them: they were
// about another machine (a host's id is made from its name). Remove forgets them already; this is for what
// is there all the same: a host that was taken out of config.yaml, a state.json from a backup. Re-pair is
// not an Add: it keeps what was changed for the machine (TestEditHostRepairKeepsAndRemoveForgets).
func TestEditHostAHostAddedUnderAnIdWithStoredChangesStartsClean(t *testing.T) {
	e := setup(t)
	e.s.Enroll.Interval = 10 * time.Millisecond
	e.s.control, e.s.adminRL = newRateLimiter(1000), newRateLimiter(1000)
	e.s.Enroll.PanelAddr = "100.64.0.10"
	// the machine answers the pairing only when the test lets it: until then there is a join code and nothing else
	answer := make(chan struct{})
	var once sync.Once
	letItAnswer := func() { once.Do(func() { close(answer) }) }
	t.Cleanup(letItAnswer)
	e.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
		<-answer
		return 200, enroll.Response{AgentVersion: "42-abc", Platform: "linux", Token: "boxa-read-token-xxxxxxxxxxxxxxxxx",
			ControlToken: "boxa-control-token-yyyyyyyyyyyyyyy"}, "", nil
	}
	e.b.Mu.Lock()
	e.b.Metrics["box-a"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil } // it relays a plug of its own, "desk"
	e.b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["ups"] = []any{jsonx.M{"name": "rack", "state": "online", "description": "Rack UPS"}}
		m["power_sources"] = append(m["power_sources"].([]any), jsonx.M{"id": "spare", "name": "Spare plug", "type": "shelly", "ok": true, "watts": 55.0})
		return m, nil
	}
	e.b.Mu.Unlock()
	// what an earlier machine called BOX-A left behind: its own plug out of the total, the laptop's UPS and spare
	// plug given to it; and one change that is about the laptop alone
	if _, _, err := e.s.Poller.Admin.EditHostOptions("earlier", func(o *adminstate.HostOptions) error {
		o.Agents["box-a"] = adminstate.HostOption{Exclude: map[string]bool{"desk": true}}
		o.Agents["laptop"] = adminstate.HostOption{UPS: map[string]map[string]bool{"rack": {"box-a": true, "laptop": true}}, Links: map[string]string{"spare": "box-a"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c := e.cookie(controller(), 0)
	hdr := []string{"Cookie", c, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	p := e.do("POST", "/api/admin/hosts", map[string]any{"name": "BOX-A", "os": "linux", "site": "Alice"}, hdr...)
	if p.code != 201 {
		t.Fatal(p.code, string(p.raw))
	}
	time.Sleep(50 * time.Millisecond) // the pairing has asked the machine by now (every 10 ms), and waits for it
	if e.storedAgent("box-a") == nil {
		t.Fatal("a join code alone (no machine has answered) took the stored changes out")
	}
	letItAnswer()
	for i := 0; ; i++ {
		time.Sleep(10 * time.Millisecond)
		e.s.Poller.PollOnce(context.Background())
		if g := e.do("GET", "/api/admin/enrollments/"+p.body["id"].(string), nil, "Cookie", c); g.body["done"] == true && g.body["state"] == "online" {
			break
		} else if i > 300 {
			t.Fatal("the enrollment never finished", g.body["state"], g.body["message"])
		}
	}
	e.s.Poller.PollOnce(context.Background())
	if x := e.do("GET", "/api/systems/box-a", nil, "Cookie", c); len(strs(x.body["ups"])) != 0 || x.body["power_w"] != 80.0 {
		t.Errorf("the new host: ups %v, power_w %v (want no UPS, and its own plug alone, counted)", x.body["ups"], x.body["power_w"])
	}
	lap := e.storedAgent("laptop")
	if raw := fmt.Sprint(e.storedOptions()); strings.Contains(raw, "box-a") || fmt.Sprint(lap["ups_powers"]) != "map[rack:map[laptop:true]]" {
		t.Errorf("state.json after the host was added (nothing about box-a; the laptop's own change stays): %s", raw)
	}
	var forgot map[string]any
	for _, en := range e.auditEntries("", "host.options") {
		if p, _ := en["params"].(map[string]any); p["because"] == "host added" && en["target"] == "box-a" {
			forgot = en
		}
	}
	if forgot == nil || forgot["outcome"] != "ok" || !slices.Equal(strs(forgot["params"].(map[string]any)["removed"]), []string{"the overrides of box-a", "laptop: UPS rack no longer powers it", "laptop: device spare is no longer linked to it"}) {
		t.Errorf("the audit entry of what Add host took out: %v", forgot)
	}
	// a second host, with nothing stored under its id: no entry about it
	n := len(e.auditEntries("", "host.options"))
	e.b.Mu.Lock()
	e.b.Metrics["box-c"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	e.b.Mu.Unlock()
	p = e.do("POST", "/api/admin/hosts", map[string]any{"name": "BOX-C", "os": "linux", "site": "Alice"}, hdr...)
	for i := 0; p.code == 201; i++ {
		time.Sleep(10 * time.Millisecond)
		e.s.Poller.PollOnce(context.Background())
		if g := e.do("GET", "/api/admin/enrollments/"+p.body["id"].(string), nil, "Cookie", c); g.body["done"] == true || i > 300 {
			break
		}
	}
	if len(e.auditEntries("", "host.options")) != n {
		t.Error("an Add host with nothing to forget wrote a host.options entry")
	}
}

// When what is kept under the new host's id cannot be taken out (state.json cannot be written), the host is
// not added: it would start with links that were made for another machine, and nothing would say so. The
// pairing ends with a sentence that says why, nothing of the host is stored or polled, the audit log has the
// failure, and what was kept is still there, used by nothing.
func TestEditHostAnAddIsRefusedWhenWhatIsKeptForItsIdCannotBeTakenOut(t *testing.T) {
	e := setup(t)
	e.s.Enroll.Interval = 10 * time.Millisecond
	e.s.control, e.s.adminRL = newRateLimiter(1000), newRateLimiter(1000)
	e.s.Enroll.PanelAddr = "100.64.0.10"
	answer := make(chan struct{})
	var once sync.Once
	letItAnswer := func() { once.Do(func() { close(answer) }) }
	t.Cleanup(letItAnswer)
	e.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
		<-answer
		return 200, enroll.Response{AgentVersion: "42-abc", Platform: "linux", Token: "boxa-read-token-xxxxxxxxxxxxxxxxx"}, "", nil
	}
	e.b.Mu.Lock()
	e.b.Metrics["box-a"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	e.b.Mu.Unlock()
	if _, _, err := e.s.Poller.Admin.EditHostOptions("earlier", func(o *adminstate.HostOptions) error {
		o.Agents["box-a"] = adminstate.HostOption{Exclude: map[string]bool{"desk": true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	kept := fmt.Sprint(e.storedOptions())
	c := e.cookie(controller(), 0)
	p := e.do("POST", "/api/admin/hosts", map[string]any{"name": "BOX-A", "os": "linux", "site": "Alice"}, "Cookie", c, "X-Requested-With", "fairy", "Content-Type", "application/json")
	if p.code != 201 {
		t.Fatal(p.code, string(p.raw))
	}
	// state.json cannot be written from now on (it can still be read): a folder is where its lock file was
	lock := filepath.Join(e.s.Cfg.DataDir, "state.json.lock")
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	letItAnswer()
	var g resp
	for i := 0; ; i++ {
		time.Sleep(10 * time.Millisecond)
		if g = e.do("GET", "/api/admin/enrollments/"+p.body["id"].(string), nil, "Cookie", c); g.body["done"] == true {
			break
		} else if i > 300 {
			t.Fatal("the enrollment never finished", g.body["state"], g.body["message"])
		}
	}
	msg, _ := g.body["message"].(string)
	if g.body["state"] != "failed" || !strings.HasPrefix(msg, "cannot add it: what \"Edit host\" still keeps under this host's id could not be taken out (") ||
		!strings.HasSuffix(msg, "). Nothing was stored on the panel; make a new code to try again") {
		t.Errorf("the enrollment: state %v, message %q", g.body["state"], msg)
	}
	if hosts, err := e.s.Managed.Load(); err != nil || len(hosts) != 0 {
		t.Errorf("the refused host is in the list of added hosts: %v %v", hosts, err)
	}
	if _, err := e.s.Managed.Secret("box-a"); err == nil {
		t.Error("the refused host's tokens were stored")
	}
	if _, polled := e.s.Poller.AgentByID("box-a"); polled {
		t.Error("the refused host is polled")
	}
	var failure map[string]any
	for _, en := range e.auditEntries("", "host.options") {
		if p, _ := en["params"].(map[string]any); p["because"] == "host added" && en["target"] == "box-a" {
			failure = en
		}
	}
	if out, _ := failure["outcome"].(string); !strings.HasPrefix(out, "error: ") {
		t.Errorf("the audit entry of the clean-up that failed: %v", failure)
	}
	// what was kept is as it was, and is used by nothing (there is no such machine)
	if now := fmt.Sprint(e.storedOptions()); now != kept {
		t.Errorf("state.json afterwards:\nbefore %s\nnow    %s", kept, now)
	}
	// state.json can be written again: the same Add host, with a new code, works and starts clean
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	p = e.do("POST", "/api/admin/hosts", map[string]any{"name": "BOX-A", "os": "linux", "site": "Alice"}, "Cookie", c, "X-Requested-With", "fairy", "Content-Type", "application/json")
	if p.code != 201 {
		t.Fatal("a second Add host:", p.code, string(p.raw))
	}
	for i := 0; ; i++ {
		time.Sleep(10 * time.Millisecond)
		e.s.Poller.PollOnce(context.Background())
		if g = e.do("GET", "/api/admin/enrollments/"+p.body["id"].(string), nil, "Cookie", c); g.body["done"] == true {
			break
		} else if i > 300 {
			t.Fatal("the second enrollment never finished", g.body["state"], g.body["message"])
		}
	}
	if g.body["state"] != "online" || e.storedAgent("box-a") != nil {
		t.Errorf("the second Add host: state %v (%v), kept under its id: %v", g.body["state"], g.body["message"], e.storedAgent("box-a"))
	}
}

// A link decides who can SEE a UPS or a plug: the viewers of the site of a machine it is linked to, else of
// the machine that reads it (view.go). The API says so before a change is stored: "Confirm it's you", the
// answer and the audit entry carry one note for each site whose viewers get to see a device or lose it.
//
// And what such a viewer then sees is what a link written in config.yaml always showed: the same answers,
// byte for byte. The id of a relayed device begins with the id of the agent that reads it ("nas-agent/rack"),
// also for a viewer who may not see that agent's machine; that is so for config.yaml's links since before
// "Edit host", and this feature shows no more of it.
func TestEditHostALinkDecidesWhoSeesTheDevice(t *testing.T) {
	// PVE-B is of the site Home; PVE-A and the NAS, which reads every device, of the site Away
	sites := func(r *ehRig) (home, away string) {
		t.Helper()
		_ = r.s.Access.EnsureSites([]string{"Home", "Away"})
		for id, site := range map[string]string{ehNodeB: "Home", ehNodeA: "Away", ehNAS: "Away"} {
			if _, err := r.s.Access.Assign(id, site); err != nil {
				t.Fatal(err)
			}
		}
		mk := func(sub, site string) string {
			c := r.cookie(model.User{Sub: sub, Name: model.P(sub), Email: model.P(sub + "@example.com"), Groups: []string{"viewers"}}, 0)
			if _, _, err := r.s.Access.SetRole(sub, "viewer", []string{site}, "test", false); err != nil {
				t.Fatal(err)
			}
			return c
		}
		return mk("vh", "Home"), mk("va", "Away")
	}
	// what a viewer is given about UPSes, plugs and rows, without the times of day
	times := regexp.MustCompile(`"(generated_at|last_success|last_seen|timestamp|at|since|[a-z_]*_at|[a-z_]*_s|[a-z_]*age[a-z_]*)":("[^"]*"|[0-9.e+-]+|null),?`)
	seen := func(r *ehRig, cookie string) string {
		t.Helper()
		var out []string
		for _, path := range []string{"/api/ups", "/api/power", "/api/systems"} {
			g := r.do("GET", path, nil, "Cookie", cookie)
			if g.code != 200 {
				t.Fatalf("GET %s: %d", path, g.code)
			}
			raw := string(g.raw)
			if i := strings.Index(raw, `,"history":`); i >= 0 { // the points of the UPS's chart: one for each poll there was
				raw = raw[:i] + "}"
			}
			out = append(out, path+" "+times.ReplaceAllString(raw, ""))
		}
		return strings.Join(out, "\n")
	}
	r := ehSetup(t)
	home, away := sites(r)
	before := seen(r, home)
	for _, hidden := range []string{"nas-agent", "Rack UPS", "B plug", "PVE-A", `"NAS"`} {
		if strings.Contains(before, hidden) {
			t.Fatalf("the rig: the Home viewer sees %q before anything is linked to PVE-B:\n%s", hidden, before)
		}
	}
	// the dialog's data: this machine's site, and for each device who sees it now and who would
	g := r.options(ehNodeB)
	rack, bp := ehEntry(t, g, "ups", "nas-agent/rack"), ehEntry(t, g, "plugs", "nas-agent/b-plug")
	if g.body["site"] != "Home" || fmt.Sprint(rack["sees"], rack["sees_on"], rack["sees_off"]) != "[Away] [Away Home] [Away]" ||
		fmt.Sprint(bp["sees"], bp["sees_linked"], bp["sees_unlinked"], bp["sees_config"]) != "[Away] [Home] [Away] [Away]" {
		t.Fatalf("who sees what, in the dialog's data: site %v, rack %v, b-plug %v", g.body["site"], rack, bp)
	}
	body := map[string]any{"ups": []any{ehUPS("nas-agent/rack", true)}, "plugs": []any{map[string]any{"id": "nas-agent/b-plug", "linked": true}}}
	notes := []string{"Viewers of Home will see UPS Rack UPS", "Viewers of Home will see plug B plug", "Viewers of Away will no longer see plug B plug"}
	lines := []string{"UPS Rack UPS (read by NAS): powers PVE-B", "Plug B plug (read by NAS): linked to PVE-B"}
	// said before it is kept for the confirmation ...
	old := r.do("PUT", "/api/admin/hosts/"+ehNodeB+"/options", body, "Cookie", r.staleCookie(controller(), time.Hour), "X-Requested-With", "fairy", "Content-Type", "application/json")
	if d, _ := old.body["detail"].(map[string]any); !stepUpRequired(old) || !slices.Equal(strs(d["items"]), append(slices.Clone(lines), notes...)) {
		t.Fatalf("the lines of \"Confirm it's you\": %d %s", old.code, old.raw)
	}
	// ... in the answer, and in the audit entry
	p := r.ok(ehNodeB, body)
	if !slices.Equal(strs(p.body["changed"]), lines) || !slices.Equal(strs(p.body["visibility"]), notes) {
		t.Fatalf("the answer: changed %q, visibility %q", strs(p.body["changed"]), strs(p.body["visibility"]))
	}
	if en := r.auditEntries("", "host.options")[0]["params"].(map[string]any); !slices.Equal(strs(en["visibility"]), notes) {
		t.Fatalf("the audit entry: %v", en)
	}
	r.poll()
	linked := seen(r, home)
	if !strings.Contains(linked, `"id":"nas-agent/rack"`) || !strings.Contains(linked, `"id":"nas-agent/b-plug"`) || strings.Contains(linked, "PVE-A") || strings.Contains(linked, `"NAS"`) {
		t.Fatalf("after the link the Home viewer sees the two devices, and no machine of the other site:\n%s", linked)
	}
	// of the machine that reads them, which is of the other site, the viewer is given the id inside the devices'
	// ids and nothing else: not who relays them ("via"), and of the rows a UPS powers only the ones they may see
	if strings.Count(linked, "nas-agent") != strings.Count(linked, `"nas-agent/`) || !strings.Contains(linked, `"via":null`) || strings.Contains(linked, `"via":"`) ||
		!strings.Contains(linked, `"powers":["`+ehNodeB+`"]`) {
		t.Fatalf("what the Home viewer is given about the reading machine:\n%s", linked)
	}
	if a := seen(r, away); strings.Contains(a, "B plug") || !strings.Contains(a, "Rack UPS") {
		t.Fatalf("the Away viewer after the link (the plug is gone for them, the UPS still powers PVE-A):\n%s", a)
	}
	// the same links written in config.yaml, on a panel without any change of "Edit host": the same answers
	r2 := ehSetupWith(t, ehConfig(t, `ups_powers: {rack: [lab-a-node-pve-a, lab-b-node-pve-b]}, power_links: {a-plug: lab-a-agent, b-plug: lab-b-agent}, power_exclude_from_total: [corner]`), true)
	home2, _ := sites(r2)
	if byConfig := seen(r2, home2); byConfig != linked {
		t.Errorf("a link made in the panel shows a viewer something else than the same link in config.yaml:\n--- config.yaml\n%s\n--- Edit host\n%s", byConfig, linked)
	}
	// taken back: said the other way round; a change that moves nobody's view has no note
	p = r.ok(ehNodeB, map[string]any{"reset": []any{map[string]any{"id": "nas-agent/rack", "field": "ups"}, map[string]any{"id": "nas-agent/b-plug", "field": "plugs"}}})
	if got := strs(p.body["visibility"]); !slices.Equal(got, []string{"Viewers of Home will no longer see UPS Rack UPS", "Viewers of Away will see plug B plug", "Viewers of Home will no longer see plug B plug"}) {
		t.Fatalf("visibility of the Reset: %q", got)
	}
	p = r.ok(ehNodeA, map[string]any{"ups": []any{ehUPS("nas-agent/desk", true)}, "plugs": []any{map[string]any{"id": "nas-agent/a-plug", "in_total": false}}})
	if got := strs(p.body["visibility"]); len(got) != 0 {
		t.Fatalf("a change inside one site: %q", got)
	}
	// a device with a site of its own (Users page) is seen by that site's viewers whatever it is linked to: no note
	if _, err := r.s.Access.Assign("ups:nas-agent/desk", "Away"); err != nil {
		t.Fatal(err)
	}
	p = r.ok(ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/desk", true)}})
	if got := strs(p.body["visibility"]); len(got) != 0 || strings.Contains(seen(r, home), "Desk UPS") {
		t.Fatalf("a UPS with a site of its own: notes %q", got)
	}
}

// Trap (f). Two admins save at the same moment. Each request says only its own ticks and is applied, under
// the lock of state.json, to what the other left: nothing is lost, the file is whole after every write, every
// answer shows the stored state it left (its rev), and of two changes to the same tick the later one counts.
func TestEditHostTwoAdminsAtOnce(t *testing.T) {
	r := ehSetup(t)
	second := model.User{Sub: "u9", Name: model.P("Bea"), Email: model.P("b@example.com"), EmailVerified: true, Groups: []string{"viewers"}}
	bea := r.cookie(second, 0)
	if _, _, err := r.s.Access.SetRole("u9", "admin", nil, "test", false); err != nil {
		t.Fatal(err)
	}
	who := [][]string{r.hdr, {"Cookie", bea, "X-Requested-With", "fairy"}}
	put := func(admin int, id string, body any) resp {
		return r.do("PUT", "/api/admin/hosts/"+id+"/options", body, append([]string{"Content-Type", "application/json"}, who[admin]...)...)
	}
	plug := func(id string, linked bool) map[string]any { return map[string]any{"id": id, "linked": linked} }
	// 1. different things, all in the entry of the one agent that reads them, eight at a time
	changes := []struct {
		id   string
		body map[string]any
	}{
		{ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/rack", true)}}},
		{ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/desk", true)}}},
		{ehNodeA, map[string]any{"ups": []any{ehUPS("nas-agent/desk", true)}}},
		{ehNodeB, map[string]any{"plugs": []any{plug("nas-agent/b-plug", true)}}},
		{ehNodeA, map[string]any{"plugs": []any{plug("nas-agent/corner", true)}}},
		{ehNAS, map[string]any{"plugs": []any{map[string]any{"id": "nas-agent/own", "in_total": false}}}},
		{ehNAS, map[string]any{"ups": []any{ehUPS("nas-agent/rack", true)}}},
		{ehNodeA, map[string]any{"plugs": []any{map[string]any{"id": "nas-agent/a-plug", "in_total": false}}}},
	}
	out := make([]resp, len(changes))
	var wg sync.WaitGroup
	for i, c := range changes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = put(i%2, c.id, c.body)
		}()
	}
	wg.Wait()
	var revs []int
	for i, p := range out {
		if p.code != 200 || len(strs(p.body["changed"])) != 1 {
			t.Fatalf("change %d: %d %s", i, p.code, p.raw)
		}
		revs = append(revs, int(p.body["rev"].(float64)))
	}
	sort.Ints(revs)
	if !slices.Equal(revs, []int{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("the revs of eight changes at once: %v (each write is one step after another)", revs)
	}
	nas := r.storedAgent(ehNAS)
	if fmt.Sprint(nas["ups_powers"]) != "map[desk:map["+ehNodeA+":true "+ehNodeB+":true] rack:map["+ehNodeB+":true "+ehNAS+":true]]" ||
		fmt.Sprint(nas["power_links"]) != "map[b-plug:lab-b-agent corner:lab-a-agent]" ||
		fmt.Sprint(nas["power_exclude_from_total"]) != "map[a-plug:true own:true]" || r.storedOptions()["rev"] != 8.0 {
		t.Fatalf("after eight changes at once, one is missing (or state.json holds more than the eight): %v", r.storedOptions())
	}
	if n := len(r.auditEntries("", "host.options")); n != 8 {
		t.Fatalf("%d audit entries, want 8", n)
	}
	// 2. the same tick, one admin on, the other off, many times: every answer is the state its own write left,
	// and the file ends as the answer with the highest rev says
	for round := 0; round < 12; round++ {
		res := make([]resp, 2)
		for i := range res {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res[i] = put(i, ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/rack", i == 0)}})
			}()
		}
		wg.Wait()
		last, top := false, -1.0
		for i, p := range res {
			if p.code != 200 {
				t.Fatalf("round %d: %d %s", round, p.code, p.raw)
			}
			on := ehEntry(t, p, "ups", "nas-agent/rack")["on"].(bool)
			// an answer that wrote shows what it asked for; one that had nothing to change shows what was there
			if len(strs(p.body["changed"])) == 1 && on != (i == 0) {
				t.Fatalf("round %d: admin %d changed it and the answer shows on=%v", round, i, on)
			}
			if rev := p.body["rev"].(float64); rev > top || (rev == top && len(strs(p.body["changed"])) == 1) {
				top, last = rev, on
			}
		}
		stored := r.storedAgent(ehNAS)["ups_powers"].(map[string]any)["rack"].(map[string]any)[ehNodeB] == true
		if stored != last || r.storedOptions()["rev"] != top {
			t.Fatalf("round %d: the file says %v at rev %v, the last answer said %v at rev %v", round, stored, r.storedOptions()["rev"], last, top)
		}
	}
	// what the two did not touch is as it was
	if got := r.storedAgent(ehNAS)["ups_powers"].(map[string]any)["desk"].(map[string]any); len(got) != 2 {
		t.Fatalf("desk after the rounds: %v", got)
	}
}

// Trap (g). One machine, two ids: a Proxmox node with an agent is its node's row while that row is there, and
// its agent's row after a restart of the panel with the machine off. The dialog opens on the row that is
// shown and changes the same machine either way.
//
//   - A plug's link is written with the agent's id, and the watts follow the machine to whichever row it is.
//   - A UPS's rows are matched by their exact id today (ups.Manager), as in a hand-written config.yaml: the id
//     of the row shown at that moment is written, and the chip is on that row only. Ticked while the machine
//     is shown under its agent's id, it does not follow to the node's row; the dialog on the node's row then
//     shows it unticked, a tick there adds the node's id, and taking the tick out removes both.
func TestEditHostOnANodeRowAndOnItsAgentsRow(t *testing.T) {
	r := ehSetupWith(t, ehConfig(t, ehNASLinks), false) // PVE-B's Proxmox API never answered: no node row
	if r.do("GET", "/api/systems/"+ehNodeB, nil, r.hdr[:2]...).code != 404 || r.row("lab-b-agent")["kind"] != "agent" {
		t.Fatal("the rig: PVE-B is not shown under its agent's id")
	}
	g := r.options("lab-b-agent")
	if g.body["id"] != "lab-b-agent" || g.body["agent"] != "lab-b-agent" {
		t.Fatalf("the dialog on the agent's row: %s", g.raw)
	}
	// the node's id names the same machine while its row is not there: the row that is shown, its agent's
	if byNode := r.options(ehNodeB); byNode.body["id"] != "lab-b-agent" {
		t.Fatalf("asked by the node's id while its row is not there: %v", byNode.body["id"])
	}
	r.ok("lab-b-agent", map[string]any{"ups": []any{ehUPS("nas-agent/rack", true)}, "plugs": []any{map[string]any{"id": "nas-agent/b-plug", "linked": true}}})
	nas := r.storedAgent(ehNAS)
	if got := fmt.Sprint(nas["ups_powers"]); got != "map[rack:map[lab-b-agent:true]]" {
		t.Fatalf("ups_powers (the row shown now): %v", got)
	}
	if got := fmt.Sprint(nas["power_links"]); got != "map[b-plug:lab-b-agent]" {
		t.Fatalf("power_links (the agent's id): %v", got)
	}
	en := r.auditEntries("", "host.options")[0]
	if p := en["params"].(map[string]any); en["target"] != "lab-b-agent" || p["agent"] != "lab-b-agent" || p["changes"].([]any)[0].(map[string]any)["agent"] != ehNAS {
		t.Fatalf("audit: target %v, params %v", en["target"], p)
	}
	r.poll()
	if x := r.row("lab-b-agent"); !slices.Equal(strs(x["ups"]), []string{"nas-agent/rack"}) || x["power_w"] != 200.0 {
		t.Fatalf("the agent's row: ups %v, power_w %v", x["ups"], x["power_w"])
	}
	// the machine's Proxmox API answers: it is its node's row from now on
	r.mu.Lock()
	r.bUp = true
	r.mu.Unlock()
	r.poll()
	r.poll()
	x := r.row(ehNodeB)
	if x["power_w"] != 200.0 {
		t.Fatalf("the plug's watts did not follow the machine to its node's row: %v", x["power_w"])
	}
	if got := strs(x["ups"]); len(got) != 0 {
		t.Fatalf("the UPS chip followed to the node's row: %v (UPS rows are matched by their exact id: if this changed, change the dialog and its words too)", got)
	}
	g = r.options(ehNodeB)
	if g.body["agent"] != "lab-b-agent" || ehEntry(t, g, "ups", "nas-agent/rack")["on"] != false || ehEntry(t, g, "plugs", "nas-agent/b-plug")["linked"] != true {
		t.Fatalf("the dialog on the node's row (the UPS as the page shows it, the plug linked): %s", g.raw)
	}
	// by the agent's id the same machine is found: its node's row
	if byAgent := r.options("lab-b-agent"); byAgent.body["id"] != ehNodeB {
		t.Fatalf("asked by the agent's id: %v", byAgent.body["id"])
	}
	// a tick on the node's row: the node's id is added, and the chip is there
	r.ok(ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/rack", true)}})
	if got := fmt.Sprint(r.storedAgent(ehNAS)["ups_powers"]); got != "map[rack:map[lab-b-agent:true "+ehNodeB+":true]]" {
		t.Fatalf("ups_powers after the tick on the node's row: %v", got)
	}
	r.poll()
	if got := strs(r.row(ehNodeB)["ups"]); !slices.Equal(got, []string{"nas-agent/rack"}) {
		t.Fatalf("the node's row: %v", got)
	}
	// the tick taken out: the machine is taken out under both ids, and nobody else
	p := r.ok(ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/rack", false)}, "plugs": []any{map[string]any{"id": "nas-agent/b-plug", "linked": false}}})
	if got := strs(p.body["changed"]); !slices.Equal(got, []string{"UPS Rack UPS (read by NAS): no longer powers PVE-B", "Plug B plug (read by NAS): no longer linked to PVE-B"}) {
		t.Fatalf("changed: %v", got)
	}
	if nas = r.storedAgent(ehNAS); nas != nil {
		t.Fatalf("after the ticks were taken out (every line is config.yaml's again): %v", nas)
	}
	r.poll()
	if a := r.row(ehNodeA); !slices.Equal(strs(a["ups"]), []string{"nas-agent/rack"}) || a["power_w"] != 100.0 {
		t.Fatalf("PVE-A, which nobody changed: ups %v, power_w %v", a["ups"], a["power_w"])
	}
	// PVE-A, on its node's row from the start: the plug with its agent's id, the UPS with its node's id
	r.ok(ehNodeA, map[string]any{"ups": []any{ehUPS("nas-agent/desk", true)}, "plugs": []any{map[string]any{"id": "nas-agent/corner", "linked": true}}})
	nas = r.storedAgent(ehNAS)
	if fmt.Sprint(nas["ups_powers"]) != "map[desk:map["+ehNodeA+":true]]" || fmt.Sprint(nas["power_links"]) != "map[corner:lab-a-agent]" {
		t.Fatalf("PVE-A: %v", nas)
	}
}

// "Linked by the machine itself" is what the AGENT says about a device (system_hint: self), not where the
// panel showed the plug at the reading agent's last answer. While that agent is silent or its polling is
// paused the old picture stays for as long as the silence lasts, and the dialog, the refusals and the lines
// must not be made from it: a plug that was unlinked from another machine in that time is linked to nothing,
// on every page; and a plug the agent calls its own is its own again.
func TestEditHostPlugStateWhileTheReadingAgentIsSilent(t *testing.T) {
	plug := func(id string, linked bool) map[string]any {
		return map[string]any{"plugs": []any{map[string]any{"id": id, "linked": linked}}}
	}
	silent := func(r *ehRig) {
		r.b.Mu.Lock()
		r.b.Metrics[ehNAS] = func() (jsonx.M, error) { return nil, fmt.Errorf("down") }
		r.b.Mu.Unlock()
		r.poll()
	}
	state := func(r *ehRig, page, id string) string {
		t.Helper()
		e := ehEntry(t, r.options(page), "plugs", id)
		return fmt.Sprintf("linked=%v self=%v other=%v mine=%v", e["linked"], e["self"], e["other"], e["mine"])
	}
	t.Run("a plug unlinked from another machine", func(t *testing.T) {
		r := ehSetup(t)
		r.ok(ehNodeB, plug("nas-agent/b-plug", true))
		r.poll()
		silent(r)
		if got := strs(r.ok(ehNodeB, plug("nas-agent/b-plug", false)).body["changed"]); !slices.Equal(got, []string{"Plug B plug (read by NAS): no longer linked to PVE-B"}) {
			t.Fatalf("unlinked while the NAS is silent: %v", got)
		}
		r.poll()
		for page, want := range map[string]string{
			ehNodeB: "linked=false self=false other=<nil> mine=false",
			ehNodeA: "linked=false self=false other=<nil> mine=false",
			ehNAS:   "linked=false self=false other=<nil> mine=true", // its own agent reads it: its total can be changed there
		} {
			if got := state(r, page, "nas-agent/b-plug"); got != want {
				t.Errorf("the dialog of %s: %s, want %s", page, got, want)
			}
		}
		// on the reading machine's page: nothing to unlink (and no refusal for a reason that is not true) ...
		if x := r.put(ehNAS, plug("nas-agent/b-plug", false)); x.code != 200 || x.body["message"] != "nothing to change" {
			t.Errorf("unlink on the NAS's page: %d %s", x.code, x.raw)
		}
		// ... and it can be linked there
		if x := r.put(ehNAS, plug("nas-agent/b-plug", true)); x.code != 200 || !slices.Equal(strs(x.body["changed"]), []string{"Plug B plug (read by NAS): linked to NAS"}) {
			t.Errorf("link on the NAS's page: %d %s", x.code, x.raw)
		}
		if state(r, ehNAS, "nas-agent/b-plug") != "linked=true self=false other=<nil> mine=true" {
			t.Errorf("after the link on the NAS's page: %s", state(r, ehNAS, "nas-agent/b-plug"))
		}
		// taken out again there, still without an answer of the NAS: linked to nothing, and not "set on the machine itself"
		if x := r.put(ehNAS, plug("nas-agent/b-plug", false)); x.code != 200 || !slices.Equal(strs(x.body["changed"]), []string{"Plug B plug (read by NAS): no longer linked to NAS"}) {
			t.Errorf("unlink on the NAS's page after the link: %d %s", x.code, x.raw)
		}
		if got := state(r, ehNAS, "nas-agent/b-plug"); got != "linked=false self=false other=<nil> mine=true" {
			t.Errorf("after that: %s", got)
		}
		// another machine takes it: from nowhere, so the line names no machine it "was on"
		x := r.put(ehNodeA, plug("nas-agent/b-plug", true))
		if x.code != 200 || !slices.Equal(strs(x.body["changed"]), []string{"Plug B plug (read by NAS): linked to PVE-A"}) {
			t.Errorf("link on PVE-A's page: %d %s", x.code, x.raw)
		}
		en := r.auditEntries("", "host.options")
		if lines := strs(en[0]["params"].(map[string]any)["lines"]); !slices.Equal(lines, []string{"Plug B plug (read by NAS): linked to PVE-A"}) {
			t.Errorf("the audit entry's line: %v", lines)
		}
	})
	t.Run("the plug the agent calls its own", func(t *testing.T) {
		r := ehSetup(t)
		r.ok(ehNodeB, plug("nas-agent/own", true)) // a link wins over what the agent says
		r.poll()
		if got := state(r, ehNAS, "nas-agent/own"); got != "linked=false self=false other=PVE-B mine=true" {
			t.Fatalf("given to PVE-B, on the NAS's page: %s", got)
		}
		silent(r)
		r.ok(ehNodeB, plug("nas-agent/own", false))
		r.poll()
		// no link any more: it is the NAS's own again, as its agent said when it last answered
		if got := state(r, ehNAS, "nas-agent/own"); got != "linked=true self=true other=<nil> mine=true" {
			t.Errorf("the NAS's page: %s", got)
		}
		if got := state(r, ehNodeB, "nas-agent/own"); got != "linked=false self=false other=NAS mine=false" {
			t.Errorf("PVE-B's page: %s", got)
		}
		if x := r.put(ehNAS, plug("nas-agent/own", false)); x.code != 422 || !strings.Contains(fmt.Sprint(x.body["detail"]), "NAS plug is linked by the machine itself") {
			t.Errorf("unlink of its own plug on the NAS's page: %d %s", x.code, x.raw)
		}
		if x := r.put(ehNodeA, plug("nas-agent/own", true)); x.code != 200 || !slices.Equal(strs(x.body["changed"]), []string{"Plug NAS plug (read by NAS): linked to PVE-A (was on NAS)"}) {
			t.Errorf("link on PVE-A's page: %d %s", x.code, x.raw)
		}
	})
}

// A state.json that holds something else under "host_options" never stops a start and never changes what an
// agent's entry says: every broken part is left out, config.yaml's value counts for it, the rest is used, and
// the dialog and the log say what was ignored. Whatever the file says, nothing but the three link fields is
// ever read from it.
func TestEditHostABrokenOverlayIsIgnoredNotRefused(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &logs}, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	for name, c := range map[string]struct {
		overlay string
		ignored int      // lines in the dialog
		upsB    []string // PVE-B's UPSes afterwards
	}{
		"a text":                          {`"links"`, 1, nil},
		"a list":                          {`[{"nas-agent": {}}]`, 1, nil},
		"null":                            {`null`, 1, nil},
		"agents is a list":                {`{"rev": 3, "agents": []}`, 1, nil},
		"a rev that is no number":         {`{"rev": "seven", "agents": {"nas-agent": {"ups_powers": {"desk": {"lab-b-node-pve-b": true}}}}}`, 1, []string{"nas-agent/desk"}},
		"an entry that is no object":      {`{"agents": {"nas-agent": "x", "lab-a-agent": 7}}`, 2, nil},
		"an id no agent can have":         {`{"agents": {"NAS Agent!": {"ups_powers": {"desk": {"lab-b-node-pve-b": true}}}}}`, 1, nil},
		"a field of the wrong form":       {`{"agents": {"nas-agent": {"ups_powers": ["lab-b-node-pve-b"], "power_links": {"b-plug": 5}, "power_exclude_from_total": "corner"}}}`, 3, nil},
		"a row id no row can have":        {`{"agents": {"nas-agent": {"ups_powers": {"desk": {"Lab B!": true}}}}}`, 1, nil},
		"lists, as config.yaml has them":  {`{"agents": {"nas-agent": {"ups_powers": {"desk": ["lab-b-node-pve-b"]}, "power_exclude_from_total": ["a-plug"]}}}`, 2, nil},
		"a decision that is no yes or no": {`{"agents": {"nas-agent": {"ups_powers": {"desk": {"lab-b-node-pve-b": "yes"}}, "power_exclude_from_total": {"a-plug": 1}}}}`, 2, nil},
		"seventeen devices":               {`{"agents": {"nas-agent": {"power_exclude_from_total": {"d1":true,"d2":true,"d3":true,"d4":true,"d5":true,"d6":true,"d7":true,"d8":true,"d9":true,"d10":true,"d11":true,"d12":true,"d13":true,"d14":true,"d15":true,"d16":true,"d17":true}}}}`, 1, nil},
		"one bad field, one good":         {`{"agents": {"nas-agent": {"power_links": 7, "ups_powers": {"rack": {"lab-b-node-pve-b": true}}}}}`, 1, []string{"nas-agent/rack"}},
		"fields that cannot be set":       {`{"agents": {"lab-b-agent": {"url": "http://198.51.100.7:9101", "token_env": "FAIRY_SESSION_SECRET", "power_control": true, "wol_relay": true, "verify_tls": false, "node": "PVE-A", "name": "x"}}}`, 7, nil},
		"an agent there is not":           {`{"agents": {"gone-agent": {"ups_powers": {"desk": {"lab-b-node-pve-b": true}}}}}`, 0, nil},
		"a row there is not":              {`{"agents": {"nas-agent": {"ups_powers": {"desk": {"lab-b-node-pve-b": true, "old-pc": true}}, "power_links": {"a-plug": "lab-a-agent", "b-plug": "old-pc"}}}}`, 0, []string{"nas-agent/desk"}},
		"what Fairy writes, and a tail":   {`{"rev": 4, "agents": {"nas-agent": {"ups_powers": {"desk": {"lab-b-node-pve-b": true}}, "by": "x", "at": "2026-11-13T23:40:00Z"}}, "more": 1}`, 1, []string{"nas-agent/desk"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := ehConfig(t, ehNASLinks)
			if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cfg.DataDir, "state.json"), []byte(`{"host_options": `+c.overlay+`}`), 0o600); err != nil {
				t.Fatal(err)
			}
			logs.Reset()
			r := ehSetupWith(t, cfg, true) // the panel starts
			// every agent's entry is what config.yaml says
			for _, a := range r.s.Poller.Agents() {
				if a.ID == "lab-b-agent" && (a.URL != "http://lab-b-agent" || a.TokenEnv != "TOK_LAB_B" || !a.PowerControl || a.WolRelay || a.VerifyTLS != nil || a.Node != "PVE-B" || a.Name != "PVE-B") {
					t.Fatalf("the agent's entry changed: %+v", a)
				}
			}
			got := strs(r.row(ehNodeB)["ups"])
			if !slices.Equal(got, append([]string{}, c.upsB...)) {
				t.Errorf("PVE-B's UPSes: %v, want %v", got, c.upsB)
			}
			// what config.yaml gives PVE-A counts wherever its field was not validly overridden
			if a := strs(r.row(ehNodeA)["ups"]); len(c.upsB) == 0 && !slices.Equal(a, []string{"nas-agent/rack"}) {
				t.Errorf("PVE-A's UPSes: %v (config.yaml's value counts for what is ignored)", a)
			}
			if w := r.row(ehNodeA)["power_w"]; w != 100.0 {
				t.Errorf("PVE-A's watts: %v", w)
			}
			if w := r.row(ehNodeB)["power_w"]; w != nil {
				t.Errorf("PVE-B's watts: %v (a link to a row there is not must not become a link to anything)", w)
			}
			g := r.options(ehNodeB)
			if ig := strs(g.body["ignored"]); len(ig) != c.ignored || g.body["ignored_count"] != float64(c.ignored) {
				t.Errorf("the dialog's ignored lines: %d (count %v), want %d: %v", len(ig), g.body["ignored_count"], c.ignored, ig)
			}
			r.poll()
			warns := 0
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "level=WARN") && strings.Contains(line, "Edit host") {
					warns++
				}
				if strings.Contains(line, "level=ERROR") {
					t.Errorf("an ERROR in the log: %s", line)
				}
			}
			if want := max(c.ignored, 1); warns != want {
				t.Errorf("%d WARN lines about it, want %d (one for each thing, once):\n%s", warns, want, logs.String())
			}
			// the next change is stored, and what was broken is gone from the file with it. What is under a key this
			// build does not know is not broken: it stays in the file, unused, and is still said
			r.ok(ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/rack", !slices.Contains(got, "nas-agent/rack"))}})
			kept := map[string]int{"fields that cannot be set": 7, "what Fairy writes, and a tail": 1}[name]
			ig := strs(r.options(ehNodeB).body["ignored"])
			if len(ig) != kept {
				t.Errorf("after the next change: %d ignored lines, want %d: %v", len(ig), kept, ig)
			}
			for _, line := range ig {
				if !strings.Contains(line, "it is not used (and left in the file as it is)") {
					t.Errorf("after the next change, still ignored: %q", line)
				}
			}
			if kept > 0 {
				raw := fmt.Sprint(r.storedOptions())
				if name == "fields that cannot be set" && (!strings.Contains(raw, "url:http://198.51.100.7:9101") || !strings.Contains(raw, "token_env:FAIRY_SESSION_SECRET")) || name != "fields that cannot be set" && !strings.Contains(raw, "more:1") {
					t.Errorf("the unknown keys after the next change: %s", raw)
				}
				// ... and never used: the agent's entry is config.yaml's
				for _, a := range r.s.Poller.Agents() {
					if a.ID == "lab-b-agent" && (a.URL != "http://lab-b-agent" || a.TokenEnv != "TOK_LAB_B" || !a.PowerControl || a.WolRelay) {
						t.Errorf("the agent's entry: %+v", a)
					}
				}
			}
		})
	}
}

// What the panel ignores under "host_options" is said in the log when the panel starts, also when no agent
// answers then (a poll without an answer asks nothing of the overlay): once.
func TestEditHostABrokenOverlayIsSaidAtTheStartAlsoWhenNoAgentAnswers(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &logs}, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	cfg := ehConfig(t, ehNASLinks)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "state.json"), []byte(`{"host_options": {"agents": {"nas-agent": {"power_links": 7}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b := testutil.NewBackend()
	b.PVEOf = func(string) ([]jsonx.M, error) { return nil, fmt.Errorf("down") }
	for _, id := range []string{ehNAS, "lab-a-agent", "lab-b-agent"} {
		b.Metrics[id] = func() (jsonx.M, error) { return nil, fmt.Errorf("down") }
	}
	e := ehStart(t, cfg, b)
	for i := 0; i < 3; i++ {
		e.s.Poller.PollOnce(context.Background())
	}
	if n := strings.Count(logs.String(), `an \"Edit host\" override is ignored: agent nas-agent: power_links is not of the form Fairy writes`); n != 1 {
		t.Fatalf("%d WARN lines about the ignored override on a panel none of whose agents answers, want 1:\n%s", n, logs.String())
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

// What is linked where decides what is shown, and nothing else. The same panel twice, once with overrides
// that give every UPS and plug to PVE-B (and leave everything out of the total): a shutdown of PVE-B, its
// power capabilities and the list of "Update unpowered hosts" are the same, and so is every request that goes
// to a machine.
func TestEditHostChangesNoControlPath(t *testing.T) {
	type seen struct{ caps, wake, power, requests string }
	look := func(withOverrides bool) seen {
		r := ehSetup(t)
		r.s.control = newRateLimiter(1000)
		if withOverrides {
			r.ok(ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/rack", true), ehUPS("nas-agent/desk", true)},
				"plugs": []any{map[string]any{"id": "nas-agent/a-plug", "linked": true, "in_total": false}, map[string]any{"id": "nas-agent/b-plug", "linked": true, "in_total": false},
					map[string]any{"id": "nas-agent/own", "linked": true, "in_total": false}}})
			r.ok(ehNodeA, map[string]any{"ups": []any{ehUPS("nas-agent/rack", false)}})
			r.poll()
			if got := strs(r.row(ehNodeB)["ups"]); len(got) != 2 {
				t.Fatalf("the overrides do not count: %v", got)
			}
		}
		r.b.Mu.Lock()
		r.b.Requests = nil
		r.b.Mu.Unlock()
		caps := r.do("GET", "/api/systems/"+ehNodeB+"/power", nil, r.hdr[:2]...)
		wake := r.do("GET", "/api/admin/wake-update", nil, r.hdr[:2]...)
		pw := r.do("POST", "/api/systems/"+ehNodeB+"/power", map[string]any{"action": "shutdown", "delay_s": 60, "confirm": "PVE-B"}, append([]string{"Content-Type", "application/json"}, r.hdr...)...)
		if caps.code != 200 || wake.code != 200 || pw.code != 200 {
			t.Fatalf("caps %d, wake-update %d, shutdown %d %s", caps.code, wake.code, pw.code, pw.raw)
		}
		hosts, _ := json.Marshal(wake.body["hosts"])
		r.b.Mu.Lock()
		defer r.b.Mu.Unlock()
		// times of day and ages differ between two panels; what a machine may be asked does not
		strip := regexp.MustCompile(`"(at|[a-z_]*_at|[a-z_]*_ago_s|[a-z_]*age_s|since|generated_at|last_seen|seen_s)":("[^"]*"|[0-9.e+-]+|null),?`)
		return seen{caps: strip.ReplaceAllString(string(caps.raw), ""), wake: strip.ReplaceAllString(string(hosts), ""),
			power: strip.ReplaceAllString(string(pw.raw), ""), requests: strings.Join(r.b.Requests, "\n")}
	}
	plain, changed := look(false), look(true)
	if plain.requests == "" || !strings.Contains(plain.requests, "lab-b-agent POST /power") {
		t.Fatalf("the shutdown did not go to the machine: %q", plain.requests)
	}
	if plain != changed {
		t.Errorf("with overrides something a control path does differs:\n%+v\n%+v", plain, changed)
	}
	// And in the source: the overlay is read where links are handed to the power and the UPS manager, by the
	// API of the dialog, by Remove and by `fairy admin config check`. Nowhere else.
	allowed := map[string]bool{
		"adminstate/hostoptions.go": true, "adminstate/hostoptions_read.go": true, "poller/hostoptions.go": true, "poller/agents.go": true,
		"web/api_hostoptions.go": true, "web/api_enroll.go": true, "admin/hostoptions_check.go": true,
	}
	// every way to get at the overlay or at the links made with it (a call; a comment that names one is none)
	word := regexp.MustCompile(`\b(HostOptions|HostOptionsRead|ReadHostOptions|EditHostOptions|hostOptions|forgetHostOptions|LinksOf|BaseLinks)\(|hostOptionsKey\]`)
	found := map[string]bool{}
	for _, root := range []string{"..", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel := strings.TrimPrefix(filepath.ToSlash(path), "../")
			if word.Match(src) {
				found[rel] = true
				if !allowed[rel] {
					t.Errorf("%s reads or names the \"Edit host\" overlay: only the poll of an agent's devices, the dialog's API, Remove and the config check may", rel)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for f := range allowed {
		if !found[f] {
			t.Errorf("%s no longer names the overlay: correct the list in this test", f)
		}
	}
	// in the poll, the links go to the two managers and nowhere else; the run of "Update unpowered hosts",
	// power control and the wake are other files and are not in the list above
	agents := string(must(os.ReadFile("../poller/agents.go")))
	if n := strings.Count(agents, "p.LinksOf(a)"); n != 1 {
		t.Errorf("poller/agents.go asks LinksOf %d times, want 1: when an agent's answer is handed to the two managers (its silence hands them no links: they read none then)", n)
	}
	for _, use := range regexp.MustCompile(`(?m)^.*\bl\.(PowerLinks|PowerExcludeFromTotal|UPSPowers)\b.*$`).FindAllString(agents, -1) {
		if !strings.Contains(use, "p.Power.UpdateRemote(") && !strings.Contains(use, "p.UPS.UpdateFromAgent(") {
			t.Errorf("poller/agents.go uses the links for something else: %s", strings.TrimSpace(use))
		}
	}
	for _, f := range []string{"../wakerun", "../powerctl"} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s is not where this test looks for it: %v", f, err)
		}
	}
}

// A request is small whatever it holds: each of its three lists takes at most 128 entries. The 129th is
// refused for that, whoever asks and whatever the entries are; 128 are looked at one by one.
func TestEditHostARequestHasAtMost128EntriesInAList(t *testing.T) {
	r := ehSetup(t)
	list := func(n int, entry func(i int) any) []any {
		var out []any
		for i := 0; i < n; i++ {
			out = append(out, entry(i))
		}
		return out
	}
	ups := func(i int) any { return ehUPS(fmt.Sprintf("nas-agent/u%d", i), true) }
	plug := func(i int) any { return map[string]any{"id": fmt.Sprintf("nas-agent/p%d", i), "linked": true} }
	reset := func(i int) any { return map[string]any{"id": fmt.Sprintf("nas-agent/p%d", i), "field": "plugs"} }
	for name, entry := range map[string]func(int) any{"ups": ups, "plugs": plug, "reset": reset} {
		over := r.put(ehNodeB, map[string]any{name: list(maxHostOptionEntries+1, entry)})
		if over.code != 422 || over.body["detail"] != "too many entries in one request" {
			t.Errorf("%d entries in %s: %d %s", maxHostOptionEntries+1, name, over.code, over.raw)
		}
		// at the limit the entries themselves are looked at: these name devices no agent relays
		at := r.put(ehNodeB, map[string]any{name: list(maxHostOptionEntries, entry)})
		if d := fmt.Sprint(at.body["detail"]); at.code != 422 || !strings.HasPrefix(d, "no agent relays the ") {
			t.Errorf("%d entries in %s: %d %s", maxHostOptionEntries, name, at.code, at.raw)
		}
	}
	if maxHostOptionEntries != 128 || r.storedOptions() != nil {
		t.Errorf("the limit is %d; stored: %v", maxHostOptionEntries, r.storedOptions())
	}
}

// What the API stores is checked by the same check that reads it back (config.LinkProblems): the names and ids
// of the decisions, and what counts with them laid over config.yaml. A change whose result an agent could not
// have (a 33rd row for one UPS, a 17th device) is refused, with the reason, and nothing is stored. What that
// check finds in config.yaml itself stops nothing here: a decision is about one line and copies no line of
// config.yaml, so a line of config.yaml that is wrong stays config.yaml's to put right, and the other lines
// can be changed.
func TestEditHostRefusesWhatTheSharedCheckWouldIgnore(t *testing.T) {
	var rows, devs []string
	for i := 0; i < config.MaxLinkRows; i++ {
		rows = append(rows, fmt.Sprintf("pc-%d", i))
	}
	for i := 0; i < config.MaxLinkDevices; i++ {
		devs = append(devs, fmt.Sprintf("d%d", i))
	}
	r := ehSetupWith(t, ehConfig(t, `ups_powers: {rack: [`+strings.Join(rows, ", ")+`]}, power_links: {Bad_ID: lab-a-agent}, power_exclude_from_total: [`+strings.Join(devs, ", ")+`]`), true)
	for name, c := range map[string]struct {
		body map[string]any
		want string
	}{
		"a 33rd row for one UPS": {map[string]any{"ups": []any{ehUPS("nas-agent/rack", true)}}, "NAS: ups_powers: UPS rack powers 33 rows, at most 32 are taken"},
		"a 17th device left out of the total": {map[string]any{"plugs": []any{map[string]any{"id": "nas-agent/own", "in_total": false}}},
			"NAS: power_exclude_from_total: 17 devices, and an agent relays at most 16"},
	} {
		id := ehNodeB
		if strings.Contains(name, "17th") {
			id = ehNAS // its own plug
		}
		got := r.put(id, c.body)
		if got.code != 422 || got.body["detail"] != c.want {
			t.Errorf("%s: %d %s\nwant 422 %q", name, got.code, got.raw, c.want)
		}
		if r.storedOptions() != nil {
			t.Fatalf("%s: refused, and stored: %v", name, r.storedOptions())
		}
	}
	// each refusal is in the audit log as one, with the reason, never as a change: one entry for each
	refused := map[string]string{}
	for _, en := range r.auditEntries("", "host.options") {
		if en["outcome"] == "ok" || !strings.HasPrefix(fmt.Sprint(en["outcome"]), "error 422: ") || en["params"].(map[string]any)["machine"] == nil {
			t.Errorf("audit: %v", en)
		}
		refused[fmt.Sprint(en["outcome"])] = fmt.Sprint(en["target"])
	}
	if want := map[string]string{"error 422: NAS: ups_powers: UPS rack powers 33 rows, at most 32 are taken": ehNodeB,
		"error 422: NAS: power_exclude_from_total: 17 devices, and an agent relays at most 16": ehNAS}; !maps.Equal(refused, want) || len(r.auditEntries("", "host.options")) != 2 {
		t.Errorf("the audit entries of the refusals: %v\nwant %v", refused, want)
	}
	// a line that is in order can be changed for the same agent: the desk UPS has room ...
	p := r.ok(ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/desk", true)}})
	if got := strs(p.body["changed"]); !slices.Equal(got, []string{"UPS Desk UPS (read by NAS): powers PVE-B"}) {
		t.Fatalf("changed: %v", got)
	}
	// ... and so can a plug's link, next to the link of config.yaml that names a device no agent can have
	// (Bad_ID): that line is config.yaml's own, and is not copied into what the panel stores
	p = r.ok(ehNodeB, map[string]any{"plugs": []any{map[string]any{"id": "nas-agent/b-plug", "linked": true}}})
	if got := strs(p.body["changed"]); !slices.Equal(got, []string{"Plug B plug (read by NAS): linked to PVE-B"}) || fmt.Sprint(r.storedAgent(ehNAS)["power_links"]) != "map[b-plug:lab-b-agent]" {
		t.Fatalf("a link next to config.yaml's Bad_ID: %s", p.raw)
	}
	// the limit holds for that plug too, now that it is PVE-B's: a 17th device cannot be left out of the total
	if x := r.put(ehNodeB, map[string]any{"plugs": []any{map[string]any{"id": "nas-agent/b-plug", "in_total": false}}}); x.code != 422 ||
		x.body["detail"] != "NAS: power_exclude_from_total: 17 devices, and an agent relays at most 16" {
		t.Fatalf("a 17th device on PVE-B's page: %d %s", x.code, x.raw)
	}
	// and what was stored is read back without anything ignored
	if ig := strs(r.options(ehNodeB).body["ignored"]); len(ig) != 0 {
		t.Fatalf("ignored: %v", ig)
	}
}
