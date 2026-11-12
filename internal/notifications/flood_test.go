package notifications

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

func all(Event) bool { return true }

// events returns the store's events (oldest first).
func events(s *Store) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event{}, s.d.Events...)
}

// L7, the audit's first flood: an agent that reports a new failed update on every poll (every 2 seconds) used to
// make one warning and one push message each time: 1800 an hour. Now it is one event per host per 10 minutes
// that counts its repeats, and only that one is pushed.
func TestRepeatedUpdateFailuresAreMerged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.json")
	s := Open(path)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	var pushed []string
	s.Push = func(title, _ string) { pushed = append(pushed, title) }
	fail := func(sys, msg string) Event {
		return s.Add(Event{Kind: "update_failed", Type: "update", Severity: Warning, SystemID: sys, Host: strings.ToUpper(sys),
			Text: "agent update failed: " + msg})
	}
	for i := range 1800 { // one hour, a new message every time
		fail("evil", fmt.Sprintf("attempt %d", i))
		if i == 7 { // another host fails once in between: its own event, pushed
			fail("nas", "cannot download the release")
		}
		now = now.Add(2 * time.Second)
	}
	evs := events(s)
	n := 0
	for _, e := range evs {
		if e.SystemID != "evil" {
			continue
		}
		n++
		if e.Count != 300 || !strings.HasSuffix(e.Text, " (x300)") || strings.Count(e.Text, "(x") != 1 || !strings.Contains(e.Text, "attempt ") {
			t.Fatalf("%+v", e)
		}
	}
	if n != 6 || len(evs) != 7 {
		t.Fatalf("%d events of the flooding host, %d in all", n, len(evs))
	}
	if len(pushed) != 7 {
		t.Fatalf("%d push messages: %v", len(pushed), pushed)
	}
	if p := s.List("u", all, Filter{}); p.Unread != 7 {
		t.Fatalf("unread %d", p.Unread)
	}
	// a repeat does not make a notification the user has read unread again, and returns the event it was counted on
	s.MarkRead("u", nil, true)
	first := fail("evil", "again")
	again := fail("evil", "and again")
	if p := s.List("u", all, Filter{}); p.Unread != 1 || again.ID != first.ID || again.Count != 2 || len(pushed) != 8 {
		t.Fatalf("unread %d, ids %d %d, count %d, pushed %d", p.Unread, first.ID, again.ID, again.Count, len(pushed))
	}
	// other kinds are never merged: two real events stay two
	for range 2 {
		s.Add(Event{Kind: "update_started", Type: "update", Severity: Info, SystemID: "evil", Text: "update requested"})
		s.Add(Event{Kind: "host_offline", Type: "host", Severity: Warning, SystemID: "evil", Text: "offline", Key: "offline:evil", Alarm: true})
	}
	if got := len(events(s)); got != 7+1+4 {
		t.Fatalf("%d events", got)
	}
	// the count survives a restart
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s2 := Open(path)
	s2.Now = func() time.Time { return now }
	if e := s2.Add(Event{Kind: "update_failed", Severity: Warning, SystemID: "evil", Text: "x"}); e.Count != 3 || !strings.HasSuffix(e.Text, "again (x3)") {
		t.Fatalf("%+v", e)
	}
}

