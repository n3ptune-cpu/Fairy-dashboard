// Package powerctl decides which power actions a system offers (and why not) and carries them out.
//
// Routes, in order of preference:
//   - hosts / Proxmox nodes with an agent that has power_control enabled: shutdown / reboot (delayed, cancellable),
//     and for hosts (not nodes) whose agent reports it can, sleep
//   - Proxmox nodes without one, when the cluster entry has `node_power: true`: POST /nodes/{node}/status
//   - Proxmox guests, when the cluster entry has a power token: start / shutdown (ACPI) / reboot / stop (hard)
//   - offline hosts with `wol:` configured: magic packet sent by a relay agent on the same LAN
//   - offline hosts with `bmc_power:` configured: power on through their BMC, by a relay agent that can reach it
//
// Every write is control-group + CSRF + step-up + rate-limited + audited in the web package; typed confirmation
// is enforced here.
package powerctl

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// Actions are the power actions the API accepts.
var Actions = []string{"start", "shutdown", "reboot", "sleep", "stop", "cancel", "wake", "bmc_on"}

// Request is the POST /api/systems/{id}/power body.
type Request struct {
	Action  string  `json:"action"`
	DelayS  *int    `json:"delay_s"` // agent shutdown/reboot only; default 60
	Confirm *string `json:"confirm"` // the system's name, typed (shutdown / stop)
	// OnlyVia, when set, is the one route the action may take ("agent"): Execute refuses with ErrOtherRoute,
	// before anything is sent, when the route it would take now is another one. It is never read from a request
	// body. A wake / update / shut down run sets it for its shutdown: that one must be the agent's delayed,
	// cancellable shutdown, never the Proxmox API's, which starts at once.
	OnlyVia string `json:"-"`
	// PendingUnseen, for a cancel: send it to the system's own agent also when the panel's view shows nothing
	// pending. It is never read from a request body. A wake / update / shut down run sets it for the cancel of
	// a shutdown it asked for itself: when that request got no answer it may have arrived all the same, and
	// the panel's view shows the countdown only after a poll that was answered (and drops it again after one
	// that was not, once the agent's time for it has passed by the panel's clock). The agent answers a cancel
	// with "ok" when nothing is pending, so the request is safe either way (second review of fix7a, R7A-09).
	PendingUnseen bool `json:"-"`
}

// ErrOtherRoute is Execute's refusal of a request whose OnlyVia is not the route the action would take now.
var ErrOtherRoute = apierr.New(409, "the action would not go the way it was asked to go; nothing was sent")

// Validate checks the request and fills in the default delay (60 s).
func (r *Request) Validate() error {
	if !slices.Contains(Actions, r.Action) {
		return apierr.New(422, "action must be one of start, shutdown, reboot, sleep, stop, cancel, wake, bmc_on")
	}
	if r.DelayS == nil {
		d := 60
		r.DelayS = &d
	}
	if *r.DelayS < 10 || *r.DelayS > 86400 {
		return apierr.New(422, "delay_s must be between 10 and 86400")
	}
	if r.Confirm != nil && len(*r.Confirm) > 200 {
		return apierr.New(422, "confirm is too long")
	}
	return nil
}

// nonEmpty returns &s, or nil for "" (JSON null).
func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// mkcap builds one action entry: the route (via) when available, else the reason why not.
func mkcap(action string, ok bool, via, reason, confirm string, delayed bool) model.PowerActionCap {
	c := model.PowerActionCap{Action: action, Available: ok, Confirm: confirm, Delayed: delayed}
	if ok {
		c.Via = nonEmpty(via)
	} else {
		c.Reason = nonEmpty(reason)
	}
	return c
}

