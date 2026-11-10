package admin

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/health"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/tokens"
)

// takeFlag removes a boolean flag (e.g. --force) from anywhere in args.
func takeFlag(args []string, name string) ([]string, bool) {
	found := false
	out := args[:0:0]
	for _, a := range args {
		if a == "--"+name || a == "-"+name {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

// takeValue removes `--name VALUE` (or --name=VALUE) from anywhere in args.
func takeValue(args []string, name string) ([]string, string, bool) {
	out := args[:0:0]
	val, found := "", false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--"+name && i+1 < len(args):
			val, found = args[i+1], true
			i++
		case strings.HasPrefix(a, "--"+name+"="):
			val, found = strings.TrimPrefix(a, "--"+name+"="), true
		default:
			out = append(out, a)
		}
	}
	return out, val, found
}

// friendly turns an access error into the CLI's message (the API detail without the status code).
func friendly(err error) error {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return fmt.Errorf("%v", ae.Detail)
	}
	return err
}

// stdin is where the CLI reads the answer to a question from (a variable so the tests can answer).
var stdin io.Reader = os.Stdin

// confirm asks a yes/no question on the terminal. Only "yes" (or "y") is a yes; no answer (no terminal, end of
// input) is a no.
func (c *cli) confirm(question string) bool {
	fmt.Fprintf(c.out, "%s Type yes to continue: ", question)
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	if !strings.HasSuffix(line, "\n") {
		fmt.Fprintln(c.out)
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "yes" || a == "y"
}

// emailNote is the user's email as the CLI shows it: an address the identity provider did not verify is
// marked, because the account's owner may have typed it in.
func emailNote(u access.User) string {
	switch {
	case u.Email == "":
		return "-"
	case u.Verified():
		return u.Email
	}
	return u.Email + " (unverified)"
}

// accountLine describes one account completely on one line: the subject is the only part that cannot be
// claimed, so it is always there.
func accountLine(u access.User) string {
	name := u.Name
	if name == "" {
		name = "-"
	}
	return fmt.Sprintf("subject %s, e-mail %s, name %s, %s (%s), last seen %s", cell(u.Sub), cell(emailNote(u)), cell(name),
		cell(access.Describe(u)), cell(u.Via), ago(u.LastSeen))
}

// resolve finds the account an identifier means and prints it, so the operator sees which account the command
// changes: two accounts can show the same name and address. A subject always selects its account. An e-mail
// address selects an account only when the identity provider verified it there; for an address that accounts
// only claim, they are listed with their subjects and nothing is changed.
func (c *cli) resolve(st *access.Store, ident string) (access.User, error) {
	if err := st.Err(); err != nil {
		return access.User{}, err
	}
	d := st.Load()
	u, err := d.Find(ident)
	verified, claimed := d.ByEmail(ident)
	if err != nil {
		if list := append(verified, claimed...); len(list) > 0 {
			fmt.Fprintf(c.errw, "%d account(s) have the e-mail %s:\n", len(list), cell(ident))
			for _, x := range list {
				fmt.Fprintln(c.errw, "  "+accountLine(x))
			}
			if access.IsEmailNotVerified(err) {
				fmt.Fprintln(c.errw, "The identity provider has not verified this address for any of them (or they last signed in before")
				fmt.Fprintln(c.errw, "the panel recorded that), and an unverified address can be typed in by the account's owner.")
			}
			fmt.Fprintln(c.errw, "Name the account by its subject instead, e.g.: fairy admin users set-role <subject> admin")
		}
		return access.User{}, friendly(err)
	}
	fmt.Fprintln(c.out, "Account: "+accountLine(u))
	if _, bySubject := d.Users[ident]; !bySubject && len(claimed) > 0 {
		fmt.Fprintf(c.out, "Note: %d other account(s) claim this e-mail without verification and are not changed:\n", len(claimed))
		for _, x := range claimed {
			fmt.Fprintln(c.out, "  "+accountLine(x))
		}
	}
	return u, nil
}

func dash(list []string) string {
	if len(list) == 0 {
		return "-"
	}
	return strings.Join(list, ", ")
}

// users runs `fairy admin users list|set-role|sites|remove`.
func (c *cli) users(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errUsage
	}
	if err := c.load(); err != nil {
		return 1, err
	}
	st := access.NewStore(c.cfg.DataDir)
	if err := st.EnsureSites(c.cfg.Sites()); err != nil {
		return 1, err
	}
	args, force := takeFlag(args, "force")
	args, yes := takeFlag(args, "yes")
	if len(args) == 0 {
		return 2, errUsage
	}
	switch args[0] {
	case "list":
		return c.usersList(st)
	case "set-role":
		args, sitesArg, hasSites := takeValue(args, "sites")
		if len(args) != 3 {
			return 2, errUsage
		}
		var sites []string
		if hasSites {
			var err error
			if sites, err = access.ParseSites(sitesArg); err != nil {
				return 2, err
			}
		}
		return c.usersSetRole(st, args[1], args[2], sites, force, yes)
	case "sites":
		if len(args) != 3 {
			return 2, errUsage
		}
		sites := []string{}
		if args[2] != "-" {
			var err error
			if sites, err = access.ParseSites(args[2]); err != nil {
				return 2, err
			}
		}
		return c.usersSetRole(st, args[1], access.RoleViewer, sites, force, yes)
	case "remove":
		if len(args) != 2 {
			return 2, errUsage
		}
		u, err := c.resolve(st, args[1])
		if err != nil {
			return 1, err
		}
		u, err = st.Remove(u.Sub, force, "cli by "+osUser()) // by subject: the account that was shown
		if err != nil {
			if err = friendly(err); strings.Contains(err.Error(), "last admin") {
				err = fmt.Errorf("%w, or add --force", err)
			}
			return 1, err
		}
		sessions, serr := adminstate.New(c.cfg.DataDir).RevokeUserSessions(u.Sub)
		toks, terr := tokens.NewStore(c.cfg.DataDir).RevokeOwner(u.Sub)
		c.log("user.remove", u.Label(), map[string]any{"sub": u.Sub, "was": access.Describe(u), "force": force,
			"sessions_revoked": sessions, "tokens_revoked": toks})
		fmt.Fprintf(c.out, "Removed %s (was %s). Revoked %d session(s) and %d API token(s).\n", cell(u.Label()),
			cell(access.Describe(u)), sessions, toks)
		fmt.Fprintln(c.out, "If they sign in again they wait for access (never admin via oidc.control_groups / bootstrap_admins).")
		if err := errors.Join(serr, terr); err != nil {
			return 1, err
		}
		return 0, nil
	}
	return 2, errUsage
}

