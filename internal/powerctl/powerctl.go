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

// Capabilities lists every action with availability, reason, confirmation and route.
func Capabilities(p *poller.Poller, s model.System) model.PowerCaps {
	caps := capabilities(p, s)
	blocked := ""
	if p.Admin.ControlDisabled() != nil {
		blocked = "control is disabled by an administrator (fairy admin control enable)"
	} else if p.Admin.PowerDisabled(s.ID) {
		blocked = fmt.Sprintf("power control for this host is disabled by an administrator (fairy admin host %s power-control enable)", s.ID)
	}
	if blocked != "" {
		for i, a := range caps.Actions {
			if a.Available {
				caps.Actions[i].Available, caps.Actions[i].Reason, caps.Actions[i].Via = false, &blocked, nil
			}
		}
	}
	return caps
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
			if firstErr == nil {
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

func agentCall(ctx context.Context, p *poller.Poller, a config.Agent, path string, body any) (jsonx.M, error) {
	status, data, err := p.AgentRequest(ctx, a, "POST", path, body)
	if err != nil {
		return nil, apierr.New(502, "agent unreachable: "+devices.Describe(err))
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
		return nil, refusal(apierr.New(409, HelperStoppedWhy)) // the helper stopped since the last poll: not an agent failure
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
