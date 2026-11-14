package web

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// "Edit host" (the ⋯ menu of a host's page, admins): which UPS a machine is on, which plugs are its plugs,
// and which of them count in the total. These are the three link fields of config.yaml (ups_powers,
// power_links, power_exclude_from_total); what is changed here is kept as an overlay in state.json
// (adminstate.HostOptions) and counts with the next poll of the agent that reads the device
// (poller.LinksOf). config.yaml is never written and stays the base: "Reset" takes an override out.
//
//	GET /api/admin/hosts/{id}/options    what is set for this machine, where each value comes from, the choices
//	PUT /api/admin/hosts/{id}/options    {"ups": [{"id", "on"}], "plugs": [{"id", "linked", "in_total"}],
//	                                      "reset": [{"agent", "field"}]}
//
// The request is about ONE machine, the one whose page is open, and says only what the admin changed (ticks),
// never a whole list: two admins who save at the same moment each change their own ticks on what the other
// left (adminstate.EditHostOptions), and a dialog that was open for an hour cannot put back what somebody
// else took out in the meantime.
//
// Where it is stored is not where it is edited. The link fields are in the entry of the agent that READS the
// device: "Rack UPS powers PC-B" is a line in the entry of the NAS that reads the UPS. The API takes the
// machine, finds the reading agent itself (the id of a UPS or plug begins with it), and says both in the
// answer, in the lines of "Confirm it's you" and in the audit entry (target: the machine; changes[].agent:
// whose entry was overridden).
//
// What can be sent is a closed set: the three lists above with the fields named there, decoded strictly. A
// request cannot name a row, an address, a token or a switch: the row is the machine of the path, and the
// rest has no field. Never editable here: id, url, token_env, control_token_env, verify_tls, node, links_to,
// fan_control, power_control, self_update, wol_relay, owner, site, name.
//
// Who may: the guard of the Users page's changes (adminGuardFor): an admin's browser session (API tokens:
// 403), the CSRF header and the panel's own origin, "Confirm it's you" inside the step-up window, the rate
// limit of admin changes. The kill switch does not apply: it stops device control, and this controls no
// device. Every change is one entry in the audit log (host.options, with before and after; it also goes to
// the notification channel) and one in the bell.

const (
	hostOptionsRoute = "PUT /api/admin/hosts/{id}/options"
	// How many entries one request may have. A machine has a handful of UPSes and plugs; the lists are bounded
	// so that a request is small work whatever it holds.
	maxHostOptionEntries = 128
)

func init() {
	// What a kept request does, for "Confirm it's you" and its audit entries (pending.go), and the route's line
	// in the description of the API (docs.go). Added here, next to the route, so that this feature's lines are
	// in one file.
	actionText[hostOptionsRoute] = "change the links of {id}"
	actionList[hostOptionsRoute] = (*reqCtx).hostOptionsItems
	apiDoc = append(apiDoc,
		struct{ method, path, summary string }{"GET", "/api/admin/hosts/{id}/options", "Edit host: which UPS powers this machine, which plugs are linked to it and which count in the total, with where each value comes from (config.yaml or changed in the panel) and the agent that reads the device (admins, browser session only)"},
		struct{ method, path, summary string }{"PUT", "/api/admin/hosts/{id}/options", "Edit host: change the links of one machine: {ups: [{id, on}], plugs: [{id, linked, in_total}], reset: [{agent, field: ups|plugs|total}]}; only what is named changes. Stored as an overlay in state.json (config.yaml is not written; reset takes an override out), counts with the reading agent's next poll. Admins, browser session only (API tokens get 403), CSRF header, step-up. 422: an unknown field, UPS or plug; 409: not a host. Audited as host.options"})
}

func (s *Server) hostOptionsRoutes() {
	s.mux.HandleFunc("GET /api/admin/hosts/{id}/options", s.h(s.hostOptionsGet))
	s.mux.HandleFunc("PUT /api/admin/hosts/{id}/options", s.h(s.hostOptionsPut))
}

