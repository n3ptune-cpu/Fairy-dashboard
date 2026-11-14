package web

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
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
//	                                      "reset": [{"id", "field"}], "leftovers": true,
//	                                      "unrelayed": [{"id", "kind"}]}
//
// The request is about ONE machine, the one whose page is open, and says only what the admin changed (ticks),
// never a whole list: two admins who save at the same moment each change their own ticks on what the other
// left (adminstate.EditHostOptions), and a dialog that was open for an hour cannot put back what somebody
// else took out in the meantime.
//
// What is stored is as small as what is asked: one decision for each line that was changed ("Rack UPS powers
// PC-B", "the plug is linked to PC-B", "the plug is not in the total"), laid over config.yaml. A change made
// on the page of one machine says nothing about another machine: the lines nobody changed follow config.yaml,
// also when config.yaml is edited later. "Reset" takes the decision of one line out. The one way a change for
// this machine changes another is a plug: it has one place, so linking it here takes it from where it was, and
// the line says where that was.
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
		struct{ method, path, summary string }{"PUT", "/api/admin/hosts/{id}/options", "Edit host: change the links of one machine: {ups: [{id, on}], plugs: [{id, linked, in_total}], reset: [{id, field: ups|plugs|total}], leftovers: true, unrelayed: [{id, kind: ups|plug}]}; only what is named changes, for this machine (leftovers: the stored changes about machines that are gone are removed; unrelayed: the stored changes about these devices are removed, each only if its reading machine does not relay it right now). Stored as single decisions laid over config.yaml in state.json (config.yaml is not written; reset takes the decision of one line out), counts with the reading agent's next poll. Admins, browser session only (API tokens get 403), CSRF header, step-up. 422: an unknown field, UPS or plug; 409: not a host. Audited as host.options"})
}

func (s *Server) hostOptionsRoutes() {
	// What the reader leaves out of state.json is said in the log (once) when the panel starts: here, because
	// the poller asks for the overlay only when an agent answers, and on a panel none of whose agents answers
	// at the start nothing would say it until one does.
	_, _ = s.Poller.Admin.HostOptionsRead()
	s.mux.HandleFunc("GET /api/admin/hosts/{id}/options", s.h(s.hostOptionsGet))
	s.mux.HandleFunc("PUT /api/admin/hosts/{id}/options", s.h(s.hostOptionsPut))
}

// hostOptionsReq is everything "Edit host" can be asked to change.
type hostOptionsReq struct {
	UPS   []hoUPSReq   `json:"ups"`
	Plugs []hoPlugReq  `json:"plugs"`
	Reset []hoResetReq `json:"reset"`
	// Leftovers (true): the stored changes about machines that are gone are taken out (leftovers).
	Leftovers *bool `json:"leftovers"`
	// Unrelayed: the stored changes about these devices are taken out, each only if its reading machine does
	// not relay it right now (leftovers). Named one by one: what is removed is what the dialog listed.
	Unrelayed []hoAwayReq `json:"unrelayed"`
}

// hoAwayReq names a UPS (kind "ups") or a plug (kind "plug") by its id, "<reading agent>/<name>".
type hoAwayReq struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// check is everything about a request that can be said without looking at any machine: is there anything in
// it, is each entry whole, is no line named twice. A request that fails here can never be taken, whatever is
// stored, so the admin who sent it is told so (422) before being asked to confirm it with a passkey. Only an
// admin is: who may is asked first (hostOptionsPut).
func (r hostOptionsReq) check() error {
	bad := func(msg string) error { return apierr.New(422, msg) }
	if len(r.UPS)+len(r.Plugs)+len(r.Reset)+len(r.Unrelayed) == 0 && (r.Leftovers == nil || !*r.Leftovers) {
		return bad("nothing to change")
	}
	if len(r.UPS) > maxHostOptionEntries || len(r.Plugs) > maxHostOptionEntries || len(r.Reset) > maxHostOptionEntries || len(r.Unrelayed) > maxHostOptionEntries {
		return bad("too many entries in one request")
	}
	// a line is a field and a device: it can be reset or ticked once
	seen := map[string]bool{}
	once := func(field, id string) bool {
		k := field + "\x00" + id
		was := seen[k]
		seen[k] = true
		return !was
	}
	for _, e := range r.Unrelayed {
		if e.Kind != "ups" && e.Kind != "plug" {
			return bad("unrelayed: no such kind (ups or plug)")
		}
		if !once("unrelayed "+e.Kind, e.ID) {
			return bad("unrelayed: an entry is there twice")
		}
	}
	for _, e := range r.Reset {
		if _, ok := hoFields[e.Field]; !ok {
			return bad("reset: no such field (ups, plugs or total)")
		}
		if !once(e.Field, e.ID) {
			return bad("reset: an entry is there twice")
		}
	}
	for _, e := range r.UPS {
		if e.On == nil {
			return bad("ups: \"on\" is missing")
		}
		if !once("ups", e.ID) {
			return bad("ups: an entry is there twice")
		}
	}
	plugs := map[string]bool{}
	for _, e := range r.Plugs {
		if e.Linked == nil && e.InTotal == nil {
			return bad("plugs: neither \"linked\" nor \"in_total\" is given")
		}
		if plugs[e.ID] || (e.Linked != nil && !once("plugs", e.ID)) || (e.InTotal != nil && !once("total", e.ID)) {
			return bad("plugs: an entry is there twice")
		}
		plugs[e.ID] = true
	}
	return nil
}

