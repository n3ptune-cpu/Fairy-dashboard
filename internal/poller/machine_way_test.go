package poller

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The way down alone (MachineUnder), over rows and links made up here: where it ends when it ends at no
// machine, and why; that it goes on at the row an id is shown as; that an id of two things ends it before any
// row is asked; and how it counts the agents on the way.
func TestMachineUnderSaysWhereAndWhyItEnds(t *testing.T) {
	rows := map[string]*model.System{}
	add := func(id, kind, parent string) {
		r := &model.System{ID: id, Name: strings.ToUpper(id), Kind: kind}
		if parent != "" {
			r.ParentID = model.P(parent)
		}
		rows[id] = r
	}
	add("node-1", "node", "")
	add("vm-1", "qemu", "node-1")
	add("vm-lost", "qemu", "node-9")
	for _, id := range []string{"pc", "in-vm", "in-lost", "to-nothing", "a", "b", "loop-a", "loop-b", "into-loop", "to-own", "to-double", "far-double", "to-doubles-agent", "to-shown-double", "double"} {
		add(id, "agent", "")
	}
	// (own-agent is a node's own agent: a part of the row node-1, it has no row of its own. "double" is an id of
	// two things; "runs-on-double" is an agent without a row of its own, shown as node-1, that runs on it.)
	runs := map[string]string{"in-vm": "vm-1", "in-lost": "vm-lost", "to-nothing": "vm-404", "a": "b", "b": "node-1", "loop-a": "loop-b", "loop-b": "loop-a", "into-loop": "loop-a",
		"to-own": "own-agent", "own-agent": "node-1", "to-double": "double", "far-double": "to-double", "to-doubles-agent": "runs-on-double", "runs-on-double": "double",
		"to-shown-double": "shown-as-double"}
	shown := map[string]string{"own-agent": "node-1", "runs-on-double": "node-1", "shown-as-double": "double"}
	for i := 1; i <= 10; i++ {
		add(fmt.Sprintf("c%d", i), "agent", "")
		runs[fmt.Sprintf("c%d", i)] = fmt.Sprintf("c%d", i+1)
	}
	runs["c10"] = "own-agent" // ten agents with a row, and the node's own agent behind them
	follow := func(start string) (string, string, WayEnd) {
		m, over, end := MachineUnder(start, func(id string) *model.System { return rows[id] },
			func(id string) (string, bool) { return runs[id], false },
			func(id string) bool { return id == "double" },
			func(id string) string {
				if s, ok := shown[id]; ok {
					return s
				}
				return id
			})
		ids := []string{}
		for _, r := range over {
			ids = append(ids, r.ID)
		}
		if m == nil {
			return "", strings.Join(ids, " "), end
		}
		return m.ID, strings.Join(ids, " "), end
	}
	for _, c := range []struct {
		start, machine, over string
		end                  WayEnd
		words                string
	}{
		{"pc", "pc", "", WayEnd{}, ""},
		{"in-vm", "node-1", "in-vm vm-1", WayEnd{}, ""},
		{"a", "node-1", "a b", WayEnd{}, ""},
		{"nowhere", "", "", WayEnd{WayNoRow, "nowhere"}, "no row has the id nowhere"},
		{"to-nothing", "", "to-nothing", WayEnd{WayNoRow, "vm-404"}, "to-nothing leads to vm-404, and no row has that id"},
		{"vm-lost", "", "vm-lost", WayEnd{WayNoNode, "vm-lost"}, "vm-lost is a guest, and the node it runs on has no row"},
		{"in-lost", "", "in-lost vm-lost", WayEnd{WayNoNode, "vm-lost"}, "in-lost leads to vm-lost, a guest, and the node it runs on has no row"},
		{"loop-a", "", "loop-a loop-b", WayEnd{WayBack, "loop-a"}, "links_to leads from loop-a back to loop-a"},
		{"into-loop", "", "into-loop loop-a loop-b", WayEnd{WayBack, "loop-a"}, "links_to leads from into-loop back to loop-a"},
		// an agent that is named by its id and shown as its node's row: the way goes on at that row
		{"to-own", "node-1", "to-own", WayEnd{}, ""},
		// an id of two things ends the way where it stands, as it is written
		{"double", "", "", WayEnd{WayTwice, "double"}, "double is the id of an agent and of a node or a guest the Proxmox API lists, and the panel cannot tell which is meant"},
		{"to-double", "", "to-double", WayEnd{WayTwice, "double"}, "to-double leads to double, which is the id of an agent and of a node or a guest the Proxmox API lists, and the panel cannot tell which is meant"},
		{"far-double", "", "far-double", WayEnd{WayTwice, "double"}, "far-double leads to double, which is the id of an agent and of a node or a guest the Proxmox API lists, and the panel cannot tell which is meant"},
		// ... also when the agent that runs on it is shown as another row: that row is not asked
		{"to-doubles-agent", "", "to-doubles-agent", WayEnd{WayTwice, "double"}, ""},
		// ... and when an id is shown as the row of an id of two things
		{"to-shown-double", "", "to-shown-double", WayEnd{WayTwice, "double"}, ""},
		// eight agents that name where they run are followed, nine are not; the node's own agent, which has no
		// row, is one of them
		{"c4", "node-1", "c4 c5 c6 c7 c8 c9 c10", WayEnd{}, ""},                                                                // 7 and the node's own agent
		{"c3", "", "c3 c4 c5 c6 c7 c8 c9 c10", WayEnd{WayLong, "own-agent"}, "links_to leads from c3 over more than 8 agents"}, // 8 and the node's own agent
		{"c2", "", "c2 c3 c4 c5 c6 c7 c8 c9", WayEnd{WayLong, "c10"}, "links_to leads from c2 over more than 8 agents"},
	} {
		m, over, end := follow(c.start)
		if m != c.machine || over != c.over || end != c.end {
			t.Errorf("from %s: the machine is %q over %q, the end %+v; want %q over %q, %+v", c.start, m, over, end, c.machine, c.over, c.end)
		}
		if c.words != "" && end.Text(c.start) != c.words {
			t.Errorf("from %s: in words %q, want %q", c.start, end.Text(c.start), c.words)
		}
		if c.end.Why == "" && end.Text(c.start) != "" {
			t.Errorf("from %s: a way that ends at a machine has words: %q", c.start, end.Text(c.start))
		}
	}
}
