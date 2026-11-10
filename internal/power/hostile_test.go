package power

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// One hostile agent cannot make the panel keep an arbitrary number of power sources (M1): 100,000 relayed rows
// leave 16, ids must look like config ids, texts are cut, and the history (which copies every source into each
// sample) stays bounded. What was dropped is logged once, not on every poll.
func TestRelayedSourcesAreCappedPerAgent(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	pm := manager(t, nil, nil, nil)
	long := strings.Repeat("N", 1<<16)
	flood := func(round int) []map[string]any {
		rows := make([]map[string]any, 0, 100_000)
		for i := range 100_000 {
			rows = append(rows, map[string]any{"id": fmt.Sprintf("p%d-%d", round, i), "name": long, "type": long, "ok": true, "watts": 1.0})
		}
		return rows
	}
	for round := range 3 { // new ids on every poll: the old ones go, never more than 16 stay
		pm.UpdateRemote("evil", flood(round), false, nil, nil)
		pm.SampleHistory()
		ov := pm.Overview()
		if len(ov.Sources) != MaxRemoteSources {
			t.Fatalf("round %d: %d sources kept", round, len(ov.Sources))
		}
		for _, s := range ov.Sources {
			if len(s.Name) > maxRemoteName || len(s.Type) > maxRemoteType || !strings.HasPrefix(s.ID, fmt.Sprintf("evil/p%d-", round)) {
				t.Fatalf("%+v", s)
			}
		}
	}
	pm.mu.Lock()
	keys, states := len(pm.remoteKeys), len(pm.remote)
	pm.mu.Unlock()
	if keys != MaxRemoteSources || states != MaxRemoteSources {
		t.Fatal("kept", keys, states)
	}
	h := pm.HistoryView("")
	if len(h.Points) != 3 {
		t.Fatal("points", len(h.Points))
	}
	for _, p := range h.Points {
		if len(p.Sources) > MaxRemoteSources {
			t.Fatalf("%d sources in one history sample", len(p.Sources))
		}
	}
	if b, err := json.Marshal(h); err != nil || len(b) > 16<<10 {
		t.Fatal("history", len(b), err)
	}
	if n := strings.Count(logs.String(), "were dropped"); n != 1 {
		t.Fatalf("dropped rows logged %d times:\n%s", n, logs.String())
	}

	// ids that could not come from a config file are refused; a well-behaved agent next to it is untouched
	pm.UpdateRemote("evil", []map[string]any{{"id": "../../etc", "ok": true}, {"id": "UPPER", "ok": true}, {"id": strings.Repeat("a", 64), "ok": true},
		{"id": "a b", "ok": true}, {"id": "fine-1", "ok": true, "watts": 5.0}, {"id": 7.0, "ok": true}, {"id": "", "ok": true}}, false, nil, nil)
	pm.UpdateRemote("laptop", []map[string]any{{"id": "desk", "name": "Desk plug", "type": "tuya", "ok": true, "watts": 80.0}}, false, nil, nil)
	ov := byID(pm.Overview())
	if len(ov) != 3 || ov["evil/fine-1"].Status != "ok" || ov["evil/7"].Status != "ok" || ov["laptop/desk"].Name != "Desk plug" || ov["laptop/desk"].Type != "tuya" {
		t.Fatalf("%+v", ov)
	}
	// the same id twice counts once, and the last row wins (as before)
	rows := []map[string]any{}
	for i := range 40 {
		rows = append(rows, map[string]any{"id": "same", "ok": true, "watts": float64(i)})
	}
	pm.UpdateRemote("laptop", rows, false, nil, nil)
	if d := byID(pm.Overview())["laptop/same"]; *d.Readings[0].Watts != 39 {
		t.Fatalf("%+v", d)
	}
	// still one log line while it lasts; once the agent behaved and then misbehaves again, that is logged again
	if n := strings.Count(logs.String(), "were dropped"); n != 1 {
		t.Fatalf("logged %d times", n)
	}
	pm.UpdateRemote("evil", []map[string]any{{"id": "fine-1", "ok": true, "watts": 5.0}}, false, nil, nil)
	pm.UpdateRemote("evil", flood(9), false, nil, nil)
	if n := strings.Count(logs.String(), "were dropped"); n != 2 {
		t.Fatalf("logged %d times", n)
	}
}
