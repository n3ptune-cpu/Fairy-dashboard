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

	"github.com/n3ptune-cpu/Fairy-agent/pkg/power"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/ups"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/demo"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/web"
)

// serveFlags are the `fairy serve` command-line options. Empty strings are filled in by resolve, after the env
// file was loaded, so FAIRY_CONFIG_FILE / FAIRY_LISTEN / FAIRY_LOG_LEVEL may also come from fairy.env.
type serveFlags struct {
	cfgPath, envFile, listen, logLevel string
	envFileSet                         bool // --env-file given explicitly (then an unreadable file is an error)
	demo                               bool
	demoAs                             string // demo only: preview as "viewer:Straw", "pending", ...
	logo                               string // --logo: overrides branding.logo (handy to try a logo in the demo)
}

// defaultListen is where the panel listens when neither --listen, FAIRY_LISTEN nor `listen:` say otherwise.
const defaultListen = "127.0.0.1:8080"

// loadEnv loads the env file (secrets) before anything reads the environment. The default file may be
// unreadable (0600 root, loaded by systemd's EnvironmentFile= instead): that is only a note. An explicit
// --env-file that cannot be read is an error.
func (f *serveFlags) loadEnv() error {
	if f.demo || f.envFile == "" {
		return nil
	}
	err := config.LoadEnvFile(f.envFile)
	if err != nil && !f.envFileSet && errors.Is(err, os.ErrPermission) {
		fmt.Fprintf(os.Stderr, "note: %s is not readable by this user; using the environment as given "+
			"(e.g. systemd EnvironmentFile=)\n", f.envFile)
		return nil
	}
	return err
}

// resolve fills in what the flags left empty from the environment (after loadEnv) and the defaults.
func (f *serveFlags) resolve() {
	if f.cfgPath == "" {
		f.cfgPath = config.PathFromEnv()
	}
	if f.logLevel == "" {
		f.logLevel = env("FAIRY_LOG_LEVEL", "info")
	}
	if f.demo && f.listen == "" {
		f.listen = "127.0.0.1:8090"
	}
}

// listenAddr is --listen, else $FAIRY_LISTEN, else `listen:` in config.yaml, else 127.0.0.1:8080.
func (f *serveFlags) listenAddr(cfg *config.Panel) string {
	switch {
	case f.listen != "":
		return f.listen
	case os.Getenv("FAIRY_LISTEN") != "":
		return os.Getenv("FAIRY_LISTEN")
	case cfg != nil && cfg.Listen != "":
		return cfg.Listen
	}
	return defaultListen
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
	if err := f.loadEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "cannot read env file %s: %v\n", f.envFile, err)
		return 1
	}
	f.resolve()
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
	if f.logo != "" {
		p.cfg.Branding.Logo = f.logo
	}
	audit.SetRotation(p.cfg.AuditRotation())
	if !ensureAuditLog(p.cfg.AuditLog) || !checkStateFiles(p.cfg.DataDir) {
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
	addr := f.listenAddr(p.cfg)
	if f.demo {
		addr = f.listen
	}
	if w := proxyWarning(addr, p.cfg); w != "" && !f.demo {
		slog.Warn(w)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("listen failed", "addr", addr, "error", err)
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
	cfgPath := fs.String("config", "", "config.yaml (default $FAIRY_CONFIG_FILE or "+config.DefaultConfigPath()+")")
	envFile := fs.String("env-file", config.DefaultEnvFile(), `secrets file, loaded if readable (never overrides the environment; "" = none)`)
	listen := fs.String("listen", "", "address:port (default $FAIRY_LISTEN, else listen: in config.yaml, else "+defaultListen+")")
	demoMode := fs.Bool("demo", false, "simulated data, NO authentication (loopback preview only)")
	logLevel := fs.String("log-level", "", "debug|info|warn|error (default $FAIRY_LOG_LEVEL or info)")
	logo := fs.String("logo", "", "logo image (PNG, JPEG, WebP or GIF, at most 1 MiB): overrides branding.logo; works with --demo")
	demoAs := fs.String("demo-as", "", `demo only: preview as "viewer:Straw", "viewer:Neptune", "pending" or "admin"`)
	if err := fs.Parse(args); err != nil {
		return serveFlags{}, false
	}
	f := serveFlags{cfgPath: *cfgPath, envFile: *envFile, envFileSet: flagSet(fs, "env-file"), listen: *listen,
		logLevel: *logLevel, demo: *demoMode, demoAs: *demoAs, logo: *logo}
	if f.demoAs != "" && !f.demo {
		fmt.Fprintln(os.Stderr, "--demo-as only works with --demo (a real panel takes roles from the Users page)")
		return serveFlags{}, false
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

// setupPanel loads the secrets and config.yaml, validates the device configs and builds the poller.
func setupPanel(f serveFlags) (*panel, int) {
	secrets, err := config.LoadSecrets()
	if err != nil {
		slog.Error(err.Error() + " (set it in fairy.env, loaded by systemd's EnvironmentFile= or --env-file)")
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
	if err := statefile.CheckOwner(path); err != nil {
		slog.Error("refusing to start: audit log", "error", err)
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

// checkStateFiles refuses to start when a security state file is unreadable, corrupt or owned by another user:
// the panel never treats such a file as empty (that could hand out roles again or lose revocations).
func checkStateFiles(dataDir string) bool {
	ok := true
	for _, name := range []string{"users.json", "sessions.json", "tokens.json", "state.json"} {
		if err := statefile.Check(filepath.Join(dataDir, name)); err != nil {
			slog.Error("refusing to start: fix this state file first", "error", err)
			ok = false
		}
	}
	return ok
}

// proxyWarning warns when the panel listens beyond loopback (nginx in another container) but trusts only
// loopback proxies: every request would then seem to come from nginx, so one client could use up the login
// rate limit for everyone and the audit log would show nginx's address.
func proxyWarning(addr string, cfg *config.Panel) string {
	if cfg == nil || loopback(addr) {
		return ""
	}
	for _, p := range cfg.Trusted() {
		if !p.Addr().IsLoopback() {
			return ""
		}
	}
	return "listening on " + addr + " but trusted_proxies only lists loopback: set trusted_proxies to nginx's " +
		"address (e.g. [10.0.0.5/32]) so rate limits and the audit log see the real client IP"
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
