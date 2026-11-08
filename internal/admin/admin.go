// Package admin is the break-glass CLI: `fairy admin ...`.
//
// Run on the panel's host as the service user or root, e.g. while Pocket ID is down. It edits the panel's state
// files directly (the running server picks the changes up within a second) and writes every action to the audit
// log with source "cli" and the local OS user. It never creates a web session or any password login.
package admin

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/n3ptune-cpu/Fairy/internal/audit"
	"github.com/n3ptune-cpu/Fairy/internal/config"
)

const usage = `fairy admin: break-glass administration (no web login needed)

usage: fairy admin [--config FILE] [--env-file FILE] <command> ...

  users list
  users set-role <email|subject> admin|viewer|pending [--sites A,B] [--force]
  users sites <email|subject> <site,site,...|->     make them a viewer of these sites
  users remove <email|subject> [--force]            (--force: even the last admin)
  sites list
  sites assign <system-id|power:ID|ups:ID> <site|->  (- = back to the default; creates the site)
  sites create <name> | sites rename <old> <new> | sites delete <name>
  tokens create --name NAME --owner SUB|system [--scope read|control] [--sites A,B] [--expires-days N]
  tokens list
  tokens revoke <id-or-name>
  sessions list
  sessions revoke <session-id-prefix>
  sessions revoke-all
  control disable [--reason TEXT]
  control enable
  host <system-id> power-control disable|enable
  poll <system-id> <seconds|default|paused>
  audit tail [-n 20] [--follow] [--json]
  config check
  health
`

type cli struct {
	cfgPath string
	out     io.Writer
	errw    io.Writer
	cfg     *config.Panel
}

func (c *cli) load() error {
	cfg, err := config.Load(c.cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("config not found: %s (use --config or FAIRY_CONFIG_FILE)", c.cfgPath)
		}
		return err
	}
	c.cfg = cfg
	return nil
}

func (c *cli) log(action, target string, params map[string]any) {
	ip := "local"
	if _, err := audit.Write(c.cfg.AuditLog, audit.Entry{Actor: osUser(), Action: action, Target: target, Params: params,
		Source: "cli", IP: &ip}); err != nil {
		fmt.Fprintf(c.errw, "warning: audit log write failed: %v\n", err)
	}
}

// Main runs `fairy admin <args>` and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fairy admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	cfgPath := fs.String("config", config.PathFromEnv(), "config.yaml")
	envFile := fs.String("env-file", config.DefaultEnvFile(), "env file loaded if present (never overrides)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 2
	}
	if err := config.LoadEnvFile(*envFile); err != nil && !errors.Is(err, os.ErrPermission) {
		fmt.Fprintf(stderr, "warning: %s: %v\n", *envFile, err)
	}
	c := &cli{cfgPath: *cfgPath, out: stdout, errw: stderr}
	var err error
	code := 0
	switch rest[0] {
	case "users":
		code, err = c.users(rest[1:])
	case "sites":
		code, err = c.sites(rest[1:])
	case "tokens":
		code, err = c.tokens(rest[1:])
	case "sessions":
		code, err = c.sessions(rest[1:])
	case "control":
		code, err = c.control(rest[1:])
	case "host":
		code, err = c.host(rest[1:])
	case "poll":
		code, err = c.poll(rest[1:])
	case "audit":
		code, err = c.audit(rest[1:])
	case "config":
		if len(rest) < 2 || rest[1] != "check" {
			fmt.Fprint(stderr, usage)
			return 2
		}
		return c.configCheck()
	case "health":
		code, err = c.health()
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		if code == 0 {
			code = 2
		}
	}
	return code
}

var errUsage = errors.New("invalid arguments (see `fairy admin help`)")
