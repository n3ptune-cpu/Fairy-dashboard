package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/power"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// The last 24 hours of a plug and the end of the host that relayed it (final review of build 16, J2-C).
//
// Remove forgot the day of a host's plugs only "if the host is polled". A host that the panel left out at its
// start (its tokens file is damaged: the Add host dialog lists it with "Re-pair it") is not polled, and it
// can be removed: the day of its plugs, read from power-day.json at that start, stayed in memory and in the
// file, and a machine added under the same id within a day showed the old machine's graph for a plug of the
// same id. Remove takes the day whenever what is removed is the added host itself, polled or not (not the
// unused copy of a machine that config.yaml has: that machine stays, and its day). And Add host takes out
// what is still there under the new host's id when the machine has paired, as it does with what "Edit host"
// kept.

// dayRig starts a panel that has the hosts of hedConfig, a day file with an hour of readings of each source
// in before (as the run before left it), and whatever the test put into the list of added hosts.
func dayRig(t *testing.T, cfg *config.Panel, b *testutil.Backend, wantSkipped int, before ...string) (r *hedRig, file string) {
	t.Helper()
	c := *cfg
	c.Agents, c.Systems, c.Proxmox = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems), slices.Clone(cfg.Proxmox)
	if _, skipped, err := managed.NewStore(cfg.DataDir).Merge(&c); err != nil || len(skipped) != wantSkipped {
		t.Fatalf("the rig: the start leaves %d added hosts out, want %d: %v %v", len(skipped), wantSkipped, err, skipped)
	}
	file = filepath.Join(cfg.DataDir, power.DayFile)
	prev := power.NewDayStore()
	prev.Load(file, time.Now())
	var samples []power.DaySample
	for _, id := range before {
		samples = append(samples, power.DaySample{ID: id, W: 40})
	}
	for m := 60; m >= 2; m-- {
		prev.Record(time.Now().Add(-time.Duration(m)*time.Minute), samples, nil)
	}
	if !prev.Flush(time.Now(), 5*time.Second) {
		t.Fatal("the rig: the day file was not written")
	}
	r = hedStart(t, &c, b)
	r.s.adminRL = newRateLimiter(1000)
	r.s.Enroll.Scripts, r.s.Enroll.PanelAddr, r.s.Enroll.Interval = testInstallers, "100.64.0.10", 10*time.Millisecond
	r.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
		return 200, enroll.Response{AgentVersion: "42-abc", Platform: "linux", Token: strings.Repeat("r", 32), ControlToken: strings.Repeat("d", 32)}, "", nil
	}
	for _, id := range before {
		if !r.s.Poller.Power.Day.Has(id, time.Now()) {
			t.Fatalf("the rig: the day of %s was not read from the file", id)
		}
	}
	return r, file
}

// inDayFile reports whether power-day.json, written now, names the source.
func inDayFile(t *testing.T, r *hedRig, file, id string) bool {
	t.Helper()
	r.s.Poller.Power.Day.Flush(time.Now(), 5*time.Second)
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(raw), `"`+id+`"`)
}

// plugHost is an agent that relays a plug of its own, "desk".
func plugHost(b *testutil.Backend, id string) {
	b.Metrics[id] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["hostname"] = id
		return m, nil
	}
}

