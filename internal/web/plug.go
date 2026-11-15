package web

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/powerctl"
)

// The sockets of a plug, the panel's part (PLUGS-DESIGN.md, stage 1a, sections 4.2, 4.5 and 4.6).
//
// Whether a socket can be switched at all is decided on the machine that reads the plug, in the agent's own
// config.yaml, and the agent says it in the socket's row (power.Manager keeps the row). Nothing here can turn
// that "no" into a "yes". What this file adds are the panel's own reasons to say no, from what the panel
// knows and the agent does not: which machine hangs on a socket, and whether it runs.
//
//   - the link: socket_links in the panel's entry of the agent names the row; the only socket of a plug takes
//     the plug's own link (power_links, system_hint: self); any other socket is linked to nothing;
//   - no socket of an agent is switched while the panel's entry of it has no home_assistant_system: the panel
//     has to be told once, in writing, where that agent's Home Assistant runs (as the agent has to be told
//     feeds_self before it switches anything);
//   - the locks: a socket that feeds the machine that reads the plugs, the machine Home Assistant runs on, or
//     the panel's own, is never switched from here, in either direction; and neither is a socket whose link
//     names no row the panel has, because for that one the panel cannot tell any of the three;
//   - "still running": a socket whose machine runs is not switched off without a second, typed confirmation
//     (api_plug.go).
//
//   - "off" asks first unless the panel has a sign that the machine on the socket is off (runningState): that it
//     does not answer is no such sign. And "off" for a socket that is linked to nothing asks first always: the
//     panel does not know what is plugged in.
//
// These are guards against a mistake. They depend on links somebody wrote, and a panel that was taken over
// has none of them. What holds against the panel is on the machine: switching is off unless its config turns
// it on, only listed sockets are switched, the ones in feeds_self never, and every switch needs a passkey
// approval the machine checks itself.

// socketLook is what the panel knows about one socket right now.
type socketLook struct {
	src   model.PowerSourceState
	sock  model.PowerSocket // as the agent's row has it
	agent config.Agent      // the agent that reads the plug
	dev   string            // the plug's id on that agent
	// link is the id the socket is linked to, as it is written ("": linked to nothing). shown is the id of the
	// row that machine is shown as right now, row that row (nil: no row has the id now).
	link, shown string
	row         *model.System
	// lock is why the panel never switches this socket ("": no lock): model.SocketLock...
	lock string
	// ownLink: the link is the plug's own (the only socket of a plug), not a line of socket_links.
	ownLink bool
	// knownOff: the linked machine was seen to go off and has given no sign of life since (Poller.KnownOff).
	// at and fresh: the time of this look, and how old a reading of the plug may be to count (offline_after).
	// They are what runningState needs beyond the row.
	knownOff bool
	at       time.Time
	fresh    time.Duration
}

// plugLooks answers questions about the sockets of one moment: the rows as they are now, and what was worked
// out for an agent once.
type plugLooks struct {
	s     *Server
	rows  map[string]*model.System
	snap  []model.System
	now   time.Time // the time of these looks, by the clock a relayed reading is stamped with (power.Manager: time.Now)
	agent map[string]plugRows
	names map[string]string // the names of the agents' machines
}

// plugRows are the ids of the rows that are an agent's own machine (or the machine it runs on), and of the
// machine its Home Assistant runs on (nil: not set; haKnown false: set, and no row has that id now).
type plugRows struct {
	own, ha map[string]bool
	haKnown bool
}

func (s *Server) newPlugLooks(snap []model.System) *plugLooks {
	l := &plugLooks{s: s, snap: snap, rows: make(map[string]*model.System, len(snap)), agent: map[string]plugRows{}, names: map[string]string{}, now: time.Now()}
	for i := range snap {
		l.rows[snap[i].ID] = &snap[i]
	}
	return l
}

// machineName is the name of the machine an agent runs on, for a sentence (worked out once per agent).
func (l *plugLooks) machineName(agentID string) string {
	if n, ok := l.names[agentID]; ok {
		return n
	}
	n := l.s.machineNames([]string{agentID})[0]
	l.names[agentID] = n
	return n
}

