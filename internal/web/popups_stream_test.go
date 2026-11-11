package web

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// The pop-ups for new notifications (README, "Notifications"), the server's part: the stream's `notes_new`.
// The settings are in popups_settings_test.go, the page's part in popups_ui_test.go.

const hostileName = `<img src=x onerror=alert(1)>`

type notesNewEvent struct {
	Last   int64 `json:"last"`
	Events []struct {
		ID       int64  `json:"id"`
		Kind     string `json:"kind"`
		Severity string `json:"severity"`
		Urgent   bool   `json:"urgent"`
		SystemID string `json:"system_id"`
		Host     string `json:"host"`
		Text     string `json:"text"`
	} `json:"events"`
	More       int `json:"more"`
	MoreUrgent int `json:"more_urgent"`
}

// notesNew returns the `notes_new` events of an SSE body.
func notesNew(t *testing.T, body string) []notesNewEvent {
	t.Helper()
	var out []notesNewEvent
	for _, ev := range strings.Split(body, "\n\n") {
		data, ok := strings.CutPrefix(ev, "event: notes_new\ndata: ")
		if !ok {
			continue
		}
		var n notesNewEvent
		if err := json.Unmarshal([]byte(data), &n); err != nil {
			t.Fatalf("notes_new is not JSON: %v: %s", err, data)
		}
		out = append(out, n)
	}
	return out
}

// A stream says where the page's pop-ups start and then sends the warnings and critical events that are made
// while it is open: to each user the ones of their sites, never an info event, never one from before. A host
// name is an agent's word: it travels as JSON text.
func TestStreamSendsNewNotesForPopups(t *testing.T) {
	e := setupSites(t)
	old := e.s.Notes.Add(notifications.Event{T: time.Now().Add(-2 * time.Minute), Kind: "host_offline", Type: "host", Severity: notifications.Warning,
		SystemID: "laptop", Host: "laptop", Text: "offline", Key: "offline:laptop", Alarm: true})
	bodies := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for who, path := range map[string]string{subAdmin: "/api/stream", subAlice: "/api/stream?watch=laptop", subBob: "/api/stream"} {
		ck := e.cookie(person(who), 0)
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body := e.streamBody(path, "Cookie", ck)
			if code != 200 {
				t.Errorf("%s: stream %d", who, code)
			}
			mu.Lock()
			bodies[who] = body
			mu.Unlock()
		}()
	}
	time.Sleep(300 * time.Millisecond) // the streams are open
	e.s.Notes.Add(notifications.Event{Kind: "host_online", Type: "host", Severity: notifications.Info, SystemID: "laptop", Host: "laptop", Text: "back online"})
	tower := e.s.Notes.Add(notifications.Event{Kind: "host_offline", Type: "host", Severity: notifications.Warning, SystemID: "tower", Host: hostileName,
		Text: "offline · " + strings.Repeat("x", 600), Key: "offline:tower", Alarm: true})
	laptop := e.s.Notes.Add(notifications.Event{Kind: "update_failed", Type: "update", Severity: notifications.Warning, SystemID: "laptop", Host: "laptop",
		Text: `agent update failed: "</script><script>alert(2)</script>`})
	panel := e.s.Notes.Add(notifications.Event{Kind: "panel_update_failed", Type: "update", Severity: notifications.Warning, Text: "panel update failed"})
	wg.Wait()

	for who, want := range map[string][]int64{subAdmin: {tower.ID, laptop.ID, panel.ID}, subAlice: {laptop.ID}, subBob: {tower.ID}} {
		body := bodies[who]
		got := notesNew(t, body)
		if len(got) != 2 {
			t.Fatalf("%s: want the opening notes_new and one with the new events, got %d: %+v", who, len(got), got)
		}
		if first := got[0]; first.Last != old.ID || len(first.Events) != 0 || first.More != 0 {
			t.Errorf("%s: the first notes_new says where the page starts, without events: %+v", who, first)
		}
		var ids []int64
		for _, ev := range got[1].Events {
			ids = append(ids, ev.ID)
			if ev.Severity == notifications.Info || ev.ID <= old.ID {
				t.Errorf("%s: an info event or one from before: %+v", who, ev)
			}
			if ev.ID == tower.ID && (ev.Host != hostileName || !ev.Urgent || len(ev.Text) > newNoteTextMax+len("…") || !strings.HasSuffix(ev.Text, "…")) {
				t.Errorf("%s: the alert of the hostile host: %+v", who, ev)
			}
			if ev.ID == laptop.ID && (ev.Urgent || !strings.Contains(ev.Text, "</script><script>alert(2)</script>")) {
				t.Errorf("%s: the failed update: %+v", who, ev)
			}
		}
		if len(ids) != len(want) || got[1].Last != panel.ID || got[1].More != 0 {
			t.Errorf("%s: got events %v (last %d), want %v (last %d)", who, ids, got[1].Last, want, panel.ID)
		}
		for i := range want {
			if i < len(ids) && ids[i] != want[i] {
				t.Errorf("%s: got events %v, want %v", who, ids, want)
			}
		}
		// the angle brackets of an agent's words never travel as they are (they are JSON < ...)
		if i := strings.Index(body, "event: notes_new"); strings.Contains(body[i:], "<img") || strings.Contains(body[i:], "<script") {
			t.Errorf("%s: markup in the stream: %.300s", who, body[i:])
		}
	}

	admin := e.cookie(person(subAdmin), 0)
	// notes_after: go on after the notification the page saw last, but never with one older than a minute
	_, body := e.streamBody("/api/stream?notes_after=0", "Cookie", admin)
	got := notesNew(t, body)
	if len(got) != 1 || got[0].Last != panel.ID || len(got[0].Events) != 3 || got[0].Events[0].ID != tower.ID {
		t.Fatalf("notes_after=0: want the three of the last minute, not the old alarm: %+v", got)
	}
	_, body = e.streamBody("/api/stream?watch=laptop&notes_after="+itoa(laptop.ID), "Cookie", admin)
	if got := notesNew(t, body); len(got) != 1 || len(got[0].Events) != 1 || got[0].Events[0].ID != panel.ID {
		t.Fatalf("notes_after=%d: want the one after it: %+v", laptop.ID, got)
	}
	// a number above the newest id (a page that outlived the panel's list) starts from now
	_, body = e.streamBody("/api/stream?notes_after=999999", "Cookie", admin)
	if got := notesNew(t, body); len(got) != 1 || len(got[0].Events) != 0 || got[0].Last != panel.ID {
		t.Fatalf("notes_after above the newest id: %+v", got)
	}
	for _, bad := range []string{"x", "-1", "1.5", "99999999999999999999", "1%202"} {
		if r := e.do("GET", "/api/stream?notes_after="+bad, nil, "Cookie", admin); r.code != 422 {
			t.Errorf("notes_after=%s: %d", bad, r.code)
		}
	}
}

