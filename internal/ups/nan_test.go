package ups

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	upssrc "github.com/n3ptune-cpu/Fairy-agent/pkg/ups"
)

// nutRow is the row the shared NUT reader makes of these variables: what the panel's own UPS source hands over,
// and what an agent puts into its /metrics.
func nutRow(vars map[string]string) map[string]any {
	return upssrc.RowMap(devices.UPSFromVars("cp", vars, ""))
}

func kindsOf(m *Manager, key string) string {
	var out []string
	for _, e := range m.EventsSince(0) {
		if e.UpsID == key {
			out = append(out, e.Kind)
		}
	}
	return strings.Join(out, " ")
}

// fakeUpsd is a NUT server with one UPS, "cp", that answers the variables stored in vars.
func fakeUpsd(t *testing.T, vars *atomic.Value) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					switch line = strings.TrimSpace(line); {
					case line == "LIST UPS":
						fmt.Fprint(c, "BEGIN LIST UPS\nUPS cp \"CyberPower\"\nEND LIST UPS\n")
					case line == "LIST VAR cp":
						fmt.Fprint(c, "BEGIN LIST VAR cp\n")
						for k, v := range vars.Load().(map[string]string) {
							fmt.Fprintf(c, "VAR cp %s \"%s\"\n", k, v)
						}
						fmt.Fprint(c, "END LIST VAR cp\n")
					case line == "LOGOUT":
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// L11: a UPS server that sends "NaN" (or "Inf") for one value. The shared reader cannot encode that UPS, and the
// panel used to keep showing "online, charge 100" for ever: no error, no event, never stale. Now the UPS the panel
// knows goes stale with a reason, then unreachable with an event (and with it a notification), and comes back
// when its data does. Through the real NUT source and a fake upsd.
func TestUPSThatReportsNaNIsNotShownHealthy(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	good := map[string]string{"ups.status": "OL", "battery.charge": "100", "battery.runtime": "1500", "ups.load": "20", "ups.realpower": "180"}
	var vars atomic.Value
	vars.Store(good)
	port := fakeUpsd(t, &vars)
	m, err := NewManager([]map[string]any{{"id": "rack", "host": "127.0.0.1", "port": port, "powers": []any{"node-a"}}},
		5, 2*time.Second, 0.2, 3600, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.PollAll(ctx)
	st := m.State("rack/cp")
	if st == nil || st.Status != "online" || st.Stale || st.Error != nil || *st.BatteryCharge != 100 || *st.Watts != 180 {
		t.Fatalf("control case: %+v", st)
	}
	for _, bad := range []string{"NaN", "Inf", "-Inf", "+Infinity", "nan"} {
		// really on low battery, with one value that is not a number
		vars.Store(map[string]string{"ups.status": "OB LB", "battery.charge": "4", "battery.runtime": "60", "ups.realpower": bad})
		m.PollAll(ctx)
		st = m.State("rack/cp")
		if !st.Stale || st.Error == nil || *st.Error != errUnreadable {
			t.Fatalf("%s: still shown as healthy: %+v", bad, st)
		}
		time.Sleep(250 * time.Millisecond) // longer than offline_after
		m.PollAll(ctx)
		st = m.State("rack/cp")
		if st.Status != "unreachable" || !st.Stale || !strings.HasSuffix(kindsOf(m, "rack/cp"), "communication_lost") {
			t.Fatalf("%s: %+v, events %q", bad, st, kindsOf(m, "rack/cp"))
		}
		if _, err := json.Marshal(m.Overview(true)); err != nil {
			t.Fatal(err)
		}
		vars.Store(good)
		m.PollAll(ctx)
		st = m.State("rack/cp")
		if st.Status != "online" || st.Stale || st.Error != nil || !strings.HasSuffix(kindsOf(m, "rack/cp"), "communication_restored") {
			t.Fatalf("%s: not back: %+v, events %q", bad, st, kindsOf(m, "rack/cp"))
		}
	}
	if n := strings.Count(logs.String(), "UPS data that cannot be read"); n != 5 { // once per episode, not per poll
		t.Fatalf("logged %d times", n)
	}
	// a UPS that is no longer in the server's answer is not "online with its last values" either
	m.mu.Lock()
	src := m.Sources[0]
	m.mu.Unlock()
	gone := &upssrc.Source{ID: src.ID, Name: src.Name, Powers: src.Powers,
		Read: func(context.Context, time.Duration) ([]map[string]any, error) { return nil, nil }}
	m.PollSource(ctx, gone, time.Second)
	if st = m.State("rack/cp"); !st.Stale || *st.Error != errMissing {
		t.Fatalf("missing: %+v", st)
	}
}

// The same through an agent (L11). Agents up to v0.5.3 put null into their `ups` list for a UPS they could not
// encode; the panel used to read that as "UPS removed" and dropped it without a word.
func TestAgentUPSThatReportsNaN(t *testing.T) {
	m, err := NewManager(nil, 5, time.Second, 0.2, 3600, "")
	if err != nil {
		t.Fatal(err)
	}
	wire := func(rows ...map[string]any) []map[string]any { // as it travels: JSON, then the poller's AgentRows
		b, err := json.Marshal(map[string]any{"ups": rows})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		_ = json.Unmarshal(b, &payload)
		return AgentRows(payload["ups"])
	}
	good := nutRow(map[string]string{"ups.status": "OL", "battery.charge": "100", "battery.runtime": "1500"})
	nan := nutRow(map[string]string{"ups.status": "OB LB", "battery.charge": "4", "ups.realpower": "NaN"})
	if good == nil || nan != nil {
		t.Fatal("the shared reader's rows changed: adapt this test", good, nan)
	}
	m.UpdateFromAgent("nas", wire(good), false, nil, nil)
	if st := m.State("nas/cp"); st == nil || st.Status != "online" || st.Stale {
		t.Fatalf("%+v", st)
	}
	m.UpdateFromAgent("nas", wire(nan), false, nil, nil) // the wire carries [null]
	st := m.State("nas/cp")
	if st == nil || !st.Stale || st.Error == nil || *st.Error != errUnreadable {
		t.Fatalf("dropped or still healthy: %+v", st)
	}
	time.Sleep(250 * time.Millisecond)
	m.UpdateFromAgent("nas", wire(nan), false, nil, nil)
	if st = m.State("nas/cp"); st.Status != "unreachable" || kindsOf(m, "nas/cp") != "communication_lost" {
		t.Fatalf("%+v %q", st, kindsOf(m, "nas/cp"))
	}
	m.UpdateFromAgent("nas", wire(good), false, nil, nil)
	if st = m.State("nas/cp"); st.Status != "online" || st.Stale || st.Error != nil {
		t.Fatalf("not back: %+v", st)
	}
	// a UPS that is really gone from the agent's answer (removed, relay turned off) is still removed
	m.UpdateFromAgent("nas", wire(), false, nil, nil)
	if m.State("nas/cp") != nil {
		t.Fatal("removed UPS kept")
	}
	// more entries than the limit, objects or not: no more rows than the limit
	if n := len(AgentRows(make([]any, 1000))); n != MaxAgentUPS {
		t.Fatal(n)
	}
	if AgentRows("nope") == nil || len(AgentRows(nil)) != 0 {
		t.Fatal("not a list")
	}
}

// L11, range checks: charge 0 to 100, load and run time not negative, nothing absurd. Such a value is dropped
// and the UPS says so (stale, with the names of the values) instead of showing it; what the UPS says about its
// state still counts, so "on battery" still raises its event.
func TestUPSValuesOutOfRange(t *testing.T) {
	m, err := NewManager(nil, 5, time.Second, 60, 3600, "")
	if err != nil {
		t.Fatal(err)
	}
	m.UpdateFromAgent("nas", []map[string]any{nutRow(map[string]string{"ups.status": "OB", "battery.charge": "250", "battery.runtime": "-5",
		"ups.load": "-1", "ups.realpower": "1e308", "input.voltage": "231", "battery.voltage": "1e300"})}, false, nil, nil)
	st := m.State("nas/cp")
	if st.Status != "on_battery" || st.BatteryCharge != nil || st.BatteryRuntimeS != nil || st.LoadPercent != nil || st.Watts != nil ||
		st.BatteryVoltage != nil || *st.InputVoltage != 231 {
		t.Fatalf("%+v", st)
	}
	want := "the UPS reported values that cannot be right; they are not shown: battery_charge, battery_runtime_s, battery_voltage, load_percent, watts"
	if !st.Stale || st.Error == nil || *st.Error != want {
		t.Fatalf("stale %v, error %v", st.Stale, st.Error)
	}
	if kindsOf(m, "nas/cp") != "on_battery" {
		t.Fatalf("events %q", kindsOf(m, "nas/cp"))
	}
	// values that are no numbers at all (a text, a run time too large for a whole number) count the same
	m.UpdateFromAgent("nas", []map[string]any{{"name": "cp", "state": "online", "battery_charge": "NaN", "battery_runtime_s": 1e300,
		"load_percent": 20.0, "mfr": 42.0}}, false, nil, nil)
	st = m.State("nas/cp")
	if !st.Stale || *st.Error != "the UPS reported values that cannot be right; they are not shown: battery_charge, battery_runtime_s" ||
		*st.LoadPercent != 20 || st.BatteryCharge != nil {
		t.Fatalf("%+v %v", st, *st.Error)
	}
	m.SampleHistory()
	if _, err := json.Marshal(m.Overview(true)); err != nil {
		t.Fatal(err)
	}
	// normal values, also at the ends of their ranges, are untouched
	m.UpdateFromAgent("nas", []map[string]any{nutRow(map[string]string{"ups.status": "OL", "battery.charge": "100", "battery.runtime": "0",
		"ups.load": "0", "ups.realpower.nominal": "900", "input.voltage": "231.5", "output.voltage": "230", "input.frequency": "50",
		"battery.voltage": "27.1"})}, false, nil, nil)
	st = m.State("nas/cp")
	if st.Stale || st.Error != nil || *st.BatteryCharge != 100 || *st.BatteryRuntimeS != 0 || *st.LoadPercent != 0 || *st.Watts != 0 ||
		!st.WattsEstimated || *st.NominalW != 900 || *st.InputVoltage != 231.5 || *st.InputFrequency != 50 || *st.BatteryVoltage != 27.1 {
		t.Fatalf("%+v", st)
	}
}
