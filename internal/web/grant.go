package web

import (
	"log/slog"
	"slices"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/tokens"
)

// pendingDetail is the 403 a signed-in user without a role gets from every data endpoint.
var pendingDetail = map[string]any{"code": "pending_access",
	"message": "Waiting for access: ask an admin to give you a role on the Users page."}

// seenEvery is how often a signed-in user's "last seen" is written to users.json.
const seenEvery = 5 * time.Minute

// bootstrapRole is the role a user gets at their first real sign-in (a fresh Pocket ID login, never a session
// cookie): admin when they are in oidc.control_groups or their email is in oidc.bootstrap_admins (so an upgrade
// from a version without roles, or a fresh users.json, never locks the owner out), else pending. Only a
// verified email counts. The store only applies it while users.json has no admin at all, and never for a
// subject an admin removed.
func (s *Server) bootstrapRole(u *model.User) (role, via string) {
	o := s.Cfg.OIDC
	if u.Auth != "token" && canControl(u, o) {
		return access.RoleAdmin, "control_groups"
	}
	if emailIn(u, o.BootstrapAdmins) {
		return access.RoleAdmin, "bootstrap_admins"
	}
	return access.RolePending, "first sign-in"
}

// identityOf is what the identity provider said about u, for users.json: the subject, the name, and the email
// address with whether the provider verified it (the Users page and the admin CLI mark an address that is only
// claimed, and never find an account by it).
func identityOf(u *model.User) access.Identity {
	return access.Identity{Sub: u.Sub, Email: model.Deref(u.Email), EmailVerified: u.EmailVerified, Name: model.Deref(u.Name)}
}

// recordUser adds a user to users.json the first time they are seen and refreshes email, name and last seen.
// signIn is true only for a fresh Pocket ID login (the OIDC callback): only then may control_groups /
// bootstrap_admins make a new user admin. A user first seen through an existing session cookie (whose groups
// may be days old) always starts as pending. A new user is audited.
func (s *Server) recordUser(c *reqCtx, u *model.User, signIn bool) (access.User, error) {
	role, via := access.RolePending, "first sign-in"
	if signIn {
		role, via = s.bootstrapRole(u)
	}
	rec, created, err := s.Access.SeenIdentity(identityOf(u), role, via)
	if err != nil { // fail closed: a role that could not be recorded is not granted
		slog.Error("users.json write failed: the user gets no access until it is fixed", "sub", u.Sub, "error", err)
		return access.User{Sub: u.Sub, Role: access.RolePending}, err
	}
	s.seenMu.Lock()
	s.seen[u.Sub] = time.Now()
	s.seenMu.Unlock()
	if created {
		c.audit(u, "user.first_sign_in", "", map[string]any{"role": rec.Role, "via": rec.Via}, "ok")
	}
	return rec, nil
}

// userGrant is the access of a signed-in user (browser session or demo user).
func (s *Server) userGrant(c *reqCtx, u *model.User) access.Grant {
	if s.Access.Err() != nil { // users.json unreadable or corrupt: nobody gets access (fail closed)
		return access.Grant{Role: access.RolePending}
	}
	rec, ok := s.Access.Load().Users[u.Sub]
	if !ok {
		if u.Auth != "demo" { // a session of a user not in users.json gets nothing (sessionUser ends it)
			return access.Grant{Role: access.RolePending}
		}
		rec, _ = s.recordUser(c, u, true) // the loopback-only demo user is its own sign-in
		return rec.Grant()
	}
	s.seenMu.Lock()
	stale := time.Since(s.seen[u.Sub]) > seenEvery
	if stale {
		s.seen[u.Sub] = time.Now()
	}
	s.seenMu.Unlock()
	if stale {
		_, _, _ = s.Access.SeenIdentity(identityOf(u), access.RolePending, "")
	}
	return rec.Grant()
}

// tokenGrant is the access of an API token: a control token acts as an admin; a read token sees its sites
// (every site when it has no list). Either way a token never sees more than its owner: the token is capped to
// the owner's role and sites in users.json, so demoting a user also limits their tokens, and a token whose
// owner is not in users.json (removed, or never signed in) gets nothing. Only tokens owned by "system" keep
// their own scope.
func (s *Server) tokenGrant(rec *tokens.Record) access.Grant {
	if s.Access.Err() != nil {
		return access.Grant{Role: access.RolePending}
	}
	g := access.Grant{Role: access.RoleViewer, All: rec.Sites == nil, Sites: slices.Clone(rec.Sites)}
	if rec.Scope == "control" {
		g = access.AdminGrant()
	}
	if rec.Owner == "system" {
		return g
	}
	owner, ok := s.Access.Load().Users[rec.Owner]
	if !ok { // fail closed: an unknown or removed owner's token has no access
		return access.Grant{Role: access.RolePending}
	}
	return g.Cap(owner.Grant())
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
