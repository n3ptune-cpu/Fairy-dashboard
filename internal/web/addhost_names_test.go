package web

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Add host and the names of the hosts the panel has (second look of the review of Edit host, a3). A host's
// name is typed to confirm a power action on it, and "Edit host" refuses a name another host has. Add host
// did not look: a host added as "Den" next to a host that was named Den in the panel got another id and was
// accepted.

// addHost sends Add host as the admin who just signed in.
func (r *hedRig) addHost(body map[string]any) resp {
	r.t.Helper()
	return r.do("POST", "/api/admin/hosts", body, append([]string{"Content-Type", "application/json"}, r.hdr...)...)
}

// enrollment waits until the Add host with this id is done, and returns it.
func (r *hedRig) enrollment(id string) map[string]any {
	r.t.Helper()
	for i := 0; ; i++ {
		time.Sleep(10 * time.Millisecond)
		r.poll()
		g := r.do("GET", "/api/admin/enrollments/"+id, nil, r.hdr[:2]...)
		if g.body["done"] == true {
			return g.body
		} else if i > 500 {
			r.t.Fatalf("the Add host never finished: %v %v", g.body["state"], g.body["message"])
		}
	}
}

func TestAddHostNameIsNotAnotherHosts(t *testing.T) {
	cfg := hedConfig(t)
	for i := range cfg.Agents {
		if cfg.Agents[i].ID == "box-d" {
			cfg.Agents[i].Name = "Tower" // a name config.yaml gives a host whose id is another word
		}
	}
	b := hedBackend()
	for _, id := range []string{"garage", "loft"} {
		b.Metrics[id] = func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			m["hostname"] = id
			delete(m, "power_sources")
			return m, nil
		}
	}
	r := hedStart(t, cfg, b)
	r.s.Enroll.Scripts, r.s.Enroll.PanelAddr, r.s.Enroll.Interval = testInstallers, "100.64.0.10", 10*time.Millisecond
	gate := make(chan struct{}) // the machine answers the pairing request when this is closed
	r.s.Enroll.Pair = func(ctx context.Context, url string, _ []byte) (int, enroll.Response, string, error) {
		select {
		case <-gate:
		case <-ctx.Done():
			return 0, enroll.Response{}, "", errors.New("no answer")
		}
		return 200, enroll.Response{AgentVersion: "42-abc", Platform: "linux", Token: strings.Repeat("r", 32)}, "", nil
	}
	refused := func(what, name, want string) {
		t.Helper()
		p := r.addHost(map[string]any{"name": name, "os": "linux"})
		if p.code != 400 || !strings.Contains(string(p.raw), want) || !strings.Contains(string(p.raw), "Nothing was stored") {
			t.Errorf("%s: Add host %q: %d %s", what, name, p.code, p.raw)
		}
	}
	// the name a host is shown under, from config.yaml
	refused("config.yaml's name of a host with another id", "TOWER", "name: another host is called Tower already")
	// a name given in Edit host, and the name config.yaml has for that host (a Reset there shows it again)
	r.ok("box-d", map[string]any{"name": "Shelf"})
	r.ok("pc-b", map[string]any{"name": "Den"})
	refused("a name given in Edit host", "den", "name: another host is called Den already")
	refused("a name given in Edit host", "SHELF", "name: another host is called Shelf already")
	refused("the name config.yaml has for a renamed host", "tower", "name: Tower is the name config.yaml has for another host (shown as Shelf now)")
	// a name kept for a host that is not shown right now
	r.hedGive("pc-z", "Cellar")
	refused("a name kept for a host without a row", "cellar", "name: Cellar is kept as the name of another host (pc-z) that is not shown right now")
	if l := r.s.Enroll.List(); len(l) != 0 {
		t.Fatalf("a refused Add host made a join code: %v", l)
	}
	for _, e := range r.auditEntries("", "host.add") {
		if out, _ := e["outcome"].(string); !strings.HasPrefix(out, "refused: ") || !strings.Contains(out, "name: ") {
			t.Errorf("audit: %v", e)
		}
	}

	// What state.json still keeps under the NEW host's own id is not another host's: Add host takes it out
	// when the machine has paired (Hooks.Fresh), and the name is free.
	r.hedGive("workshop", "Workshop")
	if p := r.addHost(map[string]any{"name": "Workshop", "os": "linux"}); p.code != 201 {
		t.Errorf("Add host under a name that is kept under its own id: %d %s", p.code, p.raw)
	} else if c := r.do("DELETE", "/api/admin/enrollments/"+p.body["id"].(string), nil, r.hdr...); c.code != 200 {
		t.Errorf("cancel: %d %s", c.code, c.raw)
	}

	// A free name gets its code. The name is asked again when the machine has paired: it was given to another
	// host in between, and the new host is not added.
	p := r.addHost(map[string]any{"name": "Garage", "os": "linux"})
	if p.code != 201 {
		t.Fatalf("Add host Garage: %d %s", p.code, p.raw)
	}
	r.ok("pc-a", map[string]any{"name": "GARAGE"})
	close(gate)
	e := r.enrollment(p.body["id"].(string))
	if msg, _ := e["message"].(string); e["state"] != "failed" || !strings.Contains(msg, "cannot add it: name: another host is called GARAGE already") || !strings.Contains(msg, "make a new code under another name") {
		t.Errorf("the Add host whose name was taken meanwhile: %v %v", e["state"], e["message"])
	}
	if _, ok := r.s.Poller.AgentByID("garage"); ok {
		t.Error("garage is polled")
	}
	if _, ok := r.s.managedHost("garage"); ok {
		t.Error("garage is in the list of added hosts")
	}

	// Re-pair asks nothing about the name: the host keeps the name it was added under, also while its row
	// shows another one, and also when its own name is what the row shows.
	p = r.addHost(map[string]any{"name": "Loft", "os": "linux"})
	if p.code != 201 {
		t.Fatalf("Add host Loft: %d %s", p.code, p.raw)
	}
	if e := r.enrollment(p.body["id"].(string)); e["state"] != "online" {
		t.Fatalf("Loft: %v %v", e["state"], e["message"])
	}
	for _, shown := range []string{"Loft", "Upstairs"} {
		if shown != "Loft" {
			r.ok("loft", map[string]any{"name": shown})
		}
		rp := r.do("POST", "/api/admin/hosts/loft/repair", map[string]any{"confirm": "Loft"}, append([]string{"Content-Type", "application/json"}, r.hdr...)...)
		if rp.code != 201 {
			t.Fatalf("Re-pair of Loft while it is shown as %s: %d %s", shown, rp.code, rp.raw)
		}
		if e := r.enrollment(rp.body["id"].(string)); e["state"] != "online" || r.row("loft")["name"] != shown {
			t.Errorf("Re-pair of Loft shown as %s: %v %v, row %v", shown, e["state"], e["message"], r.row("loft")["name"])
		}
	}
	// ... and the name it was added under is the configuration's name of that host: no other host gets it in
	// Edit host (an Add host under it is the same id, and refused as that)
	if p := r.put("pc-b", map[string]any{"name": "loft"}); p.code != 422 || !strings.Contains(string(p.raw), "Loft is the name config.yaml has for another host (shown as Upstairs now)") {
		t.Errorf("Edit host, the name an added host was added under: %d %s", p.code, p.raw)
	}
}
