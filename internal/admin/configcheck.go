package admin

import (
	"errors"
	"fmt"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/configutil"
	"os"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// secretEnv is an environment variable that must hold a secret, and what it is for.
type secretEnv struct{ what, env string }

// secretEnvs lists the secret variables the config refers to: agent read and control tokens, then Proxmox
// read-only and power tokens.
func secretEnvs(cfg *config.Panel) []secretEnv {
	var envs []secretEnv
	for _, a := range cfg.Agents {
		envs = append(envs, secretEnv{"agent " + a.ID, a.TokenEnv})
	}
	for _, a := range cfg.Agents {
		if e := a.ControlEnv(); e != "" {
			envs = append(envs, secretEnv{"agent " + a.ID + " control", e})
		}
	}
	for _, p := range cfg.Proxmox {
		e := p.TokenSecretEnv
		if e == "" {
			e = "FAIRY_PROXMOX_TOKEN_SECRET"
		}
		envs = append(envs, secretEnv{"proxmox " + p.ID, e})
	}
	for _, p := range cfg.Proxmox {
		if p.PowerTokenSecretEnv != "" {
			envs = append(envs, secretEnv{"proxmox " + p.ID + " power", p.PowerTokenSecretEnv})
		}
	}
	return envs
}

// configCheck validates config.yaml and the environment. Secrets are reported as present/missing only.
func (c *cli) configCheck() int {
	var problems, notes []string
	cfg, err := config.LoadFull(c.cfgPath) // the one check of the file: what `fairy serve` starts with
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(c.out, "FAIL  config not found: %s\n", c.cfgPath)
		} else {
			fmt.Fprintf(c.out, "FAIL  config %s is invalid:\n%v\n", c.cfgPath, err)
		}
		return 1
	}
	notes = append(notes, fmt.Sprintf("config %s: valid", c.cfgPath))
	if p := config.SessionSecretProblem(os.Getenv("FAIRY_SESSION_SECRET")); p != "" {
		// too short, the example value or one character repeated: `fairy serve` does not start with it
		problems = append(problems, "FAIRY_SESSION_SECRET: "+p)
	} else {
		notes = append(notes, "FAIRY_SESSION_SECRET: present")
	}
	if cfg.OIDC.Issuer == "" {
		problems = append(problems, "oidc.issuer is empty: there is no Pocket ID to sign in with")
	} else {
		notes = append(notes, fmt.Sprintf("oidc.issuer: %s  (callback %s/auth/callback)", cfg.OIDC.Issuer, cfg.BaseURL()))
	}
	if os.Getenv("FAIRY_OIDC_CLIENT_SECRET") != "" {
		notes = append(notes, "FAIRY_OIDC_CLIENT_SECRET: present")
	} else {
		notes = append(notes, "FAIRY_OIDC_CLIENT_SECRET: not set (public client: PKCE only)")
	}
	if !strings.HasPrefix(cfg.PublicURL, "https://") {
		problems = append(problems, "public_url "+cfg.BaseURL()+" is not https")
	}
	if len(cfg.OIDC.AllowedGroups) == 0 {
		problems = append(problems, "oidc.allowed_groups is empty: any Pocket ID user of this client may sign in")
	}
	limit := fmt.Sprintf("control_rate_per_min: %d control requests a minute for one admin", cfg.ControlRatePerMin)
	if cfg.ControlRatePerMin != config.DefaultControlRatePerMin { // the example file used to set 6: such a file keeps it
		limit += fmt.Sprintf(" (set in config.yaml; without the line it is %d)", config.DefaultControlRatePerMin)
	}
	notes = append(notes, limit)
	problems, notes = approvalCheck(cfg, problems, notes)
	problems, notes = adminCheck(cfg, problems, notes)
	problems, notes = managedCheck(cfg, problems, notes)
	problems, inferred := nodeLinkCheck(cfg, problems)
	okLines, said := wakeUpdateCheck(cfg)
	notes, inferred = append(notes, okLines...), append(inferred, said...)
	inferred = append(inferred, systemIDCheck(cfg)...)
	okLines, said = powerLinkCheck(cfg)
	notes, inferred = append(notes, okLines...), append(inferred, said...)
	for _, e := range secretEnvs(cfg) {
		if os.Getenv(e.env) != "" {
			notes = append(notes, fmt.Sprintf("%s (%s): present", e.env, e.what))
		} else {
			problems = append(problems, fmt.Sprintf("%s (%s): missing", e.env, e.what))
		}
	}
	problems = append(problems, config.EnvProblems()...) // a comment behind a value in fairy.env
	on := "off"
	if os.Getenv("FAIRY_NOTIFY_URL") != "" {
		on = "on"
	}
	notes = append(notes, "notifications: "+on+" (FAIRY_NOTIFY_URL)")
	for _, n := range notes {
		fmt.Fprintln(c.out, "ok    "+n)
	}
	for _, n := range cfg.Ignored() { // read, not used, and never a reason for exit code 1
		fmt.Fprintln(c.out, "note  "+n)
	}
	for _, n := range inferred { // a node worked out from an id_prefix, a short off_after_s: said, and never a reason for exit code 1
		fmt.Fprintln(c.out, "note  "+n)
	}
	for _, p := range problems {
		fmt.Fprintln(c.out, "WARN  "+p)
	}
	if len(problems) > 0 {
		return 1
	}
	return 0
}

