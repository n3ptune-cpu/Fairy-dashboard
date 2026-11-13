package poller

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// Second review of build 14, R1 (build 13 did the same). Two sites, each with a node the installer called
// "pve", each with an agent `node: pve` whose id begins with its own site's id_prefix. The configuration
// decides neither (each entry can mean the other's node too: guard d), and the rows then put BOTH agents into
// the first node of that name that was there: the row of site alpha's node belonged to whichever agent stood
// first in the configuration. With beta's agent first, alpha's row showed beta's sensors, and what an admin
// pressed on it that goes to an agent (shut down, restart, sleep, the fans, the update, a run) went to beta's
// machine; beta's `system_hint: self` plug landed on alpha's row.
//
// An agent is a part of the node its own id names or of none: each row is its own agent's, in either order
// of the agents, with both nodes there, with only the other site's node there, and with none.
func TestTwoSitesWithANodeOfOneName(t *testing.T) {
	own := map[string]string{"alpha-x": "alpha-node-pve", "beta-y": "beta-node-pve"}
	for _, order := range [][]string{{"alpha-x", "beta-y"}, {"beta-y", "alpha-x"}} {
		for _, there := range []map[string]string{
			{"alpha": "pve", "beta": "pve"}, // both nodes answer
			{"alpha": "pve"},                // site beta's host is off: its Proxmox API does not answer
			{"beta": "pve"},
			{},
		} {
			when := fmt.Sprintf("agents in the order %v, the nodes that are there: %v", order, there)
			agents := ""
			for _, id := range order {
				agents += fmt.Sprintf(linkAgent, id, id, id, "node: pve")
			}
			p, _ := linkPanel(t, [][2]string{{"alpha", "alpha-"}, {"beta", "beta-"}}, agents, "", there)
			p.PollOnce(context.Background())
			rows := snap(p)
			for agent, node := range own {
				site := strings.TrimSuffix(node, "-node-pve")
				if _, _, ok := p.NodeOf(agent); ok {
					t.Errorf("%s: the configuration decides the node of %s (guard d is as wide as it was: no wol: is given by a guess)", when, agent)
				}
				if got := p.LinkOf(p.agents[agent]); got != node {
					t.Errorf("%s: the rows link %s to %s, want %s", when, agent, got, node)
				}
				shown, want := p.ShownSystem(agent), agent
				if there[site] != "" {
					want = node
				}
				if shown == nil || shown.ID != want {
					t.Errorf("%s: %s is shown as %+v, want the row %s", when, agent, shown, want)
				}
				if got := p.ShownID(agent); got != want { // where its `system_hint: self` plug lands
					t.Errorf("%s: what is linked to %s lands on the row %s, want %s", when, agent, got, want)
				}
				// what is pressed on the node's row and goes to an agent goes to this agent, never to the other
				if a, ok := p.AgentFor(node); !ok || a.ID != agent {
					t.Errorf("%s: WRONG MACHINE: what is pressed on the row %s goes to agent %q, want %s", when, node, a.ID, agent)
				}
				if there[site] != "" {
					if _, sep := rows[agent]; sep || len(rows[node].Temps) == 0 {
						t.Errorf("%s: the row %s lacks the sensors of its agent %s (a row of its own: %v)", when, node, agent, sep)
					}
				} else if _, sep := rows[agent]; !sep {
					t.Errorf("%s: %s has no row of its own although its node's row is not there", when, agent)
				}
			}
		}
	}
}

// An agent whose id begins with no id_prefix is still placed by the node's name alone, as in build 13: the
// first node of that name that is there. That can be another machine's node, so it is said: one line in the
// log at the start for every agent whose node is not decided, also when no systems: entry is written for the
// node (until now such an agent was placed without a word), with where the rows put it.
func TestAnAgentThatIsPlacedByTheNodesNameIsNamedAtTheStart(t *testing.T) {
	var logs bytes.Buffer
	var logMu sync.Mutex
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&logMu, &logs}, nil)))
	defer slog.SetDefault(old)
	agents := fmt.Sprintf(linkAgent, "spare-box", "S", "s", "node: pve") + fmt.Sprintf(linkAgent, "alpha-x", "X", "x", "node: pve") + fmt.Sprintf(linkAgent, "beta-y", "Y", "y", "node: pve")
	p, _ := linkPanel(t, [][2]string{{"alpha", "alpha-"}, {"beta", "beta-"}}, agents, "", map[string]string{"beta": "pve"})
	p.PollOnce(context.Background())
	if got := p.LinkOf(p.agents["spare-box"]); got != "beta-node-pve" {
		t.Errorf("an id that begins with no id_prefix: linked to %s, want the first node of that name that is there (beta-node-pve), as before", got)
	}
	if got := p.LinkOf(p.agents["alpha-x"]); got != "alpha-node-pve" {
		t.Errorf("alpha-x is linked to %s although only the other site's node is there", got)
	}
	logMu.Lock()
	buf := logs.String()
	logMu.Unlock()
	const tail = "A wol:, the switches and the timings that are kept under the node's id are not used for it while it is shown under its own id. To say which node it is, write links_to: "
	for _, want := range []string{
		"level=INFO msg=\"config: agent spare-box (node: pve): its id begins with the id_prefix of none of the proxmox: entries, so the configuration does not say which node is the agent's " +
			"(it can be alpha-node-pve or beta-node-pve). On the pages it is a part of the first of these nodes that is there, which can be another machine's: " +
			"what is pressed on that row and goes to an agent goes to this one. " + tail + "<the id of its node> in the agent's entry\"",
		"level=INFO msg=\"config: agent alpha-x (node: pve): the entry of spare-box and of beta-y can mean the same node, alpha-node-pve, so the configuration does not say which node is the agent's " +
			"(it can be alpha-node-pve or beta-node-pve). On the pages it is a part of the row of alpha-node-pve and of no other node " +
			"(the row of a node is one agent's: the first of the configuration that names it). " + tail + "<the id of its own node> in the entry of each of these agents\"",
		"level=INFO msg=\"config: agent beta-y (node: pve): the entry of spare-box and of alpha-x can mean the same node, beta-node-pve",
	} {
		if strings.Count(buf, want) != 1 {
			t.Errorf("the log lacks (or has twice) %s:\n%s", want, buf)
		}
	}
	if w := p.NodeLinkWarnings(); len(w) != 0 {
		t.Errorf("warnings without a systems: entry: %q", w)
	}
	if n := p.NodeLinksUndecided(); len(n) != 3 {
		t.Errorf("NodeLinksUndecided: %q", n)
	}
}
