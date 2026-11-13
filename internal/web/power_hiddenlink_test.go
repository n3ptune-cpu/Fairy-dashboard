package web

import (
	"math"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// A power source that a viewer sees because it is assigned to their site can be linked to a machine they do
// not see. The link is the id of that machine's row: it is taken out of what the viewer gets, in the readings
// and, since a source keeps its link while it has no reading (build 14), in the source's own system_id. A
// viewer must never be given the id of a row that is hidden from them.
func TestTheLinkOfAPowerSourceToAHiddenMachineIsNotGivenToAViewer(t *testing.T) {
	e := setupSites(t)
	rows := []map[string]any{
		{"id": "good", "name": "Rack plug", "type": "homeassistant", "ok": true, "watts": 30.0, "system_hint": "self"},
		{"id": "bad", "name": "Rack plug 2", "type": "homeassistant", "ok": false, "error": "home assistant has no such entity (http 404)", "system_hint": "self"}}
	e.s.Poller.Power.UpdateRemote("secret-box", rows, false, nil, nil)
	for _, id := range []string{"secret-box/good", "secret-box/bad"} {
		if _, err := e.s.Access.Assign("power:"+id, "Alice"); err != nil {
			t.Fatal(err)
		}
	}
	admin, _ := powerSources(t, e, e.cookie(person(subAdmin), 0))
	for _, id := range []string{"secret-box/good", "secret-box/bad"} {
		if s := admin[id]; s == nil || s["system_id"] != "secret-box" || s["via"] != "secret-box" {
			t.Fatalf("the fixture: the admin's row of %s: %v", id, s)
		}
	}
	alice, _ := powerSources(t, e, e.cookie(person(subAlice), 0))
	for _, id := range []string{"secret-box/good", "secret-box/bad"} {
		s := alice[id]
		if s == nil {
			t.Fatalf("Alice does not see %s, which is assigned to her site: %v", id, alice)
		}
		if s["system_id"] != nil || s["via"] != nil {
			t.Errorf("%s as Alice gets it names a machine she does not see: system_id %v, via %v", id, s["system_id"], s["via"])
		}
		for _, r := range s["readings"].([]any) {
			if r.(map[string]any)["system_id"] != nil {
				t.Errorf("%s: a reading names a machine Alice does not see: %v", id, r)
			}
		}
	}
}

// A power source that cannot be written as JSON is replaced by a stub (encode.go). The stub keeps what says
// whose the source is: its id, its type and name, the agent that relays it, and its link (system_id), which
// decides who sees the source at all and on which host's page its reason is shown.
func TestTheStubOfAPowerSourceThatCannotBeWrittenKeepsItsLink(t *testing.T) {
	nan := math.NaN()
	bad := model.PowerSourceState{ID: "relay/plug", Type: "homeassistant", Name: "Plug", Status: "ok", IncludeInTotal: false,
		Via: model.P("relay"), SystemID: model.P("pc-a"), Readings: []model.PowerReading{{Watts: &nan, SystemID: model.P("pc-a")}}}
	good := model.PowerSourceState{ID: "desk", Type: "shelly", Name: "Desk", Status: "ok", IncludeInTotal: true, SystemID: model.P("pc-b"), Readings: []model.PowerReading{}}
	if writable(bad) || !writable(good) {
		t.Fatal("the fixture: one source that cannot be written and one that can")
	}
	out, ok := reduced(model.PowerOverview{Sources: []model.PowerSourceState{bad, good}})
	ov, isOv := out.(model.PowerOverview)
	if !ok || !isOv || len(ov.Sources) != 2 || !writable(ov) {
		t.Fatalf("reduced: %+v %v", out, ok)
	}
	s := ov.Sources[0]
	if s.ID != "relay/plug" || s.Type != "homeassistant" || s.Name != "Plug" || s.Status != "unreachable" || s.IncludeInTotal || len(s.Readings) != 0 ||
		s.Error == nil || *s.Error != cannotShow || s.Via == nil || *s.Via != "relay" {
		t.Errorf("the stub: %+v", s)
	}
	if s.SystemID == nil || *s.SystemID != "pc-a" {
		t.Errorf("the stub lost the source's link: system_id %v, want pc-a", s.SystemID)
	}
	if g := ov.Sources[1]; g.ID != "desk" || g.SystemID == nil || *g.SystemID != "pc-b" || g.Status != "ok" {
		t.Errorf("the source that can be written was changed: %+v", g)
	}
}
