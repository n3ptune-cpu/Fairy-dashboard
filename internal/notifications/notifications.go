// Package notifications is the panel's Notifications page: a bounded list of events (hosts going offline and
// coming back, temperatures over the limit and back, UPS power events, power actions, updates, Add host) with a
// per-user read / dismissed state, stored in the data folder (notifications.json). Events are made on state
// CHANGES (see Watcher), never on every poll.
package notifications

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Severity values.
const (
	Info     = "info"
	Warning  = "warning"
	Critical = "critical"
)

// Limits: at most MaxEvents, none older than MaxAge (except alarms that still hold). One host, or one UPS
// source, holds at most MaxPerHost of them: a machine that produces events without end uses up its own share,
// not the list.
const (
	MaxEvents  = 2000
	MaxAge     = 30 * 24 * time.Hour
	MaxPerHost = MaxEvents / 4
	// RepeatWindow: a repeat of a merged kind (see merged) within this time of the first one is counted on that
	// event instead of becoming a new one.
	RepeatWindow = 10 * time.Minute
)

// merged are the kinds a monitored machine can cause again and again by what it reports: a new failed update
// on every poll, another version on every poll (N2: 500 "agent now runs X" in 17 minutes), a temperature that
// goes over the limit and back every few seconds (N2: 150 pushed warnings an hour). Per host there is one such
// event per RepeatWindow; repeats raise its Count ("x7" in the text), are not pushed and are not new unread
// notifications. A repeated alarm (temp_high) holds again when it repeats, and the event that ends it (temp_ok)
// ends it again, so the list always shows the state the host is in now.
var merged = map[string]bool{"update_failed": true, "update_done": true, "temp_high": true, "temp_ok": true}

// Push limits: one source (a machine, a Proxmox API with all its nodes and guests, a UPS source) gets at most
// MaxPushPerSource push messages per PushWindow. The first event over the limit is pushed as one line that says
// so; the rest are counted and named with the source's next pushed event. Every event is still on the
// Notifications page. Events of the panel itself are not limited.
const (
	MaxPushPerSource = 5
	PushWindow       = time.Minute
)

// Event is one notification.
type Event struct {
	ID       int64      `json:"id"`
	T        time.Time  `json:"t"`
	Kind     string     `json:"kind"` // host_offline, host_online, host_off, temp_high, temp_ok, ups_*, power_*, update_*, host_added, ...
	Type     string     `json:"type"` // host, temperature, ups, power, update, enroll
	Severity string     `json:"severity"`
	SystemID string     `json:"system_id,omitempty"`
	UPSID    string     `json:"ups_id,omitempty"`
	Host     string     `json:"host,omitempty"` // display name at the time
	Text     string     `json:"text"`
	Key      string     `json:"key,omitempty"` // an alarm's condition (offline:<id>); cleared by the matching "back" event
	Alarm    bool       `json:"alarm,omitempty"`
	Resolved *time.Time `json:"resolved,omitempty"`
	// Count is how often the event happened when repeats were merged into it (2 and up; see merged). The text
	// ends in " (x<Count>)".
	Count int `json:"count,omitempty"`
	// Source is whose events this one counts with when that is more than the one system: "proxmox:<cluster>"
	// for a node or guest the panel knows from a Proxmox API. All of them share one share of the list and one
	// push limit, however many systems that API lists.
	Source string `json:"source,omitempty"`
}

// owner is whose share of the list (and whose push limit) an event counts towards: its Source when it has one
// (a Proxmox API), else its system, or for a UPS event the UPS's source (the part of the UPS id before the
// slash: a configured UPS source or the agent that relays it). "" for events of the panel itself, which have
// no limit of their own.
func (e Event) owner() string {
	if e.Source != "" {
		return e.Source
	}
	if e.SystemID != "" {
		return e.SystemID
	}
	if i := strings.IndexByte(e.UPSID, '/'); i > 0 {
		return e.UPSID[:i]
	}
	return e.UPSID
}

// tier says what is given up first when the list is full: 0 info events and alarms that are over, 1 other
// warnings, 2 alarms that still hold (only when nothing else is left).
func (e Event) tier() int {
	switch {
	case e.Active():
		return 2
	case e.Alarm || e.Severity == Info || e.Severity == "":
		return 0
	}
	return 1
}