// L7: a flood from one machine used to turn the whole list over (2000 events, oldest out first) and with it the
// alarms that still held: "UPS on battery" and "host offline" vanished, the active-alert count fell to zero, and
// they were not raised again. Now one host holds at most a quarter of the list, and a standing alarm is the
// last thing the list gives up.
func TestStandingAlarmsSurviveAFlood(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "n.json"))
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { now = now.Add(time.Second); return now }
	battery := s.Add(Event{Kind: "ups_on_battery", Type: "ups", Severity: Warning, UPSID: "rack/apc", Host: "APC", Text: "power lost: on battery",
		Key: "ups:rack/apc", Alarm: true})
	offline := s.Add(Event{Kind: "host_offline", Type: "host", Severity: Warning, SystemID: "nas", Host: "NAS", Text: "offline", Key: "offline:nas", Alarm: true})
	for i := range 100 { // what other hosts have in the list
		s.Add(Event{Kind: "power_shutdown", Type: "power", Severity: Info, SystemID: fmt.Sprintf("host%d", i%5), Text: "shutdown requested"})
	}
	s.MarkRead("u", []int64{battery.ID, 50}, false)
	s.Dismiss("u", []int64{60})
	for i := range 2500 { // the flood, 5000 events: one agent, a new UPS name every time (the audit's second flood), plus host events
		s.Add(Event{Kind: "ups_on_battery", Type: "ups", Severity: Warning, UPSID: fmt.Sprintf("evil/u%d", i), Host: "x", Text: "on battery",
			Key: fmt.Sprintf("ups:evil/u%d", i), Alarm: true})
		s.Add(Event{Kind: "update_started", Type: "update", Severity: Info, SystemID: "evil", Host: "EVIL", Text: "update requested"})
	}
	evs := events(s)
	byOwner := map[string]int{}
	ids := map[int64]bool{}
	for _, e := range evs {
		byOwner[e.owner()]++
		ids[e.ID] = true
	}
	if !ids[battery.ID] || !ids[offline.ID] {
		t.Fatal("a standing alarm was pushed out of the list")
	}
	if byOwner["evil"] != MaxPerHost || len(evs) != 2+100+MaxPerHost {
		t.Fatalf("the flooding host holds %d events, the list %d", byOwner["evil"], len(evs))
	}
	p := s.List("u", func(e Event) bool { return e.owner() != "evil" }, Filter{Limit: 200})
	if p.ActiveAlerts != 2 || len(p.Events) != 101 || p.Unread != 99 { // one dismissed, two read: the marks are kept too
		t.Fatalf("active alerts %d, events %d, unread %d", p.ActiveAlerts, len(p.Events), p.Unread)
	}
	// within its share the flooding host gives up its info events first, then its oldest alarms
	for _, e := range evs {
		if e.owner() == "evil" && (e.Kind != "ups_on_battery" || e.ID < 4000) { // ids 103 to 5102; the newest 500 alarms stay
			t.Fatalf("kept %+v", e)
		}
	}
	// the "back to normal" event still resolves the alarm it belongs to
	s.Add(Event{Kind: "ups_power_restored", Type: "ups", Severity: Info, UPSID: "rack/apc", Host: "APC", Text: "power restored", Key: "ups:rack/apc"})
	if p := s.List("u", func(e Event) bool { return e.owner() != "evil" }, Filter{}); p.ActiveAlerts != 1 {
		t.Fatalf("active alerts %d", p.ActiveAlerts)
	}
}

// The list as a whole: beyond MaxEvents it gives up info events and alarms that are over first (oldest first),
// then other warnings, and alarms that still hold only when nothing else is left. Marks of events that are gone
// are forgotten, the others stay.
func TestFullListGivesUpTheLeastImportantFirst(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "n.json"))
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { now = now.Add(time.Second); return now }
	host := func(i int) string { return fmt.Sprintf("h%d", i%20) } // 20 hosts: none reaches its own share
	var alarms, warnings, over []int64
	for i := range 40 {
		alarms = append(alarms, s.Add(Event{Kind: "disk_failing", Severity: Warning, SystemID: host(i), Text: "failing", Key: fmt.Sprintf("disk:%d", i), Alarm: true}).ID)
	}
	for i := range 40 {
		warnings = append(warnings, s.Add(Event{Kind: "host_unstable", Severity: Warning, SystemID: host(i), Text: "unstable"}).ID)
	}
	for i := range 40 { // alarms that are over
		id := s.Add(Event{Kind: "host_offline", Severity: Warning, SystemID: host(i), Text: "offline", Key: fmt.Sprintf("offline:%d", i), Alarm: true}).ID
		s.Add(Event{Kind: "host_online", Severity: Info, SystemID: host(i), Text: "back online", Key: fmt.Sprintf("offline:%d", i)})
		over = append(over, id)
	}
	s.MarkRead("u", []int64{alarms[0], over[0]}, false)
	s.Dismiss("u", []int64{warnings[0], over[1]})
	for i := range MaxEvents { // more info events than the list holds
		s.Add(Event{Kind: "power_reboot", Severity: Info, SystemID: host(i), Text: "reboot requested"})
	}
	evs := events(s)
	if len(evs) != MaxEvents {
		t.Fatalf("%d events", len(evs))
	}
	ids := map[int64]bool{}
	for _, e := range evs {
		ids[e.ID] = true
	}
	for _, id := range append(append([]int64{}, alarms...), warnings...) {
		if !ids[id] {
			t.Fatalf("event %d (an alarm that holds, or a warning) was given up before info events", id)
		}
	}
	for _, id := range over {
		if ids[id] {
			t.Fatalf("event %d (an alarm that is over) outlived newer events", id)
		}
	}
	s.mu.Lock()
	u := s.d.Users["u"]
	read, dismissed := fmt.Sprint(u.Read), fmt.Sprint(u.Dismissed)
	s.mu.Unlock()
	if read != fmt.Sprint([]int64{alarms[0]}) || dismissed != fmt.Sprint([]int64{warnings[0]}) {
		t.Fatalf("marks: read %s dismissed %s", read, dismissed)
	}
	// now warnings fill it: the info events go, then the oldest warnings; the alarms that hold are still there
	for i := range MaxEvents {
		s.Add(Event{Kind: "host_unstable", Severity: Critical, SystemID: host(i), Text: "unstable"})
	}
	evs = events(s)
	active := 0
	for _, e := range evs {
		if e.Active() {
			active++
		}
		if e.Severity == Info {
			t.Fatalf("an info event outlived warnings: %+v", e)
		}
	}
	if len(evs) != MaxEvents || active != 40 || evs[0].ID != alarms[0] {
		t.Fatalf("%d events, %d active alarms", len(evs), active)
	}
	// age: events older than MaxAge go, an alarm that still holds stays
	now = now.Add(MaxAge + time.Hour)
	s.Add(Event{Kind: "x", Text: "new"})
	if evs = events(s); len(evs) != 41 || !evs[0].Active() || evs[40].Kind != "x" {
		t.Fatalf("%d events after %s", len(evs), MaxAge)
	}
}

