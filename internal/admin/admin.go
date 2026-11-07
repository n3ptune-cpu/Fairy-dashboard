// Package admin is the break-glass CLI: `fairy admin ...`.
//
// Run on the panel's host as the service user or root, e.g. while Pocket ID is down. It edits the panel's state
// files directly (the running server picks the changes up within a second) and writes every action to the audit
// log with source "cli" and the local OS user. It never creates a web session or any password login.
package admin

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nsxservices/fairy/internal/adminstate"
	"github.com/nsxservices/fairy/internal/audit"
	"github.com/nsxservices/fairy/internal/config"
	"github.com/nsxservices/fairy/internal/power"
	"github.com/nsxservices/fairy/internal/tokens"
	"github.com/nsxservices/fairy/internal/ups"
)

const usage = `fairy admin: break-glass administration (no web login needed)

usage: fairy admin [--config FILE] [--env-file FILE] <command> ...

  tokens create --name NAME --owner SUB|system [--scope read|control] [--expires-days N]
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

func osUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return fmt.Sprintf("uid %d", os.Getuid())
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

func ago(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	s := time.Since(*t).Seconds()
	switch {
	case s < 120:
		return fmt.Sprintf("%.0fs ago", s)
	case s < 7200:
		return fmt.Sprintf("%.0fm ago", s/60)
	case s < 172800:
		return fmt.Sprintf("%.1fh ago", s/3600)
	}
	return fmt.Sprintf("%.0fd ago", s/86400)
}

func agoISO(s string) string {
	if s == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "?"
	}
	return ago(&t)
}

func table(rows [][]string, head []string) string {
	w := make([]int, len(head))
	for _, r := range append([][]string{head}, rows...) {
		for i, c := range r {
			w[i] = max(w[i], len([]rune(c)))
		}
	}
	line := func(r []string) string {
		parts := make([]string, len(r))
		for i, c := range r {
			parts[i] = c + strings.Repeat(" ", w[i]-len([]rune(c)))
		}
		return strings.TrimRight(strings.Join(parts, "  "), " ")
	}
	dash := make([]string, len(head))
	for i := range head {
		dash[i] = strings.Repeat("-", w[i])
	}
	out := []string{line(head), line(dash)}
	for _, r := range rows {
		out = append(out, line(r))
	}
	return strings.Join(out, "\n")
}

// Main runs `fairy admin <args>` and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fairy admin", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	def := os.Getenv("FAIRY_CONFIG_FILE")
	if def == "" {
		def = config.DefaultConfigPath()
	}
	cfgPath := fs.String("config", def, "config.yaml")
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

// ---------------------------------------------------------------- tokens
func (c *cli) tokens(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errUsage
	}
	if err := c.load(); err != nil {
		return 1, err
	}
	store := tokens.NewStore(c.cfg.DataDir)
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("tokens create", flag.ContinueOnError)
		fs.SetOutput(c.errw)
		name := fs.String("name", "", "token name (1-64 chars)")
		owner := fs.String("owner", "", `an OIDC sub, or "system"`)
		scope := fs.String("scope", "read", "read or control")
		days := fs.Int("expires-days", 0, "expiry in days (0 = never)")
		if err := fs.Parse(args[1:]); err != nil {
			return 2, errUsage
		}
		if *name == "" || *owner == "" {
			return 2, errors.New("--name and --owner are required")
		}
		raw, r, err := store.Create(*name, *owner, *scope, "cli:"+osUser(), *days)
		if err != nil {
			return 2, err
		}
		var exp any
		expStr := "never"
		if r.ExpiresAt != nil {
			exp = r.ExpiresAt.Format(time.RFC3339)
			expStr = r.ExpiresAt.Format("2006-01-02")
		}
		c.log("token.create", r.Name, map[string]any{"id": r.ID, "owner": r.Owner, "scope": r.Scope, "expires_at": exp})
		fmt.Fprintf(c.out, "Created API token %q (id %s, scope %s, owner %s, expires %s)\n\n    %s\n\n", r.Name, r.ID, r.Scope, r.Owner, expStr, raw)
		fmt.Fprintln(c.out, "This is the only time the token is shown; only its SHA-256 hash is stored.")
		fmt.Fprintf(c.out, "Use it as:  curl -H 'Authorization: Bearer %s…' %s/api/summary\n", raw[:7], c.cfg.BaseURL())
		return 0, nil
	case "list":
		var rows [][]string
		for _, r := range store.List() {
			state := "active"
			if r.RevokedAt != nil {
				state = "revoked"
			} else if !r.Active() {
				state = "expired"
			}
			exp := "never"
			if r.ExpiresAt != nil {
				exp = r.ExpiresAt.Format("2006-01-02")
			}
			rows = append(rows, []string{r.ID, r.Name, r.Scope, r.Owner, state, "…" + r.Hint, r.CreatedAt.Format("2006-01-02"), exp, ago(r.LastUsed)})
		}
		if len(rows) == 0 {
			fmt.Fprintln(c.out, "No API tokens.")
		} else {
			fmt.Fprintln(c.out, table(rows, []string{"ID", "NAME", "SCOPE", "OWNER", "STATE", "ENDS", "CREATED", "EXPIRES", "LAST USED"}))
		}
		return 0, nil
	case "revoke":
		if len(args) != 2 {
			return 2, errUsage
		}
		r, err := store.Revoke(args[1])
		if errors.Is(err, tokens.ErrNotFound) {
			fmt.Fprintf(c.errw, "no active token with id or name %q\n", args[1])
			return 1, nil
		}
		if err != nil {
			return 1, err
		}
		c.log("token.revoke", r.Name, map[string]any{"id": r.ID})
		fmt.Fprintf(c.out, "Revoked API token %q (id %s). It stops working immediately.\n", r.Name, r.ID)
		return 0, nil
	}
	return 2, errUsage
}

// ---------------------------------------------------------------- sessions
func (c *cli) sessions(args []string) (int, error) {
	if len(args) == 0 {
		return 2, errUsage
	}
	if err := c.load(); err != nil {
		return 1, err
	}
	st := adminstate.New(c.cfg.DataDir)
	switch args[0] {
	case "list":
		var rows [][]string
		for _, s := range st.ListSessions() {
			who := s.Email
			if who == "" {
				who = s.Name
			}
			if who == "" {
				who = s.Sub
			}
			ip := s.IP
			if ip == "" {
				ip = "?"
			}
			rows = append(rows, []string{s.SID[:min(8, len(s.SID))] + "…", who, ip, agoISO(s.Created)})
		}
		if len(rows) == 0 {
			fmt.Fprintln(c.out, "No active sessions.")
		} else {
			fmt.Fprintln(c.out, table(rows, []string{"SESSION", "USER", "IP", "SIGNED IN"}))
		}
		return 0, nil
	case "revoke":
		if len(args) != 2 {
			return 2, errUsage
		}
		n, err := st.RevokeSession(strings.TrimSuffix(args[1], "…"))
		if err != nil {
			return 2, err
		}
		if n == 0 {
			fmt.Fprintf(c.errw, "no session starting with %q\n", args[1])
			return 1, nil
		}
		c.log("sessions.revoke", "", map[string]any{"prefix": args[1][:min(8, len(args[1]))], "count": n})
		fmt.Fprintf(c.out, "Revoked %d session(s). That browser must sign in with Pocket ID again.\n", n)
		return 0, nil
	case "revoke-all":
		n, err := st.RevokeAllSessions()
		if err != nil {
			return 1, err
		}
		c.log("sessions.revoke_all", "", map[string]any{"count": n})
		fmt.Fprintf(c.out, "Revoked %d session(s). Every browser must sign in with Pocket ID again.\n", n)
		return 0, nil
	}
	return 2, errUsage
}

// ---------------------------------------------------------------- kill switch / host / poll
func (c *cli) control(args []string) (int, error) {
	if len(args) == 0 || (args[0] != "disable" && args[0] != "enable") {
		return 2, errUsage
	}
	fs := flag.NewFlagSet("control", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	reason := fs.String("reason", "", "shown in the UI banner and the audit log")
	if err := fs.Parse(args[1:]); err != nil {
		return 2, errUsage
	}
	if err := c.load(); err != nil {
		return 1, err
	}
	disable := args[0] == "disable"
	if err := adminstate.New(c.cfg.DataDir).SetControlDisabled(disable, osUser()+" (cli)", *reason); err != nil {
		return 1, err
	}
	params := map[string]any{}
	if *reason != "" {
		params["reason"] = *reason
	}
	c.log("control."+args[0], "", params)
	if disable {
		fmt.Fprintln(c.out, "Control DISABLED: fan, power, Wake-on-LAN and polling changes are refused for everyone")
		fmt.Fprintln(c.out, "(sessions and API tokens alike). The UI shows a banner. Re-enable with: fairy admin control enable")
	} else {
		fmt.Fprintln(c.out, "Control enabled again.")
	}
	return 0, nil
}

func (c *cli) host(args []string) (int, error) {
	if len(args) != 3 || args[1] != "power-control" || (args[2] != "disable" && args[2] != "enable") {
		return 2, errUsage
	}
	if err := c.load(); err != nil {
		return 1, err
	}
	if err := adminstate.New(c.cfg.DataDir).SetPowerDisabled(args[0], args[2] == "disable"); err != nil {
		return 1, err
	}
	c.log("host.power_control."+args[2], args[0], nil)
	state := "enabled"
	if args[2] == "disable" {
		state = "disabled"
	}
	fmt.Fprintf(c.out, "Power control for %s: %s.\n", args[0], state)
	return 0, nil
}

func (c *cli) poll(args []string) (int, error) {
	if len(args) != 2 {
		return 2, errUsage
	}
	id, v := args[0], args[1]
	var value any
	switch v {
	case "default":
	case "paused":
		value = "paused"
	default:
		f, err := strconv.ParseFloat(strings.TrimSuffix(v, "s"), 64)
		if err != nil {
			return 2, errors.New("value must be seconds (1-300), 'default' or 'paused'")
		}
		if f < 1 || f > 300 {
			return 2, errors.New("interval must be between 1 and 300 seconds")
		}
		value = f
	}
	if err := c.load(); err != nil {
		return 1, err
	}
	h := c.healthData()
	if known, _ := h["system_ids"].([]any); len(known) > 0 {
		found := false
		for _, k := range known {
			found = found || k == id
		}
		if !found {
			fmt.Fprintf(c.errw, "warning: %q is not a known system id right now\n", id)
		}
	}
	if err := adminstate.New(c.cfg.DataDir).SetPoll(id, value); err != nil {
		return 1, err
	}
	c.log("poll.set", id, map[string]any{"interval_s": v})
	fmt.Fprintf(c.out, "Polling for %s: %s. The running panel applies it within a second.\n", id, v)
	return 0, nil
}

// ---------------------------------------------------------------- audit
func cut(v any, n int) string {
	t := ""
	if v != nil {
		t = fmt.Sprint(v)
	}
	if t == "" {
		t = "-"
	}
	r := []rune(t)
	if len(r) <= n {
		return t
	}
	return string(r[:n-1]) + "…"
}

func pad(s string, n int) string {
	if l := len([]rune(s)); l < n {
		return s + strings.Repeat(" ", n-l)
	}
	return s
}

func fmtEntry(e map[string]any) string {
	ts := fmt.Sprint(e["ts"])
	if len(ts) > 19 {
		ts = ts[:19]
	}
	ts = strings.Replace(ts, "T", " ", 1)
	s := fmt.Sprintf("%s  %s %s %s %s %v", ts, pad(cut(e["source"], 16), 16), pad(cut(e["actor"], 24), 24),
		pad(cut(e["action"], 26), 26), pad(cut(e["target"], 28), 28), e["outcome"])
	if ip, ok := e["ip"].(string); ok && ip != "" {
		s += "  [" + ip + "]"
	}
	return s
}

func (c *cli) audit(args []string) (int, error) {
	if len(args) == 0 || args[0] != "tail" {
		return 2, errUsage
	}
	fs := flag.NewFlagSet("audit tail", flag.ContinueOnError)
	fs.SetOutput(c.errw)
	n := fs.Int("n", 20, "entries")
	follow := fs.Bool("follow", false, "keep printing new entries")
	fs.BoolVar(follow, "f", false, "shorthand for --follow")
	asJSON := fs.Bool("json", false, "raw JSON lines")
	if err := fs.Parse(args[1:]); err != nil {
		return 2, errUsage
	}
	if err := c.load(); err != nil {
		return 1, err
	}
	show := func(e map[string]any) {
		if *asJSON {
			b, _ := json.Marshal(e)
			fmt.Fprintln(c.out, string(b))
		} else {
			fmt.Fprintln(c.out, fmtEntry(e))
		}
	}
	entries, _ := audit.Read(c.cfg.AuditLog, "", "", "", nil, max(1, *n))
	for i := len(entries) - 1; i >= 0; i-- {
		show(entries[i])
	}
	if !*follow {
		return 0, nil
	}
	f, err := os.Open(c.cfg.AuditLog)
	if err != nil {
		fmt.Fprintf(c.errw, "no audit log yet at %s\n", c.cfg.AuditLog)
		return 1, nil
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return 1, err
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if err != nil {
			return 1, err
		}
		var e map[string]any
		if json.Unmarshal(line, &e) == nil {
			show(e)
		}
	}
}

// ---------------------------------------------------------------- config check / health
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
	if !strings.HasPrefix(cfg.PublicURL, "https://") {
		problems = append(problems, "public_url "+cfg.BaseURL()+" is not https")
	}
	if len(cfg.OIDC.AllowedGroups) == 0 {
		problems = append(problems, "oidc.allowed_groups is empty: any Pocket ID user of this client may sign in")
	}
	if len(cfg.OIDC.ControlGroups) == 0 {
		notes = append(notes, "oidc.control_groups is empty: nobody can use control actions from the UI")
	}
	type ev struct{ what, env string }
	var envs []ev
	for _, a := range cfg.Agents {
		envs = append(envs, ev{"agent " + a.ID, a.TokenEnv})
	}
	for _, a := range cfg.Agents {
		if e := a.ControlEnv(); e != "" {
			envs = append(envs, ev{"agent " + a.ID + " control", e})
		}
	}
	for _, p := range cfg.Proxmox {
		e := p.TokenSecretEnv
		if e == "" {
			e = "FAIRY_PROXMOX_TOKEN_SECRET"
		}
		envs = append(envs, ev{"proxmox " + p.ID, e})
	}
	for _, p := range cfg.Proxmox {
		if p.PowerTokenSecretEnv != "" {
			envs = append(envs, ev{"proxmox " + p.ID + " power", p.PowerTokenSecretEnv})
		}
	}
	for _, e := range envs {
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

func (c *cli) healthData() map[string]any {
	b, err := os.ReadFile(filepath.Join(c.cfg.DataDir, "health.json"))
	if err != nil {
		return map[string]any{}
	}
	var h map[string]any
	if json.Unmarshal(b, &h) != nil {
		return map[string]any{}
	}
	return h
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func (c *cli) health() (int, error) {
	if err := c.load(); err != nil {
		return 1, err
	}
	h := c.healthData()
	if len(h) == 0 {
		fmt.Fprintf(c.out, "No %s: is the panel running (it writes this every 5 s)?\n", filepath.Join(c.cfg.DataDir, "health.json"))
		return 1, nil
	}
	written, _ := time.Parse(time.RFC3339Nano, str(h["written_at"]))
	age := time.Since(written).Seconds()
	state := "RUNNING"
	if age >= 30 {
		state = "STALE"
	}
	started := str(h["started_at"])
	if len(started) > 19 {
		started = started[:19]
	}
	fmt.Fprintf(c.out, "panel: %s (health written %.0fs ago, up since %s)\n", state, age, started)
	if cs := adminstate.New(c.cfg.DataDir).ControlDisabled(); cs != nil { // live state, not the 5 s snapshot
		r := cs.Reason
		if r == "" {
			r = "no reason"
		}
		fmt.Fprintf(c.out, "control: DISABLED by %s at %s (%s)\n", cs.By, cs.At.Format("2006-01-02T15:04:05"), r)
	} else {
		fmt.Fprintln(c.out, "control: enabled")
	}
	if sys, ok := h["systems"].(map[string]any); ok {
		var parts []string
		for _, k := range []string{"online", "stale", "offline", "stopped", "paused"} {
			parts = append(parts, fmt.Sprintf("%s %v", k, sys[k]))
		}
		fmt.Fprintln(c.out, "systems: "+strings.Join(parts, ", "))
	}
	var rows [][]string
	srcs, _ := h["sources"].([]any)
	for _, x := range srcs {
		s, _ := x.(map[string]any)
		st := "FAIL"
		if ok, _ := s["ok"].(bool); ok {
			st = "ok"
		}
		lat := "-"
		if l, ok := s["latency_ms"].(float64); ok {
			lat = fmt.Sprintf("%g ms", l)
		}
		rows = append(rows, []string{str(s["name"]), st, lat, agoISO(str(s["last_success"])), str(s["last_error"])})
	}
	for _, key := range []string{"power_sources", "ups"} {
		l, _ := h[key].([]any)
		prefix := "power:"
		if key == "ups" {
			prefix = "ups:"
		}
		for _, x := range l {
			s, _ := x.(map[string]any)
			rows = append(rows, []string{prefix + str(s["id"]), str(s["status"]), "-", "", str(s["error"])})
		}
	}
	fmt.Fprintln(c.out, table(rows, []string{"SOURCE", "STATE", "LATENCY", "LAST OK", "ERROR"}))
	if age >= 30 {
		return 1, nil
	}
	return 0, nil
}