// Active reports whether an alarm's condition still holds.
func (e Event) Active() bool { return e.Alarm && e.Resolved == nil }

// Urgent reports whether the event is one a pop-up shows with the setting "Critical only" (README,
// "Notifications"): an event of severity critical (today only "UPS: low battery"), or one that starts an
// alert, a condition that then holds (a host offline, a temperature over the limit, a UPS on battery or out
// of reach). These are the events the Overview counts as "active alerts". Other warnings (an update that
// failed, an unstable host, Add host that failed) are not urgent.
func (e Event) Urgent() bool { return e.Severity == Critical || e.Alarm }

// severity reports whether the event is in the filter's severity (see Filter.Severity).
func (f Filter) severity(e Event) bool {
	switch f.Severity {
	case "":
		return true
	case Critical:
		return e.Urgent()
	case Warning:
		return e.Severity == Warning && !e.Urgent()
	}
	return e.Severity == f.Severity
}

type userState struct {
	ReadUpTo  int64   `json:"read_up_to"`
	Read      []int64 `json:"read,omitempty"`
	Dismissed []int64 `json:"dismissed,omitempty"`
}

type fileData struct {
	Next   int64                 `json:"next"`
	Events []Event               `json:"events"`
	Users  map[string]*userState `json:"users"`
}

// Store holds the events (oldest first) and who has read or dismissed which.
type Store struct {
	path string
	// Push sends warnings and critical events to FAIRY_NOTIFY_URL (ntfy / Discord); may be nil.
	Push func(title, message string)
	Now  func() time.Time

	mu     sync.Mutex
	d      fileData
	dirty  bool
	subs   map[chan struct{}]bool
	pushes map[string]*pushCount // push messages per source in the current window (see MaxPushPerSource)
}

// pushCount is one source's push messages in its current window.
type pushCount struct {
	start time.Time
	sent  int // pushed in this window
	held  int // not pushed in this window (over the limit)
}

// pushFor decides what is pushed for an event of owner: the event itself (ok), with a note about the events of
// this source that were not pushed before; or, for the first event over the limit, one line that says that
// this source is over it (notice); or nothing. Caller holds s.mu.
func (s *Store) pushFor(owner string, now time.Time) (ok bool, note, notice string) {
	if owner == "" {
		return true, "", ""
	}
	if s.pushes == nil {
		s.pushes = map[string]*pushCount{}
	}
	if len(s.pushes) > 4096 { // sources that were quiet for a window: forget them
		for k, c := range s.pushes {
			if now.Sub(c.start) >= PushWindow && c.held == 0 {
				delete(s.pushes, k)
			}
		}
	}
	c := s.pushes[owner]
	if c == nil {
		c = &pushCount{start: now}
		s.pushes[owner] = c
	}
	if now.Sub(c.start) >= PushWindow {
		if c.held > 0 {
			note = fmt.Sprintf(" (and %d more from this source that were not pushed: see the Notifications page)", c.held)
		}
		c.start, c.sent, c.held = now, 0, 0
	}
	if c.sent < MaxPushPerSource {
		c.sent++
		return true, note, ""
	}
	c.held++
	if c.held == 1 {
		notice = fmt.Sprintf("more than %d events in a minute from %s: the next ones are not pushed, they are on the Notifications page",
			MaxPushPerSource, owner)
	}
	return false, "", notice
}

// Open loads path (a missing file is empty). A corrupt file is moved aside (notifications are not security
// state) and the list starts empty.
func Open(path string) *Store {
	s := &Store{path: path, Now: time.Now, d: fileData{Next: 1, Users: map[string]*userState{}}, subs: map[chan struct{}]bool{}}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		slog.Error("notifications: cannot read the list; starting empty", "path", path, "error", err)
	default:
		var d fileData
		if err := json.Unmarshal(b, &d); err != nil {
			slog.Error("notifications: corrupt list moved aside; starting empty", "path", path, "error", err)
			_ = os.Rename(path, path+".corrupt")
		} else {
			if d.Users == nil {
				d.Users = map[string]*userState{}
			}
			if d.Next < 1 {
				d.Next = 1
			}
			s.d = d
		}
	}
	return s
}

