package web

import (
	"fmt"
	"sync"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// I28: Add host reads the configuration (to check a new host against it) while the poller adds and removes
// hosts and Proxmox nodes. The read goes through the poller's locks: run with -race, this test fails on a
// plain copy of the shared config struct.
func TestEnrollConfigIsReadUnderThePollersLocks(t *testing.T) {
	e := setup(t)
	hooks := e.s.enrollHooks()
	before := len(hooks.Config().Agents)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				c := hooks.Config()
				if len(c.Agents) < before || c.PublicURL != e.s.Cfg.PublicURL {
					t.Error("a snapshot lost configured agents or settings", len(c.Agents))
					return
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("race-%d", i%7)
		a := config.Agent{ID: id, URL: "http://" + id + ":9101", Token: "race-read-token-xxxxxxxxxxxxxxxxxxx", Managed: true}
		if err := e.s.Poller.AddAgent(a, &config.SystemOverride{ID: id}); err != nil {
			t.Fatal(err)
		}
		prefix := "px-" + id + "-"
		px := config.Proxmox{ID: "px-" + id, IDPrefix: &prefix, URLs: []string{"https://" + id + ":8006"}, TokenID: "fairy@pve!panel-x",
			TokenSecret: "0b7c4e2a-aaaa-bbbb-cccc-444455556666", Managed: true}
		if err := e.s.Poller.AddProxmox(px); err != nil {
			t.Fatal(err)
		}
		e.s.Poller.RemoveProxmox(px.ID)
		e.s.Poller.RemoveAgent(id)
	}
	close(stop)
	wg.Wait()
	if got := len(hooks.Config().Agents); got != before {
		t.Fatal("agents left behind", got, before)
	}
}
