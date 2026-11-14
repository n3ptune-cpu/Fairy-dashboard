package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// "Edit host": an admin changes, in the panel, which UPS a machine is on, which plugs are its plugs and which
// of them count in the total (PUT /api/admin/hosts/{id}/options). These tests ask the real handler chain who
// may do that, what can be sent, where it is kept, when it counts and what never can be changed this way.

func init() {
	// The route's row in the route / auth matrix (routes_test.go): guarded like the Users page's changes. The
	// laptop of the plain fixture relays one plug, "desk", which is its own.
	matrixRoutes = append(matrixRoutes, writeRoute{pattern: "PUT /api/admin/hosts/{id}/options", path: "/api/admin/hosts/laptop/options",
		body: map[string]any{"plugs": []any{map[string]any{"id": "laptop/desk", "in_total": false}}}, adminOnly: true, stepUp: true})
}

// ehRig is a panel with two standalone Proxmox hosts, PVE-A and PVE-B, each with an agent that is its node's
// row (lab-a-agent in the row lab-a-node-pve-a, lab-b-agent in lab-b-node-pve-b), and a third machine, NAS,
// whose agent reads the devices: the UPSes rack and desk, and the plugs a-plug, b-plug, corner and own (which
// the agent itself says is its own machine's). config.yaml links the UPS rack and the plug a-plug to PVE-A
// and leaves corner out of the total. Nothing is linked to PVE-B.
type ehRig struct {
	*env
	hdr []string // an admin's cookie and the page's header
	// bUp: the Proxmox API of PVE-B answers (its node's row is there)
	mu  sync.Mutex
	bUp bool
}

const (
	ehNodeA, ehNodeB = "lab-a-node-pve-a", "lab-b-node-pve-b"
	ehNAS            = "nas-agent"
	ehAdmin          = "n@example.com (unverified)" // the audit log's name for the admin of these tests
)

func ehConfig(t *testing.T, nasLinks string) *config.Panel {
	t.Helper()
	t.Setenv("TOK_NAS", testutil.ReadToken)
	t.Setenv("TOK_LAB_A", testutil.ReadToken+"a")
	t.Setenv("TOK_LAB_B", testutil.ReadToken+"b")
	t.Setenv("CTL_LAB", testutil.ControlToken)
	t.Setenv("PVE_SECRET", "pve-secret-uuid")
	dir := t.TempDir()
	y := `
public_url: https://panel.example.com
request_timeout: 1
offline_after: 60
audit_log: ` + fmt.Sprintf("%q", dir+"/audit.log") + `
data_dir: ` + fmt.Sprintf("%q", dir+"/data") + `
oidc: {issuer: "https://id.example.com", client_id: cid, allowed_groups: [viewers], control_groups: [controllers]}
proxmox:
  - {id: lab-a, id_prefix: lab-a-, cluster_name: PVE-A, standalone: true, urls: ["https://pve-a:8006"],
     token_id: "fairy@pve!panel", token_secret_env: PVE_SECRET, verify_tls: false}
  - {id: lab-b, id_prefix: lab-b-, cluster_name: PVE-B, standalone: true, urls: ["https://pve-b:8006"],
     token_id: "fairy@pve!panel", token_secret_env: PVE_SECRET, verify_tls: false}
agents:
  - {id: nas-agent, name: NAS, url: "http://nas-agent", token_env: TOK_NAS, ` + nasLinks + `}
  - {id: lab-a-agent, name: PVE-A, url: "http://lab-a-agent", token_env: TOK_LAB_A, control_token_env: CTL_LAB, node: PVE-A, wol_relay: true}
  - {id: lab-b-agent, name: PVE-B, url: "http://lab-b-agent", token_env: TOK_LAB_B, control_token_env: CTL_LAB, node: PVE-B,
     power_control: true, self_update: true}
`
	cfg, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

const ehNASLinks = `ups_powers: {rack: [lab-a-node-pve-a]}, power_links: {a-plug: lab-a-agent}, power_exclude_from_total: [corner]`

func nasMetrics() jsonx.M {
	m := testutil.AgentMetrics()
	m["hostname"] = "nas"
	m["ups"] = []any{jsonx.M{"name": "rack", "state": "online", "description": "Rack UPS", "battery_charge": 100.0},
		jsonx.M{"name": "desk", "state": "online", "description": "Desk UPS", "battery_charge": 90.0}}
	plug := func(id, name string, w float64) jsonx.M {
		return jsonx.M{"id": id, "name": name, "type": "homeassistant", "ok": true, "watts": w}
	}
	own := plug("own", "NAS plug", 30)
	own["system_hint"] = "self"
	m["power_sources"] = []any{plug("a-plug", "A plug", 100), plug("b-plug", "B plug", 200), plug("corner", "Corner plug", 400), own}
	return m
}

func ehSetup(t *testing.T) *ehRig { return ehSetupWith(t, ehConfig(t, ehNASLinks), true) }

func ehSetupWith(t *testing.T, cfg *config.Panel, bUp bool) *ehRig {
	t.Helper()
	r := &ehRig{bUp: bUp}
	b := testutil.NewBackend()
	b.PVEOf = func(cluster string) ([]jsonx.M, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		switch {
		case cluster == "lab-a":
			return testutil.HostResources("PVE-A", "stopped"), nil
		case cluster == "lab-b" && r.bUp:
			return testutil.HostResources("PVE-B", "stopped"), nil
		}
		return nil, devices.Errf("connection failed")
	}
	for _, id := range []string{"lab-a-agent", "lab-b-agent"} {
		b.Metrics[id] = func() (jsonx.M, error) {
			m := testutil.AgentMetrics()
			m["hostname"] = id
			delete(m, "power_sources")
			return m, nil
		}
	}
	b.Metrics[ehNAS] = func() (jsonx.M, error) { return nasMetrics(), nil }
	r.env = ehStart(t, cfg, b)
	r.hdr = []string{"Cookie", r.cookie(controller(), 0), "X-Requested-With", "fairy"}
	return r
}

// ehStart starts a panel on cfg's folders (a first start, or a restart on the folders of another one).
func ehStart(t *testing.T, cfg *config.Panel, b *testutil.Backend) *env {
	t.Helper()
	p, err := poller.New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	p.PollOnce(context.Background()) // the node's rows and their agents in one list
	s := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, p, nil, "test")
	s.adminRL = newRateLimiter(10000)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, s: s, srv: srv, b: b}
}