// orElse returns a, or b when a is empty.
func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// KillsPanel reports whether the panel itself runs on this system (or on a guest of this node).
func KillsPanel(p *poller.Poller, s model.System) bool {
	if slices.Contains(p.Cfg.PanelHosts, s.ID) {
		return true
	}
	if s.Kind == "node" {
		for _, g := range p.Snapshot() {
			if slices.Contains(p.Cfg.PanelHosts, g.ID) && poller.GuestOf(g, s) {
				return true
			}
		}
	}
	return false
}

// sameString reports whether two optional strings are both nil or equal.
func sameString(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// HelperStoppedWhy is why a Windows host's power actions are unavailable while its elevated helper is stopped.
const HelperStoppedWhy = "the elevated helper is stopped: start the helper first (Elevated helper, next to the host name)"

// HelperAnswerWhy is what the panel says to an agent's 503 with the code helper_stopped or helper_error: the
// request was not carried out because of the helper. It used to be HelperStoppedWhy for both, "the elevated
// helper is stopped: start the helper first", which is not true for helper_error (the helper runs and did
// not answer), and not always for helper_stopped: the agent answers a Cancel with it whenever it cannot tell
// what is pending, also in the seconds after a request its running helper did not answer (review of the
// agent's fix batch, RF7-05). The hint to start a helper that is stopped stays.
func HelperAnswerWhy(code string) string {
	if code == api.ReasonHelperError {
		return "the elevated helper runs but did not answer its agent: try again in a moment (Elevated helper, next to the host name, shows how it is)"
	}
	return "the elevated helper is stopped, or did not answer its agent just now: if it is stopped, start the helper first " +
		"(Elevated helper, next to the host name); if it runs, try again in a moment"
}

// Capabilities lists every action with availability, reason, confirmation and route.
//
// The kill switch and the per-host power switch take every action away except one: "cancel", the calling off
// of a pending shutdown, reboot or sleep countdown. A cancel only ever stops a power action, and the switches
// are what an admin reaches for to stop things: with the cancel blocked too, a countdown that was running when
// the switch was flipped could not be called off from the panel any more, and the machine went off (audit 5,
// r3-p2-06). Nothing else passes them.
func Capabilities(p *poller.Poller, s model.System) model.PowerCaps {
	caps := capabilities(p, s)
	blocked := ControlOff(p, s.ID)
	if blocked != "" {
		for i, a := range caps.Actions {
			if a.Action == "cancel" {
				continue
			}
			if a.Available {
				caps.Actions[i].Available, caps.Actions[i].Reason, caps.Actions[i].Via = false, &blocked, nil
			}
		}
	}
	return caps
}

// ControlOff is why an administrator's switch takes the power actions of a system away right now ("": none
// does): the kill switch, or the power switch of this host. The cancel of a countdown is exempt (Capabilities).
func ControlOff(p *poller.Poller, id string) string {
	if p.Admin.ControlDisabled() != nil {
		return "control is disabled by an administrator (fairy admin control enable)"
	}
	if p.Admin.PowerDisabled(id) {
		return fmt.Sprintf("power control for this host is disabled by an administrator (fairy admin host %s power-control enable)", id)
	}
	return ""
}

func capabilities(p *poller.Poller, s model.System) model.PowerCaps {
	kill := KillsPanel(p, s)
	pve, hasPVE := p.ClusterCfg(model.Deref(s.ClusterID))
	typedIfKill := "click" // actions that take the panel down need the name typed, not just a click
	if kill {
		typedIfKill = "typed"
	}
	var caps []model.PowerActionCap
	if s.Kind == "qemu" || s.Kind == "lxc" {
		caps = guestCaps(s, pve, hasPVE, typedIfKill)
	} else {
		caps = hostCaps(p, s, pve, hasPVE, typedIfKill)
	}
	pc := model.PowerCaps{SystemID: s.ID, Name: s.Name, Actions: caps, Pending: s.PowerPending, KillsPanel: kill}
	if kill {
		pc.Warning = model.P("The panel itself runs here: shutting this down takes Fairy offline until it is powered on again by other means.")
	}
	return pc
}

// isUp: a paused system is still running (only its polling is paused).
func isUp(s model.System) bool { return s.Status == "online" || s.Status == "paused" }

// guestCaps: Proxmox guests are started and stopped through the API, with the cluster's power token.
func guestCaps(s model.System, pve config.Proxmox, hasPVE bool, typedIfKill string) []model.PowerActionCap {
	up := isUp(s)
	why := ""
	if !hasPVE || pve.PowerTokenID == "" {
		name := "this cluster"
		if hasPVE {
			name = pve.ClusterName
		}
		why = "no power token for " + name + " (see README: FairyPower role)"
	}
	running := ""
	if !up {
		running = "guest is not running"
	}
	return []model.PowerActionCap{
		mkcap("start", why == "" && s.Status == "stopped", "proxmox-api", orElse(why, "guest is not stopped"), typedIfKill, false),
		mkcap("shutdown", why == "" && up, "proxmox-api", orElse(why, running), "typed", false),
		mkcap("reboot", why == "" && up, "proxmox-api", orElse(why, running), typedIfKill, false),
		mkcap("stop", why == "" && up, "proxmox-api", orElse(why, running), "typed", false)}
}

// hostCaps: hosts and nodes shut down / reboot through their own agent (delayed, cancellable) or, for nodes,
// the Proxmox API when node_power is on; offline hosts can be woken over a relay agent.
func hostCaps(p *poller.Poller, s model.System, pve config.Proxmox, hasPVE bool, typedIfKill string) []model.PowerActionCap {
	up := isUp(s)
	agent, hasAgent := p.AgentFor(s.ID)
	via, why := "", ""
	switch {
	case hasAgent && agent.PowerControl:
		via = "agent"
	case s.Kind == "node" && hasPVE && pve.NodePower:
		via = "proxmox-api"
	case hasAgent:
		why = "power control is not enabled on " + agent.DisplayName() + "'s agent (owner opts in: power_control: " +
			"enabled: true in the agent's config.yaml, and power_control: true in the panel's agents: entry)"
	case s.Kind == "node":
		why = "no agent with power_control enabled on this node and node_power is off for its cluster"
	default:
		why = "power control is not enabled for this host"
	}
	cancelBase := why                                // nothing pending: no need to blame the helper
	if via == "agent" && s.PowerHelperReason != "" { // the agent shuts down / reboots / sleeps through its helper
		why = HelperStoppedWhy
		if s.PowerHelperReason == api.ReasonHelperError {
			why = "the agent's elevated helper is not answering: power actions need it (see the Elevated helper menu)"
		}
	}
	if s.PowerPending != nil {
		cancelBase = why
	}
	offline := ""
	if !up {
		offline = "host is not online"
	}
	delayed := via == "agent"
	cancelWhy := "only agent-scheduled shutdowns can be cancelled"
	if via == "agent" {
		cancelWhy = "nothing pending"
	}
	caps := []model.PowerActionCap{
		mkcap("shutdown", why == "" && up, via, orElse(why, offline), "typed", delayed),
		mkcap("reboot", why == "" && up, via, orElse(why, offline), typedIfKill, delayed)}
	if s.Kind != "node" { // a hypervisor is not put to sleep from the panel
		caps = append(caps, sleepCap(s, via, why, offline))
	}
	caps = append(caps,
		mkcap("cancel", via == "agent" && s.PowerPending != nil && why == "", via, orElse(cancelBase, cancelWhy), "none", false),
		wakeCap(p, s))
	if o := p.Override(s.ID); o != nil && o.BMCPower != nil {
		caps = append(caps, bmcCap(p, s, *o.BMCPower))
	}
	return caps
}

// bmcCap: "Power on (via BMC, through <relay>)" while the host is off and the relay agent is online and reports
// the target.
func bmcCap(p *poller.Poller, s model.System, b config.BMCPower) model.PowerActionCap {
	via := "bmc:" + b.Relay
	relay := p.RelaySystem(b.Relay)
	label := b.Relay
	if relay != nil {
		label = relay.Name
	}
	c := func(ok bool, why string) model.PowerActionCap {
		x := mkcap("bmc_on", ok, via, why, "typed", false)
		x.ViaLabel = model.P(label)
		return x
	}
	switch {
	case s.Status != "offline" && s.Status != "stale":
		return c(false, "host is already on")
	case relay == nil || relay.Status != "online":
		return c(false, "relay "+label+" is offline")
	case relay.BMCRelayTargets == nil:
		return c(false, label+"'s agent has no bmc_relay (needs Fairy-agent v0.4.2 with bmc_relay enabled)")
	case !slices.Contains(relay.BMCRelayTargets, b.Target):
		return c(false, label+"'s agent has no bmc_relay target "+b.Target)
	}
	return c(true, "")
}

// sleepCap: through the host's own agent only, when it reports that the machine can sleep (Fairy-agent v0.4.2+).
func sleepCap(s model.System, via, why, offline string) model.PowerActionCap {
	switch {
	case why != "":
	case via != "agent":
		why = "sleep goes through the host's own agent"
	case s.Sleep == nil && offline == "":
		why = "the agent does not report sleep (needs Fairy-agent v0.4.2 or newer)"
	case s.Sleep != nil && !*s.Sleep:
		why = "this machine cannot sleep"
		if n := model.Deref(s.SleepNote); n != "" {
			why += ": " + n
		}
	}
	return mkcap("sleep", why == "" && offline == "", via, orElse(why, offline), "typed", true)
}

func wakeCap(p *poller.Poller, s model.System) model.PowerActionCap {
	o := p.Override(s.ID)
	if o == nil || o.Wol == nil {
		return mkcap("wake", false, "", "no wol: configured for this host (needs a relay agent on its LAN)", "none", false)
	}
	relay := p.RelaySystem(o.Wol.Relay)
	via := "wol:" + o.Wol.Relay
	if s.Status != "offline" && s.Status != "stale" {
		c := mkcap("wake", false, via, "host is already on", "none", false)
		return c
	}
	if relay == nil || relay.Status != "online" {
		return mkcap("wake", false, via, "relay "+o.Wol.Relay+" is offline", "none", false)
	}
	c := mkcap("wake", true, via, "", "none", false)
	c.ViaLabel = model.P(relay.Name)
	return c
}

// CheckConfirmation enforces typed confirmation: the system's name must be typed (case-insensitive).
func CheckConfirmation(s model.System, c model.PowerActionCap, confirm *string) error {
	if c.Confirm != "typed" {
		return nil
	}
	got := ""
	if confirm != nil {
		got = *confirm
	}
	if !strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(s.Name)) {
		return apierr.Errf(400, "type the system name (%s) to confirm %s", s.Name, c.Action)
	}
	return nil
}