// hostOptionsReq is everything "Edit host" can be asked to change.
type hostOptionsReq struct {
	UPS   []hoUPSReq   `json:"ups"`
	Plugs []hoPlugReq  `json:"plugs"`
	Reset []hoResetReq `json:"reset"`
}

// hoUPSReq: the UPS with this id (as GET /api/ups names it, "<agent>/<name>") powers this machine, or not.
type hoUPSReq struct {
	ID string `json:"id"`
	On *bool  `json:"on"`
}

// hoPlugReq: the plug with this id (as GET /api/power names it, "<agent>/<device>") is this machine's, or
// not; it counts in the total, or not. A field that is not sent is not changed.
type hoPlugReq struct {
	ID      string `json:"id"`
	Linked  *bool  `json:"linked"`
	InTotal *bool  `json:"in_total"`
}

// hoResetReq: what the panel changed in this field of this agent's entry is taken out; config.yaml's value
// counts again. field: "ups" (ups_powers), "plugs" (power_links) or "total" (power_exclude_from_total).
type hoResetReq struct {
	Agent string `json:"agent"`
	Field string `json:"field"`
}

// hoFields maps the API's names of the three fields to config.yaml's, and to the words of a line.
var hoFields = map[string]struct{ field, words string }{
	"ups":   {config.FieldUPSPowers, "UPS links read by %s: back to config.yaml"},
	"plugs": {config.FieldPowerLinks, "Plug links read by %s: back to config.yaml"},
	"total": {config.FieldExclude, "Total of the plugs read by %s: back to config.yaml"},
}

// hoMachine is the machine of one request and what can be linked to it: the row it is shown as, the agent
// that row is, and every UPS and plug an agent relays. It is made from the poller's state before the store
// is locked; apply and view then only compute.
type hoMachine struct {
	row    model.System
	agent  string   // the agent this row is ("": a node without an agent)
	ids    []string // every id this machine is known under (its row's, its agent's, its node's)
	plugTo string   // the id a plug's link to this machine is written with
	ups    []hoUPS
	plugs  []hoPlug
	base   map[string]config.Links // reading agent -> the links of its entry (config.yaml)
	names  map[string]string       // reading agent -> the name of the row it is shown as
	shown  func(id string) string  // poller.ShownID
	rowOf  func(id string) string  // the name of the row with this id ("": none)
}

type hoUPS struct{ id, agent, name, label string }

type hoPlug struct {
	id, agent, dev, label string
	on                    string // the row the panel shows it on right now ("": none)
}

