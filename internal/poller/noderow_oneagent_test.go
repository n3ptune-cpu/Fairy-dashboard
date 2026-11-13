package poller

import (
	"context"
	"fmt"
	"testing"
)

// Two agents that name one node (two agents installed on one host, or an agent in a guest with the host's
// node:). The node's row is ONE agent's, the first of the configuration: its sensors, its approval block, and
// AgentFor and ShownSystem name it for the row. The second agent keeps a row of its own, so its check and
// its version are on a row too. Until build 13 the second was merged over the first and had no row: the
// node's row showed the second agent's block next to the first agent's record and buttons.
func TestTheRowOfANodeBelongsToOneAgent(t *testing.T) {
	p, _ := linkPanel(t, [][2]string{{"lab", "lab-"}},
		fmt.Sprintf(linkAgent, "first-agent", "First", "first", "node: pve")+fmt.Sprintf(linkAgent, "second-agent", "Second", "second", "node: pve"),
		"", map[string]string{"lab": "pve"})
	for i := 0; i < 3; i++ { // the same answer poll after poll, whichever agent answered last
		p.PollOnce(context.Background())
		rows := snap(p)
		if _, own := rows["first-agent"]; own {
			t.Error("the first agent has a row of its own")
		}
		second, own := rows["second-agent"]
		if !own || second.Kind != "agent" || len(second.Temps) == 0 {
			t.Errorf("the second agent has no row of its own: %v %+v", own, second)
		}
		if len(rows["lab-node-pve"].Temps) == 0 {
			t.Error("the node's row has no agent's sensors")
		}
		if a, ok := p.AgentFor("lab-node-pve"); !ok || a.ID != "first-agent" {
			t.Errorf("AgentFor(the node) = %q", a.ID)
		}
		if s := p.ShownSystem("first-agent"); s == nil || s.ID != "lab-node-pve" {
			t.Errorf("ShownSystem(first-agent) = %+v", s)
		}
		if s := p.ShownSystem("second-agent"); s == nil || s.ID != "second-agent" {
			t.Errorf("ShownSystem(second-agent) = %+v, want its own row", s)
		}
	}
}
