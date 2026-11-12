package poller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// ListAskedAgo: how old the panel's list from a Proxmox API can be, for the wake / update / shut down run's
// "which guests run on this node" (audit 6, r4-p2-01: the list was only asked to be from after the node came
// online, and with the API polled every five minutes it was minutes old). It counts from the moment the request
// left, on the steady clock: an answer that was on its way for three seconds shows the cluster as it was up to
// three seconds before it arrived, and the time of day has no part in it.
func TestListAskedAgoCountsFromWhenTheRequestLeft(t *testing.T) {
	b := testutil.NewBackend()
	var ck sync.Mutex
	steady, skew := time.Duration(0), time.Duration(0)
	pass := func(d, set time.Duration) {
		ck.Lock()
		steady, skew = steady+d, skew+set
		ck.Unlock()
	}
	api, takes := true, time.Duration(0)
	b.PVE = func() ([]jsonx.M, error) {
		pass(takes, 0) // the answer is this long on its way
		if !api {
			return nil, context.DeadlineExceeded
		}
		return testutil.PVEResources(), nil
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
	ctx := context.Background()
	pve := p.Clusters()[0]
	src := "proxmox:" + pve.ID
	poll := func() { p.pollProxmox(ctx, pve, time.Second) }
	want := func(when string, ago time.Duration, ok bool) {
		t.Helper()
		if got, has := p.ListAskedAgo(src); got != ago || has != ok {
			t.Errorf("%s: asked for %v ago, has a list: %v; want %v, %v", when, got, has, ago, ok)
		}
	}
	want("before the first poll", 0, false)
	poll()
	want("right after a poll that took no time", 0, true)
	pass(7*time.Second, 0)
	want("7 s later", 7*time.Second, true)
	// an answer that is three seconds on its way: the list is three seconds old when it arrives
	set(func() { takes = 3 * time.Second })
	poll()
	want("right after an answer that took 3 s", 3*time.Second, true)
	set(func() { takes = 0 })
	// the time of day has no part in it
	pass(5*time.Second, -time.Hour)
	want("5 s later, the time of day set back an hour", 8*time.Second, true)
	pass(0, 3*time.Hour)
	want("the time of day set forward", 8*time.Second, true)
	// a failed poll: the panel still shows the list of the poll before, and knows that it is not the latest
	set(func() { api = false })
	pass(2*time.Second, 0)
	poll()
	want("after a failed poll", 0, false)
	set(func() { api = true })
	pass(2*time.Second, 0)
	poll()
	want("after the next good poll", 0, true)
	if _, ok := p.ListAskedAgo("proxmox:no-such-cluster"); ok {
		t.Error("a source the panel does not have has a list")
	}
	// a cluster that was taken out of the panel leaves nothing behind
	p.mu.Lock()
	n := len(p.sourceAsked)
	p.mu.Unlock()
	if n != 1 {
		t.Errorf("%d entries kept, want one per Proxmox source", n)
	}
}
