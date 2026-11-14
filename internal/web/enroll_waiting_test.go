package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Re-review of Edit host round 2, H1. A host that was added in the panel and that a start left out (its tokens
// file is damaged: it waits for a Re-pair) is not in the running configuration, and an Add host under its name
// was accepted like any other: 201, a join code. When the machine paired, the new host took the waiting host's
// place in managed-hosts.yaml, and the clean-up of an added host (Hooks.Fresh) took out what "Edit host" had
// stored for the waiting one, with the audit entry "host added". The README says of such a host that what was
// changed for it is kept and counts again after the Re-pair.
//
// The Add is refused now, with a sentence that names the two things to do instead, and nothing is stored. The
// host's own Re-pair goes on working: the machine is polled again with the tokens of that pairing, and what
// was changed for it counts again.
func TestAddHostUnderTheIdOfAnAddedHostThatWaitsForRepairIsRefused(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := managed.NewStore(cfg.DataDir)
	if err := st.Put(managed.Host{ID: "box-a", Name: "BOX-A", URL: "http://box-a:9101", OS: "linux", AddedAt: time.Now().UTC()},
		managed.Secret{Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	b := testutil.NewBackend()
	b.Metrics["box-a"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["ups"] = []any{jsonx.M{"name": "rack", "state": "online", "description": "Rack UPS"}}
		return m, nil
	}
	e, _ := ehStartAdded(t, cfg, b)
	hdr := []string{"Cookie", e.cookie(controller(), 0), "X-Requested-With", "fairy", "Content-Type", "application/json"}
	if r := e.do("PUT", "/api/admin/hosts/box-a/options", map[string]any{"ups": []any{ehUPS("laptop/rack", true)}}, hdr...); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	before := fmt.Sprint(e.storedOptions()["agents"])
	if !strings.Contains(before, "box-a") {
		t.Fatal("the fixture: nothing is stored about box-a:", before)
	}
	// the tokens file is damaged; the next start leaves the host out
	tokens := filepath.Join(cfg.DataDir, "secrets", "agent-box-a.json")
	if err := os.WriteFile(tokens, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	e2, skipped := ehStartAdded(t, cfg, b)
	if len(skipped) != 1 || !strings.Contains(skipped[0], "Re-pair it") {
		t.Fatal("the fixture: the host is not left out:", skipped)
	}
	e2.s.Enroll.Interval = 10 * time.Millisecond
	e2.s.control, e2.s.adminRL = newRateLimiter(1000), newRateLimiter(1000)
	e2.s.Enroll.PanelAddr, e2.s.Enroll.Scripts = "100.64.0.10", testInstallers
	e2.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
		return 200, enroll.Response{AgentVersion: "42-abc", Platform: "linux", Token: strings.Repeat("r", 32), ControlToken: strings.Repeat("c", 32)}, "", nil
	}
	c := e2.cookie(controller(), 0)
	hdr = []string{"Cookie", c, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	list, _ := os.ReadFile(st.Path())

	// the page is told that the host is there and not polled, and why (the Add host dialog lists it)
	if row := hostRow(t, e2, c, "box-a"); row == nil || row["active"] != false || row["shadowed"] != false || row["why"] != "its tokens file agent-box-a.json is not valid JSON" {
		t.Fatalf("GET /api/admin/hosts: the row of box-a: %v", row)
	}

	// 1. the Add under its name
	p := e2.do("POST", "/api/admin/hosts", map[string]any{"name": "BOX-A", "os": "linux", "site": "Alice"}, hdr...)
	want := "the panel already has a host with the id box-a (BOX-A, added with Add host) that is not polled right now: " +
		"the list of this dialog says why. Re-pair that host, or Remove it first; an Add under its id would take its place. Nothing was stored"
	if p.code != 400 || p.body["detail"] != want {
		t.Fatalf("the Add was not refused with the sentence: %d %s\nwant %s", p.code, p.raw, want)
	}
	if now := fmt.Sprint(e2.storedOptions()["agents"]); now != before {
		t.Fatalf("the refused Add changed what is stored for the waiting host:\nbefore %s\nnow    %s", before, now)
	}
	if now, _ := os.ReadFile(st.Path()); string(now) != string(list) {
		t.Fatalf("the refused Add changed the list of added hosts:\n%s", now)
	}
	if now, _ := os.ReadFile(tokens); string(now) != "{" {
		t.Fatal("the refused Add wrote the tokens file")
	}
	if l, _ := e2.do("GET", "/api/admin/hosts", nil, "Cookie", c).body["enrollments"].([]any); len(l) != 0 {
		t.Fatalf("the refused Add left an enrollment: %v", l)
	}
	refused := false
	for _, en := range e2.auditEntries("", "host.add") {
		refused = refused || (en["target"] == "box-a" && en["outcome"] == "refused: "+want)
	}
	if !refused {
		t.Fatalf("the refusal is not in the audit log like other refused adds: %v", e2.auditEntries("", "host.add"))
	}

	// 2. its Re-pair
	p = e2.do("POST", "/api/admin/hosts/box-a/repair", map[string]any{"confirm": "BOX-A"}, hdr...)
	if p.code != 201 {
		t.Fatal("the Re-pair of the waiting host:", p.code, string(p.raw))
	}
	state := ""
	for i := 0; i < 300 && state == ""; i++ {
		time.Sleep(10 * time.Millisecond)
		e2.s.Poller.PollOnce(context.Background())
		if g := e2.do("GET", "/api/admin/enrollments/"+p.body["id"].(string), nil, "Cookie", c); g.body["done"] == true {
			state = fmt.Sprint(g.body["state"], " ", g.body["message"])
		}
	}
	if !strings.HasPrefix(state, "online ") {
		t.Fatal("the Re-pair did not end online:", state)
	}
	// the machine is the panel's again, with the tokens of this pairing at the address it was added with
	a, ok := e2.s.Poller.AgentByID("box-a")
	if !ok || !a.Managed || a.Token != strings.Repeat("r", 32) || a.ControlToken != strings.Repeat("c", 32) || a.URL != "http://box-a:9101" {
		t.Fatalf("the re-paired host's entry: there %v, managed %v, address %s", ok, a.Managed, a.URL)
	}
	if sec, err := st.Secret("box-a"); err != nil || sec.Token != strings.Repeat("r", 32) {
		t.Fatal("the Re-pair's tokens are not in the tokens file:", err)
	}
	// what was changed for it was kept, and counts again
	if now := fmt.Sprint(e2.storedOptions()["agents"]); now != before {
		t.Fatalf("the Re-pair changed what is stored for the host:\nbefore %s\nnow    %s", before, now)
	}
	e2.s.Poller.PollOnce(context.Background())
	if ups := strs(e2.do("GET", "/api/systems/box-a", nil, "Cookie", c).body["ups"]); !slices.Equal(ups, []string{"laptop/rack"}) {
		t.Fatalf("after the Re-pair the host's UPS link does not count: %v", ups)
	}
	for _, en := range e2.auditEntries("", "host.options") {
		if pp, _ := en["params"].(map[string]any); pp["because"] == "host added" {
			t.Fatalf("something was taken out as for an added host: %v", en)
		}
	}
	// and now that it is polled, an Add under its name is refused as it always was
	p = e2.do("POST", "/api/admin/hosts", map[string]any{"name": "BOX-A", "os": "linux", "site": "Alice"}, hdr...)
	if p.code != 400 || p.body["detail"] != "an agent with id box-a is already configured" {
		t.Fatalf("an Add under the id of a host that is polled: %d %s", p.code, p.raw)
	}
}
