// Package health is the panel's status file, <data_dir>/health.json.
//
// The running panel rewrites it every few seconds; `fairy admin health` and `fairy admin poll` read it, so the
// break-glass CLI can report on the panel without a web login.
package health

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// FileName is the status file's name inside the data directory.
const FileName = "health.json"

// Report is the content of health.json. The fields are in alphabetical order of their JSON names so the file
// keeps the key order it had when it was written from a map.
type Report struct {
	ControlDisabled *model.ControlState  `json:"control_disabled"`
	Jobs            map[string]Job       `json:"jobs"`          // scheduler job key -> stats
	PowerSources    []Source             `json:"power_sources"` // configured and agent-relayed power sources
	Sources         []model.SourceStatus `json:"sources"`       // Proxmox clusters and agents
	StartedAt       string               `json:"started_at"`    // RFC 3339
	SystemIDs       []string             `json:"system_ids"`
	Systems         map[string]int       `json:"systems"` // status (online, stale, ...) -> count
	UPS             []Source             `json:"ups"`
	Version         int                  `json:"version"`
	WrittenAt       string               `json:"written_at"` // RFC 3339
}

// Job is one scheduler job's counters.
type Job struct {
	EffectiveS *float64 `json:"effective_s"` // nil while paused
	LastMS     *float64 `json:"last_ms"`     // nil before the first run
	Runs       int      `json:"runs"`
	Skipped    int      `json:"skipped"`
}

// Source is the state of one power source or UPS.
type Source struct {
	Error  *string `json:"error"`
	ID     string  `json:"id"`
	Status string  `json:"status"`
}

// Path returns the location of health.json in dataDir.
func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }

// Write replaces health.json atomically (write to a temporary file, then rename), mode 0600.
func Write(dataDir string, r Report) error {
	b, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	path := Path(dataDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Read loads health.json from dataDir.
func Read(dataDir string) (Report, error) {
	var r Report
	b, err := statefile.ReadFile(Path(dataDir), statefile.MaxBytes) // a plain file of a sane size, or it is not read
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(b, &r)
	return r, err
}
