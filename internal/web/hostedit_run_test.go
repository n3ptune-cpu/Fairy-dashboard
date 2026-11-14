package web

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// "Edit host", a machine's own run timings (off_after_s and shutdown_delay_s), through the real handler
// chain: who may, what can be sent, the confirmation every change asks for, where a decision is kept and
// what it lies over, and that a run keeps the numbers it started with.

func ehPath(id string) string { return "/api/admin/hosts/" + id + "/options" }

func (a *armed) ehPut(id string, body any, hdr ...string) resp {
	return a.e.do("PUT", ehPath(id), body, append([]string{"Cookie", a.sess, "X-Requested-With", "fairy", "Content-Type", "application/json"}, hdr...)...)
}

// ehAsk sends a change of "Edit host" and says how it was answered: "each" (its own confirmation), "window"
// (the usual one), "saved" or "nothing".
func (a *armed) ehAsk(id string, body any) (string, resp) {
	a.e.t.Helper()
	r := a.ehPut(id, body)
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
	a.e.t.Fatalf("PUT %s %v: %d %s", id, body, r.code, r.raw)
	return "", r
}

// ehConfirmed sends a change that must ask for its own confirmation, confirms it, and returns the lines the
// confirmation showed and what the panel's replay answered.
func (a *armed) ehConfirmed(id string, body any) (items []string, res map[string]any) {
	a.e.t.Helper()
	how, r := a.ehAsk(id, body)
	d, _ := r.body["detail"].(map[string]any)
	if how != "each" || d["pending"] != true {
		a.e.t.Fatalf("PUT %s %v was answered %q, want its own confirmation: %s", id, body, how, r.raw)
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		a.e.t.Fatalf("PUT %s %v: the confirmation: %d %s", id, body, cb.code, cb.raw)
	}
	res, _ = a.e.do("GET", "/api/me", nil, "Cookie", a.sess).body["pending_result"].(map[string]any)
	if res == nil {
		a.e.t.Fatalf("the confirmed change of %s %v has no result", id, body)
	}
	return strs(d["items"]), res
}

// ehRun is one of the machine's two timings in the dialog's data.
func (a *armed) ehRun(id, key string) map[string]any {
	a.e.t.Helper()
	g := a.e.do("GET", ehPath(id), nil, "Cookie", a.sess)
	h, _ := g.body["host"].(map[string]any)
	run, _ := h["run"].(map[string]any)
	rows, _ := run["timings"].([]any)
	for _, x := range rows {
		if row, _ := x.(map[string]any); row["key"] == key {
			return row
		}
	}
	a.e.t.Fatalf("the options of %s have no run timing %s: %d %s", id, key, g.code, g.raw)
	return nil
}

func ehRunIs(row map[string]any, value int, from string, base int, baseFrom string) bool {
	return row["value"] == float64(value) && row["from"] == from && row["base"] == float64(base) && row["base_from"] == baseFrom
}

// storedRun is "wake_update" of a machine's entry in state.json, as "key=value" in the list's order.
func (e *env) storedRun(id string) string {
	e.t.Helper()
	run, _ := e.storedHost(id)["wake_update"].(map[string]any)
	var out []string
	for _, tm := range config.MachineRunTimings() {
		if v, ok := run[tm.Key]; ok {
			out = append(out, fmt.Sprintf("%s=%v", tm.Key, v))
		}
	}
	if len(out) != len(run) {
		e.t.Fatalf("wake_update of %s holds something else than the two timings: %v", id, run)
	}
	return strings.Join(out, " ")
}