// hostOptionsMachine finds the machine a request is about and collects what can be linked to it. id is a
// row's id, or the id of the agent that row is.
func (c *reqCtx) hostOptionsMachine(id string) (*hoMachine, error) {
	p := c.s.Poller
	row, err := c.find(p.ShownID(id))
	if err != nil {
		return nil, apierr.New(404, "no such host")
	}
	if row.Kind != "node" && row.Kind != "agent" {
		return nil, apierr.New(409, "only hosts can be edited here (not a VM or a container)")
	}
	m := &hoMachine{row: row, plugTo: row.ID, base: map[string]config.Links{}, names: map[string]string{}, shown: p.ShownID}
	// The agent this row is: the agent with this id, or the one a node's row belongs to. Asked both ways, so
	// that a second agent that names the same node (it keeps a row of its own) is never taken for the node's.
	if a, ok := p.AgentFor(row.ID); ok && p.ShownID(a.ID) == row.ID {
		m.agent = a.ID
		// A plug's link takes either id of a machine (power.Manager.ShownID): the agent's is written, which stays
		// the machine's id when its node's row is not there (after a restart of the panel with the machine off).
		m.plugTo = a.ID
	}
	m.ids = append(m.ids, row.ID)
	for _, x := range append(p.SameMachine(row.ID), m.agent) {
		if x != "" && !slices.Contains(m.ids, x) && p.ShownID(x) == row.ID {
			m.ids = append(m.ids, x)
		}
	}
	rows := map[string]string{}
	for _, x := range p.Snapshot() {
		rows[x.ID] = x.Name
	}
	m.rowOf = func(id string) string { return rows[id] }
	reader := func(agentID string) bool {
		if _, known := m.base[agentID]; known {
			return true
		}
		l, ok := p.BaseLinks(agentID)
		if !ok {
			return false // relayed by something that is no agent of this panel: nothing can be stored for it
		}
		m.base[agentID] = l
		if m.names[agentID] = rows[p.ShownID(agentID)]; m.names[agentID] == "" {
			m.names[agentID] = agentID
		}
		return true
	}
	for _, u := range p.UPS.Overview(false).UPS {
		if u.Via == nil || !reader(*u.Via) {
			continue
		}
		name, ok := strings.CutPrefix(u.ID, *u.Via+"/")
		if !ok || !config.ValidUPSName(name) {
			continue
		}
		m.ups = append(m.ups, hoUPS{id: u.ID, agent: *u.Via, name: name, label: u.Name})
	}
	for _, st := range p.Power.Overview().Sources {
		if st.Via == nil || !reader(*st.Via) {
			continue
		}
		dev, ok := strings.CutPrefix(st.ID, *st.Via+"/")
		if !ok {
			continue
		}
		m.plugs = append(m.plugs, hoPlug{id: st.ID, agent: *st.Via, dev: dev, label: st.Name, on: model.Deref(st.SystemID)})
	}
	slices.SortFunc(m.ups, func(a, b hoUPS) int { return strings.Compare(a.label+"\x00"+a.id, b.label+"\x00"+b.id) })
	slices.SortFunc(m.plugs, func(a, b hoPlug) int { return strings.Compare(a.label+"\x00"+a.id, b.label+"\x00"+b.id) })
	return m, nil
}

// mine reports whether id is one of this machine's ids.
func (m *hoMachine) mine(id string) bool {
	return id != "" && (slices.Contains(m.ids, id) || m.shown(id) == m.row.ID)
}

// plugState is how a plug stands to this machine under the links l of its reading agent.
//
// byLink: power_links names this machine. self: no link is set and the panel shows the plug on a row all the
// same: the agent itself says the device is its own machine's (system_hint: self in the agent's own
// config), which the panel's entry cannot take away, only replace by a link to another machine. selfMine:
// and that machine is this one. to: the row it is linked to by power_links ("": none).
func (m *hoMachine) plugState(p hoPlug, l config.Links) (byLink, self, selfMine bool, to string) {
	to = l.PowerLinks[p.dev]
	byLink = m.mine(to)
	self = to == "" && p.on != ""
	selfMine = self && m.agent != "" && p.agent == m.agent
	return byLink, self, selfMine, to
}

// hoChange is one field of one agent's entry that a request changed: for the audit log.
type hoChange struct {
	Agent      string `json:"agent"`
	Field      string `json:"field"`
	Before     any    `json:"before"`
	After      any    `json:"after"`
	BeforeFrom string `json:"before_from"` // "config.yaml" or "panel"
	AfterFrom  string `json:"after_from"`
}

