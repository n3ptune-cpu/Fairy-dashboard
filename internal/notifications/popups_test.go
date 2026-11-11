package notifications

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// Which events a pop-up shows with "Critical only" (Event.Urgent), made the way the panel makes them: by the
// watcher from host states and UPS events. A change here changes what pops up for everyone with the default
// setting, and the README's list.
func TestWhichEventsAreUrgent(t *testing.T) {
	st := Open(filepath.Join(t.TempDir(), "n.json"))
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	st.Now = func() time.Time { return now }
	w := &Watcher{Store: st}
	host := func(status string, temp float64) []model.System {
		return []model.System{{ID: "nas", Name: "NAS", Kind: "agent", Status: status, Temps: []model.Temperature{{Sensor: "cpu", CurrentC: temp}}}}
	}
	hold := func(sys []model.System, ups ...model.UpsEvent) { // the state holds for a minute
		for i := 0; i < 30; i++ {
			now = now.Add(2 * time.Second)
			w.Observe(now, sys, ups)
			ups = nil
		}
	}
	w.Observe(now, host("online", 40), nil)
	hold(host("online", 95))
	hold(host("online", 50))
	hold(host("offline", 0))
	hold(host("online", 50))
	for _, k := range [][2]string{{"on_battery", Warning}, {"low_battery", Critical}, {"power_restored", Info}, {"communication_lost", Warning},
		{"communication_restored", Info}, {"replace_battery", Warning}, {"overload", Warning}} {
		hold(host("online", 50), model.UpsEvent{UpsID: "nas/apc", UpsName: "APC", Kind: k[0], Severity: k[1], Message: k[0]})
	}
	st.Add(Event{Kind: "update_failed", Type: "update", Severity: Warning, SystemID: "nas", Text: "agent update failed"})
	st.Add(Event{Kind: "host_unstable", Type: "host", Severity: Warning, SystemID: "nas", Text: "unstable"})
	st.Add(Event{Kind: "pairing_failed", Type: "enroll", Severity: Warning, Text: "Add host failed"})
	st.Add(Event{Kind: "helper_stopped", Type: "host", Severity: Info, SystemID: "nas", Text: "elevated helper stopped by x"})

	got := map[string]string{}
	for _, e := range st.List("u", all, Filter{Limit: 200}).Events {
		got[e.Kind] = fmt.Sprintf("%s urgent=%v", e.Severity, e.Urgent())
	}
	want := map[string]string{
		// "Critical only" shows these
		"ups_low_battery": "critical urgent=true", "host_offline": "warning urgent=true", "temp_high": "warning urgent=true",
		"ups_on_battery": "warning urgent=true", "ups_communication_lost": "warning urgent=true",
		// "Critical and warnings" shows these too
		"update_failed": "warning urgent=false", "host_unstable": "warning urgent=false", "pairing_failed": "warning urgent=false",
		"ups_replace_battery": "warning urgent=false", "ups_overload": "warning urgent=false",
		// never a pop-up
		"host_online": "info urgent=false", "temp_ok": "info urgent=false", "ups_power_restored": "info urgent=false",
		"ups_communication_restored": "info urgent=false", "helper_stopped": "info urgent=false",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("events made: %v", got)
	}
}

