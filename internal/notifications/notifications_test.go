package notifications

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

func host(id, status string, temp float64) model.System {
	s := model.System{ID: id, Name: strings.ToUpper(id), Kind: "agent", Status: status}
	if temp > 0 {
		s.Temps = []model.Temperature{{Sensor: "k10temp", Label: model.P("Tctl"), CurrentC: temp}}
	}
	return s
}

func kinds(s *Store) string {
	p := s.List("u", func(Event) bool { return true }, Filter{Limit: 200})
	var out []string
	for i := len(p.Events) - 1; i >= 0; i-- {
		out = append(out, p.Events[i].Kind)
	}
	return strings.Join(out, ",")
}

func TestWatcherDebounceAndPanelOff(t *testing.T) {
	var pushed []string
	s := Open(filepath.Join(t.TempDir(), "n.json"))
	s.Push = func(title, _ string) { pushed = append(pushed, title) }
	w := &Watcher{Store: s}
	t0 := time.Unix(1_800_000_000, 0)
	at := func(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }
	w.Observe(at(0), []model.System{host("a", "offline", 0), host("b", "online", 50), {ID: "qemu-1", Kind: "qemu", Status: "online"}}, nil)
	if kinds(s) != "" {
		t.Fatal("events at start:", kinds(s))
	}
	// a blip shorter than the debounce is no event
	w.Observe(at(10), []model.System{host("a", "offline", 0), host("b", "offline", 0)}, nil)
	w.Observe(at(20), []model.System{host("a", "offline", 0), host("b", "online", 50)}, nil)
	w.Observe(at(60), []model.System{host("a", "offline", 0), host("b", "online", 50), {ID: "qemu-1", Kind: "qemu", Status: "stopped"}}, nil)
	if kinds(s) != "" {
		t.Fatal("blip / guest made events:", kinds(s))
	}
	// b goes offline for real
	w.Observe(at(100), []model.System{host("b", "offline", 0)}, nil)
	w.Observe(at(140), []model.System{host("b", "offline", 0)}, nil)
	if kinds(s) != "host_offline" || len(pushed) != 1 {
		t.Fatal(kinds(s), pushed)
	}
	if _, active := s.Counts("u", func(Event) bool { return true }); active != 1 {
		t.Fatal("active alerts", active)
	}
	w.Observe(at(200), []model.System{host("b", "online", 50)}, nil)
	w.Observe(at(240), []model.System{host("b", "online", 50)}, nil)
	if kinds(s) != "host_offline,host_online" {
		t.Fatal(kinds(s))
	}
	if _, active := s.Counts("u", func(Event) bool { return true }); active != 0 {
		t.Fatal("alarm not resolved")
	}
	// shut down from the panel: informational
	w.NotePanelPower("b", "shutdown", at(250))
	w.Observe(at(300), []model.System{host("b", "offline", 0)}, nil)
	w.Observe(at(340), []model.System{host("b", "offline", 0)}, nil)
	w.Observe(at(400), []model.System{host("b", "online", 40)}, nil)
	w.Observe(at(440), []model.System{host("b", "online", 40)}, nil)
	if kinds(s) != "host_offline,host_online,host_off,host_online" || len(pushed) != 1 {
		t.Fatal(kinds(s), pushed)
	}
	// sent to sleep from the panel: "asleep", informational, then "awake again"
	w.NotePanelPower("b", "sleep", at(1450))
	w.Observe(at(1500), []model.System{host("b", "offline", 0)}, nil)
	w.Observe(at(1540), []model.System{host("b", "offline", 0)}, nil)
	w.Observe(at(1600), []model.System{host("b", "online", 40)}, nil)
	w.Observe(at(1640), []model.System{host("b", "online", 40)}, nil)
	if kinds(s) != "host_offline,host_online,host_off,host_online,host_asleep,host_online" || len(pushed) != 1 {
		t.Fatal(kinds(s), pushed)
	}
	if l := s.List("u", func(Event) bool { return true }, Filter{Limit: 1}); l.Events[0].Text != "awake again" {
		t.Fatal(l.Events[0])
	}
}

