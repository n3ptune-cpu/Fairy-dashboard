package poller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Fourth review of build 14, F4. The panel kept an agent's tokens under the agent's id and looked them up when
// a request was made, and the request went to the address in the entry the caller had in hand. After a Re-pair
// of the id to another machine the two no longer belonged together: a poll of the OLD entry that started just
// after it (the scheduler lets a job that was picked finish) sent the NEW machine's read token to the OLD
// address, and a control request made with the old entry in hand would have sent the new control token there.

// authSeen is a stand-in agent that keeps the Authorization header of every request it gets.
type authSeen struct {
	mu  sync.Mutex
	got []string
}

func (s *authSeen) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

func tokenAgent(t *testing.T) (string, *authSeen) {
	t.Helper()
	seen := &authSeen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.mu.Lock()
		seen.got = append(seen.got, r.Header.Get("Authorization"))
		seen.mu.Unlock()
		_ = json.NewEncoder(w).Encode(testutil.AgentMetrics())
	}))
	t.Cleanup(srv.Close)
	return srv.URL, seen
}

// The four tokens of the tests: the old machine's and the new machine's.
var (
	oldRead, oldControl = strings.Repeat("o", 32), strings.Repeat("p", 32)
	newRead, newControl = strings.Repeat("n", 32), strings.Repeat("m", 32)
)

// onlyOf fails the test when the agent got a request with another Authorization header than the given ones,
// without printing the header.
func onlyOf(t *testing.T, who string, seen *authSeen, own ...string) {
	t.Helper()
	for i, h := range seen.all() {
		switch {
		case strings.Contains(h, newRead) || strings.Contains(h, newControl):
			if !slices.Contains(own, h) {
				t.Errorf("%s: request %d carried a token of the NEW entry", who, i+1)
			}
		case !slices.Contains(own, h):
			t.Errorf("%s: request %d carried a header of %d bytes that is not one of its own", who, i+1, len(h))
		}
	}
}