func TestRemoveOfAnAddedHostThatIsNotPolledTakesItsPlugsDayAndTheNextHostStartsEmpty(t *testing.T) {
	cfg := hedConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := managed.NewStore(cfg.DataDir)
	if err := st.Put(managed.Host{ID: "box-m", Name: "Box M", URL: "http://box-m", Site: "site-a", OS: "linux", AddedAt: time.Now().UTC(),
		Options: managed.Options{PowerControl: true, SelfUpdate: true}}, managed.Secret{Token: strings.Repeat("m", 32), ControlToken: strings.Repeat("c", 32)}); err != nil {
		t.Fatal(err)
	}
	// its tokens file is damaged before this start of the panel: the host is listed, and not polled
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "secrets", "agent-box-m.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := hedBackend()
	plugHost(b, "box-m")
	const own, other = "box-m/desk", "pc-a/desk"
	r, file := dayRig(t, cfg, b, 1, own, other)
	hdr := append([]string{"Content-Type", "application/json"}, r.hdr...)
	admin := r.hdr[:2]
	day := r.s.Poller.Power.Day
	if _, polled := r.s.Poller.AgentByID("box-m"); polled {
		t.Fatal("the rig: box-m is polled")
	}
	if row := hostRow(t, r.env, r.hdr[1], "box-m"); row == nil || row["active"] != false || row["shadowed"] != false {
		t.Fatalf("the rig: the row of box-m in the Add host dialog: %v", row)
	}

	// --- Remove: the host is not polled, and its plug's day goes all the same
	if rm := r.do("DELETE", "/api/admin/hosts/box-m", map[string]any{"confirm": "Box M"}, hdr...); rm.code != 200 || rm.body["message"] != "Box M removed from the panel" {
		t.Fatal("Remove:", rm.code, string(rm.raw))
	}
	if hosts, _ := st.Load(); len(hosts) != 0 {
		t.Fatalf("the host is still in the list of added hosts: %v", hosts)
	}
	if day.Has(own, time.Now()) {
		t.Errorf("the last 24 hours of %s are still in the panel's memory after the Remove of its host", own)
	}
	if inDayFile(t, r, file, own) {
		t.Errorf("%s still names %s after the Remove of its host", power.DayFile, own)
	}
	if !day.Has(other, time.Now()) || !inDayFile(t, r, file, other) {
		t.Errorf("the Remove of box-m took the day of %s", other)
	}
	r.s.Poller.Power.SampleHistory()
	if day.Has(own, time.Now()) {
		t.Error("the day came back with the next history tick")
	}

	// --- Add host under the same id, within the day: the plug of the new machine starts with an empty day
	added := time.Now()
	p := r.addHost(map[string]any{"name": "box-m", "os": "linux"})
	if p.code != 201 {
		t.Fatalf("Add host: %d %s", p.code, p.raw)
	}
	if e := r.enrollment(p.body["id"].(string)); e["state"] != "online" {
		t.Fatalf("Add host: %v %v", e["state"], e["message"])
	}
	r.poll()
	if a, polled := r.s.Poller.AgentByID("box-m"); !polled || !a.Managed {
		t.Fatalf("the rig: the new host is polled as box-m: %v", polled)
	}
	if g := r.do("GET", "/api/power", nil, admin...); g.code != 200 || !strings.Contains(string(g.raw), `"`+own+`"`) {
		t.Fatalf("the rig: the new host relays no plug %s: %d %.300s", own, g.code, g.raw)
	}
	if s := day.Series(own, time.Now()); s.Summary != nil && s.Start < added.Unix()-60 {
		t.Errorf("the new host's plug has the old machine's day: it begins %s before the host was added: %+v", added.Sub(time.Unix(s.Start, 0)).Round(time.Second), s.Summary)
	}
	if g := dayOf(r.env, own, admin...); g.code != 200 || g.body["summary"] != nil {
		t.Errorf("the day of the new host's plug, as the page asks for it right after Add host: %d %.300s", g.code, g.raw)
	}
	// ... and its own readings begin its day (the minute after a removal starts none: power.DayStore.Forget)
	later := time.Now().Add(2 * time.Minute)
	day.Record(later, []power.DaySample{{ID: own, W: 80}}, nil)
	if s := day.Series(own, later); s.Summary == nil || s.Summary.CoveredS > 120 || s.Start < added.Unix()-60 || s.Summary.MaxW != 80 {
		t.Errorf("the new host's own readings: %+v from %d", s.Summary, s.Start)
	}
}

