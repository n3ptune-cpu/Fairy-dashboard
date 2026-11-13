package poller

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/scheduler"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Second review of build 14, R6. The second agent of a node has a row of its own (a9aa8ad), and its poll job
// was still filed under the node's id. The row of the second agent had no poll job: its interval could not be
// set, "Pause" on it showed the row as paused while its agent was polled as before, and "Pause" on the node's
// row paused both agents.
func TestTheSecondAgentOfANodeHasThePollJobOfItsOwnRow(t *testing.T) {
	p, b := linkPanel(t, [][2]string{{"lab", "lab-"}},
		fmt.Sprintf(linkAgent, "first-agent", "First", "first", "node: pve")+fmt.Sprintf(linkAgent, "second-agent", "Second", "second", "node: pve"),
		"", map[string]string{"lab": "pve"})
	p.PollOnce(context.Background())
	jobs := func(id string) (all, paused []string) {
		for _, j := range p.PollStatus(id).Jobs {
			all = append(all, j.Job)
			if j.Paused {
				paused = append(paused, j.Job)
			}
		}
		return all, paused
	}
	if all, _ := jobs("second-agent"); !slices.Equal(all, []string{"agent:second-agent"}) {
		t.Fatalf("the poll jobs of the row second-agent: %v, want its own agent's", all)
	}
	if all, _ := jobs("lab-node-pve"); !slices.Equal(all, []string{"proxmox:lab", "agent:first-agent"}) {
		t.Fatalf("the poll jobs of the node's row: %v, want its Proxmox API and the agent the row belongs to", all)
	}
	asked := map[string]int{} // agent id -> how often it was polled (counted under the backend's lock)
	b.Mu.Lock()
	for _, id := range []string{"first-agent", "second-agent"} {
		b.Metrics[id] = func() (jsonx.M, error) { asked[id]++; return testutil.AgentMetrics(), nil }
	}
	b.Mu.Unlock()
	polls := func(agent string) int {
		b.Mu.Lock()
		defer b.Mu.Unlock()
		return asked[agent]
	}
	round := func() (first, second int) {
		f, s := polls("first-agent"), polls("second-agent")
		p.PollOnce(context.Background())
		return polls("first-agent") - f, polls("second-agent") - s
	}
	// Pause on the second agent's row: that agent is not polled any more, the node's row and its agent are
	if err := p.SetPoll("second-agent", &scheduler.Override{Paused: true}); err != nil {
		t.Fatal(err)
	}
	if _, paused := jobs("second-agent"); !slices.Equal(paused, []string{"agent:second-agent"}) {
		t.Errorf("after Pause on the row second-agent its job is not paused: %v", paused)
	}
	if _, paused := jobs("lab-node-pve"); len(paused) != 0 {
		t.Errorf("Pause on the row second-agent paused a job of the node's row: %v", paused)
	}
	rows := snap(p)
	if rows["second-agent"].Status != "paused" || rows["lab-node-pve"].Status == "paused" {
		t.Errorf("after Pause on the row second-agent: its row is %q, the node's row %q", rows["second-agent"].Status, rows["lab-node-pve"].Status)
	}
	if f, s := round(); f != 1 || s != 0 {
		t.Errorf("with the row second-agent paused, one round polled first-agent %d time(s) and second-agent %d, want 1 and 0", f, s)
	}
	// and the other way round: Pause on the node's row leaves the second agent as it is
	if err := p.SetPoll("second-agent", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.SetPoll("lab-node-pve", &scheduler.Override{Paused: true}); err != nil {
		t.Fatal(err)
	}
	if _, paused := jobs("second-agent"); len(paused) != 0 {
		t.Errorf("Pause on the node's row paused the second agent's job: %v", paused)
	}
	if f, s := round(); f != 0 || s != 1 {
		t.Errorf("with the node's row paused, one round polled first-agent %d time(s) and second-agent %d, want 0 and 1", f, s)
	}
	// the interval of the second agent's row can be set
	if err := p.SetPoll("second-agent", &scheduler.Override{Seconds: 30}); err != nil {
		t.Fatal(err)
	}
	if st := p.PollStatus("second-agent"); len(st.Jobs) != 1 || st.Jobs[0].EffectiveS == nil || *st.Jobs[0].EffectiveS != 30 {
		t.Errorf("the interval of the row second-agent after it was set to 30 s: %+v", st.Jobs)
	}
}
