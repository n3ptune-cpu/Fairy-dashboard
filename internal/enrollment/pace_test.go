package enrollment

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
)

// The numbers of pace.go against the agent's own budget (enroll.RatePerMin requests a minute per address,
// counted over the last 60 seconds). If the agent module changes its number, PairInterval follows it and
// this test says whether the margin still holds.
func TestPairPaceStaysInsideTheAgentsBudget(t *testing.T) {
	if enroll.RatePerMin != 5 || PairInterval != 15*time.Second {
		t.Fatalf("the agent answers %d pairing requests a minute and the panel asks every %s: look at pace.go again", enroll.RatePerMin, PairInterval)
	}
	shortest := PairInterval - PairInterval/pairJitterDiv
	longest := PairInterval + PairInterval/pairJitterDiv
	// RatePerMin gaps, each the shortest one, must be longer than the agent's minute: then the request after
	// five counted ones always finds the first of them a minute old. The panel gives up on a request after
	// 5 s (run), so two arrivals can move towards each other by at most that much: the margin covers it.
	if margin := shortest*enroll.RatePerMin - time.Minute; margin < 5*time.Second {
		t.Fatalf("the shortest gap %s leaves a margin of %s on the agent's minute", shortest, margin)
	}
	for i := 0; i < 20000; i++ {
		if d := jittered(PairInterval); d < shortest || d > longest {
			t.Fatalf("a gap of %s (want %s to %s)", d, shortest, longest)
		}
	}
	if jittered(0) != 0 || jittered(-time.Second) != 0 {
		t.Fatal("no gap, no jitter")
	}
	// a manager made without an interval asks at the default pace, not in a loop
	if (&Manager{}).interval() != PairInterval || (&Manager{Interval: time.Second}).interval() != time.Second {
		t.Fatal("interval")
	}
}

// paceRig is a manager at the default pace whose "machine" is a function; it records when each request went
// out. It runs inside a synctest bubble, so the 15 minutes of a join code pass in no time.
type paceRig struct {
	m      *Manager
	mu     sync.Mutex
	at     map[string][]time.Time // per host URL
	keys   map[string]bool        // the pairing keys the requests carried (one per code)
	events []string
	answer func(url string, n int) (int, enroll.Response, string, error) // n: the request's number for that URL, from 1
}

func newPaceRig(t *testing.T) *paceRig {
	t.Helper()
	r := &paceRig{at: map[string][]time.Time{}, keys: map[string]bool{}}
	r.m = New(managed.NewStore(t.TempDir()), Hooks{
		Config: func() *config.Panel { return testConfig(t) },
		Remove: func(string) {},
		Add:    func(managed.Entry) error { return nil },
		Online: func(string) (bool, string) { return true, "" },
		Event: func(_ Enrollment, kind, _ string) {
			r.mu.Lock()
			r.events = append(r.events, kind)
			r.mu.Unlock()
		},
	}, "100.64.0.10", Fixed(testInstallers))
	r.m.Pair = func(_ context.Context, url string, key []byte) (int, enroll.Response, string, error) {
		r.mu.Lock()
		r.at[url] = append(r.at[url], time.Now())
		r.keys[string(key)] = true
		n, answer := len(r.at[url]), r.answer
		r.mu.Unlock()
		return answer(url, n)
	}
	return r
}

func (r *paceRig) times(url string) []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.at[url]...)
}

func (r *paceRig) add(t *testing.T, name string) Created {
	t.Helper()
	cr, err := r.m.Create(context.Background(), Request{Name: name, OS: "linux"}, nil, "admin", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return cr
}

// neverRefused says whether an agent that counts like the real one (pkg/agent limiter: n requests in the last
// window, a refused request is not counted) would have answered every one of these requests.
func neverRefused(at []time.Time) bool {
	for i := enroll.RatePerMin; i < len(at); i++ {
		if at[i].Sub(at[i-enroll.RatePerMin]) <= time.Minute {
			return false
		}
	}
	return true
}

// letEnrollmentsEnd lets every enrollment of a synctest bubble run out, so that a test that fails early
// leaves no goroutine behind (synctest reports that instead of the failure).
func letEnrollmentsEnd() { time.Sleep(enroll.TTL + 5*time.Minute) }

var notYet = func(string, int) (int, enroll.Response, string, error) {
	return 404, enroll.Response{}, "pairing_inactive", nil
}

// A waiting Add host asks at once, then about four times a minute for as long as the code lives, and an
// agent's budget of five a minute is never reached.
func TestPairingRequestsArePaced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		defer letEnrollmentsEnd()
		r := newPaceRig(t)
		r.answer = notYet
		start := time.Now()
		cr := r.add(t, "box")
		time.Sleep(enroll.TTL + time.Minute) // the whole life of the code
		e, _ := r.m.Get(cr.ID)
		if e.State != Expired || !e.Done {
			t.Fatalf("%+v", e)
		}
		at := r.times("http://box:9101")
		if len(at) < 50 || len(at) > 70 { // 15 minutes at 4 a minute: about 60
			t.Fatal("requests in 15 minutes:", len(at))
		}
		if at[0].Sub(start) > time.Second {
			t.Fatal("the first request waited", at[0].Sub(start))
		}
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < PairInterval-PairInterval/pairJitterDiv || gap > PairInterval+PairInterval/pairJitterDiv {
				t.Fatalf("request %d came %s after the one before", i+1, gap)
			}
		}
		if !neverRefused(at) {
			t.Fatal("the agent's budget was reached:", at)
		}
		if e.Attempts != len(at) {
			t.Fatal(e.Attempts, len(at))
		}
	})
}