// apply makes the changes of req in o, for this machine, and returns one line for each thing that changes
// (the words of the dialog, of "Confirm it's you" and of the audit entry) and the changed fields. A tick that
// is already as asked changes nothing and has no line. An error is a refusal (422) and o is then not to be
// used.
func (m *hoMachine) apply(o *adminstate.HostOptions, req hostOptionsReq) (lines []string, changes []hoChange, err error) {
	if len(req.UPS) > maxHostOptionEntries || len(req.Plugs) > maxHostOptionEntries || len(req.Reset) > maxHostOptionEntries {
		return nil, nil, apierr.New(422, "too many entries in one request")
	}
	if o.Agents == nil {
		o.Agents = map[string]adminstate.HostOption{}
	}
	first := o.Clone()
	bad := func(format string, a ...any) error { return apierr.New(422, fmt.Sprintf(format, a...)) }
	// 1. resets: an override goes, config.yaml's value counts again
	seen := map[string]bool{}
	for _, r := range req.Reset {
		f, ok := hoFields[r.Field]
		if _, reads := m.base[r.Agent]; !ok || !reads {
			return nil, nil, bad("reset: no such field of a machine that reads devices")
		}
		if seen["reset\x00"+r.Agent+"\x00"+r.Field] {
			return nil, nil, bad("reset: an entry is there twice")
		}
		seen["reset\x00"+r.Agent+"\x00"+r.Field] = true
		e := o.Agents[r.Agent]
		if e.Reset(f.field) {
			o.Agents[r.Agent] = e
			lines = append(lines, fmt.Sprintf(f.words, m.names[r.Agent]))
		}
	}
	// 2. ticks, on a working copy of what counts for each reading agent after the resets
	work := map[string]*config.Links{}
	links := func(agent string) *config.Links {
		if l, ok := work[agent]; ok {
			return l
		}
		l := o.For(agent).Apply(m.base[agent])
		if l.UPSPowers == nil {
			l.UPSPowers = map[string][]string{}
		}
		if l.PowerLinks == nil {
			l.PowerLinks = map[string]string{}
		}
		work[agent] = &l
		return &l
	}
	dirty := map[string]map[string]bool{}
	touch := func(agent, field string) {
		if dirty[agent] == nil {
			dirty[agent] = map[string]bool{}
		}
		dirty[agent][field] = true
	}
	for _, r := range req.UPS {
		i := slices.IndexFunc(m.ups, func(u hoUPS) bool { return u.id == r.ID })
		if i < 0 || r.On == nil {
			return nil, nil, bad("ups: no such UPS read by an agent, or \"on\" is missing")
		}
		if seen["ups\x00"+r.ID] {
			return nil, nil, bad("ups: an entry is there twice")
		}
		seen["ups\x00"+r.ID] = true
		u := m.ups[i]
		l := links(u.agent)
		rows := l.UPSPowers[u.name]
		switch {
		case *r.On && !slices.Contains(rows, m.row.ID):
			// A UPS's rows are matched by their exact id (ups.Manager.SystemsMap), so the id of the row the machine
			// is shown as right now is written, as a hand-written config.yaml has it.
			l.UPSPowers[u.name] = append(slices.Clone(rows), m.row.ID)
			lines = append(lines, fmt.Sprintf("UPS %s (read by %s): powers %s", u.label, m.names[u.agent], m.row.Name))
			touch(u.agent, config.FieldUPSPowers)
		case !*r.On && slices.ContainsFunc(rows, m.mine):
			l.UPSPowers[u.name] = slices.DeleteFunc(slices.Clone(rows), m.mine) // under every id of the machine
			lines = append(lines, fmt.Sprintf("UPS %s (read by %s): no longer powers %s", u.label, m.names[u.agent], m.row.Name))
			touch(u.agent, config.FieldUPSPowers)
		}
	}
	for _, r := range req.Plugs {
		i := slices.IndexFunc(m.plugs, func(p hoPlug) bool { return p.id == r.ID })
		if i < 0 || (r.Linked == nil && r.InTotal == nil) {
			return nil, nil, bad("plugs: no such plug read by an agent, or neither \"linked\" nor \"in_total\" is given")
		}
		if seen["plug\x00"+r.ID] {
			return nil, nil, bad("plugs: an entry is there twice")
		}
		seen["plug\x00"+r.ID] = true
		p := m.plugs[i]
		l := links(p.agent)
		byLink, self, selfMine, to := m.plugState(p, *l)
		who := fmt.Sprintf("Plug %s (read by %s)", p.label, m.names[p.agent])
		if r.Linked != nil {
			switch {
			case *r.Linked && !byLink && !selfMine:
				was := m.rowOf(m.shown(to))
				if self {
					was = m.names[p.agent]
				}
				line := fmt.Sprintf("%s: linked to %s", who, m.row.Name)
				if was != "" {
					line += " (was on " + was + ")"
				}
				l.PowerLinks[p.dev] = m.plugTo
				lines = append(lines, line)
				touch(p.agent, config.FieldPowerLinks)
				byLink = true
			case !*r.Linked && byLink:
				delete(l.PowerLinks, p.dev)
				lines = append(lines, fmt.Sprintf("%s: no longer linked to %s", who, m.row.Name))
				touch(p.agent, config.FieldPowerLinks)
				byLink = false
			case !*r.Linked && selfMine:
				return nil, nil, bad("%s is linked by the machine itself (system_hint: self in the agent's config): it cannot be unlinked here", p.label)
			}
		}
		if r.InTotal != nil {
			// only for a plug of this machine: one it is linked to (after this request), or one its own agent reads
			if !byLink && !selfMine && (m.agent == "" || p.agent != m.agent) {
				return nil, nil, bad("%s is not a plug of %s: link it first, or change it on the page of the machine it belongs to", p.label, m.row.Name)
			}
			switch out := slices.Contains(l.PowerExcludeFromTotal, p.dev); {
			case !*r.InTotal && !out:
				l.PowerExcludeFromTotal = append(slices.Clone(l.PowerExcludeFromTotal), p.dev)
				lines = append(lines, who+": not in the total")
				touch(p.agent, config.FieldExclude)
			case *r.InTotal && out:
				l.PowerExcludeFromTotal = slices.DeleteFunc(slices.Clone(l.PowerExcludeFromTotal), func(d string) bool { return d == p.dev })
				lines = append(lines, who+": in the total again")
				touch(p.agent, config.FieldExclude)
			}
		}
	}
	// 3. what changed becomes the override of that field in the reading agent's entry, checked like every link
	for _, agent := range slices.Sorted(maps.Keys(dirty)) {
		e := o.Agents[agent]
		for _, field := range config.LinkFields {
			if dirty[agent][field] {
				e.Set(field, tidyLinks(*work[agent]))
			}
		}
		if p := config.LinkProblems(e.Links); len(p) > 0 {
			if len(config.LinkProblems(m.base[agent])) > 0 { // an override begins as a copy of config.yaml's value
				return nil, nil, bad("%s: %s (config.yaml has it in the entry of %s: correct it there first)", m.names[agent], p[0].Text, agent)
			}
			return nil, nil, bad("%s: %s", m.names[agent], p[0].Text)
		}
		o.Agents[agent] = e
	}
	// what differs from the start, field by field: for the audit log
	for _, agent := range slices.Sorted(maps.Keys(m.base)) {
		was, is := first.For(agent), o.For(agent)
		for _, field := range config.LinkFields {
			a, b := hoValue(was.Apply(m.base[agent]), field), hoValue(is.Apply(m.base[agent]), field)
			if was.Has(field) == is.Has(field) && fmt.Sprint(a) == fmt.Sprint(b) {
				continue
			}
			changes = append(changes, hoChange{Agent: agent, Field: field, Before: a, After: b, BeforeFrom: hoFrom(was.Has(field)), AfterFrom: hoFrom(is.Has(field))})
		}
	}
	return lines, changes, nil
}

