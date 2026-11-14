package web

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/configutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// "Edit host", the machine itself (stage 1b): its name, its site and its Wake-on-LAN details, in the dialog
// and the route of api_hostoptions.go.
//
//	PUT /api/admin/hosts/{id}/options   ... "name": "Gaming PC", "site": "site-b" | "",
//	                                         "wol": {"macs": [...], "relay": "<agent id>", "port": 7 | 9} | {"off": true},
//	                                         "reset": [{"field": "name"}, {"field": "wol"}]
//
// Where each is kept:
//
//   - name and wol: decisions in state.json next to the link decisions (adminstate.HostEdit), laid over the
//     configuration. A value that is the configuration's is not stored; Reset takes a decision out.
//   - site: the assignment of the Users page (users.json, access.Store.Assign), not a second store. "" takes
//     the assignment out (the default counts: config.yaml's site, or none), and so does the default itself.
//
// What can be sent is a closed set. Of a host's Wake-on-LAN only three things can be changed: which network
// cards the packet is for (MAC addresses), which of the agents that ALREADY may send wake packets sends it,
// and the port (7 or 9). The address a packet is sent to (broadcast) and the password that goes with it
// (password_env, an environment variable of the panel) have no field: with them a request could make an agent
// send packets to an address of its choice, or hand a secret of the panel's environment to an agent. They are
// shown read-only. wol_relay itself (may this agent send wake packets at all) is set on the machine or in
// config.yaml, as before.
//
// Who may: as for the links, and for anything about Wake-on-LAN a confirmation of its own (stepUpEach): a
// wake packet for a MAC of the admin's choice is something the buttons of the panel could not do before, so
// a confirmation given for something else a minute ago does not count for it.

// hoWolReq is what a request says a host's Wake-on-LAN shall be. A field that is not sent is not changed.
type hoWolReq struct {
	MACs  *[]string `json:"macs"`
	Relay *string   `json:"relay"`
	Port  *int      `json:"port"`
	Off   *bool     `json:"off"` // true: no Wake-on-LAN for this host (then nothing else is sent)
}

// hostSet reports whether the request says anything about the machine itself.
func (r hostOptionsReq) hostSet() bool { return r.Name != nil || r.Site != nil || r.Wol != nil }

func (r hostOptionsReq) resets(field string) bool {
	return slices.ContainsFunc(r.Reset, func(e hoResetReq) bool { return e.Field == field })
}

// touchesWol reports whether the request changes anything about Wake-on-LAN (it then needs its own
// confirmation).
func (r hostOptionsReq) touchesWol() bool { return r.Wol != nil || r.resets("wol") }

// checkHost is the part of check about the machine itself: the form of a name, a site and the Wake-on-LAN
// details, before anybody is asked to confirm anything.
func (r hostOptionsReq) checkHost() error {
	bad := func(msg string) error { return apierr.New(422, msg) }
	if r.Name != nil {
		switch name, err := adminstate.HostName(*r.Name); {
		case err != nil:
			return bad(err.Error())
		case name == "":
			return bad("name: type a name (Reset puts back the name from config.yaml)")
		case r.resets("name"):
			return bad("name: a new name and a reset at once")
		}
	}
	if r.Site != nil && *r.Site != "" && !config.ValidSite(*r.Site) {
		return bad("site: no such site")
	}
	if w := r.Wol; w != nil {
		switch {
		case r.resets("wol"):
			return bad("wol: a change and a reset at once")
		case w.MACs == nil && w.Relay == nil && w.Port == nil && w.Off == nil:
			return bad("wol: nothing is given")
		case w.Off != nil && !*w.Off:
			return bad("wol: \"off\" can only be true (Reset puts back the Wake-on-LAN from config.yaml)")
		case w.Off != nil && (w.MACs != nil || w.Relay != nil || w.Port != nil):
			return bad("wol: \"off\" stands alone")
		}
		if w.MACs != nil {
			if _, err := adminstate.WolMACs(*w.MACs); err != nil {
				return bad("wol: " + err.Error())
			}
		}
		if w.Relay != nil && !configutil.ValidID(*w.Relay) {
			return bad("wol: no such relay")
		}
		if w.Port != nil && *w.Port != 7 && *w.Port != 9 {
			return bad("wol: the port must be 7 or 9")
		}
	}
	return nil
}

