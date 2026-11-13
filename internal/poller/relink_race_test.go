package poller

import (
	"fmt"
	"maps"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// relink read the configuration and stored its answer in two steps, with no lock of its own around the two.
// Two changes at once (a host is added while a Proxmox node is added, or both are removed) could each read,
// and the one that had read first store last: what linkOfLocked then went by was the answer to a
// configuration that no longer was, until the next change (review of build 14, second round, R2).
//
// Here the answer changes with either change: alpha-x (node: pve) is decided while it is alone, and not while
// other-y, whose id begins with no id_prefix, can mean the same node. After both calls have come back, what is
// kept must be what the configuration says.
func TestTheKeptLinksAreTheConfigurationsAfterTwoChangesAtOnce(t *testing.T) {
	nodes := map[string]string{"alpha": "pve"}
	p, _ := linkPanel(t, [][2]string{{"alpha", "alpha-"}}, fmt.Sprintf(linkAgent, "alpha-x", "X", "x", "node: pve"), "", nodes)
	pfx := "beta-"
	beta := config.Proxmox{ID: "beta", IDPrefix: &pfx, ClusterName: "B", Standalone: true, URLs: []string{"https://b:8006"}, TokenID: "fairy@pve!panel", Managed: true}
	y := config.Agent{ID: "other-y", Name: "Y", URL: "http://y", Node: "pve", Managed: true, Token: testutil.ReadToken}
	want := func() map[string]string {
		links, _ := p.links()
		d := map[string]string{}
		for id, l := range links {
			d[id] = l.ID
		}
		return d
	}
	both := func(a, b func()) {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); a() }()
		go func() { defer wg.Done(); b() }()
		wg.Wait()
	}
	for i := 0; i < 3000; i++ {
		both(func() { _ = p.AddProxmox(beta) }, func() { _ = p.AddAgent(y, nil) })
		if got := *p.decided.Load(); !maps.Equal(got, want()) {
			t.Fatalf("after two adds at once (round %d): kept %v, the configuration says %v", i, got, want())
		}
		both(func() { p.RemoveProxmox("beta") }, func() { p.RemoveAgent("other-y") })
		if got := *p.decided.Load(); !maps.Equal(got, want()) {
			t.Fatalf("after two removes at once (round %d): kept %v, the configuration says %v", i, got, want())
		}
	}
}

// The same without luck: the first call is held between its reading and its keeping while a second change,
// with its own call, is made. Without the lock the second call kept its answer and the first one, let go,
// kept the older answer over it. With it the second change's call waits for the first.
func TestALinkThatWasReadBeforeAChangeIsNotKeptAfterIt(t *testing.T) {
	nodes := map[string]string{"alpha": "pve"}
	p, _ := linkPanel(t, [][2]string{{"alpha", "alpha-"}, {"beta", "beta-"}}, fmt.Sprintf(linkAgent, "alpha-x", "X", "x", "node: pve"), "", nodes)
	if got := (*p.decided.Load())["alpha-x"]; got != "alpha-node-pve" {
		t.Fatalf("the fixture: alpha-x alone is decided: %q", got)
	}
	y := config.Agent{ID: "other-y", Name: "Y", URL: "http://y", Node: "pve", Managed: true, Token: testutil.ReadToken}
	held, letGo := make(chan struct{}), make(chan struct{})
	first := true
	p.relinkRead = func() {
		if first { // only the first call is held; it has read the configuration without other-y
			first = false
			close(held)
			<-letGo
		}
	}
	firstDone, secondDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(firstDone); p.relink() }()
	<-held
	go func() { defer close(secondDone); _ = p.AddAgent(y, nil) }()
	select {
	case <-secondDone: // without the lock: the change and its call are through while the first call is held
	case <-time.After(200 * time.Millisecond): // with it: the change's call waits for the first
	}
	close(letGo)
	<-firstDone
	<-secondDone
	if got, ok := (*p.decided.Load())["alpha-x"]; ok {
		t.Fatalf("other-y can mean the node of alpha-x now, and the links that are kept still give alpha-x %s: the answer that was read before the change was kept after it", got)
	}
}
