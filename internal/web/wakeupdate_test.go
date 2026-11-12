package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/updates"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// "Update unpowered hosts" through the real routes, the real poller and powerctl, against the test
// backend. The run's own logic is tested in internal/wakerun; here: who may start one, what "Confirm it's you"
// is bound to, what reaches the agents, the audit log and the notifications, and the restart.

// farm is three machines behind the test backend: "nas" (always on, relays the wake packets), "pc-a" and
// "pc-b" (off, with Wake-on-LAN through nas, power control and self-update). The test's "laptop" is on and
// has self-update but no Wake-on-LAN. One second of the farm: the machines react to the requests the panel
// sent, the poller polls, the run takes a step. Poller and run share the farm's clock.
type farm struct {
	e       *env
	now     time.Time
	on      map[string]bool
	booted  map[string]time.Time
	ready   map[string]time.Time // the agent answers from then on
	version map[string]string
	upd     map[string]time.Time // an update finishes then
	lastAt  map[string]float64
	pending map[string]time.Time // a shutdown is due then
	wakes   bool                 // a wake packet starts a machine
	seen    int                  // requests of the backend already handled
	// poll: how the poller polls in a second of the farm (nil: every job once, PollOnce). A test that needs the
	// poller's own schedule (what is due, and what was asked for with Kick) runs the scheduler here.
	poll func()
}

const farmNew = "17-bbbbbbb"

func newFarm(t *testing.T, e *env) *farm {
	t.Helper()
	f := &farm{e: e, now: time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC), on: map[string]bool{}, booted: map[string]time.Time{}, ready: map[string]time.Time{},
		version: map[string]string{}, upd: map[string]time.Time{}, lastAt: map[string]float64{}, pending: map[string]time.Time{}, wakes: true}
	p := e.s.Poller
	p.Clock = func() time.Time { return f.now }
	e.s.WakeRuns.Now = p.Clock
	began := f.now
	e.s.WakeRuns.Mono = func() time.Duration { return f.now.Sub(began) } // the run's steady clock goes with the farm's
	add := func(a config.Agent, o *config.SystemOverride) {
		a.Managed, a.Token, a.ControlToken, a.URL = true, "read-word", "control-word", "http://"+a.ID
		if err := p.AddAgent(a, o); err != nil {
			t.Fatal(err)
		}
	}
	add(config.Agent{ID: "nas", Name: "NAS", WolRelay: true}, nil)
	for i, id := range []string{"pc-a", "pc-b"} {
		add(config.Agent{ID: id, Name: strings.ToUpper(id), PowerControl: true, SelfUpdate: true},
			&config.SystemOverride{ID: id, Wol: &config.Wol{MAC: fmt.Sprintf("00:00:5e:00:53:%02x", i+1), Relay: "nas"}})
		f.version[id] = "16-aaaaaaa"
	}
	e.b.Mu.Lock()
	e.b.Metrics["nas"] = func() (jsonx.M, error) { m := testutil.AgentMetrics(); m["hostname"] = "nas"; return m, nil }
	for _, id := range []string{"pc-a", "pc-b"} {
		e.b.Metrics[id] = func() (jsonx.M, error) { return f.metrics(id) }
	}
	e.b.Mu.Unlock()
	f.seen = len(e.b.Requests)
	p.PollOnce(context.Background())
	return f
}

// metrics is what a farm machine's agent answers (the backend's lock is held).
func (f *farm) metrics(id string) (jsonx.M, error) {
	if !f.on[id] || f.now.Before(f.ready[id]) {
		return nil, devices.Errf("connection failed")
	}
	m := testutil.AgentMetrics()
	m["hostname"], m["uptime_s"], m["agent_version"] = id, f.now.Sub(f.booted[id]).Seconds(), f.version[id]
	m["features"].(jsonx.M)["self_update"] = true
	su := jsonx.M{"state": "idle", "since": 0.0}
	if at, busy := f.upd[id]; busy && f.now.Before(at) {
		su["state"], su["message"] = "downloading", "downloading "+farmNew
	}
	if at := f.lastAt[id]; at > 0 {
		su["last"] = jsonx.M{"ok": true, "from": "16-aaaaaaa", "to": farmNew, "message": "updated", "at": at}
	}
	m["self_update"] = su
	pc := jsonx.M{"enabled": true, "pending": nil, "min_delay_s": 10.0}
	if at, ok := f.pending[id]; ok {
		pc["pending"] = jsonx.M{"action": "shutdown", "at": float64(at.Unix()), "delay_s": 120.0, "requested_by": "run"}
	}
	m["power_control"] = pc
	return m, nil
}

var (
	farmMAC   = regexp.MustCompile(`mac:00:00:5e:00:53:0(\d)`)
	farmDelay = regexp.MustCompile(`delay_s:(\d+)`)
)

// tick is one second of the farm.
func (f *farm) tick() {
	f.now = f.now.Add(time.Second)
	f.e.b.Mu.Lock()
	reqs := f.e.b.Requests[f.seen:]
	f.seen = len(f.e.b.Requests)
	for _, r := range reqs {
		id := strings.Fields(r)[0]
		switch {
		case strings.Contains(r, "POST /wol"):
			if m := farmMAC.FindStringSubmatch(r); m != nil && f.wakes {
				pc := map[string]string{"1": "pc-a", "2": "pc-b"}[m[1]]
				if !f.on[pc] {
					f.on[pc], f.booted[pc], f.ready[pc] = true, f.now.Add(5*time.Second), f.now.Add(20*time.Second)
				}
			}
		case strings.Contains(r, "POST /update"):
			f.upd[id] = f.now.Add(10 * time.Second)
		case strings.Contains(r, "POST /power") && strings.Contains(r, "action:shutdown"):
			d := 60
			if m := farmDelay.FindStringSubmatch(r); m != nil {
				fmt.Sscan(m[1], &d)
			}
			f.pending[id] = f.now.Add(time.Duration(d) * time.Second)
		case strings.Contains(r, "POST /power") && strings.Contains(r, "action:cancel"):
			delete(f.pending, id)
		}
	}
	for id, at := range f.upd {
		if !f.now.Before(at) && f.version[id] != farmNew {
			f.version[id], f.ready[id], f.lastAt[id] = farmNew, f.now.Add(3*time.Second), float64(f.now.Unix())
		}
	}
	for id, at := range f.pending {
		if !f.now.Before(at) {
			f.on[id] = false
			delete(f.pending, id)
		}
	}
	f.e.b.Mu.Unlock()
	if f.poll != nil {
		f.poll()
	} else {
		f.e.s.Poller.PollOnce(context.Background())
	}
	f.e.s.WakeRuns.Tick(context.Background())
}

