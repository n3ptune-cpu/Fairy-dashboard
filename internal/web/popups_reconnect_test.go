package web

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// r2-b1 I-1: a critical pop-up was missed while the bell counted the event. A browser that loses the live
// stream opens it again by itself, with the address the page gave it at the start (no notes_after) and the id of
// the last event it was sent (Last-Event-ID). The panel started that stream "from now", so the events made
// during the break were never pop-ups. Each `notes_new` now carries its `last` as the stream's event id, and a
// stream opened with Last-Event-ID goes on after it.
func TestAStreamTheBrowserReopensGoesOnAfterTheLastNotesNew(t *testing.T) {
	e := setupSites(t)
	admin := e.cookie(person(subAdmin), 0)
	before := e.s.Notes.Add(notifications.Event{Kind: "host_offline", Type: "host", Severity: notifications.Warning,
		SystemID: "laptop", Host: "laptop", Text: "offline", Key: "offline:laptop", Alarm: true})

	// the page's first stream: where its pop-ups start, and the same number as the stream's event id
	_, body := e.streamBody("/api/stream", "Cookie", admin)
	got := notesNew(t, body)
	if len(got) != 1 || got[0].Last != before.ID || len(got[0].Events) != 0 {
		t.Fatalf("first stream: %+v", got)
	}
	if !strings.Contains(body, "\n\nid: "+itoa(before.ID)+"\n\n") {
		t.Fatalf("the first notes_new is not followed by its id: %.400s", body)
	}

	// the connection breaks; a UPS goes on battery; the browser opens the stream again
	during := e.s.Notes.Add(notifications.Event{Kind: "ups_on_battery", Type: "ups", Severity: notifications.Warning,
		UPSID: "rack/rack", Host: "Rack UPS", Text: "power lost: on battery", Key: "ups:rack/rack", Alarm: true})
	_, body = e.streamBody("/api/stream", "Cookie", admin, "Last-Event-ID", itoa(before.ID))
	got = notesNew(t, body)
	if len(got) != 1 || len(got[0].Events) != 1 || got[0].Events[0].ID != during.ID || !got[0].Events[0].Urgent || got[0].Last != during.ID {
		t.Fatalf("reopened with Last-Event-ID %d: want the event made during the break (%d): %+v", before.ID, during.ID, got)
	}
	if !strings.Contains(body, "\n\nid: "+itoa(during.ID)+"\n\n") {
		t.Errorf("the notes_new with the event is not followed by its id: %.400s", body)
	}

	// the newer of the two counts: the address still says where the page started, the header what it saw last
	_, body = e.streamBody("/api/stream?notes_after="+itoa(before.ID-1), "Cookie", admin, "Last-Event-ID", itoa(during.ID))
	if got := notesNew(t, body); len(got) != 1 || len(got[0].Events) != 0 || got[0].Last != during.ID {
		t.Errorf("notes_after below Last-Event-ID: nothing is sent twice: %+v", got)
	}
	// a UPS event has an id of its own on the stream: the notes id follows it, so that it is the one the browser keeps
	ups := e.s.Poller.UPS
	ups.UpdateFromAgent("laptop", []map[string]any{{"name": "apc", "state": "online"}}, false, nil, nil)
	done := make(chan string, 1)
	go func() { _, b := e.streamBody("/api/stream", "Cookie", admin); done <- b }()
	time.Sleep(400 * time.Millisecond) // the stream is open: its UPS events start now
	ups.UpdateFromAgent("laptop", []map[string]any{{"name": "apc", "state": "on_battery"}}, false, nil, nil)
	body = <-done
	i := strings.LastIndex(body, "\nid: ")
	if j := strings.Index(body, "event: ups_event\nid: ups-"); j < 0 || i < j || !strings.HasPrefix(body[i:], "\nid: "+itoa(e.s.Notes.LastID())+"\n\n") {
		t.Errorf("after an ups_event (at %d) the last id of the stream (at %d) is not the notes id %d: %.300s", j, i, e.s.Notes.LastID(), body[max(i, 0):])
	}

	// without the header nothing changes: a new page starts from now; and a header that is no number is not an error
	for _, h := range []string{"abc", "-5"} {
		code, body := e.streamBody("/api/stream", "Cookie", admin, "Last-Event-ID", h)
		if got := notesNew(t, body); code != 200 || len(got) != 1 || len(got[0].Events) != 0 || got[0].Last != e.s.Notes.LastID() {
			t.Errorf("Last-Event-ID %q: %d %+v", h, code, got)
		}
	}
}
