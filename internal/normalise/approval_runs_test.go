package normalise

import (
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// The "runs" and "run" members of an agent's approval block (Fairy-agent v0.9.0), read as defensively as the
// rest of the block: only the one version of runs this panel speaks, and a run only when it is what the agent
// can send. Anything else reads as "says nothing about runs", which keeps the machine out of a run.
func TestApprovalBlockRuns(t *testing.T) {
	block := func(extra jsonx.M) jsonx.M {
		b := jsonx.M{"state": "required", "enforced_by": "agent", "proto": 2.0, "machine_id": "machine-id-of-pc-a-0000001"}
		for k, v := range extra {
			b[k] = v
		}
		return jsonx.M{"approval": b}
	}
	const id = "0123456789abcdef"
	for _, c := range []struct {
		name      string
		extra     jsonx.M
		runs      int
		state     string
		offAt     int64
		wantNoRun bool
	}{
		{"an agent from before runs", nil, 0, "", 0, true},
		{"runs 1, no run", jsonx.M{"runs": 1.0}, 1, "", 0, true},
		{"armed", jsonx.M{"runs": 1.0, "run": jsonx.M{"id": id, "state": "armed", "off_at": 1794650000.0}}, 1, "armed", 1794650000, false},
		{"cancelled, no time", jsonx.M{"runs": 1.0, "run": jsonx.M{"id": id, "state": "cancelled"}}, 1, "cancelled", 0, false},
		{"another version of runs", jsonx.M{"runs": 2.0, "run": jsonx.M{"id": id, "state": "armed"}}, 0, "", 0, true},
		{"runs as a text", jsonx.M{"runs": "1"}, 0, "", 0, true},
		{"a run without runs", jsonx.M{"run": jsonx.M{"id": id, "state": "armed"}}, 0, "", 0, true},
		{"an id that is none", jsonx.M{"runs": 1.0, "run": jsonx.M{"id": "0123456789ABCDEF", "state": "armed"}}, 1, "", 0, true},
		{"a state nobody knows", jsonx.M{"runs": 1.0, "run": jsonx.M{"id": id, "state": "armed "}}, 1, "", 0, true},
		{"a time that is none", jsonx.M{"runs": 1.0, "run": jsonx.M{"id": id, "state": "armed", "off_at": -5.0}}, 1, "armed", 0, false},
		{"a run that is no object", jsonx.M{"runs": 1.0, "run": "armed"}, 1, "", 0, true},
	} {
		ap := AgentApproval(block(c.extra))
		if ap == nil {
			t.Fatalf("%s: no block", c.name)
		}
		if ap.Runs != c.runs || (ap.Run == nil) != c.wantNoRun {
			t.Errorf("%s: runs %d run %+v", c.name, ap.Runs, ap.Run)
			continue
		}
		if ap.Run != nil && (ap.Run.ID != id || ap.Run.State != c.state || ap.Run.OffAt != c.offAt) {
			t.Errorf("%s: run %+v", c.name, ap.Run)
		}
	}
}
