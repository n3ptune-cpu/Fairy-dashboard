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

	// start 2: its tokens file is damaged
	tokens := filepath.Join(cfg.DataDir, "secrets", "agent-box-a.json")
	good, err := os.ReadFile(tokens)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokens, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, skipped = ehStartAdded(t, cfg, b)
	if len(skipped) != 1 || !strings.Contains(skipped[0], "Re-pair it") {
		t.Fatalf("start 2 left out: %v", skipped)
	}
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
	if len(skipped) != 0 {
		t.Fatal("start 3 left out:", skipped)
	}
	if got := onRack(e); !slices.Equal(got, []string{"laptop/rack"}) {
		t.Errorf("BOX-A is back and its UPS link is gone: %v", got)
	}
	if now := fmt.Sprint(e.storedOptions()["agents"]); now != stored {
		t.Errorf("state.json when BOX-A is back:\nbefore %s\nnow    %s", stored, now)
	}
}