// A decision lies over what counts for the machine anyway: its own block in config.yaml, or where it has
// none, the panel's timing. The dialog says the value, where it comes from and what would count without the
// decision; a value that counts anyway is not stored and takes a stored one out; Reset takes one out. The
// lines of the confirmation, of the answer and of the audit log are the same, with what a change means for
// the machine under it.
func TestEditHostRunTimingsAreDecisionsOverWhatCountsAnyway(t *testing.T) {
	a := signedIn(t)
	e := a.e
	newFarm(t, e)
	e.s.control, e.s.login = newRateLimiter(1000), newRateLimiter(1000)               // this test confirms many changes in one minute
	e.s.Poller.Override("pc-b").WakeUpdate = &config.SystemWakeUpdate{OffAfterS: 600} // systems: - {id: pc-b, wake_update: {off_after_s: 600}}
	e.rtSet(map[string]int{config.KeyShutdownDelay: 30})                              // "Run timings": the panel's countdown is 30 s
	for id, want := range map[string][2]string{"pc-a": {"900 run", "30 run"}, "pc-b": {"600 config", "30 run"}} {
		for i, tm := range config.MachineRunTimings() {
			row := a.ehRun(id, tm.Key)
			if got := fmt.Sprintf("%v %v", row["value"], row["from"]); got != want[i] || row["base"] != row["value"] || row["base_from"] != row["from"] ||
				row["min"] != float64(tm.Min) || row["max"] != float64(tm.Max) || row["by"] != nil || row["at"] != nil {
				t.Fatalf("%s %s before any change: %v", id, tm.Key, row)
			}
		}
	}
	// pc-a has no block: 360 s is its own from now on
	lines := []string{"Counts as off after: 360 s = 6 minutes (was 900 s = 15 minutes)",
		"If PC-A is silent for longer than 6 minutes, it counts as off. If it was restarting and comes up during a run, the run can switch it off after its update."}
	items, res := a.ehConfirmed("pc-a", map[string]any{"wake_update": map[string]any{"off_after_s": 360}})
	if !slices.Equal(items, lines) || res["ok"] != true || res["message"] != "changes saved" {
		t.Fatalf("the first decision: confirmed %q, answered %v", items, res)
	}
	if got := e.storedRun("pc-a"); got != "off_after_s=360" {
		t.Fatalf("stored: %q", got)
	}
	if row := a.ehRun("pc-a", "off_after_s"); !ehRunIs(row, 360, "panel", 900, "run") || row["by"] == nil || row["at"] == nil {
		t.Fatalf("the dialog after the change: %v", row)
	}
	if row := a.ehRun("pc-a", "shutdown_delay_s"); !ehRunIs(row, 30, "run", 30, "run") || row["by"] != nil {
		t.Fatalf("the other timing was not changed: %v", row)
	}
	audit := e.auditEntries("", "host.options")
	p, _ := audit[0]["params"].(map[string]any)
	if c := changeOf(p, "off_after_s"); audit[0]["target"] != "pc-a" || audit[0]["outcome"] != "ok" || c["before"] != 900.0 || c["after"] != 360.0 ||
		c["before_from"] != "run timings" || c["after_from"] != "panel" || c["stored"] != 360.0 || c["host"] != "pc-a" || !slices.Equal(strs(p["lines"]), lines) || p["run_going"] != nil {
		t.Fatalf("the audit entry: %v", audit[0])
	}
	// the page of "Update unpowered hosts" and the poller have it at once
	if off, delay := e.s.Poller.WakeUpdateOverride("pc-a"); off != 360 || delay != 0 {
		t.Fatalf("in force for pc-a: %d, %d", off, delay)
	}
	if pa := wuHostRow(t, e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess), "pc-a"); pa["off_after_s"] != 360.0 || pa["shutdown_delay_s"] != nil {
		t.Fatalf("pc-a's row on the page: %v", pa)
	}
	// the panel's own value typed for it: the decision goes, and the lines say where the value comes from now
	items, _ = a.ehConfirmed("pc-a", map[string]any{"wake_update": map[string]any{"off_after_s": 900}})
	if !slices.Equal(items, []string{"Counts as off after: back to Run timings: 900 s = 15 minutes (was 360 s = 6 minutes)",
		"If PC-A is silent for longer than 15 minutes, it counts as off. If it was restarting and comes up during a run, the run can switch it off after its update."}) {
		t.Fatalf("the panel's own value typed: %q", items)
	}
	if e.storedHost("pc-a") != nil {
		t.Fatalf("a value that counts anyway is stored: %v", e.storedHost("pc-a"))
	}
	if c := changeOf(e.auditEntries("", "host.options")[0]["params"].(map[string]any), "off_after_s"); c["after_from"] != "run timings" || c["before_from"] != "panel" || c["stored"] != nil {
		t.Fatalf("the audit entry of a decision that went: %v", c)
	}
	// pc-b has a block: both timings, each with its sentence (the countdown gets shorter)
	items, _ = a.ehConfirmed("pc-b", map[string]any{"wake_update": map[string]any{"off_after_s": 420, "shutdown_delay_s": 20}})
	if !slices.Equal(items, []string{"Counts as off after: 420 s = 7 minutes (was 600 s = 10 minutes)",
		"If PC-B is silent for longer than 7 minutes, it counts as off. If it was restarting and comes up during a run, the run can switch it off after its update.",
		"Shutdown countdown: 20 s (was 30 s)", "Somebody at PC-B has 20 seconds to cancel a run's shutdown."}) {
		t.Fatalf("two timings: %q", items)
	}
	if got := e.storedRun("pc-b"); got != "off_after_s=420 shutdown_delay_s=20" {
		t.Fatalf("stored: %q", got)
	}
	if row := a.ehRun("pc-b", "off_after_s"); !ehRunIs(row, 420, "panel", 600, "config") {
		t.Fatalf("pc-b in the dialog: %v", row)
	}
	// a longer countdown: no sentence under it (nobody loses time to stop a shutdown)
	if items, _ = a.ehConfirmed("pc-b", map[string]any{"wake_update": map[string]any{"shutdown_delay_s": 45}}); !slices.Equal(items, []string{"Shutdown countdown: 45 s (was 20 s)"}) {
		t.Fatalf("a longer countdown: %q", items)
	}
	// Reset: config.yaml's own value counts again
	items, _ = a.ehConfirmed("pc-b", map[string]any{"reset": []any{map[string]any{"field": "off_after_s"}}})
	if !slices.Equal(items, []string{"Counts as off after: back to config.yaml: 600 s = 10 minutes (was 420 s = 7 minutes)",
		"If PC-B is silent for longer than 10 minutes, it counts as off. If it was restarting and comes up during a run, the run can switch it off after its update."}) || e.storedRun("pc-b") != "shutdown_delay_s=45" {
		t.Fatalf("Reset: %q, stored %q", items, e.storedRun("pc-b"))
	}
	if c := changeOf(e.auditEntries("", "host.options")[0]["params"].(map[string]any), "off_after_s"); c["after_from"] != "config.yaml" || c["after"] != 600.0 {
		t.Fatalf("the audit entry of the Reset: %v", c)
	}
	// config.yaml's own value typed, and a Reset of what was never changed: asked, and nothing to change
	for _, body := range []map[string]any{{"wake_update": map[string]any{"off_after_s": 600}}, {"reset": []any{map[string]any{"field": "off_after_s"}}}} {
		if items, res := a.ehConfirmed("pc-b", body); len(items) != 0 || res["message"] != "nothing to change" || e.storedRun("pc-b") != "shutdown_delay_s=45" {
			t.Fatalf("%v: %q %v, stored %q", body, items, res, e.storedRun("pc-b"))
		}
	}
	// the panel's own countdown becomes the machine's 45 s: a Reset then changes no value, and says so
	e.rtSet(map[string]int{config.KeyShutdownDelay: 45})
	if row := a.ehRun("pc-b", "shutdown_delay_s"); !ehRunIs(row, 45, "panel", 45, "run") {
		t.Fatalf("a decision that is the panel's value now is still a decision: %v", row)
	}
	if items, _ = a.ehConfirmed("pc-b", map[string]any{"reset": []any{map[string]any{"field": "shutdown_delay_s"}}}); !slices.Equal(items,
		[]string{"Shutdown countdown: back to Run timings: nothing changes now (45 s)"}) || e.storedHost("pc-b") != nil {
		t.Fatalf("a Reset that changes no value: %q, stored %v", items, e.storedHost("pc-b"))
	}
	// next to a name: one request, one entry, and the sentence has the new name
	items, _ = a.ehConfirmed("pc-a", map[string]any{"name": "Den PC", "wake_update": map[string]any{"off_after_s": 300}})
	if !slices.Equal(items, []string{"Name: Den PC (was PC-A)", "Counts as off after: 300 s = 5 minutes (was 900 s = 15 minutes)",
		"If Den PC is silent for longer than 5 minutes, it counts as off. If it was restarting and comes up during a run, the run can switch it off after its update."}) {
		t.Fatalf("a name and a timing: %q", items)
	}
	if h := e.storedHost("pc-a"); h["name"] != "Den PC" || e.storedRun("pc-a") != "off_after_s=300" || h["wol"] != nil {
		t.Fatalf("stored: %v", h)
	}
}

