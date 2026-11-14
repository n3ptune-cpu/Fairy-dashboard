package web

import (
	"context"
	"errors"
	"slices"
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

// What is NOT refused: the row a new host becomes a part of is its own, whatever it is called (an agent for
// a Proxmox node the panel has, added under the node's name), and a guest's name (a virtual machine that
// gets an agent of its own is added under its name). And a node keeps being found by its own name after
// "Edit host" gave its row another one: Add host lists it by the name the machine answers with.
func TestAddHostOnANodeAndUnderAGuestsName(t *testing.T) {
	e := setup(t)
	e.s.control = newRateLimiter(100)
	e.s.Enroll.PanelAddr = "100.64.0.10"
	e.s.Enroll.Pair = func(ctx context.Context, url string, _ []byte) (int, enroll.Response, string, error) {
		return 0, enroll.Response{}, "", errors.New("dial tcp: connection refused") // nobody answers: the codes stay waiting
	}
	hdr := []string{"Cookie", e.cookie(controller(), 0), "X-Requested-With", "fairy", "Content-Type", "application/json"}
	add := func(body map[string]any) resp { return e.do("POST", "/api/admin/hosts", body, hdr...) }
	if p := e.do("PUT", "/api/admin/hosts/node-srv-pvea/options", map[string]any{"name": "Rack node"}, hdr...); p.code != 200 {
		t.Fatalf("the node is given a name in Edit host: %d %s", p.code, p.raw)
	}
	g := e.do("GET", "/api/admin/hosts", nil, hdr[:2]...)
	var nodes []string
	for _, n := range g.body["nodes"].([]any) {
		m := n.(map[string]any)
		shown, _ := m["shown"].(string)
		nodes = append(nodes, m["name"].(string)+"|"+shown+"|"+m["system_id"].(string))
	}
	slices.Sort(nodes)
	if !slices.Equal(nodes, []string{"SRV-PVEA|Rack node|node-srv-pvea", "SRV-PVEB||node-srv-pveb"}) {
		t.Fatalf("the nodes of the Add host dialog: %v", nodes)
	}
	// the dialog's choice says both names, and sends the node's own
	if !strings.Contains(appJS(t), "`<option value=\"${esc(n.name)}\">Yes: ${esc(n.name)}${n.cluster ? ` (${esc(n.cluster)})` : \"\"}, already in the panel${n.shown ? ` as ${esc(n.shown)}` : \"\"}</option>`") {
		t.Error("app.js: the node choice of Add host does not say the name the row shows any more")
	}
	// on the renamed node, by the node's own name
	if p := add(map[string]any{"name": "BOX-A", "os": "linux", "node": "SRV-PVEA"}); p.code != 201 {
		t.Errorf("Add host on the node that Edit host renamed: %d %s", p.code, p.raw)
	}
	if p := add(map[string]any{"name": "BOX-B", "os": "linux", "node": "Rack node"}); p.code != 400 {
		t.Errorf("Add host on a node by the name its row shows: %d %s", p.code, p.raw)
	}
	// an agent for a node the panel has, called like the node: the node's row is its own
	if p := add(map[string]any{"name": "SRV-PVEB", "os": "linux", "node": "SRV-PVEB"}); p.code != 201 {
		t.Errorf("Add host of a node's agent under the node's name: %d %s", p.code, p.raw)
	}
	// ... and not as a machine of its own next to that node
	for name, want := range map[string]string{
		"srv-pveb-x": "",
		"TrueNAS":    "", // a guest of a node: its name is not asked
	} {
		if p := add(map[string]any{"name": name, "os": "linux"}); p.code != 201 {
			t.Errorf("Add host %q: %d %s (%s)", name, p.code, p.raw, want)
		}
	}
	if p := add(map[string]any{"name": "Srv-Pvea", "os": "linux"}); p.code != 400 || !strings.Contains(string(p.raw), "name: SRV-PVEA is the name config.yaml has for another host (shown as Rack node now)") {
		t.Errorf("Add host of a second machine under a node's own name: %d %s", p.code, p.raw)
	}
}
