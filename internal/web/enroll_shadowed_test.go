package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// hostRow is the row of one added host in GET /api/admin/hosts (nil: it is not listed).
func hostRow(t *testing.T, e *env, cookie, id string) map[string]any {
	t.Helper()
	g := e.do("GET", "/api/admin/hosts", nil, "Cookie", cookie)
	if g.code != 200 {
		t.Fatal(g.code, string(g.raw))
	}
	rows, _ := g.body["managed"].([]any)
	for _, r := range rows {
		if m := r.(map[string]any); m["id"] == id {
			return m
		}
	}
	return nil
}

// Check of the build 15 candidate, K1. An added host whose id config.yaml has too ("a host with the same id
// in config.yaml wins") is in the list of added hosts and is not the host that is polled: the machine is
// polled as config.yaml's entry. The Add host dialog listed it under "not polled right now", with Remove. That
// Remove did what the Remove of a real added host does: it took out what "Edit host" had stored for the
// machine, its model name and its switch for "Update unpowered hosts", each with the audit entry "host
// removed". All three are config.yaml's host's, which stays. (The same Remove was on the machine's own page
// before: its box "Added with Add host".)
//
// Remove of such a copy takes the copy away, its entry in the list and its tokens, and nothing else; its row
// says what it is; and it is not paired again, because the panel would refuse what the machine answers.
func TestRemoveOfAnAddHostCopyThatConfigYAMLShadowsTakesOnlyTheCopy(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := managed.NewStore(cfg.DataDir)
	if err := st.Put(managed.Host{ID: "laptop", Name: "LAPTOP", URL: "http://laptop-old:9101", OS: "linux", AddedAt: time.Now().UTC()},
		managed.Secret{Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	b := testutil.NewBackend()
	b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["ups"] = []any{jsonx.M{"name": "rack", "state": "online", "description": "Rack UPS"}}
		return m, nil
	}
	e, skipped := ehStartAdded(t, cfg, b)
	if len(skipped) != 1 {
		t.Fatal("the fixture: the copy is not left out:", skipped)
	}
	e.s.control, e.s.adminRL = newRateLimiter(1000), newRateLimiter(1000)
	e.s.Enroll.PanelAddr, e.s.Enroll.Scripts = "100.64.0.10", testInstallers
	paired := 0
	e.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
		paired++
		return 200, enroll.Response{AgentVersion: "42-abc", Platform: "linux", Token: strings.Repeat("r", 32)}, "", nil
	}
	c := e.cookie(controller(), 0)
	hdr := []string{"Cookie", c, "X-Requested-With", "fairy", "Content-Type", "application/json"}

	// the row says what it is
	row := hostRow(t, e, c, "laptop")
	wantWhy := "config.yaml has an entry with the id laptop, and config.yaml wins: the machine is polled as that entry, and this copy from Add host is not used"
	if row == nil || row["active"] != false || row["shadowed"] != true || row["why"] != wantWhy {
		t.Fatalf("the row of the copy: %v\nwant why %q", row, wantWhy)
	}

	// what the panel holds for the machine, which is config.yaml's host
	if r := e.do("PUT", "/api/admin/hosts/laptop/options", map[string]any{"ups": []any{ehUPS("laptop/rack", true)},
		"plugs": []any{map[string]any{"id": "laptop/desk", "in_total": false}}}, hdr...); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	if _, err := e.s.Poller.Admin.SetHostModel("laptop", "My laptop"); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Poller.Admin.SetWakeUpdate("laptop", true); err != nil {
		t.Fatal(err)
	}
	before := fmt.Sprint(e.storedOptions()["agents"])
	audited := func(action string) int { return len(e.auditEntries("", action)) }
	nOpt, nModel, nSwitch := audited("host.options"), audited("host.model"), audited("wakeupdate.switch")

	// it is not paired again: refused before a code is made
	p := e.do("POST", "/api/admin/hosts/laptop/repair", map[string]any{"confirm": "LAPTOP"}, hdr...)
	wantRepair := "LAPTOP is polled as config.yaml's entry (config.yaml has the id laptop, and config.yaml wins): the copy made by Add host is not used, and it is not paired again. " +
		"The panel would refuse what the machine answers, and the machine may then hold tokens that config.yaml's entry does not have. " +
		"Remove the unused copy; or take the entry out of config.yaml, restart the panel, and Re-pair then"
	if p.code != 409 || p.body["detail"] != wantRepair {
		t.Fatalf("Re-pair of the copy: %d %s\nwant %s", p.code, p.raw, wantRepair)
	}
	if l, _ := e.do("GET", "/api/admin/hosts", nil, "Cookie", c).body["enrollments"].([]any); len(l) != 0 || paired != 0 {
		t.Fatalf("the refused Re-pair left an enrollment or asked the machine: %v, %d", l, paired)
	}
	refused := false
	for _, en := range e.auditEntries("", "host.repair") {
		refused = refused || (en["target"] == "laptop" && en["outcome"] == "refused: "+wantRepair)
	}
	if !refused {
		t.Fatalf("the refused Re-pair is not in the audit log: %v", e.auditEntries("", "host.repair"))
	}

	// Remove: the copy goes, and nothing of the machine
	r := e.do("DELETE", "/api/admin/hosts/laptop", map[string]any{"confirm": "LAPTOP"}, hdr...)
	wantMsg := "the unused Add host copy of LAPTOP was removed. LAPTOP stays in the panel as config.yaml's entry, and nothing that is stored for it was changed"
	if r.code != 200 || r.body["message"] != wantMsg {
		t.Fatalf("Remove of the copy: %d %s\nwant %s", r.code, r.raw, wantMsg)
	}
	e.s.Poller.PollOnce(context.Background())
	if x := e.do("GET", "/api/systems/laptop", nil, "Cookie", c); x.code != 200 || x.body["status"] != "online" {
		t.Errorf("config.yaml's host after the Remove: %d %v", x.code, x.body["status"])
	}
	if now := fmt.Sprint(e.storedOptions()["agents"]); now != before {
		t.Errorf("the Remove took what Edit host stored for config.yaml's host:\nbefore %s\nnow    %s", before, now)
	}
	if m := e.s.Poller.Admin.HostModel("laptop"); m != "My laptop" {
		t.Errorf("the Remove took the model name of config.yaml's host: %q", m)
	}
	if !e.s.Poller.Admin.WakeUpdateEnabled("laptop") {
		t.Error("the Remove switched config.yaml's host off for \"Update unpowered hosts\"")
	}
	if audited("host.options") != nOpt || audited("host.model") != nModel || audited("wakeupdate.switch") != nSwitch {
		t.Errorf("the Remove wrote audit entries about the machine: host.options %d, host.model %d, wakeupdate.switch %d more",
			audited("host.options")-nOpt, audited("host.model")-nModel, audited("wakeupdate.switch")-nSwitch)
	}
	// the copy is gone: its entry, its tokens, its row (at once, not at the next start)
	if hosts, _ := st.Load(); len(hosts) != 0 {
		t.Errorf("the copy is still in the list: %+v", hosts)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "secrets", "agent-laptop.json")); !os.IsNotExist(err) {
		t.Errorf("the copy's tokens file is still there: %v", err)
	}
	if row := hostRow(t, e, c, "laptop"); row != nil {
		t.Errorf("the copy is still listed: %v", row)
	}
	var last map[string]any
	for _, en := range e.auditEntries("", "host.remove") {
		if en["target"] == "laptop" && en["outcome"] == "ok" {
			last, _ = en["params"].(map[string]any)
		}
	}
	if last == nil || last["copy_only"] != true || last["wake_update_was_on"] != false || last["custom_model_removed"] != false {
		t.Errorf("the audit entry of the Remove: %v", last)
	}
}