// Who may, and what can be sent. Nobody without an admin's browser session changes a timing. What cannot be
// taken is refused (422) before anybody is asked to confirm it: a value outside its range, a key that is
// none of the two, a value and its Reset at once. The floors are the ones of config.yaml.
func TestEditHostRunTimingsWhoMayAndWhatCanBeSent(t *testing.T) {
	a := signedIn(t) // an admin whose sign-in is an hour old
	e := a.e
	newFarm(t, e)
	body := map[string]any{"wake_update": map[string]any{"off_after_s": 300}}
	write := []string{"X-Requested-With", "fairy", "Content-Type", "application/json"}
	if r := e.do("PUT", ehPath("pc-a"), body, write...); r.code != 401 {
		t.Fatalf("not signed in: %d", r.code)
	}
	vc := e.cookie(model.User{Sub: "u7", Name: model.P("Vee"), Email: model.P("v@example.com"), Groups: []string{"viewers"}}, 0)
	_ = e.s.Access.EnsureSites([]string{"Home"})
	if _, _, err := e.s.Access.SetRole("u7", "viewer", []string{"Home"}, "test", false); err != nil {
		t.Fatal(err)
	}
	pend := e.cookie(model.User{Sub: "u8", Name: model.P("Pat"), Groups: []string{"viewers"}}, 0) // waits for access
	for who, ck := range map[string]string{"a viewer": vc, "a user who waits for access": pend} {
		if r := e.do("PUT", ehPath("pc-a"), body, append([]string{"Cookie", ck}, write...)...); r.code != 403 {
			t.Fatalf("%s: %d %s", who, r.code, r.raw)
		}
		if r := e.do("GET", ehPath("pc-a"), nil, "Cookie", ck); r.code != 403 || strings.Contains(string(r.raw), "off_after_s") {
			t.Fatalf("GET, %s: %d %s", who, r.code, r.raw)
		}
	}
	for _, scope := range []string{"read", "control"} {
		tok, _, err := e.s.Tokens.Create("t-"+scope, "system", scope, "cli:test", 0)
		if err != nil {
			t.Fatal(err)
		}
		if r := e.do("PUT", ehPath("pc-a"), body, "Authorization", "Bearer "+tok, "Content-Type", "application/json"); r.code != 403 || r.body["detail"] != "API tokens are not accepted on this endpoint" {
			t.Fatalf("a %s token: %d %s", scope, r.code, r.raw)
		}
		if r := e.do("PUT", ehPath("pc-a"), body, append([]string{"Authorization", "Bearer " + tok, "Cookie", a.sess}, write...)...); r.code != 403 {
			t.Fatalf("a %s token next to an admin's cookie: %d", scope, r.code)
		}
	}
	if r := e.do("PUT", ehPath("pc-a"), body, "Cookie", a.sess, "Content-Type", "application/json"); r.code != 403 {
		t.Fatalf("an admin's cookie without the page's header: %d", r.code)
	}
	// what can never be taken: said at once, to the admin whose sign-in is old too (never 401 "confirm it's you")
	run := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return map[string]any{"wake_update": m}
	}
	reset := func(fields ...any) []any {
		var out []any
		for _, f := range fields {
			out = append(out, map[string]any{"field": f})
		}
		return out
	}
	for what, bad := range map[string]any{
		"a second below the floor":      run("off_after_s", 299),
		"a minute":                      run("off_after_s", 60),
		"zero":                          run("off_after_s", 0),
		"negative":                      run("off_after_s", -900),
		"above the range":               run("off_after_s", 3601),
		"a fraction":                    run("off_after_s", 300.5),
		"a text":                        run("off_after_s", "600"),
		"a countdown below its floor":   run("shutdown_delay_s", 9),
		"a countdown of zero":           run("shutdown_delay_s", 0),
		"a countdown above the range":   run("shutdown_delay_s", 3601),
		"one good value and a bad one":  run("off_after_s", 600, "shutdown_delay_s", 5),
		"nothing in it":                 run(),
		"a timing of the panel":         run("online_timeout_s", 60),
		"the pacing":                    run("agent_requests_per_min", 30),
		"another key":                   run("off_after_s", 600, "url", "http://198.51.100.7"),
		"a key in another spelling":     run("Off_After_S", 600),
		"a list":                        map[string]any{"wake_update": []any{600, 30}},
		"a value and its Reset":         map[string]any{"wake_update": map[string]any{"off_after_s": 600}, "reset": reset("off_after_s")},
		"a Reset twice":                 map[string]any{"reset": reset("off_after_s", "off_after_s")},
		"a Reset with an id":            map[string]any{"reset": []any{map[string]any{"field": "shutdown_delay_s", "id": "pc-b"}}},
		"a Reset of a panel's timing":   map[string]any{"reset": reset("settle_s")},
		"a key next to it at the top":   map[string]any{"wake_update": map[string]any{"off_after_s": 600}, "power_control": true},
		"the timings at the top":        map[string]any{"off_after_s": 600},
		"the key twice, in two letters": jsonRaw(`{"wake_update": {"off_after_s": 600}, "Wake_Update": {"off_after_s": 300}}`),
		"the value twice":               jsonRaw(`{"wake_update": {"off_after_s": 600, "off_after_s": 300}}`),
	} {
		r := a.ehPut("pc-a", bad)
		if r.code != 422 && r.code != 400 {
			t.Errorf("%s: %d %s", what, r.code, r.raw)
		}
	}
	if e.storedHost("pc-a") != nil || len(e.auditEntries("", "host.options")) != 0 {
		t.Fatalf("something was stored or audited as a change: %v %v", e.storedHost("pc-a"), e.auditEntries("", "host.options"))
	}
	if r := a.ehPut("pc-a", run("off_after_s", 299)); r.code != 422 || !strings.Contains(fmt.Sprint(r.body["detail"]), "wake_update: off_after_s must be 300 to 3600") {
		t.Errorf("the refusal names the range: %d %s", r.code, r.raw)
	}
	if r := a.ehPut("pc-a", run("shutdown_delay_s", 9)); r.code != 422 || !strings.Contains(fmt.Sprint(r.body["detail"]), "wake_update: shutdown_delay_s must be 10 to 3600") {
		t.Errorf("the refusal names the range: %d %s", r.code, r.raw)
	}
	// the ends of the ranges are asked for (and can then be confirmed)
	for _, ok := range []map[string]any{run("off_after_s", 300), run("off_after_s", 3600), run("shutdown_delay_s", 10), run("shutdown_delay_s", 3600)} {
		if how, r := a.ehAsk("pc-a", ok); how != "each" {
			t.Errorf("%v: %q %s", ok, how, r.raw)
		}
	}
	if e.storedHost("pc-a") != nil {
		t.Fatalf("something was stored without a confirmation: %v", e.storedHost("pc-a"))
	}
}

