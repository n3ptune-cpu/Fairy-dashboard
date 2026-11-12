package poller

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// goneEnv is a panel whose machines.json holds a record for "old-box", a host that is not in the configuration
// any more (taken out of config.yaml or renamed there), next to one for "tower", which is.
type goneEnv struct {
	cfg  *config.Panel
	b    *testutil.Backend
	evs  []MachineEvent
	file func() map[string]approval.Record
}

const goneID = "Z2hvc3QtbWFjaGluZS1pZC0wMQ"

func newGoneEnv(t *testing.T) *goneEnv {
	t.Helper()
	g := &goneEnv{cfg: testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower"), b: testutil.NewBackend()}
	at := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	st := adminstate.New(g.cfg.DataDir)
	if err := st.SetMachines(map[string]approval.Record{
		"old-box": {MachineID: goneID, How: approval.PinTurnOn, At: at, By: "nora", State: api.ApprovalRequired, StateAt: at},
		"tower":   {MachineID: "dG93ZXItbWFjaGluZS1pZC0wMQ", How: approval.PinAccepted, At: at, By: "nora", State: api.ApprovalRequired, StateAt: at},
	}, nil); err != nil {
		t.Fatal(err)
	}
	g.file = func() map[string]approval.Record {
		recs, err := st.Machines()
		if err != nil {
			t.Fatal(err)
		}
		return recs
	}
	return g
}

func (g *goneEnv) start(t *testing.T) *Poller {
	t.Helper()
	p, err := New(g.cfg, config.Secrets{}, g.b)
	if err != nil {
		t.Fatal(err)
	}
	p.OnMachine = func(e MachineEvent) { g.evs = append(g.evs, e) }
	loaded(p) // what the server does once it listens to the record
	return p
}

// loaded calls p.MachinesLoaded. (Through an interface, so that this file also builds against a poller from
// before the fix, where these tests fail for what they test and not for a missing name.)
func loaded(p any) {
	if l, ok := p.(interface{ MachinesLoaded() }); ok {
		l.MachinesLoaded()
	}
}

// shows makes the agent id answer with the check on and the machine id goneID.
func (g *goneEnv) shows(id string) {
	g.b.Mu.Lock()
	defer g.b.Mu.Unlock()
	g.b.Metrics[id] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["approval"] = jsonx.M{"state": api.ApprovalRequired, "proto": float64(api.ApprovalProto), "machine_id": goneID}
		return m, nil
	}
}

// A record does not outlive its host (audit 6, r4-x1-03). The record of a host that is not in the configuration
// any more is dropped when the panel starts, said once, and taken out of the file; the record of a host that
// is there stays as it is.
func TestTheRecordOfAHostThatIsGoneIsDroppedAtTheStart(t *testing.T) {
	g := newGoneEnv(t)
	p := g.start(t)
	if len(g.evs) != 1 || g.evs[0].Kind != "gone" || g.evs[0].AgentID != "old-box" || g.evs[0].Binding.Record.MachineID != goneID {
		t.Fatalf("what was announced: %+v", g.evs)
	}
	if recs := g.file(); len(recs) != 1 || recs["tower"].MachineID == "" || recs["tower"].How != approval.PinAccepted {
		t.Fatalf("machines.json after the start: %+v", recs)
	}
	if b := p.BindingOf("old-box"); b.Known {
		t.Fatalf("the record is still in memory: %+v", b)
	}
	if b := p.BindingOf("tower"); !b.Known || b.Record.MachineID == "" {
		t.Fatalf("the record of a host that is there: %+v", b)
	}
	// said once: not again by the same poller, and not at the next start
	loaded(p)
	g.start(t)
	if len(g.evs) != 1 {
		t.Fatalf("announced again: %+v", g.evs)
	}
}

// (a) of the finding: the same machine under a new agent id showed "the same machine id as old-box, which keeps
// it", had no button, and its accept was refused: kept by a host that does not exist. It is "not on record"
// now, held by nobody, and one accept puts its id on record.
func TestAMachineUnderANewNameCanBeAccepted(t *testing.T) {
	g := newGoneEnv(t)
	p := g.start(t)
	g.shows("laptop") // the machine that was "old-box" is called "laptop" in config.yaml now
	p.PollOnce(context.Background())
	b := p.BindingOf("laptop")
	if b.Status != approval.BindNoRecord || len(b.With) != 0 || len(b.Held) != 0 || b.Record.MachineID != "" {
		t.Fatalf("the machine under its new name: %+v", b)
	}
	what, _, err := p.AcceptMachine("laptop", "nora", AcceptWant{MachineID: goneID})
	if what != "id" || err != nil {
		t.Fatalf("the accept: %q %v", what, err)
	}
	if b := p.BindingOf("laptop"); !b.OK() || b.Record.MachineID != goneID || b.Record.How != approval.PinAccepted || b.Record.Holder {
		t.Fatalf("after the accept: %+v", b)
	}
}

// (b) of the finding: a host that is added later under the old id had the old machine's id on record from its
// first answer ("turn-on", by the admin of then), without a Turn on and without an accept. It starts with
// nothing on record: showing that id it is "not on record" like any host the panel did not turn on.
func TestAHostAddedUnderAnOldIDInheritsNothing(t *testing.T) {
	g := newGoneEnv(t)
	p := g.start(t)
	ghost := g.cfg.Agents[1]
	ghost.ID, ghost.URL = "old-box", "http://old-box"
	g.shows("old-box")
	if err := p.AddAgent(ghost, nil); err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	b := p.BindingOf("old-box")
	if b.Status != approval.BindNoRecord || b.Record.MachineID != "" || b.Record.How != "" || b.Record.By != "" {
		t.Fatalf("the host added under the old id: %+v", b)
	}
	// and across a restart with that host in the configuration: still nothing but "it showed the check"
	g.cfg.Agents = append(g.cfg.Agents, ghost)
	p = g.start(t)
	if b := p.BindingOf("old-box"); b.Record.MachineID != "" || !b.Record.Unaccepted {
		t.Fatalf("after a restart: %+v", b)
	}
}

// The same when machines.json could not be read at the start and can be read later: the record of a host that
// is gone is dropped then, and said with the next thing the poller announces.
func TestTheRecordOfAHostThatIsGoneIsDroppedWhenTheFileCanBeReadAgain(t *testing.T) {
	g := newGoneEnv(t)
	path := adminstate.MachinesPath(g.cfg.DataDir)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := g.start(t)
	if b := p.BindingOf("laptop"); b.Status != approval.BindNoStore || len(g.evs) != 0 {
		t.Fatalf("with an unreadable file: %+v %+v", b, g.evs)
	}
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	g.shows("laptop")
	p.PollOnce(context.Background())
	var gone []string
	for _, e := range g.evs {
		if e.Kind == "gone" {
			gone = append(gone, e.AgentID)
		}
	}
	if len(gone) != 1 || gone[0] != "old-box" {
		t.Fatalf("announced: %+v", g.evs)
	}
	if b := p.BindingOf("laptop"); b.Status != approval.BindNoRecord || len(b.Held) != 0 {
		t.Fatalf("laptop: %+v", b)
	}
	if recs := g.file(); recs["old-box"].MachineID != "" {
		t.Fatalf("machines.json: %+v", recs)
	}
}
