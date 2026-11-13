package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// After a restart of the panel a machine that is off is shown with the time it was last seen (build 13: "last
// seen never"; the time was in memory only). The time is read from last-seen.json and is for the row alone.
// "Update unpowered hosts" must not learn anything from it: a machine counts as off after a quarter of an hour
// of polls without an answer, by a clock that starts with the panel, so right after the start the machine is
// "not sure", as without the file, although the file says it was last seen three hours ago; and it is "off"
// when the quarter of an hour has passed, as without the file.
func TestLastSeenFromBeforeARestartIsShownAndDecidesNothing(t *testing.T) {
	seenAt := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC) // the farm's clock starts at 03:00
	start := func(kept bool) (*twoHosts, string) {
		cfg := testutil.TwoHostsConfig(t, "")
		if kept {
			if err := adminstate.New(cfg.DataDir).SetAgentSeen(map[string]time.Time{"lab-b-agent": seenAt}, nil); err != nil {
				t.Fatal(err)
			}
		}
		h := setupNodeOff(t, cfg, &twoHosts{agent: "lab-b-agent", node: "lab-b-node-pve-b", cluster: "lab-b", name: "PVE-B",
			others: map[string]string{"lab-a": "PVE-A"}}, "lab-a-agent")
		h.f.optIn("lab-b-agent")
		return h, h.e.cookie(controller(), 0)
	}
	type view struct {
		lastSeen, status string
		row              string // the machine's row of GET /api/admin/wake-update, as JSON
	}
	look := func(h *twoHosts, admin string) view {
		t.Helper()
		sys := h.e.do("GET", "/api/systems/lab-b-agent", nil, "Cookie", admin)
		if sys.code != 200 {
			t.Fatalf("the host's page: %d %s", sys.code, sys.raw)
		}
		v := view{status: sys.body["status"].(string)}
		if s, ok := sys.body["last_seen"].(string); ok {
			v.lastSeen = s
		}
		row, _ := json.Marshal(wuHostRow(t, h.e.do("GET", "/api/admin/wake-update", nil, "Cookie", admin), "lab-b-agent"))
		v.row = string(row)
		return v
	}
	plain, plainAdmin := start(false)
	kept, keptAdmin := start(true)
	for _, step := range []struct {
		name  string
		pass  func(h *twoHosts)
		power string
	}{
		{"right after the start", func(h *twoHosts) { h.f.tick() }, "unclear"},
		{"a minute and a half later", func(h *twoHosts) {
			for i := 0; i < 90; i++ {
				h.f.tick()
			}
		}, "unclear"},
		{"a quarter of an hour of polls later", func(h *twoHosts) { h.f.wait(15 * time.Minute) }, "off"},
	} {
		step.pass(plain)
		step.pass(kept)
		a, b := look(plain, plainAdmin), look(kept, keptAdmin)
		if a.lastSeen != "" || a.status != "offline" {
			t.Fatalf("%s, without the file: last seen %q, %s", step.name, a.lastSeen, a.status)
		}
		if at, err := time.Parse(time.RFC3339, b.lastSeen); err != nil || !at.Equal(seenAt) || b.status != "offline" {
			t.Fatalf("%s, with the file: last seen %q (want %s), %s", step.name, b.lastSeen, seenAt.Format(time.RFC3339), b.status)
		}
		if a.row != b.row {
			t.Fatalf("%s: \"Update unpowered hosts\" says something else with the file:\n  without: %s\n  with:    %s", step.name, a.row, b.row)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(b.row), &row); err != nil || row["power"] != step.power || (row["not_sure"] == true) != (step.power == "unclear") {
			t.Fatalf("%s: the machine's row: %s", step.name, b.row)
		}
	}
}
