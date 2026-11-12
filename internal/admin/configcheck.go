package admin

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
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
