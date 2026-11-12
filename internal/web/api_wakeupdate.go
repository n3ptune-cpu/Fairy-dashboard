package web

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/powerctl"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// "Update unpowered hosts" (Updates page; README: "Wake, update, shut down"). The run itself is
// internal/wakerun; this file is its doors and its eyes:
//
//   - GET    /api/admin/wake-update          admins: which machines can take part and why not, the run that is
//     going, the last runs
//   - POST   /api/admin/wake-update          start a run with exactly the machines and options of the body
//   - DELETE /api/admin/wake-update/{id}     cancel the run that is going
//   - PUT    /api/systems/{id}/wake-update   switch a machine on or off for these runs
//
// Who may: admins, the same people who may use the power menu. All three writes need a browser session: an
// API token cannot "confirm it's you", and a run goes on for many minutes after the one request that starts
// it. Starting a run asks "Confirm it's you" every time, also seconds after another confirmation
// (stepUpEach): the dialog is where the list of machines is shown, and the request that is kept, and replayed
// by the panel after the sign-in, is the one with that list and those options. Another list needs another
// confirmation.
//
// A run sends only what the page's buttons send (wake through the relay, the agent's self-update, the delayed
// shutdown, its cancel), through the same functions (powerctl), so the kill switch, the per-host power switch
// and a stopped helper stop a run's request like they stop a click.

func (s *Server) wakeUpdateRoutes() {
	s.mux.HandleFunc("GET /api/admin/wake-update", s.h(s.wakeUpdateGet))
	s.mux.HandleFunc("POST /api/admin/wake-update", s.h(s.wakeUpdateStart))
	s.mux.HandleFunc("DELETE /api/admin/wake-update/{id}", s.h(s.wakeUpdateCancel))
	s.mux.HandleFunc("PUT /api/systems/{id}/wake-update", s.h(s.wakeUpdateSwitch))
}

// wuHost is one machine on the page: whether it is switched on for these runs, and how it could be in a run
// started now.
type wuHost struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	OptedIn bool   `json:"opted_in"`
	// CanJoin: it can be switched on for these runs at all (WhyNot says why not).
	CanJoin bool   `json:"can_join"`
	WhyNot  string `json:"why_not,omitempty"`
	// CanWake: Fairy can wake it and switch it off again (NoWake says why not: then it is only ever updated
	// while it is on).
	CanWake bool   `json:"can_wake"`
	NoWake  string `json:"no_wake,omitempty"`
	Relay   string `json:"relay,omitempty"` // the name of the machine that sends its wake packets
	// Power is what the panel can say now: "on" (its agent answers, or the machine shows as on another way),
	// "off" (no answer for wakerun.MinOff), "unclear" (anything between: it may be restarting). The run decides
	// again by itself, with the same rule; this is only what the page offers.
	Power string `json:"power"`
	// Choice is how it can be in a run right now: "wake" (it is off), "update_only" (it is on), or "" with Why.
	Choice string `json:"choice"`
	Why    string `json:"why,omitempty"`
	// NotSure: it does not answer, and not for long enough to be taken for off. The dialog lists it apart.
	NotSure bool `json:"not_sure,omitempty"`
	// Asleep: the panel itself sent it to sleep, and it has not answered since. For the panel "off" and
	// "asleep" look the same (nothing at all), so the page says "asleep or off" and the dialog lists it apart,
	// unticked: a machine that resumes from sleep does not look freshly started, and a run leaves it on.
	Asleep  bool   `json:"asleep,omitempty"`
	Version string `json:"version,omitempty"`
	Behind  *bool  `json:"behind"`
}

type wakeUpdatePage struct {
	Hosts    []wuHost         `json:"hosts"`
	Settings wakerun.Settings `json:"settings"`
	Latest   string           `json:"latest,omitempty"` // the newest Fairy-agent release the panel knows
	Active   *wakerun.Run     `json:"active"`
	Last     []*wakerun.Run   `json:"last"`
	Now      time.Time        `json:"now"`
	Problem  string           `json:"problem,omitempty"` // why no run can start at all
	// OffAfterS: a machine counts as off after this long without an answer (wakerun.MinOff; not a setting).
	OffAfterS int `json:"off_after_s"`
	MaxHosts  int `json:"max_hosts"`
}

// runOps is the run's view of the panel (wakerun.Ops): the poller, the configuration, and the existing four
// requests.
type runOps struct{ s *Server }

func (o runOps) system(id string) (model.System, bool) {
	for _, x := range o.s.Poller.Snapshot() {
		if x.ID == id {
			return x, x.Kind == "node" || x.Kind == "agent"
		}
	}
	return model.System{}, false
}

func (o runOps) Host(id string) (wakerun.HostInfo, bool) {
	x, ok := o.system(id)
	if !ok {
		return wakerun.HostInfo{}, false
	}
	a, ok := o.s.Poller.AgentFor(id)
	if !ok {
		return wakerun.HostInfo{}, false
	}
	info := wakerun.HostInfo{ID: id, Name: x.Name, AgentID: a.ID, OptedIn: o.s.Poller.Admin.WakeUpdateEnabled(id), Entry: o.s.Poller.AgentEntry(a.ID)}
	if ov := o.s.Poller.Override(id); ov != nil && ov.Wol != nil {
		info.RelayID, info.MACs = ov.Wol.Relay, len(ov.Wol.AllMACs())
	}
	return info, true
}