// hoHost is what is known about the machine of a request itself, collected before the store is locked.
type hoHost struct {
	key      string                                                                      // the id its decisions are kept under
	baseName string                                                                      // the row's name without the panel's
	baseWol  *config.Wol                                                                 // the configuration's Wake-on-LAN (nil: none)
	relays   []hoRelay                                                                   // the agents that may send wake packets, this machine aside
	others   []hoOther                                                                   // the other hosts, for whose names this one must not be taken
	guests   []hoOther                                                                   // ... and the guests (their names are typed to confirm a shutdown or a stop too)
	mine     func(id string) bool                                                        // whether something kept under id is about this machine
	wolWith  func(base *config.Wol, e adminstate.WolEdit) (w *config.Wol, unused string) // poller.WolWith for this machine
}

// hoOther is another host (or a guest) as far as its name goes: the row it is shown as, the ids under which
// a name given in the panel is kept for it, and the names the configuration has for it (the row's name
// without the panel's, and its agent's name: the one it is shown under after a Reset, the other after a
// restart of the panel while the machine is off).
type hoOther struct {
	row    string
	ids    []string
	names  []string
	absent bool // a host of the Add host list that is not polled right now: it has no row, and comes back under its name
}

type hoRelay struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
}

// hostOptionsHost collects hoHost for the machine m is about.
func (s *Server) hostOptionsHost(m *hoMachine) *hoHost {
	p := s.Poller
	row := m.row.ID
	h := &hoHost{key: p.HostKey(row), baseName: p.BaseName(row), baseWol: p.ConfigWol(row),
		wolWith: func(base *config.Wol, e adminstate.WolEdit) (*config.Wol, string) { return p.WolWith(row, base, e) }}
	if h.baseName == "" {
		h.baseName = m.row.Name
	}
	for _, a := range p.WolRelays() {
		if m.mine(a.ID) {
			continue // a machine does not send its own wake packet
		}
		r := hoRelay{ID: a.ID, Name: a.DisplayName()}
		if x := p.RelaySystem(a.ID); x != nil {
			r.Name, r.Online = x.Name, x.Status == "online"
		}
		h.relays = append(h.relays, r)
	}
	h.mine = func(id string) bool { return id == row || m.mine(id) }
	h.others, h.guests = s.otherNames(h.mine)
	return h
}

// otherNames collects, for a machine (mine: whether an id is one of its own), everybody else whose name it
// must not be taken for: the other hosts (others) and the guests. A host is a row of a node or an agent, with
// the ids its panel-given name can be kept under and the names the configuration has for it; an agent that
// has no row (it is a part of no row yet: added a moment ago, never polled); and a host of the Add host list
// that the panel does not poll right now (its tokens file is damaged: "Re-pair it"), which comes back under
// its name.
func (s *Server) otherNames(mine func(id string) bool) (others, guests []hoOther) {
	p := s.Poller
	known := func(id string) bool {
		return slices.ContainsFunc(others, func(o hoOther) bool { return slices.Contains(o.ids, id) })
	}
	for _, x := range p.Snapshot() {
		switch {
		case mine(x.ID):
		case x.Kind == "node" || x.Kind == "agent":
			o := hoOther{row: x.ID, ids: p.MachineIDs(x.ID), names: []string{p.BaseName(x.ID)}}
			for _, id := range o.ids {
				if a, ok := p.AgentByID(id); ok && a.Name != "" && !slices.Contains(o.names, a.Name) {
					o.names = append(o.names, a.Name)
				}
			}
			others = append(others, o)
		default:
			guests = append(guests, hoOther{row: x.ID, names: []string{x.Name}})
		}
	}
	for _, a := range p.Agents() {
		if a.Name != "" && !mine(a.ID) && !known(a.ID) {
			others = append(others, hoOther{row: a.ID, ids: []string{a.ID}, names: []string{a.Name}})
		}
	}
	if s.Managed != nil {
		added, _ := s.Managed.Load() // a list that cannot be read adds nobody here: Add host refuses on its own then (idFree)
		for _, x := range added {
			if x.Name != "" && !mine(x.ID) && !mine(x.Shown()) && !known(x.ID) {
				others = append(others, hoOther{row: x.ID, ids: slices.Compact([]string{x.ID, x.Shown()}), names: []string{x.Name}, absent: true})
			}
		}
	}
	return others, guests
}

