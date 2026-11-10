package agentcontract_test

// The panel's pairing pace against the real agent's budget. The agent looks at enroll.RatePerMin pairing
// requests a minute from one address (pkg/agent/pairing.go, a sliding minute on the wall clock) and answers
// 429 to the rest. The panel used to ask every 3 s: five answers in the first twelve seconds, then 429 for
// the rest of every minute, so a machine whose command had just finished waited up to a minute for the
// panel. Now the panel asks four times a minute (internal/enrollment/pace.go) and must never see a 429 from
// an agent it alone is asking.
//
// The test runs the panel's real enrollment manager at its default pace against the real fairy-agent handler
// for whole minutes. So that this takes no time, both run on a synctest clock: the pairing requests are the
// panel's real HTTP code (enrollment.HTTPPairVia), handed to the agent's handler in the test process instead
// of through a socket.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/enrollment"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// budgetRig is the real enrollment manager asking the real agent handler, on the test's clock.
type budgetRig struct {
	t     *testing.T
	m     *enrollment.Manager
	store *managed.Store
	agent *machine

	mu   sync.Mutex
	hits []int // the status of every answer the agent gave to a pairing request, in order
	at   []time.Time
}

func newBudgetRig(t *testing.T, agent *machine) *budgetRig {
	t.Helper()
	cfg, err := config.Parse([]byte("public_url: https://panel.example.com\noidc: {issuer: \"https://id.example.com\", client_id: cid, allowed_groups: [homelab]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := &budgetRig{t: t, agent: agent, store: managed.NewStore(t.TempDir())}
	r.m = enrollment.New(r.store, enrollment.Hooks{
		Config: func() *config.Panel { return cfg },
		Add:    func(managed.Entry) error { return nil },
		Remove: func(string) {},
		Online: func(string) (bool, string) { return true, "" },
		Event:  func(enrollment.Enrollment, string, string) {},
	}, "127.0.0.1", enrollment.Fixed{Repo: "n3ptune-cpu/Fairy-agent", Tag: "build-20", Version: "20-abcdef0",
		Base: "https://github.com/n3ptune-cpu/Fairy-agent/releases/download/build-20", SH: repeat("a1"), PS1: repeat("b2"), Sums: repeat("c3"),
		PinSH: true, PinPS1: true, PairV2SH: true, PairV2PS1: true, StdinSH: true, EnvPS1: true, MemoryPS1: true})
	r.m.AllowNonTailscale = true
	toAgent := roundTrip(func(req *http.Request) (*http.Response, error) {
		req.RemoteAddr = "127.0.0.1:40123" // the panel's address, as the agent's listener would report it
		rec := httptest.NewRecorder()
		agent.real.ServeHTTP(rec, req)
		r.mu.Lock()
		r.hits, r.at = append(r.hits, rec.Code), append(r.at, time.Now())
		r.mu.Unlock()
		return rec.Result(), nil
	})
	r.m.Pair = func(ctx context.Context, url string, key []byte) (int, enroll.Response, string, error) {
		return enrollment.HTTPPairVia(ctx, toAgent, url, key)
	}
	return r
}

func repeat(s string) string {
	out := ""
	for i := 0; i < 32; i++ {
		out += s
	}
	return out
}

func (r *budgetRig) answers() ([]int, []time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.hits...), append([]time.Time(nil), r.at...)
}

// online waits (on the test's clock) until the enrollment is online and returns how long that took.
func (r *budgetRig) online(id string) time.Duration {
	r.t.Helper()
	start := time.Now()
	for time.Since(start) < 2*time.Minute {
		if e, _ := r.m.Get(id); e.State == enrollment.Online {
			return time.Since(start)
		}
		time.Sleep(100 * time.Millisecond)
	}
	e, _ := r.m.Get(id)
	r.t.Fatalf("not online after 2 minutes: %+v", e)
	return 0
}

func TestPairingNeverReachesTheAgentsBudget(t *testing.T) {
	agent := newMachine(t) // built outside the bubble; its handler then runs on the bubble's clock
	synctest.Test(t, func(t *testing.T) {
		// every enrollment ends before the bubble does, also when the test fails early
		defer time.Sleep(enroll.TTL + 5*time.Minute)
		r := newBudgetRig(t, agent)
		if r.m.Interval != enrollment.PairInterval {
			t.Fatal("this test is about the default pace:", r.m.Interval)
		}
		req := enrollment.Request{Name: "pairbox", Address: "127.0.0.1", OS: "linux", Options: managed.Options{PowerControl: true}}
		cr, err := r.m.Create(context.Background(), req, nil, "admin", func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}

		// 1. Add host. The admin needs four minutes to get to the machine: the agent has no code yet and is
		// asked the whole time.
		time.Sleep(4 * time.Minute)
		if e, _ := r.m.Get(cr.ID); e.State != enrollment.Waiting || e.Hint != "" {
			t.Fatalf("%+v", e)
		}
		// 2. The command runs on the machine: the agent stores the key of the code.
		agent.join(cr.Code, true)
		if took := r.online(cr.ID); took > enrollment.PairInterval+enrollment.PairInterval/4 {
			t.Fatal("pairing came", took, "after the command, with an agent that refused nothing")
		}
		if st := agent.pairState(); st.Closed != "paired" || st.Failures != 0 {
			t.Fatalf("%+v", st)
		}
		if sec, err := r.store.Secret("pairbox"); err != nil || sec.Token != readTok || sec.ControlToken != ctlTok {
			t.Fatal("the agent's tokens did not arrive:", err)
		}

		// 3. Re-pair at once (the agent's count of the minute before is still running), and again four
		// minutes until the command runs.
		hosts, err := r.store.Load()
		if err != nil || len(hosts) != 1 {
			t.Fatal(hosts, err)
		}
		cr2, err := r.m.Create(context.Background(), req, &hosts[0], "admin", func(string) string { return "" })
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(4 * time.Minute)
		agent.join(cr2.Code, true)
		if took := r.online(cr2.ID); took > enrollment.PairInterval+enrollment.PairInterval/4 {
			t.Fatal("the Re-pair came", took, "after the command")
		}

		hits, at := r.answers()
		n200 := 0
		for i, st := range hits {
			switch st {
			case 200:
				n200++
			case 404: // "pairing_inactive": no code on the machine yet
			default:
				t.Fatalf("answer %d of %d was %d (429 = the agent's budget was reached): %v", i+1, len(hits), st, hits)
			}
		}
		if n200 != 2 || len(hits) < 30 { // 8 minutes at 4 a minute
			t.Fatal(n200, len(hits), hits)
		}
		for i := enroll.RatePerMin; i < len(at); i++ {
			if at[i].Sub(at[i-enroll.RatePerMin]) <= time.Minute {
				t.Fatalf("requests %d to %d went out inside one minute", i-enroll.RatePerMin+1, i+1)
			}
		}

		// The check on the test itself: the agent in this test does count. Asked again and again within one
		// minute, with a code on the machine or without, it refuses from the sixth request on.
		for i := 0; i < 2*enroll.RatePerMin; i++ {
			_, _, _, _ = r.m.Pair(context.Background(), "http://127.0.0.1:9101", make([]byte, enroll.KeySize))
		}
		hits, _ = r.answers()
		if last := hits[len(hits)-1]; last != 429 {
			t.Fatal("the agent of this test has no budget: the test proves nothing. Last answer:", last)
		}
	})
}