// Execute carries out one action after re-checking that it is available and, for typed actions, that the
// system's name was typed. The route (agent, Proxmox API or Wake-on-LAN relay) comes from Capabilities.
func Execute(ctx context.Context, p *poller.Poller, s model.System, req Request, requestedBy string) (model.PowerResult, error) {
	if req.Action == "cancel" && req.PendingUnseen && s.PowerPending == nil {
		// Only for the look at the routes below (s is this call's own copy): a cancel is on offer when something
		// is pending, and the caller knows of a request of its own that may be. Everything else that decides
		// about the cancel stays: it goes to the system's own agent with power control, or nowhere.
		s.PowerPending = &model.PowerPending{Action: "shutdown"}
	}
	caps := Capabilities(p, s)
	var c *model.PowerActionCap
	for i := range caps.Actions {
		if caps.Actions[i].Action == req.Action {
			c = &caps.Actions[i]
		}
	}
	if c == nil {
		return model.PowerResult{}, apierr.Errf(400, "%s does not apply to a %s", req.Action, s.Kind)
	}
	if !c.Available {
		r := req.Action + " is not available"
		if c.Reason != nil {
			r = *c.Reason
		}
		return model.PowerResult{}, apierr.New(409, r)
	}
	if err := CheckConfirmation(s, *c, req.Confirm); err != nil {
		return model.PowerResult{}, err
	}
	if req.OnlyVia != "" && model.Deref(c.Via) != req.OnlyVia { // the same look at the routes that picks the one below
		return model.PowerResult{}, ErrOtherRoute
	}
	res := model.PowerResult{SystemID: s.ID, Action: req.Action, Via: model.Deref(c.Via)}
	switch {
	case req.Action == "wake":
		return executeWake(ctx, p, s, res)
	case req.Action == "bmc_on":
		return executeBMC(ctx, p, s, requestedBy, res)
	case res.Via == "agent":
		return executeViaAgent(ctx, p, s, req, requestedBy, res)
	}
	return executeViaProxmox(ctx, p, s, req, res)
}