// A host's name is typed to confirm a power action on it, and a guest's to confirm its shutdown or stop
// (powerctl.CheckConfirmation). So no two rows that can be asked for may have one name, and "one name" is what
// a person cannot tell apart (adminstate.NameKey). The names of UPSes and plugs are not in this: nothing is
// confirmed by typing one, and no action is taken on one by its name.
//
// hoName is a name that is taken: whose it is, and how. panel: an admin gave it in the panel (it is what the
// host is shown under). config: the configuration has it for the host, which is shown under another name
// right now; it is what a Reset there would show.
type hoName struct {
	name, row string
	guest     bool
	panel     bool
	absent    bool   // its host has no row right now: the name is kept for it (state.json, the Add host list)
	shown     string // for a config name: what the host is shown under now
}

// names is every name of the other rows under o: by its key, the names that are shown now (shown) and the
// names the configuration has for hosts that are shown under another one (config). It is asked with what
// state.json holds at that moment (inside adminstate.EditHostOptions): of two admins who name two hosts at
// once, the second is told that the name is taken.
func (h *hoHost) names(o adminstate.HostOptions) (shown, config map[string]hoName) {
	shown, config = map[string]hoName{}, map[string]hoName{}
	for _, g := range h.guests {
		shown[adminstate.NameKey(g.names[0])] = hoName{name: g.names[0], row: g.row, guest: true}
	}
	for _, x := range h.others {
		now, panel := x.names[0], false
		for _, id := range x.ids { // the entry the poller finds for it (poller.hostEditIn)
			if e, ok := o.Hosts[id]; ok && !e.Empty() {
				if e.Name != "" {
					now, panel = e.Name, true
				}
				break
			}
		}
		for _, n := range x.names {
			if n != "" && adminstate.NameKey(n) != adminstate.NameKey(now) {
				config[adminstate.NameKey(n)] = hoName{name: n, row: x.row, shown: now}
			}
		}
		if now != "" {
			shown[adminstate.NameKey(now)] = hoName{name: now, row: x.row, panel: panel, absent: x.absent}
		}
	}
	// ... and a name that state.json keeps under an id that is nobody's row right now: an added host that is
	// left out at this start, a Proxmox node without an agent whose API does not answer, a host taken out of
	// config.yaml. The name waits for that host, and is on its row the moment it is back.
	for _, id := range slices.Sorted(maps.Keys(o.Hosts)) {
		e := o.Hosts[id]
		if e.Name == "" || (h.mine != nil && h.mine(id)) || slices.ContainsFunc(h.others, func(x hoOther) bool { return slices.Contains(x.ids, id) }) {
			continue
		}
		if key := adminstate.NameKey(e.Name); shown[key].name == "" {
			shown[key] = hoName{name: e.Name, row: id, panel: true, absent: true}
		}
	}
	return shown, config
}

