package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

var watchRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// A live-update stream holds a connection, a ticker and a copy of what it sent for as long as it is open, so
// their number is bounded: per user (all their browser tabs together) or per API token, and for the whole panel.
// The next one is answered 429 with Retry-After until one closes.
const (
	maxStreamsPerUser = 8
	maxStreamsTotal   = 200
	streamRetryAfterS = "10"
)

// streamSlots counts the open streams.
type streamSlots struct {
	mu             sync.Mutex
	perUser, total int            // the limits
	open           map[string]int // user subject or "token:<id>" -> open streams
	all            int
}

// acquire takes a slot for key; ok is false when key or the panel is at its limit. release gives it back (once).
func (s *streamSlots) acquire(key string) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.all >= s.total || s.open[key] >= s.perUser {
		return nil, false
	}
	s.all++
	s.open[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.all--
			if s.open[key]--; s.open[key] <= 0 {
				delete(s.open, key)
			}
		})
	}, true
}

// keepaliveEvery is how often a `: keepalive` comment is sent, even while data flows (proxy idle timers).
const keepaliveEvery = 15 * time.Second

// stream: Server-Sent Events, pushed every `intervals.stream` (1 s): `systems` (full, first), then
// `systems_delta` {changed, removed}; `summary`, `ups`, `power` only when they change; `ups_event` on UPS
// transitions; `notes` (the bell's counts) when they change; `notes_new` with the warnings and critical events
// made since the stream was opened (the page's pop-ups, see writeNewNotes); a `: keepalive` comment every 15 s
// regardless of traffic (proxy idle timers).
// `?watch=<id>` boosts that system to 1 s polling while the stream is open. `?notes_after=<id>` continues
// `notes_new` after that notification instead of "now" (the page gives the `last` it was sent, so that opening
// a host's page, which opens a new stream, loses no event).
//
// Everything sent is filtered to what the user may see (see view); access is re-checked every tick and the
// stream ends when it changes (the browser reconnects and gets a fresh, re-filtered `systems` list).
//
// The stream is background traffic: it never counts as use of the session (see reqCtx.active). It asks every
// tick whether the session is still valid and ends when it was revoked, reached session_max_age or idled out;
// the reconnect is answered 401, which the UI turns into the sign-in view.
func (s *Server) stream(c *reqCtx) error {
	v, err := c.view()
	if err != nil {
		return err
	}
	grant := *c.g
	watch := c.r.URL.Query().Get("watch")
	if watch != "" && (len(watch) > 80 || !watchRe.MatchString(watch)) {
		return apierr.New(422, "invalid watch id")
	}
	boost := ""
	if watch != "" && s.Poller.HasSystem(watch) && v.Has(watch) {
		boost = watch
	}
	notesAfter := int64(-1) // from now
	if q := c.r.URL.Query().Get("notes_after"); q != "" {
		if notesAfter, err = strconv.ParseInt(q, 10, 64); err != nil || notesAfter < 0 {
			return apierr.New(422, "notes_after must be a notification id")
		}
	}
	// A browser that lost the connection opens the stream again by itself: with the address the page gave it and
	// the id of the last `notes_new` it was sent (writeNewNotes). It goes on after that one, not "from now", or
	// the events made during the break would never be pop-ups. The newer of the two counts.
	if id, err := strconv.ParseInt(c.r.Header.Get("Last-Event-ID"), 10, 64); err == nil && id > notesAfter {
		notesAfter = id
	}
	release, ok := s.codec.live.streams.acquire(c.user.Sub) // the user, or "token:<id>"
	if !ok {
		c.w.Header().Set("Retry-After", streamRetryAfterS)
		return apierr.New(429, "too many live-update connections (at most "+strconv.Itoa(maxStreamsPerUser)+
			" per user or API token): close another Fairy tab")
	}
	defer release()
	c.flush()
	h := c.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	c.w.WriteHeader(200)
	rc := http.NewResponseController(c.w)
	if boost != "" {
		s.Poller.Scheduler.BoostAcquire(boost)
		defer s.Poller.Scheduler.BoostRelease(boost)
	}
	st := &streamState{s: s, v: v, upsSeq: s.Poller.UPS.Seq(), last: map[string][]byte{}, quiet: time.Now()}
	if c.user != nil {
		st.sub = c.user.Sub
	}
	if s.Notes != nil {
		st.noteAfter = s.Notes.LastID() // only events made after connecting ...
		if notesAfter >= 0 && notesAfter < st.noteAfter {
			st.noteAfter = notesAfter // ... or after the one the page saw last (never older than newNotesMaxAge)
		}
	}
	iv := time.Duration(s.Cfg.Intervals.Stream * float64(time.Second))
	if iv <= 0 {
		iv = time.Second
	}
	tick := time.NewTicker(iv)
	defer tick.Stop()
	isSession := c.user != nil && c.user.Auth == "session"
	for {
		if isSession { // revoked / expired / idle while streaming (asking changes nothing: no use is recorded)
			if why := s.sessionCheck(c.session()); why != "" {
				if why != sessRevoked { // too old, idle or the user is gone: leave sessions.json as well
					s.endSession(c.session().SID, why)
				}
				return nil
			}
		}
		if c.token != nil && !s.Tokens.ActiveID(c.token.ID) { // API token revoked / expired while streaming
			return nil
		}
		if g := s.grantOf(c, c.user); !g.Same(grant) { // role or sites changed: start over, filtered anew
			return nil
		} else if st.ticks > 0 {
			st.v = s.newView(g)
		}
		st.ticks++
		if out := st.next(); len(out) > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := c.w.Write(out); err != nil {
				return nil
			}
			if err := rc.Flush(); err != nil {
				return nil
			}
		}
		select {
		case <-c.r.Context().Done():
			return nil
		case <-tick.C:
		}
	}
}