func (o runOps) Observe(id string) wakerun.Observation {
	a, ok := o.s.Poller.AgentFor(id)
	if !ok {
		return wakerun.Observation{}
	}
	info, sys, known := o.s.Poller.AgentSeen(a.ID)
	obs := wakerun.Observation{Online: info.Online, SeenAt: info.SeenAt, QuietSince: info.QuietSince, Version: info.Version, UpdateEnabled: info.Enabled,
		Paused: o.s.Poller.Scheduler.IsPaused(id) || o.s.Poller.Scheduler.IsPaused(a.ID)}
	if known {
		obs.Status = sys.Status
	}
	obs.Silent = wakerun.Silence{Row: info.Silent.Row, N: info.Silent.N, BeganAgo: info.Silent.BeganAgo, EndedAgo: info.Silent.EndedAgo}
	// On without a usable answer from its agent: the machine itself refused the panel's latest poll, or took
	// part in it in another way (an HTTP status, an answer that is not the agent's, a connection that was made
	// and then gave nothing: poller.AgentInfo.Sign), or it is a Proxmox node that the cluster shows as online.
	if x, ok := o.system(id); info.Refused {
		obs.OnBy = wakerun.OnByRefused
	} else if info.Sign != "" {
		obs.OnBy = info.Sign
	} else if ok && x.Kind == "node" && x.Status == "online" && !info.Online {
		obs.OnBy = "the Proxmox API shows it as online"
	}
	if info.Online {
		obs.UptimeS = sys.UptimeS
		if p := sys.PowerPending; p != nil {
			obs.Pending = &wakerun.Pending{Action: p.Action, At: p.At}
		}
	}
	if su := info.SelfUpdate; su != nil {
		obs.UpdateState, _ = su["state"].(string)
		obs.UpdateMessage, _ = su["message"].(string)
		if last, ok := su["last"].(map[string]any); ok {
			obs.LastAt, _ = last["at"].(float64)
			obs.LastOK, _ = last["ok"].(bool)
			obs.LastMessage, _ = last["message"].(string)
		}
	}
	if n, ok := release.BuildNumber(info.Version); ok {
		if l := o.s.latest(); l.Build != nil {
			b := n < *l.Build
			obs.Behind = &b
		}
	}
	return obs
}

func (o runOps) Probe(ctx context.Context, id string) string {
	a, ok := o.s.Poller.AgentFor(id)
	if !ok {
		return "error"
	}
	if got := o.s.Poller.ProbeAgent(ctx, a.ID); got != poller.ProbeRefused {
		return got
	}
	return wakerun.ProbeRefused
}

func (o runOps) Kick(id string) {
	if a, ok := o.s.Poller.AgentFor(id); ok {
		o.s.Poller.Scheduler.Kick("agent:" + a.ID)
	}
}

// opError turns the error of a control request into the run's: the agent's own code decides whether it is an
// approval the agent asks for (item 1.1) or its rate limit; a request that left the panel and got no usable
// answer is "no answer" (the machine may have taken it); the text is the one the page would show.
func opError(err error) error {
	if err == nil {
		return nil
	}
	out := &wakerun.OpError{Text: "the request failed"}
	var ae *apierr.Error
	if errors.As(err, &ae) {
		if t, ok := ae.Detail.(string); ok && t != "" {
			out.Text = t
		}
	}
	var lost *powerctl.Unanswered
	if errors.As(err, &lost) {
		out.Code = wakerun.CodeNoAnswer
	}
	var ref *powerctl.AgentRefusal
	if errors.As(err, &ref) {
		switch {
		case strings.HasPrefix(ref.Code, "approval_"): // approval_required, approval_refused, approval_unavailable
			out.Code = wakerun.CodeApproval
		case ref.Status == 429:
			out.Code = wakerun.CodeRateLimited
		}
	}
	return out
}

// power sends one of the existing power requests for a run, through the function the power menu uses.
func (o runOps) power(ctx context.Context, id string, req powerctl.Request, by string) (model.PowerResult, error) {
	x, ok := o.system(id)
	if !ok {
		return model.PowerResult{}, &wakerun.OpError{Text: "the machine is no longer in the panel"}
	}
	if err := req.Validate(); err != nil {
		return model.PowerResult{}, opError(err)
	}
	if req.Action == "shutdown" { // the typed confirmation of the power menu: the run was confirmed for the named list
		req.Confirm = &x.Name
	}
	res, err := powerctl.Execute(ctx, o.s.Poller, x, req, by)
	if errors.Is(err, powerctl.ErrOtherRoute) {
		return res, &wakerun.OpError{Code: wakerun.CodeNoAgentRoute, Text: "its own agent can no longer take the request"}
	}
	if err != nil {
		return res, opError(err)
	}
	// like a click: a shutdown from the panel is expected, the machine going offline is "off", not an alarm
	o.s.Watch.NotePanelPower(x.ID, req.Action, time.Now())
	return res, nil
}