// offline lets a good quarter of an hour pass: the poller shows the machines that are off as "offline" after a
// minute, and after wakerun.MinOff without an answer they are off for a run too.
func (f *farm) offline() {
	for i := 0; i < 90; i++ {
		f.tick()
	}
	f.wait(wakerun.MinOff)
}

// wait lets time pass in which nothing happens but a poll every ten seconds: what the poll loop does with a
// machine that does not answer (it is asked every second and put off by up to ten). A pause longer than the
// poll interval allows would be "the panel was not watching", and the 15 minutes would start again.
func (f *farm) wait(d time.Duration) {
	for end := f.now.Add(d); f.now.Before(end); {
		f.now = f.now.Add(10 * time.Second)
		f.e.s.Poller.PollOnce(context.Background())
	}
}

// finish lets the farm run until no run is going, and returns the last one.
func (f *farm) finish() *wakerun.Run {
	f.e.t.Helper()
	for i := 0; i < 3600; i++ {
		if a, _ := f.e.s.WakeRuns.Snapshot(); a == nil {
			_, hist := f.e.s.WakeRuns.Snapshot()
			if len(hist) == 0 {
				f.e.t.Fatal("no run")
			}
			return hist[0]
		}
		f.tick()
	}
	f.e.t.Fatal("the run did not end in an hour")
	return nil
}

func (f *farm) optIn(ids ...string) {
	for _, id := range ids {
		if err := f.e.s.Poller.Admin.SetWakeUpdate(id, true); err != nil {
			f.e.t.Fatal(err)
		}
	}
}

func (f *farm) requests() []string {
	f.e.b.Mu.Lock()
	defer f.e.b.Mu.Unlock()
	var out []string
	for _, r := range f.e.b.Requests {
		for _, path := range []string{"/wol", "/update", "/power"} {
			if strings.Contains(r, "POST "+path+" ") {
				out = append(out, strings.Fields(r)[0]+" "+path)
			}
		}
	}
	return out
}

