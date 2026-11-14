package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// What "Edit host" calls gone, and what it does not: a stored change is offered for removal only when the
// machine it is about is no machine of this panel. A machine or a device that is merely not there at the
// moment is not gone, and one click must not take its links away.

// ehStartAdded starts a panel the way `fairy` does: cfg with the hosts added in the panel (managed-hosts.yaml
// in cfg's data folder) merged in. skipped is one line for each added host this start leaves out.
func ehStartAdded(t *testing.T, cfg *config.Panel, b *testutil.Backend) (e *env, skipped []string) {
	t.Helper()
	c := *cfg
	c.Agents, c.Systems, c.Proxmox = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems), slices.Clone(cfg.Proxmox)
	_, skipped, err := managed.NewStore(cfg.DataDir).Merge(&c)
	if err != nil {
		t.Fatal(err)
	}
	return ehStart(t, &c, b), skipped
}

// A host added in the panel whose tokens file is damaged is left out at the next start and waits for Re-pair
// ("tokens unreadable: Re-pair it"). It is still in the list of added hosts: it is a machine of this panel,
// and what was changed for it is not "about machines that are gone". Nothing of it is offered for removal on
// any page, a Remove that is sent all the same takes nothing of it, and when its tokens are right again its
// links are as they were.
func TestEditHostAnAddedHostThatIsLeftOutAtOneStartIsNotGone(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := managed.NewStore(cfg.DataDir)
	if err := st.Put(managed.Host{ID: "box-a", Name: "BOX-A", URL: "http://box-a", OS: "linux", AddedAt: time.Now().UTC()},
		managed.Secret{Token: strings.Repeat("r", 32)}); err != nil {
		t.Fatal(err)
	}
	b := testutil.NewBackend()
	b.Metrics["box-a"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil } // it relays a plug of its own, "desk"
	b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["ups"] = []any{jsonx.M{"name": "rack", "state": "online", "description": "Rack UPS"}}
		m["power_sources"] = append(m["power_sources"].([]any), jsonx.M{"id": "spare", "name": "Spare plug", "type": "shelly", "ok": true, "watts": 55.0})
		return m, nil
	}
	hdr := func(e *env) []string {
		return []string{"Cookie", e.cookie(controller(), 0), "X-Requested-With", "fairy", "Content-Type", "application/json"}
	}
	put := func(e *env, id string, body any) resp {
		t.Helper()
		p := e.do("PUT", "/api/admin/hosts/"+id+"/options", body, hdr(e)...)
		if p.code != 200 || p.body["ok"] != true {
			t.Fatalf("PUT options of %s %v: %d %s", id, body, p.code, p.raw)
		}
		return p
	}
	onRack := func(e *env) []string {
		t.Helper()
		e.s.Poller.PollOnce(context.Background())
		return strs(e.do("GET", "/api/systems/box-a", nil, hdr(e)[:2]...).body["ups"])
	}

	// start 1: the host is there; it is put on the laptop's UPS, gets the laptop's spare plug, and its own plug
	// is left out of the total
	e, skipped := ehStartAdded(t, cfg, b)
	if len(skipped) != 0 {
		t.Fatal("start 1 left out:", skipped)
	}
	put(e, "box-a", map[string]any{"ups": []any{ehUPS("laptop/rack", true)},
		"plugs": []any{map[string]any{"id": "laptop/spare", "linked": true}, map[string]any{"id": "box-a/desk", "in_total": false}}})
	if got := onRack(e); !slices.Equal(got, []string{"laptop/rack"}) {
		t.Fatalf("the rig: BOX-A is on the rack UPS: %v", got)
	}
	stored := fmt.Sprint(e.storedOptions()["agents"])

	// start 2: its tokens file is damaged. And a second added host is left out with it: BOX-N, added as "a new
	// Proxmox node", which is shown as its node's row; the laptop's entry keeps a change about that row
	tokens := filepath.Join(cfg.DataDir, "secrets", "agent-box-a.json")
	good, err := os.ReadFile(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokens, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	const nodeRow = "alice-box-n-node-box-n"
	boxN := managed.Host{ID: "box-n", Name: "BOX-N", URL: "http://box-n", OS: "linux", AddedAt: time.Now().UTC(), ProxmoxWanted: true, SystemID: nodeRow,
		Proxmox: &managed.PVE{ID: "alice-box-n", IDPrefix: "alice-box-n-", Node: "BOX-N", URL: "https://box-n:8006", ServerName: "BOX-N", TokenID: "fairy@pve!panel-box-n"}}
	if err := st.Put(boxN, managed.Secret{Token: strings.Repeat("n", 32)}); err != nil {
		t.Fatal(err)
	}
	if boxN.Shown() != nodeRow || boxN.Shown() == boxN.ID {
		t.Fatalf("the rig: BOX-N is shown as its node's row: %q", boxN.Shown())
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "secrets", "agent-box-n.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, skipped = ehStartAdded(t, cfg, b)
	if len(skipped) != 2 || !strings.Contains(skipped[0], "Re-pair it") || !strings.Contains(skipped[1], "Re-pair it") {
		t.Fatalf("start 2 left out: %v", skipped)
	}
	if _, _, err := e.s.Poller.Admin.EditHostOptions("earlier", func(o *adminstate.HostOptions) error {
		lap := o.Agents["laptop"].Clone()
		lap.SetUPS("rack", nodeRow, true)
		o.Agents["laptop"] = lap
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if e.s.Poller.KnownRow(nodeRow) || !strings.Contains(fmt.Sprint(e.storedAgent("laptop")["ups_powers"]), nodeRow) {
		t.Fatalf("the rig: a change about BOX-N's node row is stored, and the poller knows no such row at this start: %v", e.storedAgent("laptop"))
	}
	stored = fmt.Sprint(e.storedOptions()["agents"])
	for _, id := range []string{"laptop", "tower"} {
		g := e.do("GET", "/api/admin/hosts/"+id+"/options", nil, hdr(e)[:2]...)
		if g.code != 200 || g.body["leftover_count"] != 0.0 || len(strs(g.body["leftovers"])) != 0 {
			t.Errorf("%s's dialog while BOX-A waits for Re-pair offers its changes for removal: %v %v", id, g.body["leftover_count"], g.body["leftovers"])
		}
	}
	if p := put(e, "laptop", map[string]any{"leftovers": true}); p.body["message"] != "nothing to change" {
		t.Errorf("a Remove sent while BOX-A waits for Re-pair: %s", p.raw)
	}
	if now := fmt.Sprint(e.storedOptions()["agents"]); now != stored {
		t.Errorf("state.json after that Remove:\nbefore %s\nnow    %s", stored, now)
	}
	// ... and what really is about a machine that is gone is still found next to it, and goes alone
	put(e, "laptop", map[string]any{"ups": []any{ehUPS("laptop/rack", true)}})
	stored = fmt.Sprint(e.storedOptions()["agents"])
	state := filepath.Join(cfg.DataDir, "state.json")
	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	file := map[string]map[string]any{}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	file["host_options"]["agents"].(map[string]any)["old-pc"] = map[string]any{"power_exclude_from_total": map[string]any{"desk": true}}
	if raw, err = json.Marshal(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	e, _ = ehStartAdded(t, cfg, b)
	want := []string{"1 stored change kept with old-pc, which is no machine of this panel"}
	if g := e.do("GET", "/api/admin/hosts/laptop/options", nil, hdr(e)[:2]...); !slices.Equal(strs(g.body["leftovers"]), want) || g.body["leftover_count"] != 1.0 {
		t.Fatalf("with a real leftover next to it: %v %v", g.body["leftovers"], g.body["leftover_count"])
	}
	// the list of added hosts cannot be read: which ids are machines of this panel is not known, nothing is offered
	list := st.Path()
	hosts, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(list, []byte("hosts: {"), 0o600); err != nil {
		t.Fatal(err)
	}
	if g := e.do("GET", "/api/admin/hosts/laptop/options", nil, hdr(e)[:2]...); g.code != 200 || g.body["leftover_count"] != 0.0 || len(strs(g.body["leftovers"])) != 0 {
		t.Errorf("while managed-hosts.yaml cannot be read: %d %v %v", g.code, g.body["leftover_count"], g.body["leftovers"])
	}
	if err := os.WriteFile(list, hosts, 0o600); err != nil {
		t.Fatal(err)
	}
	if p := put(e, "laptop", map[string]any{"leftovers": true}); !slices.Equal(strs(p.body["changed"]), []string{"Removed: " + want[0]}) {
		t.Fatalf("Remove: %s", p.raw)
	}
	if now := fmt.Sprint(e.storedOptions()["agents"]); now != stored {
		t.Errorf("state.json after the Remove of what is gone (BOX-A's changes stay):\nbefore %s\nnow    %s", stored, now)
	}

	// start 3: the tokens file is right again
	if err := os.WriteFile(tokens, good, 0o600); err != nil {
		t.Fatal(err)
	}
	e, skipped = ehStartAdded(t, cfg, b)
	if len(skipped) != 1 || !strings.Contains(skipped[0], "box-n") {
		t.Fatal("start 3 left out (BOX-N still waits for Re-pair):", skipped)
	}
	if got := onRack(e); !slices.Equal(got, []string{"laptop/rack"}) {
		t.Errorf("BOX-A is back and its UPS link is gone: %v", got)
	}
	if now := fmt.Sprint(e.storedOptions()["agents"]); now != stored {
		t.Errorf("state.json when BOX-A is back:\nbefore %s\nnow    %s", stored, now)
	}
}

// A UPS or a plug that its machine does not relay at the moment is not gone. The panel cannot tell a device
// that was taken away from one whose NUT or Home Assistant is down when the agent is asked: both are an
// answer without the device. So it says what it knows ("does not relay right now"), counts such changes
// apart from those about machines that are gone, leaves them alone when those are removed, and takes them out
// only when a request names the device.
func TestEditHostADeviceThatIsNotRelayedRightNowIsNotGone(t *testing.T) {
	r := ehSetup(t)
	r.ok(ehNodeB, map[string]any{"ups": []any{ehUPS("nas-agent/rack", true)},
		"plugs": []any{map[string]any{"id": "nas-agent/b-plug", "linked": true, "in_total": false}}})
	r.ok(ehNodeA, map[string]any{"ups": []any{ehUPS("nas-agent/desk", true)}})
	stored := fmt.Sprint(r.storedOptions()["agents"])
	// the panel is started again while NUT on the NAS is down and its Home Assistant does not know b-plug: the
	// agent answers, without the UPSes and without that plug
	nutDown := func() (jsonx.M, error) {
		m := nasMetrics()
		m["ups"] = []any{jsonx.M{"name": "upsd", "ok": false, "error": "connection refused"}}
		m["power_sources"] = slices.DeleteFunc(m["power_sources"].([]any), func(x any) bool { return x.(jsonx.M)["id"] == "b-plug" })
		return m, nil
	}
	r.b.Mu.Lock()
	r.b.Metrics[ehNAS] = nutDown
	r.b.Mu.Unlock()
	r = r.again(nil)
	type away struct {
		id, kind, text string
		count          float64
	}
	unrelayed := func(g resp) (out []away) {
		t.Helper()
		l, _ := g.body["unrelayed"].([]any)
		for _, x := range l {
			m := x.(map[string]any)
			out = append(out, away{fmt.Sprint(m["id"]), fmt.Sprint(m["kind"]), fmt.Sprint(m["text"]), m["count"].(float64)})
		}
		return out
	}
	want := []away{{"nas-agent/desk", "ups", "1 stored change about the UPS desk, which NAS does not relay right now", 1},
		{"nas-agent/rack", "ups", "1 stored change about the UPS rack, which NAS does not relay right now", 1},
		{"nas-agent/b-plug", "plug", "2 stored changes about the plug b-plug, which NAS does not relay right now", 2}}
	for _, id := range []string{ehNodeA, ehNodeB, ehNAS} {
		g := r.options(id)
		if g.body["leftover_count"] != 0.0 || len(strs(g.body["leftovers"])) != 0 {
			t.Errorf("%s: what is not relayed right now is counted as about machines that are gone: %v %v", id, g.body["leftover_count"], g.body["leftovers"])
		}
		if got := unrelayed(g); !slices.Equal(got, want) || g.body["unrelayed_count"] != 4.0 {
			t.Errorf("%s: unrelayed %v (count %v), want %v (4)", id, got, g.body["unrelayed_count"], want)
		}
	}
	// the Remove of what is gone does not touch them
	if p := r.ok(ehNodeA, map[string]any{"leftovers": true}); p.body["message"] != "nothing to change" {
		t.Errorf("leftovers: true: %s", p.raw)
	}
	if now := fmt.Sprint(r.storedOptions()["agents"]); now != stored {
		t.Fatalf("state.json after a Remove of what is gone:\nbefore %s\nnow    %s", stored, now)
	}
	// only what a request names goes: the rack UPS, by its id and kind. A UPS id sent as a plug is no device
	// with stored changes; a device that is relayed (it has its line) is not taken either
	for _, body := range []map[string]any{
		{"unrelayed": []any{map[string]any{"id": "nas-agent/rack", "kind": "plug"}}},
		{"unrelayed": []any{map[string]any{"id": "nas-agent/a-plug", "kind": "plug"}}},
		{"unrelayed": []any{map[string]any{"id": "nas-agent/attic", "kind": "ups"}}},
	} {
		if p := r.ok(ehNodeA, body); p.body["message"] != "nothing to change" {
			t.Errorf("%v: %s", body, p.raw)
		}
	}
	for body, why := range map[string]string{
		`{"unrelayed": []}`:                                      "nothing to change",
		`{"unrelayed": true}`:                                    "invalid request body",
		`{"unrelayed": [{"id": "nas-agent/rack"}]}`:              "unrelayed: no such kind (ups or plug)",
		`{"unrelayed": [{"id": "nas-agent/rack", "kind": "x"}]}`: "unrelayed: no such kind (ups or plug)",
		`{"unrelayed": [{"id": "nas-agent/rack", "kind": "ups"}, {"id": "nas-agent/rack", "kind": "ups"}]}`: "unrelayed: an entry is there twice",
	} {
		if p := r.doRaw("PUT", "/api/admin/hosts/"+ehNodeA+"/options", body, append([]string{"Content-Type", "application/json"}, r.hdr...)...); p.code != 422 || !strings.Contains(string(p.raw), why) {
			t.Errorf("%s: %d %s (want 422, %q)", body, p.code, p.raw, why)
		}
	}
	if now := fmt.Sprint(r.storedOptions()["agents"]); now != stored {
		t.Fatalf("state.json after requests that name nothing that is stored and not relayed:\nbefore %s\nnow    %s", stored, now)
	}
	// an old sign-in is asked to confirm, with the line
	rack := map[string]any{"unrelayed": []any{map[string]any{"id": "nas-agent/rack", "kind": "ups"}}}
	removed := []string{"Removed: 1 stored change about the UPS rack, which NAS does not relay right now"}
	old := r.do("PUT", "/api/admin/hosts/"+ehNodeA+"/options", rack, "Cookie", r.staleCookie(controller(), time.Hour), "X-Requested-With", "fairy", "Content-Type", "application/json")
	if d, _ := old.body["detail"].(map[string]any); !stepUpRequired(old) || !slices.Equal(strs(d["items"]), removed) {
		t.Fatalf("an old sign-in is asked to confirm, with the line: %d %s", old.code, old.raw)
	}
	p := r.ok(ehNodeA, rack)
	if !slices.Equal(strs(p.body["changed"]), removed) || p.body["unrelayed_count"] != 3.0 {
		t.Fatalf("the rack UPS's change removed: %s", p.raw)
	}
	nas := r.storedAgent(ehNAS)
	if fmt.Sprint(nas["ups_powers"]) != "map[desk:map["+ehNodeA+":true]]" || fmt.Sprint(nas["power_links"]) != "map[b-plug:lab-b-agent]" || fmt.Sprint(nas["power_exclude_from_total"]) != "map[b-plug:true]" {
		t.Fatalf("state.json afterwards (the rack UPS's change alone is gone): %v", nas)
	}
	en := r.auditEntries("", "host.options")[0]
	if lines := strs(en["params"].(map[string]any)["lines"]); !slices.Equal(lines, removed) || en["outcome"] != "ok" || en["params"].(map[string]any)["removed_count"] != 1.0 {
		t.Fatalf("the audit entry: %v", en)
	}
	// NUT and the plug are back: the changes that were kept have their lines and count again; the one that was
	// removed does not
	r.b.Mu.Lock()
	r.b.Metrics[ehNAS] = func() (jsonx.M, error) { return nasMetrics(), nil }
	r.b.Mu.Unlock()
	r.poll()
	g := r.options(ehNodeA)
	if len(unrelayed(g)) != 0 || g.body["unrelayed_count"] != 0.0 || ehEntry(t, g, "ups", "nas-agent/desk")["on"] != true || ehEntry(t, g, "ups", "nas-agent/desk")["from"] != "panel" {
		t.Errorf("the devices are back: %s", g.raw)
	}
	// a device that is relayed has its line: a request that names it all the same (a dialog that was open
	// while the device came back) takes nothing
	stored = fmt.Sprint(r.storedOptions()["agents"])
	back := map[string]any{"unrelayed": []any{map[string]any{"id": "nas-agent/desk", "kind": "ups"}, map[string]any{"id": "nas-agent/b-plug", "kind": "plug"}}}
	if p := r.ok(ehNodeA, back); p.body["message"] != "nothing to change" || fmt.Sprint(r.storedOptions()["agents"]) != stored {
		t.Errorf("devices that are relayed again, named for removal: %s\nstate.json: %v", p.raw, r.storedOptions())
	}
	a, b := r.row(ehNodeA), r.row(ehNodeB)
	if on := strs(a["ups"]); !slices.Contains(on, "nas-agent/desk") || !slices.Contains(on, "nas-agent/rack") || len(strs(b["ups"])) != 0 || b["power_w"] != 200.0 {
		t.Errorf("after the devices are back: PVE-A ups %v (want desk, by the change that was kept, and rack, by config.yaml), PVE-B ups %v (want none) power_w %v (want 200, its plug)", a["ups"], b["ups"], b["power_w"])
	}
}

// Many devices that are not relayed right now: the dialog's data names the first twenty and counts all the
// changes; a request can only name what was listed, so the rest is listed (and can be named) afterwards.
func TestEditHostManyDevicesThatAreNotRelayedRightNow(t *testing.T) {
	cfg := ehConfig(t, ehNASLinks)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var ups, plugs []string
	for i := 0; i < 12; i++ {
		ups = append(ups, fmt.Sprintf(`"attic-%02d": {"lab-b-node-pve-b": true}`, i))
		plugs = append(plugs, fmt.Sprintf(`"shed-%02d": true`, i))
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "state.json"), []byte(`{"host_options": {"rev": 1, "agents": {"nas-agent": {
		"ups_powers": {`+strings.Join(ups, ",")+`}, "power_exclude_from_total": {`+strings.Join(plugs, ",")+`}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := ehSetupWith(t, cfg, true)
	g := r.options(ehNodeA)
	list, _ := g.body["unrelayed"].([]any)
	if len(list) != maxLeftoverLines || g.body["unrelayed_count"] != 24.0 || g.body["leftover_count"] != 0.0 {
		t.Fatalf("24 devices that are not relayed: %d entries, count %v, leftovers %v", len(list), g.body["unrelayed_count"], g.body["leftover_count"])
	}
	var named []any
	for _, x := range list {
		m := x.(map[string]any)
		named = append(named, map[string]any{"id": m["id"], "kind": m["kind"]})
	}
	p := r.ok(ehNodeA, map[string]any{"unrelayed": named})
	rest, _ := p.body["unrelayed"].([]any)
	if len(strs(p.body["changed"])) != maxLeftoverLines || len(rest) != 4 || p.body["unrelayed_count"] != 4.0 {
		t.Fatalf("after the twenty that were listed went: %d lines, %d entries left, count %v", len(strs(p.body["changed"])), len(rest), p.body["unrelayed_count"])
	}
	if en := r.auditEntries("", "host.options")[0]["params"].(map[string]any); en["removed_count"] != 20.0 {
		t.Fatalf("the audit entry: removed_count %v", en["removed_count"])
	}
}

// What a removal answers (and writes to the audit log, and shows in "Confirm it's you") is what the dialog
// listed, line for line, also when both kinds go in one request and one of them is longer than twenty lines:
// each kind is shortened by itself, as the dialog's data has it.
func TestEditHostARemovalSaysTheLinesThatWereListed(t *testing.T) {
	cfg := ehConfig(t, ehNASLinks)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var agents []string
	for i := 0; i < 25; i++ {
		agents = append(agents, fmt.Sprintf(`"gone-%02d": {"power_exclude_from_total": {"x": true}}`, i))
	}
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "state.json"), []byte(`{"host_options": {"rev": 1, "agents": {`+strings.Join(agents, ",")+`,
		"nas-agent": {"ups_powers": {"attic": {"lab-b-node-pve-b": true}}, "power_exclude_from_total": {"shed": true}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r := ehSetupWith(t, cfg, true)
	g := r.options(ehNodeA)
	var want []string
	for _, l := range strs(g.body["leftovers"]) {
		want = append(want, "Removed: "+l)
	}
	var named []any
	list, _ := g.body["unrelayed"].([]any)
	for _, x := range list {
		m := x.(map[string]any)
		want = append(want, "Removed: "+fmt.Sprint(m["text"]))
		named = append(named, map[string]any{"id": m["id"], "kind": m["kind"]})
	}
	if len(want) != 23 || want[20] != "Removed: 5 more of the same kind" || len(named) != 2 {
		t.Fatalf("the rig: 25 changes about machines that are gone (20 lines and how many more) and 2 devices that are not relayed: %q", want)
	}
	p := r.ok(ehNodeA, map[string]any{"leftovers": true, "unrelayed": named})
	if got := strs(p.body["changed"]); !slices.Equal(got, want) {
		t.Fatalf("the removal says\n%q\nthe dialog listed\n%q", got, want)
	}
	if en := r.auditEntries("", "host.options")[0]["params"].(map[string]any); !slices.Equal(strs(en["lines"]), want) || en["removed_count"] != 27.0 {
		t.Fatalf("the audit entry: removed_count %v, lines %q", en["removed_count"], strs(en["lines"]))
	}
	if r.storedOptions()["agents"] != nil && len(r.storedOptions()["agents"].(map[string]any)) != 0 {
		t.Fatalf("state.json afterwards: %v", r.storedOptions())
	}
}
