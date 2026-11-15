package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// `fairy serve --demo` has plugs with sockets, so that the Power page can be looked at without the hardware:
// a powerbar with four sockets, a plug its own agent never switches, and the plug of the machine that reads
// them. The rows are what a Fairy-agent of v0.10.0 relays (api.SocketRow, member for member), and the panel
// keeps every socket of them.
func TestDemoHasPlugsWithSockets(t *testing.T) {
	cfg, _, p, err := Build(t.TempDir(), "http://127.0.0.1:8090")
	if err != nil {
		t.Fatal(err)
	}
	var agent = cfg.Agents[0]
	for _, a := range cfg.Agents {
		if a.ID == "mini-i7-agent" {
			agent = a
		}
	}
	m, err := NewBackend(cfg).FetchAgentMetrics(context.Background(), agent, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, src := range jsonx.Objs(m["power_sources"]) {
		list, _ := src["sockets"].([]any)
		for _, item := range list {
			raw, _ := json.Marshal(item)
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			var row api.SocketRow
			if err := dec.Decode(&row); err != nil {
				t.Errorf("%v: not a socket row of the agent: %v", src["id"], err)
				continue
			}
			rows++
			if api.CheckSocketID(row.ID) != nil || api.CheckSocketTag(row.Tag) != nil || row.Name == "" || row.Switchable != (row.Why == nil) ||
				(row.State != api.SocketStateOn && row.State != api.SocketStateOff) {
				t.Errorf("%v: %+v", src["id"], row)
			}
			for _, k := range []string{"id", "name", "state", "switchable", "why", "tag", "error"} { // every member, also when null
				if _, ok := item.(map[string]any)[k]; !ok {
					t.Errorf("%v/%s: no member %q", src["id"], row.ID, k)
				}
			}
		}
	}
	if rows != 6 {
		t.Fatalf("%d sockets in the demo agent's answer, want 6", rows)
	}
	p.PollOnce(context.Background())
	kept := map[string]int{}
	states := map[string]string{}
	for _, s := range p.Power.Overview().Sources {
		kept[s.ID] = len(s.Sockets)
		for _, k := range s.Sockets {
			states[s.ID+"/"+k.ID] = k.State + " " + k.Why
		}
	}
	if kept["mini-i7-agent/desk-bar"] != 4 || kept["mini-i7-agent/mini-plug"] != 1 || kept["mini-i7-agent/corner-plug"] != 1 || kept["plug-tower"] != 0 {
		t.Errorf("sockets the panel keeps: %v", kept)
	}
	if states["mini-i7-agent/desk-bar/ch2"] != "off " || states["mini-i7-agent/desk-bar/ch4"] != "off "+api.WhyNotAllowed ||
		states["mini-i7-agent/corner-plug/main"] != "on "+api.WhyFeedsSelf {
		t.Errorf("the sockets as the panel keeps them: %v", states)
	}
}
