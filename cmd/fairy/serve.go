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
	"runtime"
	"syscall"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/demo"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/enrollment"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/panelupdate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/updates"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/web"
)

// serveFlags are the `fairy serve` command-line options. Empty strings are filled in by resolve, after the env
// file was loaded, so FAIRY_CONFIG_FILE / FAIRY_LISTEN / FAIRY_LOG_LEVEL may also come from fairy.env.
type serveFlags struct {
	cfgPath, envFile, listen, logLevel string
	envFileSet                         bool // --env-file given explicitly (then an unreadable file is an error)
	demo                               bool
	demoAs                             string // demo only: preview as "viewer:Bob", "pending", ...
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
	skipped  []string    // hosts added in the panel that could not be loaded (and why)
}

// serve runs `fairy serve` and returns the process exit code.
func serve(args []string) int {
	f, ok := parseServeFlags(args)
	if !ok {
		return 2
	}
	testDefaults(&f) // a test build keeps to its own folder (does nothing in every other build, see testmode_off.go)
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
	switch {
	case testBuild:
		p, code = setupTest(f)
	case f.demo:
		p, code = setupDemo(f)
	default:
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
	srv.Settings.Check() // a damaged settings.json is moved aside and named in the log now, not at the first request
	srv.Updates = latestSource(p.cfg, f.demo, p.cfg.Updates.AgentRepo, release.AgentProgram)
	srv.PanelUpdates = latestSource(p.cfg, f.demo, p.cfg.Updates.PanelRepo, release.PanelProgram)
	srv.ManagedSkipped = p.skipped
	if key, err := release.ParsePublicKey(releaseKey); err == nil { // Add host checks the install script with it
		srv.Enroll.Scripts = enrollment.PinnedVerifier(p.cfg.Updates.AgentRepo, p.cfg.Enroll.AgentRelease, key)
	} else if !f.demo {
		slog.Warn("Add host is off: this build has no release key to check the Fairy-agent install script with", "error", err)
	}
	switch {
	case f.demo:
		srv.Enroll.Pair = demo.Pair
		srv.Enroll.Interval = demo.PairEvery
		srv.Enroll.Scripts = demo.Installers()
		if srv.Enroll.PanelAddr == "" {
			srv.Enroll.PanelAddr = "100.64.0.10" // the demo has no Tailscale; any address shows the command
			srv.Enroll.PanelSource, srv.Enroll.PanelVia = "demo", ""
		}
		to := srv.PanelUpdates.Latest().Version
		srv.PanelRunner = &panelupdate.Demo{From: version, To: to}
		demo.SeedNotes(srv.Notes)
	case runtime.GOOS == "linux":
		srv.PanelRunner = panelupdate.Systemd{}
		if !testBuild { // "Update panel" looks at the file this panel was started with before it starts the run
			srv.ConfigCheck = func() string { return configFileProblem(f.cfgPath) }
		}
	}
	listening := func() {}
	if testBuild { // sign-in off, for a browser that opens the address listening() prints
		if listening = testServer(srv, p); listening == nil {
			return 1
		}
	}
	if srv.Enroll.PanelAddr == "" {
		slog.Warn("Add host: this machine has no single Tailscale address; set enroll: {panel_address: 100.x.y.z} in config.yaml")
	} else {
		slog.Info("Add host: panel address", "address", srv.Enroll.PanelAddr, "source", srv.Enroll.PanelSource, "via", srv.Enroll.PanelVia)
	}
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
	pollerDone := make(chan struct{})
	go func() { defer close(pollerDone); p.poller.Run(ctx) }()
	slog.Info("fairy listening", "version", version, "addr", ln.Addr().String(), "public_url", p.cfg.BaseURL(),
		"demo", f.demo)
	listening()
	err = srv.Run(ctx, ln)
	stop() // also when the server failed: the poller stops, and writes what it still has to write
	// One wait for both writers, pollerStopWait in all: the poller (an agent's version of the last minute) and
	// the server's watcher (the notifications of the last seconds). They stop side by side.
	until := time.Now().Add(pollerStopWait)
	if !waitStopped(pollerDone, time.Until(until)) {
		slog.Warn("the poller did not finish within " + pollerStopWait.String() + " (a request to a machine hangs): stopping without it; " +
			"an agent version seen in the last minute may not be in state.json")
	}
	if !waitStopped(srv.Saved(), time.Until(until)) {
		slog.Warn("the notifications of the last seconds were not written within " + pollerStopWait.String() + ": stopping without that; " +
			"an event of the last ten seconds may not be in notifications.json")
	}
	if err != nil {
		slog.Error("server failed", "error", err)
		return 1
	}
	slog.Info("stopped")
	return 0
}

// pollerStopWait is how long a stop waits for what still has to be written: the poller's last write, and the
// notifications of the last seconds (web.Server.Saved). When the poller stops it lets the polls
// that are on their way end and then writes what waited to be written: an agent's version of the last minute
// (Poller.FlushVersions). Nothing waited for it, so the process was gone before that write, every time (second
// review, P11). The wait has an end, so that a poll that hangs cannot keep the panel from stopping: systemd
// waits 90 s before it kills, an admin at a console less.
const pollerStopWait = 5 * time.Second

// waitStopped waits for done to be closed, at most d, and reports whether it was.
func waitStopped(done <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
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
	demoAs := fs.String("demo-as", "", `demo only: preview as "viewer:Bob", "viewer:Alice", "pending" or "admin"`)
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

// latestSource is where the Updates page learns the newest release of a program (Fairy-agent, or the panel
// itself): GitHub (cached, in the background), a fixed answer in demo mode, nothing with
// updates.check_latest: false.
func latestSource(cfg *config.Panel, isDemo bool, repo, program string) updates.Source {
	if isDemo {
		if program == release.PanelProgram {
			return demo.PanelLatest()
		}
		return demo.Latest()
	}
	if !cfg.Updates.Check() {
		return nil
	}
	key, err := release.ParsePublicKey(releaseKey)
	if err != nil {
		key = nil // this panel build has no release key: the Updates page shows the latest tag, unverified
	}
	ch, err := updates.NewChecker(repo, program, "fairy/"+version, key)
	if err != nil {
		slog.Warn("updates: release check off", "error", err)
		return nil
	}
	return ch
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

// setupPanel loads the secrets and config.yaml and builds the panel from them (buildPanel).
func setupPanel(f serveFlags) (*panel, int) {
	secrets, err := config.LoadSecrets()
	if err != nil {
		slog.Error(err.Error() + " (set it in fairy.env, loaded by systemd's EnvironmentFile= or --env-file)")
		return nil, 1
	}
	cfg, err := config.LoadFull(f.cfgPath) // the one check of the file: `fairy update` asks the same before it swaps
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Error("config not found", "path", f.cfgPath)
		} else {
			slog.Error("invalid config", "path", f.cfgPath, "error", err)
		}
		return nil, 1
	}
	p, code := buildPanel(cfg, secrets)
	if p != nil {
		for _, w := range web.DeploymentWarnings(cfg, secrets) {
			slog.Warn(w)
		}
	}
	return p, code
}

// buildPanel adds the hosts added in the panel and builds the poller, from a config that was checked in full
// (config.LoadFull, config.ParseFull).
func buildPanel(cfg *config.Panel, secrets config.Secrets) (*panel, int) {
	// Hosts added in the panel (Add host): merged after config.yaml, which wins on the same id; each one is
	// validated like a hand-written entry, a bad one is skipped (logged, shown on the Add host dialog).
	//
	// A list that cannot be read at all stops the start, like state.json: managed-hosts.yaml (or a host's
	// tokens file) that is a symbolic link, a pipe or a folder, or a list that is unreadable or not YAML. The
	// panel used to start without the hosts, with one ERROR in the log: every machine added with Add host was
	// gone from the pages and from the runs, and an update to this version went through like that (audit 5,
	// r3-p4-03). Refused, `fairy update` rolls back by itself and the old version runs on.
	added, skipped, err := managed.NewStore(cfg.DataDir).Merge(cfg)
	if err != nil {
		slog.Error("refusing to start: the hosts added in the panel (Add host) cannot be read. The panel reads its state files only as plain files: "+
			"put the file itself where a link is (cp --remove-destination <target> <link>), or restore it from a backup; moved away, the panel starts without those hosts",
			"file", filepath.Join(cfg.DataDir, managed.FileName), "error", err)
		return nil, 1
	}
	for _, w := range skipped {
		slog.Warn(w)
	}
	if len(added) > 0 {
		slog.Info("hosts added in the panel", "count", len(added))
	}
	p, err := poller.New(cfg, secrets, nil)
	if err != nil {
		slog.Error("startup failed", "error", err)
		return nil, 1
	}
	return &panel{cfg: cfg, secrets: secrets, poller: p, skipped: skipped}, 0
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

// checkStateFiles refuses to start when a security state file is unreadable, not a plain file, owned by another
// user, or (users.json, state.json) empty or corrupt: the panel never treats such a file as empty (that could
// hand out roles again, lose the list of removed people or switch the kill switch off).
//
// sessions.json and tokens.json that are empty or corrupt do not keep the panel down (second review of audit 5's
// round 2, note 2). Read as nothing they say "nobody is signed in" and "no token is valid", which is the safe
// reading, and a panel that stays down for it cannot be reached by an owner who is not at the machine. The
// damaged file is moved aside first, as <name>.corrupt-<UTC time> (statefile.KeepAside: never over a copy that
// is already there, at most five copies of a file), so that it is kept (the next sign-in or token would
// otherwise find a damaged file it must not write over, and nobody could sign in), and one ERROR names it.
// When it cannot be moved (no name is free, the folder cannot be written) the panel starts all the same: the
// damaged file is not used for anything, the ERROR says that it is still in place, and that file cannot be
// written until someone moves it away.
func checkStateFiles(dataDir string) bool {
	ok := true
	for _, name := range []string{"users.json", "sessions.json", "tokens.json", "state.json"} {
		path := filepath.Join(dataDir, name)
		err := statefile.Check(path)
		if err == nil {
			continue
		}
		if means := map[string]string{"sessions.json": "nobody is signed in: everyone signs in again",
			"tokens.json": "no API token is valid: make new ones with fairy admin tokens create"}[name]; means != "" && statefile.IsDamaged(err) {
			if aside, rerr := statefile.KeepAside(path); rerr == nil {
				slog.Error(name+" is damaged: the panel starts without it ("+means+"). The damaged file is kept as "+aside, "error", err)
			} else {
				slog.Error(name+" is damaged and could not be moved aside: the panel starts without it ("+means+"), and THE DAMAGED FILE IS STILL IN PLACE at "+
					path+". Nothing can be written to it until it is moved away (nobody can sign in, or no token can be made): move it yourself and restart",
					"error", err, "move", rerr)
			}
			continue
		}
		slog.Error("refusing to start: fix this state file first", "error", err)
		ok = false
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