// Subscribe returns a channel that gets a (non-blocking) signal on every new event; cancel with the func.
func (s *Store) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs[ch] = true
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}

// Add records an event (ID and time filled in) and returns it. An event with a Key resolves the open alarms
// with the same key when it is not an alarm itself (the "back to normal" event). A repeat of a merged kind is
// counted on the event it repeats, which is returned instead (see merged).
func (s *Store) Add(e Event) Event {
	s.mu.Lock()
	now := s.Now().UTC()
	if e.T.IsZero() {
		e.T = now
	}
	if e.Severity == "" {
		e.Severity = Info
	}
	resolve := func() { // an event with a Key that is not an alarm ends the alarms that hold under that key
		if e.Key == "" || e.Alarm {
			return
		}
		for i := range s.d.Events {
			if x := &s.d.Events[i]; x.Key == e.Key && x.Active() {
				t := e.T
				x.Resolved = &t
			}
		}
	}
	if merged[e.Kind] {
		for i := len(s.d.Events) - 1; i >= 0; i-- {
			x := &s.d.Events[i]
			if x.Kind != e.Kind || x.SystemID != e.SystemID || x.UPSID != e.UPSID {
				continue
			}
			if d := now.Sub(x.T); d < 0 || d >= RepeatWindow {
				break // the latest one is too old: this is a new event
			}
			if x.Count >= 2 {
				x.Text = strings.TrimSuffix(x.Text, fmt.Sprintf(" (x%d)", x.Count))
			}
			x.Count = max(x.Count, 1) + 1
			x.Text += fmt.Sprintf(" (x%d)", x.Count)
			if x.Alarm {
				x.Resolved = nil // the condition is back: the alarm holds again
			}
			resolve()
			out := *x
			s.dirty = true
			s.signal()
			s.mu.Unlock()
			return out // counted, not pushed again
		}
	}
	resolve()
	e.ID = s.d.Next
	s.d.Next++
	s.d.Events = append(s.d.Events, e)
	s.trim(now, e.owner())
	s.dirty = true
	s.signal()
	push := s.Push
	var ok bool
	var note, notice string
	if push != nil && (e.Severity == Warning || e.Severity == Critical) {
		ok, note, notice = s.pushFor(e.owner(), now)
	}
	s.mu.Unlock()
	if ok {
		title := "Fairy: " + e.Text
		if e.Host != "" {
			title = "Fairy: " + e.Host + ": " + e.Text
		}
		push(title, fmt.Sprintf("%s (%s) at %s%s", e.Text, e.Severity, e.T.Format(time.RFC3339), note))
	} else if notice != "" {
		push("Fairy: "+notice, fmt.Sprintf("%s (%s) at %s", notice, Warning, e.T.Format(time.RFC3339)))
	}
	return e
}

