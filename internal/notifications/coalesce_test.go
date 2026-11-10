package notifications

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// What L7's limits left out (N1's notification half and N2), as the review measured it: a temperature that
// flaps was pushed every time (150 an hour), a version that flips filled the host's share of the list (500
// unread in 17 minutes), and one Proxmox API that lists 2100 nodes and takes them offline sent 2100 pushes at
// once and pushed both real standing alarms out of the list.

type rig struct {
	st     *Store
	w      *Watcher
	now    time.Time
	pushed []string
}

func newCoalesceRig(t *testing.T) *rig {
	r := &rig{st: Open(filepath.Join(t.TempDir(), "n.json")), now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	r.st.Now = func() time.Time { return r.now }
	r.st.Push = func(title, body string) { r.pushed = append(r.pushed, title+" | "+body) }
	r.w = &Watcher{Store: r.st}
	return r
}

func (r *rig) list() Page {
	return r.st.List("u", func(Event) bool { return true }, Filter{Limit: 200})
}

func (r *rig) pushes(sub string) int {
	n := 0
	for _, p := range r.pushed {
		if strings.Contains(p, sub) {
			n++
		}
	}
	return n
}

func hostAt(id string, temp float64) model.System {
	return model.System{ID: id, Name: id, Kind: "agent", Status: "online", Temps: []model.Temperature{{Sensor: "x", CurrentC: temp}}}
}

// N2: one agent whose hottest sensor goes over and under the limit every 12 seconds. Each change holds for the
// 10 s the watcher asks for, so each was an event and every "hot" one a pushed warning: 150 an hour.
func TestTemperatureFlapIsNotPushedEveryTime(t *testing.T) {
	r := newCoalesceRig(t)
	r.w.Observe(r.now, []model.System{hostAt("evil", 40), hostAt("good", 40)}, nil)
	hot := false
	for s := 0; s < 3600; s += 2 { // one hour of 2 s polls
		r.now = r.now.Add(2 * time.Second)
		if s%12 == 0 {
			hot = !hot
		}
		temp := 40.0
		if hot {
			temp = 99
		}
		r.w.Observe(r.now, []model.System{hostAt("evil", temp), hostAt("good", 40)}, nil)
	}
	p := r.list()
	t.Logf("one hour of a temperature flapping every 12 s: %d push messages, %d events, %d unread", len(r.pushed), len(p.Events), p.Unread)
	// one "hot" event per RepeatWindow (10 minutes) is pushed; the repeats are counted on it
	if n := len(r.pushed); n < 6 || n > 7 {
		t.Fatalf("%d push messages in an hour, want one per %s: %v", n, RepeatWindow, r.pushed)
	}
	if p.Unread > 14 || len(p.Events) > 14 {
		t.Fatalf("%d events, %d unread", len(p.Events), p.Unread)
	}
	merged := false
	for _, e := range p.Events {
		merged = merged || (e.Kind == "temp_high" && e.Count > 10 && strings.Contains(e.Text, fmt.Sprintf("(x%d)", e.Count)))
	}
	if !merged {
		t.Fatal("the repeats must be counted on the event:", p.Events)
	}
}

// ... and what really happens stays visible. A host that gets hot and stays hot has one alarm that holds; when
// it cools down the alarm is over; and whatever a flapping host did in between, the list ends in the state the
// host is in.
func TestRealTemperatureChangesStayVisible(t *testing.T) {
	r := newCoalesceRig(t)
	step := func(temp float64, d time.Duration) {
		for end := r.now.Add(d); r.now.Before(end); {
			r.now = r.now.Add(2 * time.Second)
			r.w.Observe(r.now, []model.System{hostAt("nas", temp)}, nil)
		}
	}
	r.w.Observe(r.now, []model.System{hostAt("nas", 40)}, nil)
	step(95, time.Minute)
	if p := r.list(); p.ActiveAlerts != 1 || len(r.pushed) != 1 || !strings.Contains(r.pushed[0], "95 °C") {
		t.Fatalf("hot and staying hot: %d active alerts, pushed %v", p.ActiveAlerts, r.pushed)
	}
	step(95, 30*time.Minute) // still hot: nothing new, the alarm holds
	if p := r.list(); p.ActiveAlerts != 1 || len(r.pushed) != 1 || len(p.Events) != 1 {
		t.Fatalf("still hot: %d active, %d pushed, %d events", p.ActiveAlerts, len(r.pushed), len(p.Events))
	}
	step(50, time.Minute)
	if p := r.list(); p.ActiveAlerts != 0 || len(p.Events) != 2 || p.Events[0].Kind != "temp_ok" {
		t.Fatalf("cooled down: %d active, events %v", p.ActiveAlerts, p.Events)
	}
	// a burst of flapping that ends hot: the alarm holds at the end, although its repeats were merged ...
	for i := 0; i < 7; i++ {
		step(95, 14*time.Second)
		step(50, 14*time.Second)
	}
	step(95, time.Minute)
	if p := r.list(); p.ActiveAlerts != 1 {
		t.Fatalf("flapped and ended hot: %d active alerts: %v", p.ActiveAlerts, p.Events)
	}
	// ... and one that ends cool: nothing holds
	step(50, time.Minute)
	if p := r.list(); p.ActiveAlerts != 0 {
		t.Fatalf("flapped and ended cool: %d active alerts", p.ActiveAlerts)
	}
	// much later the host gets hot again: a new event, pushed again
	before := len(r.pushed)
	step(50, 20*time.Minute)
	step(96, time.Minute)
	if p := r.list(); p.ActiveAlerts != 1 || len(r.pushed) != before+1 {
		t.Fatalf("hot again after a quiet time: %d active, %d new pushes", p.ActiveAlerts, len(r.pushed)-before)
	}
}

// N2: an agent that reports another version on every poll made one "agent now runs X" event per poll, up to
// its share of the list (500 unread in 17 minutes).
func TestVersionFlipIsMerged(t *testing.T) {
	r := newCoalesceRig(t)
	for i := 0; i < 510; i++ { // 17 minutes of 2 s polls
		r.st.Add(Event{Kind: "update_done", Type: "update", Severity: Info, SystemID: "evil", Host: "EVIL",
			Text: fmt.Sprintf("agent now runs %d (was %d)", i+1, i)})
		r.now = r.now.Add(2 * time.Second)
	}
	p := r.list()
	if len(p.Events) != 2 || p.Unread != 2 || p.Events[1].Count != 300 {
		t.Fatalf("%d events, %d unread: %+v", len(p.Events), p.Unread, p.Events)
	}
	// a real update (one change) is one plain event, as before
	r.st.Add(Event{Kind: "update_done", Type: "update", Severity: Info, SystemID: "nas", Host: "NAS", Text: "agent now runs 14 (was 13)"})
	if e := r.list().Events[0]; e.SystemID != "nas" || e.Count != 0 || e.Text != "agent now runs 14 (was 13)" {
		t.Fatalf("%+v", e)
	}
}

func pveNodesAt(cluster string, n int, status string) []model.System {
	out := make([]model.System, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, model.System{ID: fmt.Sprintf("%s-node-n%d", cluster, i), Name: fmt.Sprintf("n%d", i), Kind: "node", Source: "proxmox",
			ClusterID: model.P(cluster), Status: status})
	}
	return out
}

