package power

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The sockets of a plug, as an agent from v0.10.0 on relays them in the row of the plug ("sockets"). The
// panel keeps what it needs to show a socket and to name it in a switch request, and nothing of a row is
// trusted beyond that: an agent is a machine the panel does not control.

func socketRow(id, name, state string, switchable bool, why any, tag string) map[string]any {
	return map[string]any{"id": id, "name": name, "state": state, "switchable": switchable, "why": why, "tag": tag, "error": nil}
}

func socketsOf(t *testing.T, pm *Manager, id string) []model.PowerSocket {
	t.Helper()
	for _, s := range pm.Overview().Sources {
		if s.ID == id {
			return s.Sockets
		}
	}
	t.Fatalf("no source %s", id)
	return nil
}

// a tag as an agent makes one: 22 characters
func tagOf(socket string) string {
	return api.SocketTag("desk-bar", socket, "a name", "switch.example_"+socket)
}

func TestRelayedSocketsAreKept(t *testing.T) {
	pm := manager(t, nil, nil, nil)
	pm.UpdateRemote("mini", []map[string]any{{"id": "desk-bar", "name": "Monitor powerbar", "type": "homeassistant", "ok": true, "watts": 95.5,
		"sockets": []any{
			socketRow("ch1", "Left monitor", "on", true, nil, tagOf("ch1")),
			socketRow("ch2", "ch2", "off", true, nil, tagOf("ch2")),
			socketRow("ch3", "Lamp", "on", false, api.WhyNotAllowed, tagOf("ch3")),
			map[string]any{"id": "ch4", "name": "Printer", "state": "unknown", "switchable": true, "why": nil, "tag": tagOf("ch4"), "error": "timeout"},
		}},
		{"id": "meter-only", "name": "A meter", "type": "shelly", "ok": true, "watts": 3.0}}, false, nil, nil)
	got := socketsOf(t, pm, "mini/desk-bar")
	if len(got) != 4 {
		t.Fatalf("%d sockets kept: %+v", len(got), got)
	}
	want := []model.PowerSocket{
		{ID: "ch1", Name: "Left monitor", State: "on", Switchable: true, Tag: tagOf("ch1")},
		{ID: "ch2", Name: "ch2", State: "off", Switchable: true, Tag: tagOf("ch2")},
		{ID: "ch3", Name: "Lamp", State: "on", Why: api.WhyNotAllowed, Tag: tagOf("ch3")},
		{ID: "ch4", Name: "Printer", State: "unknown", Switchable: true, Tag: tagOf("ch4"), Error: model.P("timeout")},
	}
	for i, w := range want {
		g := got[i]
		if g.ID != w.ID || g.Name != w.Name || g.State != w.State || g.Switchable != w.Switchable || g.Why != w.Why || g.Tag != w.Tag ||
			model.Deref(g.Error) != model.Deref(w.Error) || g.SystemID != nil || g.Lock != "" {
			t.Errorf("socket %d: %+v, want %+v", i, g, w)
		}
	}
	// a row without the member has none, and the member is then not in the answer at all
	if s := socketsOf(t, pm, "mini/meter-only"); s != nil {
		t.Errorf("a meter without sockets: %+v", s)
	}
	// the row stays what its reading made it, whatever its sockets say
	for _, s := range pm.Overview().Sources {
		if s.ID == "mini/desk-bar" && (s.Status != "ok" || len(s.Readings) != 1 || *s.Readings[0].Watts != 95.5) {
			t.Errorf("the row of the plug: %+v", s)
		}
	}
	// the agent takes the sockets out of its config: they go
	pm.UpdateRemote("mini", []map[string]any{{"id": "desk-bar", "name": "Monitor powerbar", "type": "homeassistant", "ok": true, "watts": 95.5}}, false, nil, nil)
	if s := socketsOf(t, pm, "mini/desk-bar"); s != nil {
		t.Errorf("after the agent stopped relaying sockets: %+v", s)
	}
}

