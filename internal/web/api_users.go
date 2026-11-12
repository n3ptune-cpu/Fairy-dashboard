package web

import (
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The Users page (admins only): who may see and do what, the sites, and which site every host, power source
// and UPS belongs to. Reads need an admin browser session; every change also needs the CSRF header, a recent
// Pocket ID sign-in (step-up, same window as control actions) and is written to the audit log.

type adminUser struct {
	access.User
	You bool `json:"you"`
}

type adminSite struct {
	Name     string `json:"name"`
	Users    int    `json:"users"`
	Hosts    int    `json:"hosts"`
	InConfig bool   `json:"in_config"` // named in config.yaml: rename / delete it there
}

// adminTarget is one thing that can be put into a site.
type adminTarget struct {
	Target      string   `json:"target"` // system id, power:<id> or ups:<id>
	Kind        string   `json:"kind"`   // node | qemu | lxc | agent | power | ups
	Name        string   `json:"name"`
	Parent      *string  `json:"parent"`
	Cluster     *string  `json:"cluster"`
	Site        string   `json:"site"` // effective site; "" = admins only
	From        string   `json:"from"` // panel | config | node | link | ""
	Assigned    string   `json:"assigned"`
	Default     string   `json:"default"`
	DefaultFrom string   `json:"default_from"`
	Follows     []string `json:"follows"` // power sources / UPSes: the hosts they follow
}

type adminPage struct {
	Users   []adminUser   `json:"users"`
	Sites   []adminSite   `json:"sites"`
	Targets []adminTarget `json:"targets"`
	Roles   []string      `json:"roles"`
}

func (s *Server) adminRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/admin/users", s.h(s.adminUsers))
	m.HandleFunc("PUT /api/admin/users/{sub}", s.h(s.adminSetUser))
	m.HandleFunc("DELETE /api/admin/users/{sub}", s.h(s.adminRemoveUser))
	m.HandleFunc("POST /api/admin/sites", s.h(s.adminCreateSite))
	m.HandleFunc("PUT /api/admin/sites/{name}", s.h(s.adminRenameSite))
	m.HandleFunc("DELETE /api/admin/sites/{name}", s.h(s.adminDeleteSite))
	m.HandleFunc("PUT /api/admin/assign", s.h(s.adminAssign))
}

// adminRead: admin with a browser session (API tokens are refused, like on the settings endpoints).
func (c *reqCtx) adminRead() (*model.User, error) {
	u, err := c.requireSession()
	if err != nil {
		return nil, err
	}
	if _, g, _ := c.grant(); !g.Admin() {
		return nil, c.deny(u, 403, "the Users page is for admins only", "users page denied")
	}
	return u, nil
}

func (s *Server) adminUsers(c *reqCtx) error {
	u, err := c.adminRead()
	if err != nil {
		return err
	}
	c.json(200, s.adminPage(u))
	return nil
}