// approvalCheck explains the approval: block: the Pocket ID client "Fairy PC actions", with which a person
// approves a fan, power or update action for the machine itself to verify (README: "The check on the machine
// itself"). The block is optional. Its secret is reported as present / not set only.
func approvalCheck(cfg *config.Panel, problems, notes []string) ([]string, []string) {
	if !cfg.ApprovalOn() {
		return problems, append(notes, "approval: not set. Machines that check a passkey approval themselves cannot be switched on "+
			"for that check or controlled from this panel (approval: {client_id: ...}, the Client ID of a separate client for PC actions in Pocket ID)")
	}
	iss := cfg.ApprovalIssuer()
	from := "approval.issuer"
	if cfg.Approval.Issuer == "" {
		from = "oidc.issuer"
	}
	notes = append(notes, fmt.Sprintf("approval.client_id: %s  (at %s, from %s; it must be the Client ID of the separate client for PC actions in Pocket ID, not the sign-in client's)", cfg.Approval.ClientID, iss, from),
		fmt.Sprintf("approval callback to register in Pocket ID for that client: %s  (Requires Re-Authentication: ON, PKCE: ON)", cfg.ApprovalCallback()))
	if why := cfg.ApprovalIssuerProblem(); why != "" { // the words of the Approval dialog's "Not ready" and of the log at start
		problems = append(problems, "approval: "+why+"; it is "+iss+". The machines only accept an https issuer, so the check "+
			"cannot be switched on for any of them")
	}
	if os.Getenv(config.ApprovalSecretEnv) != "" {
		notes = append(notes, config.ApprovalSecretEnv+": present")
	} else {
		notes = append(notes, config.ApprovalSecretEnv+": not set (public client: PKCE only)")
	}
	notes = append(notes, fmt.Sprintf("approval.users: %d more than the panel's admins and a host's owner; approval.owners: %d owner labels "+
		"with a Pocket ID user (they may approve on a machine that is switched on for the check from now on; a machine that already has "+
		"its list is changed only on the machine)", len(cfg.Approval.Users), len(cfg.Approval.Owners)),
		fmt.Sprintf("approval.run_approval_wait_s: %d (kept for later: \"Update unpowered hosts\" does not ask for approvals yet, a machine that checks them is in no run)",
			cfg.Approval.RunApprovalWaitS))
	return problems, notes
}

