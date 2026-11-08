// Command fairy is the Fairy homelab panel: `fairy serve`, `fairy admin ...`, `fairy version`.
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

	"github.com/n3ptune-cpu/Fairy/internal/admin"
	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/demo"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/poller"
	"github.com/n3ptune-cpu/Fairy/internal/power"
	"github.com/n3ptune-cpu/Fairy/internal/ups"
	"github.com/n3ptune-cpu/Fairy/internal/web"
)

var version = "dev" // set with -ldflags "-X main.version=..."

const usage = `Fairy homelab panel

usage:
  fairy serve [--config FILE] [--env-file FILE] [--listen 127.0.0.1:8080]
  fairy serve --demo [--listen 127.0.0.1:8090]   simulated data, NO login, loopback only
  fairy admin ...                                break-glass CLI (fairy admin help)
  fairy version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		os.Exit(serve(os.Args[2:]))
	case "admin":
		os.Exit(admin.Main(os.Args[2:], os.Stdout, os.Stderr))
	case "version", "--version", "-v":
		fmt.Printf("fairy %s (%s, %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

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

func serve(args []string) int {
	fs := flag.NewFlagSet("fairy serve", flag.ContinueOnError)
	cfgPath := fs.String("config", config.PathFromEnv(), "config.yaml")
	envFile := fs.String("env-file", config.DefaultEnvFile(), "secrets file, loaded if present (never overrides the environment)")
	listen := fs.String("listen", env("FAIRY_LISTEN", "127.0.0.1:8080"), "address:port (behind nginx)")
	demoMode := fs.Bool("demo", false, "simulated data, NO authentication (loopback preview only)")
	logLevel := fs.String("log-level", env("FAIRY_LOG_LEVEL", "info"), "debug|info|warn|error")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(*logLevel))
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		cfg     *config.Panel
		secrets config.Secrets
		p       *poller.Poller
		err     error
	)
	if *demoMode {
		if !fsSet(fs, "listen") {
			*listen = "127.0.0.1:8090"
		}
		if !loopback(*listen) { // demo has no login: it may only ever listen on loopback
			fmt.Fprintln(os.Stderr, "Refusing to start demo mode: it bypasses authentication and only binds to 127.0.0.1 / ::1.")
			return 2
		}
		dir, _ := filepath.Abs(env("FAIRY_DEMO_DIR", ".fairy-demo"))
		cfg, secrets, p, err = demo.Build(dir, env("FAIRY_DEMO_URL", "http://"+*listen))
		if err != nil {
			slog.Error("demo setup failed", "error", err)
			return 1
		}
		for i := 0; i < 3; i++ {
			slog.Warn("!!! DEMO MODE: simulated data, AUTHENTICATION BYPASSED, localhost only. Never expose it. !!!")
		}
	} else {
		if err := config.LoadEnvFile(*envFile); err != nil {
			slog.Error("cannot read env file", "path", *envFile, "error", err)
			return 1
		}
		if secrets, err = config.LoadSecrets(); err != nil {
			slog.Error(err.Error())
			return 1
		}
		if cfg, err = config.Load(*cfgPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				slog.Error("config not found", "path", *cfgPath)
			} else {
				slog.Error("invalid config", "path", *cfgPath, "error", err)
			}
			return 1
		}
		if err := power.ValidateConfigs(cfg.PowerSources); err != nil {
			slog.Error("invalid config", "error", err)
			return 1
		}
		if err := ups.ValidateConfigs(cfg.UPS); err != nil {
			slog.Error("invalid config", "error", err)
			return 1
		}
		if p, err = poller.New(cfg, secrets, nil); err != nil {
			slog.Error("startup failed", "error", err)
			return 1
		}
		for _, w := range web.DeploymentWarnings(cfg, secrets) {
			slog.Warn(w)
		}
	}
	// append-only JSON-lines audit log, 0600
	if err := os.MkdirAll(filepath.Dir(cfg.AuditLog), 0o700); err != nil {
		slog.Error("audit log directory", "error", err)
		return 1
	}
	if f, err := os.OpenFile(cfg.AuditLog, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); err != nil {
		slog.Error("audit log", "error", err)
		return 1
	} else {
		f.Close()
	}
	var demoUser *model.User
	if *demoMode {
		u := demo.User
		demoUser = &u
	}
	srv := web.New(cfg, secrets, p, demoUser, version)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		slog.Error("listen failed", "addr", *listen, "error", err)
		return 1
	}
	go p.Run(ctx)
	slog.Info("fairy listening", "version", version, "addr", ln.Addr().String(), "public_url", cfg.BaseURL(),
		"demo", *demoMode)
	if err := srv.Run(ctx, ln); err != nil {
		slog.Error("server failed", "error", err)
		return 1
	}
	slog.Info("stopped")
	return 0
}

func fsSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}
