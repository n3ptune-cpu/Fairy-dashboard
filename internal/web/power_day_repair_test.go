package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/power"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// The last 24 hours of the plugs a host relays, at a Re-pair and at a Remove of that host.
//
// Re-pair is new tokens for the same machine under the same id. Inside the panel it takes the host out and
// puts it in again, and the first half used to empty the graphs of every plug the host relays: a day of
// readings gone for a click that changes nothing about the plugs. The day stays now, and goes on when the
// host answers again. Remove is the end of the host in the panel: the days go with it, at once, in memory and
// in the file, so that a machine added later under the same name starts with empty graphs.
func TestPowerDayRepairKeepsItAndRemoveForgetsIt(t *testing.T) {
	e := setup(t)
	e.s.Enroll.Interval = 10 * time.Millisecond
	e.s.control, e.s.adminRL = newRateLimiter(1000), newRateLimiter(1000)
	e.s.Enroll.PanelAddr = "100.64.0.10"
	e.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
		return 200, enroll.Response{AgentVersion: "42-abc", Platform: "linux", Token: "pcz-read-token-xxxxxxxxxxxxxxxxxx",
			ControlToken: "pcz-control-token-yyyyyyyyyyyyyyyy"}, "", nil
	}
	e.b.Mu.Lock()
	e.b.Metrics["pc-z"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil } // it relays a plug, "desk"
	e.b.Mu.Unlock()
	c := e.cookie(controller(), 0)
	hdr := []string{"Cookie", c, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	run := func(path string, body map[string]any) {
		t.Helper()
		p := e.do("POST", path, body, hdr...)
		if p.code != 201 {
			t.Fatal(path, p.code, string(p.raw))
		}
		id := p.body["id"].(string)
		for i := 0; i < 300; i++ {
			time.Sleep(10 * time.Millisecond)
			e.s.Poller.PollOnce(context.Background())
			if g := e.do("GET", "/api/admin/enrollments/"+id, nil, "Cookie", c); g.body["done"] == true {
				if g.body["state"] != "online" {
					t.Fatal(path, g.body["state"], g.body["message"])
				}
				return
			}
		}
		t.Fatal("the enrollment never finished", path)
	}
	day := e.s.Poller.Power.Day
	file := filepath.Join(e.s.Poller.Cfg.DataDir, power.DayFile)
	const own, other = "pc-z/desk", "laptop/desk"

	// an hour of readings (as if the host had been there for an hour), so that there is a day to lose
	for m := 60; m >= 2; m-- {
		day.Record(time.Now().Add(-time.Duration(m)*time.Minute), []power.DaySample{{ID: own, W: 40}}, nil)
	}
	run("/api/admin/hosts", map[string]any{"name": "PC-Z", "os": "linux", "site": "Alice"})
	e.s.Poller.PollOnce(context.Background())
	before := day.Series(own, time.Now())
	if before.Summary == nil || before.Summary.CoveredS < 60*60 || before.Summary.MaxW != 80 || !day.Has(other, time.Now()) {
		t.Fatalf("the rig: the new host's plug: %+v; the laptop's has a day: %v", before.Summary, day.Has(other, time.Now()))
	}

	run("/api/admin/hosts/pc-z/repair", map[string]any{"confirm": "PC-Z"})
	after := day.Series(own, time.Now())
	if after.Summary == nil || after.Start != before.Start || after.Summary.CoveredS < before.Summary.CoveredS {
		t.Fatalf("Re-pair emptied the graph of the plug the host relays: before it %+v from %d, after it %+v from %d",
			before.Summary, before.Start, after.Summary, after.Start)
	}
	e.s.Poller.PollOnce(context.Background())
	if r := dayOf(e, own, "Cookie", c); r.code != 200 || !strings.Contains(string(r.raw), `"summary"`) {
		t.Errorf("the day after Re-pair, as the page asks for it: %d %s", r.code, r.raw)
	}

	if r := e.do("DELETE", "/api/admin/hosts/pc-z", map[string]any{"confirm": "PC-Z"}, hdr...); r.code != 200 {
		t.Fatal("remove", r.code, string(r.raw))
	}
	if day.Has(own, time.Now()) {
		t.Error("the day of a removed host's plug is still there")
	}
	if !day.Has(other, time.Now()) {
		t.Error("the Remove of one host took the day of another host's plug")
	}
	// the Remove wrote the file itself, before it answered: the next start has no day for the plug either
	raw, err := os.ReadFile(file)
	if err != nil || strings.Contains(string(raw), own) || !strings.Contains(string(raw), other) {
		t.Errorf("%s after the Remove (%v) names the removed host's plug, or not the other one: %.200s", power.DayFile, err, raw)
	}
	// and a history tick that was on its way does not bring it back
	e.s.Poller.Power.SampleHistory()
	if day.Has(own, time.Now()) {
		t.Error("the day came back with the next history tick")
	}
}

// The Remove of the unused copy of a machine that config.yaml has (Add host under an id config.yaml uses)
// takes the copy away and nothing of the machine: its agent stays, the plug it relays stays, and so does the
// plug's day.
func TestPowerDayRemoveOfAnUnusedCopyKeepsTheDay(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := managed.NewStore(cfg.DataDir).Put(managed.Host{ID: "laptop", Name: "LAPTOP", URL: "http://laptop-old:9101", OS: "linux", AddedAt: time.Now().UTC()},
		managed.Secret{Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	e, skipped := ehStartAdded(t, cfg, testutil.NewBackend())
	if len(skipped) != 1 {
		t.Fatal("the rig: the copy is not left out:", skipped)
	}
	e.s.control, e.s.adminRL = newRateLimiter(1000), newRateLimiter(1000)
	e.s.Poller.PollOnce(context.Background())
	day := e.s.Poller.Power.Day
	if !day.Has("laptop/desk", time.Now()) {
		t.Fatal("the rig: the plug config.yaml's laptop relays has no day")
	}
	hdr := []string{"Cookie", e.cookie(controller(), 0), "X-Requested-With", "fairy", "Content-Type", "application/json"}
	if r := e.do("DELETE", "/api/admin/hosts/laptop", map[string]any{"confirm": "LAPTOP"}, hdr...); r.code != 200 {
		t.Fatal("remove", r.code, string(r.raw))
	}
	if !day.Has("laptop/desk", time.Now()) {
		t.Error("the Remove of the unused copy emptied the graph of the plug that config.yaml's host relays")
	}
}