// Add host itself starts the new host's plugs with an empty day, whatever was there under its id: here the day
// of a plug of a host that config.yaml had until the last start (nothing removed it: there was no Remove).
func TestAddHostTakesOutAnOlderDayUnderTheNewHostsID(t *testing.T) {
	cfg := hedConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	b := hedBackend()
	plugHost(b, "garage")
	const old, other = "garage/desk", "pc-a/desk"
	r, file := dayRig(t, cfg, b, 0, old, other)
	day := r.s.Poller.Power.Day
	gate := make(chan struct{}) // the machine answers the pairing request when this is closed
	pair := r.s.Enroll.Pair
	r.s.Enroll.Pair = func(ctx context.Context, url string, body []byte) (int, enroll.Response, string, error) {
		select {
		case <-gate:
		case <-ctx.Done():
			return 0, enroll.Response{}, "", errors.New("no answer")
		}
		return pair(ctx, url, body)
	}
	added := time.Now()
	p := r.addHost(map[string]any{"name": "Garage", "os": "linux"})
	if p.code != 201 {
		t.Fatalf("Add host: %d %s", p.code, p.raw)
	}
	time.Sleep(50 * time.Millisecond)
	if !day.Has(old, time.Now()) {
		t.Error("the day went when the join code was made: nothing is added yet") // an Add host that is cancelled takes nothing
	}
	close(gate)
	if e := r.enrollment(p.body["id"].(string)); e["state"] != "online" {
		t.Fatalf("Add host: %v %v", e["state"], e["message"])
	}
	r.poll()
	if s := day.Series(old, time.Now()); s.Summary != nil && s.Start < added.Unix()-60 {
		t.Errorf("the new host's plug has the day that was there under its id: it begins %s before the host was added", added.Sub(time.Unix(s.Start, 0)).Round(time.Second))
	}
	if g := dayOf(r.env, old, r.hdr[:2]...); g.code != 200 || g.body["summary"] != nil {
		t.Errorf("the day of the new host's plug, as the page asks for it right after Add host: %d %.300s", g.code, g.raw)
	}
	if inDayFile(t, r, file, old) {
		t.Errorf("%s still names %s, with the day from before the host was added", power.DayFile, old)
	}
	if !day.Has(other, time.Now()) || !inDayFile(t, r, file, other) {
		t.Errorf("Add host took the day of %s", other)
	}
}

// What stays as it was: the Remove of the unused copy of a machine that config.yaml has leaves that machine's
// day alone. The machine stays in the panel, as config.yaml's entry, and so does the graph of its plug.
func TestRemoveOfTheUnusedCopyKeepsTheDayOfConfigYAMLsHost(t *testing.T) {
	cfg := hedConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := managed.NewStore(cfg.DataDir)
	if err := st.Put(managed.Host{ID: "pc-a", Name: "PC-A", URL: "http://pc-a-old", OS: "linux", AddedAt: time.Now().UTC()},
		managed.Secret{Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	const own = "pc-a/desk" // config.yaml's pc-a relays it
	r, file := dayRig(t, cfg, hedBackend(), 1, own)
	r.poll()
	if row := hostRow(t, r.env, r.hdr[1], "pc-a"); row == nil || row["shadowed"] != true {
		t.Fatalf("the rig: the row of the copy: %v", row)
	}
	start := r.s.Poller.Power.Day.Series(own, time.Now())
	rm := r.do("DELETE", "/api/admin/hosts/pc-a", map[string]any{"confirm": "PC-A"}, append([]string{"Content-Type", "application/json"}, r.hdr...)...)
	if rm.code != 200 || !strings.HasPrefix(rm.body["message"].(string), "the unused Add host copy of PC-A was removed.") {
		t.Fatal("Remove of the copy:", rm.code, string(rm.raw))
	}
	if hosts, _ := st.Load(); len(hosts) != 0 {
		t.Fatalf("the copy is still in the list: %v", hosts)
	}
	now := r.s.Poller.Power.Day.Series(own, time.Now())
	if now.Summary == nil || start.Summary == nil || now.Start != start.Start || now.Summary.CoveredS < start.Summary.CoveredS {
		t.Errorf("the Remove of the copy took the day of config.yaml's host: before %+v, now %+v", start.Summary, now.Summary)
	}
	if !inDayFile(t, r, file, own) {
		t.Errorf("%s no longer names %s after the Remove of the copy", power.DayFile, own)
	}
}
