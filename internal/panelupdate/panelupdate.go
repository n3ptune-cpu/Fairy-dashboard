// Package panelupdate is the Updates page's "Update panel" button: the panel (user fairy, no root) starts the
// root oneshot unit fairy-panel-update.service (allowed by one polkit rule, no arguments from the web request),
// which runs `fairy update --yes --from-panel` and writes its steps to a status file the panel shows, also
// after its own restart. Installed and kept up to date by `fairy update` (as root on the panel host).
package panelupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/deploy"
)

// Paths on the panel host.
const (
	Unit       = "fairy-panel-update.service"
	UnitPath   = "/etc/systemd/system/" + Unit
	PolkitPath = "/etc/polkit-1/rules.d/49-fairy-panel.rules"
	StatusPath = "/var/lib/fairy-panel-update/status.json"
)

// Status is the status file: the latest run of the unit.
type Status struct {
	State     string    `json:"state"` // checking, downloading, staged, restarting, done, up_to_date, rolled_back, failed
	From      string    `json:"from"`
	To        string    `json:"to,omitempty"`
	Message   string    `json:"message"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Finished  bool      `json:"finished"`
}

// Finished states. "stopped" is never written by a run: it is what an unfinished status becomes once the run
// that wrote it cannot be alive any more (Settled).
func finished(state string) bool {
	switch state {
	case "done", "up_to_date", "rolled_back", "failed", "stopped":
		return true
	}
	return false
}

// How long a run can live, and when what it left behind counts as dead.
const (
	// RunLimit is the unit's TimeoutStartSec (deploy/fairy-panel-update.service; a test keeps the two equal):
	// `fairy update` gives itself 15 minutes and may need two more to put the old program back, so it can
	// report its own failure before systemd stops it.
	RunLimit = 18 * time.Minute
	// StaleAfter: an unfinished status older than this is not a running update, whatever systemd says.
	StaleAfter = RunLimit + 2*time.Minute
	// DownloadStaleAfter: a ".download-*" file next to the program that was last written longer ago belongs to
	// no running download (a download ends after 10 minutes at most).
	DownloadStaleAfter = 15 * time.Minute
)

// Activity is what systemd says about the update unit.
type Activity int

const (
	ActivityUnknown Activity = iota // systemctl gave no usable answer: only the status file's age decides
	ActivityRunning                 // the unit is starting or running
	ActivityNone                    // the unit is not running (inactive or failed)
)

// UnitActivity asks `systemctl is-active` about the update unit (any user may ask).
func UnitActivity() Activity {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", Unit).Output() // exit status 3 = not active: the word decides
	return parseActivity(string(out))
}

func parseActivity(out string) Activity {
	switch strings.TrimSpace(out) {
	case "active", "activating", "deactivating", "reloading":
		return ActivityRunning
	case "inactive", "failed":
		return ActivityNone
	}
	return ActivityUnknown
}

// Dead says whether an unfinished status is what a stopped run left behind (a reboot, a crash, systemd's time
// limit): it has no start time, it is older than any run can be, or the unit is not running.
func (st Status) Dead(now time.Time, act Activity) bool {
	if st.Finished {
		return false
	}
	if st.StartedAt.IsZero() || now.Sub(st.StartedAt) > StaleAfter {
		return true
	}
	return act == ActivityNone
}

// Settled returns st, or for a dead run the finished status "stopped" that says where and when it stopped.
// UpdatedAt stays the time of the run's last step.
func (st Status) Settled(now time.Time, act Activity) Status {
	if !st.Dead(now, act) {
		return st
	}
	at := ""
	if !st.UpdatedAt.IsZero() {
		at = ", " + st.UpdatedAt.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	st.Message = clip(fmt.Sprintf("stopped before it finished (while %s%s)", stepName(st.State), at), 300)
	st.State, st.Finished = "stopped", true
	return st
}

func stepName(state string) string {
	switch state {
	case "checking", "downloading", "restarting":
		return state
	case "staged":
		return "installing"
	}
	return "starting"
}

// SettleFile rewrites the status file when it holds a dead run, so the file says "stopped" from then on
// (`fairy update` by hand as root does this). It returns the dead run's status as it was, and whether there was one.
func SettleFile(path string, now time.Time, act Activity) (Status, bool) {
	st, ok := Read(path)
	if !ok || !st.Dead(now, act) {
		return Status{}, false
	}
	if err := writeFile(path, st.Settled(now, act)); err != nil {
		fmt.Fprintln(os.Stderr, "fairy update: status file:", err)
	}
	return st, true
}

// RemoveStaleDownloads deletes what a stopped download left next to the program: regular files named
// ".download-*" in dir that were last written more than DownloadStaleAfter ago. It returns their names.
func RemoveStaleDownloads(dir string, now time.Time) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var removed []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".download-") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) <= DownloadStaleAfter {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			removed = append(removed, e.Name())
		}
	}
	return removed
}

// Writer records the unit's steps (used by `fairy update --from-panel`, as root).
type Writer struct {
	Path string
	From string
	mu   sync.Mutex
	st   Status
}

// Step records one step (release.CLI's Progress).
func (w *Writer) Step(state, to, msg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now().UTC()
	if w.st.StartedAt.IsZero() {
		w.st.StartedAt, w.st.From = now, w.From
	}
	w.st.State, w.st.Message, w.st.UpdatedAt, w.st.Finished = state, clip(msg, 300), now, finished(state)
	if to != "" {
		w.st.To = to
	}
	if err := writeFile(w.Path, w.st); err != nil {
		fmt.Fprintln(os.Stderr, "fairy update: status file:", err)
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func writeFile(path string, st Status) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Read returns the last run's status (ok=false when the unit never ran).
func Read(path string) (Status, bool) {
	var st Status
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 64<<10 || json.Unmarshal(b, &st) != nil {
		return Status{}, false
	}
	return st, true
}

// Runner starts the unit and reads its status (the real one, or the demo's simulation).
type Runner interface {
	// Ready says whether the button can work here, and why not.
	Ready() (bool, string)
	Start(ctx context.Context) error
	Status() (Status, bool)
}

// Systemd is the real Runner.
type Systemd struct {
	StatusPath string
	// Active asks systemd whether the unit runs (nil: UnitActivity).
	Active func() Activity
}

// Ready: the unit is installed (by `fairy update` as root, once).
func (s Systemd) Ready() (bool, string) {
	if _, err := os.Stat(UnitPath); err != nil {
		return false, "one-time setup missing: run `fairy update` once as root on the panel host (it installs " + Unit + " and its polkit rule)"
	}
	return true, ""
}

// Start asks systemd to start the unit (polkit allows exactly this for the user fairy); it does not wait.
func (s Systemd) Start(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "start", "--no-block", Unit).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "Access denied") || strings.Contains(msg, "authentication") {
			msg += " (the polkit rule " + PolkitPath + " is missing or polkit is not installed: run `fairy update` as root once)"
		}
		return errors.New("systemctl start " + Unit + ": " + clip(msg, 300))
	}
	return nil
}

// Status reads the status file. An unfinished run counts as running only while it can be alive: what a
// stopped run left behind is returned as the finished state "stopped" (Settled), so the page shows it as that
// and the button works again.
func (s Systemd) Status() (Status, bool) {
	p := s.StatusPath
	if p == "" {
		p = StatusPath
	}
	st, ok := Read(p)
	if !ok || st.Finished {
		return st, ok
	}
	now, act := time.Now(), ActivityUnknown
	if !st.Dead(now, act) { // young enough to be alive: systemd knows
		ask := UnitActivity
		if s.Active != nil {
			ask = s.Active
		}
		act = ask()
	}
	return st.Settled(now, act), true
}

// Install writes the unit and the polkit rule (root, from `fairy update`) and reloads systemd. It returns what
// it changed, for the CLI's output.
func Install(unitPath, polkitPath string, run func(name string, args ...string) error) ([]string, error) {
	var changed []string
	put := func(path string, data []byte) error {
		if b, err := os.ReadFile(path); err == nil && string(b) == string(data) {
			return nil
		}
		if err := os.WriteFile(path+".new", data, 0o644); err != nil {
			return err
		}
		if err := os.Rename(path+".new", path); err != nil {
			return err
		}
		changed = append(changed, path)
		return nil
	}
	if err := put(unitPath, deploy.PanelUpdateUnit); err != nil {
		return changed, err
	}
	if _, err := os.Stat(filepath.Dir(polkitPath)); err != nil {
		if run != nil && run("apt-get", "install", "-y", "-q", "polkitd") == nil {
			changed = append(changed, "installed polkitd")
		}
		if _, err := os.Stat(filepath.Dir(polkitPath)); err != nil {
			return changed, errors.New("polkit is not installed (apt install polkitd), so the panel cannot start the update unit")
		}
	}
	if err := put(polkitPath, deploy.PanelPolkitRules); err != nil {
		return changed, err
	}
	if len(changed) > 0 && run != nil {
		if err := run("systemctl", "daemon-reload"); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// Demo simulates the unit in demo mode: downloading, staged, restarting, done.
type Demo struct {
	From, To string
	mu       sync.Mutex
	st       Status
	ok       bool
}

// Ready: always in the demo.
func (d *Demo) Ready() (bool, string) { return true, "" }

// Start runs the simulation in the background.
func (d *Demo) Start(context.Context) error {
	d.mu.Lock()
	d.st = Status{State: "checking", From: d.From, To: d.To, Message: "asking GitHub for the latest release", StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	d.ok = true
	d.mu.Unlock()
	go func() {
		for _, s := range [][2]string{{"downloading", "downloading fairy-linux-amd64"}, {"staged", "downloaded and verified"},
			{"restarting", "installing and restarting"}, {"done", "fairy " + d.To + " is running (demo: nothing was installed)"}} {
			time.Sleep(1500 * time.Millisecond)
			d.mu.Lock()
			d.st.State, d.st.Message, d.st.UpdatedAt, d.st.Finished = s[0], s[1], time.Now().UTC(), finished(s[0])
			d.mu.Unlock()
		}
	}()
	return nil
}

// Status is the simulation's state.
func (d *Demo) Status() (Status, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.st, d.ok
}