// signal tells the subscribers that the list changed. Caller holds s.mu.
func (s *Store) signal() {
	for ch := range s.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// trim keeps the list within its limits after an event of owner was added. Caller holds s.mu.
//
// Events older than MaxAge go, the source (a host, a Proxmox API, a UPS source) that just added one gives up
// its own events beyond MaxPerHost, and beyond MaxEvents the list gives up one event at a time. What goes first
// is decided by Event.tier: info events and alarms that are over, then other warnings; an alarm that still holds
// ("UPS on battery", "host offline") is only given up when nothing else is left, because it is not raised again
// while its condition lasts. Within a tier the source with the most events gives up its oldest (evictFair). So a
// flood of events from one source, or from several, cannot push a standing alarm of another one out of the list.
func (s *Store) trim(now time.Time, owner string) {
	n := len(s.d.Events)
	old, aged := 0, 0 // events are in time order: the ones past MaxAge are at the front
	for ; old < n && now.Sub(s.d.Events[old].T) > MaxAge; old++ {
		if !s.d.Events[old].Active() {
			aged++
		}
	}
	if aged > 0 {
		kept := make([]Event, 0, n-aged)
		for i, e := range s.d.Events {
			if i >= old || e.Active() {
				kept = append(kept, e)
			}
		}
		s.d.Events = kept
	}
	if owner != "" {
		mine := 0
		for _, e := range s.d.Events {
			if e.owner() == owner {
				mine++
			}
		}
		for ; mine > MaxPerHost; mine-- {
			s.evict(func(e Event) bool { return e.owner() == owner })
		}
	}
	for len(s.d.Events) > MaxEvents {
		s.evictFair()
	}
	if len(s.d.Events) == n {
		return
	}
	present := make(map[int64]bool, len(s.d.Events))
	for _, e := range s.d.Events {
		present[e.ID] = true
	}
	for _, u := range s.d.Users { // forget read / dismissed marks of events that are gone
		u.Read = slices.DeleteFunc(u.Read, func(id int64) bool { return !present[id] })
		u.Dismissed = slices.DeleteFunc(u.Dismissed, func(id int64) bool { return !present[id] })
	}
}

// evict removes one of the events that match: the oldest of the lowest tier (see Event.tier). Caller holds s.mu.
func (s *Store) evict(match func(Event) bool) {
	at, tier := -1, 3
	for i, e := range s.d.Events {
		if t := e.tier(); t < tier && match(e) {
			at, tier = i, t
			if t == 0 {
				break
			}
		}
	}
	if at >= 0 {
		s.d.Events = slices.Delete(s.d.Events, at, at+1)
	}
}

// evictFair removes one event from a full list: of the lowest tier that is there, the oldest event of the
// source that holds the most events of that tier. So when the list is full of alarms that still hold, a
// source with many of them gives up its own, and one alarm of another source is not what goes. (Events of
// the panel itself count as one source.) Caller holds s.mu.
func (s *Store) evictFair() {
	tier := 3
	for _, e := range s.d.Events {
		if t := e.tier(); t < tier {
			tier = t
		}
	}
	count := map[string]int{}
	first := map[string]int{}
	for i, e := range s.d.Events {
		if e.tier() != tier {
			continue
		}
		o := e.owner()
		if _, ok := first[o]; !ok {
			first[o] = i
		}
		count[o]++
	}
	at, most := -1, 0
	for o, n := range count {
		if n > most || (n == most && first[o] < at) {
			at, most = first[o], n
		}
	}
	if at >= 0 {
		s.d.Events = slices.Delete(s.d.Events, at, at+1)
	}
}

// HasKey reports whether any event carries key (one-off events, e.g. a finished panel update, are made once).
func (s *Store) HasKey(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.d.Events {
		if e.Key == key {
			return true
		}
	}
	return false
}

// Filter narrows List.
type Filter struct {
	// Severity: info | warning | critical | "" (all). The words are the page's (Settings, the pop-ups): "critical"
	// is every urgent event (Event.Urgent: severity critical, or one that starts an alert, such as a host that
	// goes offline), "warning" the other warnings. Every event is in exactly one of the three.
	Severity string
	Type     string
	Host     string // system id
	Unread   bool
	Before   int64 // ids below this (paging); 0 = newest
	Limit    int
}

// View is an event as one user sees it.
type View struct {
	Event
	Read   bool `json:"read"`
	Active bool `json:"active"`
}

// Page is GET /api/notifications.
type Page struct {
	Events       []View `json:"events"`
	Unread       int    `json:"unread"`
	ActiveAlerts int    `json:"active_alerts"`
	Next         *int64 `json:"next"`
}

func (u *userState) read(id int64) bool {
	return id <= u.ReadUpTo || slices.Contains(u.Read, id)
}

// List is a user's view of the events, newest first. visible decides what the user may see (sites, roles).
func (s *Store) List(sub string, visible func(Event) bool, f Filter) Page {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.d.Users[sub]
	if u == nil {
		u = &userState{}
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	p := Page{Events: []View{}}
	for i := len(s.d.Events) - 1; i >= 0; i-- {
		e := s.d.Events[i]
		if slices.Contains(u.Dismissed, e.ID) || !visible(e) {
			continue
		}
		read := u.read(e.ID)
		if !read {
			p.Unread++
		}
		if e.Active() {
			p.ActiveAlerts++
		}
		if (f.Before > 0 && e.ID >= f.Before) || !f.severity(e) || (f.Type != "" && e.Type != f.Type) ||
			(f.Host != "" && e.SystemID != f.Host) || (f.Unread && read) {
			continue
		}
		if len(p.Events) == f.Limit {
			if p.Next == nil {
				id := p.Events[len(p.Events)-1].ID
				p.Next = &id
			}
			continue
		}
		p.Events = append(p.Events, View{Event: e, Read: read, Active: e.Active()})
	}
	return p
}

// Counts is the bell's number (unread) and the Overview's (active alerts) for a user.
func (s *Store) Counts(sub string, visible func(Event) bool) (unread, active int) {
	p := s.List(sub, visible, Filter{Limit: 1})
	return p.Unread, p.ActiveAlerts
}

// LastID is the id of the newest event made so far (0 when there was none).
func (s *Store) LastID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Next - 1
}

// Since returns the warnings and critical events with an id above after that visible allows, oldest first:
// what an open page shows as pop-ups (the live stream's `notes_new`). last is the newest id there is, to ask
// with next time. Events made before notBefore are left out (a page that was away for longer finds them on
// the Notifications page), and so are info events. At most limit events are returned: urgent ones first (see
// Event.Urgent), then the newest; more says how many were left out and moreUrgent how many of those were
// urgent. A repeat that was counted on an older event (see merged) is not a new event and is not returned.
func (s *Store) Since(after int64, notBefore time.Time, visible func(Event) bool, limit int) (evs []Event, more, moreUrgent int, last int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	last = s.d.Next - 1
	for i := len(s.d.Events) - 1; i >= 0; i-- { // ids rise along the list
		e := s.d.Events[i]
		if e.ID <= after {
			break
		}
		if (e.Severity != Warning && e.Severity != Critical) || e.T.Before(notBefore) || !visible(e) {
			continue
		}
		evs = append(evs, e)
	}
	if limit < 0 {
		limit = 0
	}
	if len(evs) > limit {
		slices.SortStableFunc(evs, func(a, b Event) int { // urgent first; within each, the newest first (as collected)
			switch {
			case a.Urgent() == b.Urgent():
				return 0
			case a.Urgent():
				return -1
			}
			return 1
		})
		for _, e := range evs[limit:] {
			more++
			if e.Urgent() {
				moreUrgent++
			}
		}
		evs = evs[:limit]
	}
	slices.SortFunc(evs, func(a, b Event) int { return cmp.Compare(a.ID, b.ID) })
	return evs, more, moreUrgent, last
}

func (s *Store) user(sub string) *userState {
	u := s.d.Users[sub]
	if u == nil {
		u = &userState{}
		s.d.Users[sub] = u
	}
	return u
}

// MarkRead marks events read for a user; all marks every event up to now.
func (s *Store) MarkRead(sub string, ids []int64, all bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.user(sub)
	if all {
		u.ReadUpTo, u.Read = s.d.Next-1, nil
	} else {
		for _, id := range ids {
			if id > u.ReadUpTo && id < s.d.Next && !slices.Contains(u.Read, id) {
				u.Read = append(u.Read, id)
			}
		}
		if len(u.Read) > MaxEvents {
			u.Read = u.Read[len(u.Read)-MaxEvents:]
		}
	}
	s.dirty = true
}

// Dismiss hides events from a user's list (they stay for others and in the file until they age out).
func (s *Store) Dismiss(sub string, ids []int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.user(sub)
	for _, id := range ids {
		if id < s.d.Next && !slices.Contains(u.Dismissed, id) {
			u.Dismissed = append(u.Dismissed, id)
		}
	}
	if len(u.Dismissed) > MaxEvents {
		u.Dismissed = u.Dismissed[len(u.Dismissed)-MaxEvents:]
	}
	s.dirty = true
}

// Flush writes the list when it changed (atomically, 0600).
func (s *Store) Flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	b, err := json.Marshal(s.d)
	s.dirty = false
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Types and severities the page filters by.
var (
	Types      = []string{"host", "temperature", "ups", "power", "update", "enroll"}
	Severities = []string{Info, Warning, Critical}
)

// ValidType reports whether t is a known type ("" too).
func ValidType(t string) bool { return t == "" || slices.Contains(Types, t) }

// ValidSeverity reports whether v is a known severity ("" too).
func ValidSeverity(v string) bool { return v == "" || slices.Contains(Severities, v) }

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
