package web

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/enrollment"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// Add host (admins): the dialog's data, creating a pending enrollment (a control action: CSRF, kill switch,
// step-up, rate limit, audit host.add), its progress, cancel (browser session, CSRF, step-up), and for hosts added this way Re-pair (a new code
// when the panel lost the tokens) and Remove. The machine runs one command; the panel pairs by itself, with
// pairing v2 only (internal/enrollment): an answer that does not prove the join code is never stored, and an
// agent or release from before v2 is reported as too old (409 for the release, a hint while it waits).

func (s *Server) enrollRoutes() {
	s.mux.HandleFunc("GET /api/admin/hosts", s.h(s.hostsPage))
	s.mux.HandleFunc("POST /api/admin/hosts", s.h(s.hostAdd))
	s.mux.HandleFunc("POST /api/admin/hosts/{id}/repair", s.h(s.hostRepair))
	s.mux.HandleFunc("DELETE /api/admin/hosts/{id}", s.h(s.hostRemove))
	s.mux.HandleFunc("GET /api/admin/enrollments/{id}", s.h(s.enrollmentGet))
	s.mux.HandleFunc("DELETE /api/admin/enrollments/{id}", s.h(s.enrollmentCancel))
}

// enrollHooks connects the enrollment manager to the poller, the managed store, the audit log and notifications.
func (s *Server) enrollHooks() enrollment.Hooks {
	return enrollment.Hooks{
		// One consistent copy, read under the poller's locks: the poller replaces the agents, systems and
		// proxmox lists of the shared configuration whenever a host is added, re-paired or removed.
		Config: s.Poller.ConfigSnapshot,
		Add: func(e managed.Entry) error {
			if e.Host.Site != "" {
				_ = s.Access.EnsureSites([]string{e.Host.Site})
			}
			if e.Proxmox != nil { // the node first, so the agent attaches to it at once
				if err := s.Poller.AddProxmox(*e.Proxmox); err != nil {
					return err
				}
			}
			if err := s.Poller.AddAgent(e.Agent, e.Override); err != nil {
				if e.Proxmox != nil {
					s.Poller.RemoveProxmox(e.Proxmox.ID)
				}
				return err
			}
			return nil
		},
		Remove: s.unpoll,
		// Add host starts clean: what "Edit host" still keeps under the new host's id (its own entry, and changes
		// about its rows kept with other agents) was made for another machine. Remove takes it out already; this
		// is for what is there all the same (a host taken out of config.yaml, a state.json from a backup). It is
		// asked before anything of the host is stored, and when it cannot be done the host is not added: with
		// those changes it would show another machine's UPS and plugs as its own, and nothing would say so.
		Fresh: func(h managed.Host) error {
			by := h.AddedBy
			if by == "" {
				by = "fairy"
			}
			gone, notes, err := s.forgetHostOptions(by, h.ID, rowsOf(h)...)
			if err != nil {
				slog.Warn("Add host: what \"Edit host\" kept under the new host's id could not be taken out: the host is not added", "host", h.ID, "error", err)
				s.writeAudit(audit.Entry{Actor: by, Action: "host.options", Target: h.ID, Source: "fairy", Params: map[string]any{"because": "host added"}, Outcome: "error: " + err.Error()})
				return fmt.Errorf("what \"Edit host\" still keeps under this host's id could not be taken out (%v)", err)
			}
			if len(gone) > 0 {
				s.writeAudit(audit.Entry{Actor: by, Action: "host.options", Target: h.ID, Source: "fairy", Params: forgotParams(gone, notes, "host added"), Outcome: "ok"})
			}
			return nil
		},
		Online: func(id string) (bool, string) {
			info, _ := s.Poller.AgentInfo(id)
			if h, ok := s.managedHost(id); ok && h.Proxmox != nil { // a node: its Proxmox API must answer too
				if ok, why := s.proxmoxOnline(h.Proxmox.ID); !ok {
					return false, why
				}
			}
			if info.Online {
				return true, ""
			}
			for _, x := range s.Poller.Snapshot() {
				if x.ID == id && x.Error != nil {
					return false, *x.Error
				}
			}
			return false, ""
		},
		Event: s.enrollEvent,
		AgentVersion: func(id string) string {
			info, _ := s.Poller.AgentInfo(id)
			return info.Version
		},
	}
}