func (o runOps) Wake(ctx context.Context, id, by string) error {
	_, err := o.power(ctx, id, powerctl.Request{Action: "wake"}, by)
	return err
}

// Update is the Update button's request. grant is the approval of item 1.1; no agent takes one yet.
func (o runOps) Update(ctx context.Context, id, by string, _ any) error {
	a, ok := o.s.Poller.AgentFor(id)
	if !ok {
		return &wakerun.OpError{Text: "the machine is no longer in the panel"}
	}
	_, err := powerctl.UpdateAgent(ctx, o.s.Poller, a, by)
	return opError(err)
}

// Shutdown is the agent's delayed, cancellable shutdown and nothing else. The route is part of the request
// (OnlyVia): when the machine's agent can no longer take it (its power control was switched off during the
// run) powerctl refuses before anything is sent, and never falls back to the Proxmox API's node shutdown,
// which starts at once and cannot be cancelled. The run then leaves the machine on (CodeNoAgentRoute).
func (o runOps) Shutdown(ctx context.Context, id string, delay time.Duration, by string, _ any) (time.Time, error) {
	d := int(delay.Seconds())
	res, err := o.power(ctx, id, powerctl.Request{Action: "shutdown", DelayS: &d, OnlyVia: "agent"}, by)
	if err != nil {
		return time.Time{}, err
	}
	if res.Via != "agent" { // cannot happen with OnlyVia; kept as the second look
		return time.Time{}, &wakerun.OpError{Code: wakerun.CodeNoAgentRoute, Text: "the shutdown did not go through the machine's own agent"}
	}
	if res.Pending != nil {
		return res.Pending.At, nil
	}
	return time.Time{}, nil
}

func (o runOps) CancelShutdown(ctx context.Context, id, by string) error {
	_, err := o.power(ctx, id, powerctl.Request{Action: "cancel"}, by)
	return err
}

// Guests: what the panel knows from the Proxmox API about the guests of a node. A machine the panel knows
// only through its agent, and whose agent says it is a Proxmox host, has guests the panel cannot see.
func (o runOps) Guests(id string, since time.Time) (int, bool, string) {
	x, ok := o.system(id)
	if !ok {
		return 0, false, "the machine is no longer in the panel"
	}
	if x.Kind != "node" {
		a, _ := o.s.Poller.AgentFor(id)
		_, own, _ := o.s.Poller.AgentSeen(a.ID)
		if strings.Contains(strings.ToLower(model.Deref(own.OS)), "proxmox") {
			return 0, false, "it is a Proxmox host, and the panel has no Proxmox API for it"
		}
		if h, ok := o.s.managedHost(id); ok && h.ProxmoxWanted {
			return 0, false, "it was added as a Proxmox node, and the panel has no Proxmox API for it"
		}
		return 0, true, ""
	}
	src := "proxmox:" + model.Deref(x.ClusterID)
	fresh := false
	for _, st := range o.s.Poller.Summary().Sources {
		if st.Name == src {
			fresh = st.OK && st.LastSuccess != nil && st.LastSuccess.After(since)
		}
	}
	if !fresh {
		return 0, false, "its Proxmox API has not answered since it came online"
	}
	// The panel counts the guests it has. When the API listed more rows than the panel takes from one source
	// (normalise.MaxProxmoxGuests), rows were left out, and a guest that runs on this node may be one of them:
	// that is "cannot see its guests" (second review, N1), not "none runs".
	if o.s.Poller.ProxmoxRowsLeftOut(model.Deref(x.ClusterID)) {
		return 0, false, "its Proxmox API lists more nodes or guests than the panel takes, so the panel does not see all of its guests"
	}
	// A guest keeps its node on unless the panel can say that it is stopped: shown as stopped, and "stopped" in
	// so many words in the Proxmox API's latest answer. Shown as running: it runs. Everything else (paused,
	// suspended, "unknown", a word the panel does not know, a guest whose polling is paused, a guest row without
	// a node) is "the panel cannot say": the node is left on for that too (r2-p2-07).
	var names, unsure []string
	for _, g := range o.s.Poller.Snapshot() {
		if (g.Kind != "qemu" && g.Kind != "lxc") || model.Deref(g.ClusterID) != model.Deref(x.ClusterID) {
			continue
		}
		if g.ParentID != nil && !poller.GuestOf(g, x) { // by the node's id, not by the name shown for it
			continue // a guest of another node
		}
		state, ok := o.s.Poller.GuestState(g.ID)
		switch {
		case g.ParentID == nil: // the cluster did not say which node it is on
			unsure = append(unsure, g.Name+" (no node given)")
		case g.Status == "online":
			names = append(names, g.Name)
		case g.Status == "stopped" && ok && state == "stopped":
		default:
			unsure = append(unsure, g.Name+" ("+guestStateWord(g, state)+")")
		}
	}
	short := func(l []string) string {
		sort.Strings(l)
		if len(l) > 3 {
			l = append(l[:3], "…")
		}
		return strings.Join(l, ", ")
	}
	if n := len(names); n > 0 {
		return n, true, short(names)
	}
	if len(unsure) > 0 {
		return 0, false, "the Proxmox API does not show as stopped: " + short(unsure)
	}
	return 0, true, ""
}