// managedCheck reports the hosts added in the panel (Add host): managed-hosts.yaml and their tokens in the data
// folder, validated like config.yaml's own entries (a host with the same id in config.yaml wins).
func managedCheck(cfg *config.Panel, problems, notes []string) ([]string, []string) {
	st := managed.NewStore(cfg.DataDir)
	c := *cfg
	c.Agents, c.Systems = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems)
	added, skipped, err := st.Merge(&c)
	if err != nil {
		return append(problems, "hosts added in the panel: "+err.Error()), notes
	}
	for _, e := range added {
		where := e.Agent.URL
		if e.Host.Node != "" {
			where += ", on node " + e.Host.Node
		}
		notes = append(notes, fmt.Sprintf("managed host %s (%s): valid, tokens present  (%s)", e.Host.ID, e.Host.Name, where))
	}
	if len(added) == 0 && len(skipped) == 0 {
		notes = append(notes, "managed hosts (Add host): none ("+st.Path()+")")
	}
	return append(problems, skipped...), notes
}

// nodeLinkCheck is the warning `fairy serve` logs at its start for an agent on a Proxmox node (node:) when the
// configuration does not say which node that is although a `systems:` entry is written under an id the node
// can have: after a restart of the panel while that machine is off, its wol: or bmc_power: is then not found
// (poller.NodeWarnings has the rule). The hosts added in the panel are part of the question, as at the start;
// what is wrong with their list itself, managedCheck has said.
//
// inferred is the other thing the start logs: one line for every agent whose node was worked out from an
// id_prefix (poller.NodeInferred), so that no link is guessed without a word, and one for every agent whose
// node is not decided and for which there is no warning (poller.NodeUndecided: no `systems:` entry is written
// for its node), which says where the rows put that agent. They are notes, not warnings.
func nodeLinkCheck(cfg *config.Panel, problems []string) (out, inferred []string) {
	c := *cfg
	c.Agents, c.Systems, c.Proxmox = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems), slices.Clone(cfg.Proxmox)
	_, _, _ = managed.NewStore(cfg.DataDir).Merge(&c)
	return append(problems, poller.NodeWarnings(&c)...), append(poller.NodeInferred(&c), poller.NodeUndecided(&c)...)
}

// systemIDCheck is one note for every `systems:` entry of config.yaml whose id is no machine's: not the id of
// an agent (config.yaml's or one added in the panel), and not of the form a node or a guest of one of the
// proxmox: entries has (<id_prefix>node-<name>, <id_prefix>qemu-<vmid>, <id_prefix>lxc-<vmid>). Such an entry
// is read and counts for nothing, which is what a typing mistake in the id looks like: the machine it was
// written for runs with the panel's own values, and nothing else says so. A note, not a warning: whether a
// node or guest of that name exists is only known once its Proxmox API has answered, and an entry may be
// written before its machine is added.
func systemIDCheck(cfg *config.Panel) []string {
	c := *cfg
	c.Agents, c.Systems, c.Proxmox = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems), slices.Clone(cfg.Proxmox)
	_, _, _ = managed.NewStore(cfg.DataDir).Merge(&c)
	known := map[string]bool{}
	for _, a := range c.Agents {
		known[a.ID] = true
	}
	var out []string
	for _, o := range cfg.Systems {
		fits := known[o.ID]
		for _, p := range c.Proxmox {
			for _, kind := range []string{"node-", "qemu-", "lxc-"} {
				fits = fits || (strings.HasPrefix(o.ID, p.Prefix()+kind) && len(o.ID) > len(p.Prefix()+kind))
			}
		}
		if fits {
			continue
		}
		what := "the entry counts for no machine"
		if o.WakeUpdate != nil {
			what = "the entry and its wake_update: block count for no machine (the machine it was written for has the panel's own timings)"
		}
		out = append(out, fmt.Sprintf("system %s: no agent has this id, and it is not the id of a node or a guest of a proxmox: entry "+
			"(<id_prefix>node-<name>, <id_prefix>qemu-<vmid>, <id_prefix>lxc-<vmid>): %s. A typing mistake in the id?", o.ID, what))
	}
	return out
}

