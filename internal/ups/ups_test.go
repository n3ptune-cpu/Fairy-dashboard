package ups

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	upssrc "github.com/n3ptune-cpu/Fairy-agent/pkg/ups"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

func rows(status, charge string) []map[string]any {
	return []map[string]any{upssrc.RowMap(devices.UPSFromVars("cyberpower", map[string]string{"ups.status": status,
		"battery.charge": charge, "battery.runtime": "1500", "ups.load": "20", "ups.realpower.nominal": "900"}, ""))}
}

func TestTransitionsAndEvents(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(nil, 5, time.Second, 0.05, 3600, filepath.Join(dir, "ups-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	powers := map[string][]string{"cyberpower": {"node-a", "pc"}}
	names := map[string]string{"cyberpower": "Bob's CyberPower"}
	m.UpdateFromAgent("asus", rows("OL", "100"), false, powers, names)
	st := m.State("asus/cyberpower")
	if st.Name != "Bob's CyberPower" || st.Status != "online" || *st.Via != "asus" || len(m.SystemsMap()["pc"]) != 1 {
		t.Fatalf("%+v", st)
	}
	m.UpdateFromAgent("asus", rows("OB DISCHRG", "87"), false, powers, names)
	m.UpdateFromAgent("asus", rows("OB DISCHRG LB", "12"), false, powers, names)
	m.UpdateFromAgent("asus", rows("OL CHRG RB", "14"), false, powers, names)
	kinds := []string{}
	for _, e := range m.EventsSince(0) {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "on_battery,low_battery,power_restored,replace_battery" {
		t.Fatal(kinds)
	}
	if ev := m.EventsSince(0)[0]; !strings.Contains(ev.Message, "87.0% charge, 25:00 runtime") || ev.Severity != "warning" {
		t.Fatal(ev.Message)
	}
	// agent unreachable: first stale, then (after offline_after) unreachable + communication_lost
	m.UpdateFromAgent("asus", nil, true, powers, names)
	time.Sleep(80 * time.Millisecond)
	m.UpdateFromAgent("asus", nil, true, powers, names)
	st = m.State("asus/cyberpower")
	if st.Status != "unreachable" || !st.Stale || *st.Error != "agent unreachable" {
		t.Fatalf("%+v", st)
	}
	m.UpdateFromAgent("asus", rows("OL", "100"), false, powers, names)
	evs := m.EventsSince(4)
	if len(evs) != 2 || evs[0].Kind != "communication_lost" || evs[1].Kind != "communication_restored" {
		t.Fatalf("%+v", evs)
	}
	m.UpdateFromAgent("asus", []map[string]any{}, false, powers, names) // relay turned off -> removed
	if m.State("asus/cyberpower") != nil {
		t.Fatal("not removed")
	}
	m.SampleHistory()
}

func TestNUTSourceFailureAndConfig(t *testing.T) {
	m, err := NewManager([]map[string]any{{"id": "rack", "host": "127.0.0.1", "port": 1, "powers": map[string]any{"ups": []any{"node-a"}}}},
		5, 200*time.Millisecond, 60, 60, "")
	if err != nil {
		t.Fatal(err)
	}
	m.PollAll(context.Background()) // nothing listens on port 1: no panic, and the source is shown as what cannot be reached
	if ov := m.Overview(true).UPS; len(ov) != 1 || ov[0].ID != "rack" || ov[0].Status != "unreachable" || ov[0].Error == nil ||
		len(ov[0].Powers) != 1 || ov[0].Powers[0] != "node-a" {
		t.Fatalf("a source that never answered: %+v", ov)
	}
	if _, err := upssrc.Build(map[string]any{"id": "x", "type": "apcupsd", "host": "h"}); err == nil {
		t.Fatal("unknown type")
	}
	if _, err := upssrc.Build(map[string]any{"id": "x", "host": "h", "powers": []any{"Not Valid"}}); err == nil {
		t.Fatal("bad system id")
	}
	if FmtRuntime(nil) != "?" || FmtRuntime(func() *int { v := 125; return &v }()) != "2:05" {
		t.Fatal("runtime fmt")
	}
}

// r2-p3-01 (audit 5): a UPS server that has not answered since the panel started was invisible. Only UPSes the
// panel already knew were marked failed, and it knew none: GET /api/ups was empty, the page said "No UPS
// configured", there was no event, no notification and no log line. The same happened when the server answered
// without a UPS the panel could read. Now the configured source has an entry of its own from its first failed
// poll: unreachable, with the reason, one event (the notification "a UPS cannot be reached" comes from it) and
// one log line. When the server answers, the entry goes and its alert ends.
func TestAUPSSourceThatNeverAnsweredIsShown(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	m, err := NewManager(nil, 5, time.Second, 60, 3600, "")
	if err != nil {
		t.Fatal(err)
	}
	var notified []model.UpsEvent
	m.OnEvent = func(e model.UpsEvent) { notified = append(notified, e) }
	var answer func() ([]map[string]any, error)
	src := &upssrc.Source{ID: "dead", Name: "Rack UPS", Powers: upssrc.Powers{ByName: map[string][]string{"cp": {"node-b", "node-a"}, "apc": {"node-a"}}},
		Read: func(context.Context, time.Duration) ([]map[string]any, error) { return answer() }}
	m.Sources = append(m.Sources, src)
	ctx := context.Background()

	answer = func() ([]map[string]any, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	}
	for i := 0; i < 3; i++ { // three polls: one entry, one event, one log line
		m.PollSource(ctx, src, 0)
	}
	ov := m.Overview(false).UPS
	if len(ov) != 1 {
		t.Fatalf("want one entry for the source, got %+v", ov)
	}
	u := ov[0]
	if u.ID != "dead" || u.Name != "Rack UPS" || u.Status != "unreachable" || !u.Stale || u.Error == nil || *u.Error == "" || u.LastSeen != nil ||
		strings.Join(u.Powers, " ") != "node-a node-b" || u.Flags == nil || u.BatteryCharge != nil {
		t.Fatalf("%+v", u)
	}
	if got := kindsOf(m, "dead"); got != "communication_lost" || len(notified) != 1 || notified[0].Severity != "warning" ||
		!strings.Contains(notified[0].Message, "has not given the panel a UPS since the panel started ("+*u.Error+")") {
		t.Fatalf("events %q, notified %+v", got, notified)
	}
	if n := strings.Count(logs.String(), "UPS event"); n != 1 || !strings.Contains(logs.String(), "Rack UPS") {
		t.Fatalf("%d log lines for it:\n%s", n, logs.String())
	}
	if sm := m.SystemsMap(); len(sm["node-a"]) != 1 || sm["node-a"][0] != "dead" { // the hosts it powers show it
		t.Fatalf("%v", sm)
	}
	m.SampleHistory() // no point for an entry that has no values
	if len(m.Overview(true).History["dead"]) != 0 {
		t.Fatal("history for the source's entry")
	}
	// another reason: the text follows, no second event
	answer = func() ([]map[string]any, error) { return nil, context.DeadlineExceeded }
	m.PollSource(ctx, src, 0)
	if u2 := m.State("dead"); u2 == nil || *u2.Error == *u.Error || kindsOf(m, "dead") != "communication_lost" {
		t.Fatalf("%+v, events %q", u2, kindsOf(m, "dead"))
	}
	// it answers, without a UPS the panel can show: still the source's entry, with that reason
	answer = func() ([]map[string]any, error) { return []map[string]any{}, nil }
	m.PollSource(ctx, src, 0)
	if u3 := m.State("dead"); u3 == nil || u3.Status != "unreachable" || *u3.Error != errNoUPS || len(m.Overview(false).UPS) != 1 {
		t.Fatalf("%+v", u3)
	}
	answer = func() ([]map[string]any, error) {
		return []map[string]any{nil, {"name": "cp", "ok": false, "error": "x"}}, nil
	}
	m.PollSource(ctx, src, 0)
	if u4 := m.State("dead"); u4 == nil || *u4.Error != errUnreadable {
		t.Fatalf("%+v", u4)
	}
	// it answers with its UPS: the UPS is listed under its own id, the source's entry is gone, its alert ends
	answer = func() ([]map[string]any, error) {
		return []map[string]any{nutRow(map[string]string{"ups.status": "OL", "battery.charge": "100"})}, nil
	}
	m.PollSource(ctx, src, 0)
	ov = m.Overview(false).UPS
	if len(ov) != 1 || ov[0].ID != "dead/cp" || ov[0].Status != "online" || m.State("dead") != nil {
		t.Fatalf("%+v", ov)
	}
	if got := kindsOf(m, "dead"); got != "communication_lost communication_restored" {
		t.Fatalf("events of the source's entry: %q", got)
	}
	// from here on it is a UPS like every other: the server stops, the UPS fails, no entry for the source again
	answer = func() ([]map[string]any, error) { return nil, context.DeadlineExceeded }
	m.PollSource(ctx, src, 0)
	if ov = m.Overview(false).UPS; len(ov) != 1 || ov[0].ID != "dead/cp" || !ov[0].Stale || m.State("dead") != nil {
		t.Fatalf("%+v", ov)
	}
	// a source that answers at its first poll never gets such an entry
	m2, _ := NewManager(nil, 5, time.Second, 60, 3600, "")
	ok := &upssrc.Source{ID: "fine", Read: func(context.Context, time.Duration) ([]map[string]any, error) {
		return []map[string]any{nutRow(map[string]string{"ups.status": "OL"})}, nil
	}}
	m2.PollSource(ctx, ok, 0)
	if ov := m2.Overview(false).UPS; len(ov) != 1 || ov[0].ID != "fine/cp" || len(m2.EventsSince(0)) != 0 {
		t.Fatalf("%+v", ov)
	}
}