// executeWake asks the relay agent named in the system's `wol:` entry to send the magic packet.
func executeWake(ctx context.Context, p *poller.Poller, s model.System, res model.PowerResult) (model.PowerResult, error) {
	o := p.Override(s.ID)
	relay, ok := p.AgentByID(o.Wol.Relay)
	if !ok {
		return res, apierr.New(409, "relay agent not configured")
	}
	port := o.Wol.Port
	if port == 0 {
		port = 9
	}
	macs := o.Wol.AllMACs()
	var sent int
	var firstErr error
	for _, mac := range macs { // every network card of the machine (wol: macs), one packet each
		body := map[string]any{"mac": mac, "broadcast": nil, "port": port}
		if o.Wol.Broadcast != "" {
			body["broadcast"] = o.Wol.Broadcast
		}
		if o.Wol.PasswordEnv != "" {
			if pw := os.Getenv(o.Wol.PasswordEnv); pw != "" {
				body["password"] = pw
			}
		}
		if _, err := agentCall(ctx, p, relay, "/wol", body); err != nil {
			var lost *Unanswered // a packet that may have gone out comes before a plain refusal
			if firstErr == nil || (errors.As(err, &lost) && !errors.As(firstErr, &lost)) {
				firstErr = err
			}
			continue
		}
		sent++
	}
	if sent == 0 {
		return res, firstErr
	}
	res.Message = "magic packet sent via " + relay.DisplayName() + "; give it a minute to boot"
	if len(macs) > 1 {
		res.Message = fmt.Sprintf("magic packets sent to %d of %d network cards via %s; give it a minute to boot", sent, len(macs), relay.DisplayName())
	}
	return res, nil
}

