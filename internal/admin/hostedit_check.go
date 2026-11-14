package admin

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// hostEditsCheck is the part of hostOptionsCheck about the machines themselves: the names and the
// Wake-on-LAN details an admin changed in the panel (state.json, "host_options", "hosts":
// adminstate.HostEdit). They are not in config.yaml either, so this lists them:
//
//   - ok: one line per machine with decisions: who made the last one and when, the name with config.yaml's,
//     and the Wake-on-LAN that counts with config.yaml's;
//   - warn: Wake-on-LAN decisions the panel does not use (the relay is no agent with wol_relay: true, no MAC
//     or no relay is left, a relay decision next to a target address or password of config.yaml), and
//     decisions kept under an id that is no machine of this panel.
//
// A machine's site is not here: it is the assignment of the Users page (`fairy admin sites`).
func hostEditsCheck(c *config.Panel, o adminstate.HostOptions) (ok, warn []string) {
	agents, systems, nodeOf := map[string]config.Agent{}, map[string]config.SystemOverride{}, map[string]string{}
	for _, a := range c.Agents {
		agents[a.ID] = a
	}
	for _, s := range c.Systems {
		systems[s.ID] = s
	}
	decided, _ := poller.NodeAgents(c)
	for node, agent := range decided {
		nodeOf[agent] = node
	}
	wolText := func(w *config.Wol, none string) string {
		if w == nil {
			return none
		}
		port := w.Port
		if port == 0 {
			port = 9
		}
		return fmt.Sprintf("to %s through %s, port %d", strings.Join(w.AllMACs(), ", "), w.Relay, port)
	}
	var strangers []string
	for _, key := range slices.Sorted(maps.Keys(o.Hosts)) {
		e := o.Hosts[key]
		if !poller.PossibleRow(key, c.Agents, c.Proxmox) {
			strangers = append(strangers, fmt.Sprintf("Edit host: state.json has changes about %s itself (name, Wake-on-LAN), which is no agent's id and not the id of a node of a proxmox: entry: "+
				"they are ignored (a host that was removed?)", key))
			continue
		}
		who := "changed in the panel (\"Edit host\")"
		if e.By != "" {
			who += " by " + cell(e.By)
		}
		if !e.At.IsZero() {
			who += ", " + e.At.UTC().Format("2006-01-02 15:04 UTC")
		}
		var parts []string
		if e.Name != "" {
			was := "the name the machine or its node reports"
			if s := systems[key]; s.Name != "" {
				was = fmt.Sprintf("%q", s.Name)
			} else if a := agents[key]; a.Name != "" && nodeOf[key] == "" {
				was = fmt.Sprintf("%q", a.Name)
			}
			parts = append(parts, fmt.Sprintf("name %q (config.yaml: %s)", e.Name, was))
		}
		if e.Wol != nil {
			base := systems[key].Wol
			if base == nil && nodeOf[key] != "" { // the agent of a node: the wol: written under the node's id
				base = systems[nodeOf[key]].Wol
			}
			w, unused := e.Wol.Apply(base)
			if unused == "" && w != nil && e.Wol.Relay != "" {
				switch a, isAgent := agents[e.Wol.Relay]; {
				case !isAgent:
					unused = "the relay " + e.Wol.Relay + " is no agent of this panel"
				case !a.WolRelay:
					unused = "the relay " + e.Wol.Relay + " has no wol_relay: true"
				case e.Wol.Relay == key || nodeOf[e.Wol.Relay] == key:
					unused = "a machine cannot send its own wake packet"
				}
			}
			if unused != "" {
				warn = append(warn, fmt.Sprintf("Edit host: the Wake-on-LAN details changed in the panel for %s are not used (%s): config.yaml's count (%s)", key, cell(unused), cell(wolText(base, "none"))))
			} else {
				parts = append(parts, fmt.Sprintf("Wake-on-LAN %s (config.yaml: %s)", wolText(w, "off"), wolText(base, "none")))
			}
		}
		if len(parts) > 0 {
			ok = append(ok, fmt.Sprintf("host %s: %s, used INSTEAD of what config.yaml says about the same thing until Reset: %s", key, who, cell(strings.Join(parts, "; "))))
		}
	}
	return ok, append(warn, firstLines(strangers, "Edit host: and %d more ids with changes about a machine itself that are no machine's")...)
}