// again is the panel started again on the same folders (state.json, the audit log) with cfg (nil: the same
// configuration).
func (r *ehRig) again(cfg *config.Panel) *ehRig {
	r.t.Helper()
	if cfg == nil {
		cfg = r.s.Cfg
	}
	cfg.DataDir, cfg.AuditLog = r.s.Cfg.DataDir, r.s.Cfg.AuditLog
	n := &ehRig{bUp: r.bUp}
	r.b.Mu.Lock()
	r.b.PVEOf = func(cluster string) ([]jsonx.M, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		switch {
		case cluster == "lab-a":
			return testutil.HostResources("PVE-A", "stopped"), nil
		case cluster == "lab-b" && n.bUp:
			return testutil.HostResources("PVE-B", "stopped"), nil
		}
		return nil, devices.Errf("connection failed")
	}
	r.b.Mu.Unlock()
	n.env = ehStart(r.t, cfg, r.b)
	n.hdr = []string{"Cookie", n.cookie(controller(), 0), "X-Requested-With", "fairy"}
	return n
}

func (r *ehRig) poll() { r.s.Poller.PollOnce(context.Background()) }

func (r *ehRig) options(id string) resp {
	r.t.Helper()
	g := r.do("GET", "/api/admin/hosts/"+id+"/options", nil, r.hdr[:2]...)
	if g.code != 200 {
		r.t.Fatalf("GET options of %s: %d %s", id, g.code, g.raw)
	}
	return g
}

func (r *ehRig) put(id string, body any) resp {
	r.t.Helper()
	return r.do("PUT", "/api/admin/hosts/"+id+"/options", body, append([]string{"Content-Type", "application/json"}, r.hdr...)...)
}

// ok is put for a change that must be taken.
func (r *ehRig) ok(id string, body any) resp {
	r.t.Helper()
	p := r.put(id, body)
	if p.code != 200 || p.body["ok"] != true {
		r.t.Fatalf("PUT options of %s %v: %d %s", id, body, p.code, p.raw)
	}
	return p
}