// executeBMC asks the relay agent named in the system's `bmc_power:` entry to power the machine on through its
// BMC.
func executeBMC(ctx context.Context, p *poller.Poller, s model.System, requestedBy string, res model.PowerResult) (model.PowerResult, error) {
	o := p.Override(s.ID)
	relay, ok := p.AgentByID(o.BMCPower.Relay)
	if !ok {
		return res, apierr.New(409, "relay agent not configured")
	}
	if _, err := agentCall(ctx, p, relay, "/bmc/power", map[string]any{"target": o.BMCPower.Target, "action": "on",
		"requested_by": requestedBy}); err != nil {
		return res, err
	}
	res.Message = "power on sent to the BMC via " + relay.DisplayName() + "; give it a minute or two to boot"
	return res, nil
}

// executeViaAgent schedules (or cancels) a delayed shutdown / reboot on the host's own agent and shows the
// pending action at once, before the agent's next report.
func executeViaAgent(ctx context.Context, p *poller.Poller, s model.System, req Request, requestedBy string,
	res model.PowerResult) (model.PowerResult, error) {
	agent, _ := p.AgentFor(s.ID)
	data, err := agentCall(ctx, p, agent, "/power", map[string]any{"action": req.Action, "delay_s": *req.DelayS,
		"requested_by": requestedBy})
	if err != nil {
		return res, err
	}
	pending := normalise.Pending(data) // the same checks as for the block in the agent's report
	if req.Action == "shutdown" && (pending == nil || pending.Action != "shutdown") {
		// The agent's "yes" to a shutdown is a good status WITH the countdown it started: it always answers
		// {"action":"shutdown","pending":{...}}. A good status with any other body is not the agent speaking
		// (second review of fix7a, R7A-08: every non-empty JSON object counted as "accepted"). Like every answer
		// that is not the agent's, it is "no answer": the request may have arrived all the same, and a caller
		// that goes on (the run) watches the machine and never takes this for a countdown it knows of.
		return res, &Unanswered{Err: apierr.New(502, "agent unreachable: its answer to the shutdown has no countdown in it, so it is not an answer of the agent")}
	}
	p.NotePending(s.ID, pending)
	res.Pending = pending
	if req.Action == "cancel" {
		res.Message = "cancelled"
	} else {
		d := *req.DelayS
		if pending != nil {
			d = pending.DelayS
		}
		res.Message = fmt.Sprintf("%s in %d s", req.Action, d)
	}
	return res, nil
}

