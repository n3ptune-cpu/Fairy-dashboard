package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// "Run timings": an admin changes, in the panel, the timings of "Update unpowered hosts" (GET and PUT
// /api/admin/run-timings). These tests ask the real handler chain who may do that, what can be sent, which
// change asks for its own confirmation every time, where it is kept and what it changes.

func init() {
	// The route's row in the route / auth matrix (routes_test.go). The request changes off_after_s, and every
	// change of that asks to confirm, also right after a sign-in ("each"). The kill switch does not stop it.
	matrixRoutes = append(matrixRoutes, writeRoute{pattern: "PUT /api/admin/run-timings", path: "/api/admin/run-timings",
		body: map[string]any{"off_after_s": 360}, adminOnly: true, stepUp: true, each: true})
}

const rtPath = "/api/admin/run-timings"

var rtWrite = []string{"X-Requested-With", "fairy", "Content-Type", "application/json"}

// rtPut sends a change as the admin with the given cookie.
func (e *env) rtPut(cookie string, body any, hdr ...string) resp {
	return e.do("PUT", rtPath, body, append(append([]string{"Cookie", cookie}, rtWrite...), hdr...)...)
}

// rtStored is "run_timings" of state.json (nil: the key is not there).
func (e *env) rtStored() map[string]any {
	e.t.Helper()
	st := map[string]any{}
	b, err := os.ReadFile(filepath.Join(e.s.Cfg.DataDir, "state.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		e.t.Fatalf("state.json is not JSON any more: %v\n%s", err, b)
	}
	o, _ := st["run_timings"].(map[string]any)
	return o
}

// rtValues is the stored decisions as "key=value" in the list's order ("" when there are none).
func (e *env) rtValues() string {
	var out []string
	values, _ := e.rtStored()["values"].(map[string]any)
	for _, tm := range config.RunTimings() {
		if v, ok := values[tm.Key]; ok {
			out = append(out, fmt.Sprintf("%s=%v", tm.Key, v))
		}
	}
	return strings.Join(out, " ")
}

// rtRow is one timing of an answer of GET or PUT.
func rtRow(t *testing.T, r resp, key string) map[string]any {
	t.Helper()
	rows, _ := r.body["timings"].([]any)
	for _, x := range rows {
		if row, _ := x.(map[string]any); row["key"] == key {
			return row
		}
	}
	t.Fatalf("no timing %s in %s", key, r.raw)
	return nil
}

// Who may: nobody without an admin's browser session reads or changes a timing; an admin whose sign-in is
// old is asked to confirm with the panel's own lines; what is confirmed is stored once and written to the
// audit log with the old and the new value.
func TestRunTimingsWhoMay(t *testing.T) {
	a := signedIn(t) // an admin whose sign-in is an hour old
	e := a.e
	body := map[string]any{"settle_s": 90}
	if r := e.do("GET", rtPath, nil); r.code != 401 {
		t.Fatalf("GET, not signed in: %d", r.code)
	}
	if r := e.do("PUT", rtPath, body, rtWrite...); r.code != 401 {
		t.Fatalf("PUT, not signed in: %d", r.code)
	}
	v := model.User{Sub: "u7", Name: model.P("Vee"), Email: model.P("v@example.com"), Groups: []string{"viewers"}}
	vc := e.cookie(v, 0)
	_ = e.s.Access.EnsureSites([]string{"Home"})
	if _, _, err := e.s.Access.SetRole("u7", "viewer", []string{"Home"}, "test", false); err != nil {
		t.Fatal(err)
	}
	pend := e.cookie(model.User{Sub: "u8", Name: model.P("Pat"), Groups: []string{"viewers"}}, 0) // waits for access
	for who, ck := range map[string]string{"a viewer": vc, "a user who waits for access": pend} {
		if r := e.do("GET", rtPath, nil, "Cookie", ck); r.code != 403 || strings.Contains(string(r.raw), "timings\":") {
			t.Fatalf("GET, %s: %d %s", who, r.code, r.raw)
		}
		if r := e.rtPut(ck, body); r.code != 403 {
			t.Fatalf("PUT, %s: %d %s", who, r.code, r.raw)
		}
	}
	if r := e.do("GET", rtPath, nil, "Cookie", vc); r.body["detail"] != "only admins can change the run timings" {
		t.Fatalf("GET, a viewer: %s", r.raw)
	}
	for _, scope := range []string{"read", "control"} {
		tok, _, err := e.s.Tokens.Create("t-"+scope, "system", scope, "cli:test", 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, method := range []string{"GET", "PUT"} {
			if r := e.do(method, rtPath, body, "Authorization", "Bearer "+tok, "Content-Type", "application/json"); r.code != 403 || r.body["detail"] != "API tokens are not accepted on this endpoint" {
				t.Fatalf("%s with a %s token: %d %s", method, scope, r.code, r.raw)
			}
			if r := e.do(method, rtPath, body, append([]string{"Authorization", "Bearer " + tok, "Cookie", a.sess}, rtWrite...)...); r.code != 403 {
				t.Fatalf("%s with a %s token next to an admin's cookie: %d", method, scope, r.code)
			}
		}
	}
	if r := e.do("PUT", rtPath, body, "Cookie", a.sess, "Content-Type", "application/json"); r.code != 403 {
		t.Fatalf("an admin's cookie without the page's header: %d", r.code)
	}
	if e.rtStored() != nil {
		t.Fatal("something was stored by somebody who may not")
	}
	// the admin may look (reading asks for no confirmation): six timings, config.yaml's values
	get := e.do("GET", rtPath, nil, "Cookie", a.sess)
	if rows, _ := get.body["timings"].([]any); get.code != 200 || len(rows) != 6 || get.body["rev"] != 0.0 || get.body["active"] != false || get.body["confirm_wait_s"] != 600.0 {
		t.Fatalf("GET, the admin: %d %s", get.code, get.raw)
	}
	var keys []string
	for _, tm := range config.RunTimings() {
		row := rtRow(t, get, tm.Key)
		keys = append(keys, tm.Key)
		if row["value"] != float64(tm.Default) || row["from"] != "config" || row["config"] != float64(tm.Default) || row["min"] != float64(tm.Min) || row["max"] != float64(tm.Max) ||
			row["default"] != float64(tm.Default) || row["by"] != nil || row["at"] != nil {
			t.Errorf("%s before any change: %v", tm.Key, row)
		}
	}
	var confirm []string
	for _, key := range []string{"off_after_s", "shutdown_delay_s", "online_timeout_s", "settle_s", "agent_requests_per_min", "run_approval_wait_s"} {
		confirm = append(confirm, fmt.Sprint(rtRow(t, get, key)["confirm"]))
	}
	if got := strings.Join(confirm, ","); got != "always,lower,higher,,," {
		t.Errorf("which changes ask for their own confirmation: %q", got)
	}
	if strings.Join(keys, " ") != "off_after_s online_timeout_s settle_s shutdown_delay_s agent_requests_per_min run_approval_wait_s" {
		t.Errorf("the order: %v", keys)
	}
	// ... and is asked to confirm a change: nothing is stored, and the dialog gets the panel's own lines
	r := e.rtPut(a.sess, body, "X-Fairy-Action", "Run%20timings")
	d, _ := r.body["detail"].(map[string]any)
	if !stepUpRequired(r) || d["action"] != "change the run timings" || d["pending"] != true || d["per_action"] != nil ||
		!slices.Equal(strs(d["items"]), []string{"Pause before the shutdown: 90 s = 1 min 30 s (was 60 s = 1 minute)"}) ||
		!strings.Contains(fmt.Sprint(d["message"]), "changes to the run timings need a Pocket ID sign-in from the last") {
		t.Fatalf("an old sign-in: %d %s", r.code, r.raw)
	}
	if e.rtStored() != nil {
		t.Fatal("stored before it was confirmed")
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal("the confirmation:", cb.code)
	}
	if got := e.rtValues(); got != "settle_s=90" || e.rtStored()["rev"] != 1.0 {
		t.Fatalf("after the confirmation: %q %v", got, e.rtStored())
	}
	// the audit entry: who, the key, the old and the new value and where each came from
	entries := e.auditEntries("", "wakeupdate.timings")
	if len(entries) != 1 {
		t.Fatalf("%d audit entries, want 1", len(entries))
	}
	p, _ := entries[0]["params"].(map[string]any)
	ch, _ := p["changes"].([]any)
	if entries[0]["outcome"] != "ok" || entries[0]["target"] != "run timings" || len(ch) != 1 || p["rev"] != 1.0 ||
		fmt.Sprint(ch[0]) != "map[after:90 after_from:the panel before:60 before_from:config.yaml key:settle_s]" ||
		!slices.Equal(strs(p["lines"]), []string{"Pause before the shutdown: 90 s = 1 min 30 s (was 60 s = 1 minute)"}) {
		t.Fatalf("the audit entry: %v", entries[0])
	}
	me := e.do("GET", "/api/me", nil, "Cookie", a.sess)
	if pr, _ := me.body["pending_result"].(map[string]any); pr["ok"] != true || pr["message"] != "saved" || pr["label"] != "Run timings" {
		t.Fatalf("the kept result: %v", me.body["pending_result"])
	}
	// the bell has one line, without a number of the change
	notes := e.do("GET", "/api/notifications", nil, "Cookie", a.sess)
	if !strings.Contains(string(notes.raw), "run timings changed by ") {
		t.Fatalf("the bell: %s", notes.raw)
	}
	// inside the window the next plain change needs no confirmation; the kill switch does not stop it
	if err := e.s.Poller.Admin.SetControlDisabled(true, "test", "kill switch"); err != nil {
		t.Fatal(err)
	}
	r = e.rtPut(a.sess, map[string]any{"settle_s": 120, "agent_requests_per_min": 6, "run_approval_wait_s": 900})
	if r.code != 200 || r.body["ok"] != true || r.body["message"] != "saved" || e.rtValues() != "settle_s=120 agent_requests_per_min=6 run_approval_wait_s=900" ||
		!slices.Equal(strs(r.body["changed"]), []string{"Pause before the shutdown: 120 s = 2 minutes (was 90 s = 1 min 30 s)", "Requests a minute: 6 (was 8)",
			"Time for the second passkey: 900 s = 15 minutes (was 600 s = 10 minutes)"}) {
		t.Fatalf("inside the window, kill switch on: %d %s", r.code, r.raw)
	}
	if row := rtRow(t, r, "settle_s"); row["value"] != 120.0 || row["from"] != "panel" || row["config"] != 60.0 || row["by"] == nil || row["at"] == nil || r.body["rev"] != 2.0 || r.body["confirm_wait_s"] != 900.0 {
		t.Fatalf("the answer of the change: %s", r.raw)
	}
	// the rate limit of admin changes counts here: 429 with the time to wait
	e.s.adminRL = newRateLimiter(1)
	_ = e.rtPut(a.sess, map[string]any{"settle_s": 61})
	r = e.rtPut(a.sess, map[string]any{"settle_s": 62})
	if r.code != 429 || r.hdr.Get("Retry-After") == "" || !strings.HasPrefix(fmt.Sprint(r.body["detail"]), "too many changes; try again in ") {
		t.Fatalf("over the limit: %d %s", r.code, r.raw)
	}
}

// What can be sent is a closed set, checked before anybody is asked to confirm: the six keys with whole
// numbers in config.yaml's ranges, and "reset". Everything else is 422 (or 400), stores nothing and asks for
// no confirmation, for an admin whose sign-in is old as for one who just signed in.
func TestRunTimingsRequestIsAClosedSetCheckedBeforeAnyConfirmation(t *testing.T) {
	a := signedIn(t)
	e := a.e
	asked := len(e.auditEntries("", stepUpAction))
	bad := map[string]string{
		"nothing":                           `{}`,
		"only nulls":                        `{"off_after_s": null, "reset": null}`,
		"an empty reset":                    `{"reset": []}`,
		"a key that is no timing":           `{"url": "http://elsewhere"}`,
		"a token variable":                  `{"off_after_s": 360, "token_env": "FAIRY_X"}`,
		"power control":                     `{"power_control": true}`,
		"self-update":                       `{"self_update": true, "settle_s": 60}`,
		"a machine's key":                   `{"wake_update": {"off_after_s": 360}}`,
		"values":                            `{"values": {"off_after_s": 360}}`,
		"a name misspelled":                 `{"Off_After_S": 360}`,
		"upper case":                        `{"OFF_AFTER_S": 360}`,
		"a key twice":                       `{"off_after_s": 360, "off_after_s": 900}`,
		"a key twice, two spellings":        `{"off_after_s": 900, "OFF_AFTER_S": 300}`,
		"a fraction":                        `{"off_after_s": 360.5}`,
		"an exponent":                       `{"off_after_s": 3.6e2}`,
		"a text":                            `{"off_after_s": "360"}`,
		"true":                              `{"shutdown_delay_s": true}`,
		"a list":                            `{"settle_s": [60]}`,
		"an object":                         `{"settle_s": {"s": 60}}`,
		"zero":                              `{"off_after_s": 0}`,
		"negative":                          `{"shutdown_delay_s": -30}`,
		"far too large":                     `{"off_after_s": 99999999999999999999}`,
		"a reset of no timing":              `{"reset": ["url"]}`,
		"a reset of a number":               `{"reset": [360]}`,
		"a reset that is no list":           `{"reset": "off_after_s"}`,
		"a reset twice":                     `{"reset": ["settle_s", "settle_s"]}`,
		"set and reset":                     `{"settle_s": 90, "reset": ["settle_s"]}`,
		"more resets than there are keys":   `{"reset": ["settle_s", "settle_s", "settle_s", "settle_s", "settle_s", "settle_s", "settle_s"]}`,
		"not JSON":                          `{"off_after_s": `,
		"a list as the request":             `[{"off_after_s": 360}]`,
		"something after the object":        `{"settle_s": 90} {"off_after_s": 300}`,
		"one good and one bad value":        `{"settle_s": 90, "off_after_s": 299}`,
		"one good value and a bad reset":    `{"settle_s": 90, "reset": ["nope"]}`,
		"a good value and an unknown key":   `{"settle_s": 90, "x": 1}`,
		"a reset entry with other casing":   `{"reset": ["Settle_S"]}`,
		"the pacing as a fraction":          `{"agent_requests_per_min": 7.5}`,
		"a nested name spelled otherwise":   `{"reset": [{"Key": "settle_s"}]}`,
		"a value as a string with spaces":   `{"settle_s": " 90"}`,
		"a boolean reset":                   `{"reset": true}`,
		"a key of another block":            `{"step_up_window_s": 86400}`,
		"the approval users":                `{"users": ["somebody"]}`,
		"the never-in-browser list (wol)":   `{"wol_relay": true}`,
		"the never-in-browser list (owner)": `{"owner": "somebody"}`,
	}
	// every key just outside its range, at both ends
	for _, tm := range config.RunTimings() {
		bad[tm.Key+" below its range"] = fmt.Sprintf(`{"%s": %d}`, tm.Key, tm.Min-1)
		bad[tm.Key+" above its range"] = fmt.Sprintf(`{"%s": %d}`, tm.Key, tm.Max+1)
	}
	for _, who := range []string{"an old sign-in", "a fresh sign-in"} {
		if who == "a fresh sign-in" { // the admin confirms it's them: from here on inside the window
			if cb := a.stepUp("", false); cb.code != 303 {
				t.Fatal("the step-up:", cb.code)
			}
			asked = len(e.auditEntries("", stepUpAction))
		}
		for name, body := range bad {
			r := e.doRaw("PUT", rtPath, body, append([]string{"Cookie", a.sess}, rtWrite...)...)
			if r.code != 422 && r.code != 400 {
				t.Errorf("%s, %s: %d %s (want 422 or 400, and no confirmation)", name, who, r.code, r.raw)
			}
		}
		if n := len(e.auditEntries("", stepUpAction)); n != asked || e.rtStored() != nil {
			t.Errorf("%s: a request that cannot be taken asked for a confirmation or stored something: %d step-up entries (before %d), stored %v", who, n, asked, e.rtStored())
		}
	}
	fresh := a.sess
	// the sentence names the key and its range
	if r := e.rtPut(fresh, map[string]any{"off_after_s": 299}); r.body["detail"] != "off_after_s must be 300-3600" {
		t.Errorf("below the floor: %s", r.raw)
	}
	if r := e.rtPut(fresh, map[string]any{"shutdown_delay_s": 9}); r.body["detail"] != "shutdown_delay_s must be 10-3600" {
		t.Errorf("below the floor: %s", r.raw)
	}
	// who may is asked before what was sent: nobody else learns anything about the form of the request
	v := e.cookie(model.User{Sub: "u7", Name: model.P("Vee"), Email: model.P("v@example.com"), Groups: []string{"viewers"}}, 0)
	tok, _, _ := e.s.Tokens.Create("t-control", "system", "control", "cli:test", 0)
	for who, hdr := range map[string][]string{"nobody": rtWrite, "a viewer": append([]string{"Cookie", v}, rtWrite...), "a token": {"Authorization", "Bearer " + tok},
		"an admin's cookie without the header": {"Cookie", fresh}} {
		for _, body := range []string{`{}`, `{"url": 1}`, `{"off_after_s": 1}`, `{"off_after_s": `} {
			r := e.doRaw("PUT", rtPath, body, hdr...)
			if got := string(r.raw); (r.code != 401 && r.code != 403) || strings.Contains(got, "nothing to change") || strings.Contains(got, "invalid request") || strings.Contains(got, "must be") {
				t.Errorf("%s sent by %s: %d %s (want 401 or 403 and nothing about the body)", body, who, r.code, r.raw)
			}
		}
	}
	// both ends of every range are taken (each on its own; a change that asks for it is confirmed)
	for _, tm := range config.RunTimings() {
		for _, v := range []int{tm.Min, tm.Max} {
			r := e.rtPut(a.sess, map[string]any{tm.Key: v})
			if stepUpRequired(r) {
				if cb := a.confirm(pendingID(r), false); cb.code != 303 {
					t.Fatalf("%s = %d: the confirmation: %d", tm.Key, v, cb.code)
				}
				r = e.do("GET", rtPath, nil, "Cookie", a.sess)
			}
			if r.code != 200 || rtRow(t, r, tm.Key)["value"] != float64(v) {
				t.Errorf("%s = %d is not taken: %d %s", tm.Key, v, r.code, r.raw)
			}
		}
	}
}

// rtAsk sends a change and says how it was answered: "saved" (200, something changed), "nothing" (200,
// nothing to change), "each" (asked to confirm, every time), "window" (asked to confirm, the usual window).
func (a *armed) rtAsk(body any) (string, resp) {
	a.e.t.Helper()
	r := a.e.rtPut(a.sess, body)
	d, _ := r.body["detail"].(map[string]any)
	switch {
	case stepUpRequired(r) && d["per_action"] == true:
		return "each", r
	case stepUpRequired(r):
		return "window", r
	case r.code == 200 && r.body["message"] == "nothing to change":
		return "nothing", r
	case r.code == 200:
		return "saved", r
	}
	a.e.t.Fatalf("PUT %v: %d %s", body, r.code, r.raw)
	return "", r
}

// rtConfirmed sends a change that must ask for its own confirmation, and confirms it.
func (a *armed) rtConfirmed(body any) {
	a.e.t.Helper()
	how, r := a.rtAsk(body)
	if how != "each" {
		a.e.t.Fatalf("PUT %v was answered %q, want its own confirmation: %s", body, how, r.raw)
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		a.e.t.Fatalf("PUT %v: the confirmation: %d %s", body, cb.code, cb.raw)
	}
}

// The rule of the confirmation, timing by timing, for an admin who confirmed it's them a moment ago (so the
// usual window is open, and everything that only needs the window is saved at once):
//
//   - off_after_s: EVERY change asks for its own confirmation: lower, higher, back to config.yaml; also the
//     second change right after a confirmed one. The consequence is in the lines that are confirmed.
//   - shutdown_delay_s: when the value in force gets lower (a Reset that lowers it too).
//   - online_timeout_s: when the value in force gets higher.
//   - the others never.
//
// A request that mixes such a change with plain ones asks for the whole of it; one that changes nothing asks
// for nothing.
func TestRunTimingsWhichChangeAsksForItsOwnConfirmation(t *testing.T) {
	a := signedIn(t)
	e := a.e
	if cb := a.stepUp("", false); cb.code != 303 { // confirmed just now: inside the window from here on
		t.Fatal("the step-up:", cb.code)
	}
	const consequence = "A machine that is silent for longer than 6 minutes counts as off. If it was restarting and comes up during the run, the run switches it off after its update."
	// off_after_s, lowered
	how, r := a.rtAsk(map[string]any{"off_after_s": 360})
	d, _ := r.body["detail"].(map[string]any)
	if how != "each" || !slices.Equal(strs(d["items"]), []string{"Counts as off after: 360 s = 6 minutes (was 900 s = 15 minutes)", consequence}) ||
		d["message"] != "Confirm it's you: changes to when a run takes a machine for off or switches it off need their own Pocket ID sign-in, every time." {
		t.Fatalf("off_after_s lowered inside the window: %q %s", how, r.raw)
	}
	if e.rtStored() != nil {
		t.Fatal("stored before it was confirmed")
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal("the confirmation:", cb.code)
	}
	if e.rtValues() != "off_after_s=360" {
		t.Fatalf("after the confirmation: %q", e.rtValues())
	}
	// the second change right after the confirmed one: asked again, every direction
	for _, c := range []struct {
		body map[string]any
		line string
	}{
		{map[string]any{"off_after_s": 300}, "Counts as off after: 300 s = 5 minutes (was 360 s = 6 minutes)"},
		{map[string]any{"off_after_s": 1800}, "Counts as off after: 1800 s = 30 minutes (was 360 s = 6 minutes)"},
		{map[string]any{"off_after_s": 3600}, "Counts as off after: 3600 s = 1 hour (was 360 s = 6 minutes)"},
		{map[string]any{"reset": []string{"off_after_s"}}, "Counts as off after: back to config.yaml: 900 s = 15 minutes (was 360 s = 6 minutes)"},
		{map[string]any{"off_after_s": 900}, "Counts as off after: back to config.yaml: 900 s = 15 minutes (was 360 s = 6 minutes)"}, // config.yaml's value: the same as a reset
		{map[string]any{"settle_s": 30, "off_after_s": 420}, "Counts as off after: 420 s = 7 minutes (was 360 s = 6 minutes)"},       // with a plain change next to it
	} {
		how, r := a.rtAsk(c.body)
		d, _ := r.body["detail"].(map[string]any)
		if how != "each" || !slices.Contains(strs(d["items"]), c.line) {
			t.Errorf("%v right after a confirmed change: %q %s (want its own confirmation with %q)", c.body, how, r.raw, c.line)
		}
		if e.rtValues() != "off_after_s=360" {
			t.Fatalf("%v was stored without its confirmation: %q", c.body, e.rtValues())
		}
	}
	// the same value again changes nothing and asks for nothing
	if how, r := a.rtAsk(map[string]any{"off_after_s": 360}); how != "nothing" || len(strs(r.body["changed"])) != 0 {
		t.Errorf("the value that is stored already: %q %s", how, r.raw)
	}
	// the countdown: lower asks, higher does not
	if how, r := a.rtAsk(map[string]any{"shutdown_delay_s": 300}); how != "saved" || e.rtValues() != "off_after_s=360 shutdown_delay_s=300" {
		t.Fatalf("a longer countdown: %q %s", how, r.raw)
	}
	how, r = a.rtAsk(map[string]any{"shutdown_delay_s": 30})
	d, _ = r.body["detail"].(map[string]any)
	if how != "each" || !slices.Equal(strs(d["items"]), []string{"Shutdown countdown: 30 s (was 300 s = 5 minutes)", "Somebody at a machine has 30 seconds to cancel the run's shutdown."}) {
		t.Fatalf("a shorter countdown: %q %s", how, r.raw)
	}
	if how, r := a.rtAsk(map[string]any{"reset": []string{"shutdown_delay_s"}}); how != "each" { // config.yaml's 120 is lower than the 300 in force
		t.Fatalf("a reset that shortens the countdown: %q %s", how, r.raw)
	}
	a.rtConfirmed(map[string]any{"shutdown_delay_s": 30})
	if how, _ := a.rtAsk(map[string]any{"shutdown_delay_s": 20}); how != "each" {
		t.Fatalf("shorter again, right after a confirmed change: %q", how)
	}
	if how, r := a.rtAsk(map[string]any{"reset": []string{"shutdown_delay_s"}}); how != "saved" || e.rtValues() != "off_after_s=360" { // back to 120: longer
		t.Fatalf("a reset that makes the countdown longer: %q %s", how, r.raw)
	}
	// the wait for a woken machine: higher asks, lower does not
	if how, r := a.rtAsk(map[string]any{"online_timeout_s": 120}); how != "saved" {
		t.Fatalf("a shorter wait: %q %s", how, r.raw)
	}
	how, r = a.rtAsk(map[string]any{"online_timeout_s": 1800})
	d, _ = r.body["detail"].(map[string]any)
	if how != "each" || !slices.Equal(strs(d["items"]), []string{"Wait for a woken machine: 1800 s = 30 minutes (was 120 s = 2 minutes)",
		"A machine somebody switches on while the run waits for it (up to 30 minutes) is treated as woken by the run, and switched off after its update."}) {
		t.Fatalf("a longer wait: %q %s", how, r.raw)
	}
	if how, _ := a.rtAsk(map[string]any{"reset": []string{"online_timeout_s"}}); how != "each" { // back to 300: longer than 120
		t.Fatalf("a reset that makes the wait longer: %q", how)
	}
	// the others: every value inside the window, both directions
	for _, body := range []map[string]any{{"settle_s": 10}, {"settle_s": 600}, {"agent_requests_per_min": 30}, {"agent_requests_per_min": 1},
		{"run_approval_wait_s": 60}, {"run_approval_wait_s": 3600}, {"reset": []string{"settle_s", "agent_requests_per_min", "run_approval_wait_s"}}} {
		if how, r := a.rtAsk(body); how != "saved" {
			t.Errorf("%v inside the window: %q %s", body, how, r.raw)
		}
	}
	if e.rtValues() != "off_after_s=360 online_timeout_s=120" {
		t.Fatalf("at the end: %q", e.rtValues())
	}
	// a reset of a decision whose value is config.yaml's by now (config.yaml was edited to it): nothing changes
	// now, and nobody is asked for more than the window
	e.s.WakeRuns.Settings = e.s.WakeRuns.Settings.WithOffAfter(360)
	if how, r := a.rtAsk(map[string]any{"reset": []string{"off_after_s"}}); how != "saved" ||
		!slices.Equal(strs(r.body["changed"]), []string{"Counts as off after: back to config.yaml: nothing changes now (360 s = 6 minutes)"}) {
		t.Fatalf("a reset that changes no value: %q %s", how, r.raw)
	}
}

// What is confirmed is what is saved. The request waits on the server and the panel replays it: a second
// request with another value takes its place and has to be confirmed itself, a confirmation that names the
// first one runs nothing, and a request that is sent again by hand while one waits is asked again too.
func TestRunTimingsWhatIsConfirmedIsWhatIsSaved(t *testing.T) {
	a := signedIn(t)
	e := a.e
	_, first := a.rtAsk(map[string]any{"off_after_s": 600}) // the admin's own change: the dialog is open
	if id := pendingID(first); id == "" {
		t.Fatal("the first change was not kept")
	}
	// another request with the same session, with the value somebody else wants: it is not saved, it waits
	how, second := a.rtAsk(map[string]any{"off_after_s": 300})
	if how != "each" || e.rtStored() != nil {
		t.Fatalf("the swapped request: %q, stored %v", how, e.rtStored())
	}
	// the admin presses Continue in the dialog that showed 10 minutes: that request is gone, nothing runs
	if r := a.confirm(pendingID(first), false); r.code != 409 {
		t.Fatalf("the confirmation of the request that was replaced: %d %s", r.code, r.raw)
	}
	if e.rtStored() != nil {
		t.Fatalf("a value was stored that nobody confirmed: %v", e.rtStored())
	}
	// the admin's sign-in is fresh now or not: the swapped value is still asked for by itself
	if how, _ := a.rtAsk(map[string]any{"off_after_s": 300}); how != "each" || e.rtStored() != nil {
		t.Fatalf("the swapped value sent again: %q %v", how, e.rtStored())
	}
	_ = second
	// what the admin does confirm is saved, exactly once, and the next request is asked again
	a.rtConfirmed(map[string]any{"off_after_s": 600})
	if e.rtValues() != "off_after_s=600" || len(e.auditEntries("", "wakeupdate.timings")) != 1 {
		t.Fatalf("after the admin's own confirmation: %q, %d audit entries", e.rtValues(), len(e.auditEntries("", "wakeupdate.timings")))
	}
	if how, _ := a.rtAsk(map[string]any{"off_after_s": 300}); how != "each" || e.rtValues() != "off_after_s=600" {
		t.Fatalf("right after the confirmation: %q %q", how, e.rtValues())
	}
	// the same cookie from another address: asked, and nothing is kept for it (it could never be confirmed there)
	r := e.rtPut(a.sess, map[string]any{"off_after_s": 300}, otherAddr...)
	if d, _ := r.body["detail"].(map[string]any); !stepUpRequired(r) || d["pending"] != false || e.rtValues() != "off_after_s=600" {
		t.Fatalf("the cookie from another address: %d %s", r.code, r.raw)
	}
}

// A change that needed only the window when it was asked for, and needs its own confirmation by the time it
// is written (another admin changed the same timing in between): it is not written, the admin is asked.
func TestRunTimingsAChangeThatBecameOneForItsOwnConfirmationIsAskedAgain(t *testing.T) {
	a := signedIn(t)
	e := a.e
	if cb := a.stepUp("", false); cb.code != 303 {
		t.Fatal("the step-up:", cb.code)
	}
	// 200 s is a longer countdown than config.yaml's 120: the window is enough ...
	if how, _ := a.rtAsk(map[string]any{"shutdown_delay_s": 200}); how != "saved" {
		t.Fatal("the rig: a longer countdown is saved inside the window")
	}
	if how, _ := a.rtAsk(map[string]any{"reset": []string{"shutdown_delay_s"}}); how != "each" {
		t.Fatal("the rig: back to 120 is shorter and asks")
	}
	// ... the plan is made (200 -> 250: longer), and before the write somebody stores 3000
	e.s.rtBeforeWrite = func() {
		e.s.rtBeforeWrite = nil
		if _, _, err := e.s.Poller.Admin.EditRunTimings("another admin", func(o *adminstate.RunTimings) error { o.Set(config.KeyShutdownDelay, 3000); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	how, r := a.rtAsk(map[string]any{"shutdown_delay_s": 250})
	d, _ := r.body["detail"].(map[string]any)
	if how != "each" || e.rtValues() != "shutdown_delay_s=3000" || !slices.Contains(strs(d["items"]), "Shutdown countdown: 250 s = 4 min 10 s (was 3000 s = 50 minutes)") {
		t.Fatalf("a change that is a shorter countdown by the time it is written: %q %s, stored %q", how, r.raw, e.rtValues())
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 || e.rtValues() != "shutdown_delay_s=250" {
		t.Fatalf("after its confirmation: %d %q", cb.code, e.rtValues())
	}
}

// The overlay: each key is one decision over config.yaml. A value equal to config.yaml's is not stored, Reset
// takes a decision out, what no decision is about follows config.yaml, and config.yaml's own values are
// never written anywhere.
func TestRunTimingsAreDecisionsOverConfigYAML(t *testing.T) {
	a := signedIn(t)
	e := a.e
	if cb := a.stepUp("", false); cb.code != 303 {
		t.Fatal("the step-up:", cb.code)
	}
	// config.yaml of this panel: a pause of 45 s and 12 minutes for the second passkey
	e.s.WakeRuns.Settings.Settle, e.s.WakeRuns.Settings.SettleS = 45*time.Second, 45
	e.s.Cfg.Approval.RunApprovalWaitS = 720
	get := func() resp { return e.do("GET", rtPath, nil, "Cookie", a.sess) }
	if row := rtRow(t, get(), "settle_s"); row["value"] != 45.0 || row["config"] != 45.0 || row["from"] != "config" || row["default"] != 60.0 {
		t.Fatalf("config.yaml's own value: %v", row)
	}
	if r := get(); rtRow(t, r, "run_approval_wait_s")["value"] != 720.0 || r.body["confirm_wait_s"] != 720.0 {
		t.Fatalf("config.yaml's own wait: %s", r.raw)
	}
	// a value equal to config.yaml's is not stored (the default 60 is a change here: config.yaml says 45)
	if how, r := a.rtAsk(map[string]any{"settle_s": 45, "run_approval_wait_s": 720}); how != "nothing" || e.rtStored() != nil {
		t.Fatalf("config.yaml's values sent: %q %s %v", how, r.raw, e.rtStored())
	}
	if how, _ := a.rtAsk(map[string]any{"settle_s": 60, "agent_requests_per_min": 10}); how != "saved" || e.rtValues() != "settle_s=60 agent_requests_per_min=10" {
		t.Fatalf("two decisions: %q", e.rtValues())
	}
	r := get()
	if row := rtRow(t, r, "settle_s"); row["value"] != 60.0 || row["config"] != 45.0 || row["from"] != "panel" || row["by"] == nil {
		t.Fatalf("a decision: %v", row)
	}
	if row := rtRow(t, r, "shutdown_delay_s"); row["value"] != 120.0 || row["from"] != "config" || row["by"] != nil {
		t.Fatalf("a key nobody decided: %v", row)
	}
	// the values in force: the page of "Update unpowered hosts" (what its dialog and its info box show)
	page := e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
	if s := page.body["settings"].(map[string]any); s["settle_s"] != 60.0 || s["agent_requests_per_min"] != 10.0 || s["shutdown_delay_s"] != 120.0 || s["online_timeout_s"] != 300.0 || s["off_after_s"] != 900.0 {
		t.Fatalf("the page's settings: %v", s)
	}
	// a decision set back to config.yaml's value is taken out, like a reset
	if how, r := a.rtAsk(map[string]any{"settle_s": 45}); how != "saved" || e.rtValues() != "agent_requests_per_min=10" ||
		!slices.Equal(strs(r.body["changed"]), []string{"Pause before the shutdown: back to config.yaml: 45 s (was 60 s = 1 minute)"}) {
		t.Fatalf("back to config.yaml's value: %q %s", how, r.raw)
	}
	if how, r := a.rtAsk(map[string]any{"reset": []string{"agent_requests_per_min", "settle_s"}}); how != "saved" || e.rtValues() != "" ||
		!slices.Equal(strs(r.body["changed"]), []string{"Requests a minute: back to config.yaml: 8 (was 10)"}) || r.body["rev"] != 3.0 {
		t.Fatalf("a reset (one of the two keys had no decision): %q %s", how, r.raw)
	}
	if how, _ := a.rtAsk(map[string]any{"reset": []string{"agent_requests_per_min"}}); how != "nothing" {
		t.Fatalf("a reset of nothing: %q", how)
	}
	// config.yaml is edited later (the panel is started again with other values): a key nobody decided
	// follows it, a decided one does not
	if how, _ := a.rtAsk(map[string]any{"settle_s": 200}); how != "saved" {
		t.Fatal("a decision")
	}
	e.s.WakeRuns.Settings.Settle, e.s.WakeRuns.Settings.SettleS = 100*time.Second, 100
	e.s.WakeRuns.Settings.ShutdownDelay, e.s.WakeRuns.Settings.ShutdownDelayS = 240*time.Second, 240
	page = e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
	if s := page.body["settings"].(map[string]any); s["settle_s"] != 200.0 || s["shutdown_delay_s"] != 240.0 {
		t.Fatalf("after config.yaml changed: %v", s)
	}
	if row := rtRow(t, get(), "settle_s"); row["value"] != 200.0 || row["config"] != 100.0 || row["from"] != "panel" {
		t.Fatalf("the decided key: %v", row)
	}
	// nothing of this is in the panel's memory only: another panel on the same folder has the same in force
	if got := e.restart().s.wuTimesNow(); got.set.Settle != 200*time.Second || got.set.SettleS != 200 || got.confirmWait != 720 {
		t.Fatalf("after a restart: %+v", got)
	}
}

// A state.json that holds something else than Fairy writes under "run_timings" (a hand edit, a damaged file)
// never puts a value in force that config.yaml would refuse, and never stops the panel: config.yaml's value
// counts, the dialog says how many stored values are ignored, and the next save drops them.
func TestRunTimingsAHostileStoreNeverLowersAFloor(t *testing.T) {
	for name, stored := range map[string]string{
		"below every floor":   `{"rev": 9, "values": {"off_after_s": 1, "shutdown_delay_s": 0, "online_timeout_s": 5, "settle_s": 1, "agent_requests_per_min": 0, "run_approval_wait_s": 1}}`,
		"above every ceiling": `{"values": {"off_after_s": 86400, "shutdown_delay_s": 99999, "online_timeout_s": 99999, "settle_s": 99999, "agent_requests_per_min": 1000, "run_approval_wait_s": 99999}}`,
		"negative":            `{"values": {"off_after_s": -900, "shutdown_delay_s": -1}}`,
		"fractions and texts": `{"values": {"off_after_s": 300.5, "shutdown_delay_s": "10", "settle_s": null, "online_timeout_s": [60]}}`,
		"not an object":       `"off_after_s=300"`,
		"values is a list":    `{"values": [300, 10]}`,
	} {
		a := signedIn(t)
		e := a.e
		if err := os.WriteFile(filepath.Join(e.s.Cfg.DataDir, "state.json"), []byte(`{"wake_update_hosts": ["laptop"], "run_timings": `+stored+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		e2 := e.restart() // the panel starts with it
		got := e2.s.wuTimesNow()
		for _, tm := range config.RunTimings() {
			if v := got.value(tm.Key); v != tm.Default {
				t.Errorf("%s: %s is %d in force, config.yaml says %d", name, tm.Key, v, tm.Default)
			}
		}
		if tk := e2.s.runTimings(); tk.WaitS != 960 || tk.ConfirmWaitS != 600 || tk.WithinS != 1560 {
			t.Errorf("%s: the ticket numbers: %+v", name, tk)
		}
		page := e2.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
		if s, _ := page.body["settings"].(map[string]any); page.code != 200 || s["off_after_s"] != 900.0 || s["shutdown_delay_s"] != 120.0 || page.body["off_after_s"] != 900.0 {
			t.Errorf("%s: the page: %d %v", name, page.code, page.body["settings"])
		}
		r := e2.do("GET", rtPath, nil, "Cookie", a.sess)
		if n, _ := r.body["ignored_count"].(float64); r.code != 200 || n < 1 || len(strs(r.body["ignored"])) < 1 {
			t.Errorf("%s: the dialog's data does not say that stored values are ignored: %s", name, r.raw)
		}
		for _, tm := range config.RunTimings() {
			if row := rtRow(t, r, tm.Key); row["value"] != float64(tm.Default) || row["from"] != "config" {
				t.Errorf("%s: %s in the dialog: %v", name, tm.Key, row)
			}
		}
		if !e2.s.Poller.Admin.WakeUpdateEnabled("laptop") {
			t.Errorf("%s: the rest of state.json was not read", name)
		}
	}
}