// jsonRaw is a request body sent as it is written (two readings of one key cannot be made from a map).
type jsonRaw string

func (j jsonRaw) MarshalJSON() ([]byte, error) { return []byte(j), nil }

// Every change of a machine's own run timings asks for a confirmation of its own: the first, the second
// right after a confirmed one, a longer countdown, a Reset; also for an admin who confirmed something else a
// moment ago, for whom a change of the name is saved at once. What is confirmed is what is saved: a second
// request takes the place of the first and has to be confirmed itself.
func TestEditHostRunTimingsAskForTheirOwnConfirmationEveryTime(t *testing.T) {
	a := signedIn(t)
	e := a.e
	newFarm(t, e)
	e.s.control, e.s.login = newRateLimiter(1000), newRateLimiter(1000) // this test confirms many changes in one minute
	if cb := a.stepUp("", false); cb.code != 303 {                      // confirmed just now: inside the window from here on
		t.Fatal("the step-up:", cb.code)
	}
	if how, r := a.ehAsk("pc-a", map[string]any{"name": "Den PC"}); how != "saved" {
		t.Fatalf("the rig: a name is saved inside the window: %q %s", how, r.raw)
	}
	run := func(key string, v int) map[string]any { return map[string]any{"wake_update": map[string]any{key: v}} }
	for i, body := range []map[string]any{
		run("off_after_s", 360), // lower
		run("off_after_s", 420), // right after a confirmed one, and higher
		run("off_after_s", 3600),
		run("shutdown_delay_s", 300), // a longer countdown
		run("shutdown_delay_s", 10),
		{"reset": []any{map[string]any{"field": "off_after_s"}}},
		{"reset": []any{map[string]any{"field": "shutdown_delay_s"}}},
		{"name": "PC One", "wake_update": map[string]any{"shutdown_delay_s": 200}}, // next to something the window is enough for
	} {
		before := fmt.Sprint(e.storedHost("pc-a"))
		how, r := a.ehAsk("pc-a", body)
		d, _ := r.body["detail"].(map[string]any)
		if how != "each" || d["pending"] != true || fmt.Sprint(e.storedHost("pc-a")) != before ||
			!strings.Contains(fmt.Sprint(d["message"]), "changes to a host's run timings need their own Pocket ID sign-in, every time") {
			t.Fatalf("change %d %v: %q %s (stored %v)", i, body, how, r.raw, e.storedHost("pc-a"))
		}
		if cb := a.confirm(pendingID(r), false); cb.code != 303 {
			t.Fatalf("change %d: the confirmation: %d", i, cb.code)
		}
		if fmt.Sprint(e.storedHost("pc-a")) == before {
			t.Fatalf("change %d %v was confirmed and nothing was stored", i, body)
		}
	}
	if got := e.storedRun("pc-a"); got != "shutdown_delay_s=200" || e.storedHost("pc-a")["name"] != "PC One" {
		t.Fatalf("at the end: %q %v", got, e.storedHost("pc-a"))
	}
	// with Wake-on-LAN in the same request: one confirmation, which names both
	_, r := a.ehAsk("pc-a", map[string]any{"wol": map[string]any{"port": 7}, "wake_update": map[string]any{"off_after_s": 600}})
	if d, _ := r.body["detail"].(map[string]any); !strings.Contains(fmt.Sprint(d["message"]), "changes to a host's Wake-on-LAN and run timings need their own Pocket ID sign-in, every time") {
		t.Fatalf("with Wake-on-LAN: %s", r.raw)
	}
	// what is confirmed is what is saved
	_, first := a.ehAsk("pc-b", run("off_after_s", 600)) // the admin's own change: the dialog is open
	how, second := a.ehAsk("pc-b", run("off_after_s", 300))
	if how != "each" || pendingID(first) == "" || pendingID(second) == pendingID(first) || e.storedHost("pc-b") != nil {
		t.Fatalf("the swapped request: %q, stored %v", how, e.storedHost("pc-b"))
	}
	if r := a.confirm(pendingID(first), false); r.code != 409 { // "Continue" in the dialog that showed 10 minutes
		t.Fatalf("the confirmation of the request that was replaced: %d %s", r.code, r.raw)
	}
	if how, _ := a.ehAsk("pc-b", run("off_after_s", 300)); how != "each" || e.storedHost("pc-b") != nil {
		t.Fatalf("the swapped value sent again: %q %v", how, e.storedHost("pc-b"))
	}
	if items, _ := a.ehConfirmed("pc-b", run("off_after_s", 600)); len(items) != 2 || e.storedRun("pc-b") != "off_after_s=600" {
		t.Fatalf("the admin's own change: %q, stored %q", items, e.storedRun("pc-b"))
	}
	// the same cookie from another address: asked, and nothing is kept for it
	r = a.ehPut("pc-b", run("off_after_s", 300), otherAddr...)
	if d, _ := r.body["detail"].(map[string]any); !stepUpRequired(r) || d["pending"] != false || e.storedRun("pc-b") != "off_after_s=600" {
		t.Fatalf("the cookie from another address: %d %s", r.code, r.raw)
	}
}