// guestStateWord is why a guest is not taken for stopped, for the report: the Proxmox API's own word when it
// is one of its plain ones.
func guestStateWord(g model.System, state string) string {
	if g.Status == "paused" {
		return "polling paused"
	}
	switch state {
	case "paused", "suspended", "unknown":
		return state
	}
	return "state not known"
}

func (o runOps) Blocked() string {
	if o.s.Poller.Admin.ControlDisabled() != nil {
		return "control is disabled by an administrator"
	}
	return ""
}

// wuHostOf describes one machine for the page and for the checks of a start.
func (s *Server) wuHostOf(x model.System) (wuHost, bool) {
	a, ok := s.Poller.AgentFor(x.ID)
	if !ok || (x.Kind != "node" && x.Kind != "agent") {
		return wuHost{}, false
	}
	ops := runOps{s}
	o := ops.Observe(x.ID)
	h := wuHost{ID: x.ID, Name: x.Name, OptedIn: s.Poller.Admin.WakeUpdateEnabled(x.ID), Version: o.Version, Behind: o.Behind, Power: "unclear"}
	switch {
	case powerctl.KillsPanel(s.Poller, x):
		h.WhyNot = "the panel itself runs on it"
	case !a.SelfUpdate || !a.HasControl():
		h.WhyNot = "its agent cannot be updated from the panel (self_update: true and a control token in the panel's entry for it)"
	default:
		h.CanJoin = true
	}
	ov := s.Poller.Override(x.ID)
	switch {
	case ov == nil || ov.Wol == nil:
		h.NoWake = "no Wake-on-LAN is set up for it"
	case !a.PowerControl:
		h.NoWake = "power control is not enabled for its agent, so Fairy could not switch it off again"
	default:
		h.CanWake = h.CanJoin
		h.Relay = ov.Wol.Relay
		if r := s.Poller.RelaySystem(ov.Wol.Relay); r != nil {
			h.Relay = r.Name
		}
	}
	now := s.Poller.Clock().UTC()
	switch {
	case o.Online || o.OnBy != "":
		h.Power = "on"
	case wakerun.OffNow(o, now):
		h.Power = "off"
		h.Asleep = x.Asleep
	}
	switch {
	case !h.CanJoin:
		h.Why = h.WhyNot
	case !h.OptedIn:
		h.Why = "not switched on for this (on the machine's page: the ⋯ menu)"
	case s.Poller.Admin.PowerDisabled(x.ID):
		h.Why = "power control for this host is disabled by an administrator"
	case o.Paused:
		h.Why = "polling is paused for it"
	case o.Online:
		h.Choice = wakerun.ModeUpdateOnly
	case h.Power == "on":
		h.Why = "it is on (" + o.OnBy + "), and its agent does not answer"
	case h.Power == "unclear":
		h.Why, h.NotSure = notSureWhy(o, now), true
	case !h.CanWake:
		h.Why = "it is off, and Fairy cannot wake it: " + h.NoWake
	default:
		if r := s.Poller.RelaySystem(ov.Wol.Relay); r == nil || r.Status != "online" {
			h.Why = "it is off, and " + h.Relay + ", which sends its wake packets, is offline"
		} else {
			h.Choice = wakerun.ModeWake
		}
	}
	return h, true
}

// notSureWhy says, on the page, why a machine that does not answer is not offered as "off".
func notSureWhy(o wakerun.Observation, now time.Time) string {
	need := spanWords(wakerun.MinOff)
	if !o.QuietSince.IsZero() && !o.Paused {
		q := max(wakerun.QuietFor(o, now), time.Second)
		return fmt.Sprintf("not sure that it is off: no answer for %s so far, and Fairy takes a machine for off only after %s (it may be restarting)", spanWords(q), need)
	}
	return "not sure that it is off: the panel has not watched it long enough; Fairy takes a machine for off only after " + need + " without an answer"
}

