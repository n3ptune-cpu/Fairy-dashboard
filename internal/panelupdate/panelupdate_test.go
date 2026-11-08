package panelupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallAndStatus(t *testing.T) {
	dir := t.TempDir()
	unit, rules := filepath.Join(dir, "fairy-panel-update.service"), filepath.Join(dir, "rules.d", "49-fairy-panel.rules")
	var ran []string
	run := func(name string, args ...string) error {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return nil
	}
	if _, err := Install(unit, rules, run); err == nil {
		t.Fatal("no polkit folder must be reported")
	}
	if err := os.Mkdir(filepath.Dir(rules), 0o755); err != nil {
		t.Fatal(err)
	}
	changed, err := Install(unit, rules, run)
	if err != nil || len(changed) != 1 || ran[len(ran)-1] != "systemctl daemon-reload" { // the unit was written by the first try
		t.Fatal(changed, err, ran)
	}
	b, _ := os.ReadFile(unit)
	if !strings.Contains(string(b), "ExecStart=/usr/local/bin/fairy update --yes --from-panel") {
		t.Fatal(string(b))
	}
	r, _ := os.ReadFile(rules)
	if !strings.Contains(string(r), `action.lookup("unit") === "fairy-panel-update.service"`) || !strings.Contains(string(r), `subject.user === "fairy"`) {
		t.Fatal(string(r))
	}
	if changed, _ := Install(unit, rules, run); len(changed) != 0 {
		t.Fatal("second run changed", changed)
	}
	status := filepath.Join(dir, "status", "status.json")
	w := &Writer{Path: status, From: "8-a"}
	if _, ok := Read(status); ok {
		t.Fatal("no status yet")
	}
	w.Step("downloading", "9-b", "downloading")
	w.Step("done", "", "running")
	st, ok := Read(status)
	if !ok || st.State != "done" || !st.Finished || st.From != "8-a" || st.To != "9-b" || st.StartedAt.IsZero() {
		t.Fatalf("%+v", st)
	}
}