// nameRefusal says why name cannot be given to a host: whose it is already (shown and config are what names
// returns). "": it is free. A guest's name is in shown for Edit host only: Add host does not collect guests.
func nameRefusal(name string, shown, config map[string]hoName) string {
	key := adminstate.NameKey(name)
	switch t, taken := shown[key]; {
	case taken && t.guest:
		return fmt.Sprintf("a guest is called %s already", t.name)
	case taken && t.absent:
		return fmt.Sprintf("%s is kept as the name of another host (%s) that is not shown right now, and it comes back under that name", t.name, t.row)
	case taken:
		return fmt.Sprintf("another host is called %s already", t.name)
	}
	if t, taken := config[key]; taken {
		return fmt.Sprintf("%s is the name config.yaml has for another host (shown as %s now): after a Reset there, two hosts would have one name", t.name, t.shown)
	}
	return ""
}

func (h *hoHost) relayName(id string) string {
	for _, r := range h.relays {
		if r.ID == id {
			return r.Name
		}
	}
	return id
}

// normMAC is a MAC address of the configuration in the one spelling the panel stores (aa:bb:cc:dd:ee:ff).
func normMAC(m string) string {
	m = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(m), "-", ":"))
	if len(m) == 12 && !strings.Contains(m, ":") {
		m = m[0:2] + ":" + m[2:4] + ":" + m[4:6] + ":" + m[6:8] + ":" + m[8:10] + ":" + m[10:12]
	}
	return m
}

