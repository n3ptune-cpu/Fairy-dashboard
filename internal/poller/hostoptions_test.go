package poller

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) warns(about string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, line := range strings.Split(s.b.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, about) {
			n++
		}
	}
	return n
}

// LinksOf is where an agent's entry and what an admin changed in the panel meet: the entry's links, with
// every decision laid over the line it is about, asked at every poll. A change counts with the next poll and
// needs no restart; what no decision is about stays the entry's; a decision the panel cannot use links nothing
// and is said once; the entry itself is never changed.
func TestLinksOfIsTheEntryWithThePanelsChanges(t *testing.T) {
	logs := &syncBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	for i := range cfg.Agents {
		if cfg.Agents[i].ID == "laptop" { // what config.yaml says: the UPS powers the laptop, the spare plug is the tower's
			cfg.Agents[i].UPSPowers = map[string][]string{"rack": {"laptop"}}
			cfg.Agents[i].PowerLinks = map[string]string{"spare": "tower"}
			cfg.Agents[i].PowerExcludeFromTotal = []string{"spare"}
		}
	}
	b := testutil.NewBackend()
	b.Metrics["laptop"] = func() (jsonx.M, error) {
		m := testutil.AgentMetrics()
		m["ups"] = []any{jsonx.M{"name": "rack", "state": "online"}}
		m["power_sources"] = append(m["power_sources"].([]any), jsonx.M{"id": "spare", "name": "Spare", "type": "shelly", "ok": true, "watts": 55.0})
		return m, nil
	}
	p, err := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	laptop, _ := p.AgentByID("laptop")
	state := func() (powers []string, spareOn string, spareInTotal bool) {
		t.Helper()
		u := p.UPS.State("laptop/rack")
		if u == nil {
			t.Fatal("the laptop's UPS is not there")
		}
		for _, st := range p.Power.Overview().Sources {
			if st.ID == "laptop/spare" {
				return u.Powers, model.Deref(st.SystemID), st.IncludeInTotal
			}
		}
		t.Fatal("the laptop's spare plug is not there")
		return nil, "", false
	}
	// no override: the entry's own links, at the poll and when asked
	if l := p.LinksOf(laptop); !adminstate.SameLinks(l, laptop.Links()) {
		t.Fatalf("without an override: %+v", l)
	}
	if powers, on, total := state(); !slices.Equal(powers, []string{"laptop"}) || on != "tower" || total {
		t.Fatalf("config.yaml's links: powers %v, spare on %q, in the total %v", powers, on, total)
	}
	edit := func(fn func(o *adminstate.HostOptions)) {
		t.Helper()
		if _, _, err := p.Admin.EditHostOptions("test", func(o *adminstate.HostOptions) error { fn(o); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	// set changes the laptop's entry of the overlay
	set := func(fn func(e *adminstate.HostOption)) func(o *adminstate.HostOptions) {
		return func(o *adminstate.HostOptions) {
			e := o.Agents["laptop"]
			fn(&e)
			o.Agents["laptop"] = e
		}
	}
	// two decisions about the UPS: it powers the tower and a guest too. The row config.yaml names (the laptop)
	// stays, and so do the other two fields
	edit(set(func(e *adminstate.HostOption) { e.SetUPS("rack", "tower", true); e.SetUPS("rack", "qemu-100", true) }))
	l := p.LinksOf(laptop)
	if !slices.Equal(l.UPSPowers["rack"], []string{"laptop", "qemu-100", "tower"}) || l.PowerLinks["spare"] != "tower" || !slices.Equal(l.PowerExcludeFromTotal, []string{"spare"}) {
		t.Fatalf("with two decisions about the UPS: %+v", l)
	}
	// ... and nothing changed before the next poll, everything with it, without a restart
	if powers, _, _ := state(); !slices.Equal(powers, []string{"laptop"}) {
		t.Fatalf("before the next poll: %v", powers)
	}
	p.PollOnce(context.Background())
	if powers, on, total := state(); !slices.Equal(powers, []string{"laptop", "qemu-100", "tower"}) || on != "tower" || total {
		t.Fatalf("after the next poll: powers %v, spare on %q, in the total %v", powers, on, total)
	}
	// a row config.yaml names is taken off; the plug is linked to another row and put back into the total
	edit(set(func(e *adminstate.HostOption) {
		e.SetUPS("rack", "laptop", false)
		e.SetLink("spare", "laptop")
		e.SetExclude("spare", false)
	}))
	p.PollOnce(context.Background())
	if powers, on, total := state(); !slices.Equal(powers, []string{"qemu-100", "tower"}) || on != "laptop" || !total {
		t.Fatalf("all three fields changed: powers %v, spare on %q, in the total %v", powers, on, total)
	}
	// a link to none: the plug is on no row, whatever config.yaml says
	edit(set(func(e *adminstate.HostOption) { e.SetLink("spare", "") }))
	p.PollOnce(context.Background())
	if _, on, _ := state(); on != "" {
		t.Fatalf("linked to none: spare on %q", on)
	}
	edit(set(func(e *adminstate.HostOption) { e.SetLink("spare", "laptop") }))
	// the agent's entry is what it was: the overlay is never written into it
	if now, _ := p.AgentByID("laptop"); !adminstate.SameLinks(now.Links(), laptop.Links()) || !adminstate.SameLinks(laptop.Links(),
		config.Links{UPSPowers: map[string][]string{"rack": {"laptop"}}, PowerLinks: map[string]string{"spare": "tower"}, PowerExcludeFromTotal: []string{"spare"}}) {
		t.Fatalf("the entry changed: %+v", now.Links())
	}
	if base, ok := p.BaseLinks("laptop"); !ok || !adminstate.SameLinks(base, laptop.Links()) {
		t.Fatalf("BaseLinks: %+v", base)
	}
	if logs.warns("Edit host") != 0 {
		t.Fatalf("a WARN for overrides that are in order:\n%s", logs.b.String())
	}
	// rows the panel does not have get nothing: they are left out of what the managers get, and said once,
	// however often it is asked. "Does not power" a row there is not needs no word.
	edit(set(func(e *adminstate.HostOption) {
		e.SetUPS("rack", "old-pc", true)
		e.SetUPS("rack", "nodee-x", true)
		e.SetUPS("rack", "long-gone", false)
		e.SetLink("spare", "old-pc")
	}))
	for i := 0; i < 5; i++ {
		p.PollOnce(context.Background())
	}
	l = p.LinksOf(laptop)
	if _, linked := l.PowerLinks["spare"]; !slices.Equal(l.UPSPowers["rack"], []string{"qemu-100", "tower"}) || linked {
		t.Fatalf("rows there are not: %+v", l)
	}
	if powers, on, _ := state(); !slices.Equal(powers, []string{"qemu-100", "tower"}) || on != "" {
		t.Fatalf("the managers got: powers %v, spare on %q", powers, on)
	}
	if n := logs.warns("names rows the panel does not have"); n != 1 {
		t.Fatalf("%d WARN lines about the rows, want 1:\n%s", n, logs.b.String())
	}
	if !strings.Contains(logs.b.String(), `rows="nodee-x, old-pc"`) || !strings.Contains(logs.b.String(), "agent=laptop") {
		t.Fatalf("the WARN names the agent and the rows:\n%s", logs.b.String())
	}
	// said again when the list changes, and not when it does not
	edit(set(func(e *adminstate.HostOption) { e.DropUPS("rack", "nodee-x") }))
	p.PollOnce(context.Background())
	p.PollOnce(context.Background())
	if n := logs.warns("names rows the panel does not have"); n != 2 { // nodee-x is gone from the list: one more line, once
		t.Fatalf("%d WARN lines after the list changed, want 2:\n%s", n, logs.b.String())
	}
	// an override for an id that is no agent's: never read, said once
	edit(func(o *adminstate.HostOptions) {
		o.Agents["gone-agent"] = adminstate.HostOption{UPS: map[string]map[string]bool{"rack": {"laptop": true}}}
	})
	for i := 0; i < 4; i++ {
		p.PollOnce(context.Background())
	}
	if n := logs.warns("ids that are no agent's"); n != 1 || !strings.Contains(logs.b.String(), "agents=gone-agent") {
		t.Fatalf("%d WARN lines about the unknown agent, want 1:\n%s", n, logs.b.String())
	}
	// a node's or a guest's id is known by its form, also while its row is not there (the Proxmox API is down)
	for id, want := range map[string]bool{"laptop": true, "tower": true, "qemu-100": true, "node-srv-pvea": true, "lxc-9999": true, "node-": false, "qemu": false,
		"old-pc": false, "": false, "gone-agent": false} {
		if got := p.KnownRow(id); got != want {
			t.Errorf("KnownRow(%q) = %v, want %v", id, got, want)
		}
	}
	// Reset of everything: config.yaml's links again, with the next poll
	edit(func(o *adminstate.HostOptions) { delete(o.Agents, "laptop") })
	p.PollOnce(context.Background())
	if powers, on, total := state(); !slices.Equal(powers, []string{"laptop"}) || on != "tower" || total {
		t.Fatalf("after the overrides were taken out: powers %v, spare on %q, in the total %v", powers, on, total)
	}
	// An agent that does not answer: both managers only age what they have and read no link, so what is changed
	// meanwhile shows with the agent's next answer, not before (pollAgent hands them none of LinksOf then).
	b.Mu.Lock()
	answers := b.Metrics["laptop"]
	b.Metrics["laptop"] = func() (jsonx.M, error) { return nil, errors.New("down") }
	b.Mu.Unlock()
	p.PollOnce(context.Background())
	edit(set(func(e *adminstate.HostOption) { e.SetUPS("rack", "tower", true); e.SetLink("spare", "laptop") }))
	p.PollOnce(context.Background())
	if powers, on, _ := state(); !slices.Equal(powers, []string{"laptop"}) || on != "tower" {
		t.Fatalf("changed while the agent is silent: powers %v, spare on %q (want the picture of its last answer)", powers, on)
	}
	if l := p.LinksOf(laptop); !slices.Equal(l.UPSPowers["rack"], []string{"laptop", "tower"}) || l.PowerLinks["spare"] != "laptop" {
		t.Fatalf("what counts for it meanwhile: %+v", l)
	}
	b.Mu.Lock()
	b.Metrics["laptop"] = answers
	b.Mu.Unlock()
	p.PollOnce(context.Background())
	if powers, on, _ := state(); !slices.Equal(powers, []string{"laptop", "tower"}) || on != "laptop" {
		t.Fatalf("with its next answer: powers %v, spare on %q", powers, on)
	}
	edit(func(o *adminstate.HostOptions) { delete(o.Agents, "laptop") })
	// a second poller on the same folder (a restart): the same answer from its first poll
	edit(set(func(e *adminstate.HostOption) { e.SetUPS("rack", "tower", true); e.SetUPS("rack", "laptop", false) }))
	p2, err := New(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, b)
	if err != nil {
		t.Fatal(err)
	}
	p2.PollOnce(context.Background())
	if u := p2.UPS.State("laptop/rack"); u == nil || !slices.Equal(u.Powers, []string{"tower"}) {
		t.Fatalf("after a restart: %+v", u)
	}
	if !adminstate.SameLinks(p.LinksOf(laptop), p2.LinksOf(laptop)) {
		t.Fatal("the running panel and the restarted one differ")
	}
}
