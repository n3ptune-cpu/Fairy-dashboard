package web

import (
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/access"
	"github.com/n3ptune-cpu/Fairy/internal/apierr"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/tokens"
)

// pendingDetail is the 403 a signed-in user without a role gets from every data endpoint.
var pendingDetail = map[string]any{"code": "pending_access",
	"message": "Waiting for access: ask an admin to give you a role on the Users page."}

// seenEvery is how often a signed-in user's "last seen" is written to users.json.
const seenEvery = 5 * time.Minute

// bootstrapRole is the role a user gets the first time they sign in: admin when they are in
// oidc.control_groups or their email is in oidc.bootstrap_admins (so an upgrade from a version without roles,
// or a fresh users.json, never locks the owner out), else pending.
func (s *Server) bootstrapRole(u *model.User) (role, via string) {
	o := s.Cfg.OIDC
	if u.Auth != "token" && canControl(u, o) {
		return access.RoleAdmin, "control_groups"
	}
	if e := strings.ToLower(model.Deref(u.Email)); e != "" &&
		slices.ContainsFunc(o.BootstrapAdmins, func(x string) bool { return strings.ToLower(x) == e }) {
		return access.RoleAdmin, "bootstrap_admins"
	}
	return access.RolePending, "first sign-in"
}

// recordUser adds a user to users.json the first time they are seen (at sign-in, or on the first request of a
// session started before users.json existed) and refreshes email, name and last seen. A new user is audited.
func (s *Server) recordUser(c *reqCtx, u *model.User) (access.User, bool) {
	role, via := s.bootstrapRole(u)
	rec, created, err := s.Access.Seen(u.Sub, model.Deref(u.Email), model.Deref(u.Name), role, via)
	if err != nil {
		slog.Error("users.json write failed", "error", err)
		return access.User{Sub: u.Sub, Role: role}, false
	}
	s.seenMu.Lock()
	s.seen[u.Sub] = time.Now()
	s.seenMu.Unlock()
	if created {
		c.audit(u, "user.first_sign_in", "", map[string]any{"role": rec.Role, "via": via}, "ok")
	}
	return rec, created
}

// userGrant is the access of a signed-in user (browser session or demo user).
func (s *Server) userGrant(c *reqCtx, u *model.User) access.Grant {
	rec, ok := s.Access.Load().Users[u.Sub]
	if !ok {
		rec, _ = s.recordUser(c, u)
		return rec.Grant()
	}
	s.seenMu.Lock()
	stale := time.Since(s.seen[u.Sub]) > seenEvery
	if stale {
		s.seen[u.Sub] = time.Now()
	}
	s.seenMu.Unlock()
	if stale {
		_, _, _ = s.Access.Seen(u.Sub, model.Deref(u.Email), model.Deref(u.Name), access.RolePending, "")
	}
	return rec.Grant()
}

// tokenGrant is the access of an API token: a control token acts as an admin; a read token sees its sites
// (every site when it has no list). Either way a token never sees more than its owner: if the owner is a user
// in users.json the token is capped to that user's role and sites, so demoting or removing a user's access also
// limits their tokens. Tokens owned by "system" (or by a subject that never signed in) keep their own scope.
func (s *Server) tokenGrant(rec *tokens.Record) access.Grant {
	g := access.Grant{Role: access.RoleViewer, All: rec.Sites == nil, Sites: slices.Clone(rec.Sites)}
	if rec.Scope == "control" {
		g = access.AdminGrant()
	}
	if owner, ok := s.Access.Load().Users[rec.Owner]; ok {
		g = g.Cap(owner.Grant())
	}
	return g
}

// demoGrantNow is the demo preview role ("--demo-as" or the switcher); nil = the demo user's own role (admin).
func (s *Server) demoGrantNow() *access.Grant {
	s.demoMu.Lock()
	defer s.demoMu.Unlock()
	if s.demoView == nil {
		return nil
	}
	g := *s.demoView
	return &g
}

// grantOf works out what u may see and do (not cached).
func (s *Server) grantOf(c *reqCtx, u *model.User) access.Grant {
	switch u.Auth {
	case "token":
		if c.token != nil {
			return s.tokenGrant(c.token)
		}
		return access.Grant{Role: access.RolePending}
	case "demo":
		if g := s.demoGrantNow(); g != nil {
			return *g
		}
	}
	return s.userGrant(c, u)
}

// grant returns the signed-in user and what they may see and do (cached for the request).
func (c *reqCtx) grant() (*model.User, access.Grant, error) {
	u, err := c.requireUser()
	if err != nil {
		return nil, access.Grant{}, err
	}
	if c.g == nil {
		g := c.s.grantOf(c, u)
		c.g = &g
	}
	return u, *c.g, nil
}

// view returns what the current request may see; a user without a role (pending) gets a 403.
func (c *reqCtx) view() (*view, error) {
	_, g, err := c.grant()
	if err != nil {
		return nil, err
	}
	if g.Pending() {
		return nil, apierr.New(403, pendingDetail)
	}
	return c.s.newView(g), nil
}