// powerLinkCheck is about the plugs an agent relays, as the panel's entry of that agent links them
// (power_links: {<device id on the agent>: <system id>}) and leaves them out of the total
// (power_exclude_from_total: [<device id>]). One ok line per agent says what is set, and notes say what cannot
// work:
//
//   - a device id that no device can have (not an id as the agent's own power_sources take them);
//   - power_links to an id that is no machine's (no agent's id, not of the form a node or a guest has);
//   - power_links to the id of a node whose agent the configuration does not decide: the watts are on the
//     node's row while that row is there, and on no row after a restart of the panel with the machine off
//     (poller.ShownID follows a machine to its agent's row only where the configuration says whose it is).
//
// Whether the agent has a device with that id is not known here: this command reads files and asks no
// machine. The running panel knows it from the agent's answers and says it once in its log (power.Manager,
// UpdateRemote: "... names devices the agent does not report"); the ok line says where to look.
//
// Notes, not warnings: nothing is refused, and the exit code stays as it is.
func powerLinkCheck(cfg *config.Panel) (ok, said []string) {
	c := *cfg
	c.Agents, c.Systems, c.Proxmox = slices.Clone(cfg.Agents), slices.Clone(cfg.Systems), slices.Clone(cfg.Proxmox)
	_, _, _ = managed.NewStore(cfg.DataDir).Merge(&c)
	agents := map[string]bool{}
	for _, a := range c.Agents {
		agents[a.ID] = true
	}
	upsEntries := map[string]bool{}
	for _, raw := range cfg.UPS {
		if id, _ := raw["id"].(string); id != "" {
			upsEntries[id] = true
		}
	}
	_, undecided := poller.NodeAgents(&c)
	form := func(id string, kinds ...string) bool {
		for _, p := range c.Proxmox {
			for _, kind := range kinds {
				if strings.HasPrefix(id, p.Prefix()+kind) && len(id) > len(p.Prefix()+kind) {
					return true
				}
			}
		}
		return false
	}
	sorted := func(m map[string]string) []string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return keys
	}
	for _, a := range cfg.Agents {
		if len(a.PowerLinks)+len(a.PowerExcludeFromTotal) == 0 {
			continue
		}
		var parts []string
		for _, dev := range sorted(a.PowerLinks) {
			parts = append(parts, dev+" -> "+a.PowerLinks[dev])
		}
		line := "agent " + a.ID + ": devices it relays"
		if len(parts) > 0 {
			line += ": linked " + strings.Join(parts, ", ")
		}
		if ex := slices.Sorted(slices.Values(a.PowerExcludeFromTotal)); len(ex) > 0 {
			line += "; not in the total: " + strings.Join(ex, ", ")
		}
		ok = append(ok, line+" (the device ids are the agent's own: the running panel says in its log when the agent reports no device with such an id)")
		for _, dev := range slices.Sorted(slices.Values(append(sorted(a.PowerLinks), a.PowerExcludeFromTotal...))) {
			if !configutil.ValidID(dev) {
				said = append(said, fmt.Sprintf("agent %s: %q in power_links / power_exclude_from_total is not an id a device can have "+
					"(lowercase letters, digits and dashes, as in the agent's own power_sources): nothing is linked or left out for it", a.ID, dev))
			}
		}
		for _, dev := range sorted(a.PowerLinks) {
			to := a.PowerLinks[dev]
			switch {
			case to == "" || agents[to]:
			case len(undecided[to]) > 0:
				said = append(said, fmt.Sprintf("agent %s: power_links: device %s is linked to the node %s, and the configuration does not say which agent's machine that node is "+
					"(it can be the node of %s): the watts are on the node's row while it is there, and on no row after a restart of the panel with the machine off. "+
					"To say whose node it is, write links_to: %s in that agent's entry", a.ID, dev, to, strings.Join(undecided[to], " or of "), to))
			case form(to, "node-", "qemu-", "lxc-"):
			default:
				said = append(said, fmt.Sprintf("agent %s: power_links: device %s is linked to %s, which is no agent's id and not the id of a node or a guest of a proxmox: entry: "+
					"its watts are on no row. A typing mistake in the id?", a.ID, dev, to))
			}
		}
	}
	return ok, said
}

