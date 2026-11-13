package poller

import (
	"context"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// answersWithoutACheck makes the agent id answer its polls, with no approval block.
func (g *goneEnv) answersWithoutACheck(id string) {
	g.b.Mu.Lock()
	defer g.b.Mu.Unlock()
	g.b.Metrics[id] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
}

// A record is never counted as newer than the panel's clock (review of passkey-k, RK-03): with the clock behind
// the time an id came on record, no answer was "asked at or after the record's time", and a check that was
// gone could not be seen or accepted. The record's time is taken down to the moment the agent is asked. A
// record that is not newer than the clock is left as it is.
func TestARecordIsNeverNewerThanThePanelsClock(t *testing.T) {
	g := newGoneEnv(t) // tower: an id on record since a day ago; its agent answers without a check
	g.answersWithoutACheck("tower")
	p := g.start(t)
	at := p.BindingOf("tower").Record.At
	p.PollOnce(context.Background())
	if b := p.BindingOf("tower"); !b.Record.At.Equal(at) || b.Status != approval.BindVanished {
		t.Fatalf("with a right clock: %+v (the record was of %s)", b, at)
	}

	// the same panel folder, and a clock that is two days behind: the record is "from the future"
	g2 := newGoneEnv(t)
	g2.answersWithoutACheck("tower")
	p = g2.start(t)
	behind := func() time.Time { return time.Now().Add(-48 * time.Hour) }
	p.Clock = behind
	if b := p.BindingOf("tower"); !b.Record.At.After(behind()) {
		t.Fatalf("the record is not newer than the clock in this test: %+v", b)
	}
	p.PollOnce(context.Background())
	b := p.BindingOf("tower")
	if b.Record.At.After(behind()) {
		t.Fatalf("the record is still newer than the panel's clock: %s, clock %s", b.Record.At, behind())
	}
	if b.Status != approval.BindVanished {
		t.Fatalf("the answer of the first poll does not count: %+v", b)
	}
	if what, _, err := p.AcceptMachine("tower", "nora", AcceptWant{Off: true}); what != "off" || err != nil {
		t.Fatalf("the accept with the clock behind: %q %v", what, err)
	}
}
