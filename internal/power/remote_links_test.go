package power

import (
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

const refusedWords = "home assistant refused the token (http 401)" // the agent's fixed words for it

// A device an agent relays that fails for longer than offline_after has no reading any more. It does not go
// away: it is listed for as long as the agent reports it, "unreachable", with the agent's words for why (an
// agent that reads a Home Assistant sensor reports "home assistant refused the token" all through the hour
// it waits before it asks again), and with its link. The link was carried by the readings alone until build
// 14, so a source without a reading was linked to nothing: it was gone for everyone who saw it because of the
// machine it feeds, and its reason was on no host's page.
func TestARelayedSourceThatFailsKeepsItsReasonAndItsLink(t *testing.T) {
	pm, err := NewManager(nil, 5, time.Second, 0.05, 3600, nil, func() float64 { return 0 }) // offline_after: 50 ms
	if err != nil {
		t.Fatal(err)
	}
	links := map[string]string{"plug": "pc-a", "never": "pc-b"}
	good := map[string]any{"id": "plug", "name": "PC plug", "type": "homeassistant", "ok": true, "watts": 40.0}
	bad := map[string]any{"id": "plug", "name": "PC plug", "type": "homeassistant", "ok": false, "error": refusedWords}
	never := map[string]any{"id": "never", "name": "Other plug", "type": "homeassistant", "ok": false, "error": "home assistant has no such entity (http 404)"}
	get := func(id string) model.PowerSourceState {
		t.Helper()
		st, ok := byID(pm.Overview())[id]
		if !ok {
			t.Fatalf("%s is not listed", id)
		}
		return st
	}
	pm.UpdateRemote("relay", []map[string]any{good, never}, false, links, nil)
	if st := get("relay/plug"); st.Status != "ok" || st.SystemID == nil || *st.SystemID != "pc-a" || *st.Readings[0].SystemID != "pc-a" || st.Error != nil {
		t.Fatalf("a good reading: %+v", st)
	}
	// one that never had a reading: listed at once, with its reason and its link
	if st := get("relay/never"); st.Status != "unreachable" || len(st.Readings) != 0 || st.Error == nil || *st.Error != "home assistant has no such entity (http 404)" ||
		st.SystemID == nil || *st.SystemID != "pc-b" {
		t.Fatalf("a source that never had a reading: %+v", st)
	}
	pm.UpdateRemote("relay", []map[string]any{bad, never}, false, links, nil)
	if st := get("relay/plug"); st.Status != "stale" || len(st.Readings) != 1 || *st.Error != refusedWords {
		t.Fatalf("failing, younger than offline_after: %+v", st)
	}
	time.Sleep(80 * time.Millisecond)
	for i := 0; i < 3; i++ { // the agent goes on reporting it, poll after poll
		pm.UpdateRemote("relay", []map[string]any{bad, never}, false, links, nil)
		st := get("relay/plug")
		if st.Status != "unreachable" || len(st.Readings) != 0 || st.Error == nil || *st.Error != refusedWords {
			t.Fatalf("failing for longer than offline_after: %+v", st)
		}
		if st.SystemID == nil || *st.SystemID != "pc-a" || st.Via == nil || *st.Via != "relay" || st.Type != "homeassistant" || st.Name != "PC plug" || st.LastSuccess == nil {
			t.Fatalf("what is left of it: %+v", st)
		}
	}
	if w := pm.LinkedWatts(); len(w) != 0 {
		t.Fatalf("watts of sources without a reading: %v", w)
	}
	// it reads again
	pm.UpdateRemote("relay", []map[string]any{good, never}, false, links, nil)
	if st := get("relay/plug"); st.Status != "ok" || st.Error != nil || pm.LinkedWatts()["pc-a"] != 40 {
		t.Fatalf("back: %+v", st)
	}
}

// ShownID turns every link, a source's own and its readings', into the row the machine is shown as, without
// writing to what the manager keeps.
func TestLinksAreGivenAsTheRowTheMachineIsShownAs(t *testing.T) {
	pm := manager(t, nil, nil, nil)
	shownAs := "node-x"
	pm.ShownID = func(id string) string {
		if id == "agent-x" {
			return shownAs
		}
		return id
	}
	rows := []map[string]any{{"id": "a", "ok": true, "watts": 10.0, "system_hint": "self"}, {"id": "b", "ok": true, "watts": 5.0}, {"id": "c", "ok": false, "error": "timeout"}}
	pm.UpdateRemote("agent-x", rows, false, map[string]string{"b": "pc-b", "c": "agent-x"}, nil)
	check := func(want string) {
		t.Helper()
		st := byID(pm.Overview())
		if a := st["agent-x/a"]; *a.SystemID != want || *a.Readings[0].SystemID != want {
			t.Fatalf("system_hint self: %v %v, want %s", *a.SystemID, *a.Readings[0].SystemID, want)
		}
		if b := st["agent-x/b"]; *b.SystemID != "pc-b" || *b.Readings[0].SystemID != "pc-b" {
			t.Fatalf("a link to another machine: %+v", b)
		}
		if c := st["agent-x/c"]; c.SystemID == nil || *c.SystemID != want || len(c.Readings) != 0 {
			t.Fatalf("a source without a reading: %+v", c)
		}
		if w := pm.LinkedWatts(); w[want] != 10 || w["pc-b"] != 5 || len(w) != 2 {
			t.Fatalf("watts by row: %v", w)
		}
	}
	check("node-x")
	shownAs = "agent-x" // the node's row is gone (the panel restarted with the machine off): the same links, the other row
	check("agent-x")
	shownAs = "node-x"
	check("node-x")
}