// What a hostile or broken agent sends: a hundred sockets, long names, control characters, ids and tags of
// the wrong form, words the panel does not know, "switchable" with a reason next to it.
func TestRelayedSocketsFromAHostileAgent(t *testing.T) {
	pm := manager(t, nil, nil, nil)
	var many []any
	for i := range 100 {
		many = append(many, socketRow(fmt.Sprintf("s%d", i), "x", "on", true, nil, tagOf("x")))
	}
	long := strings.Repeat("N", 1<<16)
	pm.UpdateRemote("evil", []map[string]any{
		{"id": "many", "name": "many", "type": "homeassistant", "ok": true, "watts": 1.0, "sockets": many},
		{"id": "odd", "name": "odd", "type": "homeassistant", "ok": true, "watts": 1.0, "sockets": []any{
			socketRow("ok1", long, "on", true, nil, tagOf("ok1")),
			socketRow("ok2", "line\nbreak\x1b[31m ‮evil", "ON", true, nil, tagOf("ok2")),
			socketRow("UPPER", "bad id", "on", true, nil, tagOf("x")),
			socketRow("this-id-is-too-long-for-a-socket", "bad id", "on", true, nil, tagOf("x")),
			socketRow("", "no id", "on", true, nil, tagOf("x")),
			socketRow("ok1", "the same id again", "off", true, nil, tagOf("x")),
			socketRow("notag", "no tag", "on", true, nil, ""),
			socketRow("badtag", "bad tag", "on", true, nil, strings.Repeat("!", 22)),
			socketRow("longtag", "long tag", "on", true, nil, tagOf("x")+"A"),
			socketRow("both", "switchable and a reason", "on", true, api.WhyFeedsSelf, tagOf("both")),
			socketRow("newwhy", "a reason of a later agent", "off", false, "moon_phase", tagOf("newwhy")),
			socketRow("nowhy", "not switchable, no reason", "off", false, nil, tagOf("nowhy")),
		}},
		{"id": "odd2", "name": "odd2", "type": "homeassistant", "ok": true, "watts": 1.0, "sockets": []any{
			"not an object", nil, 42,
			map[string]any{"id": "types", "name": 7, "state": 1, "switchable": "yes", "why": 3, "tag": 5, "error": long},
			map[string]any{"id": "longerr", "name": "Lamp", "state": "unknown", "switchable": true, "why": nil, "tag": tagOf("longerr"), "error": long},
		}},
		{"id": "notalist", "name": "x", "type": "homeassistant", "ok": true, "watts": 1.0, "sockets": "ch1"},
		{"id": "failing", "name": "x", "type": "homeassistant", "ok": false, "error": "sensor unavailable", "sockets": []any{socketRow("ch1", "Lamp", "off", true, nil, tagOf("ch1"))}},
	}, false, nil, nil)

	if got := socketsOf(t, pm, "evil/many"); len(got) != MaxRemoteSockets || got[0].ID != "s0" || got[MaxRemoteSockets-1].ID != fmt.Sprintf("s%d", MaxRemoteSockets-1) {
		t.Errorf("a hundred sockets: %d kept", len(got))
	}
	by := map[string]model.PowerSocket{}
	odd := append(socketsOf(t, pm, "evil/odd"), socketsOf(t, pm, "evil/odd2")...)
	for _, s := range odd {
		if _, twice := by[s.ID]; twice {
			t.Errorf("the id %s is there twice", s.ID)
		}
		by[s.ID] = s
		if len(s.Name) > maxSocketName || s.Name == "" || strings.ContainsAny(s.Name, "\n\x1b‮") || (s.Error != nil && len(*s.Error) > maxRemoteError) {
			t.Errorf("texts of %s: %q %v", s.ID, s.Name, s.Error)
		}
		if s.Switchable && (s.Why != "" || api.CheckSocketTag(s.Tag) != nil) {
			t.Errorf("%s is switchable with why %q and tag %q", s.ID, s.Why, s.Tag)
		}
		if !s.Switchable && s.Why == "" {
			t.Errorf("%s is not switchable and has no reason", s.ID)
		}
		switch s.State {
		case "on", "off", "unavailable", "unknown":
		default:
			t.Errorf("%s has the state %q", s.ID, s.State)
		}
	}
	if len(odd) != 10 {
		t.Errorf("%d sockets kept, want the 8 of the first plug and the 2 of the second", len(odd))
	}
	if s := by["longerr"]; s.Error == nil || len(*s.Error) != maxRemoteError || !s.Switchable {
		t.Errorf("a long error text: %+v", s)
	}
	for _, id := range []string{"UPPER", "this-id-is-too-long-for-a-socket", ""} {
		if _, ok := by[id]; ok {
			t.Errorf("a socket with the id %q is kept", id)
		}
	}
	if s := by["ok1"]; s.Name != strings.Repeat("N", maxSocketName) || s.State != "on" || !s.Switchable {
		t.Errorf("the first socket: its name is %d bytes, %+v", len(s.Name), s)
	}
	if s := by["ok2"]; s.State != "unknown" { // "ON" is not one of the four words
		t.Errorf("a state that is none of the four words: %+v", s)
	}
	// without a tag of the right form the socket is shown and cannot be switched
	for _, id := range []string{"notag", "badtag", "longtag", "types"} {
		if s, ok := by[id]; !ok || s.Switchable || s.Tag != "" || s.Why != model.SocketNoTag {
			t.Errorf("%s: %+v (kept %v)", id, s, ok)
		}
	}
	if s := by["both"]; s.Switchable || s.Why != api.WhyFeedsSelf {
		t.Errorf("switchable with a reason next to it: %+v", s)
	}
	if s := by["newwhy"]; s.Switchable || s.Why != model.SocketNotSwitchable {
		t.Errorf("a reason the panel does not know: %+v", s)
	}
	if s := by["nowhy"]; s.Switchable || s.Why != model.SocketNotSwitchable {
		t.Errorf("not switchable without a reason: %+v", s)
	}
	if s := by["types"]; s.Name != "types" || s.State != "unknown" {
		t.Errorf("members of the wrong type: %+v", s)
	}
	if s := socketsOf(t, pm, "evil/notalist"); s != nil {
		t.Errorf("sockets that are no list: %+v", s)
	}
	// a plug whose reading fails still has its sockets
	if s := socketsOf(t, pm, "evil/failing"); len(s) != 1 || !s[0].Switchable || s[0].State != "off" {
		t.Errorf("the sockets of a plug whose reading fails: %+v", s)
	}
}