// executeViaProxmox posts the action to the Proxmox API with the cluster's separate power token: node
// shutdown / reboot, or guest start / shutdown / reboot / stop.
func executeViaProxmox(ctx context.Context, p *poller.Poller, s model.System, req Request, res model.PowerResult) (model.PowerResult, error) {
	cluster, node, kind, vmid, ok := p.PVERef(s.ID)
	if !ok {
		return res, apierr.New(409, "not seen in the Proxmox API yet")
	}
	var err error
	if kind == "node" {
		_, err = p.PVEPower(ctx, cluster, "/nodes/"+node+"/status", url.Values{"command": {req.Action}})
	} else {
		_, err = p.PVEPower(ctx, cluster, fmt.Sprintf("/nodes/%s/%s/%d/status/%s", node, kind, *vmid, req.Action), url.Values{})
	}
	if err != nil {
		return res, err
	}
	res.Message = fmt.Sprintf("%s sent to Proxmox (task queued on %s)", req.Action, node)
	return res, nil
}

// maxAgentError is how much of an agent's own error text reaches the page, the audit log and a notification.
const maxAgentError = 300

// AgentRefusal is an agent's own "no" to a control request, with the code it gave ("approval_required",
// "helper_stopped", ...; "" when it gave none) and its HTTP status. It wraps the error the page gets, so
// everything that handles an *apierr.Error handles it unchanged; a caller that must tell the refusals apart
// (the wake / update / shut down run) reads the code.
type AgentRefusal struct {
	Err    *apierr.Error
	Code   string
	Status int // the agent's status (429: its control rate limit)
}

func (e *AgentRefusal) Error() string { return e.Err.Error() }
func (e *AgentRefusal) Unwrap() error { return e.Err }

// Unanswered is a control request that left the panel, or may have, and got no answer of the agent: a timeout,
// a connection that broke, an answer that cannot be read, and every answer that is not the agent's own: an
// error status with a body that is not the agent's (a proxy's 502, 503 or 504 page, another server's 404), a
// redirect, a good status without the agent's answer in it. The agent may have taken the request: the wake
// packet may be out, the update or the countdown may be running. It wraps the error the page gets ("agent
// unreachable: timeout"), so a click shows what it showed before; a caller that goes on working after the
// request (the wake / update / shut down run) must not take it for "nothing happened", and never sends the
// request again because of it. A request that certainly never left (poller.NotSent: no connection could be
// had) is not one of these, and neither is the agent's own "no" (AgentRefusal).
//
// Short, when set, is the reason alone, for a sentence that says "got no answer (...): it may have arrived"
// itself (a run's history).
type Unanswered struct {
	Err   *apierr.Error
	Short string
}