// spanWords is a duration for a sentence: "40 seconds", "1 minute", "15 minutes", "3 hours".
func spanWords(d time.Duration) string {
	n, unit := int64(d/time.Second), "second"
	switch {
	case d >= 2*time.Hour:
		n, unit = int64(d/time.Hour), "hour"
	case d >= time.Minute:
		n, unit = int64(d/time.Minute), "minute"
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s", n, unit)
}

func (s *Server) wakeUpdatePageData() wakeUpdatePage {
	p := wakeUpdatePage{Hosts: []wuHost{}, Settings: s.WakeRuns.Settings, Last: []*wakerun.Run{}, Now: time.Now().UTC(), MaxHosts: wakerun.MaxHosts,
		OffAfterS: int(wakerun.MinOff / time.Second)}
	if l := s.latest(); l.Version != "" {
		p.Latest = l.Version
	} else {
		p.Latest = l.Tag
	}
	for _, x := range s.Poller.Snapshot() {
		if h, ok := s.wuHostOf(x); ok {
			p.Hosts = append(p.Hosts, h)
		}
	}
	sort.SliceStable(p.Hosts, func(i, j int) bool { return p.Hosts[i].Name < p.Hosts[j].Name })
	active, hist := s.WakeRuns.Snapshot()
	p.Active = active
	if len(hist) > 5 {
		hist = hist[:5]
	}
	p.Last = append(p.Last, hist...)
	if s.WakeRuns.JournalError() != nil {
		p.Problem = "No run can be started: the run journal (wake-update.json in the panel's data folder) cannot be used. Look at it, then move it away."
	} else if s.WakeRuns.JournalWriteError() != nil {
		p.Problem = "No run can be started: the run journal (wake-update.json in the panel's data folder) cannot be written. Check that the disk has room."
	}
	return p
}

func (s *Server) wakeUpdateGet(c *reqCtx) error {
	if _, err := c.requireAdmin("\"Update unpowered hosts\""); err != nil {
		return err
	}
	c.json(200, s.wakeUpdatePageData())
	return nil
}

// wuStartBody is what a run is started with, and so what "Confirm it's you" confirms.
type wuStartBody struct {
	Hosts      []string `json:"hosts"`        // machines that are off: wake, update, switch off again
	UpdateOnly []string `json:"update_only"`  // machines that are on: update, never switch off
	UpdateIfOn bool     `json:"update_if_on"` // a machine of hosts that turns out to be on is updated, not skipped
}

// wuCheck checks a start request against what the panel knows now and returns the machines by id. Every id
// must be a machine that is switched on for these runs and can do what its list asks of it; whether it is on
// or off this second is the run's business (it looks again before every step).
func (s *Server) wuCheck(b wuStartBody) (map[string]wuHost, error) {
	n := len(b.Hosts) + len(b.UpdateOnly)
	if n == 0 {
		return nil, apierr.New(400, "no machine was chosen")
	}
	if n > wakerun.MaxHosts {
		return nil, apierr.Errf(400, "at most %d machines in one run", wakerun.MaxHosts)
	}
	known := map[string]wuHost{}
	for _, x := range s.Poller.Snapshot() {
		if h, ok := s.wuHostOf(x); ok {
			known[h.ID] = h
		}
	}
	out := map[string]wuHost{}
	for i, id := range slices.Concat(b.Hosts, b.UpdateOnly) {
		h, ok := known[id]
		if _, twice := out[id]; twice || !ok || len(id) > 80 {
			return nil, apierr.New(400, "the list names a machine twice, or one the panel does not know")
		}
		switch {
		case !h.CanJoin:
			return nil, apierr.New(400, h.Name+" cannot take part: "+h.WhyNot)
		case !h.OptedIn:
			return nil, apierr.New(400, h.Name+" is not switched on for \"Update unpowered hosts\"")
		case i < len(b.Hosts) && !h.CanWake:
			return nil, apierr.New(400, h.Name+" cannot be woken and switched off again: "+h.NoWake)
		}
		out[id] = h
	}
	return out, nil
}

// wuDescribe is the panel's own description of a start request for "Confirm it's you" and the audit log: how
// many machines, the value that names the list, and the machines by the names the panel has for them. items
// is one line per machine, saying what happens to it.
func wuDescribe(b wuStartBody, hosts map[string]wuHost) (text string, items []string) {
	names := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			out = append(out, hosts[id].Name)
		}
		return out
	}
	var parts []string
	if len(b.Hosts) > 0 {
		parts = append(parts, "wake, update and switch off again: "+strings.Join(names(b.Hosts), ", "))
		tail := "skipped if it turns out to be on"
		if b.UpdateIfOn {
			tail = "only updated if it turns out to be on"
		}
		for _, n := range names(b.Hosts) {
			items = append(items, n+": wake, update, switch off again ("+tail+")")
		}
	}
	if len(b.UpdateOnly) > 0 {
		parts = append(parts, "update only: "+strings.Join(names(b.UpdateOnly), ", "))
		for _, n := range names(b.UpdateOnly) {
			items = append(items, n+": update only; it is on and is never switched off")
		}
	}
	n := len(b.Hosts) + len(b.UpdateOnly)
	return fmt.Sprintf("%d machine%s, list %s: %s", n, map[bool]string{true: "", false: "s"}[n == 1],
		wakerun.ListHash(b.Hosts, b.UpdateOnly, b.UpdateIfOn), strings.Join(parts, "; ")), items
}

// wakeUpdateKept reads a kept start request the way the handler will.
func (c *reqCtx) wakeUpdateKept() (string, []string) {
	var b wuStartBody
	if !decodeStrictly(c.body, &b) {
		return "", nil
	}
	hosts, err := c.s.wuCheck(b)
	if err != nil {
		return "", nil
	}
	return wuDescribe(b, hosts)
}

// wakeUpdateDetail and wakeUpdateItems describe a kept start request (pending.go: actionDetail, actionList).
func (c *reqCtx) wakeUpdateDetail(*model.User) string {
	text, _ := c.wakeUpdateKept()
	return text
}

