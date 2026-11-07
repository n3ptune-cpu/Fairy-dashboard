// Package powerctl: which power actions a system offers (and why not), and carrying them out.
//
// Routes, in order of preference:
//   - hosts / Proxmox nodes with an agent that has --enable-power-control: shutdown / reboot (delayed, cancellable)
//   - Proxmox nodes without one, when the cluster entry has `node_power: true`: POST /nodes/{node}/status
//   - Proxmox guests, when the cluster entry has a power token: start / shutdown (ACPI) / reboot / stop (hard)
//   - offline hosts with `wol:` configured: magic packet sent by a relay agent on the same LAN
//
// Every write is control-group + CSRF + step-up + rate-limited + audited in the web package; typed confirmation
// is enforced here.
package powerctl

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nsxservices/fairy/internal/apierr"
	"github.com/nsxservices/fairy/internal/config"
	"github.com/nsxservices/fairy/internal/jsonx"
	"github.com/nsxservices/fairy/internal/model"
	"github.com/nsxservices/fairy/internal/normalise"
	"github.com/nsxservices/fairy/internal/poller"
)

var Actions = []string{"start", "shutdown", "reboot", "stop", "cancel", "wake"}

// Request is the POST /api/systems/{id}/power body.
type Request struct {
	Action  string  `json:"action"`
	DelayS  *int    `json:"delay_s"` // agent shutdown/reboot only (Linux rounds up to minutes); default 60
	Confirm *string `json:"confirm"` // the system's name, typed (shutdown / stop)
}