func (e *Unanswered) Error() string { return e.Err.Error() }
func (e *Unanswered) Unwrap() error { return e.Err }

// agentsNo reports whether the body of an answer with an error status is the agent's own: the agent answers
// every refusal with a JSON object that has its reason in "error" (and often a "code"). A proxy's error page,
// an empty body, another server's JSON are not that. (With the statuses 502 and 504 not even such a body is
// taken for the agent's: agentCall.)
func agentsNo(data jsonx.M) bool {
	return jsonx.S(data, "error") != "" || agentCode(data) != ""
}

// relaysOwn reports whether an answer with the status 502 or 504 is the agent's own all the same: on the one
// route where the agent answers with them itself, its BMC relay (POST /bmc/power: "the BMC at ... did not
// answer within 15 s", "the BMC refused the login"), and with one of the relay's own codes (bmc_unreachable,
// bmc_refused, bmc_error, ...). A gateway's JSON has no such code. The words of that answer are what the
// person who clicked "Power on (via BMC)" needs; a run never uses this route.
//
// The fan routes are the other place: the agent answers a fan change its BMC did not take with 502 and the
// BMC's reason ("BMC refused the fan mode: BMC completion code 0xC3 (timeout)", "cannot read the fan mode:
// ..."), without a code. Those words are the agent's and they are what the person at the fan page needs, so a
// 502 on a fan route that begins with them stays the agent's answer. Its 504 there is not: that is "the helper
// was sent the change and did not answer", which is no answer.
func relaysOwn(path string, status int, data jsonx.M) bool {
	if path == "/fan/mode" || path == "/fan/duty" {
		said := jsonx.S(data, "error")
		return status == 502 && (strings.HasPrefix(said, "BMC refused the ") || strings.HasPrefix(said, "cannot read the fan mode: "))
	}
	return path == "/bmc/power" && strings.HasPrefix(agentCode(data), "bmc_")
}

// agentCode is an agent's error code as far as the panel repeats it: a short word of a-z, 0-9 and "_".
func agentCode(data jsonx.M) string {
	c := jsonx.S(data, "code")
	if c == "" || len(c) > 40 {
		return ""
	}
	for _, r := range c {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '_' {
			return ""
		}
	}
	return c
}

// UpdateAgent asks one agent to update itself (POST /update with the control token), for a caller without a
// request of its own. Like every control action it is refused while control is disabled.
func UpdateAgent(ctx context.Context, p *poller.Poller, a config.Agent, requestedBy string) (jsonx.M, error) {
	if p.Admin.ControlDisabled() != nil {
		return nil, apierr.New(409, "control is disabled by an administrator (fairy admin control enable)")
	}
	if !a.SelfUpdate {
		return nil, apierr.New(409, "self-update is not configured for this agent (self_update: true in its agents: entry)")
	}
	return agentCall(ctx, p, a, "/update", map[string]any{"requested_by": requestedBy})
}

// NoAnswer is the first half of agentCall, for the panel's other requests that change something on a machine
// (the fan routes, the single "Update" and "Update all", the helper's stop and start: internal/web). Given what
// came of a POST to an agent (AgentRequest's three results) it returns nil when that is the agent's own answer,
// its "yes" or its "no"; the plain error when the request never left the panel; and an Unanswered in every
// other case. There is one such rule in the panel, and this is it.
func NoAnswer(path string, status int, data jsonx.M, err error) error {
	// The words are true for whoever sent the status: a proxy between panel and agent, and the agent itself,
	// whose own 504 says that its helper was handed the request and did not answer (they were "agent
	// unreachable: http 504, and not an answer of the agent": review of the agent's fix batch, RF7-08).
	notTheAgents := func() error {
		return &Unanswered{Err: apierr.New(502, fmt.Sprintf("no answer (http %d): it may have arrived", status)),
			Short: fmt.Sprintf("http %d in place of an answer", status)}
	}
	if err != nil {
		e := apierr.New(502, "agent unreachable: "+devices.Describe(err))
		var ns *poller.NotSent
		if errors.As(err, &ns) {
			return e
		}
		if status >= 300 { // an error status or a redirect whose body cannot be read
			return notTheAgents()
		}
		return &Unanswered{Err: e}
	}
	if status < 200 || (status >= 300 && status < 400) || (status >= 400 && !agentsNo(data)) || (status < 300 && len(data) == 0) {
		return notTheAgents()
	}
	if (status == 502 || status == 504) && !relaysOwn(path, status, data) {
		return notTheAgents()
	}
	return nil
}