// A run takes a machine's own two timings when it starts and keeps them. With its own off_after_s of 360 and
// a countdown of 30 s, changed in "Edit host", pc-a is "off" after seven minutes of silence and is asked for
// 30 s. The two are changed while the run is going: the answer and the audit entry say that the run keeps
// its numbers, the run does, and the next run has the new ones.
func TestARunKeepsAMachinesOwnTimingsFromItsStart(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.optIn("pc-a", "pc-b")
	a.ehConfirmed("pc-a", map[string]any{"wake_update": map[string]any{"off_after_s": 360, "shutdown_delay_s": 30}})
	silent7(f)
	page := e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
	pa, pb := wuHostRow(t, page, "pc-a"), wuHostRow(t, page, "pc-b")
	if pa["power"] != "off" || pa["choice"] != "wake" || pa["off_after_s"] != 360.0 || pa["shutdown_delay_s"] != 30.0 {
		t.Fatalf("pc-a (its own 360 s and 30 s, from Edit host): %v", pa)
	}
	if pb["power"] != "unclear" || pb["off_after_s"] != nil || pb["shutdown_delay_s"] != nil {
		t.Fatalf("pc-b (the panel's timings): %v", pb)
	}
	var delays []string
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if path == "/power" { // the agent's own answer: the countdown it was asked for (the backend's lock is held)
			last := e.b.Requests[len(e.b.Requests)-1]
			d := 0.0
			if m := farmDelay.FindStringSubmatch(last); m != nil {
				fmt.Sscan(m[1], &d)
				delays = append(delays, agent+" "+m[1])
			}
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(f.now.Add(time.Duration(d) * time.Second).Unix()), "delay_s": d, "requested_by": "x"}}, nil
		}
		return 0, nil, nil
	}
	r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}})
	if !stepUpRequired(r) {
		t.Fatalf("the start: %d %s", r.code, r.raw)
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code, string(r.raw))
	}
	own := func(run *wakerun.Run) string {
		h := hostOf(run, "pc-a")
		if h.OffAfterS == nil || h.ShutdownDelayS == nil {
			return "none"
		}
		return fmt.Sprintf("%d/%d", *h.OffAfterS, *h.ShutdownDelayS)
	}
	act, _ := e.s.WakeRuns.Snapshot()
	if act == nil || own(act) != "360/30" || act.Settings.OffAfterS != 900 || act.Settings.ShutdownDelayS != 120 {
		t.Fatalf("the run at its start: pc-a's own %s, settings %+v", own(act), act)
	}
	f.tick()
	// the change, while the run is going: to the values that would make the run do more, sooner
	_, res := a.ehConfirmed("pc-a", map[string]any{"wake_update": map[string]any{"off_after_s": 300, "shutdown_delay_s": 10}})
	if res["ok"] != true || res["message"] != "changes saved; the run that is going keeps its numbers" {
		t.Fatalf("a change while a run is going: %v", res)
	}
	if p, _ := e.auditEntries("", "host.options")[0]["params"].(map[string]any); p["run_going"] != act.ID {
		t.Fatalf("the audit entry of a change during a run does not name the run: %v", p)
	}
	if off, delay := e.s.Poller.WakeUpdateOverride("pc-a"); off != 300 || delay != 10 {
		t.Fatalf("in force for the next run: %d, %d", off, delay)
	}
	for i := 0; i < 3600; i++ {
		act, _ := e.s.WakeRuns.Snapshot()
		if act == nil {
			break
		}
		if own(act) != "360/30" {
			t.Fatalf("pc-a's own timings changed while the run was going: %s", own(act))
		}
		f.tick()
	}
	run := f.finish()
	if ha := hostOf(run, "pc-a"); ha.State != wakerun.StOff || !strings.Contains(ha.Text, "and switched off again.") || own(run) != "360/30" {
		t.Fatalf("pc-a: %s: %s (own %s)", ha.State, ha.Text, own(run))
	}
	if got := strings.Join(delays, ", "); got != "pc-a 30" {
		t.Fatalf("the countdown the run asked for: %q (its own 30 s, never the 10 s that came into force during it)", got)
	}
	// the next run: the numbers in force now
	delays = nil
	for i := 0; i < 301; i++ { // pc-a went off: five minutes of silence are enough now
		f.tick()
	}
	if pa := wuHostRow(t, e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess), "pc-a"); pa["power"] != "off" || pa["choice"] != "wake" || pa["off_after_s"] != 300.0 {
		t.Fatalf("pc-a, silent for five minutes with its own five in force: %v", pa)
	}
	r = e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code, string(r.raw))
	}
	if act, _ := e.s.WakeRuns.Snapshot(); act == nil || own(act) != "300/10" {
		t.Fatalf("the next run: pc-a's own %s", own(act))
	}
	f.finish()
	if got := strings.Join(delays, ", "); got != "pc-a 10" {
		t.Fatalf("the countdown the next run asked for: %q", got)
	}
}

