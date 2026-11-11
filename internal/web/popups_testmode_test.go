//go:build fairytest

package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// The pop-ups in a test build (TESTING-LOCAL.md): the built-in admin has the same settings and the same stream.
func TestTestModePopups(t *testing.T) {
	e := testPanel(t)
	ck := e.open()
	csrf := []string{"Cookie", ck, "X-Requested-With", "fairy"}
	popups := func(r resp) string { b, _ := json.Marshal(r.body["popups"]); return string(b) }
	if r := e.do("GET", "/api/settings", nil, "Cookie", ck); r.code != 200 || popups(r) != `{"enabled":true,"level":"critical","seconds":5}` {
		t.Fatal("defaults:", r.code, string(r.raw))
	}
	body := map[string]any{"theme": "black", "background": map[string]any{"kind": "none"}, "dim": 60, "blur": 14, "card_opacity": 72,
		"popups": map[string]any{"enabled": true, "seconds": 3, "level": "warning"}}
	if r := e.do("PUT", "/api/settings", body, csrf...); r.code != 200 || popups(r) != `{"enabled":true,"level":"warning","seconds":3}` {
		t.Fatal("save:", r.code, string(r.raw))
	}
	if got := e.s.Settings.Get(TestUser.Sub).Popups; got.Seconds != 3 || got.Level != "warning" || !got.Enabled {
		t.Fatalf("saved for %s: %+v", TestUser.Sub, got)
	}
	body["popups"] = map[string]any{"seconds": 31}
	if r := e.do("PUT", "/api/settings", body, csrf...); r.code != 422 {
		t.Fatal("31 seconds:", r.code)
	}
	done := make(chan string)
	go func() { _, b := e.streamBody("/api/stream", "Cookie", ck); done <- b }()
	time.Sleep(300 * time.Millisecond)
	ev := e.s.Notes.Add(notifications.Event{Kind: "host_offline", Type: "host", Severity: notifications.Warning, SystemID: "laptop", Host: hostileName,
		Text: "offline", Key: "offline:laptop", Alarm: true})
	got := notesNew(t, <-done)
	if len(got) != 2 || len(got[0].Events) != 0 || len(got[1].Events) != 1 || got[1].Events[0].ID != ev.ID || !got[1].Events[0].Urgent || got[1].Events[0].Host != hostileName {
		t.Fatalf("notes_new: %+v", got)
	}
}
