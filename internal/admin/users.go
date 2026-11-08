package admin

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy/internal/access"
	"github.com/n3ptune-cpu/Fairy/internal/apierr"
	"github.com/n3ptune-cpu/Fairy/internal/health"
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
		return c.usersSetRole(st, args[1], args[2], sites, force)
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
		return c.usersSetRole(st, args[1], access.RoleViewer, sites, force)
	case "remove":
		if len(args) != 2 {
			return 2, errUsage
		}
		u, err := st.Remove(args[1], force)
		if err != nil {
			if err = friendly(err); strings.Contains(err.Error(), "last admin") {
				err = fmt.Errorf("%w, or add --force", err)
			}
			return 1, err
		}
		c.log("user.remove", u.Label(), map[string]any{"sub": u.Sub, "was": access.Describe(u), "force": force})
		fmt.Fprintf(c.out, "Removed %s (was %s). If they sign in again they start as pending\n", u.Label(), access.Describe(u))
		fmt.Fprintln(c.out, "(or as admin, if they are in oidc.control_groups / oidc.bootstrap_admins).")
		return 0, nil
	}
	return 2, errUsage
}

func (c *cli) usersList(st *access.Store) (int, error) {
	d := st.Load()
	var rows [][]string
	for _, u := range d.SortedUsers() {
		rows = append(rows, []string{u.Sub, u.Name, u.Email, u.Role, dash(u.Sites), ago(u.LastSeen), u.Via})
	}
	if len(rows) == 0 {
		fmt.Fprintln(c.out, "No users yet: users appear here after their first sign-in.")
		if len(c.cfg.OIDC.ControlGroups)+len(c.cfg.OIDC.BootstrapAdmins) == 0 {
			fmt.Fprintln(c.out, "warning: oidc.control_groups and oidc.bootstrap_admins are empty, so nobody becomes admin by")
			fmt.Fprintln(c.out, "signing in. After your first sign-in run: fairy admin users set-role <your email> admin")
		}
		return 0, nil
	}
	fmt.Fprintln(c.out, table(rows, []string{"SUBJECT", "NAME", "EMAIL", "ROLE", "SITES", "LAST SEEN", "HOW"}))
	if d.Admins() == 0 {
		fmt.Fprintln(c.out, "\nwarning: there is no admin. Make one with: fairy admin users set-role <email> admin")
	}
	return 0, nil
}

func (c *cli) usersSetRole(st *access.Store, ident, role string, sites []string, force bool) (int, error) {
	if !access.ValidRole(role) {
		return 2, errors.New("role must be admin, viewer or pending")
	}
	if role != access.RoleViewer && len(sites) > 0 {
		return 2, errors.New("only viewers have sites (admins see everything)")
	}
	before, after, err := st.SetRole(ident, role, sites, "cli by "+osUser(), force)
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
	fmt.Fprintf(c.out, "%s: %s -> %s. The running panel applies it within a second.\n", after.Label(),
		access.Describe(before), access.Describe(after))
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
		fmt.Fprintf(c.out, "%s: back to its default site (config.yaml / node), or admin-only.\n", target)
	} else {
		fmt.Fprintf(c.out, "%s is now in site %q. The running panel applies it within a second.\n", target, site)
	}
	return 0, nil
}