// unpoll stops polling a managed host: its agent and, for a Proxmox node added with Add host, the node.
func (s *Server) unpoll(id string) {
	if h, ok := s.managedHost(id); ok && h.Proxmox != nil {
		s.Poller.RemoveProxmox(h.Proxmox.ID)
	}
	if a, ok := s.Poller.AgentByID(id); ok && a.Managed { // a config.yaml agent with the same id stays
		s.Poller.RemoveAgent(id)
	}
}

// proxmoxOnline reports whether a cluster's last poll worked (and its error if not).
func (s *Server) proxmoxOnline(id string) (bool, string) {
	for _, src := range s.Poller.Summary().Sources {
		if src.Name == "proxmox:"+id {
			if src.OK {
				return true, ""
			}
			return false, "Proxmox API: " + model.Deref(src.LastError)
		}
	}
	return false, "Proxmox API: not polled yet"
}

func (s *Server) enrollEvent(e enrollment.Enrollment, kind, detail string) {
	outcome := kind
	if detail != "" {
		outcome += ": " + detail
	}
	action := "host.pair"
	if e.Repair {
		action = "host.repair"
	}
	params := map[string]any{"enrollment": e.ID, "url": e.URL}
	if e.Pairing != "" {
		params["pairing"] = e.Pairing // "v2": the agent's answer was verified with the join code
	}
	if e.AgentHostname != "" {
		params["agent_hostname"] = e.AgentHostname // what the machine that answered calls itself
	}
	s.writeAudit(audit.Entry{Actor: e.CreatedBy, Action: action, Target: e.HostID, Source: "fairy", Params: params, Outcome: outcome})
	n := notifications.Event{Type: "enroll", SystemID: e.HostID, Host: e.Name}
	switch kind {
	case "online":
		n.Kind, n.Severity, n.Text = "host_added", notifications.Info, "added with Add host and online"
		if e.Repair {
			n.Text = "re-paired and online"
		}
	case "paired":
		return // the "online" event follows (or the host page shows why not)
	case "failed":
		n.Kind, n.Severity, n.Text = "pairing_failed", notifications.Warning, "Add host failed: "+detail
	case "expired":
		n.Kind, n.Severity, n.Text = "pairing_expired", notifications.Info, "Add host: the code expired before the machine ran the command"
	default:
		return
	}
	if s.Notes != nil {
		s.Notes.Add(n)
	}
}

type hostsPage struct {
	PanelAddress string `json:"panel_address"`
	PanelSource  string `json:"panel_address_source"` // config | auto | demo
	PanelVia     string `json:"panel_address_via,omitempty"`
	// AgentRelease is the Fairy-agent release the command installs (tag; the version once checked).
	AgentRelease string                  `json:"agent_release"`
	Nodes        []nodeChoice            `json:"nodes"`
	Relays       []idName                `json:"relays"`
	BMCRelays    []idName                `json:"bmc_relays"` // agents with a control token (BMC power-on relays)
	Sites        []string                `json:"sites"`
	Managed      []managedRow            `json:"managed"`
	Enrollments  []enrollment.Enrollment `json:"enrollments"`
	// Skipped: what is to be said about the list as a whole (it holds no host although tokens of added hosts
	// are there: managed.Store.ListNote). What is to be said about one host is in its row (managedRow.Why).
	Skipped []string `json:"skipped,omitempty"`
	// OutsideTailscale: enroll.allow_non_tailscale is on, so the dialog must not say that the machine has to be
	// in the Tailscale network (the test build's config has it on).
	OutsideTailscale bool `json:"outside_tailscale"`
}

type nodeChoice struct {
	Name     string `json:"name"`
	SystemID string `json:"system_id"`
	Cluster  string `json:"cluster"`
}

type idName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type managedRow struct {
	managed.Host
	SystemID string `json:"system_id"`
	Online   bool   `json:"online"`
	Active   bool   `json:"active"` // polled now, as the host Add host made
	// Shadowed: config.yaml has an entry with this id, and it wins. The machine is polled as that entry, and
	// this one, the copy Add host made, is not used. What the panel stores under the id (what "Edit host"
	// changed, the model name, the switch for "Update unpowered hosts") is that entry's, not the copy's.
	Shadowed bool `json:"shadowed"`
	// Why, for a host that is not Active: what is the matter, as it is now (managedState).
	Why string `json:"why,omitempty"`
}