func (c *reqCtx) wakeUpdateItems(*model.User) []string {
	_, items := c.wakeUpdateKept()
	return items
}

func (s *Server) wakeUpdateStart(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	var b wuStartBody
	if err := c.decodeBody(&b); err != nil {
		return err
	}
	u, err := c.controlAllowed(true)
	if err != nil {
		return err
	}
	hosts, err := s.wuCheck(b)
	if err != nil {
		return err
	}
	if s.WakeRuns.JournalError() != nil {
		return apierr.New(409, "no run can be started: the run journal (wake-update.json in the panel's data folder) cannot be used")
	}
	if s.WakeRuns.JournalWriteError() != nil { // a run sends nothing it could not write down first
		return apierr.New(409, "no run can be started: the run journal (wake-update.json in the panel's data folder) cannot be written")
	}
	if a, _ := s.WakeRuns.Snapshot(); a != nil { // before a passkey is asked for; Start checks again
		return apierr.Errf(409, "a run started by %s at %s is still going: one run at a time. Wait for it, or cancel it.", a.By, a.StartedAt.Format("15:04 UTC"))
	}
	text, _ := wuDescribe(b, hosts)
	latest := s.latest()
	req := wakerun.StartRequest{Hosts: b.Hosts, UpdateOnly: b.UpdateOnly, UpdateIfOn: b.UpdateIfOn, By: actorOf(u), BySub: u.Sub, Latest: latest.Version}
	params := map[string]any{"hosts": b.Hosts, "update_only": b.UpdateOnly, "update_if_on": b.UpdateIfOn,
		"list": wakerun.ListHash(b.Hosts, b.UpdateOnly, b.UpdateIfOn), "latest": latest.Version}
	// The limit on control requests is looked at before a passkey is asked for (switching six machines on for
	// these runs uses up a minute's worth): a start that would be refused is refused now, not after "Confirm
	// it's you". The audit log gets a line for it.
	limited := func() error {
		c.audit(u, "wakeupdate.start", "run", params, "error 429: too many control requests; nothing was started")
		return apierr.New(429, "too many control requests; slow down")
	}
	if !c.resumed() && !s.control.free(u.Sub) {
		return limited()
	}
	// Always its own confirmation: only the panel's replay of this very request (this list, these options)
	// passes, right after the sign-in that was made for it.
	if err := c.stepUpEach(u, "runs that wake, update and switch off machines"); err != nil {
		return err
	}
	// That replay counts as a control request, but the limit no longer refuses it: it was inside the limit when
	// it was asked for, and it cannot be sent again without another confirmation of its own.
	if !s.control.allow(u.Sub) && !c.resumed() {
		return limited()
	}
	r, err := s.WakeRuns.Start(req)
	if err != nil {
		var se *wakerun.StartError
		status := 400
		if errors.As(err, &se) && se.Busy {
			status = 409
		}
		c.audit(u, "wakeupdate.start", "run", params, fmt.Sprintf("error %d: %s", status, err.Error()))
		return apierr.New(status, err.Error())
	}
	params["run"] = r.ID
	c.audit(u, "wakeupdate.start", r.ID, params, "started: "+text)
	s.Notes.Add(notifications.Event{Kind: "wakeupdate_started", Type: "update", Severity: notifications.Info, Host: wuNoteHost,
		Text: fmt.Sprintf("started by %s: %s", actorOf(u), text)})
	c.json(202, map[string]any{"ok": true, "id": r.ID, "list": r.ListHash, "message": fmt.Sprintf("run started for %d machine%s",
		len(r.Hosts), map[bool]string{true: "", false: "s"}[len(r.Hosts) == 1])})
	return nil
}

// wuNoteHost is what the run's notifications are filed under.
const wuNoteHost = "Update unpowered hosts"

func (s *Server) wakeUpdateCancel(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	var b struct {
		// SwitchOff: the machines the run has already woken are still switched off again. Left out or false:
		// they stay on and are reported.
		SwitchOff bool `json:"switch_off"`
	}
	if len(c.body) > 0 {
		if err := c.decodeBody(&b); err != nil {
			return err
		}
	}
	// The kill switch does not keep an admin from calling a run off (the run stops by itself when it is on), and
	// neither does the limit on control requests: a cancel has a budget of its own (r2-p2-09). An admin who has
	// just switched six machines on for these runs can still call the run off.
	u, err := c.stopGuard(s.stopRL)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	r, err := s.WakeRuns.CancelRun(id, actorOf(u), b.SwitchOff)
	if err != nil {
		return apierr.New(404, err.Error())
	}
	c.audit(u, "wakeupdate.cancel", r.ID, map[string]any{"run": r.ID, "switch_off": r.Cancel.SwitchOff}, "ok")
	msg := "run cancelled: the machines it has woken stay on"
	woke := false
	for _, h := range r.Hosts {
		woke = woke || h.WakeSentAt != nil
	}
	switch {
	case !woke: // it woke none (only machines that are on, or it was still at its start): no word about woken ones
		msg = "run cancelled: nothing else is started"
	case r.Cancel.SwitchOff:
		msg = "run cancelled: the machines it has woken are switched off again"
	}
	c.json(200, map[string]any{"ok": true, "message": msg})
	return nil
}