func rePairPanel(t *testing.T) (*Poller, config.Agent, config.Agent, *authSeen, *authSeen) {
	t.Helper()
	oldURL, oldSeen := tokenAgent(t)
	newURL, newSeen := tokenAgent(t)
	cfg := testutil.Config(t, []string{deadURL(t)}, deadURL(t), deadURL(t))
	p, err := New(cfg, config.Secrets{SessionSecret: "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	was := config.Agent{ID: "pc-a", Name: "pc-a", URL: oldURL, Managed: true, Token: oldRead, ControlToken: oldControl}
	now := config.Agent{ID: "pc-a", Name: "pc-a", URL: newURL, Managed: true, Token: newRead, ControlToken: newControl}
	if err := p.AddAgent(was, nil); err != nil {
		t.Fatal(err)
	}
	return p, was, now, oldSeen, newSeen
}

func TestAfterARePairTheOldAddressNeverGetsTheNewTokens(t *testing.T) {
	p, was, now, oldSeen, newSeen := rePairPanel(t)
	ctx := context.Background()
	entry := p.AgentEntry("pc-a")
	p.pollEntry(ctx, was, entry, 0)
	if _, _, err := p.AgentRequest(ctx, was, "POST", "/power", map[string]any{"action": "cancel"}); err != nil {
		t.Fatal(err)
	}
	if got := oldSeen.all(); !slices.Equal(got, []string{"Bearer " + oldRead, "Bearer " + oldControl}) || snap(p)["pc-a"].Status != "online" {
		t.Fatalf("the fixture: the old machine got %d requests and is shown as %q", len(got), snap(p)["pc-a"].Status)
	}

	// Re-pair, first half: the host is taken out. A poll of its entry that starts now sends nothing.
	p.RemoveAgent("pc-a")
	p.pollEntry(ctx, was, entry, 0)
	// Second half: the id is another machine now, at another address, with tokens of its own.
	if err := p.AddAgent(now, nil); err != nil {
		t.Fatal(err)
	}
	// The old entry's poll job starts just after that, and so does everything else that has the old entry in hand.
	p.pollEntry(ctx, was, entry, 0)
	if _, err := p.Backend.FetchAgentMetrics(ctx, was, time.Second); err == nil {
		t.Error("a poll of the replaced entry was answered")
	}
	for _, method := range []string{"GET", "POST"} {
		var ns *NotSent
		if _, _, err := p.AgentRequest(ctx, was, method, "/power", nil); !errors.As(err, &ns) {
			t.Errorf("%s with the replaced entry: %v, want a request that was not sent", method, err)
		}
	}
	if _, err := p.FetchProcesses(ctx, was, "pc-a", 25, "cpu"); err == nil {
		t.Error("the process list was asked for with the replaced entry")
	}
	if got := oldSeen.all(); len(got) != 2 {
		t.Errorf("the old address got %d request(s) after its entry was replaced, want none", len(got)-2)
	}
	onlyOf(t, "the old address", oldSeen, "Bearer "+oldRead, "Bearer "+oldControl)

	// The new entry is asked with its own tokens, at its own address.
	p.pollEntry(ctx, now, p.AgentEntry("pc-a"), 0)
	if _, _, err := p.AgentRequest(ctx, now, "POST", "/power", map[string]any{"action": "cancel"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.FetchProcesses(ctx, now, "pc-a", 25, "cpu"); err != nil {
		t.Fatal(err)
	}
	if got := newSeen.all(); !slices.Equal(got, []string{"Bearer " + newRead, "Bearer " + newControl, "Bearer " + newRead}) || snap(p)["pc-a"].Status != "online" {
		t.Errorf("the new machine got %d requests, not its poll, its control request and its process list with its own tokens; it is shown as %q", len(got), snap(p)["pc-a"].Status)
	}
}

// The same while the old entry's requests are on their way from several sides: whichever moment the Re-pair
// falls into, the old address gets its own tokens or nothing, and the new address only the new ones.
func TestRequestsOfAnOldEntryDuringARePairCarryItsOwnTokensOrNothing(t *testing.T) {
	p, was, now, oldSeen, newSeen := rePairPanel(t)
	ctx := context.Background()
	entry := p.AgentEntry("pc-a")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				switch i % 2 {
				case 0:
					p.pollEntry(ctx, was, entry, 0)
				default:
					_, _, _ = p.AgentRequest(ctx, was, "POST", "/power", map[string]any{"action": "cancel"})
					_, _ = p.FetchProcesses(ctx, was, "pc-a", 25, "cpu")
				}
			}
		}(i)
	}
	time.Sleep(10 * time.Millisecond)
	p.RemoveAgent("pc-a")
	if err := p.AddAgent(now, nil); err != nil {
		t.Fatal(err)
	}
	p.pollEntry(ctx, now, p.AgentEntry("pc-a"), 0)
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
	if len(oldSeen.all()) == 0 || len(newSeen.all()) == 0 {
		t.Fatalf("the fixture: %d requests to the old address, %d to the new one", len(oldSeen.all()), len(newSeen.all()))
	}
	onlyOf(t, "the old address", oldSeen, "Bearer "+oldRead, "Bearer "+oldControl)
	onlyOf(t, "the new address", newSeen, "Bearer "+newRead, "Bearer "+newControl)
	if info, sys, known := p.AgentSeen("pc-a"); !known || info.Entry != p.AgentEntry("pc-a") || sys.Status != "online" {
		t.Errorf("the new entry after the Re-pair: row %v, entry %d of %d, %q", known, info.Entry, p.AgentEntry("pc-a"), sys.Status)
	}
}

// A Re-pair of the same machine keeps its address and gives it new tokens. A request made with the entry of
// before goes to that address with the tokens the address has now: the old ones are no longer the machine's.
func TestAfterARePairAtTheSameAddressARequestCarriesTheTokensOfNow(t *testing.T) {
	p, was, now, oldSeen, _ := rePairPanel(t)
	ctx := context.Background()
	now.URL = was.URL
	p.RemoveAgent("pc-a")
	if err := p.AddAgent(now, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.AgentRequest(ctx, was, "POST", "/power", map[string]any{"action": "cancel"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Backend.FetchAgentMetrics(ctx, was, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := oldSeen.all(); !slices.Equal(got, []string{"Bearer " + newControl, "Bearer " + newRead}) {
		t.Errorf("the machine got %d request(s), not the two with the tokens it has now", len(got))
	}
}

// A poll job of an entry that was replaced before the job started sends nothing, also when the new entry has
// the same address (its answer would be dropped: pollEntry).
func TestAPollOfAnEntryThatWasReplacedBeforeItStartedSendsNothing(t *testing.T) {
	p, was, now, oldSeen, _ := rePairPanel(t)
	ctx := context.Background()
	entry := p.AgentEntry("pc-a")
	now.URL = was.URL
	p.RemoveAgent("pc-a")
	p.pollEntry(ctx, was, entry, 0)
	if err := p.AddAgent(now, nil); err != nil {
		t.Fatal(err)
	}
	p.pollEntry(ctx, was, entry, 0)
	if got := oldSeen.all(); len(got) != 0 {
		t.Errorf("%d request(s) were sent for an entry that was no longer the panel's", len(got))
	}
	if _, _, known := p.AgentSeen("pc-a"); known {
		t.Error("a row was made for the new entry by a poll of the old one")
	}
	p.pollEntry(ctx, now, p.AgentEntry("pc-a"), 0)
	if got := oldSeen.all(); !slices.Equal(got, []string{"Bearer " + newRead}) || snap(p)["pc-a"].Status != "online" {
		t.Errorf("the new entry's own poll: %d request(s), shown as %q", len(got), snap(p)["pc-a"].Status)
	}
}