// managedState says what an added host is right now: polled as itself (active), a copy that config.yaml's
// entry of the same id shadows, or neither, and for the last two why, in words for the page.
//
// It is asked when the page asks, not kept from the start of the panel: the lines the start logs about the
// hosts it left out (Server.ManagedSkipped) stayed the same until the next start, so a host that had been
// removed or paired again since was still listed as left out; and they are written for the log, with the
// tokens file's full path and, from a file that is not JSON, the character the decoder stopped at. The page
// is told which file and what kind of thing is wrong with it (managed.Store.TokensProblem), no more.
func (s *Server) managedState(h managed.Host) (active, shadowed bool, why string) {
	a, polled := s.Poller.AgentByID(h.ID)
	switch {
	case polled && a.Managed:
		return true, false, ""
	case polled:
		return false, true, "config.yaml has an entry with the id " + h.ID + ", and config.yaml wins: the machine is polled as that entry, and this copy from Add host is not used"
	}
	if why = s.Managed.TokensProblem(h.ID); why != "" {
		return false, false, why
	}
	sec, err := s.Managed.Secret(h.ID)
	if err != nil { // it changed between the two looks
		return false, false, "its tokens file cannot be read"
	}
	if err := managed.Check(s.Poller.ConfigSnapshot(), s.Managed.EntryOf(h, sec)); err != nil {
		return false, false, "its entry does not pass the panel's check: " + strings.Join(strings.Fields(strings.ReplaceAll(err.Error(), "config invalid:\n  -", "")), " ")
	}
	return false, false, "nothing is wrong with its files any more: it is polled from the next start of the panel, or after a Re-pair"
}

// shadowedCopy reports whether the added host h is a copy that config.yaml's entry of the same id shadows
// (managedRow.Shadowed).
func (s *Server) shadowedCopy(h managed.Host) bool {
	_, shadowed, _ := s.managedState(h)
	return shadowed
}

func (s *Server) nodeSystemID(name string) string {
	for _, x := range s.Poller.Snapshot() {
		if x.Kind == "node" && x.Name == name {
			return x.ID
		}
	}
	return ""
}

func (s *Server) hostsPage(c *reqCtx) error {
	if _, err := c.requireAdmin("Add host"); err != nil {
		return err
	}
	p := hostsPage{PanelAddress: s.Enroll.PanelAddr, PanelSource: s.Enroll.PanelSource, PanelVia: s.Enroll.PanelVia, AgentRelease: s.Enroll.ReleaseLabel(), Nodes: []nodeChoice{}, Relays: []idName{}, BMCRelays: []idName{},
		Sites: []string{}, Managed: []managedRow{}, Enrollments: s.Enroll.List(), OutsideTailscale: s.Cfg.Enroll.AllowNonTailscale}
	for _, x := range s.Poller.Snapshot() {
		if x.Kind == "node" {
			cl := ""
			if x.Cluster != nil {
				cl = *x.Cluster
			}
			p.Nodes = append(p.Nodes, nodeChoice{Name: x.Name, SystemID: x.ID, Cluster: cl})
		}
	}
	for _, a := range s.Poller.Agents() {
		if a.WolRelay {
			p.Relays = append(p.Relays, idName{a.ID, a.DisplayName()})
		}
		if a.HasControl() {
			p.BMCRelays = append(p.BMCRelays, idName{a.ID, a.DisplayName()})
		}
	}
	p.Sites = append(p.Sites, s.Access.Load().Sites...)
	sort.Strings(p.Sites)
	hosts, err := s.Managed.Load()
	if err != nil {
		return apierr.New(500, "managed-hosts.yaml: "+err.Error())
	}
	for _, h := range hosts {
		info, _ := s.Poller.AgentInfo(h.ID)
		active, shadowed, why := s.managedState(h)
		p.Managed = append(p.Managed, managedRow{Host: h, SystemID: h.Shown(), Online: info.Online, Active: active, Shadowed: shadowed, Why: why})
	}
	if note := s.Managed.ListNote(); note != "" {
		p.Skipped = []string{note}
	}
	c.json(200, p)
	return nil
}