// K2 and K3. What the Add host dialog is told about an added host that is not polled: why, in fixed words and
// with the tokens file's name without its folder, never the folder and never anything of the file; and as it
// is now, not as it was when the panel started. A host that is removed is gone from the list at once, one
// that is paired again is polled and no longer in it, and one whose files were put right by hand is said to be
// polled from the next start.
func TestAddHostDialogIsToldWhyAHostIsNotPolledAsItIsNow(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st := managed.NewStore(cfg.DataDir)
	for _, id := range []string{"box-a", "box-b", "box-c", "box-d"} {
		if err := st.Put(managed.Host{ID: id, Name: strings.ToUpper(id), URL: "http://" + id + ":9101", OS: "linux", AddedAt: time.Now().UTC()},
			managed.Secret{Token: strings.Repeat("t", 32)}); err != nil {
			t.Fatal(err)
		}
	}
	file := func(id string) string { return filepath.Join(cfg.DataDir, "secrets", "agent-"+id+".json") }
	const inFile = `{"token": "QQ-this-is-in-the-file-QQ" ]` // not JSON: the error names a character of it
	if err := os.WriteFile(file("box-a"), []byte(inFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file("box-b")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file("box-c"), []byte(`{"token": "short"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b := testutil.NewBackend()
	for _, id := range []string{"box-a", "box-b", "box-c", "box-d"} {
		b.Metrics[id] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	}
	e, skipped := ehStartAdded(t, cfg, b)
	if len(skipped) != 3 || !strings.Contains(strings.Join(skipped, "\n"), cfg.DataDir) {
		t.Fatal("the fixture: three hosts left out, and the lines for the log name the files in full:", skipped)
	}
	e.s.ManagedSkipped = skipped
	e.s.Enroll.Interval = 10 * time.Millisecond
	e.s.control, e.s.adminRL = newRateLimiter(1000), newRateLimiter(1000)
	e.s.Enroll.PanelAddr, e.s.Enroll.Scripts = "100.64.0.10", testInstallers
	e.s.Enroll.Pair = func(context.Context, string, []byte) (int, enroll.Response, string, error) {
		return 200, enroll.Response{AgentVersion: "42-abc", Platform: "linux", Token: strings.Repeat("r", 32)}, "", nil
	}
	c := e.cookie(controller(), 0)
	hdr := []string{"Cookie", c, "X-Requested-With", "fairy", "Content-Type", "application/json"}

	for id, want := range map[string]string{
		"box-a": "its tokens file agent-box-a.json is not valid JSON",
		"box-b": "its tokens file agent-box-b.json is missing",
		"box-c": "its tokens file agent-box-c.json does not hold usable tokens",
	} {
		if row := hostRow(t, e, c, id); row == nil || row["active"] != false || row["shadowed"] != false || row["why"] != want {
			t.Errorf("%s: %v\nwant why %q", id, row, want)
		}
	}
	if row := hostRow(t, e, c, "box-d"); row == nil || row["active"] != true || row["why"] != nil {
		t.Errorf("a host that is polled: %v", row)
	}
	// nothing of a file, and no folder, in what the browser gets
	page := e.do("GET", "/api/admin/hosts", nil, "Cookie", c)
	for _, never := range []string{cfg.DataDir, "QQ", "invalid character", "looking for", "secrets/"} {
		if strings.Contains(string(page.raw), never) {
			t.Errorf("GET /api/admin/hosts has %q in it:\n%s", never, page.raw)
		}
	}
	if s, _ := json.Marshal(page.body["skipped"]); string(s) != "null" {
		t.Errorf("the start's own lines about hosts are sent to the page: %s", s)
	}

	// K3: the tokens file of box-c is put right by hand. The panel does not poll it before its next start, and says so
	good, _ := os.ReadFile(file("box-d"))
	if err := os.WriteFile(file("box-c"), good, 0o600); err != nil {
		t.Fatal(err)
	}
	if row := hostRow(t, e, c, "box-c"); row == nil || row["active"] != false || row["why"] != "nothing is wrong with its files any more: it is polled from the next start of the panel, or after a Re-pair" {
		t.Errorf("box-c, with its tokens file put right: %v", row)
	}
	// Remove: gone from the list at once
	if r := e.do("DELETE", "/api/admin/hosts/box-b", map[string]any{"confirm": "BOX-B"}, hdr...); r.code != 200 || r.body["message"] != "BOX-B removed from the panel" {
		t.Fatalf("Remove of box-b: %d %s", r.code, r.raw)
	}
	if row := hostRow(t, e, c, "box-b"); row != nil {
		t.Errorf("box-b is still listed after its Remove: %v", row)
	}
	// Re-pair: polled as soon as it has paired, and then it is a host like every other
	p := e.do("POST", "/api/admin/hosts/box-a/repair", map[string]any{"confirm": "BOX-A"}, hdr...)
	if p.code != 201 {
		t.Fatal("Re-pair of box-a:", p.code, string(p.raw))
	}
	ended := false
	for i := 0; i < 300 && !ended; i++ {
		time.Sleep(10 * time.Millisecond)
		e.s.Poller.PollOnce(context.Background())
		g := e.do("GET", "/api/admin/enrollments/"+p.body["id"].(string), nil, "Cookie", c)
		ended = g.body["done"] == true && g.body["state"] == "online"
	}
	if row := hostRow(t, e, c, "box-a"); !ended || row == nil || row["active"] != true || row["why"] != nil {
		t.Errorf("box-a after its Re-pair (ended online: %v): %v", ended, row)
	}
}
