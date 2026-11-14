package adminstate

import (
	"path/filepath"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// ReadHostOptions reads the "Edit host" overlay of the panel whose data folder is dataDir, for a command
// that is not the running panel (`fairy admin config check`): the overlay, and one line for every thing in
// it that the panel leaves out. Nothing is logged and nothing is written. err: state.json itself cannot be
// read (the panel then does not start, and says why).
func ReadHostOptions(dataDir string) (o HostOptions, ignored []string, err error) {
	d, err := statefile.Read(filepath.Join(dataDir, "state.json"))
	if err != nil {
		return HostOptions{Agents: map[string]HostOption{}}, nil, err
	}
	o, ignored = hostOptions(d)
	return o, ignored, nil
}