// machine returns the ids under which the machine known as id can be linked: the id itself, the machine's
// other id (a Proxmox node and its agent are one machine with two ids), the id of the row it is shown as, and,
// for a guest (a VM or a container), its node with that node's ids: what feeds the node feeds the guest.
// known reports whether any of them is a row right now.
func (l *plugLooks) machine(id string) (ids map[string]bool, known bool) {
	p := l.s.Poller
	ids = map[string]bool{}
	add := func(x string) {
		for _, y := range append(p.SameMachine(x), p.ShownID(x)) {
			if y != "" {
				ids[y] = true
			}
		}
	}
	add(id)
	if a, ok := p.AgentByID(id); ok {
		if to := p.LinkOf(a); to != "" { // the node, or the guest, the agent runs on (node:, links_to:)
			add(to)
		}
	}
	for x := range ids {
		if r := l.rows[x]; r != nil {
			known = true
			if (r.Kind == "qemu" || r.Kind == "lxc") && r.ParentID != nil && *r.ParentID != "" {
				add(*r.ParentID)
			}
		}
	}
	return ids, known
}

func (l *plugLooks) of(a config.Agent) plugRows {
	if r, ok := l.agent[a.ID]; ok {
		return r
	}
	var r plugRows
	r.own, _ = l.machine(a.ID)
	if a.HomeAssistantSystem != "" {
		r.ha, r.haKnown = l.machine(a.HomeAssistantSystem)
	}
	l.agent[a.ID] = r
	return r
}

// look works out the link and the lock of one socket of st (a source an agent relays).
func (l *plugLooks) look(st model.PowerSourceState, sock model.PowerSocket) (socketLook, bool) {
	if st.Via == nil {
		return socketLook{}, false
	}
	a, ok := l.s.Poller.AgentByID(*st.Via)
	if !ok {
		return socketLook{}, false
	}
	k := socketLook{src: st, sock: sock, agent: a, dev: strings.TrimPrefix(st.ID, a.ID+"/")}
	if to := a.SocketLinks[k.dev+"/"+sock.ID]; to != "" {
		k.link, k.shown = to, l.s.Poller.ShownID(to)
	} else if len(st.Sockets) == 1 && st.SystemID != nil && *st.SystemID != "" {
		// the only socket of a plug: what the plug measures is what hangs on it (the plug's link is already the
		// id of the row that machine is shown as: power.Manager.Overview)
		k.link, k.shown, k.ownLink = *st.SystemID, *st.SystemID, true
	}
	if k.link != "" {
		k.row = l.rows[k.shown]
		k.lock = l.lock(k)
		k.knownOff, k.at, k.fresh = l.s.Poller.KnownOff(k.shown), l.now, time.Duration(l.s.Cfg.OfflineAfter*float64(time.Second))
	}
	if k.lock == "" && a.HomeAssistantSystem == "" {
		// The panel was not told which machine this agent's Home Assistant runs on. Any socket of the agent could
		// be the one that feeds it, a socket that is linked to nothing too (nobody wrote what hangs on it): none
		// is switched until the key is set. What the panel does know about a socket is said first (the locks
		// above).
		k.lock = model.SocketLockHAUnset
	}
	return k, true
}

// lock is why the panel never switches a socket that is linked to a machine ("": it has no reason).
//
//  1. It feeds the machine that reads the plugs: the row of the agent that relays the socket, the Proxmox node
//     that agent runs on, or, for an agent in a guest, that guest's node. With it off nothing reads or switches
//     the plugs any more, this socket included.
//  2. It feeds the machine Home Assistant runs on (home_assistant_system; for a VM, also its node). With it
//     off nothing can switch the socket on again.
//  3. home_assistant_system is set and no row has that id right now (a typing mistake, or the panel has not
//     heard of the row yet): the panel cannot tell which machine that is, so it switches no socket of this
//     agent that is linked to a machine.
//  4. It feeds the machine this panel runs on (panel_hosts, the rule of the power menu).
//
// And one lock that is the agent's whole (look): while its entry has no home_assistant_system, no socket of it
// is switched, linked or not, because 2 cannot be told for any of them.
//
//  5. Its link names no row the panel has right now (a typing mistake in socket_links, or a row the panel has
//     not heard of yet). Which machine hangs on it is then not known: it could be any of the three above, and
//     whether it runs cannot be asked. So it is locked like 3, until the link names a row. (It comes after the
//     list of panel_hosts, which names ids and needs no row.)
func (l *plugLooks) lock(k socketLook) string {
	rows := l.of(k.agent)
	linked := []string{k.link, k.shown}
	linked = append(linked, l.s.Poller.SameMachine(k.link)...)
	hits := func(set map[string]bool) bool {
		return slices.ContainsFunc(linked, func(id string) bool { return set[id] })
	}
	switch {
	case hits(rows.own):
		return model.SocketLockAgent
	case rows.ha != nil && hits(rows.ha):
		return model.SocketLockHomeAssistant
	case rows.ha != nil && !rows.haKnown:
		return model.SocketLockHAUnknown
	case slices.ContainsFunc(linked, func(id string) bool { return slices.Contains(l.s.Cfg.PanelHosts, id) }):
		return model.SocketLockPanel
	case k.row == nil:
		return model.SocketLockLinkUnknown
	case powerctl.KillsPanel(l.s.Poller, *k.row):
		return model.SocketLockPanel
	}
	return ""
}

