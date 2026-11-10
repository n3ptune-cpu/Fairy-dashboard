package access

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

func validSite(s string) bool { return config.ValidSite(s) }

// User is one person who has signed in (keyed by their stable OIDC subject). Email and name are kept for
// display and for finding the user from the CLI; they are refreshed at every sign-in.
//
// The subject is the only thing the identity provider guarantees. Name and email are what the account claims,
// and a user may be able to edit them at the provider; EmailVerified says whether the provider vouched for the
// address. An address that is not verified is shown, marked as such, but never selects an account (Find).
type User struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	// EmailVerified is the provider's email_verified claim at the last sign-in. nil = not known: the entry was
	// written by a version without this field (or without a provider) and counts as not verified until the
	// user's next sign-in or request.
	EmailVerified *bool      `json:"email_verified"`
	Name          string     `json:"name"`
	Role          string     `json:"role"`
	Sites         []string   `json:"sites"` // viewers only
	CreatedAt     time.Time  `json:"created_at"`
	LastSeen      *time.Time `json:"last_seen"`
	// Via says how the user got their current role: "first sign-in", "control_groups", "bootstrap_admins",
	// "panel by <admin>", "cli by <os user>".
	Via string `json:"via"`
}

// Grant is the user's access.
func (u User) Grant() Grant {
	switch u.Role {
	case RoleAdmin:
		return AdminGrant()
	case RoleViewer:
		return Grant{Role: RoleViewer, Sites: normSites(u.Sites)}
	}
	return Grant{Role: RolePending}
}

// Verified reports whether the user has an email address the identity provider vouched for.
func (u User) Verified() bool {
	return u.Email != "" && u.EmailVerified != nil && *u.EmailVerified
}

// Account says exactly which account this is, for confirmations and command output: the label, a note when
// the email is only claimed, and the subject: "n@example.com (subject 4f2c)" or
// "n@example.com (e-mail not verified, subject 9a1b)". Two accounts can show the same name and address; the
// subject tells them apart.
func (u User) Account() string {
	l := u.Label()
	switch {
	case l == u.Sub:
		return "subject " + u.Sub
	case u.Email != "" && !u.Verified():
		return l + " (e-mail not verified, subject " + u.Sub + ")"
	}
	return l + " (subject " + u.Sub + ")"
}

// Label is the user's email, name or subject (for messages).
func (u User) Label() string {
	for _, v := range []string{u.Email, u.Name} {
		if v != "" {
			return v
		}
	}
	return u.Sub
}

// Removal is the record ("tombstone") of a user an admin removed: such a subject is never made admin again by
// oidc.control_groups / oidc.bootstrap_admins; if they sign in again they wait for access like a new user.
type Removal struct {
	Label string    `json:"label"` // email, name or subject at the time
	At    time.Time `json:"at"`
	By    string    `json:"by"`
}

// Data is the content of users.json.
type Data struct {
	Users   map[string]User    `json:"users"`   // by subject
	Sites   []string           `json:"sites"`   // sorted
	Assign  map[string]string  `json:"assign"`  // system id, "power:<id>" or "ups:<id>" -> site
	Removed map[string]Removal `json:"removed"` // removed subjects (never bootstrapped again)
}

func decode(raw statefile.Data) Data {
	b, _ := json.Marshal(raw)
	var d Data
	_ = json.Unmarshal(b, &d)
	if d.Users == nil {
		d.Users = map[string]User{}
	}
	if d.Assign == nil {
		d.Assign = map[string]string{}
	}
	if d.Removed == nil {
		d.Removed = map[string]Removal{}
	}
	for k, u := range d.Users {
		if !ValidRole(u.Role) {
			u.Role = RolePending
		}
		u.Sub = k
		if u.Sites == nil {
			u.Sites = []string{}
		}
		d.Users[k] = u
	}
	d.Sites = normSites(d.Sites)
	if d.Sites == nil {
		d.Sites = []string{}
	}
	return d
}

func (d Data) encode(into statefile.Data) {
	b, _ := json.Marshal(d)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	clear(into)
	for k, v := range m {
		into[k] = v
	}
}