func (c *cli) usersList(st *access.Store) (int, error) {
	d := st.Load()
	var rows [][]string
	for _, u := range d.SortedUsers() {
		rows = append(rows, []string{u.Sub, u.Name, emailNote(u), u.Role, dash(u.Sites), ago(u.LastSeen), u.Via})
	}
	if len(rows) == 0 {
		fmt.Fprintln(c.out, "No users yet: users appear here after their first sign-in.")
		if len(c.cfg.OIDC.ControlGroups)+len(c.cfg.OIDC.BootstrapAdmins) == 0 {
			fmt.Fprintln(c.out, "warning: oidc.control_groups and oidc.bootstrap_admins are empty, so nobody becomes admin by")
			fmt.Fprintln(c.out, "signing in. After your first sign-in run: fairy admin users set-role <your email or subject> admin")
		}
		return 0, nil
	}
	fmt.Fprintln(c.out, table(rows, []string{"SUBJECT", "NAME", "EMAIL", "ROLE", "SITES", "LAST SEEN", "HOW"}))
	if d.Admins() == 0 {
		fmt.Fprintln(c.out, "\nwarning: there is no admin. Make one with: fairy admin users set-role <email or subject> admin")
	}
	if slices.ContainsFunc(d.SortedUsers(), func(u access.User) bool { return u.Email != "" && !u.Verified() }) {
		fmt.Fprintln(c.out, "\n(unverified): the identity provider did not verify that address (or the user last signed in before the")
		fmt.Fprintln(c.out, "panel recorded it). The account's owner may have typed it in: name such an account by its SUBJECT.")
	}
	return 0, nil
}

// usersSetRole changes one account's role. It prints the account it found first, and asks before making it an
// admin (--yes skips the question): the address typed may belong to another account than the operator thinks.
func (c *cli) usersSetRole(st *access.Store, ident, role string, sites []string, force, yes bool) (int, error) {
	if !access.ValidRole(role) {
		return 2, errors.New("role must be admin, viewer or pending")
	}
	if role != access.RoleViewer && len(sites) > 0 {
		return 2, errors.New("only viewers have sites (admins see everything)")
	}
	found, err := c.resolve(st, ident)
	if err != nil {
		return 1, err
	}
	if role == access.RoleAdmin && found.Role != access.RoleAdmin && !yes &&
		!c.confirm("Make this account an admin? An admin controls every host and manages the users.") {
		fmt.Fprintln(c.errw, "Nothing changed (answer yes, or add --yes to skip the question).")
		return 1, nil
	}
	before, after, err := st.SetRole(found.Sub, role, sites, "cli by "+osUser(), force) // by subject: the account that was shown
	if err != nil {
		err = friendly(err)
		if strings.Contains(err.Error(), "unknown site") {
			err = fmt.Errorf("%w (create it first: fairy admin sites create NAME)", err)
		}
		if strings.Contains(err.Error(), "last admin") {
			err = fmt.Errorf("%w, or add --force", err)
		}
		return 1, err
	}
	c.log("user.set_role", after.Label(), map[string]any{"sub": after.Sub, "role": role, "sites": after.Sites,
		"before": access.Describe(before), "force": force})
	fmt.Fprintf(c.out, "%s: %s -> %s. The running panel applies it within a second.\n", cell(after.Label()),
		cell(access.Describe(before)), cell(access.Describe(after)))
	return 0, nil
}