// exactNames reports whether every name in a JSON body, in every object at every depth, is spelled as this
// API spells its fields (lower-case letters, digits, "_"). The decoder takes a field under any spelling of
// its name ("ID" for "id", "On" for "on"); strictBody refuses that for the outermost object of every route
// that can be kept for a step-up, and leaves the inner ones alone because other routes have free names there
// (a host's id as a key). This API has none: its ids are values.
func exactNames(b []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(b))
	var value func() bool
	value = func() bool {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		d, open := tok.(json.Delim)
		if !open {
			return true
		}
		for dec.More() {
			if d == '{' {
				name, err := dec.Token()
				if s, ok := name.(string); err != nil || !ok || !apiFieldRe.MatchString(s) {
					return false
				}
			}
			if !value() {
				return false
			}
		}
		_, err = dec.Token()
		return err == nil
	}
	return value()
}

// hostOptionsBody reads the request of a PUT: strict JSON that says each thing once (decodeBody), every name
// in it spelled exactly (exactNames), and a request that can be meant (check).
func (c *reqCtx) hostOptionsBody(req *hostOptionsReq) error {
	b, err := io.ReadAll(io.LimitReader(c.r.Body, 64<<10))
	if err != nil {
		return apierr.New(400, "unreadable body")
	}
	c.r.Body = io.NopCloser(bytes.NewReader(b))
	if err := c.decodeBody(req); err != nil {
		return err
	}
	if !exactNames(b) {
		return apierr.New(422, "invalid request body: a field is not spelled exactly as the API names it")
	}
	return req.check()
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

// hoResetReq: what the panel changed about this UPS or plug FOR THIS MACHINE is taken out; config.yaml counts
// for that line again. field: "ups" (does the UPS with this id power the machine), "plugs" (is the plug with
// this id linked to it) or "total" (does the plug count in the total).
type hoResetReq struct {
	ID    string `json:"id"`
	Field string `json:"field"`
}

// hoFields maps the API's names of the three fields to config.yaml's.
var hoFields = map[string]string{"ups": config.FieldUPSPowers, "plugs": config.FieldPowerLinks, "total": config.FieldExclude}

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
	// for leftovers: every agent of the panel with the name it is shown under; the agents that answered their
	// last poll; whether a row id that the stored changes name can be a row of this panel
	agents   map[string]string
	answered map[string]bool
	rowKnown map[string]bool
	// ... and the hosts added in the panel that this start left out (their tokens file is damaged: "Re-pair
	// it"), by their ids and the ids of their rows: they are machines of this panel, and nothing kept for
	// them is a leftover. unlisted: the list of added hosts cannot be read, so which ids are theirs is not known.
	waiting  map[string]bool
	unlisted bool
	// for who sees a device (seenBy): the site of each row ("": none, admins only), and the UPSes and plugs
	// that have a site of their own (the Users page), by their ids
	site  map[string]string
	fixed map[string]string
}

type hoUPS struct{ id, agent, name, label string }

type hoPlug struct {
	id, agent, dev, label string
	own                   bool // its agent says the device is its own machine's (system_hint: self), at its last answer
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
		m.plugs = append(m.plugs, hoPlug{id: st.ID, agent: *st.Via, dev: dev, label: st.Name, own: st.OwnHint})
	}
	// Who sees a UPS or a plug follows its links (view.go: followsLinks). The sites are read once, here.
	m.site, m.fixed = map[string]string{}, map[string]string{}
	grants := c.s.Access.Load()
	for id, ref := range c.s.sites(grants, p.Snapshot()) {
		m.site[id] = ref.Site
	}
	for _, u := range m.ups {
		m.fixed[u.id] = grants.Assign[access.UPSPrefix+u.id]
	}
	for _, pl := range m.plugs {
		m.fixed[pl.id] = grants.Assign[access.PowerPrefix+pl.id]
	}
	m.agents, m.answered, m.rowKnown = map[string]string{}, map[string]bool{}, map[string]bool{}
	for _, a := range p.Agents() {
		if m.agents[a.ID] = rows[p.ShownID(a.ID)]; m.agents[a.ID] == "" {
			m.agents[a.ID] = a.ID
		}
		info, _ := p.AgentInfo(a.ID)
		m.answered[a.ID] = info.Online
	}
	// A host added in the panel is a machine of it for as long as it is in managed-hosts.yaml, also at a start
	// that left it out. Its changes are not used meanwhile (the poller knows no such agent and no such row),
	// and they are never called gone: Re-pair brings the host back with them.
	hosts, err := c.s.Managed.Load()
	m.waiting, m.unlisted = map[string]bool{}, err != nil
	for _, h := range hosts {
		m.waiting[h.ID], m.waiting[h.Shown()] = true, true
	}
	for _, e := range p.Admin.HostOptions().Agents {
		for _, decided := range e.UPS {
			for id := range decided {
				m.rowKnown[id] = p.KnownRow(id)
			}
		}
		for _, to := range e.Links {
			if to != "" {
				m.rowKnown[to] = p.KnownRow(to)
			}
		}
	}
	slices.SortFunc(m.ups, func(a, b hoUPS) int { return strings.Compare(a.label+"\x00"+a.id, b.label+"\x00"+b.id) })
	slices.SortFunc(m.plugs, func(a, b hoPlug) int { return strings.Compare(a.label+"\x00"+a.id, b.label+"\x00"+b.id) })
	return m, nil
}