func wuHostRow(t *testing.T, r resp, id string) map[string]any {
	t.Helper()
	hosts, _ := r.body["hosts"].([]any)
	for _, h := range hosts {
		if m := h.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("%s is not on the page: %s", id, r.raw)
	return nil
}

func hostOf(r *wakerun.Run, id string) *wakerun.Host {
	for _, h := range r.Hosts {
		if h.ID == id {
			return h
		}
	}
	return &wakerun.Host{}
}

var wuWrite = []string{"X-Requested-With", "fairy", "Content-Type", "application/json"}

func (e *env) wuStart(cookie string, body any, hdr ...string) resp {
	return e.do("POST", "/api/admin/wake-update", body, append(append([]string{"Cookie", cookie}, wuWrite...), hdr...)...)
}

// The page: which machines can take part, how each could be in a run right now, and why not. Switched off for
// every machine until an admin switches it on.
func TestWakeUpdatePageListsWhatCanTakePart(t *testing.T) {
	e := setup(t)
	f := newFarm(t, e)
	f.offline()
	admin := e.cookie(controller(), 0)
	get := func() resp {
		r := e.do("GET", "/api/admin/wake-update", nil, "Cookie", admin)
		if r.code != 200 {
			t.Fatal(r.code, string(r.raw))
		}
		return r
	}
	r := get()
	for _, id := range []string{"pc-a", "pc-b", "laptop", "nas", "tower"} {
		if h := wuHostRow(t, r, id); h["opted_in"] != false || h["choice"] != "" {
			t.Errorf("%s: nothing is switched on for it by default: %v", id, h)
		}
	}
	if h := wuHostRow(t, r, "pc-a"); h["can_join"] != true || h["can_wake"] != true || h["relay"] != "NAS" || h["power"] != "off" ||
		!strings.Contains(h["why"].(string), "not switched on for this") {
		t.Fatalf("pc-a: %v", h)
	}
	if h := wuHostRow(t, r, "tower"); h["can_join"] != false || !strings.Contains(h["why_not"].(string), "cannot be updated from the panel") {
		t.Fatalf("tower has no self_update: %v", h)
	}
	if h := wuHostRow(t, r, "laptop"); h["can_join"] != true || h["can_wake"] != false || h["no_wake"] != "no Wake-on-LAN is set up for it" || h["power"] != "on" {
		t.Fatalf("laptop: %v", h)
	}
	if r.body["active"] != nil || len(r.body["last"].([]any)) != 0 || r.body["max_hosts"] != 16.0 || r.body["problem"] != nil {
		t.Fatalf("no run yet: %s", r.raw)
	}
	if s := r.body["settings"].(map[string]any); s["online_timeout_s"] != 300.0 || s["settle_s"] != 60.0 || s["shutdown_delay_s"] != 120.0 {
		t.Fatalf("settings: %v", s)
	}
	f.optIn("pc-a", "pc-b", "laptop")
	r = get()
	if wuHostRow(t, r, "pc-a")["choice"] != "wake" || wuHostRow(t, r, "laptop")["choice"] != "update_only" {
		t.Fatalf("after switching them on: %s", r.raw)
	}
	// the relay is gone: the machine cannot be woken now, and the page says which machine is missing
	e.b.Mu.Lock()
	e.b.Metrics["nas"] = func() (jsonx.M, error) { return nil, devices.Errf("connection failed") }
	e.b.Mu.Unlock()
	f.offline()
	if h := wuHostRow(t, get(), "pc-a"); h["choice"] != "" || h["why"] != "it is off, and NAS, which sends its wake packets, is offline" {
		t.Fatalf("relay offline: %v", h)
	}
	// a machine that stopped answering a moment ago is neither on nor off
	e.b.Mu.Lock()
	e.b.Metrics["laptop"] = func() (jsonx.M, error) { return nil, devices.Errf("connection failed") }
	e.b.Mu.Unlock()
	f.tick()
	if h := wuHostRow(t, get(), "laptop"); h["power"] != "unclear" || h["choice"] != "" ||
		h["why"] != "not sure that it is off: no answer for 1 second so far, and Fairy takes a machine for off only after 15 minutes (it may be restarting)" {
		t.Fatalf("stale: %v", h)
	}
	// shown as offline after a minute, and still neither on nor off: it may be restarting
	f.wait(5 * time.Minute)
	_, laptop, _ := e.s.Poller.AgentSeen("laptop")
	if h := wuHostRow(t, get(), "laptop"); h["power"] != "unclear" || h["choice"] != "" || laptop.Status != "offline" ||
		h["why"] != "not sure that it is off: no answer for 5 minutes so far, and Fairy takes a machine for off only after 15 minutes (it may be restarting)" {
		t.Fatalf("offline for five minutes: %v (%s)", h, laptop.Status)
	}
	// after a quarter of an hour without an answer it is off
	f.wait(10 * time.Minute)
	if h := wuHostRow(t, get(), "laptop"); h["power"] != "off" || h["choice"] != "" || h["why"] != "it is off, and Fairy cannot wake it: no Wake-on-LAN is set up for it" {
		t.Fatalf("offline for a quarter of an hour: %v", h)
	}
	if got := get().body["off_after_s"]; got != 900.0 {
		t.Fatalf("off_after_s: %v", got)
	}
	// who may read the page
	if r := e.do("GET", "/api/admin/wake-update", nil, "Cookie", e.cookie(viewer, 0)); r.code != 403 {
		t.Fatal("a viewer:", r.code)
	}
	if r := e.do("GET", "/api/admin/wake-update", nil); r.code != 401 {
		t.Fatal("nobody:", r.code)
	}
}

// The per-host switch: off by default, changed by an admin in a browser session behind "Confirm it's you",
// written to the audit log, and refused for a machine that cannot take part.
func TestWakeUpdateSwitch(t *testing.T) {
	a := signedIn(t)
	e := a.e
	newFarm(t, e).offline()
	put := func(id string, on any, hdr ...string) resp {
		return e.do("PUT", "/api/systems/"+id+"/wake-update", map[string]any{"enabled": on}, append(append([]string{"Cookie", a.sess}, wuWrite...), hdr...)...)
	}
	r := put("pc-a", true)
	d, _ := r.body["detail"].(map[string]any)
	if !stepUpRequired(r) || d["action"] != `"Update unpowered hosts" for pc-a (switch it on: a run you start may then wake, update and switch off this machine)` {
		t.Fatalf("an old sign-in: %d %s", r.code, r.raw)
	}
	if e.s.Poller.Admin.WakeUpdateEnabled("pc-a") {
		t.Fatal("switched on without the confirmation")
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 || !e.s.Poller.Admin.WakeUpdateEnabled("pc-a") {
		t.Fatal("after the confirmation:", cb.code)
	}
	// inside the window now: the next ones run at once
	if r := put("laptop", true); r.code != 200 || !strings.Contains(r.body["message"].(string), "only to be updated while it is on: no Wake-on-LAN is set up for it") {
		t.Fatalf("laptop: %d %s", r.code, r.raw)
	}
	if r := put("tower", true); r.code != 409 || e.s.Poller.Admin.WakeUpdateEnabled("tower") {
		t.Fatalf("tower cannot take part: %d %s", r.code, r.raw)
	}
	if r := put("nobody", true); r.code != 404 {
		t.Fatal("unknown machine:", r.code)
	}
	if r := put("pc-a", "yes"); r.code != 422 {
		t.Fatal("enabled must be a boolean:", r.code)
	}
	if r := e.do("PUT", "/api/systems/pc-a/wake-update", map[string]any{}, append([]string{"Cookie", a.sess}, wuWrite...)...); r.code != 422 {
		t.Fatal("enabled left out:", r.code)
	}
	if r := put("pc-a", false); r.code != 200 || e.s.Poller.Admin.WakeUpdateEnabled("pc-a") {
		t.Fatalf("switching off: %d %s", r.code, r.raw)
	}
	got := map[string]string{}
	for _, en := range auditOf(e, "wakeupdate.switch") {
		got[fmt.Sprint(en["target"], " ", en["params"].(map[string]any)["enabled"])] = fmt.Sprint(en["outcome"])
	}
	if got["pc-a true"] != "ok" || got["pc-a false"] != "ok" || got["laptop true"] != "ok" || !strings.HasPrefix(got["tower true"], "error 409") {
		t.Fatalf("audit: %v", got)
	}
	// not for a token, a viewer, or a request without the page's header or from another origin
	tok, _, _ := e.s.Tokens.Create("ops", "u2", "control", "cli:test", 0)
	if r := e.do("PUT", "/api/systems/pc-b/wake-update", map[string]any{"enabled": true}, "Authorization", "Bearer "+tok); r.code != 403 {
		t.Fatal("a control token:", r.code, string(r.raw))
	}
	pat := model.User{Sub: "u9", Name: model.P("Pat"), Groups: []string{"viewers"}} // not an admin
	if r := e.do("PUT", "/api/systems/pc-b/wake-update", map[string]any{"enabled": true}, append([]string{"Cookie", e.cookie(pat, 0)}, wuWrite...)...); r.code != 403 {
		t.Fatal("not an admin:", r.code)
	}
	if r := e.do("PUT", "/api/systems/pc-b/wake-update", map[string]any{"enabled": true}, "Cookie", a.sess, "Content-Type", "application/json"); r.code != 403 {
		t.Fatal("without the CSRF header:", r.code)
	}
	if r := put("pc-b", true, "Origin", "https://evil.example"); r.code != 403 || e.s.Poller.Admin.WakeUpdateEnabled("pc-b") {
		t.Fatal("from another origin:", r.code)
	}
}

// Who may start a run: an admin, in a browser session, from the panel's own page. Not a viewer, not an API
// token of any scope (also not an admin's), not without the CSRF header, not from another origin, and not
// while control is disabled. A refused start keeps nothing and starts nothing.
func TestWakeUpdateStartIsForAdminsInABrowser(t *testing.T) {
	e := setup(t)
	f := newFarm(t, e)
	f.offline()
	f.optIn("pc-a", "pc-b")
	admin := e.cookie(controller(), 0)
	body := map[string]any{"hosts": []string{"pc-a"}}
	nothing := func(what string) {
		t.Helper()
		if a, hist := e.s.WakeRuns.Snapshot(); a != nil || len(hist) != 0 || len(f.requests()) != 0 {
			t.Fatalf("%s: a run exists, or something was sent: %v", what, f.requests())
		}
	}
	if r := e.do("POST", "/api/admin/wake-update", body); r.code != 401 {
		t.Fatal("not signed in:", r.code)
	}
	if r := e.wuStart(e.cookie(viewer, 0), body); r.code != 403 {
		t.Fatal("a viewer:", r.code)
	}
	for _, scope := range []string{"read", "control"} {
		tok, _, _ := e.s.Tokens.Create("t-"+scope, "u2", scope, "cli:test", 0) // owned by the admin
		r := e.do("POST", "/api/admin/wake-update", body, "Authorization", "Bearer "+tok, "Content-Type", "application/json")
		if r.code != 403 || !strings.Contains(string(r.raw), "API tokens cannot confirm") {
			t.Fatalf("a %s token: %d %s", scope, r.code, r.raw)
		}
	}
	if r := e.do("POST", "/api/admin/wake-update", body, "Cookie", admin, "Content-Type", "application/json"); r.code != 403 || !strings.Contains(string(r.raw), "X-Requested-With") {
		t.Fatalf("without the CSRF header: %d %s", r.code, r.raw)
	}
	if r := e.wuStart(admin, body, "Origin", "https://evil.example"); r.code != 403 || !strings.Contains(string(r.raw), "cross-origin") {
		t.Fatalf("from another origin: %d %s", r.code, r.raw)
	}
	if r := e.wuStart(admin, body, "Sec-Fetch-Site", "cross-site"); r.code != 403 {
		t.Fatal("cross-site:", r.code)
	}
	nothing("refused requests")
	if keptID(e, admin) != "" {
		t.Fatal("a refused request was kept")
	}
	// what the list may hold: checked before anybody is asked for a passkey
	for name, c := range map[string]struct {
		body any
		part string
	}{
		"nothing":               {map[string]any{}, "no machine was chosen"},
		"unknown":               {map[string]any{"hosts": []string{"pc-a", "nobody"}}, "names a machine twice, or one the panel does not know"},
		"twice":                 {map[string]any{"hosts": []string{"pc-a", "pc-a"}}, "names a machine twice"},
		"in both lists":         {map[string]any{"hosts": []string{"pc-a"}, "update_only": []string{"pc-a"}}, "names a machine twice"},
		"not switched on":       {map[string]any{"update_only": []string{"laptop"}}, "LAPTOP is not switched on"},
		"cannot take part":      {map[string]any{"update_only": []string{"tower"}}, "cannot take part"},
		"a guest":               {map[string]any{"hosts": []string{"qemu-100"}}, "one the panel does not know"},
		"more than a run takes": {map[string]any{"hosts": strings.Fields(strings.Repeat("pc-a ", 17))}, "at most 16 machines"},
	} {
		r := e.wuStart(admin, c.body)
		if r.code != 400 || !strings.Contains(strings.ToLower(fmt.Sprint(r.body["detail"])), strings.ToLower(c.part)) {
			t.Errorf("%s: %d %s", name, r.code, r.raw)
		}
	}
	f.optIn("laptop")
	if r := e.wuStart(admin, map[string]any{"hosts": []string{"laptop"}}); r.code != 400 || !strings.Contains(string(r.raw), "cannot be woken and switched off again: no Wake-on-LAN is set up for it") {
		t.Fatalf("a machine without Wake-on-LAN in the wake list: %d %s", r.code, r.raw)
	}
	for name, raw := range map[string]string{
		"an unknown field":    `{"hosts":["pc-a"],"shutdown_all":true}`,
		"a field twice":       `{"hosts":["pc-a"],"hosts":["pc-b"]}`,
		"a field in capitals": `{"hosts":["pc-a"],"HOSTS":["pc-b"]}`,
		"not a list":          `{"hosts":"pc-a"}`,
	} {
		req := httptest.NewRequest("POST", e.srv.URL+"/api/admin/wake-update", strings.NewReader(raw))
		req.RequestURI = ""
		for i := 0; i+1 < len(wuWrite); i += 2 {
			req.Header.Set(wuWrite[i], wuWrite[i+1])
		}
		req.Header.Set("Cookie", admin)
		res, err := e.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 422 {
			t.Errorf("%s: %d", name, res.StatusCode)
		}
	}
	nothing("bad lists")
	// the kill switch
	if err := e.s.Poller.Admin.SetControlDisabled(true, "test", "kill switch"); err != nil {
		t.Fatal(err)
	}
	if r := e.wuStart(admin, body); r.code != 423 {
		t.Fatal("control disabled:", r.code)
	}
	nothing("kill switch")
}

// Starting a run always asks "Confirm it's you", also for an admin who confirmed something else a second ago.
// The dialog gets the panel's own description: the machines by the panel's names, what happens to each, and
// the value that names the list. What runs after the sign-in is the kept request: that list, those options.
func TestWakeUpdateStartAlwaysAsksAndIsBoundToItsList(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.offline()
	f.optIn("pc-a", "pc-b", "laptop")
	if r := a.stepUp("&next=/", false); r.code != 303 { // the admin has just confirmed: inside the 3-minute window
		t.Fatal(r.code)
	}
	if r := e.poll(a.sess); r.code != 200 {
		t.Fatal("another control action runs inside the window:", r.code)
	}
	bodyA := map[string]any{"hosts": []string{"pc-a"}, "update_only": []string{"laptop"}, "update_if_on": true}
	r := e.wuStart(a.sess, bodyA, "X-Fairy-Action", "Update%20everything%20everywhere")
	d, _ := r.body["detail"].(map[string]any)
	hashA := wakerun.ListHash([]string{"pc-a"}, []string{"laptop"}, true)
	wantAction := "Update unpowered hosts (2 machines, list " + hashA + ": wake, update and switch off again: PC-A; update only: laptop)"
	if !stepUpRequired(r) || d["per_action"] != true || d["pending"] != true || d["action"] != wantAction {
		t.Fatalf("inside the window it must still ask, with the panel's description: %d %s", r.code, r.raw)
	}
	items := fmt.Sprint(d["items"])
	if items != "[PC-A: wake, update, switch off again (only updated if it turns out to be on) laptop: update only; it is on and is never switched off]" {
		t.Fatalf("the dialog's lines: %s", items)
	}
	if a, _ := e.s.WakeRuns.Snapshot(); a != nil || len(f.requests()) != 0 {
		t.Fatal("something started before the confirmation")
	}
	idA := pendingID(r)

	// "Continue" with more than the kept action's id is refused and forgets the action: the list cannot be
	// changed on the way to the sign-in
	if x := e.do("POST", stepUpPath, map[string]any{"pending_id": idA, "next": "/", "hosts": []string{"pc-b"}}, append([]string{"Cookie", a.sess}, wuWrite...)...); x.code != 422 {
		t.Fatal("extra fields in the confirmation:", x.code)
	}
	if x := a.confirm(idA, false); x.code != 409 {
		t.Fatal("the action must be forgotten after that:", x.code)
	}
	// asked again: kept again, under a new id
	r = e.wuStart(a.sess, bodyA)
	if idA2 := pendingID(r); idA2 == "" || idA2 == idA {
		t.Fatal("not kept again")
	} else {
		idA = idA2
	}
	// a second request, for another list, replaces the kept one: only one action waits per session, and the
	// confirmation of the first no longer exists
	bodyB := map[string]any{"hosts": []string{"pc-a", "pc-b"}}
	rb := e.wuStart(a.sess, bodyB)
	idB := pendingID(rb)
	if !stepUpRequired(rb) || idB == "" || idB == idA {
		t.Fatalf("list B: %d %s", rb.code, rb.raw)
	}
	if x := a.confirm(idA, false); x.code != 409 {
		t.Fatal("list A's id must no longer confirm anything:", x.code)
	}
	if a, _ := e.s.WakeRuns.Snapshot(); a != nil {
		t.Fatal("a run started")
	}
	// the admin confirms list A after all
	r = e.wuStart(a.sess, bodyA)
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal("confirmation:", cb.code, string(cb.raw))
	}
	run, _ := e.s.WakeRuns.Snapshot()
	if run == nil || run.ListHash != hashA || !run.UpdateIfOn || len(run.Hosts) != 2 || hostOf(run, "pc-a").Mode != wakerun.ModeWake ||
		hostOf(run, "laptop").Mode != wakerun.ModeUpdateOnly || hostOf(run, "pc-b").ID != "" {
		t.Fatalf("the run that started is not list A: %+v", run)
	}
	me := e.do("GET", "/api/me", nil, "Cookie", a.sess)
	pr, _ := me.body["pending_result"].(map[string]any)
	if pr["ok"] != true || pr["message"] != "run started for 2 machines" || pr["ref"] != run.ID {
		t.Fatalf("the result for the page: %v", pr)
	}
	// the confirmation was for that one request: the same list again, seconds later, is asked again. (Here a
	// run is going, which is said before anybody is asked.)
	if x := e.wuStart(a.sess, bodyA); x.code != 409 || !strings.Contains(fmt.Sprint(x.body["detail"]), "is still going: one run at a time") {
		t.Fatalf("a second start while one runs: %d %s", x.code, x.raw)
	}
	if keptID(e, a.sess) != "" {
		t.Fatal("the refused second start was kept")
	}
	// audit: the start names the list, and the step-up entries name what was confirmed
	st := auditOf(e, "wakeupdate.start")
	if len(st) != 1 || st[0]["target"] != run.ID || st[0]["params"].(map[string]any)["list"] != hashA || !strings.Contains(st[0]["outcome"].(string), "list "+hashA) {
		t.Fatalf("audit of the start: %v", st)
	}
	found := false
	for _, en := range auditOf(e, "auth.step_up") {
		if p, _ := en["params"].(map[string]any); p != nil && p["runs"] == wantAction {
			found = true
		}
	}
	if !found {
		t.Fatal("no step-up entry names the confirmed list")
	}
}

// Between "Confirm it's you" and the sign-in coming back, the list is not trusted either: the kept request is
// checked again when it runs. A machine that was switched off for these runs meanwhile stops the whole start.
func TestWakeUpdateKeptStartIsCheckedAgainWhenItRuns(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.offline()
	f.optIn("pc-a", "pc-b")
	r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a", "pc-b"}})
	if !stepUpRequired(r) {
		t.Fatal(r.code, string(r.raw))
	}
	if err := e.s.Poller.Admin.SetWakeUpdate("pc-b", false); err != nil {
		t.Fatal(err)
	}
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	pr, _ := e.do("GET", "/api/me", nil, "Cookie", a.sess).body["pending_result"].(map[string]any)
	if pr["ok"] != false || !strings.Contains(fmt.Sprint(pr["message"]), "PC-B is not switched on") {
		t.Fatalf("the result: %v", pr)
	}
	if run, hist := e.s.WakeRuns.Snapshot(); run != nil || len(hist) != 0 || len(f.requests()) != 0 {
		t.Fatal("a run started with a machine that is no longer allowed")
	}
}

// A whole run through the real poller and powerctl: what the agents are sent (the existing three requests and
// nothing else), the audit log (who, which machines, every step), the notification with the summary, and
// that no token is in any of it. Then the cancel route.
func TestWakeUpdateRunEndToEnd(t *testing.T) {
	a := signedIn(t)
	e := a.e
	f := newFarm(t, e)
	f.offline()
	f.optIn("pc-a", "pc-b", "laptop")
	e.b.Reply = func(agent, path string) (int, jsonx.M, error) {
		if path == "/power" { // the agent's own answer: the countdown it scheduled
			return 200, jsonx.M{"action": "shutdown", "pending": jsonx.M{"action": "shutdown", "at": float64(f.now.Add(120 * time.Second).Unix()), "delay_s": 120.0, "requested_by": "x"}}, nil
		}
		return 0, nil, nil
	}
	r := e.wuStart(a.sess, map[string]any{"hosts": []string{"pc-a"}, "update_only": []string{"laptop"}})
	if cb := a.confirm(pendingID(r), false); cb.code != 303 {
		t.Fatal(cb.code)
	}
	page := e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
	act, _ := page.body["active"].(map[string]any)
	if act == nil || act["state"] != "running" || len(act["hosts"].([]any)) != 2 {
		t.Fatalf("the page while it runs: %s", page.raw)
	}
	run := f.finish()
	pc, lap := hostOf(run, "pc-a"), hostOf(run, "laptop")
	if run.State != wakerun.RunDone || pc.State != wakerun.StOff || !strings.Contains(pc.Text, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.") {
		t.Fatalf("pc-a: %s: %s", pc.State, pc.Text)
	}
	// laptop's agent does not report self-update: it is not asked, it stays on, and the report says why
	if lap.State != wakerun.StStayedOn || lap.Text != "It was on and stays on. Its update did not work: self-update is not enabled on its agent." {
		t.Fatalf("laptop: %s: %s", lap.State, lap.Text)
	}
	if got := strings.Join(f.requests(), ", "); got != "nas /wol, pc-a /update, pc-a /power" {
		t.Fatalf("what the agents were sent: %s", got)
	}
	e.b.Mu.Lock()
	all := strings.Join(e.b.Requests, "\n")
	e.b.Mu.Unlock()
	for _, want := range []string{"nas POST /wol map[broadcast:<nil> mac:00:00:5e:00:53:01 port:9]", "pc-a POST /update map[requested_by:", "pc-a POST /power map[action:shutdown delay_s:120 requested_by:"} {
		if !strings.Contains(all, want) {
			t.Errorf("the agents were not sent %q:\n%s", want, all)
		}
	}
	// the audit log
	who := actorOf(&model.User{Sub: run.BySub, Name: model.P(run.By)})
	var lines []string
	for _, action := range []string{"wakeupdate.start", "power.wake", "update.agent", "power.shutdown", "wakeupdate.host", "wakeupdate.end"} {
		for _, en := range auditOf(e, action) {
			p, _ := en["params"].(map[string]any)
			if en["action"] != action || p["run"] != run.ID || en["actor"] != who {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s %s %s", action, en["target"], en["source"]))
			if action != "wakeupdate.start" && en["source"] != "wake-update run" {
				t.Errorf("%s: source %v", action, en["source"])
			}
		}
	}
	for _, want := range []string{"wakeupdate.start " + run.ID, "power.wake pc-a wake-update run", "update.agent pc-a wake-update run",
		"power.shutdown pc-a wake-update run", "wakeupdate.host pc-a wake-update run", "wakeupdate.host laptop wake-update run", "wakeupdate.end " + run.ID + " wake-update run"} {
		if !strings.Contains(strings.Join(lines, "\n"), want) {
			t.Errorf("the audit log lacks %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	// the notification: one for the finished run, with a line per machine
	found := ""
	for _, n := range e.s.Notes.List("u2", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events {
		if n.Kind == "wakeupdate_done" {
			found = n.Text
		}
	}
	if !strings.Contains(found, "PC-A: Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.") || !strings.Contains(found, "laptop: It was on and stays on.") {
		t.Fatalf("the notification: %q", found)
	}
	// the page after the run, and after a reload of the panel's memory: the report stays
	page = e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
	last, _ := page.body["last"].([]any)
	if page.body["active"] != nil || len(last) != 1 || last[0].(map[string]any)["id"] != run.ID || last[0].(map[string]any)["summary"] == "" {
		t.Fatalf("the page after the run: %s", page.raw)
	}
	// nothing that is a secret is in the journal, the audit log or the page
	journal, _ := os.ReadFile(filepath.Join(e.s.Cfg.DataDir, "wake-update.json"))
	auditLog, _ := os.ReadFile(e.s.Cfg.AuditLog)
	for where, text := range map[string]string{"the journal": string(journal), "the audit log": string(auditLog), "the page": string(page.raw)} {
		noneOf(t, where, text, "read-word", "control-word", testutil.ReadToken, testutil.ControlToken, cookieValue(a.sess), sidOf(e, a.sess))
	}
	if st, err := os.Stat(filepath.Join(e.s.Cfg.DataDir, "wake-update.json")); err != nil || st.Mode().Perm() != 0o600 || !strings.Contains(string(journal), run.ID) {
		t.Fatal("the journal:", err)
	}
}

// Cancelling: an admin in a browser, behind the usual step-up, also while control is disabled. The default
// leaves the woken machines on; the body can ask for them to be switched off again.
func TestWakeUpdateCancelRoute(t *testing.T) {
	e := setup(t)
	f := newFarm(t, e)
	f.wakes = false // the machines never come online: the run waits
	f.offline()
	f.optIn("pc-a", "pc-b")
	admin := e.cookie(controller(), 0)
	start := func() *wakerun.Run {
		t.Helper()
		r, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a", "pc-b"}, By: "Alice", BySub: "u2"})
		if err != nil {
			t.Fatal(err)
		}
		f.tick()
		f.tick()
		return r
	}
	del := func(id, cookie string, body any, hdr ...string) resp {
		return e.do("DELETE", "/api/admin/wake-update/"+id, body, append(append([]string{"Cookie", cookie}, wuWrite...), hdr...)...)
	}
	run := start()
	if r := del("0000000000000000", admin, nil); r.code != 404 {
		t.Fatal("another id:", r.code)
	}
	tok, _, _ := e.s.Tokens.Create("ops", "u2", "control", "cli:test", 0)
	if r := e.do("DELETE", "/api/admin/wake-update/"+run.ID, nil, "Authorization", "Bearer "+tok); r.code != 403 {
		t.Fatal("a control token:", r.code)
	}
	if r := del(run.ID, e.cookie(viewer, 0), nil); r.code != 403 {
		t.Fatal("a viewer:", r.code)
	}
	if r := e.do("DELETE", "/api/admin/wake-update/"+run.ID, nil, "Cookie", admin); r.code != 403 {
		t.Fatal("without the CSRF header:", r.code)
	}
	if r := del(run.ID, admin, nil, "Origin", "https://evil.example"); r.code != 403 {
		t.Fatal("from another origin:", r.code)
	}
	stale := e.staleCookie(controller(), time.Hour)
	r := del(run.ID, stale, map[string]any{"switch_off": true})
	d, _ := r.body["detail"].(map[string]any)
	if !stepUpRequired(r) || d["action"] != `cancel the run of "Update unpowered hosts" (the machines it has woken are switched off again)` {
		t.Fatalf("an old sign-in: %d %s", r.code, r.raw)
	}
	if a, _ := e.s.WakeRuns.Snapshot(); a == nil || a.Cancel != nil {
		t.Fatal("cancelled by a refused request")
	}
	// with the kill switch on an admin can still call it off (and the run stops by itself at its next step)
	if err := e.s.Poller.Admin.SetControlDisabled(true, "test", "kill switch"); err != nil {
		t.Fatal(err)
	}
	if r := del(run.ID, admin, nil); r.code != 200 || r.body["message"] != "run cancelled: the machines it has woken stay on" {
		t.Fatalf("cancel: %d %s", r.code, r.raw)
	}
	if err := e.s.Poller.Admin.SetControlDisabled(false, "test", ""); err != nil {
		t.Fatal(err)
	}
	done := f.finish()
	if done.State != wakerun.RunCancelled || done.Cancel == nil || done.Cancel.SwitchOff || hostOf(done, "pc-a").State != wakerun.StGaveUp {
		t.Fatalf("the cancelled run: %s %+v", done.State, done.Cancel)
	}
	if r := del(run.ID, admin, nil); r.code != 404 {
		t.Fatal("a finished run cannot be cancelled:", r.code)
	}
	// the other kind
	run = start()
	if r := del(run.ID, admin, map[string]any{"switch_off": true}); r.code != 200 || r.body["message"] != "run cancelled: the machines it has woken are switched off again" {
		t.Fatalf("cancel with switch off: %d %s", r.code, r.raw)
	}
	if a, _ := e.s.WakeRuns.Snapshot(); a == nil || a.Cancel == nil || !a.Cancel.SwitchOff || a.Cancel.By == "" {
		t.Fatal("not recorded")
	}
	if r := del(run.ID, admin, map[string]any{"switch_off": "yes"}); r.code != 422 {
		t.Fatal("switch_off must be a boolean:", r.code)
	}
	en := auditOf(e, "wakeupdate.cancel")
	if len(en) != 2 || en[0]["outcome"] != "ok" {
		t.Fatalf("audit: %v", en)
	}
}

// The panel restarts while a run is going. The new panel reports it, once, in the audit log and as a warning,
// shows it on the page as interrupted with what each machine was doing, and sends nothing.
func TestWakeUpdateRestartIsReportedNotContinued(t *testing.T) {
	e := setup(t)
	f := newFarm(t, e)
	f.offline()
	f.optIn("pc-a", "pc-b")
	f.wakes = true
	run, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a", "pc-b"}, By: "Alice", BySub: "u2"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		f.tick()
		if a, _ := e.s.WakeRuns.Snapshot(); hostOf(a, "pc-a").State == wakerun.StUpdating && hostOf(a, "pc-a").UpdateSentAt != nil {
			break
		}
	}
	sent := len(f.requests())
	if sent == 0 {
		t.Fatal("the run had not done anything yet")
	}
	// the restarted panel: a new server over the same data folder and poller
	s2 := New(e.s.Cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, e.s.Poller, nil, "test")
	srv := httptest.NewServer(s2.Handler())
	defer srv.Close()
	e2 := &env{t: t, s: s2, srv: srv, b: e.b}
	act, hist := s2.WakeRuns.Snapshot()
	if act != nil || len(hist) != 1 || hist[0].ID != run.ID || hist[0].State != wakerun.RunInterrupted {
		t.Fatalf("after the restart: %v %d", act, len(hist))
	}
	if h := hostOf(hist[0], "pc-a"); h.State != wakerun.StInterrupted || !strings.Contains(h.Text, "It had come online after the run's wake and was ON (updating)") {
		t.Fatalf("pc-a: %s", h.Text)
	}
	for i := 0; i < 600; i++ { // ten minutes of the new panel's steps
		s2.WakeRuns.Tick(context.Background())
	}
	if len(f.requests()) != sent {
		t.Fatalf("the restarted panel sent requests: %v", f.requests()[sent:])
	}
	en := auditOf(e2, "wakeupdate.interrupted")
	if len(en) != 1 || en[0]["target"] != run.ID || en[0]["actor"] != "Alice" || en[0]["source"] != "wake-update run" {
		t.Fatalf("audit: %v", en)
	}
	warn := ""
	for _, n := range s2.Notes.List("u2", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events {
		if n.Kind == "wakeupdate_interrupted" {
			warn = n.Severity + ": " + n.Text
		}
	}
	if !strings.HasPrefix(warn, "warning: the panel restarted during the run Alice started at 03:16 UTC; it is not continued.") || !strings.Contains(warn, "PC-A: The panel restarted during the run. It had come online after the run's wake and was ON") {
		t.Fatalf("the warning: %q", warn)
	}
	page := e2.do("GET", "/api/admin/wake-update", nil, "Cookie", e2.cookie(controller(), 0))
	last, _ := page.body["last"].([]any)
	if page.body["active"] != nil || len(last) != 1 || last[0].(map[string]any)["state"] != "interrupted" {
		t.Fatalf("the page: %s", page.raw)
	}
	// a third start of the panel does not report it again
	s3 := New(e.s.Cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, e.s.Poller, nil, "test")
	if n := len(auditOf(&env{t: t, s: s3}, "wakeupdate.interrupted")); n != 1 {
		t.Fatalf("%d reports after another restart", n)
	}
}

// What the panel knows about the guests of a Proxmox node, for "leave a node with running guests on": the
// count from its Proxmox API when that answered since the node came online, "cannot tell" otherwise and for a
// Proxmox machine the panel only knows through its agent.
func TestWakeUpdateGuestsOfANode(t *testing.T) {
	e := setup(t)
	ops := runOps{e.s}
	long := time.Now().Add(-time.Hour)
	// the fixture: SRV-PVEA with a running VM (win11) and a stopped container
	if n, known, names := ops.Guests("node-srv-pvea", long); n != 1 || !known || names != "win11" {
		t.Fatalf("node with one running guest: %d %v %q", n, known, names)
	}
	if n, known, why := ops.Guests("node-srv-pvea", time.Now().Add(time.Hour)); known || n != 0 || !strings.Contains(why, "has not answered since it came online") {
		t.Fatalf("no answer from the Proxmox API since the node came online: %d %v %q", n, known, why)
	}
	if n, known, _ := ops.Guests("laptop", long); n != 0 || !known {
		t.Fatal("an ordinary machine has no guests")
	}
	e.b.Mu.Lock()
	e.b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["platform"] = jsonx.M{"system": "Linux", "pretty": "Proxmox VE 9.0"}
		return m, nil
	}
	e.b.Mu.Unlock()
	e.s.Poller.PollOnce(context.Background())
	if _, known, why := ops.Guests("laptop", long); known || why != "it is a Proxmox host, and the panel has no Proxmox API for it" {
		t.Fatalf("a Proxmox host known only through its agent: %v %q", known, why)
	}
	if _, known, _ := ops.Guests("nobody", long); known {
		t.Fatal("an unknown machine")
	}
}

// An agent that answers "approval required" (item 1.1) or with its rate limit is told apart from any other
// refusal; the kill switch stops a run's request like it stops a click.
func TestWakeUpdateRequestErrors(t *testing.T) {
	e := setup(t)
	ops := runOps{e.s}
	ctx := context.Background()
	code := func(err error) string {
		oe, _ := err.(*wakerun.OpError)
		if oe == nil {
			return "?"
		}
		return oe.Code + "|" + oe.Text
	}
	for _, c := range []struct {
		status int
		body   jsonx.M
		want   string
	}{
		{403, jsonx.M{"error": "approval required: confirm this action with Pocket ID", "code": "approval_required"}, "approval_required|approval required: confirm this action with Pocket ID"},
		{403, jsonx.M{"error": "approval refused (nonce)", "code": "approval_refused"}, "approval_required|approval refused (nonce)"},
		{503, jsonx.M{"error": "the trust file is broken", "code": "approval_unavailable"}, "approval_required|the trust file is broken"},
		{429, jsonx.M{"error": "too many control requests; try again in a minute"}, "rate_limited|too many control requests; try again in a minute"},
		{409, jsonx.M{"error": "a shutdown is already pending; cancel it first"}, "|a shutdown is already pending; cancel it first"},
	} {
		e.b.Reply = func(string, string) (int, jsonx.M, error) { return c.status, c.body, nil }
		if got := code(ops.Update(ctx, "laptop", "Ann", nil)); got != c.want {
			t.Errorf("update, agent %d: %q, want %q", c.status, got, c.want)
		}
		if _, err := ops.Shutdown(ctx, "laptop", 2*time.Minute, "Ann", nil); code(err) != c.want {
			t.Errorf("shutdown, agent %d: %q, want %q", c.status, code(err), c.want)
		}
	}
	e.b.Reply = nil
	if ops.Blocked() != "" {
		t.Fatal("blocked without the kill switch")
	}
	if err := e.s.Poller.Admin.SetControlDisabled(true, "test", "kill switch"); err != nil {
		t.Fatal(err)
	}
	e.b.Mu.Lock()
	n := len(e.b.Requests)
	e.b.Mu.Unlock()
	if ops.Blocked() == "" || ops.Update(ctx, "laptop", "Ann", nil) == nil || ops.Wake(ctx, "laptop", "Ann") == nil {
		t.Fatal("control disabled: every request must be refused")
	}
	if _, err := ops.Shutdown(ctx, "laptop", 2*time.Minute, "Ann", nil); err == nil || !strings.Contains(err.Error(), "control is disabled") {
		t.Fatal("control disabled: shutdown", err)
	}
	e.b.Mu.Lock()
	defer e.b.Mu.Unlock()
	if len(e.b.Requests) != n {
		t.Fatalf("with control disabled something was sent: %v", e.b.Requests[n:])
	}
}

// The panel's own update restarts the panel, and a run is never continued after a restart: while a run is
// going, "Update panel" is refused and says why.
func TestPanelUpdateWaitsForARun(t *testing.T) {
	e := setup(t)
	f := newFarm(t, e)
	f.wakes = false
	f.offline()
	f.optIn("pc-a")
	runner := &fakeRunner{ready: true}
	e.s.PanelRunner = runner
	e.s.Version = "8-ddddddd"
	e.s.PanelUpdates = updates.Static{Tag: "build-9", Version: "9-ccccccc", Build: model.P(9), Verified: true}
	admin := e.cookie(controller(), 0)
	run, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"})
	if err != nil {
		t.Fatal(err)
	}
	post := func() resp {
		return e.do("POST", "/api/admin/panel-update", map[string]any{}, "Cookie", admin, "X-Requested-With", "fairy")
	}
	if r := post(); r.code != 409 || runner.starts != 0 || !strings.Contains(fmt.Sprint(r.body["detail"]), `"Update unpowered hosts" is running (started by Alice)`) {
		t.Fatalf("while a run is going: %d %s", r.code, r.raw)
	}
	if _, err := e.s.WakeRuns.CancelRun(run.ID, "Alice", false); err != nil {
		t.Fatal(err)
	}
	f.finish()
	if r := post(); r.code != 202 || runner.starts != 1 {
		t.Fatalf("after the run: %d %s", r.code, r.raw)
	}
}

// A run journal with a null machine entry (review finding W4) made the server panic while it was being made:
// the panel did not start. Now the entry is left out, the panel starts, the page shows the runs, and nothing
// stops a new run.
func TestWakeUpdateJournalWithANullHostDoesNotStopThePanel(t *testing.T) {
	e := setup(t)
	if err := os.MkdirAll(e.s.Cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"version":1,"active":{"id":"x","state":"running","by":"Alice","started_at":"2026-11-11T02:00:00Z","hosts":[null]},
		"history":[{"id":"y","state":"done","by":"Alice","hosts":[null,{"id":"laptop","name":"laptop","mode":"update_only","state":"stayed_on","text":"It was on and stays on.","on":"on","log":[]}]}]}`
	if err := os.WriteFile(filepath.Join(e.s.Cfg.DataDir, "wake-update.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s2 := New(e.s.Cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, e.s.Poller, nil, "test") // panicked here
	srv := httptest.NewServer(s2.Handler())
	defer srv.Close()
	e2 := &env{t: t, s: s2, srv: srv, b: e.b}
	if err := s2.WakeRuns.JournalError(); err != nil {
		t.Fatalf("the journal is usable: %v", err)
	}
	page := e2.do("GET", "/api/admin/wake-update", nil, "Cookie", e2.cookie(controller(), 0))
	last, _ := page.body["last"].([]any)
	if page.code != 200 || page.body["problem"] != nil || page.body["active"] != nil || len(last) != 2 {
		t.Fatalf("the page: %d %s", page.code, page.raw)
	}
	first, second := last[0].(map[string]any), last[1].(map[string]any)
	if hosts, ok := first["hosts"].([]any); first["state"] != "interrupted" || !ok || len(hosts) != 0 {
		t.Fatalf("the interrupted run, without the empty entry: %v", first)
	}
	if hosts, _ := second["hosts"].([]any); len(hosts) != 1 || hosts[0].(map[string]any)["text"] != "It was on and stays on." {
		t.Fatalf("the finished run keeps its real entry: %v", second)
	}
	if err := s2.Poller.Admin.SetWakeUpdate("laptop", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.WakeRuns.Start(wakerun.StartRequest{UpdateOnly: []string{"laptop"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatalf("a new run: %v", err)
	}
}
