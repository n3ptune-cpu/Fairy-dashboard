package web

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The hints the panel and the admin CLI show name fairy-agent's config.yaml keys (processes: enabled: false,
// power_control: enabled: true) and the install script's switches, never the prototype agent's command-line
// flags, which fairy-agent does not have.
func TestNoPrototypeAgentFlagHints(t *testing.T) {
	root := filepath.Join("..", "..")
	gone := []string{"--no-processes", "--enable-power-control", "-NoProcesses", "-EnablePowerControl"}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".js", ".html", ".md", ".yaml", ".yml", ".sh", ".ps1", ".service":
		default:
			return nil
		}
		if strings.HasSuffix(path, "hints_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, g := range gone {
			if strings.Contains(string(b), g) {
				t.Errorf("%s still mentions %s (fairy-agent has no such flag: name its config.yaml key)", path, g)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := fs.ReadFile(staticFS, "static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(app), "processes: enabled: false in its config.yaml") {
		t.Error("the Processes tab no longer says how the owner turned it off (processes: enabled: false)")
	}
}