// socketGate is why no socket of this agent can be switched for a reason of the passkey check, by the panel's
// own record of the machine ("": the check is on and can be used). A socket is switched only with an approval
// the machine checks itself, so a machine that is not on record as one with the check is sent nothing.
func (s *Server) socketGate(agentID string) string {
	if !s.Poller.BindingOf(agentID).OK() {
		return api.WhyApprovalUnavailable // an alarm about which machine it is: no approval is asked for it
	}
	ap := s.Poller.ApprovalOf(agentID)
	switch {
	case ap == nil || ap.State == api.ApprovalOff || ap.State == "":
		return api.WhyApprovalOff
	case ap.State != api.ApprovalRequired:
		return api.WhyApprovalUnavailable
	}
	return ""
}

// socketWhy is why the socket cannot be switched from this panel by an admin right now ("": it can), the
// first reason that applies: the panel's lock; what the agent says (its own config, or that it does not
// answer); that the panel holds no control token for the agent; the passkey check by the panel's record.
func (s *Server) socketWhy(k socketLook) string {
	switch {
	case k.lock != "":
		return k.lock
	case !k.sock.Switchable && k.sock.Why != "":
		return k.sock.Why
	case !k.sock.Switchable:
		return model.SocketNotSwitchable
	case k.sock.Tag == "":
		return model.SocketNoTag
	case !k.agent.HasControl():
		return model.SocketNoControl
	}
	return s.socketGate(k.agent.ID)
}

// sockets is the sockets of a source as an answer of the API carries them: the agent's row with the panel's
// rules laid over it. admin: the viewer may switch (an admin). Everybody else gets the state and the name, no
// tag and no reason. see says whether the viewer may see a row.
func (l *plugLooks) sockets(st model.PowerSourceState, admin bool, see func(id string) bool) []model.PowerSocket {
	if len(st.Sockets) == 0 {
		return nil
	}
	out := make([]model.PowerSocket, 0, len(st.Sockets)) // a new list: the manager's own is never written to
	for _, sock := range st.Sockets {
		k, ok := l.look(st, sock)
		o := model.PowerSocket{ID: sock.ID, Name: sock.Name, State: sock.State, Error: sock.Error}
		if !ok || !admin {
			o.Why = model.SocketReadOnly
			if ok && k.row != nil && see(k.shown) {
				o.SystemID = model.P(k.shown)
			}
			out = append(out, o)
			continue
		}
		if k.row != nil {
			o.SystemID = model.P(k.shown)
		}
		o.Tag, o.Lock, o.Why = sock.Tag, k.lock, l.s.socketWhy(k)
		o.Switchable = o.Why == ""
		if o.Why != "" { // the sentence behind the lock, as the route says it when it refuses
			o.Note = k.whyText(o.Why, l.machineName(k.agent.ID))
		}
		out = append(out, o)
	}
	return out
}

// whyText is the sentence for why this socket cannot be switched (socketWhyText), where it depends on more
// than the reason's word: a link that names no row is put right in another place for the only socket of a plug.
func (k socketLook) whyText(why, machine string) string {
	switch {
	case why == model.SocketLockLinkUnknown && k.ownLink:
		return "Locked. This plug is linked to a machine the panel does not have: correct its link (power_links, or Edit host)."
	case why == model.SocketLockHAUnset && k.agent.Managed:
		// added with Add host: it has no entry in config.yaml, and Edit host has no such field
		return "The panel does not know which machine runs Home Assistant for this plug. A machine added with Add host cannot be given that setting yet."
	}
	return socketWhyText(why, machine)
}

// socketIdleWatts: a fresh reading of a socket's own meter under this is a sign that nothing on the socket
// runs (what a machine that is off still draws); at or over it, that something does.
const socketIdleWatts = 5.0

// running says whether the machine a socket is linked to counts as running, and how the page names it: every
// state but "off" (runningState). A link to an id that no row has counts, under the id as its name (the route
// does not get that far: such a socket is locked).
func (k socketLook) running() (name string, yes bool) {
	switch {
	case k.link == "":
		return "", false
	case k.row == nil:
		return k.link, true
	}
	return k.row.Name, k.runningState() != ""
}