// An agent that does not answer: what its sockets were is not what they are. They are shown without a state
// and cannot be switched, and they come back with the agent's next answer.
func TestSocketsOfAnAgentThatDoesNotAnswer(t *testing.T) {
	pm := manager(t, nil, nil, nil)
	rows := []map[string]any{{"id": "desk-bar", "name": "Monitor powerbar", "type": "homeassistant", "ok": true, "watts": 95.5,
		"sockets": []any{socketRow("ch1", "Left monitor", "on", true, nil, tagOf("ch1")), socketRow("ch3", "Lamp", "on", false, api.WhyNotAllowed, tagOf("ch3"))}}}
	pm.UpdateRemote("mini", rows, false, nil, nil)
	pm.UpdateRemote("mini", nil, true, nil, nil)
	got := socketsOf(t, pm, "mini/desk-bar")
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	for _, s := range got {
		if s.Switchable || s.State != api.SocketStateUnknown || s.Why != model.SocketAgentUnreachable || s.Name == "" || s.Tag == "" {
			t.Errorf("while the agent does not answer: %+v", s)
		}
	}
	pm.UpdateRemote("mini", rows, false, nil, nil)
	if got := socketsOf(t, pm, "mini/desk-bar"); !got[0].Switchable || got[0].State != "on" || got[1].Why != api.WhyNotAllowed {
		t.Errorf("after the agent answered again: %+v", got)
	}
}

// The panel's entry of an agent links sockets (socket_links); a socket named there that the agent does not
// report is said once in the log, as for power_links.
func TestSocketLinksToSocketsTheAgentDoesNotReport(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)
	pm := manager(t, nil, nil, nil)
	rows := []map[string]any{{"id": "desk-bar", "name": "Monitor powerbar", "type": "homeassistant", "ok": true, "watts": 95.5,
		"sockets": []any{socketRow("ch1", "Left monitor", "on", true, nil, tagOf("ch1"))}}}
	links := map[string]string{"desk-bar/ch1": "pc-a", "desk-bar/ch9": "pc-b", "gone/main": "pc-c"}
	for range 3 {
		pm.UpdateRemote("mini", rows, false, nil, nil)
		pm.NoteSocketLinks("mini", links)
	}
	if n := strings.Count(logs.String(), "socket_links"); n != 1 {
		t.Fatalf("said %d times:\n%s", n, logs.String())
	}
	if out := logs.String(); !strings.Contains(out, "desk-bar/ch9, gone/main") || strings.Contains(out, "desk-bar/ch1,") {
		t.Errorf("the line: %s", out)
	}
	// nothing is said about an agent that has no such links, or once the names are right
	logs.Reset()
	pm.NoteSocketLinks("mini", map[string]string{"desk-bar/ch1": "pc-a"})
	pm.NoteSocketLinks("other", nil)
	if logs.Len() != 0 {
		t.Errorf("said: %s", logs.String())
	}
}