func wolMACs(w *config.Wol) []string {
	out := []string{}
	if w == nil {
		return out
	}
	for _, m := range w.AllMACs() {
		if m = normMAC(m); !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

func wolPort(w *config.Wol) int {
	if w == nil || w.Port == 0 {
		return 9
	}
	return w.Port
}

// wolWords is a Wake-on-LAN as a line says it: "off", or where the packet goes and who sends it.
func (h *hoHost) wolWords(w *config.Wol) string {
	if w == nil {
		return "off"
	}
	return fmt.Sprintf("to %s through %s, port %d", strings.Join(wolMACs(w), ", "), h.relayName(w.Relay), wolPort(w))
}

// wolValue is a Wake-on-LAN as the audit log and the dialog have it (nil: none). Only what can be changed
// here: never the target address or the password.
func (h *hoHost) wolValue(w *config.Wol) any {
	if w == nil {
		return nil
	}
	return map[string]any{"macs": wolMACs(w), "relay": w.Relay, "relay_name": h.relayName(w.Relay), "port": wolPort(w)}
}

// effective is the name and the Wake-on-LAN that count for the machine under the decisions e, and why the
// Wake-on-LAN decisions are not used ("": they are).
func (h *hoHost) effective(e adminstate.HostEdit) (name string, wol *config.Wol, unused string) {
	name = h.baseName
	if e.Name != "" {
		name = e.Name
	}
	if e.Wol == nil {
		wol, _ = adminstate.WolEdit{}.Apply(h.baseWol)
		return name, wol, ""
	}
	wol, unused = h.wolWith(h.baseWol, *e.Wol)
	return name, wol, unused
}

// wolDecision turns what a request says a host's Wake-on-LAN shall be into decisions: cur with each thing
// the request names, and without a decision for what is the configuration's value anyway. nil: nothing is
// decided (the configuration counts).
func (h *hoHost) wolDecision(req hoWolReq, cur *adminstate.WolEdit) (*adminstate.WolEdit, error) {
	base := h.baseWol
	if req.Off != nil {
		if base == nil {
			return nil, nil // it has none: nothing to switch off
		}
		return &adminstate.WolEdit{Off: true}, nil
	}
	d := adminstate.WolEdit{}
	if cur != nil && !cur.Off {
		d = cur.Clone()
	}
	if req.MACs != nil {
		macs, err := adminstate.WolMACs(*req.MACs)
		if err != nil {
			return nil, apierr.New(422, "wol: "+err.Error())
		}
		if d.MACs = macs; slices.Equal(slices.Sorted(slices.Values(macs)), slices.Sorted(slices.Values(wolMACs(base)))) {
			d.MACs = nil
		}
	}
	if req.Relay != nil {
		switch {
		case base != nil && *req.Relay == base.Relay:
			d.Relay = ""
		case adminstate.WolRelayFixed(base) != "":
			return nil, apierr.New(422, "wol: the relay cannot be changed here: "+adminstate.WolRelayFixed(base))
		default:
			d.Relay = *req.Relay
		}
	}
	if req.Port != nil {
		if d.Port = *req.Port; d.Port == wolPort(base) {
			d.Port = 0
		}
	}
	if d.Empty() {
		return nil, nil
	}
	return &d, nil
}

// applyHost makes the changes of req about the machine itself in o: its name and its Wake-on-LAN (the site
// is not kept here: hoSite). It returns one line for each thing that changes and the changes for the audit
// log. An error is a refusal (422).
func (m *hoMachine) applyHost(o *adminstate.HostOptions, req hostOptionsReq) (lines []string, changes []hoChange, err error) {
	h := m.host
	if h == nil || (!req.hostSet() && !req.resets("name") && !req.resets("wol")) {
		return nil, nil, nil
	}
	bad := func(format string, a ...any) error { return apierr.New(422, fmt.Sprintf(format, a...)) }
	was := o.Host(h.key).Clone()
	e := was.Clone()
	if req.resets("name") {
		e.Name = ""
	}
	if req.resets("wol") {
		e.Wol = nil
	}
	if req.Name != nil {
		name, err := adminstate.HostName(*req.Name)
		if err != nil || name == "" {
			return nil, nil, bad("name: not a name")
		}
		switch {
		case name == h.baseName:
			e.Name = "" // config.yaml's own name: nothing to keep (and the same as a Reset: below)
		default:
			shown, config := h.names(*o)
			if why := nameRefusal(name, shown, config); why != "" {
				return nil, nil, bad("name: %s", why)
			}
			e.Name = name
		}
	}
	if was.Name != "" && e.Name == "" {
		// Back to config.yaml's name, by Reset or by typing it. Another host may have been given that name in the
		// panel meanwhile: then this one keeps the name it has until that one is renamed. (Two hosts that
		// config.yaml itself gives one name are config.yaml's, and are not refused here.)
		shown, _ := h.names(*o)
		if t, taken := shown[adminstate.NameKey(h.baseName)]; taken && t.panel {
			return nil, nil, bad("name: config.yaml's name of this host is %s, and another host (%s) was given that name here: rename that host first", h.baseName, t.row)
		}
	}
	if req.Wol != nil {
		if e.Wol, err = h.wolDecision(*req.Wol, e.Wol); err != nil {
			return nil, nil, err
		}
	}
	nameWas, wolWas, _ := h.effective(was)
	nameIs, wolIs, unused := h.effective(e)
	wolChanged := (was.Wol == nil) != (e.Wol == nil) || (was.Wol != nil && !was.Wol.Same(*e.Wol))
	if wolChanged && unused != "" {
		return nil, nil, bad("wol: %s", unused)
	}
	from := func(panel bool) string { return hoFrom(panel) }
	if was.Name != e.Name {
		if req.resets("name") {
			lines = append(lines, "Name: "+hoBack+nameIs)
		} else {
			lines = append(lines, fmt.Sprintf("Name: %s (was %s)", nameIs, nameWas))
		}
		changes = append(changes, hoChange{Host: h.key, Field: "name", Before: nameWas, After: nameIs, BeforeFrom: from(was.Name != ""), AfterFrom: from(e.Name != ""), Stored: strOrNil(e.Name)})
	}
	if wolChanged {
		if req.resets("wol") {
			lines = append(lines, "Wake-on-LAN: "+hoBack+h.wolWords(wolIs))
		} else {
			lines = append(lines, fmt.Sprintf("Wake-on-LAN: %s (was %s)", h.wolWords(wolIs), h.wolWords(wolWas)))
		}
		var stored any
		if e.Wol != nil {
			stored = *e.Wol
		}
		changes = append(changes, hoChange{Host: h.key, Field: "wol", Before: h.wolValue(wolWas), After: h.wolValue(wolIs), BeforeFrom: from(was.Wol != nil), AfterFrom: from(e.Wol != nil), Stored: stored})
	}
	o.SetHost(h.key, e)
	return lines, changes, nil
}

// hostView is the "host" part of the dialog's data: what counts for the machine itself, where each value
// comes from, the choices, and what is shown read-only.
func (m *hoMachine) hostView(o adminstate.HostOptions, sites *hoSites) map[string]any {
	h := m.host
	e := o.Host(h.key)
	name, wol, unused := h.effective(e)
	who := func(panel bool) (string, any, any) {
		if !panel {
			return "config", nil, nil
		}
		var at any
		if !e.At.IsZero() {
			at = e.At.UTC().Format(time.RFC3339)
		}
		return "panel", strOrNil(e.By), at
	}
	nfrom, nby, nat := who(e.Name != "")
	wfrom, wby, wat := who(e.Wol != nil)
	relays := h.relays
	if relays == nil {
		relays = []hoRelay{}
	}
	w := map[string]any{"on": wol != nil, "macs": wolMACs(wol), "relay": nil, "relay_name": nil, "port": wolPort(wol), "from": wfrom, "by": wby, "at": wat,
		"config": h.wolValue(h.baseWol), "target": nil, "password": false, "relay_fixed": strOrNil(adminstate.WolRelayFixed(h.baseWol)),
		"unused": strOrNil(unused), "relays": relays}
	if wol != nil {
		w["relay"], w["relay_name"] = wol.Relay, h.relayName(wol.Relay)
	}
	if b := h.baseWol; b != nil { // what is never set here, as facts: is there a target address, is there a password
		w["target"], w["password"] = strOrNil(b.Broadcast), b.PasswordEnv != ""
	}
	return map[string]any{"key": h.key,
		"name": map[string]any{"value": name, "from": nfrom, "by": nby, "at": nat, "config": h.baseName},
		"site": sites.view(),
		"wol":  w}
}

// hoSites is the site of a request's machine and what a change of it would move.
//
// The site decides who SEES a machine: its row, the guests that have no site of their own, and the UPSes and
// plugs that follow their links to it (view.go). It decides nothing else: only admins control anything, and
// an admin sees every site. So a change here is the change the Users page makes ("PUT /api/admin/assign"),
// with the same guard, and it is stored where that page stores it.
type hoSites struct {
	m       *hoMachine
	s       *Server
	grants  access.Data
	snap    []model.System
	ref     siteRef // the machine's site now
	choices []string
}

func (s *Server) hostOptionsSites(m *hoMachine) *hoSites {
	x := &hoSites{m: m, s: s, grants: s.Access.Load(), snap: s.Poller.Snapshot()}
	x.ref = s.sites(x.grants, x.snap)[m.row.ID]
	x.choices = slices.Sorted(slices.Values(x.grants.Sites))
	return x
}

// hoSiteChange is one change of a machine's site, worked out and not yet made.
type hoSiteChange struct {
	assign string   // what is stored for the row afterwards ("": no assignment, the default counts)
	was    siteRef  // before
	is     siteRef  // after
	line   string   // "Site: site-b (was site-a)"
	rows   []string // whose view of the machine and its guests moves
	after  *hoMachine
}

func orNone(site string) string {
	if site == "" {
		return "none (admins only)"
	}
	return site
}

// plan works out what choosing want for the machine does ("": back to the default). nil: nothing changes.
// An error is a refusal (422: no such site).
func (x *hoSites) plan(want string) (*hoSiteChange, error) {
	if want != "" && !x.grants.HasSite(want) {
		return nil, apierr.New(422, "site: no such site")
	}
	id := x.m.row.ID
	assign := want
	if want == x.ref.Default {
		assign = "" // the default itself: nothing to keep
	}
	if assign == x.grants.Assign[id] {
		return nil, nil
	}
	g := x.grants
	g.Assign = maps.Clone(x.grants.Assign)
	if g.Assign == nil {
		g.Assign = map[string]string{}
	}
	if assign == "" {
		delete(g.Assign, id)
	} else {
		g.Assign[id] = assign
	}
	before, after := x.s.sites(x.grants, x.snap), x.s.sites(g, x.snap)
	c := &hoSiteChange{assign: assign, was: before[id], is: after[id]}
	if c.line = fmt.Sprintf("Site: %s (was %s)", orNone(c.is.Site), orNone(c.was.Site)); want == "" {
		c.line = fmt.Sprintf("Site: back to the default: %s (was %s)", orNone(c.is.Site), orNone(c.was.Site))
	}
	guests := 0
	for _, r := range x.snap {
		if r.ID != id && before[r.ID].Site != after[r.ID].Site {
			guests++
		}
	}
	what := x.m.row.Name
	switch {
	case guests == 1:
		what += " and 1 guest of it"
	case guests > 1:
		what += fmt.Sprintf(" and %d guests of it", guests)
	}
	if c.is.Site != "" {
		c.rows = append(c.rows, fmt.Sprintf("Viewers of %s will see %s", c.is.Site, what))
	}
	if c.was.Site != "" {
		c.rows = append(c.rows, fmt.Sprintf("Viewers of %s will no longer see %s", c.was.Site, what))
	}
	if c.is.Site == "" {
		c.rows = append(c.rows, fmt.Sprintf("Only admins will see %s", what))
	}
	am := *x.m
	am.site = make(map[string]string, len(after))
	for rid, ref := range after {
		am.site[rid] = ref.Site
	}
	c.after = &am
	return c, nil
}

// notes is whose view the change moves, of the machine and of the devices that follow it, with the links as
// before and after say them.
func (c *hoSiteChange) notes(m *hoMachine, before, after adminstate.HostOptions) []string {
	return append(slices.Clone(c.rows), m.visibilityTo(c.after, before, after)...)
}

// view is the "site" part of the dialog's data: the site now and where it comes from, the choices, and for
// each choice the line and the notes a save would have ("" is "back to the default").
func (x *hoSites) view() map[string]any {
	o := x.s.Poller.Admin.HostOptions()
	plans := map[string]any{}
	for _, want := range append([]string{""}, x.choices...) {
		if c, err := x.plan(want); err == nil && c != nil {
			plans[want] = map[string]any{"line": c.line, "notes": c.notes(x.m, o, o)}
		}
	}
	choices := x.choices
	if choices == nil {
		choices = []string{}
	}
	return map[string]any{"value": strOrNil(x.ref.Site), "from": strOrNil(x.ref.From), "default": strOrNil(x.ref.Default), "choices": choices, "plans": plans}
}

// wakeLosers is the machines that lose a change with a Remove of the host with agentID (ids: its other rows):
// those whose Wake-on-LAN details, as changed in "Edit host", name it as the sender of their wake packets. The
// whole change of such a machine goes with the relay (adminstate: forgetHosts), never half of it. By the names
// they are shown under, for the Remove dialog, its answer and the audit log.
func (s *Server) wakeLosers(agentID string, ids ...string) []string {
	keys := s.Poller.Admin.HostOptions().RelayUsers(append(slices.Clone(ids), agentID)...)
	if len(keys) == 0 {
		return nil
	}
	snap := s.Poller.Snapshot()
	var names []string
	for _, key := range keys {
		name := key
		if sys := s.Poller.ShownSystem(key); sys != nil {
			name = sys.Name
		} else if i := slices.IndexFunc(snap, func(x model.System) bool { return x.ID == key }); i >= 0 {
			name = snap[i].Name
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// wakeLostText is the sentence of Remove's answer about them ("": there are none).
func wakeLostText(removed string, names []string) string {
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf(". %s was chosen in Edit host as the sender of the wake packets of %s: that change was taken out with it, and what config.yaml says about Wake-on-LAN counts there again",
		removed, strings.Join(names, ", "))
}