func ehUPS(id string, on bool) map[string]any { return map[string]any{"id": id, "on": on} }

// entry returns one UPS or plug of an options answer by its id.
func ehEntry(t *testing.T, r resp, list, id string) map[string]any {
	t.Helper()
	l, _ := r.body[list].([]any)
	for _, x := range l {
		if m := x.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("no %s in %s of %s", id, list, r.raw)
	return nil
}

// stored is "host_options" of state.json as it is on the disk (nil: the key is not there).
func (e *env) storedOptions() map[string]any {
	e.t.Helper()
	st := map[string]any{}
	b, err := os.ReadFile(filepath.Join(e.s.Cfg.DataDir, "state.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		e.t.Fatalf("state.json is not JSON any more: %v\n%s", err, b)
	}
	o, _ := st["host_options"].(map[string]any)
	return o
}

// storedAgent is one agent's overrides in state.json (nil: none).
func (e *env) storedAgent(id string) map[string]any {
	e.t.Helper()
	a, _ := e.storedOptions()["agents"].(map[string]any)
	m, _ := a[id].(map[string]any)
	return m
}

// row is a system as the API hands it out.
func (r *ehRig) row(id string) map[string]any {
	r.t.Helper()
	g := r.do("GET", "/api/systems/"+id, nil, r.hdr[:2]...)
	if g.code != 200 {
		r.t.Fatalf("GET %s: %d %s", id, g.code, g.raw)
	}
	return g.body
}

func strs(v any) []string {
	l, _ := v.([]any)
	out := []string{}
	for _, x := range l {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

// The rig is what its comment says: both machines are their node's row, PVE-A is on the rack UPS and has the
// plug's watts, PVE-B has neither.
func TestEditHostRig(t *testing.T) {
	r := ehSetup(t)
	a, b := r.row(ehNodeA), r.row(ehNodeB)
	if got := strs(a["ups"]); !slices.Equal(got, []string{"nas-agent/rack"}) || a["power_w"] != 100.0 {
		t.Fatalf("PVE-A: ups %v, power_w %v", got, a["power_w"])
	}
	if got := strs(b["ups"]); len(got) != 0 || b["power_w"] != nil {
		t.Fatalf("PVE-B: ups %v, power_w %v", got, b["power_w"])
	}
	for _, id := range []string{"lab-a-agent", "lab-b-agent"} {
		if r.do("GET", "/api/systems/"+id, nil, r.hdr[:2]...).code != 404 {
			t.Fatalf("%s has a row of its own: the rig is not two nodes with their agents", id)
		}
	}
	if r.storedOptions() != nil {
		t.Fatal("a new panel has no host_options")
	}
}

// The dialog's data. Every UPS and plug an agent relays is a choice, each with the agent that reads it, what
// counts now and where that comes from. Nothing is changed by looking.
func TestEditHostShowsWhatCountsAndWhereItComesFrom(t *testing.T) {
	r := ehSetup(t)
	g := r.options(ehNodeA)
	if g.body["id"] != ehNodeA || g.body["name"] != "PVE-A" || g.body["agent"] != "lab-a-agent" || g.body["rev"] != 0.0 {
		t.Fatalf("the machine: %s", g.raw)
	}
	rack, desk := ehEntry(t, g, "ups", "nas-agent/rack"), ehEntry(t, g, "ups", "nas-agent/desk")
	if rack["on"] != true || rack["from"] != "config" || rack["config_on"] != true || rack["agent"] != ehNAS || rack["agent_name"] != "NAS" || rack["name"] != "Rack UPS" {
		t.Errorf("rack: %v", rack)
	}
	if desk["on"] != false || desk["from"] != "config" {
		t.Errorf("desk: %v", desk)
	}
	ap, bp, own, corner := ehEntry(t, g, "plugs", "nas-agent/a-plug"), ehEntry(t, g, "plugs", "nas-agent/b-plug"), ehEntry(t, g, "plugs", "nas-agent/own"), ehEntry(t, g, "plugs", "nas-agent/corner")
	if ap["linked"] != true || ap["from"] != "config" || ap["config_linked"] != true || ap["in_total"] != true || ap["mine"] != true || ap["self"] != false || ap["other"] != nil {
		t.Errorf("a-plug on PVE-A's page: %v", ap)
	}
	if bp["linked"] != false || bp["mine"] != false || bp["other"] != nil {
		t.Errorf("b-plug (linked to nothing): %v", bp)
	}
	if own["linked"] != false || own["self"] != false || own["other"] != "NAS" || own["mine"] != false {
		t.Errorf("the NAS's own plug on PVE-A's page: %v", own)
	}
	if corner["in_total"] != false || corner["total_from"] != "config" || corner["config_in_total"] != false {
		t.Errorf("corner: %v", corner)
	}
	// the same plugs on the page of the machine that reads them: its own plug is linked by the machine itself,
	// and every plug it reads is one whose place in the total can be changed there
	n := r.options(ehNAS)
	if n.body["agent"] != ehNAS {
		t.Fatalf("the NAS: %s", n.raw)
	}
	if own := ehEntry(t, n, "plugs", "nas-agent/own"); own["linked"] != true || own["self"] != true || own["mine"] != true {
		t.Errorf("the NAS's own plug on its own page: %v", own)
	}
	if ap := ehEntry(t, n, "plugs", "nas-agent/a-plug"); ap["linked"] != false || ap["other"] != "PVE-A" || ap["mine"] != true {
		t.Errorf("a-plug on the NAS's page: %v", ap)
	}
	if ig, _ := g.body["ignored"].([]any); len(ig) != 0 {
		t.Errorf("ignored: %v", ig)
	}
	if r.storedOptions() != nil || len(r.auditEntries("", "host.options")) != 0 {
		t.Error("looking changed something")
	}
	// the agent's id names the same machine while its node's row is shown
	if byAgent := r.options("lab-a-agent"); byAgent.body["id"] != ehNodeA {
		t.Errorf("asked by the agent's id: %v", byAgent.body["id"])
	}
}

// Trap (a) and the round trip. "PVE-B is on the rack UPS" is a line of the entry of the NAS, which reads the
// UPS: the change is made on PVE-B's page and stored under nas-agent, and the answer, the audit entry and the
// bell name both. It counts with the NAS's next poll, without a restart, and it is the same after one. Reset
// takes it out, and config.yaml's value counts again.
func TestEditHostUPSLinkIsStoredWithTheAgentThatReadsTheUPS(t *testing.T) {
	r := ehSetup(t)
	p := r.ok(ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/rack", true)}})
	want := "UPS Rack UPS (read by NAS): powers PVE-B"
	if got := strs(p.body["changed"]); !slices.Equal(got, []string{want}) || p.body["message"] != "links saved" || p.body["rev"] != 1.0 {
		t.Fatalf("the answer: %s", p.raw)
	}
	if rack := ehEntry(t, p, "ups", "nas-agent/rack"); rack["on"] != true || rack["from"] != "panel" || rack["config_on"] != false || rack["by"] != ehAdmin {
		t.Fatalf("rack in the answer: %v", rack)
	}
	// stored under the agent that reads the UPS, with the id of the row PVE-B is shown as; nothing under PVE-B
	nas := r.storedAgent(ehNAS)
	ups, _ := nas["ups_powers"].(map[string]any)
	if got := strs(ups["rack"]); !slices.Equal(got, []string{ehNodeA, ehNodeB}) || nas["by"] != ehAdmin || nas["at"] == nil {
		t.Fatalf("state.json, nas-agent: %v", nas)
	}
	if _, has := nas["power_links"]; has || r.storedAgent("lab-b-agent") != nil || len(r.storedOptions()["agents"].(map[string]any)) != 1 {
		t.Fatalf("state.json holds more than the one field of the one agent: %v", r.storedOptions())
	}
	// the audit entry: the machine that was edited is the target, the agent whose entry is overridden is named
	en := r.auditEntries("", "host.options")
	if len(en) != 1 || en[0]["target"] != ehNodeB || en[0]["outcome"] != "ok" {
		t.Fatalf("audit: %v", en)
	}
	params := en[0]["params"].(map[string]any)
	ch := params["changes"].([]any)[0].(map[string]any)
	if params["machine"] != "PVE-B" || params["agent"] != "lab-b-agent" || len(params["changes"].([]any)) != 1 || !slices.Equal(strs(params["lines"]), []string{want}) ||
		ch["agent"] != ehNAS || ch["field"] != "ups_powers" || ch["before_from"] != "config.yaml" || ch["after_from"] != "panel" ||
		!slices.Equal(strs(ch["before"].(map[string]any)["rack"]), []string{ehNodeA}) || !slices.Equal(strs(ch["after"].(map[string]any)["rack"]), []string{ehNodeA, ehNodeB}) {
		t.Fatalf("audit params: %v", params)
	}
	// one entry in the bell, on the machine
	notes := r.do("GET", "/api/notifications", nil, r.hdr[:2]...)
	if n := strings.Count(string(notes.raw), `"kind":"host_options"`); n != 1 || !strings.Contains(string(notes.raw), `"text":"links changed by `+ehAdmin+`"`) ||
		!strings.Contains(string(notes.raw), `"system_id":"`+ehNodeB+`"`) {
		t.Fatalf("the bell: %d entries: %s", n, notes.raw)
	}
	// it counts with the next poll of the NAS: not before, and without a restart
	if got := strs(r.row(ehNodeB)["ups"]); len(got) != 0 {
		t.Fatalf("PVE-B has the UPS before the NAS was polled again: %v", got)
	}
	r.poll()
	if got := strs(r.row(ehNodeB)["ups"]); !slices.Equal(got, []string{"nas-agent/rack"}) {
		t.Fatalf("PVE-B after the next poll: %v", got)
	}
	if got := strs(r.row(ehNodeA)["ups"]); !slices.Equal(got, []string{"nas-agent/rack"}) {
		t.Fatalf("PVE-A lost the UPS config.yaml gives it: %v", got)
	}
	// the same request again changes nothing and is no second entry anywhere
	p2 := r.ok(ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/rack", true)}})
	if p2.body["message"] != "nothing to change" || p2.body["rev"] != 1.0 || len(strs(p2.body["changed"])) != 0 || len(r.auditEntries("", "host.options")) != 1 {
		t.Fatalf("the same change again: %s", p2.raw)
	}
	// (e) a restart: the same values from the first poll on, and the dialog says the same
	r2 := r.again(nil)
	if got := strs(r2.row(ehNodeB)["ups"]); !slices.Equal(got, []string{"nas-agent/rack"}) {
		t.Fatalf("after a restart: %v", got)
	}
	g := r2.options(ehNodeB)
	if rack := ehEntry(t, g, "ups", "nas-agent/rack"); rack["on"] != true || rack["from"] != "panel" || g.body["rev"] != 1.0 {
		t.Fatalf("the dialog after a restart: %v", rack)
	}
	// Reset: the override of that field of that agent goes; config.yaml's value shows again
	p3 := r2.ok(ehNodeB, map[string]any{"reset": []any{map[string]any{"agent": ehNAS, "field": "ups"}}})
	if got := strs(p3.body["changed"]); !slices.Equal(got, []string{"UPS links read by NAS: back to config.yaml"}) || p3.body["rev"] != 2.0 {
		t.Fatalf("reset: %s", p3.raw)
	}
	if rack := ehEntry(t, p3, "ups", "nas-agent/rack"); rack["on"] != false || rack["from"] != "config" || rack["by"] != nil {
		t.Fatalf("rack after Reset: %v", rack)
	}
	if r2.storedAgent(ehNAS) != nil {
		t.Fatalf("state.json after Reset: %v", r2.storedOptions())
	}
	r2.poll()
	if got := strs(r2.row(ehNodeB)["ups"]); len(got) != 0 {
		t.Fatalf("PVE-B after Reset and a poll: %v", got)
	}
	if got := strs(r2.row(ehNodeA)["ups"]); !slices.Equal(got, []string{"nas-agent/rack"}) {
		t.Fatalf("PVE-A after Reset: %v", got)
	}
	last := r2.auditEntries("", "host.options")[0]["params"].(map[string]any)["changes"].([]any)[0].(map[string]any)
	if last["before_from"] != "panel" || last["after_from"] != "config.yaml" || !slices.Equal(strs(last["after"].(map[string]any)["rack"]), []string{ehNodeA}) {
		t.Fatalf("the audit entry of the Reset: %v", last)
	}
	// a Reset of a field that has no override changes nothing
	if p := r2.ok(ehNodeB, map[string]any{"reset": []any{map[string]any{"agent": ehNAS, "field": "ups"}}}); p.body["message"] != "nothing to change" || p.body["rev"] != 2.0 {
		t.Fatalf("a second Reset: %s", p.raw)
	}
}

// Plugs and the total. A plug's link is written with the machine's agent id, which names the machine whether
// its node's row is there or not (trap g); the watts move with the next poll. A plug can be moved from another
// machine, taken out of the total and put back. What cannot be meant is refused.
func TestEditHostPlugLinksAndTheTotal(t *testing.T) {
	r := ehSetup(t)
	plug := func(id string, kv ...any) map[string]any {
		m := map[string]any{"id": id}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	// b-plug becomes PVE-B's, on the page of PVE-B's node row
	p := r.ok(ehNodeB, map[string]any{"plugs": []any{plug("nas-agent/b-plug", "linked", true)}})
	if got := strs(p.body["changed"]); !slices.Equal(got, []string{"Plug B plug (read by NAS): linked to PVE-B"}) {
		t.Fatalf("changed: %v", got)
	}
	links, _ := r.storedAgent(ehNAS)["power_links"].(map[string]any)
	if links["b-plug"] != "lab-b-agent" || links["a-plug"] != "lab-a-agent" || len(links) != 2 {
		t.Fatalf("power_links of nas-agent in state.json (config.yaml's link stays, the new one is the agent's id): %v", links)
	}
	if r.row(ehNodeB)["power_w"] != nil {
		t.Fatal("the watts are there before the NAS was polled again")
	}
	r.poll()
	if w := r.row(ehNodeB)["power_w"]; w != 200.0 {
		t.Fatalf("PVE-B's watts after the next poll: %v", w)
	}
	if w := r.row(ehNodeA)["power_w"]; w != 100.0 {
		t.Fatalf("PVE-A's watts: %v", w)
	}
	// it is PVE-B's plug now: its place in the total can be changed there; the total follows
	total := func() any { return r.do("GET", "/api/power", nil, r.hdr[:2]...).body["total_w"] }
	before := total()
	p = r.ok(ehNodeB, map[string]any{"plugs": []any{plug("nas-agent/b-plug", "in_total", false)}})
	if got := strs(p.body["changed"]); !slices.Equal(got, []string{"Plug B plug (read by NAS): not in the total"}) {
		t.Fatalf("changed: %v", got)
	}
	if ex := strs(r.storedAgent(ehNAS)["power_exclude_from_total"]); !slices.Equal(ex, []string{"b-plug", "corner"}) {
		t.Fatalf("power_exclude_from_total in state.json: %v", ex)
	}
	r.poll()
	if after := total(); before.(float64)-after.(float64) != 200 {
		t.Fatalf("the total: %v before, %v after b-plug was taken out", before, after)
	}
	// a-plug is moved from PVE-A to PVE-B: the line says where it was
	p = r.ok(ehNodeB, map[string]any{"plugs": []any{plug("nas-agent/a-plug", "linked", true)}})
	if got := strs(p.body["changed"]); !slices.Equal(got, []string{"Plug A plug (read by NAS): linked to PVE-B (was on PVE-A)"}) {
		t.Fatalf("changed: %v", got)
	}
	r.poll()
	if a, b := r.row(ehNodeA)["power_w"], r.row(ehNodeB)["power_w"]; a != nil || b != 300.0 {
		t.Fatalf("after the move: PVE-A %v, PVE-B %v", a, b)
	}
	// unlinked again, and both in one request with the total
	p = r.ok(ehNodeB, map[string]any{"plugs": []any{plug("nas-agent/a-plug", "linked", false), plug("nas-agent/b-plug", "in_total", true)}})
	if got := strs(p.body["changed"]); !slices.Equal(got, []string{"Plug A plug (read by NAS): no longer linked to PVE-B", "Plug B plug (read by NAS): in the total again"}) {
		t.Fatalf("changed: %v", got)
	}
	// what cannot be meant: 422, and nothing is stored for it
	stored := fmt.Sprint(r.storedOptions())
	for name, c := range map[string]struct {
		id   string
		body map[string]any
		want string
	}{
		"the total of a plug that is not this machine's": {ehNodeB, map[string]any{"plugs": []any{plug("nas-agent/corner", "in_total", true)}}, "Corner plug is not a plug of PVE-B"},
		"unlinking the plug the machine itself links":    {ehNAS, map[string]any{"plugs": []any{plug("nas-agent/own", "linked", false)}}, "NAS plug is linked by the machine itself"},
		"a plug no agent relays":                         {ehNodeB, map[string]any{"plugs": []any{plug("nas-agent/nope", "linked", true)}}, "no such plug"},
		"a plug of something that is no agent":           {ehNodeB, map[string]any{"plugs": []any{plug("elsewhere/b-plug", "linked", true)}}, "no such plug"},
		"a plug with nothing to change":                  {ehNodeB, map[string]any{"plugs": []any{plug("nas-agent/b-plug")}}, "neither"},
		"a plug twice":                                   {ehNodeB, map[string]any{"plugs": []any{plug("nas-agent/b-plug", "linked", true), plug("nas-agent/b-plug", "linked", false)}}, "twice"},
		"a UPS no agent relays":                          {ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/attic", true)}}, "no such UPS"},
		"a UPS without on":                               {ehNodeB, map[string]any{"ups": []any{map[string]any{"id": "nas-agent/rack"}}}, "no such UPS"},
		"a UPS twice":                                    {ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/rack", true), ehUPS("nas-agent/rack", true)}}, "twice"},
		"a reset of a field there is not":                {ehNodeB, map[string]any{"reset": []any{map[string]any{"agent": ehNAS, "field": "url"}}}, "no such field"},
		"a reset for an agent that reads nothing":        {ehNodeB, map[string]any{"reset": []any{map[string]any{"agent": "lab-a-agent", "field": "ups"}}}, "no such field"},
		"an empty request":                               {ehNodeB, map[string]any{}, "nothing to change"},
	} {
		got := r.put(c.id, c.body)
		if d, _ := got.body["detail"].(string); got.code != 422 || !strings.Contains(d, c.want) {
			t.Errorf("%s: %d %s (want 422 with %q)", name, got.code, got.raw, c.want)
		}
		if now := fmt.Sprint(r.storedOptions()); now != stored {
			t.Fatalf("%s: refused, and state.json changed:\n%s\n%s", name, stored, now)
		}
	}
	// the total of a plug its own agent reads can be changed on that machine's page, linked or not
	p = r.ok(ehNAS, map[string]any{"plugs": []any{plug("nas-agent/corner", "in_total", true)}})
	if got := strs(p.body["changed"]); !slices.Equal(got, []string{"Plug Corner plug (read by NAS): in the total again"}) {
		t.Fatalf("changed: %v", got)
	}
	// the NAS's own plug can be given to another machine (a link wins over what the agent says), and given back
	r.ok(ehNodeB, map[string]any{"plugs": []any{plug("nas-agent/own", "linked", true)}})
	r.poll()
	if w := r.row(ehNodeB)["power_w"]; w != 230.0 {
		t.Fatalf("PVE-B with the NAS's plug: %v", w)
	}
	r.ok(ehNodeB, map[string]any{"plugs": []any{plug("nas-agent/own", "linked", false)}})
	r.poll()
	if w := r.row(ehNAS)["power_w"]; w != 30.0 {
		t.Fatalf("the NAS has its own plug back: %v", w)
	}
	// not a host, and not there
	if got := r.put("lab-a-qemu-100", map[string]any{"ups": []any{ehUPS("nas-agent/rack", true)}}); got.code != 409 {
		t.Errorf("a VM: %d %s", got.code, got.raw)
	}
	if got := r.put("no-such-host", map[string]any{"ups": []any{ehUPS("nas-agent/rack", true)}}); got.code != 404 {
		t.Errorf("an unknown host: %d %s", got.code, got.raw)
	}
	if got := r.do("GET", "/api/admin/hosts/lab-a-qemu-100/options", nil, r.hdr[:2]...); got.code != 409 {
		t.Errorf("GET for a VM: %d", got.code)
	}
}

// The closed set. Nothing that says where an agent is, which token it has, which node it is or what the panel
// may do to a machine has a field in this API: a request that names one is refused whole, whatever else is in
// it, at the top and inside every entry, and neither the stored overrides nor the agent's entry change.
func TestEditHostNeverEditable(t *testing.T) {
	r := ehSetup(t)
	never := []string{"id", "url", "token_env", "control_token_env", "verify_tls", "node", "links_to", "fan_control", "power_control", "self_update",
		"wol_relay", "owner", "site", "name"}
	entry := func(a config.Agent) string {
		return fmt.Sprintf("%s|%s|%s|%s|%v|%s|%s|%v|%v|%v|%v|%v|%s|%s", a.ID, a.URL, a.TokenEnv, a.ControlTokenEnv, a.VerifyTLS, a.Node, a.LinksTo, a.FanControl,
			a.PowerControl, a.SelfUpdate, a.WolRelay, model.Deref(a.Owner), a.Site, a.Name)
	}
	agents := func() string {
		var out []string
		for _, a := range r.s.Poller.Agents() {
			out = append(out, entry(a))
		}
		return strings.Join(out, "\n")
	}
	before := agents()
	good := ehUPS("nas-agent/rack", true)
	n := 0
	for _, f := range append(never, "agents", "host_options", "rev", "ups_powers", "power_links", "power_exclude_from_total", "wol", "wake_update", "by") {
		for _, v := range []any{"http://198.51.100.7:9101", true, map[string]any{"vendor": "supermicro"}} {
			for where, body := range map[string]map[string]any{
				"at the top":          {"ups": []any{good}, f: v},
				"alone":               {f: v},
				"in a UPS entry":      {"ups": []any{map[string]any{"id": "nas-agent/rack", "on": true, f: v}}},
				"in a plug entry":     {"plugs": []any{map[string]any{"id": "nas-agent/b-plug", "linked": true, f: v}}},
				"in a reset entry":    {"reset": []any{map[string]any{"agent": ehNAS, "field": "ups", f: v}}},
				"under an agent's id": {"agents": map[string]any{ehNAS: map[string]any{f: v}}},
			} {
				if f == "id" && where != "at the top" && where != "alone" && where != "in a reset entry" && where != "under an agent's id" {
					continue // "id" is the name of a UPS or plug there
				}
				n++
				got := r.put(ehNodeB, body)
				if got.code != 422 {
					t.Errorf("%q %s (%T): %d %s", f, where, v, got.code, got.raw)
				}
			}
		}
	}
	if n < 300 {
		t.Fatalf("only %d requests were tried", n)
	}
	if r.storedOptions() != nil {
		t.Errorf("a refused request stored something: %v", r.storedOptions())
	}
	if agents() != before {
		t.Errorf("an agent's entry changed:\n%s\n%s", before, agents())
	}
	if len(r.auditEntries("", "host.options")) != 0 {
		t.Error("a request that was refused for its form is in the audit log as a change")
	}
	// a field under another spelling, twice, or after the JSON value: one reading only
	for name, raw := range map[string]string{
		"UPS in capitals":      `{"UPS":[{"id":"nas-agent/rack","on":true}]}`,
		"ups twice":            `{"ups":[],"ups":[{"id":"nas-agent/rack","on":true}]}`,
		"on twice":             `{"ups":[{"id":"nas-agent/rack","on":false,"on":true}]}`,
		"something after it":   `{"ups":[{"id":"nas-agent/rack","on":true}]} {"url":"x"}`,
		"a list, not a object": `[{"id":"nas-agent/rack","on":true}]`,
		"on as a text":         `{"ups":[{"id":"nas-agent/rack","on":"yes"}]}`,
	} {
		got := r.doRaw("PUT", "/api/admin/hosts/"+ehNodeB+"/options", raw, append([]string{"Content-Type", "application/json"}, r.hdr...)...)
		if got.code != 422 || r.storedOptions() != nil {
			t.Errorf("%s: %d %s", name, got.code, got.raw)
		}
	}
	// and the one thing that is taken: the same UPS entry without anything else
	r.ok(ehNodeB, map[string]any{"ups": []any{good}})
	if agents() != before {
		t.Error("a change of a link changed an agent's entry")
	}
	// what is stored has the three link fields, who and when, and no other key
	for id, e := range r.storedOptions()["agents"].(map[string]any) {
		for k := range e.(map[string]any) {
			if !slices.Contains([]string{"ups_powers", "power_links", "power_exclude_from_total", "by", "at"}, k) {
				t.Errorf("state.json: %s has the key %q", id, k)
			}
		}
	}
}