// wakeUpdateCheck lists the timings of "Update unpowered hosts" that are not the defaults a reader would
// expect: the panel's off_after_s when it is not 900, and every machine that has timings of its own (the
// wake_update: block of its systems: entry). ok lines are what is set; said is one note for every time
// below the default after which a machine counts as off, with what that risks. A note is no warning: the
// value is allowed, it is the owner's decision, and it does not change the exit code.
func wakeUpdateCheck(cfg *config.Panel) (ok, said []string) {
	const def = 900
	mins := func(s int) string {
		if s%60 == 0 {
			return fmt.Sprintf("%d min", s/60)
		}
		return fmt.Sprintf("%d s", s)
	}
	risk := func(what string, s int) string {
		return fmt.Sprintf("%s is %d (the default is %d): a machine that restarts for longer than %s can be taken for off by a run, "+
			"which then wakes it, updates it and switches it off as a machine it woke", what, s, def, mins(s))
	}
	if v := cfg.WakeUpdate.OffAfterS; v != 0 && v != def {
		ok = append(ok, fmt.Sprintf("wake_update.off_after_s: %d (a machine counts as off after %s without an answer; without the line it is %d)", v, mins(v), def))
		if v < def {
			said = append(said, risk("wake_update.off_after_s", v))
		}
	}
	for _, o := range cfg.Systems {
		w := o.WakeUpdate
		if w == nil || (w.OffAfterS == 0 && w.ShutdownDelayS == 0) {
			continue
		}
		var parts []string
		if w.OffAfterS != 0 {
			parts = append(parts, fmt.Sprintf("off_after_s %d (it counts as off after %s without an answer)", w.OffAfterS, mins(w.OffAfterS)))
		}
		if w.ShutdownDelayS != 0 {
			parts = append(parts, fmt.Sprintf("shutdown_delay_s %d (the countdown of its shutdown in a run)", w.ShutdownDelayS))
		}
		ok = append(ok, fmt.Sprintf("system %s: wake_update of its own: %s", o.ID, strings.Join(parts, ", ")))
		if w.OffAfterS != 0 && w.OffAfterS < def {
			said = append(said, risk("system "+o.ID+": wake_update.off_after_s", w.OffAfterS))
		}
	}
	return ok, said
}

// adminCheck reports who can administer the panel. Roles live in users.json (Users page / `fairy admin users`);
// oidc.control_groups and oidc.bootstrap_admins only make someone admin while users.json has no admin at all.
func adminCheck(cfg *config.Panel, problems, notes []string) ([]string, []string) {
	st := access.NewStore(cfg.DataDir)
	d := st.Load()
	bootstrap := len(cfg.OIDC.ControlGroups)+len(cfg.OIDC.BootstrapAdmins) > 0
	switch {
	case st.Err() != nil:
		problems = append(problems, "users.json cannot be read: nobody gets access until it is fixed (see SECURITY-FIXES.md, M6)")
	case d.Admins() > 0:
		notes = append(notes, fmt.Sprintf("admins in users.json: %d (roles and sites are managed on the Users page)", d.Admins()))
	case bootstrap:
		notes = append(notes, "users.json has no admin yet: the first sign-in through oidc.control_groups / oidc.bootstrap_admins becomes admin")
	default:
		problems = append(problems, "users.json has no admin and oidc.control_groups / oidc.bootstrap_admins are empty: nobody becomes "+
			"admin by signing in (after your first sign-in: fairy admin users set-role <your email or subject> admin)")
	}
	return problems, notes
}