// tidyLinks is l as an override is stored: without the entries that say nothing (a UPS that powers no row, a
// device linked to ""), and with the devices left out of the total in order.
func tidyLinks(l config.Links) config.Links {
	l = l.Clone()
	maps.DeleteFunc(l.UPSPowers, func(_ string, rows []string) bool { return len(rows) == 0 })
	maps.DeleteFunc(l.PowerLinks, func(_, to string) bool { return to == "" })
	slices.Sort(l.PowerExcludeFromTotal)
	l.PowerExcludeFromTotal = slices.Compact(l.PowerExcludeFromTotal)
	return l
}

func hoFrom(panel bool) string {
	if panel {
		return "panel"
	}
	return "config.yaml"
}

// hoValue is one field of l as the audit log shows it.
func hoValue(l config.Links, field string) any {
	switch field {
	case config.FieldUPSPowers:
		if l.UPSPowers == nil {
			return map[string][]string{}
		}
		return l.UPSPowers
	case config.FieldPowerLinks:
		if l.PowerLinks == nil {
			return map[string]string{}
		}
		return l.PowerLinks
	}
	if l.PowerExcludeFromTotal == nil {
		return []string{}
	}
	return l.PowerExcludeFromTotal
}

// view is the answer of GET, and of PUT after the change: what counts for this machine under o, and where
// each value comes from. from "config": config.yaml's value counts. from "panel": the field was changed
// in the panel for that reading agent, and config.yaml's value of it (config_...) is not used until Reset,
// whatever is written there later.
func (m *hoMachine) view(o adminstate.HostOptions, ignored []string) map[string]any {
	from := func(agent, field string) (string, any, any) {
		e := o.For(agent)
		if !e.Has(field) {
			return "config", nil, nil
		}
		var at any
		if !e.At.IsZero() {
			at = e.At.UTC().Format(time.RFC3339)
		}
		return "panel", strOrNil(e.By), at
	}
	ups := []map[string]any{}
	for _, u := range m.ups {
		eff, base := o.For(u.agent).Apply(m.base[u.agent]), m.base[u.agent]
		src, by, at := from(u.agent, config.FieldUPSPowers)
		ups = append(ups, map[string]any{"id": u.id, "name": u.label, "agent": u.agent, "agent_name": m.names[u.agent],
			"on": slices.Contains(eff.UPSPowers[u.name], m.row.ID), "from": src, "by": by, "at": at,
			"config_on": slices.Contains(base.UPSPowers[u.name], m.row.ID)})
	}
	plugs := []map[string]any{}
	for _, p := range m.plugs {
		eff, base := o.For(p.agent).Apply(m.base[p.agent]), m.base[p.agent]
		byLink, self, selfMine, to := m.plugState(p, eff)
		other := ""
		switch {
		case byLink || selfMine:
		case self:
			other = m.names[p.agent]
		case to != "":
			other = m.rowOf(m.shown(to))
		}
		src, by, at := from(p.agent, config.FieldPowerLinks)
		tsrc, tby, tat := from(p.agent, config.FieldExclude)
		plugs = append(plugs, map[string]any{"id": p.id, "name": p.label, "agent": p.agent, "agent_name": m.names[p.agent],
			"linked": byLink || selfMine, "self": selfMine, "other": strOrNil(other), "from": src, "by": by, "at": at,
			"config_linked": m.mine(base.PowerLinks[p.dev]),
			"in_total":      !slices.Contains(eff.PowerExcludeFromTotal, p.dev), "total_from": tsrc, "total_by": tby, "total_at": tat,
			"config_in_total": !slices.Contains(base.PowerExcludeFromTotal, p.dev),
			"mine":            byLink || selfMine || (m.agent != "" && p.agent == m.agent)})
	}
	if ignored == nil {
		ignored = []string{}
	}
	return map[string]any{"id": m.row.ID, "name": m.row.Name, "agent": strOrNil(m.agent), "rev": o.Rev,
		"ups": ups, "plugs": plugs, "ignored": ignored}
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// hostOptionsRead: who may open the dialog: an admin with a browser session (API tokens: 403).
func (c *reqCtx) hostOptionsRead() (*model.User, error) {
	u, err := c.requireSession()
	if err != nil {
		return nil, err
	}
	if _, g, _ := c.grant(); !g.Admin() {
		return nil, c.deny(u, 403, "only admins can edit a host", "not an admin")
	}
	return u, nil
}

func (s *Server) hostOptionsGet(c *reqCtx) error {
	if _, err := c.hostOptionsRead(); err != nil {
		return err
	}
	m, err := c.hostOptionsMachine(c.r.PathValue("id"))
	if err != nil {
		return err
	}
	o, ignored := s.Poller.Admin.HostOptionsRead()
	c.json(200, m.view(o, ignored))
	return nil
}

// hostOptionsItems is the panel's own line for each thing a kept request would change, for "Confirm it's
// you" (pending.go: actionList): the lines apply makes, on a copy of what is stored now.
func (c *reqCtx) hostOptionsItems(*model.User) []string {
	var req hostOptionsReq
	if !decodeStrictly(c.body, &req) {
		return nil
	}
	m, err := c.hostOptionsMachine(c.r.PathValue("id"))
	if err != nil {
		return nil
	}
	o := c.s.Poller.Admin.HostOptions().Clone()
	lines, _, err := m.apply(&o, req)
	if err != nil {
		return nil
	}
	return lines
}

func (s *Server) hostOptionsPut(c *reqCtx) error {
	if _, err := c.requireSession(); err != nil { // before the body is read (the guard checks the rest after it)
		return err
	}
	var req hostOptionsReq
	if err := c.decodeBody(&req); err != nil {
		return err
	}
	u, err := c.adminGuardFor("only admins can edit a host", "changes to a host")
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	m, err := c.hostOptionsMachine(id)
	if err != nil {
		c.audit(u, "host.options", id, nil, errorOutcome(err))
		return err
	}
	if len(req.UPS)+len(req.Plugs)+len(req.Reset) == 0 {
		return apierr.New(422, "nothing to change")
	}
	var lines []string
	var changes []hoChange
	// One locked read-modify-write: the ticks are applied to what state.json holds at that moment.
	after, changed, err := s.Poller.Admin.EditHostOptions(actorOf(u), func(o *adminstate.HostOptions) error {
		var err error
		lines, changes, err = m.apply(o, req)
		return err
	})
	params := map[string]any{"machine": m.row.Name, "agent": strOrNil(m.agent)}
	if err != nil {
		if _, refused := err.(*apierr.Error); refused {
			c.audit(u, "host.options", m.row.ID, params, errorOutcome(err))
			return err
		}
		c.audit(u, "host.options", m.row.ID, params, "failed: not saved")
		return err
	}
	out := m.view(after, nil)
	out["ok"], out["changed"] = true, []string{}
	if !changed || len(changes) == 0 { // every tick was already as asked
		out["message"] = "nothing to change"
		c.json(200, out)
		return nil
	}
	params["lines"], params["changes"], params["rev"] = lines, changes, after.Rev
	c.audit(u, "host.options", m.row.ID, params, "ok")
	if s.Notes != nil {
		s.Notes.Add(notifications.Event{Kind: "host_options", Type: "host", Severity: notifications.Info, SystemID: m.row.ID, Host: m.row.Name,
			Text: "links changed by " + actorOf(u)})
	}
	s.Poller.Bump()
	out["message"], out["changed"] = "links saved", lines
	c.json(200, out)
	return nil
}

// forgetHostOptions is the "Edit host" part of Remove (api_enroll.go): the overrides of the removed host's
// agent go, and so does every reference to one of its rows in the overrides of other agents. gone is what was
// taken out (nil: there was nothing).
func (s *Server) forgetHostOptions(by, agentID string, ids ...string) (gone []string, err error) {
	_, _, err = s.Poller.Admin.EditHostOptions(by, func(o *adminstate.HostOptions) error {
		gone = o.Forget(agentID, ids...)
		return nil
	})
	return gone, err
}
