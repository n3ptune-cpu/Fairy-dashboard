package admin

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/power"
)

// powerDayCheck is the config check's line about the power sources' last 24 hours ("Last 24 hours" on the
// Power page). There is no setting for it, so the line says what is kept and where; and when something that
// is not a plain file has the file's name, a note says that nothing is kept until it is taken away (the panel
// says the same once in its log, and runs). Never a reason for exit code 1: the file decides nothing.
func powerDayCheck(cfg *config.Panel) (ok string, said []string) {
	path := filepath.Join(cfg.DataDir, power.DayFile)
	ok = fmt.Sprintf("power: the last 24 hours of every power source are kept, a minute a point, for at most %d sources, in %s (no setting; an older build leaves the file alone)",
		power.MaxDayDevices, path)
	if st, err := os.Lstat(path); err == nil && !st.Mode().IsRegular() {
		said = append(said, fmt.Sprintf("%s is not a plain file: the panel does not read or replace it, and the graphs of the last 24 hours start empty at every start of the panel until it is taken away", path))
	}
	return ok, said
}
