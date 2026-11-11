package web

import (
	"sync"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
)

// Two read requests are passed through to an agent for anyone who may see the host, a viewer and a read token
// included: the process list (GET /api/systems/{id}/processes) and the fan status (GET .../fan). Each is one
// request from the panel to the agent, on a connection of the panel's own.
//
// The agent admits a limited number of the panel's connections at a time (Fairy-agent pkg/agent: 32, and
// 128 more wait up to 3 s) and resets what is over that. Without a limit on this side, a few hundred such
// requests in flight used all of it, and what was reset then included the panel's own polls and an admin's
// control requests, "cancel shutdown" among them (T-20).
//
// So the panel passes at most maxAgentReadsPerUser of them through for one user (or API token) and
// maxAgentReadsPerAgent to one agent at the same time, and answers one more 429 at once, without asking the
// agent. The page asks for each of the two once per refresh, so several open tabs fit; and 8 of the agent's
// 32 leave room for the polls and for every control request, which are not counted here.
//
// The limit per agent is for those who may only look. Two of them at their own limit fill it, and it must
// not be their choice whether an admin sees a host's processes and fan controls: an admin (a session of an
// admin, a control token) is held to the limit per user only and takes no place in the agent's 8. The agent
// then has at most 8 plus 4 for each admin at work, still far from its 32.
const (
	maxAgentReadsPerUser  = 4
	maxAgentReadsPerAgent = 8
)

// agentReads counts the read requests that are at an agent right now.
type agentReads struct {
	mu      sync.Mutex
	byUser  map[string]int
	byAgent map[string]int
}

func newAgentReads() *agentReads {
	return &agentReads{byUser: map[string]int{}, byAgent: map[string]int{}}
}

// acquire takes a slot for this user and, unless the user is an admin, for this agent; ok is false when one
// of them is at its limit. release gives it back (once).
func (a *agentReads) acquire(user, agent string, admin bool) (release func(), ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.byUser[user] >= maxAgentReadsPerUser || (!admin && a.byAgent[agent] >= maxAgentReadsPerAgent) {
		return nil, false
	}
	a.byUser[user]++
	if !admin {
		a.byAgent[agent]++
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.byUser[user]--; a.byUser[user] <= 0 {
				delete(a.byUser, user)
			}
			if admin {
				return
			}
			if a.byAgent[agent]--; a.byAgent[agent] <= 0 {
				delete(a.byAgent, agent)
			}
		})
	}, true
}

// agentRead is called before a read request is passed through to the agent with this id: it returns the
// function that gives the slot back, or the 429 for a user or an agent that is at its limit.
func (c *reqCtx) agentRead(agentID string) (release func(), err error) {
	who := "ip:" + c.ip()
	if c.user != nil {
		who = c.user.Sub // the user, or "token:<id>"
	}
	_, g, gerr := c.grant()
	release, ok := c.s.agentReads.acquire(who, agentID, gerr == nil && g.Admin())
	if !ok {
		c.w.Header().Set("Retry-After", "1")
		return nil, apierr.New(429, "too many requests for this host's processes or fan status at the same time; try again in a moment")
	}
	return release, nil
}