func enrollErr(err error) error {
	var v *enrollment.Validation
	if errors.As(err, &v) {
		return apierr.New(400, v.Msg)
	}
	var u *enrollment.Unavailable
	if errors.As(err, &u) {
		return apierr.New(503, u.Msg)
	}
	var old *enrollment.TooOld
	if errors.As(err, &old) {
		return apierr.New(409, old.Msg)
	}
	return err
}

func (s *Server) hostAdd(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	var req enrollment.Request
	if err := c.decodeBody(&req); err != nil {
		return err
	}
	u, err := c.controlGuard(s.control)
	if err != nil {
		return err
	}
	cr, err := s.Enroll.Create(c.r.Context(), req, nil, actorOf(u), s.nodeSystemID)
	params := map[string]any{"name": req.Name, "os": req.OS, "site": req.Site, "node": req.Node, "options": req.Options,
		"wol_macs": len(req.WolMACs)}
	if err != nil {
		c.audit(u, "host.add", managed.IDFor(req.Name), params, "refused: "+err.Error())
		return enrollErr(err)
	}
	c.audit(u, "host.add", cr.HostID, params, "join code made (valid 15 min)")
	if c.resumed() {
		s.Enroll.Hold(cr, c.codeHolder(u)) // made right after a step-up sign-in: the browser fetches the code once (reveal=1)
	}
	c.json(201, cr)
	return nil
}

func (s *Server) hostRepair(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	u, err := c.controlGuard(s.control)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	h, ok := s.managedHost(id)
	if !ok {
		return apierr.New(404, "only hosts added with Add host can be re-paired here")
	}
	if s.shadowedCopy(h) {
		// A copy that config.yaml's entry shadows is not paired again. The pairing itself would go through: the
		// machine takes the code and answers. Only then is the answer checked against everything the panel has
		// (enrollment.commit), and refused, because config.yaml has the id. By then the machine has made its
		// side of a pairing the panel does not keep. So no code is made at all.
		why := h.Name + " is polled as config.yaml's entry (config.yaml has the id " + h.ID + ", and config.yaml wins): the copy made by Add host is not used, and it is not paired again. " +
			"The panel would refuse what the machine answers, and the machine may then hold tokens that config.yaml's entry does not have. " +
			"Remove the unused copy; or take the entry out of config.yaml, restart the panel, and Re-pair then"
		c.audit(u, "host.repair", h.ID, nil, "refused: "+why)
		return apierr.New(409, why)
	}
	renamed, err := c.typedRepair(h.Name)
	if err != nil {
		c.audit(u, "host.repair", h.ID, nil, "refused: "+err.Error())
		return err
	}
	var params map[string]any
	if renamed { // the admin accepts another Proxmox node name in the answer: on record
		params = map[string]any{"node_renamed": true}
	}
	cr, err := s.Enroll.Create(c.r.Context(), enrollment.Request{NodeRenamed: renamed}, &h, actorOf(u), s.nodeSystemID)
	if err != nil {
		c.audit(u, "host.repair", id, params, "refused: "+err.Error())
		return enrollErr(err)
	}
	outcome := "join code made (valid 15 min)"
	if cr.Warning != "" { // L24: on record that the admin was told
		outcome += "; warned: the machine runs a newer Fairy-agent than the release the command installs (" + cr.Release.Tag + ")"
	}
	c.audit(u, "host.repair", id, params, outcome)
	if c.resumed() {
		s.Enroll.Hold(cr, c.codeHolder(u))
	}
	c.json(201, cr)
	return nil
}

func (s *Server) managedHost(id string) (managed.Host, bool) {
	hosts, err := s.Managed.Load()
	if err != nil {
		return managed.Host{}, false
	}
	for _, h := range hosts {
		if h.ID == id || h.Shown() == id {
			return h, true
		}
	}
	return managed.Host{}, false
}