// adminPage builds the Users page: users, sites (with counts) and every assignable target.
func (s *Server) adminPage(me *model.User) adminPage {
	d := s.Access.Load()
	snap := s.Poller.Snapshot()
	ref := s.sites(d, snap)
	cfgSites := s.Cfg.Sites()
	page := adminPage{Users: []adminUser{}, Sites: []adminSite{}, Targets: []adminTarget{}, Roles: access.Roles}
	for _, x := range d.SortedUsers() {
		page.Users = append(page.Users, adminUser{x, x.Sub == me.Sub})
	}
	names := map[string]string{}
	for _, x := range snap {
		r := ref[x.ID]
		names[x.ID] = x.Name
		page.Targets = append(page.Targets, adminTarget{Target: x.ID, Kind: x.Kind, Name: x.Name, Parent: x.Parent,
			Cluster: x.Cluster, Site: r.Site, From: r.From, Assigned: d.Assign[x.ID], Default: r.Default,
			DefaultFrom: r.DefaultFrom, Follows: []string{}})
	}
	v := &view{s: s, snap: snap, data: d, site: ref}
	agentSite := func(via *string) string {
		if via == nil {
			return ""
		}
		return ref[v.agentSystem(*via)].Site
	}
	follow := func(target, kind, name string, linked []string, via *string) adminTarget {
		t := adminTarget{Target: target, Kind: kind, Name: name, Assigned: d.Assign[target], Follows: []string{}}
		var sites []string
		for _, id := range linked {
			if n, ok := names[id]; ok {
				t.Follows = append(t.Follows, n)
				if st := ref[id].Site; st != "" {
					sites = append(sites, st)
				}
			}
		}
		if len(linked) == 0 && via != nil {
			if n, ok := names[v.agentSystem(*via)]; ok {
				t.Follows = append(t.Follows, n)
			}
			if st := agentSite(via); st != "" {
				sites = append(sites, st)
			}
		}
		sort.Strings(sites)
		t.Default = strings.Join(slices.Compact(sites), ", ")
		if t.Default != "" {
			t.DefaultFrom = fromLink
		}
		t.Site, t.From = t.Default, t.DefaultFrom
		if t.Assigned != "" {
			t.Site, t.From = t.Assigned, fromPanel
		}
		return t
	}
	for _, st := range s.Poller.Power.Overview().Sources {
		page.Targets = append(page.Targets, follow(access.PowerPrefix+st.ID, "power", st.Name, v.powerLinks(st), st.Via))
	}
	for _, u := range s.Poller.UPS.Overview(false).UPS {
		page.Targets = append(page.Targets, follow(access.UPSPrefix+u.ID, "ups", u.Name, u.Powers, u.Via))
	}
	for _, name := range d.Sites {
		as := adminSite{Name: name, InConfig: slices.Contains(cfgSites, name)}
		for _, x := range d.Users {
			if x.Role == access.RoleViewer && slices.Contains(x.Sites, name) {
				as.Users++
			}
		}
		for _, t := range page.Targets { // hosts only, as the column says: not its guests, power sources and UPSes
			if t.Site == name && (t.Kind == "node" || t.Kind == "agent") {
				as.Hosts++
			}
		}
		page.Sites = append(page.Sites, as)
	}
	return page
}

type setUserReq struct {
	Role  string   `json:"role"`
	Sites []string `json:"sites"`
}

func (s *Server) adminSetUser(c *reqCtx) error {
	if _, err := c.requireSession(); err != nil { // before the body is read (adminGuard checks the rest after it)
		return err
	}
	var body setUserReq
	if err := c.decodeBody(&body); err != nil {
		return err
	}
	if !access.ValidRole(body.Role) {
		return apierr.New(422, "role must be admin, viewer or pending")
	}
	if body.Role != access.RoleViewer && len(body.Sites) > 0 {
		return apierr.New(422, "only viewers have a site list (admins see everything, pending users nothing)")
	}
	u, err := c.adminGuard()
	if err != nil {
		return err
	}
	sub := c.r.PathValue("sub")
	before, after, err := s.Access.SetRole(sub, body.Role, body.Sites, "panel by "+actorOf(u), false)
	params := map[string]any{"sub": sub, "role": body.Role, "sites": body.Sites}
	if err != nil {
		c.audit(u, "user.set_role", sub, params, errorOutcome(err))
		return err
	}
	params["before"] = access.Describe(before)
	c.audit(u, "user.set_role", after.Label(), params, "ok")
	c.json(200, map[string]any{"ok": true, "message": fmt.Sprintf("%s is now %s", after.Label(), access.Describe(after))})
	return nil
}

func (s *Server) adminRemoveUser(c *reqCtx) error {
	u, err := c.adminGuard()
	if err != nil {
		return err
	}
	sub := c.r.PathValue("sub")
	gone, err := s.Access.Remove(sub, false, "panel by "+actorOf(u))
	if err != nil {
		c.audit(u, "user.remove", sub, map[string]any{"sub": sub}, errorOutcome(err))
		return err
	}
	sessions, tokens := s.revokeUser(gone.Sub)
	c.audit(u, "user.remove", gone.Label(), map[string]any{"sub": gone.Sub, "was": access.Describe(gone),
		"sessions_revoked": sessions, "tokens_revoked": tokens}, "ok")
	c.json(200, map[string]any{"ok": true, "message": gone.Label() + " was removed from the list"})
	return nil
}

// revokeUser ends a removed user's sessions and revokes their API tokens (their access is gone already: this
// makes it final even if they are given a role again later).
func (s *Server) revokeUser(sub string) (sessions, tokens int) {
	sessions, err := s.Poller.Admin.RevokeUserSessions(sub)
	if err != nil {
		slog.Error("revoking a removed user's sessions failed", "sub", sub, "error", err)
	}
	tokens, err = s.Tokens.RevokeOwner(sub)
	if err != nil {
		slog.Error("revoking a removed user's API tokens failed", "sub", sub, "error", err)
	}
	return sessions, tokens
}

type siteReq struct {
	Name string `json:"name"`
}

