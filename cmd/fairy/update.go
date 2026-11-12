package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

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
// unit and its polkit rule, so the button works from then on: after the question was answered with yes (or
// with --yes), never before, and it says before the question what it will write.
func update(args []string) int {
	if testBuild { // a panel without sign-in is never the installed one: it has nothing to update (testmode.go)
		fmt.Fprintln(os.Stderr, "fairy update:", testUpdateOff)
		return 1
	}
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only show the installed and the latest version")
	force := fs.Bool("force", false, "also reinstall the same version or install an older one")
	yes := fs.Bool("yes", false, "do not ask before installing")
	cfgPath := fs.String("config", config.DefaultConfigPath(), "panel config; without it: the file fairy.service starts with (its --config or FAIRY_CONFIG_FILE)")
	envFile := fs.String("env-file", "/etc/fairy/fairy.env", "panel env file (FAIRY_LISTEN)")
	fromPanel := fs.Bool("from-panel", false, "run by fairy-panel-update.service (the Update panel button): no question, status file")
	_ = fs.Parse(args)
	if *fromPanel && *force {
		fmt.Fprintln(os.Stderr, "fairy update: --from-panel never forces (no downgrades from the web page)")
		return 2
	}

	// The panel's config: the file fairy.service starts with (serviceConfig), as the service reads it. A file
	// this installed version refuses (a key of a newer release, written before the update) still says where
	// the panel listens and where its releases are (config.Peek): the health check after the restart must ask
	// that address, not the default.
	unit := serviceUnit()
	cfgFile, cfgFrom, cfgKnown := serviceConfig(unit, *cfgPath, flagSet(fs, "config"))
	repo := release.PanelRepo
	cfgListen, cfgProblem, cfgStop := "", "", ""
	c, err := config.LoadFull(cfgFile) // the one check of the file: what `fairy serve` starts with
	switch {
	case !cfgKnown && !*check:
		cfgStop = configUnknown // nothing is installed over a file that could not be checked
	case err == nil:
		cfgListen, repo = c.Listen, c.Updates.PanelRepo
	case *check && errors.Is(err, os.ErrNotExist): // --check works everywhere, also where no panel is installed
	default:
		cfgProblem = configProblem(err)
		if cfgFile != config.DefaultConfigPath() {
			cfgProblem = cfgFile + ": " + cfgProblem // "config.yaml" in the messages is then this file
		}
		l, r, perr := config.Peek(cfgFile)
		if perr != nil && !*check {
			cfgStop = fmt.Sprintf("%s (%s) cannot be read at all: %s. The panel would not start again after the update's "+
				"restart: correct the file first. Nothing was changed.", cfgFile, cfgFrom, configProblem(perr))
			break
		}
		if cfgListen = l; r != "" {
			repo = r
		}
		fmt.Fprintf(os.Stderr, "fairy update: warning: this installed version (%s) cannot use %s (%s): %s\n", version, cfgFile, cfgFrom, configProblem(err))
		if !*check && !*fromPanel { // (the button's run does not go on: configStops, below)
			fmt.Fprintln(os.Stderr, configFirstNote)
		}
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
	}
	if !*check {
		// One update at a time on this host, from the first look to the end of a rollback: two runs that
		// overlap take each other's fairy.old away, and a release that does not come up then leaves no
		// working program (r2-p4-06).
		unlock, err := lockUpdate(updateLockPath(exe))
		if err != nil {
			return updateLocked(err, updateLockPath(exe), status, panelupdate.StatusPath)
		}
		defer unlock()
	}
	// A run by hand also sets up the Update panel button (a unit file, a polkit rule, the package polkitd when
	// it is missing). Nothing of that happens before the question is answered with yes: it is said first, and
	// done after an update that went through, or after a question of its own when there is nothing to update.
	in := &answers{r: bufio.NewReader(os.Stdin)}
	var buttonTodo []string
	if !*fromPanel && !*check {
		if buttonTodo = panelupdate.Pending(panelupdate.UnitPath, panelupdate.PolkitPath); len(buttonTodo) > 0 {
			fmt.Println("The Update panel button (Updates page) is not set up on this host, or its files are older than this version.")
			fmt.Println("After your yes this command will also:", strings.Join(buttonTodo, "; ")+".")
		}
	}
	if !*check {
		clearStoppedRun(filepath.Dir(exe), panelupdate.StatusPath, *fromPanel, time.Now(), panelupdate.UnitActivity)
	}
	if cfgStop != "" { // which file it is could not be told, or the file is not there or not YAML: both kinds of run stop
		fmt.Fprintln(os.Stderr, "fairy update:", cfgStop)
		if status != nil {
			status.Step("failed", "", clipTo(cfgStop, 300))
		}
		return 1
	}
	if why := configStops(*fromPanel, cfgProblem); why != "" && !*check {
		fmt.Fprintln(os.Stderr, "fairy update:", why)
		status.Step("failed", "", why)
		return 1
	}
	addr, from := healthAddr(cfgListen, *envFile, unit)
	healthURL := "http://" + addr + "/healthz"
	if !*check {
		// Before anything is replaced: the running panel must answer where the check after the restart will
		// ask. If it does not, this command has the wrong address (a --listen or FAIRY_LISTEN it cannot see),
		// and it would roll a good update back.
		if err := healthPreflight(healthURL, from, cfgProblem, serviceActive, healthOK); err != nil {
			fmt.Fprintln(os.Stderr, "fairy update:", err)
			if status != nil {
				status.Step("failed", "", err.Error())
			}
			return 1
		}
	}
	// The version that is about to be installed checks the config file too, before anything is replaced
	// (update_config.go). What it refuses stops the run, by hand and from the button: it would not start.
	asked := &newVersion{file: cfgFile}
	cli := &release.CLI{Client: client, Key: key, Program: release.PanelProgram, Current: version, Target: exe,
		Asset: release.AssetName(release.PanelProgram, runtime.GOOS, runtime.GOARCH), Out: os.Stdout, In: in,
		Check: *check, Yes: *yes, Force: *force,
		Installer: func(target, _ string) *release.Installer { return systemdInstaller(target, healthURL) },
		RunNew:    asked.runNew}
	cli.Progress = func(state, to, msg string) {
		if msg = asked.step(state, to, msg); status == nil {
			return
		}
		if cfgProblem != "" && asked.problem == "" && (state == "failed" || state == "rolled_back") { // the page shows the reason too
			msg = "config.yaml is not usable by the installed version (" + cfgProblem + "); " + msg
		}
		status.Step(state, to, msg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := cli.Run(ctx); err != nil {
		if asked.problem != "" {
			fmt.Fprintln(os.Stderr, "fairy update:", asked.stops())
			return 1
		}
		fmt.Fprintln(os.Stderr, "fairy update:", err)
		var rb *release.RolledBackError
		if cfgProblem != "" && errors.As(err, &rb) {
			fmt.Fprintf(os.Stderr, "fairy update: the installed version cannot use %s (%s), so it may not start either: "+
				"take out what it does not know, then `systemctl restart fairy`.\n", cfgFile, cfgProblem)
		}
		return 1
	}
	if len(buttonTodo) > 0 && buttonWanted(in, *yes, buttonTodo, os.Stdout) {
		setupPanelButton()
	}
	return 0
}

// errUpdateRunning: another `fairy update` on this host holds the lock.
var errUpdateRunning = errors.New("an update is already running on this host (the Update panel button, or `fairy update` in another terminal): " +
	"nothing was done. Wait for it to end and look at its result first")

// updateLockPath is the lock file: next to the program, where both a run by hand and the button's unit may
// write (the unit's ReadWritePaths). It stays there, empty.
func updateLockPath(exe string) string {
	return filepath.Join(filepath.Dir(exe), ".fairy-update.lock")
}

// updateLocked reports why the lock was not taken. The button's run also puts it into the status file for
// the page, unless that file belongs to a run that is still going (the one that holds the lock).
func updateLocked(err error, lockPath string, status *panelupdate.Writer, statusPath string) int {
	if !errors.Is(err, errUpdateRunning) {
		err = fmt.Errorf("cannot take the update lock %s: %w", lockPath, err)
	}
	fmt.Fprintln(os.Stderr, "fairy update:", err)
	if status != nil {
		if st, ok := panelupdate.Read(statusPath); !ok || st.Finished {
			status.Step("failed", "", err.Error())
		}
	}
	return 1
}

// answers hands release.CLI the terminal one line at a time and remembers what was answered, so that this
// command knows whether the CLI asked its question and what the answer was (the CLI only returns nil for
// "installed", "already up to date" and "no").
type answers struct {
	r     *bufio.Reader
	asked bool
	last  string
}

func (a *answers) Read(p []byte) (int, error) {
	line, err := a.r.ReadString('\n')
	a.asked, a.last = true, line
	n := copy(p, line)
	if n > 0 {
		return n, nil
	}
	return 0, err
}

func (a *answers) yes() bool {
	v := strings.ToLower(strings.TrimSpace(a.last))
	return v == "y" || v == "yes"
}

// buttonWanted decides, after a run that ended without an error, whether the Update panel button is set up
// now. The CLI asked and the answer was yes, or --yes was given: yes (it was said before the question). The
// answer was no: no, and "Nothing changed." is true. The CLI did not ask (already up to date): this asks.
func buttonWanted(in *answers, yes bool, todo []string, out io.Writer) bool {
	switch {
	case in.asked:
		return in.yes()
	case yes:
		return true
	}
	fmt.Fprintf(out, "Set up the Update panel button now (%s)? [y/N] ", strings.Join(todo, "; "))
	in.asked = false
	var buf [256]byte
	_, _ = in.Read(buf[:])
	if in.yes() {
		return true
	}
	fmt.Fprintln(out, "Nothing changed.")
	return false
}

// configFirstNote is said when config.yaml holds something the installed version refuses, most likely a key
// of the release that is about to be installed.
const configFirstNote = "  The order is: update first, then add new config keys, then restart. The update goes on (the new version " +
	"may know the key): the new version checks the file before anything is replaced, and the update stops there if it " +
	"refuses the file too. If the update is rolled back later, the old version will not start with this file until the key is taken out."

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// configStops is why a run by the Update panel button stops before anything is replaced when the installed
// version cannot use config.yaml ("": the run goes on).
//
// Such a file is not always a key of the release that is about to be installed. With a key that NO version
// knows (a typo) the new version refuses the file, the update is rolled back, the old version refuses it too,
// and the panel is down (second review of audit 5's round 2, note 4). Whoever pressed the button has only the
// page, and the page is then gone: so the button's run stops here, with the reason, while the panel still
// runs. A run by hand goes on as before, after its warning: there the new version may well know the key, and
// if not, the person is at a console, where the last message says what to take out.
func configStops(fromPanel bool, cfgProblem string) string {
	if !fromPanel || cfgProblem == "" {
		return ""
	}
	const lead, tail = "config.yaml is not usable by the installed version: ", ". Nothing was changed. " +
		"Correct config.yaml on the panel's host first (add the keys of a newer release after the update)."
	if room := 300 - len(lead) - len(tail) - len("…"); len(cfgProblem) > room { // the status file keeps 300 bytes, and the way out must fit
		for room > 0 && !utf8.RuneStart(cfgProblem[room]) {
			room--
		}
		cfgProblem = cfgProblem[:room] + "…"
	}
	return lead + cfgProblem + tail
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

// clearStoppedRun tidies up after an "Update panel" run that was stopped half-way (a reboot, a crash, systemd's
// time limit): what its download left next to the program is deleted, and a run by hand marks the status file
// as "stopped", so the Updates page no longer shows that run as running (the button's own run writes a new
// status anyway). Nothing of a run that can still be alive is touched.
func clearStoppedRun(dir, statusPath string, fromPanel bool, now time.Time, unit func() panelupdate.Activity) {
	for _, name := range panelupdate.RemoveStaleDownloads(dir, now) {
		fmt.Println("removed what a stopped update left behind:", filepath.Join(dir, name))
	}
	if fromPanel {
		return
	}
	if st, ok := panelupdate.Read(statusPath); !ok || st.Finished {
		return
	}
	if st, dead := panelupdate.SettleFile(statusPath, now, unit()); dead {
		fmt.Printf("An earlier Update panel run (started %s) stopped before it finished: marked as stopped.\n", st.StartedAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
}

// unitSettings is what systemd says about fairy.service: the command line it starts and the environment it
// gives it. Empty when systemd cannot be asked.
type unitSettings struct {
	Args     []string // ExecStart's arguments
	Env      []string // Environment=, as KEY=VALUE
	EnvFiles []string // EnvironmentFile=, in order
}

// serviceUnit asks systemd for fairy.service's command line and environment.
func serviceUnit() unitSettings {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", "fairy.service", "-p", "ExecStart", "-p", "Environment", "-p", "EnvironmentFiles").Output()
	if err != nil {
		return unitSettings{}
	}
	return parseUnit(string(out))
}

// parseUnit reads `systemctl show -p ExecStart -p Environment -p EnvironmentFiles`:
//
//	ExecStart={ path=/usr/local/bin/fairy ; argv[]=/usr/local/bin/fairy serve --config /etc/fairy/config.yaml --env-file  ; ignore_errors=no ; ... }
//	Environment=FAIRY_LISTEN=10.0.0.20:8080 OTHER=x
//	EnvironmentFiles=/etc/fairy/fairy.env (ignore_errors=no)
//
// systemd joins the arguments with spaces and drops empty ones (--env-file ""), so a value is the word after
// its flag unless that word is a flag itself.
func parseUnit(out string) unitSettings {
	var u unitSettings
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "ExecStart":
			if _, rest, ok := strings.Cut(v, "argv[]="); ok {
				rest, _, _ = strings.Cut(rest, " ; ")
				u.Args = strings.Fields(rest)
			}
		case "Environment":
			u.Env = append(u.Env, strings.Fields(v)...)
		case "EnvironmentFiles":
			if f, _, _ := strings.Cut(v, " ("); strings.TrimPrefix(f, "-") != "" {
				u.EnvFiles = append(u.EnvFiles, strings.TrimPrefix(f, "-"))
			}
		}
	}
	return u
}

// flag returns the value of --name (or -name, or --name=value) in the unit's command line.
func (u unitSettings) flag(name string) (string, bool) {
	for i, a := range u.Args {
		a = strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return strings.Trim(v, `"'`), true
		}
		if a == name && len(a) < len(u.Args[i]) {
			if i+1 < len(u.Args) && !strings.HasPrefix(u.Args[i+1], "-") {
				return strings.Trim(u.Args[i+1], `"'`), true
			}
			return "", true
		}
	}
	return "", false
}

// configUnknown is said when systemd does not tell how fairy.service is started and no --config was given.
const configUnknown = "cannot tell which config file fairy.service starts with (`systemctl show fairy.service` gave no command line), " +
	"so the file could not be checked before the restart. Nothing was changed. Name the file: fairy update --config /etc/fairy/config.yaml"

// serviceConfig is the config file fairy.service starts with after its restart, and where that is from (for
// messages): the file the update checks before it replaces anything, and reads `listen:` and
// `updates.panel_repo` from (r3-p4-02: it was always this command's default, /etc/fairy/config.yaml; with
// another file in the unit nothing was checked, and with FAIRY_CONFIG_FILE the button could never update).
//
// --config given to this command wins: the person says which file. Else the order is the service's own
// (serve.go, resolve): --config on the unit's command line; FAIRY_CONFIG_FILE from the unit's
// EnvironmentFile= (the last file that has it) or Environment=; FAIRY_CONFIG_FILE in the env file the service
// reads itself (--env-file of the unit, without the flag the default file; it never overrides the
// environment); else the default. known is false when systemd gave no command line for the unit and no
// --config was given: then nobody can tell, and the default is only a guess.
//
// The file is never taken from the running panel (a path handed from the unprivileged service to this root
// command would be read here and quoted in messages any user can read), and it is the unit as systemd has
// loaded it that decides which file the restart uses, not the process that runs now.
func serviceConfig(u unitSettings, given string, givenSet bool) (path, from string, known bool) {
	const key = "FAIRY_CONFIG_FILE"
	if givenSet {
		return given, "--config of this command", true
	}
	if len(u.Args) == 0 {
		return given, "the default; systemd did not say", false
	}
	if p, ok := u.flag("config"); ok && p != "" {
		return p, "--config of fairy.service", true
	}
	for i := len(u.EnvFiles) - 1; i >= 0; i-- {
		if v := envFileValues(u.EnvFiles[i], key); len(v) > 0 && v[len(v)-1] != "" {
			return v[len(v)-1], key + " in " + u.EnvFiles[i], true
		}
	}
	for i := len(u.Env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(u.Env[i], key+"="); ok && strings.Trim(v, `"'`) != "" {
			return strings.Trim(v, `"'`), "Environment=" + key + " of fairy.service", true
		}
	}
	own, ok := u.flag("env-file")
	if !ok {
		own = config.DefaultEnvFile()
	}
	if v := envFileValues(own, key); own != "" && len(v) > 0 && v[0] != "" {
		return v[0], key + " in " + own, true
	}
	return config.DefaultConfigPath(), "the default", true
}

// envFileValues are the values of key in an env file, in the file's order, read as the panel reads the file
// (config.LoadEnvFile: `export`, spaces and one pair of quotes around the value are dropped).
func envFileValues(path, key string) []string {
	var vals []string
	if b, err := os.ReadFile(path); err == nil && path != "" {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimPrefix(strings.TrimSpace(line), "export ")
			k, v, ok := strings.Cut(line, "=")
			if !ok || strings.TrimSpace(k) != key {
				continue
			}
			if v = strings.TrimSpace(v); len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
				v = v[1 : len(v)-1]
			}
			vals = append(vals, v)
		}
	}
	return vals
}

