// Package access is who may see and do what in the panel: users with a role (admin, viewer or pending) and,
// for viewers, the sites they may see; the list of sites; and which site each host, power source and UPS
// belongs to. Pocket ID only decides who may sign in; everything here is managed on the Users page or with
// `fairy admin users|sites`.
//
// It is all stored in <data_dir>/users.json (see Store), written with the same lock / re-read / rename pattern
// as the other state files so the CLI and the running panel never overwrite each other's changes.
package access

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Roles. A pending user has signed in but has no access yet ("waiting for access" page, no data).
const (
	RoleAdmin   = "admin"
	RoleViewer  = "viewer"
	RolePending = "pending"
)

// Roles lists the valid roles, most powerful first.
var Roles = []string{RoleAdmin, RoleViewer, RolePending}

// ValidRole reports whether r is admin, viewer or pending.
func ValidRole(r string) bool { return slices.Contains(Roles, r) }

// Target prefixes for site assignments of things that are not systems.
const (
	PowerPrefix = "power:"
	UPSPrefix   = "ups:"
)

// Grant is what one request may see and do.
//
// Admin: everything, including control actions, the audit log and the Users page. A viewer sees only systems,
// power sources and UPSes of its Sites; All marks a read-only grant that sees every site (an API token with
// read scope and no site list, as before roles existed). A pending grant sees nothing.
type Grant struct {
	Role  string
	Sites []string
	All   bool
}

// AdminGrant is the grant of an admin.
func AdminGrant() Grant { return Grant{Role: RoleAdmin, All: true} }

// Admin reports whether the grant may control, read the audit log and manage users.
func (g Grant) Admin() bool { return g.Role == RoleAdmin }

// Pending reports whether the grant has no access to any data.
func (g Grant) Pending() bool { return g.Role != RoleAdmin && g.Role != RoleViewer }

// SeesAll reports whether nothing has to be filtered for this grant.
func (g Grant) SeesAll() bool { return g.Admin() || (g.Role == RoleViewer && g.All) }

// Sees reports whether something in site may be shown. Things without a site ("") are visible to admins (and
// all-sites tokens) only.
func (g Grant) Sees(site string) bool {
	if g.SeesAll() {
		return true
	}
	return g.Role == RoleViewer && site != "" && slices.Contains(g.Sites, site)
}

// Same reports whether two grants allow exactly the same (used to end a live stream when access changes).
func (g Grant) Same(o Grant) bool {
	return g.Role == o.Role && g.All == o.All && slices.Equal(g.Sites, o.Sites)
}

// Cap limits g to what an owner with grant o may see: a token never sees more than the user it belongs to.
func (g Grant) Cap(o Grant) Grant {
	switch {
	case o.Admin():
		return g
	case o.Pending() || g.Pending():
		return Grant{Role: RolePending}
	}
	out := Grant{Role: RoleViewer}
	switch {
	case g.SeesAll() && o.SeesAll():
		out.All = true
	case g.SeesAll():
		out.Sites = slices.Clone(o.Sites)
	case o.SeesAll():
		out.Sites = slices.Clone(g.Sites)
	default:
		for _, s := range g.Sites {
			if slices.Contains(o.Sites, s) {
				out.Sites = append(out.Sites, s)
			}
		}
	}
	return out
}

// String describes the grant for logs and the demo switcher: "admin", "viewer:Alice,Bob", "pending".
func (g Grant) String() string {
	switch {
	case g.Admin():
		return RoleAdmin
	case g.Role == RoleViewer && g.All:
		return "viewer:*"
	case g.Role == RoleViewer:
		return RoleViewer + ":" + strings.Join(g.Sites, ",")
	}
	return RolePending
}

// ParseView parses a demo preview ("admin", "pending", "viewer:Bob" or "viewer:Alice,Bob").
func ParseView(v string) (Grant, error) {
	role, sites, _ := strings.Cut(strings.TrimSpace(v), ":")
	switch role {
	case RoleAdmin, RolePending:
		if sites != "" {
			return Grant{}, fmt.Errorf("%s takes no sites", role)
		}
		if role == RoleAdmin {
			return AdminGrant(), nil
		}
		return Grant{Role: RolePending}, nil
	case RoleViewer:
		list, err := ParseSites(sites)
		if err != nil {
			return Grant{}, err
		}
		if len(list) == 0 {
			return Grant{}, errors.New("viewer needs at least one site, e.g. viewer:Bob")
		}
		return Grant{Role: RoleViewer, Sites: list}, nil
	}
	return Grant{}, errors.New(`expected admin, pending or viewer:<site>[,<site>...]`)
}

// ParseSites splits a comma-separated site list, checking every name; the result is sorted and de-duplicated.
func ParseSites(s string) ([]string, error) {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !validSite(p) {
			return nil, fmt.Errorf("invalid site name %q", p)
		}
		out = append(out, p)
	}
	return normSites(out), nil
}

func normSites(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return slices.Compact(out)
}