// streamState is what one SSE client has already been sent.
type streamState struct {
	s         *Server
	v         *view // what this client may see (rebuilt every tick)
	ticks     int
	sent      map[string][]byte // system id -> JSON last sent (nil before the first `systems` event)
	sentOrder []string
	last      map[string][]byte // event name -> comparison key of the last summary / ups / power sent
	upsSeq    int               // last UPS event sent (starts at "now": only events after connecting)
	quiet     time.Time         // last keepalive
	sub       string            // the user, for the `notes` counts
	noteAfter int64             // id of the last notification considered for `notes_new`
	noteSent  bool              // the first `notes_new` (it carries `last` even without events) was sent
	idTaken   bool              // an `ups_event` put its own id on the stream: the notes id has to follow it again
}

// next returns the events due in this tick (possibly none).
func (st *streamState) next() []byte {
	var out bytes.Buffer
	st.writeSystems(&out)
	st.writeChanged(&out)
	st.writeUPSEvents(&out)
	st.writeNotes(&out)
	st.writeNewNotes(&out)
	if time.Since(st.quiet) > keepaliveEvery { // always, even while data flows
		st.quiet = time.Now()
		out.WriteString(": keepalive\n\n")
	}
	return out.Bytes()
}

// writeSystems sends the full `systems` list the first time, then `systems_delta` with the systems whose
// JSON changed and the ids that disappeared.
func (st *streamState) writeSystems(out *bytes.Buffer) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	snap, order := st.v.SystemsJSON()
	if st.sent == nil {
		out.WriteString(`event: systems` + "\n" + `data: {"generated_at":"` + now + `","systems":[`)
		for i, id := range order {
			if i > 0 {
				out.WriteByte(',')
			}
			out.Write(snap[id])
		}
		out.WriteString("]}\n\n")
	} else {
		var changed [][]byte
		for _, id := range order {
			if !bytes.Equal(st.sent[id], snap[id]) {
				changed = append(changed, snap[id])
			}
		}
		removed := []string{}
		for _, id := range st.sentOrder {
			if _, ok := snap[id]; !ok {
				removed = append(removed, id)
			}
		}
		if len(changed) > 0 || len(removed) > 0 {
			rb, _ := json.Marshal(removed) // a list of texts: cannot fail
			out.WriteString(`event: systems_delta` + "\n" + `data: {"generated_at":"` + now + `","changed":[`)
			out.Write(bytes.Join(changed, []byte(",")))
			out.WriteString(`],"removed":`)
			out.Write(rb)
			out.WriteString("}\n\n")
		}
	}
	st.sent, st.sentOrder = snap, order
}