// A start is confirmed for the timings its dialog showed, a machine's own two among them. The start is
// asked for (the panel keeps it), the machine's own timing is changed in "Edit host", the admin confirms the
// start: nothing is started and nothing is sent. A change about another machine, which is not in the list,
// does not stop it.
func TestAKeptStartIsRefusedWhenAMachinesOwnTimingsChanged(t *testing.T) {
	set := func(e *env, id string, run adminstate.RunEdit) {
		t.Helper()
		if _, _, err := e.s.Poller.Admin.EditHostOptions("another admin", func(o *adminstate.HostOptions) error {
			he := o.Host(id)
			he.Run = run
			o.SetHost(id, he)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		e.s.Poller.Bump()
	}
	for name, change := range map[string]adminstate.RunEdit{"its off_after_s": {OffAfterS: 300}, "its countdown": {ShutdownDelayS: 10}} {
		a := signedIn(t)
		e := a.e
		f := newFarm(t, e)
		f.optIn("pc-a")
		f.offline()
		r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}})
		if !stepUpRequired(r) {
			t.Fatalf("%s: the start: %d %s", name, r.code, r.raw)
		}
		set(e, "pc-a", change)
		before := len(f.requests())
		if cb := a.confirm(pendingID(r), false); cb.code != 303 {
			t.Fatalf("%s: the confirmation: %d", name, cb.code)
		}
		res, _ := e.do("GET", "/api/me", nil, "Cookie", a.sess).body["pending_result"].(map[string]any)
		if res["ok"] != false || res["status"] != 409.0 || res["message"] != wuTimingsChanged {
			t.Fatalf("%s: the start after the change: %v", name, res)
		}
		if act, hist := e.s.WakeRuns.Snapshot(); act != nil || len(hist) != 0 || len(f.requests()) != before {
			t.Fatalf("%s: a run was started, or something was sent: %v %v", name, act, f.requests()[before:])
		}
	}
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.optIn("pc-a")
	f.offline()
	r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}})
	set(e, "pc-b", adminstate.RunEdit{OffAfterS: 300, ShutdownDelayS: 10})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	if act, _ := e.s.WakeRuns.Snapshot(); act == nil {
		t.Fatal("a change about a machine that is not in the list stopped the start")
	}
	f.finish()
	// ... and a machine that checks passkey approvals itself: the passkey of the start comes back after its own
	// timing was changed, and nothing is started, no ticket is kept, nothing reaches the machine
	for name, change := range map[string]adminstate.RunEdit{"its off_after_s": {OffAfterS: 300}, "its countdown": {ShutdownDelayS: 20}} {
		r := newRunRig(t, "pc-a")
		asked := r.req("POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
		id, _, ok := approvalAsked(asked)
		if !ok {
			t.Fatalf("%s: the start: %d %s", name, asked.code, asked.raw)
		}
		set(r.env, "pc-a", change)
		if back := r.approve(t, id); back.code != 303 {
			t.Fatalf("%s: the way back: %d %s", name, back.code, back.raw)
		}
		if res := r.result(t); res["ok"] != false || res["message"] != wuTimingsChanged {
			t.Fatalf("%s: the start after the change: %v", name, res)
		}
		if a, hist := r.s.WakeRuns.Snapshot(); a != nil || len(hist) != 0 {
			t.Fatalf("%s: a run was started", name)
		}
		if held, _ := r.s.tickets.held(); held != "" {
			t.Fatalf("%s: an approval is kept for a run that was not started", name)
		}
		if n := len(r.m["pc-a"].Passed()); n != 0 || len(r.sent("nas", "/wol")) != 0 {
			t.Fatalf("%s: %d requests got through to the machine, %d wakes went out", name, n, len(r.sent("nas", "/wol")))
		}
	}
}