// wakeUpdateCancelDetail says which of the two cancels a kept request is.
func (c *reqCtx) wakeUpdateCancelDetail(*model.User) string {
	var b struct {
		SwitchOff bool `json:"switch_off"`
	}
	if len(c.body) > 0 && !decodeStrictly(c.body, &b) {
		return ""
	}
	if b.SwitchOff {
		return "the machines it has woken are switched off again"
	}
	return "the machines it has woken stay on"
}

func (s *Server) wakeUpdateSwitch(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	var b struct {
		Enabled *bool `json:"enabled"`
	}
	if err := c.decodeBody(&b); err != nil {
		return err
	}
	if b.Enabled == nil {
		return apierr.New(422, "enabled must be true or false")
	}
	u, err := c.controlGuard(s.control)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	x, err := c.find(id)
	if err != nil {
		return err
	}
	h, ok := s.wuHostOf(x)
	if !ok {
		return apierr.New(409, "this is not a machine with an agent of its own")
	}
	if *b.Enabled && !h.CanJoin {
		c.audit(u, "wakeupdate.switch", id, map[string]any{"enabled": true}, "error 409: "+h.WhyNot)
		return apierr.New(409, x.Name+" cannot take part: "+h.WhyNot)
	}
	if err := s.Poller.Admin.SetWakeUpdate(id, *b.Enabled); err != nil {
		return err
	}
	c.audit(u, "wakeupdate.switch", id, map[string]any{"enabled": *b.Enabled}, "ok")
	h, _ = s.wuHostOf(x)
	msg := x.Name + " is no longer part of \"Update unpowered hosts\""
	if *b.Enabled {
		msg = x.Name + " can now be chosen for \"Update unpowered hosts\""
		if !h.CanWake {
			msg += " (only to be updated while it is on: " + h.NoWake + ")"
		}
	}
	c.json(200, map[string]any{"ok": true, "message": msg, "host": h})
	return nil
}

// wakeUpdateSwitchDetail says which way a kept switch request goes.
func (c *reqCtx) wakeUpdateSwitchDetail(*model.User) string {
	var b struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeStrictly(c.body, &b) || b.Enabled == nil {
		return ""
	}
	if *b.Enabled {
		return "switch it on: a run you start may then wake, update and switch off this machine"
	}
	return "switch it off"
}

// wakeRunEvent writes what a run does to the audit log and makes its notifications. The steps are entered
// under the names the same requests have when an admin clicks them (power.wake, update.agent,
// power.shutdown, power.cancel), with the run's id, so the Power and Updates filters of the Audit page show
// them; the actor is the admin who started the run.
func (s *Server) wakeRunEvent(e wakerun.Event) {
	r := e.Run
	entry := func(action, target string, params map[string]any, outcome string) {
		if params == nil {
			params = map[string]any{}
		}
		params["run"] = r.ID
		sub := r.BySub
		s.writeAudit(audit.Entry{Actor: r.By, Sub: &sub, Action: action, Target: target, Source: "wake-update run", Params: params, Outcome: outcome})
	}
	note := func(sev, kind, sysID, host, text string) {
		if s.Notes != nil {
			s.Notes.Add(notifications.Event{Kind: kind, Type: "update", Severity: sev, SystemID: sysID, Host: host, Text: clipText(text, wuNoteMax)})
		}
	}
	switch e.Kind {
	case wakerun.EvStep:
		action := map[string]string{"wake": "power.wake", "update": "update.agent", "shutdown": "power.shutdown", "cancel": "power.cancel"}[e.Action]
		outcome := e.Outcome
		if outcome != "ok" {
			outcome = "error: " + outcome
		}
		entry(action, e.Host.ID, e.Params, outcome)
	case wakerun.EvHostEnd:
		h := e.Host
		entry("wakeupdate.host", h.ID, map[string]any{"state": h.State, "power": h.On, "update": h.Update, "mode": h.Mode}, h.Text)
	case wakerun.EvLate:
		h := e.Host
		entry("wakeupdate.host", h.ID, map[string]any{"state": h.State, "power": h.On, "late": true}, h.Text)
		note(notifications.Warning, "wakeupdate_late", h.ID, h.Name, "came online after the run had given up on it: it is ON, and Fairy did not touch it")
	case wakerun.EvApproval:
		entry("wakeupdate.approval", r.ID, nil, "needed")
		note(notifications.Warning, "wakeupdate_approval", "", wuNoteHost, "the woken machines wait for your approval of their update and shutdown (Updates page)")
	case wakerun.EvRunEnd, wakerun.EvInterrupted:
		counts := map[string]int{}
		attention := false
		for _, h := range r.Hosts {
			counts[h.State]++
			switch h.State {
			case wakerun.StLeftOn, wakerun.StGaveUp, wakerun.StInterrupted:
				attention = true
			case wakerun.StOff, wakerun.StStayedOn:
				attention = attention || (h.Update != "" && h.Update != wakerun.UpdOK && h.Update != wakerun.UpdCurrent)
			}
		}
		params := map[string]any{"state": r.State, "list": r.ListHash}
		for k, v := range counts {
			params[k] = v
		}
		action, kind, lead := "wakeupdate.end", "wakeupdate_done", "run started by "+r.By+" is over"
		switch {
		case e.Kind == wakerun.EvInterrupted:
			action, kind, attention = "wakeupdate.interrupted", "wakeupdate_interrupted", true
			lead = "the panel restarted during the run " + r.By + " started at " + r.StartedAt.Format("15:04 UTC") + "; it is not continued"
		case r.State == wakerun.RunCancelled && r.Cancel != nil:
			lead = "run started by " + r.By + " was cancelled by " + r.Cancel.By
		case r.State == wakerun.RunStopped:
			lead = "run started by " + r.By + " " + r.Note
			attention = true
		}
		entry(action, r.ID, params, r.State)
		sev := notifications.Info
		if attention {
			sev = notifications.Warning
		}
		note(sev, kind, "", wuNoteHost, wuNoteText(lead, r.Hosts, wuNoteMax))
	}
}