// Since is what the live stream asks every second for a page's pop-ups.
func TestSinceForPopups(t *testing.T) {
	st := Open(filepath.Join(t.TempDir(), "n.json"))
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	st.Now = func() time.Time { return now }
	recent := func() time.Time { return now.Add(-time.Minute) }
	if st.LastID() != 0 {
		t.Fatal("an empty list has no last id")
	}
	if evs, more, _, last := st.Since(0, recent(), all, 20); len(evs) != 0 || more != 0 || last != 0 {
		t.Fatal("empty", evs, more, last)
	}
	old := st.Add(Event{Kind: "host_offline", Severity: Warning, SystemID: "nas", Text: "offline", Key: "offline:nas", Alarm: true})
	if st.LastID() != old.ID {
		t.Fatal("last id", st.LastID())
	}
	// nothing from before the id that is asked for: a page that opens now gets no pop-up for the old alarm
	if evs, _, _, last := st.Since(st.LastID(), recent(), all, 20); len(evs) != 0 || last != old.ID {
		t.Fatal("events from before", evs, last)
	}
	now = now.Add(10 * time.Second)
	st.Add(Event{Kind: "host_online", Severity: Info, SystemID: "nas", Text: "back online", Key: "offline:nas"}) // info: never
	hidden := st.Add(Event{Kind: "host_offline", Severity: Warning, SystemID: "secret", Text: "offline", Key: "offline:secret", Alarm: true})
	crit := st.Add(Event{Kind: "ups_low_battery", Severity: Critical, UPSID: "nas/apc", Text: "LOW BATTERY"})
	fail := st.Add(Event{Kind: "update_failed", Severity: Warning, SystemID: "nas", Text: "agent update failed: x"})
	notSecret := func(e Event) bool { return e.SystemID != "secret" }
	evs, more, _, last := st.Since(old.ID, recent(), notSecret, 20)
	if len(evs) != 2 || evs[0].ID != crit.ID || evs[1].ID != fail.ID || more != 0 || last != fail.ID {
		t.Fatalf("want the critical event and the warning, oldest first: %+v (more %d, last %d)", evs, more, last)
	}
	if evs, _, _, _ := st.Since(old.ID, recent(), all, 20); len(evs) != 3 || evs[0].ID != hidden.ID {
		t.Fatalf("an admin sees the third one too: %+v", evs)
	}
	// a repeat that is counted on the same event (a failed update again) is not a new event
	st.Add(Event{Kind: "update_failed", Severity: Warning, SystemID: "nas", Text: "agent update failed: y"})
	if evs, _, _, last := st.Since(fail.ID, recent(), all, 20); len(evs) != 0 || last != fail.ID {
		t.Fatalf("a counted repeat came back as a new event: %+v", evs)
	}
	// nothing older than notBefore, however low the id that is asked for
	now = now.Add(2 * time.Minute)
	if evs, _, _, _ := st.Since(0, recent(), all, 20); len(evs) != 0 {
		t.Fatalf("events older than a minute: %+v", evs)
	}
	// a flood: the urgent ones first, then the newest; the rest is counted
	base := st.LastID()
	for i := 0; i < 30; i++ {
		st.Add(Event{Kind: "host_unstable", Severity: Warning, SystemID: fmt.Sprintf("h%d", i), Text: "unstable"})
		if i == 3 || i == 4 {
			st.Add(Event{Kind: "host_offline", Severity: Warning, SystemID: fmt.Sprintf("h%d", i), Text: "offline", Key: fmt.Sprintf("offline:h%d", i), Alarm: true})
		}
	}
	st.Add(Event{Kind: "host_offline", Severity: Warning, SystemID: "late", Text: "offline", Key: "offline:late", Alarm: true})
	evs, more, moreUrgent, last := st.Since(base, recent(), all, 5)
	urgent := 0
	for i, e := range evs {
		if e.Urgent() {
			urgent++
		}
		if i > 0 && evs[i-1].ID >= e.ID {
			t.Fatal("not oldest first", evs)
		}
	}
	if len(evs) != 5 || urgent != 3 || more != 28 || moreUrgent != 0 || last != st.LastID() || evs[4].SystemID != "late" || evs[3].SystemID != "h29" {
		t.Fatalf("%d events (%d urgent), more %d (%d urgent): %+v", len(evs), urgent, more, moreUrgent, evs)
	}
	if evs, more, moreUrgent, _ := st.Since(base, recent(), all, 2); len(evs) != 2 || more != 31 || moreUrgent != 1 {
		t.Fatalf("limit 2: %d events, more %d (%d urgent)", len(evs), more, moreUrgent)
	}
}