func (s *Server) hostRemove(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	u, err := c.controlGuard(s.control)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	h, ok := s.managedHost(id)
	if !ok {
		return apierr.New(404, "only hosts added with Add host can be removed here (others: edit config.yaml)")
	}
	if err := c.typedName(h.Name, "remove"); err != nil {
		c.audit(u, "host.remove", h.ID, nil, "refused: "+err.Error())
		return err
	}
	// A copy that config.yaml's entry of the same id shadows (managedRow.Shadowed) is removed as what it is: an
	// entry in the list of added hosts and a tokens file. The machine stays in the panel, as config.yaml's
	// entry, and everything the panel stores under its id is that entry's: its switch for "Update unpowered
	// hosts", its model name, what "Edit host" changed for it. None of the three clean-ups below is for it.
	// They ran for such a copy too, each with the audit entry "host removed": a Remove of the unused copy
	// (offered on the machine's own page, and since this build in the Add host dialog) took the model name and
	// the Edit host changes of a machine that was not removed (check of the build 15 candidate, K1).
	copyOnly := s.shadowedCopy(h)
	cleaned := []string{h.ID, h.Shown()}
	if copyOnly {
		cleaned = nil
	}
	// A machine that is removed is no longer switched on for "Update unpowered hosts". A host's id is
	// made from its name, so without this the next machine added under the same name would take part in
	// runs without anybody having switched it on. First, so that a host is never gone with the switch left on.
	wasOn := false
	for _, sid := range cleaned {
		if !s.Poller.Admin.WakeUpdateEnabled(sid) {
			continue
		}
		if err := s.Poller.Admin.SetWakeUpdate(sid, false); err != nil {
			c.audit(u, "host.remove", h.ID, nil, "error: "+err.Error())
			return err
		}
		wasOn = true
		c.audit(u, "wakeupdate.switch", sid, map[string]any{"enabled": false, "because": "host removed"}, "ok")
	}
	// Its custom model name goes with it, for the same reason: the next machine added under this name starts
	// with the model it reports itself. Also before the host is gone: a host is never removed with its name left.
	hadModel := false
	for _, sid := range cleaned {
		prev, err := s.Poller.Admin.SetHostModel(sid, "")
		if err != nil {
			c.audit(u, "host.remove", h.ID, nil, "error: "+err.Error())
			return err
		}
		if prev != "" {
			hadModel = true
			c.audit(u, "host.model", sid, map[string]any{"model": "", "before": prev, "because": "host removed"}, "ok")
		}
	}
	// What "Edit host" changed goes with it too, for the same reason: the overrides of its own agent, and every
	// change about one of the rows that go away with it in the overrides of other agents (api_hostoptions.go).
	if !copyOnly {
		gone, notes, err := s.forgetHostOptions(actorOf(u), h.ID, rowsOf(h)...)
		if err != nil {
			c.audit(u, "host.remove", h.ID, nil, "error: "+err.Error())
			return err
		}
		if len(gone) > 0 {
			c.audit(u, "host.options", h.ID, forgotParams(gone, notes, "host removed"), "ok")
		}
	}
	s.Enroll.CancelHost(h.ID) // a Re-pair still pairing must not bring the host back
	s.unpoll(h.ID)            // before the store forgets which node is the host's; config.yaml's agent of the id stays
	if err := s.Managed.Remove(h.ID); err != nil && !errors.Is(err, os.ErrNotExist) {
		c.audit(u, "host.remove", h.ID, nil, "error: "+err.Error())
		return err
	}
	params := map[string]any{"name": h.Name, "wake_update_was_on": wasOn, "custom_model_removed": hadModel}
	if copyOnly {
		params["copy_only"] = true // the unused copy of a machine that config.yaml has: the machine stays
	}
	c.audit(u, "host.remove", h.ID, params, "ok")
	text := fmt.Sprintf("removed from the panel by %s (the agent on the machine keeps running until you uninstall it)", actorOf(u))
	msg := h.Name + " removed from the panel"
	if copyOnly {
		text = fmt.Sprintf("its unused Add host copy was removed by %s (the machine stays in the panel, as config.yaml's entry)", actorOf(u))
		msg = "the unused Add host copy of " + h.Name + " was removed. " + h.Name + " stays in the panel as config.yaml's entry, and nothing that is stored for it was changed"
	}
	if s.Notes != nil {
		s.Notes.Add(notifications.Event{Kind: "host_removed", Type: "enroll", Severity: notifications.Info, Host: h.Name, Text: text})
	}
	if h.Proxmox != nil {
		msg += fmt.Sprintf(". Its read-only Proxmox token still exists on the node; to delete it, run there: pveum user token remove fairy@pve %s",
			h.Proxmox.TokenName())
	}
	c.json(200, map[string]any{"ok": true, "message": msg})
	return nil
}