// The watcher's UPS events count towards the share of the UPS's source, the host events towards the host's.
func TestEventOwner(t *testing.T) {
	for _, tc := range []struct {
		e    Event
		want string
	}{{Event{SystemID: "nas"}, "nas"}, {Event{UPSID: "rack/apc"}, "rack"}, {Event{UPSID: "agent1/cp"}, "agent1"},
		{Event{SystemID: "node-a", UPSID: "rack/apc"}, "node-a"}, {Event{UPSID: "odd"}, "odd"}, {Event{Host: "Fairy panel"}, ""}} {
		if got := tc.e.owner(); got != tc.want {
			t.Errorf("%+v: %q", tc.e, got)
		}
	}
	w := &Watcher{Store: Open(filepath.Join(t.TempDir(), "n.json"))}
	w.Observe(time.Now(), nil, nil)
	w.Observe(time.Now(), nil, []model.UpsEvent{{UpsID: "agent1/cp", UpsName: "CyberPower", Kind: "on_battery", Severity: "warning", Message: "power lost"}})
	if evs := events(w.Store); len(evs) != 1 || evs[0].owner() != "agent1" || !evs[0].Active() {
		t.Fatalf("%+v", evs)
	}
}

// r2-p3-01 (audit 5): a panel that starts while its UPS server is down. The watcher's first look only learns
// the state and makes no events, so the "cannot be reached" of the very first poll was dropped. That one kind
// passes at the first look too; everything else at start is still only learned.
func TestAUPSServerDownAtStartIsAnAlert(t *testing.T) {
	w := &Watcher{Store: Open(filepath.Join(t.TempDir(), "n.json"))}
	w.Observe(time.Now(), nil, []model.UpsEvent{
		{UpsID: "rack/apc", UpsName: "APC", Kind: "on_battery", Severity: "warning", Message: "power lost"}, // a state at start: learned, not announced
		{UpsID: "dead", UpsName: "Rack UPS", Kind: "communication_lost", Severity: "warning", Message: "the UPS server has not given the panel a UPS since the panel started (connection failed)"},
	})
	evs := events(w.Store)
	if len(evs) != 1 || evs[0].Kind != "ups_communication_lost" || evs[0].UPSID != "dead" || evs[0].Host != "Rack UPS" || !evs[0].Active() || !evs[0].Urgent() {
		t.Fatalf("%+v", evs)
	}
	// the server answers later: the alert ends
	w.Observe(time.Now(), nil, []model.UpsEvent{{UpsID: "dead", UpsName: "Rack UPS", Kind: "communication_restored", Severity: "info", Message: "the UPS server answers"}})
	active := 0
	for _, e := range events(w.Store) {
		if e.Active() {
			active++
		}
	}
	if active != 0 {
		t.Fatalf("the alert must end when the server answers: %+v", events(w.Store))
	}
}