// Admins is the number of users with the admin role.
func (d Data) Admins() int {
	n := 0
	for _, u := range d.Users {
		if u.Role == RoleAdmin {
			n++
		}
	}
	return n
}

// SortedUsers returns the users: admins first, then by email / name.
func (d Data) SortedUsers() []User {
	out := make([]User, 0, len(d.Users))
	for _, u := range d.Users {
		out = append(out, u)
	}
	rank := func(r string) int { return slices.Index(Roles, r) }
	sort.Slice(out, func(i, j int) bool {
		if rank(out[i].Role) != rank(out[j].Role) {
			return rank(out[i].Role) < rank(out[j].Role)
		}
		return strings.ToLower(out[i].Label()) < strings.ToLower(out[j].Label())
	})
	return out
}

// HasSite reports whether the site exists.
func (d Data) HasSite(name string) bool { return slices.Contains(d.Sites, name) }

// ByEmail returns the users whose email is this address (case-insensitive): those for whom the identity
// provider verified it, and those who only claim it. Both lists are sorted by subject.
func (d Data) ByEmail(email string) (verified, claimed []User) {
	if email == "" {
		return nil, nil
	}
	for _, u := range d.Users {
		switch {
		case !strings.EqualFold(u.Email, email):
		case u.Verified():
			verified = append(verified, u)
		default:
			claimed = append(claimed, u)
		}
	}
	bySub := func(l []User) { sort.Slice(l, func(i, j int) bool { return l[i].Sub < l[j].Sub }) }
	bySub(verified)
	bySub(claimed)
	return verified, claimed
}

// errEmailNotVerified is Find's answer for an address that accounts only claim.
var errEmailNotVerified = apierr.New(409, "no user has that email verified by the identity provider "+
	"(an account only claims it): use the subject id instead")

// IsEmailNotVerified reports whether err is Find's refusal to select an account by an email address that no
// account has verified.
func IsEmailNotVerified(err error) bool { return err == errEmailNotVerified }

// Find returns the user with this subject, or the one user whose verified email is this address
// (case-insensitive). An address the identity provider did not verify never selects an account: a user who
// can edit their own profile at the provider could otherwise put someone else's address on their account and
// be given that person's role. Such accounts are reached by their subject (ByEmail lists them).
func (d Data) Find(ident string) (User, error) {
	if u, ok := d.Users[ident]; ok {
		return u, nil
	}
	verified, claimed := d.ByEmail(ident)
	switch {
	case len(verified) == 1:
		return verified[0], nil
	case len(verified) > 1:
		return User{}, apierr.New(409, "several users have that email: use their subject id instead")
	case len(claimed) > 0:
		return User{}, errEmailNotVerified
	}
	return User{}, apierr.New(404, "no such user (they must sign in once before they can be given a role)")
}

// Store is users.json in the data directory.
type Store struct {
	Path  string
	watch *statefile.Watched
}

// NewStore returns the store in dataDir. Reads are cached and re-checked at most once a second, so a change
// made with the CLI applies within a second.
func NewStore(dataDir string) *Store {
	p := filepath.Join(dataDir, "users.json")
	return &Store{Path: p, watch: statefile.NewWatched(p)}
}

// Load returns the current content (cached). While users.json is unreadable or corrupt it is empty and Err
// says why: callers must then grant nothing (fail closed).
func (s *Store) Load() Data { return decode(s.watch.Get()) }

// Err is why users.json cannot be used (nil when it is fine or does not exist yet).
func (s *Store) Err() error { return s.watch.Err() }

// update is a locked read-modify-write of users.json.
func (s *Store) update(fn func(*Data) error) (Data, error) {
	var out Data
	_, err := statefile.Update(s.Path, func(raw statefile.Data) error {
		d := decode(raw)
		if err := fn(&d); err != nil {
			return err
		}
		d.Sites = normSites(d.Sites)
		d.encode(raw)
		out = d
		return nil
	})
	if err != nil {
		return Data{}, err
	}
	s.watch.Refresh()
	return out, nil
}