// sites runs `fairy admin sites list|assign|create|rename|delete`.
func (c *cli) sites(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errUsage
	}
	if err := c.load(); err != nil {
		return 1, err
	}
	st := access.NewStore(c.cfg.DataDir)
	if err := st.EnsureSites(c.cfg.Sites()); err != nil {
		return 1, err
	}
	inConfig := func(name string) error {
		if slices.Contains(c.cfg.Sites(), name) {
			return fmt.Errorf("site %q is set in config.yaml (site: %s): change it there", name, name)
		}
		return nil
	}
	switch {
	case args[0] == "list" && len(args) == 1:
		return c.sitesList(st)
	case args[0] == "assign" && len(args) == 3:
		return c.sitesAssign(st, args[1], args[2])
	case args[0] == "create" && len(args) == 2:
		if err := st.CreateSite(args[1]); err != nil {
			return 1, friendly(err)
		}
		c.log("site.create", args[1], nil)
		fmt.Fprintf(c.out, "Created site %q.\n", args[1])
		return 0, nil
	case args[0] == "rename" && len(args) == 3:
		if err := inConfig(args[1]); err != nil {
			return 1, err
		}
		if err := st.RenameSite(args[1], args[2]); err != nil {
			return 1, friendly(err)
		}
		c.log("site.rename", args[1], map[string]any{"to": args[2]})
		fmt.Fprintf(c.out, "Renamed site %q to %q (users and hosts follow).\n", args[1], args[2])
		return 0, nil
	case args[0] == "delete" && len(args) == 2:
		if err := inConfig(args[1]); err != nil {
			return 1, err
		}
		if err := st.DeleteSite(args[1]); err != nil {
			return 1, friendly(err)
		}
		c.log("site.delete", args[1], nil)
		fmt.Fprintf(c.out, "Deleted site %q: its hosts are admin-only again (or back to their config.yaml site).\n", args[1])
		return 0, nil
	}
	return 2, errUsage
}

func (c *cli) sitesList(st *access.Store) (int, error) {
	d := st.Load()
	if len(d.Sites) == 0 {
		fmt.Fprintln(c.out, "No sites yet. Create one with: fairy admin sites create NAME")
		return 0, nil
	}
	var rows [][]string
	for _, s := range d.Sites {
		var viewers, hosts []string
		for _, u := range d.SortedUsers() {
			if u.Role == access.RoleViewer && slices.Contains(u.Sites, s) {
				viewers = append(viewers, u.Label())
			}
		}
		for t, site := range d.Assign {
			if site == s {
				hosts = append(hosts, t)
			}
		}
		slices.Sort(hosts)
		from := "panel"
		if slices.Contains(c.cfg.Sites(), s) {
			from = "config.yaml"
		}
		rows = append(rows, []string{s, from, dash(viewers), dash(hosts)})
	}
	fmt.Fprintln(c.out, table(rows, []string{"SITE", "FROM", "VIEWERS", "ASSIGNED IN THE PANEL"}))
	var defs [][]string
	for _, p := range c.cfg.Proxmox {
		if p.Site != "" {
			defs = append(defs, []string{"proxmox " + p.ID + " (nodes and guests)", p.Site})
		}
	}
	for _, a := range c.cfg.Agents {
		if a.Site != "" {
			defs = append(defs, []string{"agent " + a.ID, a.Site})
		}
	}
	for _, o := range c.cfg.Systems {
		if o.Site != "" {
			defs = append(defs, []string{"system " + o.ID, o.Site})
		}
	}
	if len(defs) > 0 {
		fmt.Fprintln(c.out, "\nDefaults from config.yaml (the panel's assignment wins):")
		fmt.Fprintln(c.out, table(defs, []string{"WHAT", "SITE"}))
	}
	fmt.Fprintln(c.out, "\nGuests follow their node; power sources and UPSes follow the hosts they are attached to.")
	fmt.Fprintln(c.out, "Anything without a site is visible to admins only.")
	return 0, nil
}

func (c *cli) sitesAssign(st *access.Store, target, site string) (int, error) {
	if site == "-" || site == "none" {
		site = ""
	}
	isSystem := !strings.HasPrefix(target, access.PowerPrefix) && !strings.HasPrefix(target, access.UPSPrefix)
	if h, err := health.Read(c.cfg.DataDir); isSystem && err == nil && len(h.SystemIDs) > 0 && !slices.Contains(h.SystemIDs, target) {
		fmt.Fprintf(c.errw, "warning: %q is not a known system id right now\n", target)
	}
	if site != "" && !st.Load().HasSite(site) {
		if err := st.CreateSite(site); err != nil {
			return 1, friendly(err)
		}
		c.log("site.create", site, nil)
		fmt.Fprintf(c.out, "Created site %q.\n", site)
	}
	prev, err := st.Assign(target, site)
	if err != nil {
		return 1, friendly(err)
	}
	c.log("site.assign", target, map[string]any{"site": site, "before": prev})
	if site == "" {
		fmt.Fprintf(c.out, "%s: back to its default site (config.yaml / node), or admin-only.\n", cell(target))
	} else {
		fmt.Fprintf(c.out, "%s is now in site %q. The running panel applies it within a second.\n", cell(target), site)
	}
	return 0, nil
}