// runningState is what the panel knows about the machine on a socket, for the question "this machine is still
// running": "" (it is off, or the socket is linked to nothing), "running", "asleep" (the panel sent it to
// sleep: it still holds what is open on it), or "unknown" (it may still be running). The page's dialog says
// which in its title, and the words the passkey signs for "Cut power anyway" say it too.
//
// A machine that answers runs: every status but "offline" and "stopped" ("stale" is not off, and neither is
// "paused"), and a machine whose agent is silent while the machine itself still answers (MachineOn).
//
// A machine that does not answer is NOT taken for off because of that, however long it lasts: a link that
// dropped, an agent that was stopped or hangs, and a machine that installs updates while it shuts down all
// look the same from here. It counts as off only with a sign of it:
//
//   - Proxmox says the guest is stopped;
//   - the machine was seen to go off after a shutdown the panel asked for (Poller.MarkOff: a run of "Update
//     unpowered hosts" saw its countdown and then nothing at all), and has given no sign of life since;
//   - the socket has a meter of its own, and a fresh reading of it is under socketIdleWatts (ownWatts).
//
// The socket's own meter also says the opposite: a fresh reading at or over socketIdleWatts means something on
// the socket still draws power, whatever else is known. Everything else is "unknown".
func (k socketLook) runningState() string {
	switch {
	case k.link == "":
		return ""
	case k.row == nil:
		return "unknown"
	case k.row.Asleep:
		return "asleep"
	case (k.row.Status != "offline" && k.row.Status != "stopped") || k.row.MachineOn != "":
		return "running"
	}
	switch w, ok := k.ownWatts(); {
	case ok && w >= socketIdleWatts:
		return "unknown"
	case ok, k.row.Status == "stopped", k.knownOff:
		return ""
	}
	return "unknown"
}

// ownWatts is what the socket's own meter reads right now: the watts of a plug with exactly one socket, when
// the plug is read without an error and the reading is fresh (not older than offline_after at the time of the
// look). ok is false for a socket of a powerbar (its plug measures all its sockets together; no agent reports
// watts per socket), for a reading without watts, for an old one, and for a look without a time.
func (k socketLook) ownWatts() (watts float64, ok bool) {
	if len(k.src.Sockets) != 1 || k.src.Status != "ok" || len(k.src.Readings) != 1 || k.src.Readings[0].Watts == nil || k.at.IsZero() {
		return 0, false
	}
	if age := k.at.Sub(k.src.Readings[0].Timestamp); age > k.fresh || age < -k.fresh {
		return 0, false
	}
	return *k.src.Readings[0].Watts, true
}

// runningWords is the sentence for a socket whose machine still runs, or may (the refusal of "off", and the
// warning of "Cut power anyway"). For a Proxmox node it says how many of its guests run.
func (l *plugLooks) runningWords(k socketLook) string {
	name, _ := k.running()
	switch {
	case k.row == nil:
		return fmt.Sprintf("This socket is linked to %s, and the panel has no such row right now, so it cannot tell whether that machine is off.", name)
	case k.row.Asleep:
		return name + " is asleep, not shut down: what is open on it is lost when its power is cut. Shut it down first."
	case k.runningState() == "unknown":
		// it gives no answer (or Proxmox says it is stopped) and the panel has no sign that it is off
		how := "does not answer"
		if k.row.Status == "stopped" {
			how = "is stopped"
		}
		if w, ok := k.ownWatts(); ok && w >= socketIdleWatts {
			return fmt.Sprintf("%s %s, and its plug still measures %.0f W: it may still be running. Shut it down first, or look at it.", name, how, w)
		}
		return name + " " + how + ", which does not say that it is off: it may still be running. Shut it down first, or look at it."
	case k.row.Kind == "node":
		n := 0
		for i := range l.snap {
			if g := &l.snap[i]; (g.Kind == "qemu" || g.Kind == "lxc") && g.ParentID != nil && *g.ParentID == k.row.ID &&
				model.Deref(g.ClusterID) == model.Deref(k.row.ClusterID) && g.Status != "offline" && g.Status != "stopped" {
				n++
			}
		}
		switch n {
		case 0:
			return name + " is still running. Shut it down first."
		case 1:
			return name + " is still running, with 1 guest running on it. Shut it down first."
		}
		return fmt.Sprintf("%s is still running, with %d guests running on it. Shut it down first.", name, n)
	}
	return name + " is still running. Shut it down first."
}
