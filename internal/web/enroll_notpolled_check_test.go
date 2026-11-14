package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Check of build 15, L2. An added host can be left out at the panel's start with nothing wrong in its files:
// its entry does not pass the check every entry of config.yaml passes. Its Wake-on-LAN names a relay that is
// no relay any more (wol_relay was taken out of that agent's entry), or config.yaml got an entry of its own
// under systems: with the host's id. The Add host dialog says so, with what the check said: Re-pair does not
// help such a host, config.yaml has to change.
//
// Nothing held that sentence in place: with the check's answer thrown away (the mutant S13 of the review),
// every test passed, and such a host was told as "nothing is wrong with its files any more: it is polled from
// the next start of the panel, or after a Re-pair". It is not: the next start leaves it out again.
func TestAddHostDialogSaysThatAHostsEntryDoesNotPassThePanelsCheck(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Agents {
		if cfg.Agents[i].ID == "laptop" { // the one relay of this panel; tower is none
			cfg.Agents[i].WolRelay = true
		}
	}
	// config.yaml has an entry of its own for box-b, with more in it than the machine's timings for a run
	cfg.Systems = append(cfg.Systems, config.SystemOverride{ID: "box-b", Name: "The box under the desk"})
	if err := cfg.Validate(); err != nil {
		t.Fatal("the fixture's config.yaml:", err)
	}
	token := strings.Repeat("t", 32)
	st := managed.NewStore(cfg.DataDir)
	for _, h := range []managed.Host{
		{ID: "box-a", Wol: &config.Wol{MAC: "02:00:00:00:00:0a", Relay: "tower"}},  // its relay is no relay
		{ID: "box-b", Wol: &config.Wol{MAC: "02:00:00:00:00:0b", Relay: "laptop"}}, // config.yaml has systems: box-b
		{ID: "box-c", Wol: &config.Wol{MAC: "02:00:00:00:00:0c", Relay: "laptop"}}, // nothing in its way
	} {
		h.Name, h.URL, h.OS, h.AddedAt = strings.ToUpper(h.ID), "http://"+h.ID+":9101", "linux", time.Now().UTC()
		if err := st.Put(h, managed.Secret{Token: token}); err != nil {
			t.Fatal(err)
		}
	}
	b := testutil.NewBackend()
	for _, id := range []string{"box-a", "box-b", "box-c"} {
		b.Metrics[id] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	}
	e, skipped := ehStartAdded(t, cfg, b)
	const (
		noRelay  = `system box-a: wol.relay "tower" must be an agent with wol_relay: true`
		twoLines = `systems: in config.yaml already has an entry for box-b`
	)
	if len(skipped) != 2 || skipped[0] != "managed host box-a skipped: - "+noRelay || skipped[1] != "managed host box-b skipped: "+twoLines {
		t.Fatalf("the fixture: box-a and box-b are left out at the start, each for its entry:\n%s", strings.Join(skipped, "\n"))
	}
	for _, id := range []string{"box-a", "box-b"} {
		if p := st.TokensProblem(id); p != "" {
			t.Fatalf("the fixture: the tokens file of %s is fine, and: %s", id, p)
		}
	}
	c := e.cookie(controller(), 0)
	for id, want := range map[string]string{
		"box-a": "its entry does not pass the panel's check: " + noRelay,
		"box-b": "its entry does not pass the panel's check: " + twoLines,
	} {
		row := hostRow(t, e, c, id)
		if row == nil || row["active"] != false || row["shadowed"] != false || row["why"] != want {
			t.Errorf("%s: %v\nwant why %q", id, row, want)
		}
	}
	if row := hostRow(t, e, c, "box-c"); row == nil || row["active"] != true || row["why"] != nil {
		t.Errorf("a host whose entry passes is polled: %v", row)
	}
	// the check's own heading, the folder and the tokens are not for the page
	page := e.do("GET", "/api/admin/hosts", nil, "Cookie", c)
	for _, never := range []string{"config invalid", "nothing is wrong with its files", cfg.DataDir, token} {
		if strings.Contains(string(page.raw), never) {
			t.Errorf("GET /api/admin/hosts has %q in it:\n%s", never, page.raw)
		}
	}
	// as it is now: when the tokens file goes too, that is said first (it is what a Re-pair puts right)
	if err := os.Remove(filepath.Join(cfg.DataDir, "secrets", "agent-box-a.json")); err != nil {
		t.Fatal(err)
	}
	if row := hostRow(t, e, c, "box-a"); row == nil || row["active"] != false || row["why"] != "its tokens file agent-box-a.json is missing" {
		t.Errorf("box-a without its tokens file: %v", row)
	}
}