// An agent that answers 429 is asked less often (twice the interval, then four times: its whole minute). The
// enrollment keeps waiting with the same code, says why, and pairs when the agent answers again.
func TestPairingBacksOffWhenTheAgentSaysItsBudgetIsUsedUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		defer letEnrollmentsEnd()
		r := newPaceRig(t)
		good := enroll.Response{AgentVersion: "20-abcdef0", Hostname: "box", Platform: "linux", Token: "read-token-of-the-agent-0123456789abcdef"}
		r.answer = func(_ string, n int) (int, enroll.Response, string, error) {
			switch {
			case n <= 4: // something else used the agent's budget
				return 429, enroll.Response{}, "rate_limited", nil
			case n <= 6:
				return 404, enroll.Response{}, "pairing_inactive", nil
			}
			return 200, good, "", nil
		}
		cr := r.add(t, "box")
		time.Sleep(2 * time.Minute) // requests 1 to 3 (at 0 s, about 30 s, about 90 s) were refused
		e, _ := r.m.Get(cr.ID)
		if e.State != Waiting || e.Done || e.Hint != HintRateLimited || e.Attempts != 3 {
			t.Fatalf("%+v", e)
		}
		if want := "the panel asks again in about"; !contains(e.Message, want) || !contains(e.Message, "the code stays valid") {
			t.Fatal(e.Message)
		}
		time.Sleep(10 * time.Minute)
		if e, _ = r.m.Get(cr.ID); e.State != Online || e.Pairing != managed.PairingV2 || e.Hint != "" {
			t.Fatalf("%+v", e)
		}
		at := r.times("http://box:9101")
		if len(at) != 7 {
			t.Fatal(len(at), at)
		}
		within := func(i int, mult time.Duration) {
			t.Helper()
			base := mult * PairInterval
			if gap := at[i].Sub(at[i-1]); gap < base-base/pairJitterDiv || gap > base+base/pairJitterDiv {
				t.Fatalf("request %d came %s after the one before, want about %s", i+1, gap, base)
			}
		}
		within(1, 2)              // after the first 429
		within(2, pairBackoffMax) // after the second
		within(3, pairBackoffMax) // and it stays there
		within(4, pairBackoffMax) // the fourth 429
		within(5, 1)              // an answer that is not 429: the normal pace again
		within(6, 1)
		r.mu.Lock()
		defer r.mu.Unlock()
		if len(r.keys) != 1 {
			t.Fatal("the code changed while the panel waited")
		}
		for _, k := range r.events {
			if k == "failed" || k == "expired" || k == "cancelled" {
				t.Fatal(r.events)
			}
		}
	})
}

// A new code for the same machine right after a cancelled one (or a Re-pair right after Add host) continues
// the pace: the agent's count does not start again with the panel's enrollment. Another machine is asked at
// once.
func TestANewCodeForTheSameMachineKeepsThePace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		defer letEnrollmentsEnd()
		r := newPaceRig(t)
		r.answer = notYet
		for i := 0; i < 12; i++ { // an impatient admin: a new code every 20 seconds
			cr := r.add(t, "box")
			time.Sleep(20 * time.Second)
			if !r.m.Cancel(cr.ID) {
				t.Fatal("not cancelled")
			}
		}
		other := time.Now()
		cr := r.add(t, "other-box")
		time.Sleep(time.Second)
		r.m.Cancel(cr.ID)
		synctest.Wait()
		at := r.times("http://box:9101")
		if len(at) < 12 {
			t.Fatal(len(at))
		}
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < PairInterval-PairInterval/pairJitterDiv {
				t.Fatalf("request %d came %s after the one before", i+1, gap)
			}
		}
		if !neverRefused(at) {
			t.Fatal("the agent's budget was reached:", at)
		}
		if o := r.times("http://other-box:9101"); len(o) != 1 || o[0].Sub(other) > time.Second {
			t.Fatal("another machine waited for this one's turn:", o)
		}
	})
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
