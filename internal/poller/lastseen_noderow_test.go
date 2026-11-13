package poller

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// The time a machine was last seen, kept across a restart of the panel, is also on the row of a NODE: the
// agent of a Proxmox node has no row of its own when its node's row is there, and a cluster lists a node that
// is off (its other nodes answer for it), so after a restart with such a machine off the row that is shown is
// the node's, made by a poll that never saw the node run: no time of its own. Without the kept time the row
// said "last seen never", the thing the kept time is for.
func TestTheRowOfANodeThatIsOffSinceTheStartSaysWhenItsAgentWasLastSeen(t *testing.T) {
	kept := seenStart.Add(-3 * time.Hour)
	e := newOffEnvWith(t, func(cfg *config.Panel) {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := `{"agents":{"lab-b-agent":"` + kept.UTC().Format(time.RFC3339) + `"}}`
		if err := os.WriteFile(filepath.Join(cfg.DataDir, "last-seen.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	e.set(false, "offline") // off since before the start: its agent is silent, its cluster lists the node as offline
	e.poll(0)
	rows := snap(e.p)
	row, ok := rows["lab-b-node-pve-b"]
	if _, own := rows["lab-b-agent"]; !ok || own || row.Kind != "node" {
		t.Fatalf("the fixture: the machine is shown as its node's row: %v", rows)
	}
	if row.Status != "offline" {
		t.Fatalf("the row's status: %s", row.Status)
	}
	if row.LastSeen == nil || !row.LastSeen.Equal(kept) {
		t.Fatalf("the node's row says it was last seen %v, want the kept time %v", row.LastSeen, kept)
	}
	// only in what is shown: the entries the poller decides with have no time
	e.p.mu.Lock()
	agent := e.p.systems["lab-b-agent"].LastSeen
	e.p.mu.Unlock()
	if agent != nil {
		t.Fatalf("the agent's own entry was given the kept time: %v", agent)
	}
	// the other host answers: its row has its own time, not a kept one
	if a := rows["lab-a-node-pve-a"]; a.LastSeen == nil || !a.LastSeen.Equal(e.now) {
		t.Fatalf("PVE-A: %v", a.LastSeen)
	}
}
