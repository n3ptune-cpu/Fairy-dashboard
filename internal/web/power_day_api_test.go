package web

import (
	"encoding/json"
	"math"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/power"
)

// dayOf is GET /api/power/day for one source as this caller gets it.
func dayOf(e *env, source string, hdr ...string) resp {
	return e.do("GET", "/api/power/day?source="+url.QueryEscape(source), nil, hdr...)
}

// GET /api/power/day gives a power source's last 24 hours to exactly those who get the source from
// GET /api/power. The fixture: Alice sees the laptop and its desk plug (laptop/desk); nobody but the admin
// sees secret-box and its plug (secret-box/desk); Bob sees the tower. A source the caller does not see is
// answered like one that does not exist: the same status, the same words, nothing of its id.
func TestPowerDayFollowsTheRightsOfThePowerList(t *testing.T) {
	e := setupSites(t)
	const notFound = `{"detail":"power source not found"}`
	cookie := func(sub string) []string { return []string{"Cookie", e.cookie(person(sub), 0)} }
	mine, hidden := "laptop/desk", "secret-box/desk"
	for _, id := range []string{mine, hidden} {
		if !e.s.Poller.Power.Day.Has(id, time.Now()) {
			t.Fatalf("the fixture has no day for %s", id)
		}
	}
	// not signed in, and waiting for access: no day, of any source, and the same answer for all
	for _, id := range []string{mine, hidden, "nope", ""} {
		if r := dayOf(e, id); r.code != 401 {
			t.Errorf("not signed in, %q: %d %s", id, r.code, r.raw)
		}
		if r := dayOf(e, id, cookie(subPending)...); r.code != 403 || strings.Contains(string(r.raw), "summary") {
			t.Errorf("waiting for access, %q: %d %s", id, r.code, r.raw)
		}
	}
	// Alice: her plug with its day; the hidden one, an unknown one and no id at all are one and the same answer
	r := dayOf(e, mine, cookie(subAlice)...)
	sum, _ := r.body["summary"].(map[string]any)
	if r.code != 200 || r.body["source"] != mine || r.body["step_s"] != 60.0 || sum == nil || sum["max_w"] != 80.0 || sum["energy"] != "estimated" {
		t.Fatalf("Alice's own plug: %d %s", r.code, r.raw)
	}
	if avg := r.body["avg"].([]any); len(avg) == 0 || avg[len(avg)-1] != 80.0 || len(r.body["volts"].([]any)) != len(avg) || r.body["amps"] != nil {
		t.Errorf("the lists of Alice's plug: %s", r.raw)
	}
	for _, id := range []string{hidden, "nope", "", "secret-box", "laptop", "laptop/", "/desk", "LAPTOP/DESK"} {
		if r := dayOf(e, id, cookie(subAlice)...); r.code != 404 || strings.TrimSpace(string(r.raw)) != notFound {
			t.Errorf("Alice, %q: %d %s, want 404 %s", id, r.code, r.raw, notFound)
		}
	}
	// Bob sees neither plug
	for _, id := range []string{mine, hidden, "nope"} {
		if r := dayOf(e, id, cookie(subBob)...); r.code != 404 || strings.TrimSpace(string(r.raw)) != notFound {
			t.Errorf("Bob, %q: %d %s", id, r.code, r.raw)
		}
	}
	// the admin sees both, and an unknown one is unknown for an admin too
	for _, id := range []string{mine, hidden} {
		if r := dayOf(e, id, cookie(subAdmin)...); r.code != 200 || r.body["source"] != id || r.body["summary"] == nil {
			t.Errorf("the admin, %q: %d %.200s", id, r.code, r.raw)
		}
	}
	if r := dayOf(e, "nope", cookie(subAdmin)...); r.code != 404 || strings.TrimSpace(string(r.raw)) != notFound {
		t.Errorf("the admin, an unknown source: %d %s", r.code, r.raw)
	}
	// an API token that may read reads it, like /api/power/history; it needs no CSRF header
	tok, _, err := e.s.Tokens.Create("reader", "system", "read", "cli:test", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r := dayOf(e, hidden, "Authorization", "Bearer "+tok); r.code != 200 || r.body["source"] != hidden {
		t.Errorf("a read token: %d %.200s", r.code, r.raw)
	}
	if r := e.do("POST", "/api/power/day?source="+url.QueryEscape(mine), nil, cookie(subAdmin)...); r.code == 200 {
		t.Errorf("the day answers a POST: %d", r.code)
	}

	// The site of a source is set on the Users page: secret-box's plug goes to Bob's site. Bob gets its day
	// from then on, and Alice still does not.
	if _, err := e.s.Access.Assign(access.PowerPrefix+hidden, "Bob"); err != nil {
		t.Fatal(err)
	}
	if r := dayOf(e, hidden, cookie(subBob)...); r.code != 200 || r.body["summary"] == nil {
		t.Errorf("Bob, a plug assigned to his site: %d %.200s", r.code, r.raw)
	}
	if r := dayOf(e, hidden, cookie(subAlice)...); r.code != 404 {
		t.Errorf("Alice, a plug assigned to Bob's site: %d", r.code)
	}
}

// The day is the plug's, and who may see it is asked when it is asked for. A plug relayed by somebody else's
// agent is linked to Alice's laptop: Alice gets its day and Bob does not. Linked to Bob's tower instead (Edit
// host, power_links): Bob gets the day, the whole of it, and Alice none. A plug the agent no longer reports
// has no day for anybody, the admin included, although its buckets are still in memory.
func TestPowerDayGoesWithThePlugWhenItIsLinkedElsewhere(t *testing.T) {
	e := setupSites(t)
	cookie := func(sub string) []string { return []string{"Cookie", e.cookie(person(sub), 0)} }
	pm := e.s.Poller.Power
	rows := []map[string]any{{"id": "plug-a", "name": "Plug A", "type": "homeassistant", "ok": true, "watts": 40.0, "voltage": 246.5, "current": 0.085, "energy_kwh_total": 0.004}}
	const id = "secret-box/plug-a"
	pm.UpdateRemote("secret-box", rows, false, map[string]string{"plug-a": "laptop"}, nil)
	pm.SampleHistory()
	a := dayOf(e, id, cookie(subAlice)...)
	if a.code != 200 || a.body["summary"] == nil || a.body["source"] != id {
		t.Fatalf("Alice, a plug of her laptop that someone else's agent relays: %d %s", a.code, a.raw)
	}
	if amps := a.body["amps"].([]any); amps[len(amps)-1] != 0.085 {
		t.Errorf("a small current: %v, want 0.085", amps[len(amps)-1])
	}
	if r := dayOf(e, id, cookie(subBob)...); r.code != 404 {
		t.Fatalf("Bob, a plug of Alice's laptop: %d", r.code)
	}
	pm.UpdateRemote("secret-box", rows, false, map[string]string{"plug-a": "tower"}, nil) // linked to the tower now
	pm.SampleHistory()
	b := dayOf(e, id, cookie(subBob)...)
	if b.code != 200 || b.body["start"] != a.body["start"] {
		t.Errorf("Bob, after the plug was linked to his tower: %d, start %v (Alice had %v)", b.code, b.body["start"], a.body["start"])
	}
	if r := dayOf(e, id, cookie(subAlice)...); r.code != 404 {
		t.Errorf("Alice, after the plug was linked to the tower: %d", r.code)
	}
	// the agent reports the plug no more
	pm.UpdateRemote("secret-box", []map[string]any{}, false, nil, nil)
	if !pm.Day.Has(id, time.Now()) {
		t.Fatal("the fixture: the buckets should still be in memory")
	}
	for _, sub := range []string{subAdmin, subBob, subAlice} {
		if r := dayOf(e, id, cookie(sub)...); r.code != 404 {
			t.Errorf("%s, a plug that is not reported any more: %d %.200s", sub, r.code, r.raw)
		}
	}
}

// A whole day, as the page gets it: 1440 minutes with a gap of five hours and a counter that restarted in it.
// The answer is small (parallel lists of plain numbers, null in the gap), is JSON whatever was recorded, and
// its numbers do not count the gap.
func TestPowerDayAnswerForAWholeDay(t *testing.T) {
	e := setup(t)
	now := time.Now()
	day := e.s.Poller.Power.Day
	const id = "laptop/desk" // the fixture's plug: listed, so its day can be asked for
	day.Forget("laptop/")
	k, want := 500.0, 0.0                      // the plug's counter, and what it counts in the minutes with readings
	for m := power.DaySlots - 1; m >= 1; m-- { // 23 h 59 min, ending a minute ago
		if m >= 600 && m < 900 { // five hours without readings
			continue
		}
		at := now.Add(-time.Duration(m) * time.Minute)
		w := 123.4 + 40*math.Sin(float64(m)/50)
		step := (w + 450) / 60000 // a minute at the average of its two samples, in kWh
		switch m {
		case power.DaySlots - 1: // the first minute: the counter's value there is where the day starts
		case 599:
			k = 0 // the plug lost its power in the gap: the counter starts again
		default:
			k, want = k+step, want+step
		}
		v, a := 246.5+float64(m%10)/10, w/246.5
		day.Record(at, []power.DaySample{{ID: id, W: w, V: &v, A: &a, KWh: &k}}, nil)
		day.Record(at.Add(20*time.Second), []power.DaySample{{ID: id, W: w + 900, V: &v, A: &a, KWh: &k}}, nil) // a spike in every minute
	}
	r := dayOf(e, id, "Cookie", e.cookie(controller(), 0))
	if r.code != 200 || !json.Valid(r.raw) {
		t.Fatalf("%d %.300s", r.code, r.raw)
	}
	if len(r.raw) > 48<<10 {
		t.Errorf("a whole day is %d bytes, want under 48 kB", len(r.raw))
	}
	for _, bad := range []string{"NaN", "Inf", `"devices"`, `"sources"`, `"kwh"`} {
		if strings.Contains(string(r.raw), bad) {
			t.Errorf("the answer has %s in it", bad)
		}
	}
	avg, min, max, volts, amps := r.body["avg"].([]any), r.body["min"].([]any), r.body["max"].([]any), r.body["volts"].([]any), r.body["amps"].([]any)
	if n := len(avg); n < power.DaySlots-2 || n > power.DaySlots || len(min) != n || len(max) != n || len(volts) != n || len(amps) != n {
		t.Fatalf("the lists: %d %d %d %d %d", len(avg), len(min), len(max), len(volts), len(amps))
	}
	gaps := 0
	for i := range avg {
		if avg[i] == nil {
			gaps++
			if min[i] != nil || max[i] != nil || volts[i] != nil || amps[i] != nil {
				t.Fatalf("minute %d is a gap in one list and not in another", i)
			}
			continue
		}
		if max[i].(float64)-min[i].(float64) < 899 || avg[i].(float64) <= min[i].(float64) || avg[i].(float64) >= max[i].(float64) {
			t.Fatalf("minute %d: min %v avg %v max %v: the spike of each minute is not in it", i, min[i], avg[i], max[i])
		}
	}
	if gaps < 299 || gaps > 301 {
		t.Errorf("%d minutes are null, want the 300 of the gap", gaps)
	}
	sum := r.body["summary"].(map[string]any)
	if got := sum["covered_s"].(float64); got < 1137*60 || got > 1140*60 {
		t.Errorf("covered_s %v, want about 19 h", got)
	}
	// the counter's increases before the gap and after it: not minus 500 kWh, and not the watts of a day
	if kwh := sum["energy_kwh"].(float64); sum["energy"] != "counter" || math.Abs(kwh-want) > 0.002 || sum["energy_est_kwh"] != 0.0 {
		t.Errorf("energy over a day with a counter that restarted: %v kWh (%v), want %.3f from the counter", sum["energy_kwh"], sum["energy"], want)
	}
	if sum["avg_w"].(float64) < 500 || sum["min_w"].(float64) > 124 || sum["max_w"].(float64) < 1050 {
		t.Errorf("the day's watts: %v", sum)
	}
}