// Seen records a sign-in (or the first request of a session that predates users.json): a new user gets
// newRole (admin when bootstrapped via control_groups / bootstrap_admins, else pending) with the reason via;
// an existing user keeps their role and only has email, name and last-seen refreshed. created reports a new
// user. newRole admin is only applied while users.json has no admin (bootstrap of a fresh install or an
// upgrade), and never to a subject an admin removed: otherwise they start as pending.
//
// Seen does not know whether the email is verified: a new or changed address is stored as "not known" (which
// counts as not verified), an unchanged one keeps what is stored. Sign-ins use SeenIdentity.
func (s *Store) Seen(sub, email, name, newRole, via string) (u User, created bool, err error) {
	return s.seen(sub, email, nil, name, newRole, via)
}

// Identity is what the identity provider said about a user at a sign-in.
type Identity struct {
	Sub           string
	Email         string
	EmailVerified bool // the email_verified claim was true
	Name          string
}

// SeenIdentity is Seen for a sign-in at the identity provider: it also records whether the provider verified
// the email address. A sign-in without an address keeps the stored one, as not verified.
func (s *Store) SeenIdentity(id Identity, newRole, via string) (u User, created bool, err error) {
	return s.seen(id.Sub, id.Email, &id.EmailVerified, id.Name, newRole, via)
}

func (s *Store) seen(sub, email string, verified *bool, name, newRole, via string) (u User, created bool, err error) {
	if sub == "" {
		return User{}, false, apierr.New(400, "missing subject")
	}
	_, err = s.update(func(d *Data) error {
		now := time.Now().UTC()
		cur, ok := d.Users[sub]
		if !ok {
			if _, removed := d.Removed[sub]; removed && newRole == RoleAdmin {
				newRole, via = RolePending, "first sign-in (removed before: no bootstrap)"
			}
			if newRole == RoleAdmin && d.Admins() > 0 { // bootstrap only while there is no admin at all
				newRole, via = RolePending, "first sign-in"
			}
			cur = User{Sub: sub, Role: newRole, Sites: []string{}, CreatedAt: now, Via: via}
			created = true
		}
		switch {
		case verified != nil && email != "":
			v := *verified
			cur.Email, cur.EmailVerified = email, &v
		case verified != nil: // the provider sent no address: the stored one is no longer vouched for
			if cur.Email != "" {
				cur.EmailVerified = new(bool)
			}
		case email != "":
			if cur.Email != email {
				cur.EmailVerified = nil
			}
			cur.Email = email
		}
		if name != "" {
			cur.Name = name
		}
		cur.LastSeen = &now
		d.Users[sub] = cur
		u = cur
		return nil
	})
	return u, created, err
}

// errLastAdmin is refused by SetRole and Remove (server-enforced, also for the CLI unless forced).
var errLastAdmin = apierr.New(409, "this is the last admin: make someone else admin first")

// SetRole changes a user's role (and, for a viewer, sites when sites != nil). Demoting or removing the last
// admin is refused unless force is set.
func (s *Store) SetRole(ident, role string, sites []string, by string, force bool) (before, after User, err error) {
	if !ValidRole(role) {
		return User{}, User{}, apierr.New(422, "role must be admin, viewer or pending")
	}
	_, err = s.update(func(d *Data) error {
		u, err := d.Find(ident)
		if err != nil {
			return err
		}
		before = u
		if u.Role == RoleAdmin && role != RoleAdmin && d.Admins() <= 1 && !force {
			return errLastAdmin
		}
		if sites != nil {
			if err := d.checkSites(sites); err != nil {
				return err
			}
			u.Sites = normSites(sites)
		}
		if u.Role != role {
			u.Via = by
		}
		u.Role = role
		if u.Role != RoleViewer {
			u.Sites = []string{}
		}
		d.Users[u.Sub] = u
		after = u
		return nil
	})
	return before, after, err
}

func (d *Data) checkSites(sites []string) error {
	for _, x := range sites {
		if !d.HasSite(x) {
			return apierr.Errf(422, "unknown site %q", x)
		}
	}
	return nil
}

// Remove forgets a user and keeps a tombstone (see Removal): if they sign in again they start as pending, never
// as admin via control_groups / bootstrap_admins. The caller also revokes their sessions and API tokens.
// Removing the last admin is refused unless force is set.
func (s *Store) Remove(ident string, force bool, by string) (User, error) {
	var out User
	_, err := s.update(func(d *Data) error {
		u, err := d.Find(ident)
		if err != nil {
			return err
		}
		if u.Role == RoleAdmin && d.Admins() <= 1 && !force {
			return errLastAdmin
		}
		delete(d.Users, u.Sub)
		d.Removed[u.Sub] = Removal{Label: u.Label(), At: time.Now().UTC(), By: by}
		out = u
		return nil
	})
	return out, err
}