func (r *rig) standingAlarms() (apc, nas Event) {
	apc = r.st.Add(Event{Kind: "ups_on_battery", Type: "ups", Severity: Warning, UPSID: "rack/apc", Host: "APC", Text: "power lost", Key: "ups:rack/apc", Alarm: true})
	nas = r.st.Add(Event{Kind: "host_offline", Type: "host", Severity: Warning, SystemID: "nas", Host: "nas", Text: "offline", Key: "offline:nas", Alarm: true})
	return
}

func (r *rig) holds(key string) bool {
	r.st.mu.Lock()
	defer r.st.mu.Unlock()
	for _, e := range r.st.d.Events {
		if e.Key == key && e.Active() {
			return true
		}
	}
	return false
}

// N1, the notification half. One Proxmox API lists 2100 nodes and takes them offline (the row limit now stops
// that at 64 nodes, but the list must not depend on it): each node used to be its own "host" with its own
// share, each alarm was pushed (2100 at once), and the list filled with alarms that hold, so the oldest went:
// the two real ones.
func TestManyNodesOfOneSourceAreOneSource(t *testing.T) {
	r := newCoalesceRig(t)
	r.standingAlarms()
	base := len(r.pushed)
	r.w.Observe(r.now, pveNodesAt("evil", 2100, "online"), nil)
	r.now = r.now.Add(2 * time.Second)
	r.w.Observe(r.now, pveNodesAt("evil", 2100, "offline"), nil)
	r.now = r.now.Add(31 * time.Second)
	r.w.Observe(r.now, pveNodesAt("evil", 2100, "offline"), nil)

	p := r.list()
	mine := 0
	r.st.mu.Lock()
	for _, e := range r.st.d.Events {
		if e.owner() == "proxmox:evil" {
			mine++
		}
	}
	total := len(r.st.d.Events)
	r.st.mu.Unlock()
	t.Logf("2100 listed nodes went offline: %d push messages, %d active alerts, %d events of that source in a list of %d",
		len(r.pushed)-base, p.ActiveAlerts, mine, total)
	if !r.holds("ups:rack/apc") || !r.holds("offline:nas") {
		t.Fatal("a real standing alarm was pushed out of the list")
	}
	if mine != MaxPerHost || total != MaxPerHost+2 {
		t.Fatalf("the source holds %d events (its share is %d), the list %d", mine, MaxPerHost, total)
	}
	// MaxPushPerSource alarms are pushed, then one line that says that this source is over its limit
	if n := len(r.pushed) - base; n != MaxPushPerSource+1 || r.pushes("more than 5 events in a minute from proxmox:evil") != 1 {
		t.Fatalf("%d push messages: %v", n, r.pushed[base:])
	}
	// another source is not held back by it, and neither is the panel itself
	r.st.Add(Event{Kind: "host_offline", Type: "host", Severity: Warning, SystemID: "tower", Host: "tower", Text: "offline", Key: "offline:tower", Alarm: true})
	r.st.Add(Event{Kind: "panel_update_failed", Type: "update", Severity: Warning, Text: "the panel's update failed"})
	if r.pushes("tower: offline") != 1 || r.pushes("the panel's update failed") != 1 {
		t.Fatalf("another source's alarm was not pushed: %v", r.pushed[base:])
	}
	// a minute later the source's next event is pushed again, and says how many were not
	r.now = r.now.Add(2 * time.Minute)
	e := r.st.Add(Event{Kind: "host_unstable", Type: "host", Severity: Warning, SystemID: "evil-node-n0", Host: "n0", Source: "proxmox:evil", Text: "unstable"})
	last := r.pushed[len(r.pushed)-1]
	if e.ID == 0 || !strings.Contains(last, "n0: unstable") || !strings.Contains(last, fmt.Sprintf("and %d more from this source that were not pushed", 2100-MaxPushPerSource)) {
		t.Fatalf("the next push of that source: %s", last)
	}
}

