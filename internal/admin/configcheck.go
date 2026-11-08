package admin

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/n3ptune-cpu/Fairy/internal/access"
	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/power"
	"github.com/n3ptune-cpu/Fairy/internal/ups"
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
	cfg, err := config.Load(c.cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(c.out, "FAIL  config not found: %s\n", c.cfgPath)
		} else {
			fmt.Fprintf(c.out, "FAIL  config %s is invalid:\n%v\n", c.cfgPath, err)
		}
		return 1
	}
	if err := power.ValidateConfigs(cfg.PowerSources); err != nil {
		fmt.Fprintf(c.out, "FAIL  config %s is invalid:\n%v\n", c.cfgPath, err)
		return 1
	}
	if err := ups.ValidateConfigs(cfg.UPS); err != nil {
		fmt.Fprintf(c.out, "FAIL  config %s is invalid:\n%v\n", c.cfgPath, err)
		return 1
	}
	notes = append(notes, fmt.Sprintf("config %s: valid", c.cfgPath))
	if _, err := config.LoadSecrets(); err != nil {
		problems = append(problems, "FAIRY_SESSION_SECRET: missing or shorter than 32 characters")
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
	problems, notes = adminCheck(cfg, problems, notes)
	for _, e := range secretEnvs(cfg) {
		if os.Getenv(e.env) != "" {
			notes = append(notes, fmt.Sprintf("%s (%s): present", e.env, e.what))
		} else {
			problems = append(problems, fmt.Sprintf("%s (%s): missing", e.env, e.what))
		}
	}
	on := "off"
	if os.Getenv("FAIRY_NOTIFY_URL") != "" {
		on = "on"
	}
	notes = append(notes, "notifications: "+on+" (FAIRY_NOTIFY_URL)")
	for _, n := range notes {
		fmt.Fprintln(c.out, "ok    "+n)
	}
	for _, p := range problems {
		fmt.Fprintln(c.out, "WARN  "+p)
	}
	if len(problems) > 0 {
		return 1
	}
	return 0
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
			"admin by signing in (after your first sign-in: fairy admin users set-role <your email> admin)")
	}
	return problems, notes
}
