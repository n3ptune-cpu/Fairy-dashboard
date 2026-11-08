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

// Finished states.
func finished(state string) bool {
	switch state {
	case "done", "up_to_date", "rolled_back", "failed":
		return true
	}
	return false
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

// Status reads the status file.
func (s Systemd) Status() (Status, bool) {
	p := s.StatusPath
	if p == "" {
		p = StatusPath
	}
	return Read(p)
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
