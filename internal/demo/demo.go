// Package demo is a realistic simulator for Proxmox, agents, power sources, UPSes and a Supermicro BMC.
//
// The simulator produces *raw* payloads (Proxmox cluster/resources JSON, agent /metrics JSON) and feeds them
// through the real normalisation, power-merge and API code. Authentication is bypassed with a fake user, so the
// server refuses to serve demo mode on anything but a loopback socket. NEVER expose a demo instance.
package demo

import (
	"log/slog"
	"os"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// User is the fake signed-in user of demo mode (in the control group, so admin; `--demo-as` or the switcher in
// the sidebar previews the panel as a viewer or a pending user instead).
var User = model.User{Sub: "demo-alice", Name: model.P("Alice"), PreferredUsername: model.P("alice"),
	Email: model.P("alice@example.invalid"), EmailVerified: true, Groups: []string{"viewers", "controllers"}, Auth: "demo"}

// Build returns the demo config, secrets and a poller wired to the simulator.
func Build(dir, publicURL string) (*config.Panel, config.Secrets, *poller.Poller, error) {
	register()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, config.Secrets{}, nil, err
	}
	cfg, err := Config(dir, publicURL)
	if err != nil {
		return nil, config.Secrets{}, nil, err
	}
	if err := SeedAudit(cfg.AuditLog); err != nil {
		return nil, config.Secrets{}, nil, err
	}
	if err := SeedUsers(cfg); err != nil {
		return nil, config.Secrets{}, nil, err
	}
	secrets := config.Secrets{SessionSecret: randTok() + randTok(), InsecureCookies: true}
	added, skipped, err := managed.NewStore(cfg.DataDir).Merge(cfg) // hosts added with Add host in an earlier demo run
	if err != nil {
		return nil, config.Secrets{}, nil, err
	}
	for _, w := range skipped {
		slog.Warn(w)
	}
	rememberPaired(added)
	p, err := poller.New(cfg, secrets, NewBackend(cfg))
	if err != nil {
		return nil, config.Secrets{}, nil, err
	}
	return cfg, secrets, p, nil
}

// SeedUsers fills the demo's users.json: sites Alice and Bob (named in the demo config), Alice as admin,
// Bob as a viewer of his site, someone waiting for access, and the rack UPS's power meter (no host link)
// assigned to Alice's site on the "Users page".
func SeedUsers(cfg *config.Panel) error {
	st := access.NewStore(cfg.DataDir)
	if err := st.EnsureSites(cfg.Sites()); err != nil {
		return err
	}
	for _, u := range []struct {
		sub, email, name, role string
		sites                  []string
	}{
		{User.Sub, model.Deref(User.Email), "Alice", access.RoleAdmin, nil},
		{"demo-bob", "bob@example.invalid", "Bob", access.RoleViewer, []string{"Bob"}},
		{"demo-carol", "carol@example.invalid", "Carol", access.RolePending, nil},
	} {
		id := access.Identity{Sub: u.sub, Email: u.email, EmailVerified: u.sub != "demo-carol", Name: u.name}
		if _, _, err := st.SeenIdentity(id, u.role, "demo"); err != nil { // Carol's address is only claimed: shown as unverified
			return err
		}
		if _, _, err := st.SetRole(u.sub, u.role, u.sites, "demo", true); err != nil {
			return err
		}
	}
	if st.Load().Assign[access.PowerPrefix+"ups-rack"] == "" {
		if _, err := st.Assign(access.PowerPrefix+"ups-rack", "Alice"); err != nil {
			return err
		}
	}
	return nil
}
