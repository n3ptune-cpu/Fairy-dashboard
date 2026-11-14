package poller

import (
	"context"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Third review of build 14, S3. A poll of a host is on its way when the host is removed or paired again
// (Re-pair is Remove, then Add: the id can be another machine afterwards). The poll finishes, and what it
// brought in was stored under the id: the old machine's row, "online, just seen" and its pending shutdown
// stood under the new entry, and a wake / update run that waited for the agent of the new entry to show the
// run's countdown saw it there and sent its cancel to the new machine. The result of a poll is kept only
// while the entry it was made for is still the one under the id.

// heldPoll adds the host "pc" and starts a poll of it that waits inside the request. land lets it finish,
// with the answer of a machine that counts down to a shutdown, or without one (fail), and waits for it.
func heldPoll(t *testing.T, fail bool) (p *Poller, old config.Agent, land func()) {
	t.Helper()
	p, b := linkPanel(t, [][2]string{{"lab", "lab-"}}, "", "", map[string]string{"lab": "pve"})
	old = config.Agent{ID: "pc", Name: "PC", URL: "http://old-machine:9101", Managed: true, Token: testutil.ReadToken}
	if err := p.AddAgent(old, nil); err != nil {
		t.Fatal(err)
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	first := true
	b.Mu.Lock()
	b.Metrics["pc"] = func() (jsonx.M, error) { // called with b.Mu held
		if !first { // every later poll: the machine that has the id then does not answer
			return nil, devices.Errf("connection failed")
		}
		first = false
		b.Mu.Unlock()
		close(started)
		<-release
		b.Mu.Lock()
		if fail {
			return nil, devices.Errf("connection failed")
		}
		m := testutil.AgentMetrics()
		m["power_control"] = jsonx.M{"enabled": true, "min_delay_s": 10.0, "sleep": true,
			"pending": jsonx.M{"action": "shutdown", "at": float64(time.Now().Add(30 * time.Second).Unix()), "delay_s": 30.0, "requested_by": "run"}}
		return m, nil
	}
	b.Mu.Unlock()
	entry := p.AgentEntry("pc")
	go func() { p.pollEntry(context.Background(), old, entry, time.Second); close(done) }() // what the poll job of the old entry does
	<-started
	return p, old, func() { close(release); <-done }
}

// nothingOf fails the test when anything of a poll is on record for the id.
func nothingOf(t *testing.T, p *Poller, id, when string) {
	t.Helper()
	info, sys, known := p.AgentSeen(id)
	if known || info.Online || !info.SeenAt.IsZero() || sys.PowerPending != nil || info.Silent.N != 0 || !info.QuietSince.IsZero() {
		t.Errorf("%s: a row %v (status %q), online %v, seen at %v, pending %+v, silent polls %d", when, known, sys.Status, info.Online, info.SeenAt, sys.PowerPending, info.Silent.N)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sources["agent:"+id]; ok || p.agentFailures[id] != 0 || p.agentPower[id] != nil {
		t.Errorf("%s: the health list has %+v (%v), %d failed poll(s) counted", when, s, ok, p.agentFailures[id])
	}
}

func TestAPollOfAnEntryThatWasReplacedIsDropped(t *testing.T) {
	for name, fail := range map[string]bool{"the old machine's answer": false, "the old machine's poll fails": true} {
		p, old, land := heldPoll(t, fail)
		e0 := p.AgentEntry("pc")
		p.RemoveAgent("pc") // Re-pair, to another machine
		if err := p.AddAgent(config.Agent{ID: "pc", Name: "PC", URL: "http://new-machine:9101", Managed: true, Token: testutil.ReadToken}, nil); err != nil {
			t.Fatal(err)
		}
		land()
		if a, _ := p.AgentFor("pc"); p.AgentEntry("pc") == e0 || a.URL != "http://new-machine:9101" {
			t.Fatalf("%s: the fixture: entry %d -> %d, agent %s", name, e0, p.AgentEntry("pc"), a.URL)
		}
		nothingOf(t, p, "pc", name+", landed under the new entry")
		// the new entry's own poll is its first: a failure counts once, an answer is its own
		p.pollAgent(context.Background(), old, time.Second)
		if info, sys, known := p.AgentSeen("pc"); !known || info.Online || sys.Status != "offline" || info.Silent.N != 1 || sys.PowerPending != nil {
			t.Errorf("%s: the new entry after its own first poll, which failed: row %v, status %q, online %v, silent polls %d", name, known, sys.Status, info.Online, info.Silent.N)
		}
	}
}

func TestAPollOfAHostThatWasRemovedIsDropped(t *testing.T) {
	for name, fail := range map[string]bool{"its answer": false, "its poll fails": true} {
		p, _, land := heldPoll(t, fail)
		p.RemoveAgent("pc") // Remove, or the moment between the two halves of a Re-pair
		land()
		nothingOf(t, p, "pc", name+", landed after the Remove")
		if err := p.AddAgent(config.Agent{ID: "pc", Name: "PC", URL: "http://new-machine:9101", Managed: true, Token: testutil.ReadToken}, nil); err != nil {
			t.Fatal(err)
		}
		nothingOf(t, p, "pc", name+", and the host added again")
	}
}

// What stays: the answer of a poll whose entry is still there is kept, with the entry it came under.
func TestAPollOfTheEntryThatIsThereIsKept(t *testing.T) {
	p, _, land := heldPoll(t, false)
	land()
	info, sys, known := p.AgentSeen("pc")
	if !known || !info.Online || sys.PowerPending == nil || sys.PowerPending.DelayS != 30 || info.Entry != p.AgentEntry("pc") || info.Entry == 0 {
		t.Fatalf("row %v, online %v, pending %+v, entry %d (the panel's: %d)", known, info.Online, sys.PowerPending, info.Entry, p.AgentEntry("pc"))
	}
}
