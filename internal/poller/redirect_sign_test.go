package poller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Audit 5, round 3 (r3-p2-08), the poll's side, which stays as it is: an address that answers the poll with a
// redirect is not followed and is not the agent, but it answered: the machine (or what stands in front of it)
// runs. That is a sign of life like every other HTTP answer: never silence, so never part of the 15 minutes
// after which a run takes a machine for off, and the run's direct question does not say "nothing answered".
func TestARedirectAtTheAgentsAddressIsASignOfLifeForThePoll(t *testing.T) {
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://login.example.invalid/?rd="+r.URL.Path, http.StatusFound)
	}))
	defer front.Close()
	cfg := testutil.Config(t, []string{deadURL(t)}, front.URL, deadURL(t))
	cfg.RequestTimeout = 1
	p, err := New(cfg, config.Secrets{}, nil) // the real HTTP backend
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		p.PollOnce(context.Background())
	}
	i, sys, _ := p.AgentSeen("laptop")
	if i.Online || i.Sign != "its address answered: http 302" || !i.QuietSince.IsZero() || i.Silent.N != 0 {
		t.Fatalf("after three polls answered with a redirect: %+v", i)
	}
	if sys.Status == "online" {
		t.Fatalf("a redirect is not the agent's answer: %+v", sys)
	}
	if got := p.ProbeAgent(context.Background(), "laptop"); got != "http 302" {
		t.Fatalf("the direct question: %q, want \"http 302\" (something answered: never \"timeout\" or \"connection failed\")", got)
	}
}