// envFileListen is FAIRY_LISTEN in an env file ("" when it is not there). A comment behind the value is not
// part of an address.
func envFileListen(path string) string {
	addr := ""
	if b, err := os.ReadFile(path); err == nil && path != "" {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimPrefix(strings.TrimSpace(line), "export "), "FAIRY_LISTEN="); ok {
				v, _, _ = strings.Cut(v, " #")
				addr = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	return addr
}

// healthAddr is where fairy.service listens after its restart, and where that is from (for messages). The
// order is the service's own (serve.go, listenAddr): --listen on the unit's command line; FAIRY_LISTEN from
// the unit's EnvironmentFile= (the last one wins) or Environment=, then from the env file the service reads
// itself; `listen:` in the config.yaml the service starts with; else the default. Without systemd's answer
// it is this command's own --env-file and --config, which are the unit's as shipped. 0.0.0.0 becomes 127.0.0.1.
func healthAddr(cfgListen, envFile string, u unitSettings) (addr, from string) {
	addr, from = defaultListen, "the default"
	if cfgListen != "" {
		addr, from = cfgListen, "listen: in config.yaml"
	}
	if p, ok := u.flag("config"); ok && p != "" { // the service starts with another config.yaml than --config names
		if l, _, err := config.Peek(p); err == nil && l != "" {
			addr, from = l, "listen: in "+p
		}
	}
	if v := envFileListen(envFile); v != "" {
		addr, from = v, "FAIRY_LISTEN in "+envFile
	}
	if p, ok := u.flag("env-file"); ok && p != "" {
		if v := envFileListen(p); v != "" {
			addr, from = v, "FAIRY_LISTEN in "+p
		}
	}
	for _, kv := range u.Env {
		if v, ok := strings.CutPrefix(kv, "FAIRY_LISTEN="); ok && v != "" {
			addr, from = strings.Trim(v, `"'`), "Environment=FAIRY_LISTEN of fairy.service"
		}
	}
	for _, p := range u.EnvFiles {
		if v := envFileListen(p); v != "" {
			addr, from = v, "FAIRY_LISTEN in "+p
		}
	}
	if v, ok := u.flag("listen"); ok && v != "" {
		addr, from = v, "--listen of fairy.service"
	}
	if h, p, err := net.SplitHostPort(addr); err == nil {
		if ip := net.ParseIP(h); h == "" || (ip != nil && ip.IsUnspecified()) {
			addr = net.JoinHostPort("127.0.0.1", p)
		}
	}
	return addr, from
}

// healthPreflight is asked before anything is replaced. While fairy.service runs, the panel must answer the
// health check at the address the check after the restart will use. If it does not, that address is wrong (or
// the panel is not well), and the check after the restart could only fail: a good update would be rolled
// back, and with a config.yaml the old version refuses the panel would stay down. A panel that is not running
// is not asked: an update may be what repairs it.
func healthPreflight(healthURL, from, cfgProblem string, active func() bool, healthy func(url string) error) error {
	if !active() {
		return nil
	}
	err := healthy(healthURL)
	if err == nil {
		return nil
	}
	msg := fmt.Sprintf("fairy.service is running, but the panel does not answer at %s (%v). That address is from %s. "+
		"The check after the update would fail in the same way and a good update would be rolled back, so nothing was changed. "+
		"Tell this command where the panel listens: `listen:` in config.yaml or FAIRY_LISTEN in fairy.env (the same file the service uses). "+
		"If the panel itself hangs: `systemctl stop fairy`, then run this again.",
		healthURL, err, from)
	if cfgProblem != "" {
		msg += " Also: the installed version cannot use config.yaml (" + cfgProblem + "). Update first, then add new config keys, then restart."
	}
	return errors.New(msg)
}

// serviceActive reports whether fairy.service runs now.
func serviceActive() bool {
	return systemctl(context.Background(), "is-active", "--quiet", "fairy.service") == nil
}

var healthClient = &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}

// healthOK asks the panel's /healthz once (never through a proxy).
func healthOK(url string) error {
	resp, err := healthClient.Get(url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
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