func TestWatcherTemperatureAndFlapping(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), "n.json"))
	w := &Watcher{Store: s, Debounce: 30 * time.Second, FlapWindow: 10 * time.Minute, FlapCount: 4}
	t0 := time.Unix(1_800_000_000, 0)
	sec := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }
	w.Observe(sec(0), []model.System{host("c", "online", 60)}, nil)
	w.Observe(sec(5), []model.System{host("c", "online", 90)}, nil)
	w.Observe(sec(16), []model.System{host("c", "online", 91)}, nil)
	w.Observe(sec(20), []model.System{host("c", "online", 83)}, nil) // between 80 and 85: still hot
	w.Observe(sec(40), []model.System{host("c", "online", 70)}, nil)
	w.Observe(sec(51), []model.System{host("c", "online", 70)}, nil)
	if kinds(s) != "temp_high,temp_ok" {
		t.Fatal(kinds(s))
	}
	if p := s.List("u", func(Event) bool { return true }, Filter{Limit: 5}); !strings.Contains(p.Events[1].Text, "Tctl 91") {
		t.Fatal(p.Events[1].Text)
	}
	// flapping: after 4 changes one "unstable" event, then quiet
	n := 100
	for i := 0; i < 8; i++ {
		st := []string{"offline", "online"}[i%2]
		w.Observe(sec(n), []model.System{host("c", st, 60)}, nil)
		w.Observe(sec(n+31), []model.System{host("c", st, 60)}, nil)
		n += 40
	}
	if got := kinds(s); got != "temp_high,temp_ok,host_offline,host_online,host_offline,host_unstable" {
		t.Fatal(got)
	}
	// UPS events pass through, with alarms resolved by the matching event
	w.Observe(sec(n), nil, []model.UpsEvent{{UpsID: "u1", UpsName: "CyberPower", Kind: "on_battery", Severity: "warning", Message: "power lost"}})
	w.Observe(sec(n+1), nil, []model.UpsEvent{{UpsID: "u1", UpsName: "CyberPower", Kind: "power_restored", Severity: "info", Message: "back"}})
	if _, active := s.Counts("u", func(e Event) bool { return e.Type == "ups" }); active != 0 {
		t.Fatal("ups alarm not resolved")
	}
}

func TestStoreReadDismissVisibilityAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "n.json")
	s := Open(path)
	for i := 0; i < 5; i++ {
		s.Add(Event{Kind: "host_offline", Type: "host", Severity: Warning, SystemID: []string{"x", "y"}[i%2], Text: "offline"})
	}
	onlyX := func(e Event) bool { return e.SystemID == "x" }
	if p := s.List("v", onlyX, Filter{}); len(p.Events) != 3 || p.Unread != 3 {
		t.Fatalf("visibility: %+v", p)
	}
	s.MarkRead("v", []int64{1}, false)
	s.Dismiss("v", []int64{3})
	if p := s.List("v", onlyX, Filter{}); len(p.Events) != 2 || p.Unread != 1 {
		t.Fatalf("read/dismiss: %+v", p)
	}
	if p := s.List("other", onlyX, Filter{}); len(p.Events) != 3 {
		t.Fatal("dismiss leaked to another user")
	}
	s.MarkRead("v", nil, true)
	if p := s.List("v", onlyX, Filter{Unread: true}); len(p.Events) != 0 {
		t.Fatal("mark all read")
	}
	if p := s.List("v", func(Event) bool { return true }, Filter{Limit: 2}); len(p.Events) != 2 || p.Next == nil {
		t.Fatal("paging")
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s2 := Open(path)
	if p := s2.List("v", onlyX, Filter{}); len(p.Events) != 2 || p.Unread != 0 {
		t.Fatalf("reloaded: %+v", p)
	}
	// bounded by age and count
	s2.Now = func() time.Time { return time.Now().Add(MaxAge + time.Hour) }
	s2.Add(Event{Kind: "x", Text: "new"})
	if p := s2.List("v", func(Event) bool { return true }, Filter{}); len(p.Events) != 1 {
		t.Fatal("old events kept", len(p.Events))
	}
}