// codeHolder names who a join code is held for after a step-up: the account, and the client (address and
// browser) whose confirmation made the panel create it. The replay that creates it carries that client's
// address and browser, so Hold and the browser's Reveal agree, and nobody else's Reveal does.
func (c *reqCtx) codeHolder(u *model.User) string {
	return u.Sub + "\x00" + c.s.codec.clientMark(c.client())
}

func (s *Server) enrollmentGet(c *reqCtx) error {
	u, err := c.requireAdmin("Add host")
	if err != nil {
		return err
	}
	if c.r.URL.Query().Get("reveal") == "1" {
		if u.Auth == "token" { // the join code goes to the browser that made it, never to an API token
			return c.deny(u, 403, "the join code is shown to the signed-in admin who made it, not to an API token", "API token asked for a join code")
		}
		// NEW-7: the code goes once to the client that confirmed this Add host, while that confirmation still
		// counts. authAge is the age of the sign-in as this very client (address and browser) confirmed it, so
		// the same cookie from anywhere else is refused here, and codeHolder ties the code to the account and
		// client the panel made it for. A refusal leaves the code where it is: it is not "shown" by asking.
		if age := c.authAge(u); age == nil || *age > float64(max(s.Cfg.StepUpWindowS, perActionGrace)) {
			return c.deny(u, 403, "the join code is shown once, to the browser that confirmed this Add host, right after "+
				"the confirmation. Cancel this Add host and make a new code.", "join code asked for without a fresh confirmation from this client")
		}
		cr, ok := s.Enroll.Reveal(c.r.PathValue("id"), c.codeHolder(u))
		if !ok {
			return apierr.New(404, "the code was already shown, or this Add host is over: make a new code")
		}
		c.json(200, cr)
		return nil
	}
	e, ok := s.Enroll.Get(c.r.PathValue("id"))
	if !ok {
		return apierr.New(404, "no such Add host (the panel restarted? make a new code)")
	}
	c.json(200, e)
	return nil
}

// enrollmentCancel calls a waiting Add host or Re-pair off. It is guarded like making one (NEW-3): browser
// session only (an API token is refused), CSRF, admin, step-up, rate limit. The kill switch does not refuse it.
func (s *Server) enrollmentCancel(c *reqCtx) error {
	u, err := c.stopGuard(s.control)
	if err != nil {
		return err
	}
	if !s.Enroll.Cancel(c.r.PathValue("id")) {
		return apierr.New(409, "nothing waiting to cancel")
	}
	c.audit(u, "host.cancel", c.r.PathValue("id"), nil, "ok")
	c.json(200, map[string]any{"ok": true, "message": "cancelled"})
	return nil
}

// typedRepair is Re-pair's body: the typed confirmation like typedName, and optionally "node_renamed": true
// (the admin says the Proxmox node was renamed, so the machine may answer with another node name than the one
// it was added as).
func (c *reqCtx) typedRepair(name string) (renamed bool, err error) {
	var b struct {
		Confirm     string `json:"confirm"`
		NodeRenamed bool   `json:"node_renamed"`
	}
	if err := c.decodeBody(&b); err != nil || !strings.EqualFold(strings.TrimSpace(b.Confirm), strings.TrimSpace(name)) {
		return false, apierr.New(400, fmt.Sprintf("type the host's name (%s) to confirm re-pair", name))
	}
	return b.NodeRenamed, nil
}

// typedName enforces the typed confirmation of Remove (and of stopping / starting a helper): the body is
// {"confirm": "<the host's name>"} (case-insensitive), like a shutdown.
func (c *reqCtx) typedName(name, what string) error {
	var b struct {
		Confirm string `json:"confirm"`
	}
	if err := c.decodeBody(&b); err != nil {
		return apierr.New(400, fmt.Sprintf("type the host's name (%s) to confirm %s", name, what))
	}
	if !strings.EqualFold(strings.TrimSpace(b.Confirm), strings.TrimSpace(name)) {
		return apierr.New(400, fmt.Sprintf("type the host's name (%s) to confirm %s", name, what))
	}
	return nil
}