func (s *Server) adminCreateSite(c *reqCtx) error {
	if _, err := c.requireSession(); err != nil { // before the body is read (adminGuard checks the rest after it)
		return err
	}
	var body siteReq
	if err := c.decodeBody(&body); err != nil {
		return err
	}
	u, err := c.adminGuard()
	if err != nil {
		return err
	}
	name := strings.TrimSpace(body.Name)
	if err := s.Access.CreateSite(name); err != nil {
		c.audit(u, "site.create", name, nil, errorOutcome(err))
		return err
	}
	c.audit(u, "site.create", name, nil, "ok")
	c.json(201, map[string]any{"ok": true, "message": "Site " + name + " created"})
	return nil
}

// configSiteGuard: a site named in config.yaml (`site:` defaults) must be renamed or removed there, or hosts
// would keep pointing at the old name.
func (s *Server) configSiteGuard(name string) error {
	if slices.Contains(s.Cfg.Sites(), name) {
		return apierr.Errf(409, "site %q is set in config.yaml (site: %s): change it there", name, name)
	}
	return nil
}

func (s *Server) adminRenameSite(c *reqCtx) error {
	if _, err := c.requireSession(); err != nil { // before the body is read (adminGuard checks the rest after it)
		return err
	}
	var body siteReq
	if err := c.decodeBody(&body); err != nil {
		return err
	}
	u, err := c.adminGuard()
	if err != nil {
		return err
	}
	old, name := c.r.PathValue("name"), strings.TrimSpace(body.Name)
	params := map[string]any{"to": name}
	if err := s.configSiteGuard(old); err != nil {
		c.audit(u, "site.rename", old, params, errorOutcome(err))
		return err
	}
	if err := s.Access.RenameSite(old, name); err != nil {
		c.audit(u, "site.rename", old, params, errorOutcome(err))
		return err
	}
	c.audit(u, "site.rename", old, params, "ok")
	c.json(200, map[string]any{"ok": true, "message": "Site " + old + " renamed to " + name})
	return nil
}

func (s *Server) adminDeleteSite(c *reqCtx) error {
	u, err := c.adminGuard()
	if err != nil {
		return err
	}
	name := c.r.PathValue("name")
	if err := s.configSiteGuard(name); err != nil {
		c.audit(u, "site.delete", name, nil, errorOutcome(err))
		return err
	}
	if err := s.Access.DeleteSite(name); err != nil {
		c.audit(u, "site.delete", name, nil, errorOutcome(err))
		return err
	}
	c.audit(u, "site.delete", name, nil, "ok")
	c.json(200, map[string]any{"ok": true, "message": "Site " + name + " deleted"})
	return nil
}

type assignReq struct {
	Target string  `json:"target"`
	Site   *string `json:"site"` // null or "" = back to the default
}

// knownTarget reports whether target is a current system, power source (power:<id>) or UPS (ups:<id>).
func (s *Server) knownTarget(target string) bool {
	if id, ok := strings.CutPrefix(target, access.PowerPrefix); ok {
		return slices.ContainsFunc(s.Poller.Power.Overview().Sources, func(x model.PowerSourceState) bool { return x.ID == id })
	}
	if id, ok := strings.CutPrefix(target, access.UPSPrefix); ok {
		return slices.ContainsFunc(s.Poller.UPS.Overview(false).UPS, func(x model.UpsState) bool { return x.ID == id })
	}
	return s.Poller.HasSystem(target)
}

func (s *Server) adminAssign(c *reqCtx) error {
	if _, err := c.requireSession(); err != nil { // before the body is read (adminGuard checks the rest after it)
		return err
	}
	var body assignReq
	if err := c.decodeBody(&body); err != nil {
		return err
	}
	u, err := c.adminGuard()
	if err != nil {
		return err
	}
	site := strings.TrimSpace(model.Deref(body.Site))
	params := map[string]any{"site": site}
	if !s.knownTarget(body.Target) {
		c.audit(u, "site.assign", body.Target, params, "error 404: unknown target")
		return apierr.New(404, "no such host, power source or UPS")
	}
	prev, err := s.Access.Assign(body.Target, site)
	if err != nil {
		c.audit(u, "site.assign", body.Target, params, errorOutcome(err))
		return err
	}
	params["before"] = prev
	c.audit(u, "site.assign", body.Target, params, "ok")
	msg := body.Target + " is now in " + site
	if site == "" {
		msg = body.Target + " is back to its default site"
	}
	c.json(200, map[string]any{"ok": true, "message": msg})
	return nil
}