// wuNoteMax is how long a run's notification may be.
const wuNoteMax = 1500

// wuNeedsALook reports whether a machine's result is one somebody should look at, and the two words for it:
// left on, given up on, interrupted, its power not known, or its update did not work.
func wuNeedsALook(h *wakerun.Host) (string, bool) {
	switch {
	case h.State == wakerun.StLeftOn && h.On == wakerun.OnUnknown:
		return "not confirmed", true
	case h.State == wakerun.StLeftOn:
		return "left ON", true
	case h.State == wakerun.StGaveUp:
		return "gave up", true
	case h.State == wakerun.StInterrupted:
		return "interrupted", true
	case h.On == wakerun.OnUnknown:
		return "on or off: not known", true
	case h.Update != "" && h.Update != wakerun.UpdOK && h.Update != wakerun.UpdCurrent:
		return "not updated", true
	}
	return "", false
}

// wuNoteText is the one notification of a run that is over (r2-p2-06). A run of sixteen machines does not fit
// a notification with a sentence for each, and the text used to be cut where it ran out: the machines at the
// end of the list were not named, also when they were the ones left ON. Now:
//
//   - the machines that need a look come first, each with its whole sentence, in the run's order;
//   - when everything fits, the others follow, each with its sentence (a line per machine, as before);
//   - when it does not, the others are counted ("9 skipped, 2 switched off again"), and of the machines that
//     need a look as many as fit keep their sentence; the ones after them are named with two words ("H14
//     (left ON)"). Every machine that needs a look is named, always;
//   - no sentence is ever cut in the middle. The whole list, with every sentence, stays on the Updates page
//     under "Last run" (and in the audit log).
func wuNoteText(lead string, hosts []*wakerun.Host, limit int) string {
	type look struct{ full, short string }
	var looks []look
	var others []string
	counts, order := map[string]int{}, []string{}
	for _, h := range hosts {
		if word, yes := wuNeedsALook(h); yes {
			looks = append(looks, look{h.Name + ": " + h.Text, h.Name + " (" + word + ")"})
			continue
		}
		others = append(others, h.Name+": "+h.Text)
		word := map[string]string{wakerun.StOff: "switched off again", wakerun.StStayedOn: "stay on", wakerun.StSkipped: "skipped"}[h.State]
		if word == "" {
			word = h.State
		}
		if counts[word] == 0 {
			order = append(order, word)
		}
		counts[word]++
	}
	// 1. a sentence for every machine
	all := make([]string, 0, len(hosts))
	for _, l := range looks {
		all = append(all, l.full)
	}
	if out := lead + ". " + strings.Join(append(all, others...), " | "); len(out) <= limit {
		return out
	}
	// 2. the others counted; as many whole sentences as fit, from the top; the machines after them by two words
	var rest []string
	for _, word := range order {
		rest = append(rest, fmt.Sprintf("%d %s", counts[word], word))
	}
	const where = " Every machine's result: Updates page, Last run."
	build := func(full int) string {
		parts := []string{}
		for _, l := range looks[:full] {
			parts = append(parts, l.full)
		}
		if full < len(looks) {
			var names []string
			for _, l := range looks[full:] {
				names = append(names, l.short)
			}
			parts = append(parts, map[bool]string{true: "Need a look: ", false: "Also: "}[full == 0]+strings.Join(names, ", ")+".")
		}
		if len(rest) > 0 {
			parts = append(parts, map[bool]string{true: "The others: ", false: ""}[len(looks) > 0]+strings.Join(rest, ", ")+".")
		}
		return lead + ". " + strings.Join(parts, " | ") + where
	}
	for full := len(looks); full >= 0; full-- {
		if out := build(full); len(out) <= limit {
			return out
		}
	}
	// 3. names so long that even two words each do not fit: the count, whole
	return clipText(lead, max(limit-120, 0)) + fmt.Sprintf(". %d of %d machines need a look. Every machine's result: Updates page, Last run.", len(looks), len(hosts))
}

// clipText cuts a text for a notification at a rune boundary.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
