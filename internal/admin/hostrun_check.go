package admin

import (
	"fmt"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// A machine's own timings of "Update unpowered hosts" can be changed in the panel ("Edit host", state.json:
// adminstate.RunEdit). They lie over the wake_update: block of the machine's systems: entry, or over the
// panel's own timing where it has none. `fairy admin config check` says both: the host's line of "Edit host"
// names each timing with config.yaml's value (hostEditsCheck), the line about a systems: entry says which
// of its keys is not in force (wakeUpdateCheck), and the notes about a short off_after_s and about a
// countdown a person can also choose are made from the value in force.

// machineRuns finds, for an id of the configuration, what counts for that machine's two timings. A machine
// can have two ids (its agent's and its Proxmox node's): the block and the decision are the machine's under
// either (poller.WakeUpdateOverride).
type machineRuns struct {
	o       adminstate.HostOptions
	systems map[string]config.SystemOverride
	other   map[string]string // an agent's id -> its node's id, and the node's id -> its agent's
}

func newMachineRuns(c *config.Panel, o adminstate.HostOptions) machineRuns {
	m := machineRuns{o: o, systems: map[string]config.SystemOverride{}, other: map[string]string{}}
	for _, s := range c.Systems {
		m.systems[s.ID] = s
	}
	decided, _ := poller.NodeAgents(c)
	for node, agent := range decided {
		m.other[agent], m.other[node] = node, agent
	}
	return m
}

func (m machineRuns) ids(id string) []string {
	if x := m.other[id]; x != "" {
		return []string{id, x}
	}
	return []string{id}
}

// decision is what "Edit host" decided for the machine with this id about key (0: nothing). Only a value the
// key can have is one, as in the running panel.
func (m machineRuns) decision(id, key string) int {
	t, _ := config.RunTimingOf(key)
	for _, x := range m.ids(id) {
		if v := m.o.Hosts[x].Run.Value(key); v != 0 && t.In(v) {
			return v
		}
	}
	return 0
}

// block is what config.yaml's systems: entries set for the machine with this id about key (0: nothing): the
// larger value where both of its ids have one, as the running panel takes it.
func (m machineRuns) block(id, key string) int {
	v := 0
	for _, x := range m.ids(id) {
		w := m.systems[x].WakeUpdate
		if w == nil {
			continue
		}
		switch key {
		case config.KeyOffAfter:
			v = max(v, w.OffAfterS)
		case config.KeyShutdownDelay:
			v = max(v, w.ShutdownDelayS)
		}
	}
	return v
}

// offAfterRisk is the note about a time below the default after which a machine counts as off.
func offAfterRisk(what string, s int) string {
	const def = 900
	mins := fmt.Sprintf("%d s", s)
	if s%60 == 0 {
		mins = fmt.Sprintf("%d min", s/60)
	}
	return fmt.Sprintf("%s is %d (the default is %d): a machine that restarts for longer than %s can be taken for off by a run, "+
		"which then wakes it, updates it and switches it off as a machine it woke", what, s, def, mins)
}

// countdownSame is the note about a countdown of a run's shutdown that a person can also choose in the panel.
func countdownSame(what string, s int) string {
	return fmt.Sprintf("%s is %d, which is also a countdown a person can choose in the Power menu of a machine's page (10 s, 1 min, 5 min, 15 min; 60 s is what a power request without a delay gets): "+
		"a shutdown somebody asks for with that countdown in the place of the run's cannot be told from the run's own, and a run that is cancelled, or lets go of the machine, calls it off", what, s)
}

// hostRunCheck is the part of a host's line of "Edit host" about its own run timings (parts), and the notes
// about their values.
func (m machineRuns) hostRunCheck(key string, e adminstate.HostEdit) (parts, notes []string) {
	for _, t := range config.MachineRunTimings() {
		v := e.Run.Value(t.Key)
		if v == 0 || !t.In(v) {
			continue
		}
		was := "none of its own, the panel's counts"
		if b := m.block(key, t.Key); b != 0 {
			was = fmt.Sprint(b)
		}
		parts = append(parts, fmt.Sprintf("wake_update.%s %d (config.yaml: %s)", t.Key, v, was))
		what := fmt.Sprintf("host %s: wake_update.%s (changed in the panel)", key, t.Key)
		switch {
		case t.Key == config.KeyOffAfter && v < t.Default:
			notes = append(notes, offAfterRisk(what, v))
		case t.Key == config.KeyShutdownDelay && personCountdown[v]:
			notes = append(notes, countdownSame(what, v))
		}
	}
	return parts, notes
}

// hostRuns reads what "Edit host" decided about the machines' own run timings (state.json), for the lines
// about config.yaml's systems: entries. A state.json that cannot be read: nothing (hostOptionsCheck says so).
func hostRuns(cfg *config.Panel) machineRuns {
	o, _, err := adminstate.ReadHostOptions(cfg.DataDir)
	if err != nil {
		o = adminstate.HostOptions{}
	}
	return newMachineRuns(cfg, o)
}