// A machine that checks passkey approvals itself takes a shutdown countdown of at most ten minutes in a run.
// A longer one of its own, set in "Edit host", is refused where config.yaml's is: at the start, before
// anybody is asked to confirm a run that could not do it.
func TestAnOwnCountdownFromEditHostGoesThroughTheChecksOfAStart(t *testing.T) {
	r := newRunRig(t, "pc-a")
	if _, _, err := r.s.Poller.Admin.EditHostOptions("admin", func(o *adminstate.HostOptions) error {
		o.SetHost("pc-a", adminstate.HostEdit{Run: adminstate.RunEdit{ShutdownDelayS: 660}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.s.Poller.Bump()
	x := r.req("POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if _, _, asked := approvalAsked(x); asked || x.code != 400 || !strings.Contains(string(x.raw), "takes a shutdown countdown of at most 10 minutes; its countdown is 11 minutes") {
		t.Fatalf("the start: %d %s", x.code, x.raw)
	}
	if a, _ := r.s.WakeRuns.Snapshot(); a != nil {
		t.Fatal("a run was started")
	}
	// ten minutes are taken: the start goes on to its confirmation
	if _, _, err := r.s.Poller.Admin.EditHostOptions("admin", func(o *adminstate.HostOptions) error {
		o.SetHost("pc-a", adminstate.HostEdit{Run: adminstate.RunEdit{ShutdownDelayS: 600}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.s.Poller.Bump()
	if x := r.req("POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}}); x.code == 400 {
		t.Fatalf("a countdown of ten minutes: %d %s", x.code, x.raw)
	}
}
