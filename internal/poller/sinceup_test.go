package poller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// AnsweredSinceUp: "has the Proxmox API answered since the node's agent came up" for the wake / update / shut
// down run's question about the guests of a node. It was asked with two times of day (the source's
// LastSuccess after the agent's SeenAt), and a list from before the node was woken counted as current when
// the panel's clock had been set back in between. Here the two answers are polled one by one, with a steady
// clock and a time of day that the test sets apart.
func TestAnsweredSinceUpGoesByTheSteadyClock(t *testing.T) {
	b := testutil.NewBackend()
	link, api := true, true
	b.PVE = func() ([]jsonx.M, error) {
		if !api {
			return nil, context.DeadlineExceeded
		}
		return testutil.PVEResources(), nil
	}
	b.Metrics["pvea"] = func() (jsonx.M, error) {
		if !link {
			return nil, devices.Errf("connection failed")
		}
		return testutil.AgentMetrics(), nil
	}
	set := func(f func()) {
		b.Mu.Lock()
		f()
		b.Mu.Unlock()
	}
	cfg := testutil.Config(t, []string{"http://pve.invalid"}, "http://laptop.invalid", "http://tower.invalid")
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	var ck sync.Mutex
	steady, skew := time.Duration(0), time.Duration(0)
	t0 := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	p.Clock = func() time.Time {
		ck.Lock()
		defer ck.Unlock()
		return t0.Add(steady + skew)
	}
	p.Mono = func() time.Duration {
		ck.Lock()
		defer ck.Unlock()
		return steady
	}
	pass := func(d, set time.Duration) {
		ck.Lock()
		steady, skew = steady+d, skew+set
		ck.Unlock()
	}
	if err := p.AddAgent(config.Agent{ID: "pvea", Name: "pvea", URL: "http://pvea.invalid", Token: "read-word", Managed: true, Node: "SRV-PVEA"}, nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pve := p.Clusters()[0]
	src := "proxmox:" + pve.ID
	agent := func() {
		a, _ := p.AgentByID("pvea")
		p.pollAgent(ctx, a, 0)
	}
	cluster := func() { p.pollProxmox(ctx, pve, 0) }
	// byDay is the old question, with the two times of day
	byDay := func() bool {
		i, _ := p.AgentInfo("pvea")
		for _, st := range p.Summary().Sources {
			if st.Name == src {
				return st.LastSuccess != nil && st.LastSuccess.After(i.SeenAt)
			}
		}
		return false
	}
	want := func(when string, w bool) {
		t.Helper()
		if got := p.AnsweredSinceUp(src, "pvea"); got != w {
			t.Fatalf("%s: AnsweredSinceUp = %v, want %v", when, got, w)
		}
	}
	gap := p.quietGap(p.agentInterval("pvea"))
	if gap < 20*time.Second || gap > time.Minute {
		t.Fatalf("the test is written for a quietGap of about half a minute, and it is %v", gap)
	}

	want("nothing polled yet", false)
	cluster()
	want("the Proxmox API answered, the node's agent never did", false)
	// The time of day is set back an hour, and then the node's agent answers for the first time.
	pass(10*time.Second, -time.Hour)
	agent()
	if !byDay() {
		t.Fatal("the set-up: by the time of day the Proxmox API's answer must look newer than the agent's")
	}
	want("the agent's first answer came 10 s after the Proxmox API's last one, with the time of day an hour back", false)
	pass(2*time.Second, 0)
	agent()
	want("the agent's second answer", false)
	pass(3*time.Second, 0)
	cluster()
	want("the Proxmox API answered after the agent came up", true)
	// A poll of the Proxmox API that fails takes nothing back (that its latest poll must be good is the
	// caller's rule: SourceStatus.OK); the next good one counts again.
	set(func() { api = false })
	pass(5*time.Second, 0)
	cluster()
	want("the Proxmox API's last poll failed", true)
	set(func() { api = true })
	// The agent gives nothing for a few polls, no longer than quietGap from answer to answer (it restarts
	// after its update): the row of its answers goes on, and the list from before stays good.
	agent()
	set(func() { link = false })
	pass(time.Second, 0)
	agent()
	pass(gap-2*time.Second, 0)
	agent()
	set(func() { link = true })
	pass(time.Second, 0)
	agent()
	want("the agent answers again, quietGap after its last answer", true)
	// It gives nothing for longer than that (the node was off): its next answer is a new coming up, and the
	// list from before it does not count, whatever the time of day says (it is set forward two hours here).
	set(func() { link = false })
	pass(time.Second, 0)
	agent()
	pass(gap, 2*time.Hour)
	agent()
	set(func() { link = true })
	pass(time.Second, 0)
	agent()
	want("the agent answers again after more than quietGap", false)
	pass(time.Second, -2*time.Hour)
	cluster()
	want("and the Proxmox API after it, with the time of day two hours back", true)
	if byDay() {
		t.Fatal("the set-up: by the time of day this answer of the Proxmox API must look older than the agent's")
	}
	// Nobody polled the agent for longer than quietGap (the panel was not watching): a new coming up as well.
	pass(gap+time.Second, 0)
	agent()
	want("the agent's next answer comes after more than quietGap without a poll", false)
	// The same moment on the steady clock is not "after".
	cluster()
	want("the Proxmox API answers at the agent's own moment", false)
	pass(time.Millisecond, 0)
	cluster()
	want("the Proxmox API answers a moment later", true)

	if p.AnsweredSinceUp("proxmox:nothing", "pvea") || p.AnsweredSinceUp(src, "nobody") || p.AnsweredSinceUp("", "") {
		t.Fatal("a source or an agent that the panel does not have")
	}
	p.RemoveAgent("pvea")
	want("the agent was removed", false)
}