// CreateSite adds a site.
func (s *Store) CreateSite(name string) error {
	if !validSite(name) {
		return apierr.New(422, "a site name is 1-40 letters, digits, spaces and . _ ' & - (starting with a letter or digit)")
	}
	_, err := s.update(func(d *Data) error {
		if d.HasSite(name) {
			return apierr.Errf(409, "site %q already exists", name)
		}
		d.Sites = append(d.Sites, name)
		return nil
	})
	return err
}

// EnsureSites adds the sites that do not exist yet (sites named in config.yaml).
func (s *Store) EnsureSites(names []string) error {
	missing := false
	d := s.Load()
	for _, n := range names {
		missing = missing || !d.HasSite(n)
	}
	if !missing {
		return nil
	}
	_, err := s.update(func(d *Data) error {
		for _, n := range names {
			if validSite(n) && !d.HasSite(n) {
				d.Sites = append(d.Sites, n)
			}
		}
		return nil
	})
	return err
}

// RenameSite renames a site everywhere (users' site lists and assignments).
func (s *Store) RenameSite(old, name string) error {
	if !validSite(name) {
		return apierr.New(422, "a site name is 1-40 letters, digits, spaces and . _ ' & - (starting with a letter or digit)")
	}
	_, err := s.update(func(d *Data) error {
		if !d.HasSite(old) {
			return apierr.Errf(404, "no site %q", old)
		}
		if old == name {
			return nil
		}
		if d.HasSite(name) {
			return apierr.Errf(409, "site %q already exists", name)
		}
		for i, x := range d.Sites {
			if x == old {
				d.Sites[i] = name
			}
		}
		for k, u := range d.Users {
			for i, x := range u.Sites {
				if x == old {
					u.Sites[i] = name
				}
			}
			u.Sites = normSites(u.Sites)
			d.Users[k] = u
		}
		for k, v := range d.Assign {
			if v == old {
				d.Assign[k] = name
			}
		}
		return nil
	})
	return err
}

// DeleteSite removes a site, its assignments and every user's access to it. Hosts assigned to it fall back to
// their config.yaml default (or admin-only). A site still named in config.yaml comes back at the next start.
func (s *Store) DeleteSite(name string) error {
	_, err := s.update(func(d *Data) error {
		if !d.HasSite(name) {
			return apierr.Errf(404, "no site %q", name)
		}
		d.Sites = slices.DeleteFunc(d.Sites, func(x string) bool { return x == name })
		for k, u := range d.Users {
			u.Sites = slices.DeleteFunc(u.Sites, func(x string) bool { return x == name })
			d.Users[k] = u
		}
		for k, v := range d.Assign {
			if v == name {
				delete(d.Assign, k)
			}
		}
		return nil
	})
	return err
}

// Assign puts a target (system id, "power:<id>" or "ups:<id>") into a site; site "" removes the panel
// assignment (back to the config.yaml default, the node's site for guests, or admin-only).
func (s *Store) Assign(target, site string) (previous string, err error) {
	if target == "" || len(target) > 160 || strings.ContainsAny(target, " \t\r\n") {
		return "", apierr.New(422, "invalid target")
	}
	_, err = s.update(func(d *Data) error {
		previous = d.Assign[target]
		if site == "" {
			delete(d.Assign, target)
			return nil
		}
		if !d.HasSite(site) {
			return apierr.Errf(422, "unknown site %q", site)
		}
		d.Assign[target] = site
		return nil
	})
	return previous, err
}

// Describe is a one-line summary of a user's access ("viewer of Neptune, Straw").
func Describe(u User) string {
	if u.Role == RoleViewer {
		if len(u.Sites) == 0 {
			return "viewer (no sites yet)"
		}
		return fmt.Sprintf("viewer of %s", strings.Join(u.Sites, ", "))
	}
	return u.Role
}