// A flood (one Proxmox API takes 60 nodes offline at once) is one notes_new with at most maxNewNotes events and
// the rest as a number.
func TestStreamNewNotesFloodIsCounted(t *testing.T) {
	e := setup(t)
	ck := e.cookie(controller(), 0)
	done := make(chan string)
	go func() { _, body := e.streamBody("/api/stream", "Cookie", ck); done <- body }()
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < 60; i++ {
		id := "n" + itoa(int64(i))
		ev := notifications.Event{Kind: "host_unstable", Type: "host", Severity: notifications.Warning, SystemID: id, Host: id, Text: "unstable", Source: "proxmox:x"}
		if i%2 == 0 {
			ev.Kind, ev.Text, ev.Key, ev.Alarm = "host_offline", "offline", "offline:"+id, true
		}
		e.s.Notes.Add(ev)
	}
	got := notesNew(t, <-done)
	if len(got) != 2 || len(got[1].Events) != maxNewNotes || got[1].More != 60-maxNewNotes || got[1].MoreUrgent != 30-maxNewNotes {
		t.Fatalf("want %d events and the rest counted: %d events, more %d (%d urgent)", maxNewNotes, len(got[1].Events), got[1].More, got[1].MoreUrgent)
	}
	for _, ev := range got[1].Events {
		if !ev.Urgent {
			t.Fatal("the urgent events go first:", ev)
		}
	}
}
