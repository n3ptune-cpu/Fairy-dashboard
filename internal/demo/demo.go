// Package demo: a realistic simulator for Proxmox, agents, power sources, UPSes and a Supermicro BMC.
//
// The simulator produces *raw* payloads (Proxmox cluster/resources JSON, agent /metrics JSON) and feeds them
// through the real normalisation, power-merge and API code. Authentication is bypassed with a fake user, so the
// server refuses to serve demo mode on anything but a loopback socket. NEVER expose a demo instance.
package demo

import (
	"os"

	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/poller"
)

var User = model.User{Sub: "demo-neptune", Name: model.P("Neptune"), PreferredUsername: model.P("neptune"),
	Email: model.P("neptune@example.invalid"), Groups: []string{"viewers", "controllers"}, Auth: "demo"}

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
	secrets := config.Secrets{SessionSecret: randTok() + randTok(), InsecureCookies: true}
	p, err := poller.New(cfg, secrets, NewBackend(cfg))
	if err != nil {
		return nil, config.Secrets{}, nil, err
	}
	return cfg, secrets, p, nil
}