// mine reports whether id is one of this machine's ids.
func (m *hoMachine) mine(id string) bool {
	return id != "" && (slices.Contains(m.ids, id) || m.shown(id) == m.row.ID)
}

// A link decides who can SEE a device, and nothing else decides it for a device without a site of its own
// (view.go: followsLinks): a UPS is seen by the viewers of the sites of the rows it powers, a plug by those of
// the row it is linked to, and each, when it is linked to nothing, by those of the machine that reads it.
// The functions below say it for one device, as it would be under a list of rows or a link, so that the
// dialog and the API can say BEFORE a change is stored whose view it moves.

// sitesOf is the sites in a list (sorted, each once; "" is no site: admins only).
func sitesOf(sites ...string) []string {
	out := []string{}
	for _, s := range sites {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// upsSeen is the sites whose viewers see a UPS that powers rows (the ids as the links have them).
func (m *hoMachine) upsSeen(u hoUPS, rows []string) []string {
	if own := m.fixed[u.id]; own != "" {
		return sitesOf(own)
	}
	if len(rows) == 0 {
		return sitesOf(m.site[m.shown(u.agent)])
	}
	var sites []string
	for _, id := range rows {
		sites = append(sites, m.site[id])
	}
	return sitesOf(sites...)
}

// plugSeen is the sites whose viewers see a plug that is linked to the row to ("": to none; it is then its
// reading machine's, by what its agent says or as what relays it).
func (m *hoMachine) plugSeen(p hoPlug, to string) []string {
	if own := m.fixed[p.id]; own != "" {
		return sitesOf(own)
	}
	if to == "" {
		to = p.agent
	}
	return sitesOf(m.site[m.shown(to)])
}

// counted is the links of a reading agent under o as the poller hands them to the managers (poller.LinksOf):
// config.yaml with the decisions, without what a decision gives to a row that is no row of this panel.
func (m *hoMachine) counted(o adminstate.HostOptions, agent string) config.Links {
	e := o.For(agent)
	l := e.Apply(m.base[agent])
	gone := func(id string) bool { known, asked := m.rowKnown[id]; return asked && !known }
	for name, rows := range e.UPS {
		for id, on := range rows {
			if on && gone(id) {
				l.UPSPowers[name] = slices.DeleteFunc(l.UPSPowers[name], func(x string) bool { return x == id })
			}
		}
	}
	for dev, to := range e.Links {
		if to != "" && gone(to) {
			delete(l.PowerLinks, dev)
		}
	}
	return l
}

// visibility is one note for each device and each way a change from before to after moves who sees it: the
// sites whose viewers will see it and did not, and those who saw it and will not. nil: nobody's view moves.
func (m *hoMachine) visibility(before, after adminstate.HostOptions) (notes []string) {
	note := func(what string, was, is []string) {
		gain := slices.DeleteFunc(slices.Clone(is), func(s string) bool { return slices.Contains(was, s) })
		loss := slices.DeleteFunc(slices.Clone(was), func(s string) bool { return slices.Contains(is, s) })
		if len(gain) > 0 {
			notes = append(notes, fmt.Sprintf("Viewers of %s will see %s", strings.Join(gain, ", "), what))
		}
		if len(loss) > 0 {
			notes = append(notes, fmt.Sprintf("Viewers of %s will no longer see %s", strings.Join(loss, ", "), what))
		}
	}
	for _, u := range m.ups {
		note("UPS "+u.label, m.upsSeen(u, m.counted(before, u.agent).UPSPowers[u.name]), m.upsSeen(u, m.counted(after, u.agent).UPSPowers[u.name]))
	}
	for _, p := range m.plugs {
		note("plug "+p.label, m.plugSeen(p, m.counted(before, p.agent).PowerLinks[p.dev]), m.plugSeen(p, m.counted(after, p.agent).PowerLinks[p.dev]))
	}
	return notes
}

// maxLeftoverLines is how many leftovers are said one by one; the rest is counted ("N more of the same kind").
const maxLeftoverLines = 20

// hoLeft is the stored changes that have no line in any machine's dialog, and so could never be reset.
//
// gone, n: those about machines that are gone, in lines (at most maxLeftoverLines, then how many more) and
// as a number of changes:
//
//   - everything kept with an id that is no agent's (a host that was taken out of config.yaml). A host added
//     in the panel is not among them while it is in managed-hosts.yaml, also when this start left it out;
//   - a change that names a row which is no machine of this panel (the rows of an added host are, as above).
//
// away, awayN: those about a UPS or a plug that its reading agent does not relay RIGHT NOW, one entry for
// each device (at most maxLeftoverLines) and the number of all such changes. That is all the panel knows
// about such a device: the agent answered its last poll and the device was not in the answer. It may have
// been taken away; its NUT or Home Assistant may be down for a minute. So these are never called gone, never
// go with the others, and are taken out only when a request names the device. While the agent is silent
// nothing is known about its devices at all, and its changes are in neither list; they have their lines
// again when it answers.
//
// Nothing the running panel uses is among them: the poller reads none of these (poller.LinksOf), `fairy
// admin config check` has a WARN for the two kinds of gone.
type hoLeft struct {
	gone  []string
	n     int
	away  []hoAway
	awayN int
	// what a removal took out: its lines (as gone and away have them) and how many changes they were
	removed  []string
	removedN int
}

// hoAway is the stored changes about one device that is not relayed right now.
type hoAway struct {
	ID    string `json:"id"`   // "<reading agent>/<name>", as the device's id was while it was relayed
	Kind  string `json:"kind"` // "ups" or "plug"
	Count int    `json:"count"`
	Text  string `json:"text"`
}

// capLines is lines as the panel says a long list: the first maxLeftoverLines, then how many more.
func capLines(lines []string) []string {
	if more := len(lines) - maxLeftoverLines; more > 0 {
		return append(lines[:maxLeftoverLines:maxLeftoverLines], fmt.Sprintf("%d more of the same kind", more))
	}
	return lines
}

// leftovers finds them in o (hoLeft). With gone, those about machines that are gone are taken out of o; the
// changes about a device that is not relayed right now are taken out for the devices forget names, and for
// no other.
func (m *hoMachine) leftovers(o *adminstate.HostOptions, gone bool, forget []hoAwayReq) (left hoLeft) {
	count := func(k int) string {
		if k == 1 {
			return "1 stored change"
		}
		return fmt.Sprintf("%d stored changes", k)
	}
	relays := map[string]bool{}
	labels := map[string]string{}
	for _, u := range m.ups {
		relays["ups\x00"+u.agent+"\x00"+u.name], labels["ups\x00"+u.agent+"\x00"+u.name] = true, u.label
	}
	for _, p := range m.plugs {
		relays["plug\x00"+p.agent+"\x00"+p.dev], labels["plug\x00"+p.agent+"\x00"+p.dev] = true, p.label
	}
	noRow := func(id string) bool {
		known, asked := m.rowKnown[id]
		return asked && !known && !m.waiting[id] && !m.unlisted
	}
	var lines, late []string // late: the ids that are no agent's, after the others, which name machines the admin knows
	for _, agent := range slices.Sorted(maps.Keys(o.Agents)) {
		e := o.Agents[agent].Clone()
		name, isAgent := m.agents[agent]
		if !isAgent && (m.waiting[agent] || m.unlisted) {
			continue // an added host that waits for Re-pair (or may be one): kept as it is
		}
		if !isAgent {
			if k := e.Count(); k > 0 {
				left.n += k
				late = append(late, fmt.Sprintf("%s kept with %s, which is no machine of this panel", count(k), agent))
			}
			if gone {
				o.Drop(agent) // the entry whole: also what this build does not know in it
			}
			continue
		}
		dropped := false // something of e is taken out by this call
		// a device of this agent that is not in its last answer: listed, and kept unless the request names it
		away := func(kind, dev string, k int) (drop bool) {
			x := hoAway{ID: agent + "/" + dev, Kind: kind, Count: k}
			if kind == "ups" {
				x.Text = fmt.Sprintf("%s about the UPS %s, which %s does not relay right now", count(k), dev, name)
			} else {
				x.Text = fmt.Sprintf("%s about the plug %s, which %s does not relay right now", count(k), dev, name)
			}
			if slices.Contains(forget, hoAwayReq{ID: x.ID, Kind: kind}) {
				left.removed, left.removedN = append(left.removed, x.Text), left.removedN+k
				dropped = true
				return true
			}
			left.away, left.awayN = append(left.away, x), left.awayN+k
			return false
		}
		notRelayed := func(kind, id string) bool { return m.answered[agent] && !relays[kind+"\x00"+agent+"\x00"+id] }
		for _, ups := range slices.Sorted(maps.Keys(e.UPS)) {
			if notRelayed("ups", ups) {
				if away("ups", ups, len(e.UPS[ups])) {
					delete(e.UPS, ups)
				}
				continue
			}
			for _, row := range slices.Sorted(maps.Keys(e.UPS[ups])) {
				if noRow(row) {
					left.n++
					lines = append(lines, fmt.Sprintf("UPS %s (read by %s): a stored change about %s, which is no machine of this panel", cmp.Or(labels["ups\x00"+agent+"\x00"+ups], ups), name, row))
					if gone {
						dropped = e.DropUPS(ups, row) || dropped
					}
				}
			}
		}
		devs := map[string]int{}
		for _, dev := range slices.Sorted(maps.Keys(e.Links)) {
			switch to := e.Links[dev]; {
			case notRelayed("plug", dev):
				devs[dev]++
			case to != "" && noRow(to):
				left.n++
				lines = append(lines, fmt.Sprintf("Plug %s (read by %s): a stored link to %s, which is no machine of this panel", cmp.Or(labels["plug\x00"+agent+"\x00"+dev], dev), name, to))
				if gone {
					e.DropLink(dev)
					dropped = true
				}
			}
		}
		for _, dev := range slices.Sorted(maps.Keys(e.Exclude)) {
			if notRelayed("plug", dev) {
				devs[dev]++
			}
		}
		for _, dev := range slices.Sorted(maps.Keys(devs)) {
			if away("plug", dev, devs[dev]) {
				e.DropLink(dev)
				e.DropExclude(dev)
			}
		}
		switch {
		case !dropped: // the entry stays as it is
		case e.Empty():
			delete(o.Agents, agent)
		default:
			o.Agents[agent] = e
		}
	}
	lines = append(lines, late...)
	if gone { // they were taken out above: first in what was removed, and no longer there
		left.removed, left.removedN = append(lines, left.removed...), left.removedN+left.n
		lines, left.n = nil, 0
	}
	left.gone, left.removed = capLines(lines), capLines(left.removed)
	if len(left.away) > maxLeftoverLines {
		left.away = left.away[:maxLeftoverLines:maxLeftoverLines]
	}
	return left
}

// plugState is how a plug stands to this machine under the links l of its reading agent.
//
// byLink: power_links names this machine. self: no link is set and the plug is its reading agent's all the
// same: the agent itself says the device is its own machine's (system_hint: self in the agent's own
// config), which the panel's entry cannot take away, only replace by a link to another machine. selfMine:
// and that machine is this one. to: the row it is linked to by power_links ("": none).
//
// It is made from the links (config.yaml with the panel's changes) and from what the agent says about the
// device, never from the row the panel shows the plug on: that row is the link as it was at the reading
// agent's last answer, and stays so for as long as that agent is silent or its polling is paused.
func (m *hoMachine) plugState(p hoPlug, l config.Links) (byLink, self, selfMine bool, to string) {
	to = l.PowerLinks[p.dev]
	byLink = m.mine(to)
	self = to == "" && p.own
	selfMine = self && m.agent != "" && p.agent == m.agent
	return byLink, self, selfMine, to
}

// where is the machine a plug is on under the links l, as a line names it: "" when that is this machine, or
// none.
func (m *hoMachine) where(p hoPlug, l config.Links) string {
	switch byLink, self, selfMine, to := m.plugState(p, l); {
	case byLink || selfMine:
		return ""
	case self:
		return m.names[p.agent]
	default:
		return m.rowOf(m.shown(to))
	}
}

// concerns reports whether the decision about a plug's link (there is one: to) is about this machine: it
// gives the plug to this machine, or takes it from it (config.yaml links it here). Only such a decision is
// "changed here" on this machine's page, and only such a one can be reset there.
func (m *hoMachine) concerns(p hoPlug, to string) bool {
	byLink, _, selfMine, _ := m.plugState(p, m.base[p.agent])
	return m.mine(to) || byLink || selfMine
}

// hoChange is one field of one agent's entry that a request changed: for the audit log. Before and After
// are what counts for that agent (config.yaml with the decisions), BeforeFrom and AfterFrom whether a
// decision of the panel is part of it ("panel") or it is config.yaml's alone.
type hoChange struct {
	Agent      string `json:"agent"`
	Field      string `json:"field"`
	Before     any    `json:"before"`
	After      any    `json:"after"`
	BeforeFrom string `json:"before_from"` // "config.yaml" or "panel"
	AfterFrom  string `json:"after_from"`
	Stored     any    `json:"stored"` // the panel's decisions in this field afterwards (nil: none)
}

// The words of a line, which the dialog makes the same way (app.js: ehPlan; a test holds the two together).
const hoBack = "back to config.yaml: "

func hoUPSWords(on bool, machine string) string {
	if on {
		return "powers " + machine
	}
	return "no longer powers " + machine
}

func hoLinkWords(linked bool, machine, was, then string) string {
	switch {
	case linked && was != "":
		return "linked to " + machine + " (was on " + was + ")"
	case linked:
		return "linked to " + machine
	case then != "":
		return "no longer linked to " + machine + " (on " + then + " again)"
	}
	return "no longer linked to " + machine
}

func hoTotalWords(in bool) string {
	if in {
		return "in the total again"
	}
	return "not in the total"
}

// apply makes the changes of req in o, for this machine, and returns one line for each thing that changes
// (the words of the dialog, of "Confirm it's you" and of the audit entry) and the changed fields. A tick that
// is already as asked changes nothing and has no line. An error is a refusal (422) and o is then not to be
// used.
//
// A tick becomes one decision in the entry of the agent that reads the device, about this machine's line
// only. A tick that puts a line back to what config.yaml says takes the decision out instead: what is stored
// is where the panel differs from config.yaml, and nothing else.
func (m *hoMachine) apply(o *adminstate.HostOptions, req hostOptionsReq) (lines []string, changes []hoChange, err error) {
	if err := req.check(); err != nil {
		return nil, nil, err
	}
	if o.Agents == nil {
		o.Agents = map[string]adminstate.HostOption{}
	}
	first := o.Clone()
	bad := func(format string, a ...any) error { return apierr.New(422, fmt.Sprintf(format, a...)) }
	gone := func(what, id string) error {
		return bad("no agent relays the %s %q (any more): close this and open it again", what, truncate(safeTarget(id), 80))
	}
	notMine := func(p hoPlug) error {
		return bad("%s is not a plug of %s: link it first, or change it on the page of the machine it belongs to", p.label, m.row.Name)
	}
	// the entry of a reading agent, as this request leaves it (a copy until the end)
	work := map[string]*adminstate.HostOption{}
	entry := func(agent string) *adminstate.HostOption {
		if e, ok := work[agent]; ok {
			return e
		}
		e := o.For(agent).Clone()
		work[agent] = &e
		return &e
	}
	links := func(agent string) config.Links { return entry(agent).Apply(m.base[agent]) }
	isMine := func(p hoPlug, l config.Links) bool {
		byLink, _, selfMine, _ := m.plugState(p, l)
		return byLink || selfMine || (m.agent != "" && p.agent == m.agent)
	}
	// 0. the leftovers go first: nothing below is about them (they have no line)
	if all := req.Leftovers != nil && *req.Leftovers; all || len(req.Unrelayed) > 0 {
		for _, l := range m.leftovers(o, all, req.Unrelayed).removed {
			lines = append(lines, "Removed: "+l)
		}
	}
	// 1. resets: the decision of one line goes, config.yaml counts for it again
	for _, r := range req.Reset {
		field := hoFields[r.Field]
		if field == config.FieldUPSPowers {
			i := slices.IndexFunc(m.ups, func(u hoUPS) bool { return u.id == r.ID })
			if i < 0 {
				return nil, nil, gone("UPS", r.ID)
			}
			u := m.ups[i]
			e := entry(u.agent)
			was, had := slices.Contains(links(u.agent).UPSPowers[u.name], m.row.ID), false
			for _, id := range slices.Sorted(maps.Keys(e.UPS[u.name])) {
				if m.mine(id) {
					had = e.DropUPS(u.name, id) || had
				}
			}
			if !had {
				continue
			}
			words := "nothing changes now"
			if is := slices.Contains(links(u.agent).UPSPowers[u.name], m.row.ID); is != was {
				words = hoUPSWords(is, m.row.Name)
			}
			lines = append(lines, fmt.Sprintf("UPS %s (read by %s): %s%s", u.label, m.names[u.agent], hoBack, words))
			continue
		}
		i := slices.IndexFunc(m.plugs, func(p hoPlug) bool { return p.id == r.ID })
		if i < 0 {
			return nil, nil, gone("plug", r.ID)
		}
		p := m.plugs[i]
		e := entry(p.agent)
		who := fmt.Sprintf("Plug %s (read by %s): %s", p.label, m.names[p.agent], hoBack)
		if field == config.FieldPowerLinks {
			to, has := e.Links[p.dev]
			if !has || !m.concerns(p, to) {
				continue
			}
			before := links(p.agent)
			wasLinked, _, wasSelf, _ := m.plugState(p, before)
			e.DropLink(p.dev)
			after := links(p.agent)
			isLinked, _, isSelf, _ := m.plugState(p, after)
			words := "nothing changes now"
			if was, is := wasLinked || wasSelf, isLinked || isSelf; was != is {
				words = hoLinkWords(is, m.row.Name, m.where(p, before), m.where(p, after))
			}
			lines = append(lines, who+words)
			continue
		}
		if !isMine(p, links(p.agent)) {
			return nil, nil, notMine(p)
		}
		was := slices.Contains(links(p.agent).PowerExcludeFromTotal, p.dev)
		if !e.DropExclude(p.dev) {
			continue
		}
		words := "nothing changes now"
		if is := slices.Contains(links(p.agent).PowerExcludeFromTotal, p.dev); is != was {
			words = hoTotalWords(!is)
		}
		lines = append(lines, who+words)
	}
	// 2. ticks
	for _, r := range req.UPS {
		i := slices.IndexFunc(m.ups, func(u hoUPS) bool { return u.id == r.ID })
		if i < 0 {
			return nil, nil, gone("UPS", r.ID)
		}
		u := m.ups[i]
		e, base := entry(u.agent), m.base[u.agent].UPSPowers[u.name]
		// decide is the decision about one of this machine's ids, or none where config.yaml says the same
		decide := func(id string, on bool) {
			if slices.Contains(base, id) == on {
				e.DropUPS(u.name, id)
			} else {
				e.SetUPS(u.name, id, on)
			}
		}
		rows := links(u.agent).UPSPowers[u.name]
		switch {
		case *r.On && !slices.Contains(rows, m.row.ID):
			// A UPS's rows are matched by their exact id (ups.Manager.SystemsMap), so the id of the row the machine
			// is shown as right now is decided about, as a hand-written config.yaml has it.
			decide(m.row.ID, true)
		case !*r.On && slices.ContainsFunc(rows, m.mine):
			for _, id := range rows {
				if m.mine(id) { // under every id of the machine
					decide(id, false)
				}
			}
		default:
			continue
		}
		lines = append(lines, fmt.Sprintf("UPS %s (read by %s): %s", u.label, m.names[u.agent], hoUPSWords(*r.On, m.row.Name)))
	}
	for _, r := range req.Plugs {
		i := slices.IndexFunc(m.plugs, func(p hoPlug) bool { return p.id == r.ID })
		if i < 0 {
			return nil, nil, gone("plug", r.ID)
		}
		p := m.plugs[i]
		e, base := entry(p.agent), m.base[p.agent]
		who := fmt.Sprintf("Plug %s (read by %s): ", p.label, m.names[p.agent])
		if r.Linked != nil {
			before := links(p.agent)
			switch byLink, _, selfMine, _ := m.plugState(p, before); {
			case *r.Linked && !byLink && !selfMine:
				// where config.yaml (or, with no link in it, the agent itself) already gives the plug to this machine,
				// that counts again; else the link is decided here
				if cfgLink, _, cfgSelf, _ := m.plugState(p, base); cfgLink || cfgSelf {
					e.DropLink(p.dev)
				} else {
					e.SetLink(p.dev, m.plugTo)
				}
				lines = append(lines, who+hoLinkWords(true, m.row.Name, m.where(p, before), ""))
			case !*r.Linked && byLink:
				if base.PowerLinks[p.dev] == "" {
					e.DropLink(p.dev)
				} else {
					e.SetLink(p.dev, "") // linked to none, whatever config.yaml says
				}
				lines = append(lines, who+hoLinkWords(false, m.row.Name, "", ""))
			case !*r.Linked && selfMine:
				return nil, nil, bad("%s is linked by the machine itself (system_hint: self in the agent's config): it cannot be unlinked here", p.label)
			}
		}
		if r.InTotal != nil {
			// only for a plug of this machine: one it is linked to (after this request), or one its own agent reads
			l := links(p.agent)
			if !isMine(p, l) {
				return nil, nil, notMine(p)
			}
			if out := slices.Contains(l.PowerExcludeFromTotal, p.dev); out == *r.InTotal {
				if slices.Contains(base.PowerExcludeFromTotal, p.dev) == !*r.InTotal {
					e.DropExclude(p.dev)
				} else {
					e.SetExclude(p.dev, !*r.InTotal)
				}
				lines = append(lines, who+hoTotalWords(*r.InTotal))
			}
		}
	}
	// 3. what was decided is checked like every link: the names and ids of the decisions themselves, and what
	// counts with them (config.yaml's own findings aside: those are config.yaml's, and stop nothing here)
	for _, agent := range slices.Sorted(maps.Keys(work)) {
		e := work[agent].Clone()
		if o.For(agent).Same(e) {
			continue
		}
		problems := config.LinkProblems(e.Named())
		if len(problems) == 0 {
			own := config.LinkProblems(m.base[agent])
			problems = slices.DeleteFunc(config.LinkProblems(e.Apply(m.base[agent])), func(p config.LinkProblem) bool {
				return slices.ContainsFunc(own, func(b config.LinkProblem) bool { return b.Text == p.Text })
			})
		}
		if len(problems) > 0 {
			return nil, nil, bad("%s: %s", m.names[agent], problems[0].Text)
		}
		if e.Empty() {
			delete(o.Agents, agent)
		} else {
			o.Agents[agent] = e
		}
	}
	// what differs from the start, field by field: for the audit log
	for _, agent := range slices.Sorted(maps.Keys(m.base)) {
		was, is := first.For(agent), o.For(agent)
		for _, field := range config.LinkFields {
			a, b := hoValue(was.Apply(m.base[agent]), field), hoValue(is.Apply(m.base[agent]), field)
			if fmt.Sprint(hoStored(was, field)) == fmt.Sprint(hoStored(is, field)) {
				continue
			}
			changes = append(changes, hoChange{Agent: agent, Field: field, Before: a, After: b, BeforeFrom: hoFrom(was.Has(field)), AfterFrom: hoFrom(is.Has(field)),
				Stored: hoStored(is, field)})
		}
	}
	return lines, changes, nil
}

func hoFrom(panel bool) string {
	if panel {
		return "panel"
	}
	return "config.yaml"
}

// hoStored is the decisions of one field as the audit log shows them (nil: none).
func hoStored(e adminstate.HostOption, field string) any {
	switch e = e.Clone(); {
	case !e.Has(field):
		return nil
	case field == config.FieldUPSPowers:
		return e.UPS
	case field == config.FieldPowerLinks:
		return e.Links
	}
	return e.Exclude
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
// each line's value comes from. from "config": config.yaml's value counts for the line. from "panel": the
// line was changed in the panel, and what config.yaml says about it (config_...) is not used until Reset,
// whatever is written there later. by and at are who last changed something about the devices that agent
// reads, and when (they are kept for each reading agent, not for each line).
func (m *hoMachine) view(o adminstate.HostOptions, ignored []string, ignoredCount int) map[string]any {
	who := func(agent string, panel bool) (string, any, any) {
		e := o.For(agent)
		if !panel {
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
		e, base := o.For(u.agent), m.base[u.agent]
		eff := e.Apply(base)
		decided := false
		for id := range e.UPS[u.name] {
			decided = decided || m.mine(id)
		}
		src, by, at := who(u.agent, decided)
		// who sees it now, and who would with this machine on it and off it
		rows := m.counted(o, u.agent).UPSPowers[u.name]
		others := slices.DeleteFunc(slices.Clone(rows), m.mine)
		ups = append(ups, map[string]any{"id": u.id, "name": u.label, "agent": u.agent, "agent_name": m.names[u.agent],
			"on": slices.Contains(eff.UPSPowers[u.name], m.row.ID), "from": src, "by": by, "at": at,
			"config_on": slices.Contains(base.UPSPowers[u.name], m.row.ID),
			"sees":      m.upsSeen(u, rows), "sees_on": m.upsSeen(u, append(slices.Clone(others), m.row.ID)), "sees_off": m.upsSeen(u, others)})
	}
	plugs := []map[string]any{}
	for _, p := range m.plugs {
		e, base := o.For(p.agent), m.base[p.agent]
		eff := e.Apply(base)
		byLink, _, selfMine, _ := m.plugState(p, eff)
		cfgLink, _, cfgSelf, _ := m.plugState(p, base)
		to, decided := e.Links[p.dev]
		src, by, at := who(p.agent, decided && m.concerns(p, to))
		_, decided = e.Exclude[p.dev]
		tsrc, tby, tat := who(p.agent, decided)
		plugs = append(plugs, map[string]any{"id": p.id, "name": p.label, "agent": p.agent, "agent_name": m.names[p.agent],
			"linked": byLink || selfMine, "self": selfMine, "other": strOrNil(m.where(p, eff)), "from": src, "by": by, "at": at,
			"config_linked": cfgLink || cfgSelf, "config_other": strOrNil(m.where(p, base)),
			// who sees it now, and who would: linked to this machine, unlinked from it, and after a Reset of the line
			"sees": m.plugSeen(p, m.counted(o, p.agent).PowerLinks[p.dev]), "sees_linked": m.plugSeen(p, m.plugTo), "sees_unlinked": m.plugSeen(p, ""),
			"sees_config": m.plugSeen(p, base.PowerLinks[p.dev]),
			"in_total":    !slices.Contains(eff.PowerExcludeFromTotal, p.dev), "total_from": tsrc, "total_by": tby, "total_at": tat,
			"config_in_total": !slices.Contains(base.PowerExcludeFromTotal, p.dev),
			"mine":            byLink || selfMine || (m.agent != "" && p.agent == m.agent)})
	}
	if ignored == nil {
		ignored = []string{}
	}
	left := m.leftovers(&o, false, nil)
	if left.gone == nil {
		left.gone = []string{}
	}
	if left.away == nil {
		left.away = []hoAway{}
	}
	return map[string]any{"id": m.row.ID, "name": m.row.Name, "agent": strOrNil(m.agent), "site": strOrNil(m.site[m.row.ID]), "rev": o.Rev,
		"ups": ups, "plugs": plugs, "ignored": ignored, "ignored_count": ignoredCount, "leftovers": left.gone, "leftover_count": left.n,
		"unrelayed": left.away, "unrelayed_count": left.awayN}
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
	c.json(200, m.view(o, ignored, o.Ignored))
	return nil
}

// hostOptionsItems is the panel's own line for each thing a kept request would change, for "Confirm it's
// you" (pending.go: actionList): the lines apply makes, on a copy of what is stored now.
func (c *reqCtx) hostOptionsItems(*model.User) []string {
	var req hostOptionsReq
	if !decodeStrictly(c.body, &req) || !exactNames(c.body) {
		return nil
	}
	m, err := c.hostOptionsMachine(c.r.PathValue("id"))
	if err != nil {
		return nil
	}
	o := c.s.Poller.Admin.HostOptions().Clone()
	before := o.Clone()
	lines, _, err := m.apply(&o, req)
	if err != nil {
		return nil
	}
	return append(lines, m.visibility(before, o)...) // and whose view of a device it moves
}

func (s *Server) hostOptionsPut(c *reqCtx) error {
	const denied = "only admins can edit a host"
	// Who asks comes first: a browser session, the page's header, an admin. Nobody else is told anything about
	// what they sent.
	if _, err := c.adminSession(denied); err != nil {
		return err
	}
	var req hostOptionsReq
	if err := c.hostOptionsBody(&req); err != nil { // its form, before the admin is asked to confirm it
		return err
	}
	u, err := c.adminGuardFor(denied, "changes to a host") // the same checks again, then the confirmation and the rate limit
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	m, err := c.hostOptionsMachine(id)
	if err != nil {
		c.audit(u, "host.options", id, nil, errorOutcome(err))
		return err
	}
	var lines, notes []string
	var changes []hoChange
	// One locked read-modify-write: the ticks are applied to what state.json holds at that moment.
	after, changed, err := s.Poller.Admin.EditHostOptions(actorOf(u), func(o *adminstate.HostOptions) error {
		before := o.Clone()
		var err error
		if lines, changes, err = m.apply(o, req); err == nil {
			notes = m.visibility(before, *o)
		}
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
	out := m.view(after, nil, 0)
	out["ok"], out["changed"], out["visibility"] = true, []string{}, []string{}
	if !changed { // every tick was already as asked
		out["message"] = "nothing to change"
		c.json(200, out)
		return nil
	}
	params["lines"], params["changes"], params["rev"] = lines, changes, after.Rev
	if len(notes) > 0 {
		params["visibility"], out["visibility"] = notes, notes // whose view of a device the change moves
	}
	c.audit(u, "host.options", m.row.ID, params, "ok")
	if s.Notes != nil {
		s.Notes.Add(notifications.Event{Kind: "host_options", Type: "host", Severity: notifications.Info, SystemID: m.row.ID, Host: m.row.Name,
			Text: "links changed by " + actorOf(u)})
	}
	// Nothing of the poller's picture has changed yet, so nothing is told to rebuild it: the links reach the
	// rows with the reading agent's next answer, and that poll says so itself.
	out["message"], out["changed"] = "links saved", lines
	c.json(200, out)
	return nil
}

// rowsOf is the rows that come and go with a host added with Add host, besides the row of its agent's own
// id: its node's row, when the node's proxmox: entry is the host's own (Add host "a new node"; Remove takes
// that entry out). The row of a node the panel had before the host (a proxmox: entry of config.yaml, on which
// the host was added) is not the host's: it stays when the host is removed, and what was changed about it
// (the UPS that powers the node) stays with it.
func rowsOf(h managed.Host) []string {
	if h.Proxmox != nil && h.Shown() != h.ID {
		return []string{h.Shown()}
	}
	return nil
}

// forgetHostOptions is the "Edit host" part of Remove (api_enroll.go): the overrides of the removed host's
// agent go, and so does every decision about one of its rows in the overrides of other agents. gone is what
// was taken out (nil: there was nothing).
func (s *Server) forgetHostOptions(by, agentID string, ids ...string) (gone []string, err error) {
	_, _, err = s.Poller.Admin.EditHostOptions(by, func(o *adminstate.HostOptions) error {
		gone = o.Forget(agentID, ids...)
		return nil
	})
	return gone, err
}