// writeChanged sends `summary`, `ups` and `power` when their content changed. Timestamps and other fields that
// change on every read are left out of the comparison key, so an unchanged panel sends nothing.
func (st *streamState) writeChanged(out *bytes.Buffer) {
	sum := st.v.Summary()
	upsOv := st.v.UPS(false)
	pow := st.v.Power()
	st.writeEvent(out, "summary", sum)
	st.writeEvent(out, "ups", upsOv)
	st.writeEvent(out, "power", pow)
}

// writeEvent sends one of those events unless its content is what was sent last.
func (st *streamState) writeEvent(out *bytes.Buffer, name string, payload any) {
	key, err := json.Marshal(eventKey(payload))
	if err == nil && bytes.Equal(st.last[name], key) {
		return // unchanged: nothing else is encoded
	}
	// A payload that cannot be written as JSON is sent with the failing parts replaced (see reduced); an event
	// is never sent with empty or broken data.
	full, written, ok := safeJSON(name, payload)
	if !ok {
		return
	}
	if err != nil { // the comparison key of what is really sent
		if key, err = json.Marshal(eventKey(written)); err != nil {
			key = full
		}
		if bytes.Equal(st.last[name], key) {
			return
		}
	}
	st.last[name] = key
	fmt.Fprintf(out, "event: %s\ndata: %s\n\n", name, full)
}

// eventKey is what decides whether a `summary`, `ups` or `power` event is sent again: the payload without the
// fields that change on every read.
func eventKey(v any) any {
	switch x := v.(type) {
	case model.Summary:
		return withoutTime(x)
	case model.UpsOverview:
		return struct {
			U []model.UpsState            `json:"ups"`
			H map[string][]model.UpsPoint `json:"history"`
		}{x.UPS, x.History}
	case model.PowerOverview:
		return struct {
			T *float64                 `json:"total_w"`
			S []model.PowerSourceState `json:"sources"`
		}{x.TotalW, x.Sources}
	}
	return v
}

// writeUPSEvents sends every UPS transition since the last one sent, as `ups_event` with id `ups-<seq>`.
func (st *streamState) writeUPSEvents(out *bytes.Buffer) {
	for _, e := range st.s.Poller.UPS.EventsSince(st.upsSeq) {
		st.upsSeq = e.Seq
		if !st.v.all && !st.v.upsVis[e.UpsID] {
			continue
		}
		b, _, ok := safeJSON("ups_event", e)
		if !ok {
			continue
		}
		fmt.Fprintf(out, "event: ups_event\nid: ups-%d\ndata: %s\n\n", e.Seq, b)
		st.idTaken = true
	}
}

func withoutTime(s model.Summary) model.Summary {
	s.GeneratedAt = time.Time{}
	return s
}

// writeNotes sends `notes` {unread, active_alerts} (the bell and the Overview's summary) when they changed.
func (st *streamState) writeNotes(out *bytes.Buffer) {
	if st.s.Notes == nil || st.sub == "" {
		return
	}
	unread, active := st.s.Notes.Counts(st.sub, eventVisibility(st.v))
	b := []byte(fmt.Sprintf(`{"unread":%d,"active_alerts":%d}`, unread, active))
	if !bytes.Equal(st.last["notes"], b) {
		st.last["notes"] = b
		fmt.Fprintf(out, "event: notes\ndata: %s\n\n", b)
	}
}

