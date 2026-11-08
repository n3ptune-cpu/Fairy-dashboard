package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/demo"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/poller"
	"github.com/n3ptune-cpu/Fairy/internal/power"
	"github.com/n3ptune-cpu/Fairy/internal/ups"
	"github.com/n3ptune-cpu/Fairy/internal/web"
)

// serveFlags are the `fairy serve` command-line options.
type serveFlags struct {
	cfgPath, envFile, listen, logLevel string
	demo                               bool
	demoAs                             string // demo only: preview as "viewer:Straw", "pending", ...
}

// panel is everything the HTTP server needs, from either the real config or the demo simulator.
type panel struct {
	cfg      *config.Panel
	secrets  config.Secrets
	poller   *poller.Poller
	demoUser *model.User // non-nil only in demo mode (authentication bypassed, loopback only)
}

// serve runs `fairy serve` and returns the process exit code.
func serve(args []string) int {
	f, ok := parseServeFlags(args)
	if !ok {
		return 2
	}
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(f.logLevel)) // an unknown level keeps the default (info)
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var p *panel
	var code int
	if f.demo {
		p, code = setupDemo(f)
	} else {
		p, code = setupPanel(f)
	}
	if p == nil {
		return code
	}
	if !ensureAuditLog(p.cfg.AuditLog) {
		return 1
	}
	srv := web.New(p.cfg, p.secrets, p.poller, p.demoUser, version)
	if f.demoAs != "" {
		if err := srv.SetDemoView(f.demoAs); err != nil {
			fmt.Fprintf(os.Stderr, "--demo-as: %v\n", err)
			return 2
		}
		slog.Warn("demo: previewing the panel as " + f.demoAs + " (switch in the sidebar)")
	}
	ln, err := net.Listen("tcp", f.listen)
	if err != nil {
		slog.Error("listen failed", "addr", f.listen, "error", err)
		return 1
	}
	go p.poller.Run(ctx)
	slog.Info("fairy listening", "version", version, "addr", ln.Addr().String(), "public_url", p.cfg.BaseURL(),
		"demo", f.demo)
	if err := srv.Run(ctx, ln); err != nil {
		slog.Error("server failed", "error", err)
		return 1
	}
	slog.Info("stopped")
	return 0
}

// parseServeFlags parses the `fairy serve` flags; false means the flags were invalid (or -h was asked for).
func parseServeFlags(args []string) (serveFlags, bool) {
	fs := flag.NewFlagSet("fairy serve", flag.ContinueOnError)
	cfgPath := fs.String("config", config.PathFromEnv(), "config.yaml")
	envFile := fs.String("env-file", config.DefaultEnvFile(), "secrets file, loaded if present (never overrides the environment)")
	listen := fs.String("listen", env("FAIRY_LISTEN", "127.0.0.1:8080"), "address:port (behind nginx)")
	demoMode := fs.Bool("demo", false, "simulated data, NO authentication (loopback preview only)")
	logLevel := fs.String("log-level", env("FAIRY_LOG_LEVEL", "info"), "debug|info|warn|error")
	demoAs := fs.String("demo-as", "", `demo only: preview as "viewer:Straw", "viewer:Neptune", "pending" or "admin"`)
	if err := fs.Parse(args); err != nil {
		return serveFlags{}, false
	}
	f := serveFlags{cfgPath: *cfgPath, envFile: *envFile, listen: *listen, logLevel: *logLevel, demo: *demoMode, demoAs: *demoAs}
	if f.demoAs != "" && !f.demo {
		fmt.Fprintln(os.Stderr, "--demo-as only works with --demo (a real panel takes roles from the Users page)")
		return serveFlags{}, false
	}
	if f.demo && !flagSet(fs, "listen") {
		f.listen = "127.0.0.1:8090"
	}
	return f, true
}

// setupDemo builds the simulator. Demo mode has no login, so it refuses any listen address but loopback.
func setupDemo(f serveFlags) (*panel, int) {
	if !loopback(f.listen) { // demo has no login: it may only ever listen on loopback
		fmt.Fprintln(os.Stderr, "Refusing to start demo mode: it bypasses authentication and only binds to 127.0.0.1 / ::1.")
		return nil, 2
	}
	dir, _ := filepath.Abs(env("FAIRY_DEMO_DIR", ".fairy-demo"))
	cfg, secrets, p, err := demo.Build(dir, env("FAIRY_DEMO_URL", "http://"+f.listen))
	if err != nil {
		slog.Error("demo setup failed", "error", err)
		return nil, 1
	}
	for i := 0; i < 3; i++ {
		slog.Warn("!!! DEMO MODE: simulated data, AUTHENTICATION BYPASSED, localhost only. Never expose it. !!!")
	}
	u := demo.User
	return &panel{cfg: cfg, secrets: secrets, poller: p, demoUser: &u}, 0
}

// setupPanel loads fairy.env, the secrets and config.yaml, validates the device configs and builds the poller.
func setupPanel(f serveFlags) (*panel, int) {
	if err := config.LoadEnvFile(f.envFile); err != nil {
		slog.Error("cannot read env file", "path", f.envFile, "error", err)
		return nil, 1
	}
	secrets, err := config.LoadSecrets()
	if err != nil {
		slog.Error(err.Error())
		return nil, 1
	}
	cfg, err := config.Load(f.cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Error("config not found", "path", f.cfgPath)
		} else {
			slog.Error("invalid config", "path", f.cfgPath, "error", err)
		}
		return nil, 1
	}
	if err := power.ValidateConfigs(cfg.PowerSources); err != nil {
		slog.Error("invalid config", "error", err)
		return nil, 1
	}
	if err := ups.ValidateConfigs(cfg.UPS); err != nil {
		slog.Error("invalid config", "error", err)
		return nil, 1
	}
	p, err := poller.New(cfg, secrets, nil)
	if err != nil {
		slog.Error("startup failed", "error", err)
		return nil, 1
	}
	for _, w := range web.DeploymentWarnings(cfg, secrets) {
		slog.Warn(w)
	}
	return &panel{cfg: cfg, secrets: secrets, poller: p}, 0
}

// ensureAuditLog creates the append-only JSON-lines audit log (mode 0600, directory 0700) before serving, so a
// permissions problem stops the panel at startup instead of losing audit entries later.
func ensureAuditLog(path string) bool {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.Error("audit log directory", "error", err)
		return false
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		slog.Error("audit log", "error", err)
		return false
	}
	f.Close()
	return true
}

// env returns the environment variable k, or def when it is unset or empty.
func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// loopback reports whether a host:port listens on loopback only ("localhost", 127.0.0.0/8 or ::1).
func loopback(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}

// flagSet reports whether the flag was given on the command line (not just defaulted).
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