// agentCall sends one control request to an agent and says what came of it, in one of four ways:
//
//   - the agent's "yes": a 2xx status with the agent's own answer (a JSON object that is not empty). The data.
//   - the agent's "no": a status of 400 or more with the agent's own error body (agentsNo). An AgentRefusal
//     (for 401 the plain error about the control token): nothing was done, and the agent said so itself.
//   - the request never left the panel (poller.NotSent): the plain error.
//   - everything else is "no answer of the agent" (Unanswered): a timeout or a broken connection, and every
//     answer that is not the agent's. A proxy between panel and agent answers 504 when it passed the request on
//     and the agent's answer did not come in its time, and 502 when the connection to the agent broke after the
//     request: in both the agent may have taken the request (audit 5, r3-p2-03: "Left ON: the shutdown was not
//     accepted" for a machine that then went off; "It was not woken" for one that was). An error status with
//     any other body than the agent's is treated the same: whatever stands between cannot speak for the agent.
//     A redirect is not followed, and it is not a "yes" either (r3-p2-08: a 302 with no body counted as
//     accepted, for a wake, an update, a shutdown and a cancel); nor is a good status with an empty body.
//     And 502 and 504 are "no answer" whatever the body, also with a JSON body that has an "error" in it (a
//     gateway that answers in JSON; second review of fix7a, R7A-08): they are the two statuses a gateway
//     gives after it passed the request on. The agent answers a power request with 504 itself in one case,
//     and means the same by it: it handed the request to its helper (Windows) and the helper did not answer
//     in time, so the shutdown may be scheduled, or the countdown cancelled, all the same (Fairy-agent,
//     r4-a1-01; before that it was a 500, a refusal, for a machine that then went off). It answers an update
//     and a wake with neither. It does answer its BMC relay's request with them (the BMC refused, or did not
//     answer), with a code of its own, and a fan change its BMC did not take with 502: see relaysOwn.
func agentCall(ctx context.Context, p *poller.Poller, a config.Agent, path string, body any) (jsonx.M, error) {
	status, data, err := p.AgentRequest(ctx, a, "POST", path, body)
	if e := NoAnswer(path, status, data, err); e != nil {
		return nil, e
	}
	errMsg := func(def string) string {
		if e := textsafe.Clip(jsonx.S(data, "error"), maxAgentError); e != "" {
			return e
		}
		return def
	}
	refusal := func(e *apierr.Error) error { return &AgentRefusal{Err: e, Code: agentCode(data), Status: status} }
	switch {
	case status == 503 && (jsonx.S(data, "code") == api.ReasonHelperStopped || jsonx.S(data, "code") == api.ReasonHelperError):
		return nil, refusal(apierr.New(409, HelperAnswerWhy(jsonx.S(data, "code")))) // not an agent failure
	case status == 401: // the agent answered: 424, never 502 (see apierr.Upstream)
		return nil, apierr.New(apierr.Upstream, "agent refused the control token (check control_token_env)")
	case status == 404:
		return nil, refusal(apierr.New(409, errMsg("disabled on that agent")))
	case status >= 500:
		return nil, refusal(apierr.New(apierr.Upstream, errMsg(fmt.Sprintf("agent error %d", status))))
	case status >= 400:
		return nil, refusal(apierr.New(400, errMsg(fmt.Sprintf("agent error %d", status))))
	}
	return data, nil
}