// Several hostile sources together: each stays inside its own share, but together they hold more alarms than
// the list has room for. The source with the most gives up its own; the two real alarms stay.
func TestSeveralSourcesCannotEvictAnothersStandingAlarm(t *testing.T) {
	r := newCoalesceRig(t)
	r.standingAlarms()
	var all []model.System
	for c := 0; c < 6; c++ {
		all = append(all, pveNodesAt(fmt.Sprintf("evil%d", c), 450, "online")...)
	}
	r.w.Observe(r.now, all, nil)
	for i := range all {
		all[i].Status = "offline"
	}
	r.now = r.now.Add(2 * time.Second)
	r.w.Observe(r.now, all, nil)
	r.now = r.now.Add(31 * time.Second)
	r.w.Observe(r.now, all, nil)
	r.st.mu.Lock()
	total := len(r.st.d.Events)
	per := map[string]int{}
	for _, e := range r.st.d.Events {
		per[e.owner()]++
	}
	r.st.mu.Unlock()
	if total != MaxEvents || !r.holds("ups:rack/apc") || !r.holds("offline:nas") {
		t.Fatalf("list of %d; UPS alarm holds: %v, host alarm holds: %v; per source: %v", total, r.holds("ups:rack/apc"), r.holds("offline:nas"), per)
	}
	for o, n := range per {
		if strings.HasPrefix(o, "proxmox:") && (n < 300 || n > 450) {
			t.Errorf("%s holds %d events: the sources must give up evenly", o, n)
		}
	}
	if n := len(r.pushed); n > 2+6*(MaxPushPerSource+1) {
		t.Fatalf("%d push messages for six sources", n)
	}
}

// The push limit by itself: per source, per minute.
func TestPushLimitPerSource(t *testing.T) {
	r := newCoalesceRig(t)
	warn := func(sys, text string) {
		r.st.Add(Event{Kind: "host_unstable", Type: "host", Severity: Warning, SystemID: sys, Host: sys, Text: text})
	}
	for i := 0; i < 20; i++ {
		warn("evil", fmt.Sprintf("event %d", i))
	}
	if len(r.pushed) != MaxPushPerSource+1 || !strings.Contains(r.pushed[MaxPushPerSource], "from evil") {
		t.Fatalf("%d pushes: %v", len(r.pushed), r.pushed)
	}
	if r.list().Unread != 20 {
		t.Fatal("every event is on the page, whatever was pushed")
	}
	warn("nas", "its own limit")
	if len(r.pushed) != MaxPushPerSource+2 {
		t.Fatal("another source is not limited by it")
	}
	r.now = r.now.Add(PushWindow - time.Second)
	warn("evil", "still inside the minute")
	if len(r.pushed) != MaxPushPerSource+2 {
		t.Fatal("pushed inside the window")
	}
	r.now = r.now.Add(2 * time.Second)
	warn("evil", "the next minute")
	if last := r.pushed[len(r.pushed)-1]; !strings.Contains(last, "the next minute") || !strings.Contains(last, "and 16 more from this source") {
		t.Fatalf("%s", last)
	}
	// info events are never pushed and do not use up the limit
	for i := 0; i < 50; i++ {
		r.st.Add(Event{Kind: "host_online", Type: "host", Severity: Info, SystemID: "evil", Text: "back online"})
	}
	n := len(r.pushed)
	for i := 0; i < MaxPushPerSource-1; i++ {
		warn("evil", "more")
	}
	if len(r.pushed) != n+MaxPushPerSource-1 {
		t.Fatalf("%d pushes after info events", len(r.pushed)-n)
	}
	// a UPS source counts as one source, whatever its UPSes are called
	for i := 0; i < 30; i++ {
		r.st.Add(Event{Kind: "ups_on_battery", Type: "ups", Severity: Warning, UPSID: fmt.Sprintf("relay/u%d", i), Text: "on battery",
			Key: fmt.Sprintf("ups:relay/u%d", i), Alarm: true})
	}
	if got := r.pushes("on battery"); got != MaxPushPerSource {
		t.Fatalf("%d pushes for one UPS source", got)
	}
}
