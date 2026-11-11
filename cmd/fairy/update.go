package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/panelupdate"
)

// `fairy update`: the panel host's manual update (release.CLI, the same as `fairy-agent update`), as root on
// the panel's machine:
//
//	fairy update --check   current version vs the latest Fairy-dashboard release
//	fairy update           download the latest release, verify its signature (key built into this binary) and
//	                       hash, install it as /usr/local/bin/fairy keeping fairy.old, restart fairy.service and
//	                       check it stays up and answers /healthz; otherwise put the old binary back.
//
// --yes skips the question, --force reinstalls the same or an older release. Nothing runs this automatically,
// except fairy-panel-update.service when an admin presses "Update panel" (--from-panel: no question, steps
// written to /var/lib/fairy-panel-update/status.json). Run by hand as root, it also installs or refreshes that
// unit and its polkit rule, so the button works from then on.
func update(args []string) int {
	if testBuild { // a panel without sign-in is never the installed one: it has nothing to update (testmode.go)
		fmt.Fprintln(os.Stderr, "fairy update:", testUpdateOff)
		return 1
	}
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only show the installed and the latest version")
	force := fs.Bool("force", false, "also reinstall the same version or install an older one")
	yes := fs.Bool("yes", false, "do not ask before installing")
	cfgPath := fs.String("config", "/etc/fairy/config.yaml", "panel config (updates.panel_repo, listen)")
	envFile := fs.String("env-file", "/etc/fairy/fairy.env", "panel env file (FAIRY_LISTEN)")
	fromPanel := fs.Bool("from-panel", false, "run by fairy-panel-update.service (the Update panel button): no question, status file")
	_ = fs.Parse(args)
	if *fromPanel && *force {
		fmt.Fprintln(os.Stderr, "fairy update: --from-panel never forces (no downgrades from the web page)")
		return 2
	}

	repo := release.PanelRepo
	var cfg *config.Panel
	if c, err := config.Load(*cfgPath); err == nil {
		cfg, repo = c, c.Updates.PanelRepo
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "fairy update: warning: cannot read", *cfgPath+":", err)
	}
	client, err := release.NewClient(repo, "fairy/"+version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fairy update:", err)
		return 1
	}
	key, err := release.ParsePublicKey(releaseKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fairy update:", err)
		return 1
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fairy update:", err)
		return 1
	}
	if !*check {
		if runtime.GOOS != "linux" {
			fmt.Fprintln(os.Stderr, "fairy update installs on the Linux panel host only (fairy update --check works everywhere)")
			return 1
		}
		if os.Geteuid() != 0 {
			fmt.Fprintln(os.Stderr, "fairy update must run as root (it replaces "+exe+" and restarts fairy.service)")
			return 1
		}
	}
	var status *panelupdate.Writer
	if *fromPanel {
		*yes = true
		status = &panelupdate.Writer{Path: panelupdate.StatusPath, From: version}
	} else if !*check {
		setupPanelButton()
	}
	healthURL := "http://" + healthAddr(cfg, *envFile) + "/healthz"
	cli := &release.CLI{Client: client, Key: key, Program: release.PanelProgram, Current: version, Target: exe,
		Asset: release.AssetName(release.PanelProgram, runtime.GOOS, runtime.GOARCH), Out: os.Stdout, In: os.Stdin,
		Check: *check, Yes: *yes, Force: *force,
		Installer: func(target, _ string) *release.Installer { return systemdInstaller(target, healthURL) },
		RunNew: func(path string) (string, error) {
			out, err := exec.Command(path, "version").Output()
			return string(out), err
		}}
	if status != nil {
		cli.Progress = status.Step
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := cli.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "fairy update:", err)
		return 1
	}
	return 0
}

// setupPanelButton installs or refreshes fairy-panel-update.service and its polkit rule (the Updates page's
// "Update panel" button). Problems are only reported: the manual update goes on.
func setupPanelButton() {
	run := func(name string, args ...string) error {
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %v %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	changed, err := panelupdate.Install(panelupdate.UnitPath, panelupdate.PolkitPath, run)
	for _, c := range changed {
		fmt.Println("Update panel button: installed", c)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fairy update: the Update panel button will not work yet:", err)
	} else if len(changed) > 0 {
		fmt.Println("Update panel button: ready (Updates page, admins).")
	}
}

// healthAddr is where the panel listens (FAIRY_LISTEN in fairy.env, else listen: in config.yaml, else the
// default); 0.0.0.0 becomes 127.0.0.1.
func healthAddr(cfg *config.Panel, envFile string) string {
	addr := "127.0.0.1:8080"
	if cfg != nil && cfg.Listen != "" {
		addr = cfg.Listen
	}
	if b, err := os.ReadFile(envFile); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "FAIRY_LISTEN="); ok {
				addr = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	if h, p, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(h); h == "" || (ip != nil && ip.IsUnspecified()) {
			addr = net.JoinHostPort("127.0.0.1", p)
		}
	}
	return addr
}

func systemctl(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func systemdInstaller(target, healthURL string) *release.Installer {
	hc := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}
	return &release.Installer{Target: target, HealthTimeout: 45 * time.Second, HealthPasses: 5,
		Start: func(ctx context.Context) error { return systemctl(ctx, "restart", "fairy.service") },
		Healthy: func(ctx context.Context) error {
			if err := systemctl(ctx, "is-active", "--quiet", "fairy.service"); err != nil {
				return errors.New("fairy.service is not active")
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
			resp, err := hc.Do(req)
			if err != nil {
				return fmt.Errorf("%s: %v", healthURL, err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				return fmt.Errorf("%s: HTTP %d", healthURL, resp.StatusCode)
			}
			return nil
		},
		Relabel: func(path string) {
			for _, p := range []string{"/usr/sbin/restorecon", "/sbin/restorecon"} {
				if _, err := os.Stat(p); err == nil {
					_ = exec.Command(p, path).Run()
					return
				}
			}
		}}
}