func (r *Request) Validate() error {
	if !slices.Contains(Actions, r.Action) {
		return apierr.New(422, "action must be one of start, shutdown, reboot, stop, cancel, wake")
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

func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func mkcap(action string, ok bool, via, reason, confirm string, delayed bool) model.PowerActionCap {
	c := model.PowerActionCap{Action: action, Available: ok, Confirm: confirm, Delayed: delayed}
	if ok {
		c.Via = ptr(via)
	} else {
		c.Reason = ptr(reason)
	}
	return c
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// KillsPanel: the panel itself runs on this system (or on a guest of this node).
func KillsPanel(p *poller.Poller, s model.System) bool {
	if slices.Contains(p.Cfg.PanelHosts, s.ID) {
		return true
	}
	if s.Kind == "node" {
		for _, g := range p.Snapshot() {
			if slices.Contains(p.Cfg.PanelHosts, g.ID) && eq(g.ClusterID, s.ClusterID) && g.Parent != nil && *g.Parent == s.Name {
				return true
			}
		}
	}
	return false
}

func eq(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }

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
	var caps []model.PowerActionCap
	kill := KillsPanel(p, s)
	up := s.Status == "online" || s.Status == "paused"
	clusterID := ""
	if s.ClusterID != nil {
		clusterID = *s.ClusterID
	}
	pve, hasPVE := p.ClusterCfg(clusterID)
	typedIfKill := "click"
	if kill {
		typedIfKill = "typed"
	}
	if s.Kind == "qemu" || s.Kind == "lxc" {
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
		caps = append(caps,
			mkcap("start", why == "" && s.Status == "stopped", "proxmox-api", or(why, "guest is not stopped"), typedIfKill, false),
			mkcap("shutdown", why == "" && up, "proxmox-api", or(why, running), "typed", false),
			mkcap("reboot", why == "" && up, "proxmox-api", or(why, running), typedIfKill, false),
			mkcap("stop", why == "" && up, "proxmox-api", or(why, running), "typed", false))
	} else {
		agent, hasAgent := p.AgentFor(s.ID)
		via, why := "", ""
		switch {
		case hasAgent && agent.PowerControl:
			via = "agent"
		case s.Kind == "node" && hasPVE && pve.NodePower:
			via = "proxmox-api"
		case hasAgent:
			why = "power control is not enabled on " + agent.DisplayName() + "'s agent (owner opts in: --enable-power-control)"
		case s.Kind == "node":
			why = "no agent with --enable-power-control on this node and node_power is off for its cluster"
		default:
			why = "power control is not enabled for this host"
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
		caps = append(caps,
			mkcap("shutdown", why == "" && up, via, or(why, offline), "typed", delayed),
			mkcap("reboot", why == "" && up, via, or(why, offline), typedIfKill, delayed),
			mkcap("cancel", via == "agent" && s.PowerPending != nil, via, or(why, cancelWhy), "none", false),
			wakeCap(p, s))
	}
	pc := model.PowerCaps{SystemID: s.ID, Name: s.Name, Actions: caps, Pending: s.PowerPending, KillsPanel: kill}
	if kill {
		pc.Warning = model.P("The panel itself runs here: shutting this down takes Fairy offline until it is powered on again by other means.")
	}
	return pc
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

// Execute carries out one action.
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
	via := ""
	if c.Via != nil {
		via = *c.Via
	}
	res := model.PowerResult{SystemID: s.ID, Action: req.Action, Via: via}
	if req.Action == "wake" {
		o := p.Override(s.ID)
		relay, ok := p.AgentByID(o.Wol.Relay)
		if !ok {
			return res, apierr.New(409, "relay agent not configured")
		}
		port := o.Wol.Port
		if port == 0 {
			port = 9
		}
		body := map[string]any{"mac": o.Wol.MAC, "broadcast": nil, "port": port}
		if o.Wol.Broadcast != "" {
			body["broadcast"] = o.Wol.Broadcast
		}
		if o.Wol.PasswordEnv != "" {
			if pw := os.Getenv(o.Wol.PasswordEnv); pw != "" {
				body["password"] = pw
			}
		}
		if _, err := agentCall(ctx, p, relay, "/wol", body); err != nil {
			return res, err
		}
		res.Message = "magic packet sent via " + relay.DisplayName() + "; give it a minute to boot"
		return res, nil
	}
	if via == "agent" {
		agent, _ := p.AgentFor(s.ID)
		data, err := agentCall(ctx, p, agent, "/power", map[string]any{"action": req.Action, "delay_s": *req.DelayS,
			"requested_by": requestedBy})
		if err != nil {
			return res, err
		}
		pending := parsePending(jsonx.Obj(data, "pending"))
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
	// Proxmox API with the separate power token
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

func parsePending(m jsonx.M) *model.PowerPending {
	if m == nil {
		return nil
	}
	at := jsonx.Num(m, "at")
	if at == nil {
		return nil
	}
	sec, frac := math.Modf(*at)
	pp := &model.PowerPending{Action: jsonx.S(m, "action"), At: time.Unix(int64(sec), int64(frac*1e9)).UTC(),
		RequestedBy: jsonx.Str(m, "requested_by")}
	if d := jsonx.Num(m, "delay_s"); d != nil {
		pp.DelayS = int(*d)
	}
	return pp
}

func agentCall(ctx context.Context, p *poller.Poller, a config.Agent, path string, body any) (jsonx.M, error) {
	status, data, err := p.AgentRequest(ctx, a, "POST", path, body)
	if err != nil {
		return nil, apierr.New(502, "agent unreachable: "+normalise.Describe(err))
	}
	errMsg := func(def string) string {
		if e := jsonx.S(data, "error"); e != "" {
			return e
		}
		return def
	}
	switch {
	case status == 401:
		return nil, apierr.New(502, "agent refused the control token (check control_token_env)")
	case status == 404:
		return nil, apierr.New(409, errMsg("disabled on that agent"))
	case status >= 500:
		return nil, apierr.New(502, errMsg(fmt.Sprintf("agent error %d", status)))
	case status >= 400:
		return nil, apierr.New(400, errMsg(fmt.Sprintf("agent error %d", status)))
	}
	return data, nil
}