// `notes_new`: what the page shows as pop-ups. A stream sends the warnings and critical events that were made
// while it is open: never the ones from before (no pop-ups for old events when a page loads or reconnects),
// except after `?notes_after=<id>`, and then none older than newNotesMaxAge. At most maxNewNotes events go
// into one event; a flood is counted in `more` instead (the page shows "N more").
const (
	maxNewNotes     = 20
	newNotesMaxAge  = time.Minute
	newNoteTextMax  = 240 // the pop-up shows a few lines; the whole text is on the Notifications page
	newNoteHostMax  = 80
	newNotesNothing = `{"last":%d,"events":[],"more":0,"more_urgent":0}`
)

// newNote is one event of `notes_new`: what a pop-up shows, and where its link leads (system_id, ups_id, type).
type newNote struct {
	ID       int64     `json:"id"`
	T        time.Time `json:"t"`
	Kind     string    `json:"kind"`
	Type     string    `json:"type"`
	Severity string    `json:"severity"`
	Urgent   bool      `json:"urgent"` // notifications.Event.Urgent: shown with "Critical only"
	SystemID string    `json:"system_id,omitempty"`
	UPSID    string    `json:"ups_id,omitempty"`
	Host     string    `json:"host,omitempty"`
	Text     string    `json:"text"`
}

// writeNewNotes sends `notes_new` {last, events, more, more_urgent}: once when the stream opens (no events:
// `last` is where the page's pop-ups start), then whenever a warning or critical event was made that this
// user may see. Info events and events of systems outside the user's sites are never sent. Each one is followed
// by `id: <last>` (a block of its own: it is no event, it only sets the stream's last event id), which a browser
// sends back as Last-Event-ID when it reopens the stream by itself. An `ups_event` has an id of its own
// (`ups-<seq>`, which nothing reads back): after one, the notes id is sent again, so that it stays the last.
func (st *streamState) writeNewNotes(out *bytes.Buffer) {
	if st.s.Notes == nil || st.sub == "" {
		return
	}
	evs, more, moreUrgent, last := st.s.Notes.Since(st.noteAfter, time.Now().Add(-newNotesMaxAge), eventVisibility(st.v), maxNewNotes)
	st.noteAfter = last
	if len(evs) == 0 && more == 0 {
		if !st.noteSent {
			st.noteSent = true
			fmt.Fprintf(out, "event: notes_new\ndata: "+newNotesNothing+"\n\nid: %d\n\n", last, last)
		} else if st.idTaken {
			fmt.Fprintf(out, "id: %d\n\n", last)
		}
		st.idTaken = false
		return
	}
	p := struct {
		Last       int64     `json:"last"`
		Events     []newNote `json:"events"`
		More       int       `json:"more"`
		MoreUrgent int       `json:"more_urgent"`
	}{Last: last, Events: make([]newNote, 0, len(evs)), More: more, MoreUrgent: moreUrgent}
	for _, e := range evs {
		p.Events = append(p.Events, newNoteOf(e))
	}
	b, err := json.Marshal(p)
	if err != nil { // a time that cannot be written: the events are on the Notifications page
		encodeFailed("notes_new", "", err)
		b = []byte(fmt.Sprintf(newNotesNothing, last))
	}
	st.noteSent, st.idTaken = true, false
	fmt.Fprintf(out, "event: notes_new\ndata: %s\n\nid: %d\n\n", b, last)
}

func newNoteOf(e notifications.Event) newNote {
	return newNote{ID: e.ID, T: e.T, Kind: e.Kind, Type: e.Type, Severity: e.Severity, Urgent: e.Urgent(), SystemID: e.SystemID,
		UPSID: e.UPSID, Host: clipText(e.Host, newNoteHostMax), Text: clipText(e.Text, newNoteTextMax)}
}
