package web

import (
	"testing"
)

// powerSources is GET /api/power as this viewer gets it: the sources by id, and the total.
func powerSources(t *testing.T, e *env, cookie string) (map[string]map[string]any, any) {
	t.Helper()
	r := e.do("GET", "/api/power", nil, "Cookie", cookie)
	list, ok := r.body["sources"].([]any)
	if r.code != 200 || !ok {
		t.Fatalf("power: %d %s", r.code, r.raw)
	}
	out := map[string]map[string]any{}
	for _, x := range list {
		out[x.(map[string]any)["id"].(string)] = x.(map[string]any)
	}
	return out, r.body["total_w"]
}

// The owner's shape: an agent on one site reads plugs (through Home Assistant) that feed machines elsewhere.
// A viewer sees a plug because of the machine it is linked to, not because of the agent that relays it, which
// is somebody else's machine (here "secret-box", which neither viewer sees; the plugs feed Alice's laptop).
//
// A plug that fails has no reading, and until build 14 the link was in the readings alone: the row was gone
// for Alice the moment the last reading was (after offline_after; at once for a plug that never read), with
// the reason it failed for, and "home assistant refused the token" was shown to nobody but an admin. It stays
// now, with the agent's words and its link, and the agent that relays it stays hidden.
//
// And a plug that is left out of the total (power_exclude_from_total: the plug in front of a UPS) is listed
// with its watts, marked, and counted in no total, the viewer's or the admin's.
func TestAPlugRelayedBySomeoneElsesAgentStaysWithTheViewerOfItsMachine(t *testing.T) {
	e := setupSites(t)
	const refused = "home assistant refused the token (http 401)"
	rows := []map[string]any{
		{"id": "lap-plug", "name": "Laptop plug", "type": "homeassistant", "ok": true, "watts": 40.0},
		{"id": "lap-bad", "name": "Laptop dock plug", "type": "homeassistant", "ok": false, "error": refused},
		{"id": "corner", "name": "Corner plug", "type": "homeassistant", "ok": true, "watts": 345.0},
		{"id": "own", "name": "Secret plug", "type": "homeassistant", "ok": true, "watts": 7.0, "system_hint": "self"}}
	links := map[string]string{"lap-plug": "laptop", "lap-bad": "laptop", "corner": "laptop"}
	e.s.Poller.Power.UpdateRemote("secret-box", rows, false, links, []string{"corner"})

	alice, total := powerSources(t, e, e.cookie(person(subAlice), 0))
	for _, id := range []string{"secret-box/lap-plug", "secret-box/lap-bad", "secret-box/corner"} {
		s := alice[id]
		if s == nil {
			t.Fatalf("Alice does not see %s, a plug of her laptop: %v", id, alice)
		}
		if s["system_id"] != "laptop" || s["via"] != nil || s["type"] != "homeassistant" {
			t.Errorf("%s as Alice gets it: system_id %v, via %v, type %v", id, s["system_id"], s["via"], s["type"])
		}
	}
	if s := alice["secret-box/lap-bad"]; s["status"] != "unreachable" || s["error"] != refused || len(s["readings"].([]any)) != 0 {
		t.Errorf("the plug that fails: %v", s)
	}
	if alice["secret-box/own"] != nil {
		t.Error("Alice sees the plug of the machine she does not see")
	}
	// the plug in front of the UPS: listed with its watts, marked, and not in her total (the laptop's own
	// desk plug of the fixture, 80 W, and the laptop plug, 40 W)
	c := alice["secret-box/corner"]
	if c["include_in_total"] != false || c["readings"].([]any)[0].(map[string]any)["watts"] != 345.0 {
		t.Errorf("the excluded plug: %v", c)
	}
	if total != 120.0 {
		t.Errorf("Alice's total is %v, want 120 (the excluded 345 W are in no total)", total)
	}
	// Bob sees neither the laptop nor secret-box: none of them
	bob, _ := powerSources(t, e, e.cookie(person(subBob), 0))
	for id := range bob {
		if len(id) > 11 && id[:11] == "secret-box/" {
			t.Errorf("Bob sees %s", id)
		}
	}
	// the admin sees all four, with the agent that relays them, and the same plug left out of the total
	admin, adminTotal := powerSources(t, e, e.cookie(person(subAdmin), 0))
	if s := admin["secret-box/lap-bad"]; s == nil || s["via"] != "secret-box" || s["error"] != refused || s["system_id"] != "laptop" {
		t.Errorf("the admin's row of the plug that fails: %v", s)
	}
	if admin["secret-box/own"] == nil || admin["secret-box/corner"]["include_in_total"] != false {
		t.Errorf("the admin's rows: %v", admin)
	}
	sum := 0.0
	for _, s := range admin {
		if s["include_in_total"] == true && s["status"] != "unreachable" {
			for _, r := range s["readings"].([]any) {
				if w, ok := r.(map[string]any)["watts"].(float64); ok {
					sum += w
				}
			}
		}
	}
	if adminTotal != sum || sum < 47 {
		t.Errorf("the admin's total is %v, the counted sources add up to %v", adminTotal, sum)
	}
}
