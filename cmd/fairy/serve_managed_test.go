package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// Audit 5, round 3 (r3-p4-03): managed-hosts.yaml (the hosts added with Add host) as a symbolic link. The
// release before read it through the link; this one reads state files as plain files only, and the panel
// started with every added host gone and one ERROR line: an update went through like that. The start is
// refused, like for state.json, and the message names the file; `fairy update` then rolls back by itself.
func TestStartRefusesAManagedHostsListThatIsNotAPlainFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need a privilege on Windows")
	}
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	const tok = "0123456789abcdef0123456789abcdef"
	build := func(prepare func(s *managed.Store, dir string)) (*panel, int, *config.Panel, string) {
		t.Helper()
		logs.Reset()
		cfg := testutil.Config(t, []string{"https://pve.example:8006"}, "http://127.0.0.1:1", "http://127.0.0.1:2")
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		s := managed.NewStore(cfg.DataDir)
		if err := s.Put(managed.Host{ID: "added-pc", Name: "added-pc", URL: "http://added-pc:9101", OS: "linux"}, managed.Secret{Token: tok + "r", ControlToken: tok + "c"}); err != nil {
			t.Fatal(err)
		}
		prepare(s, cfg.DataDir)
		p, code := buildPanel(cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)})
		return p, code, cfg, logs.String()
	}
	has := func(cfg *config.Panel, id string) bool {
		for _, a := range cfg.Agents {
			if a.ID == id {
				return true
			}
		}
		return false
	}
	// a plain file: the panel starts with the host
	if p, code, cfg, log := build(func(*managed.Store, string) {}); p == nil || code != 0 || !has(cfg, "added-pc") || strings.Contains(log, "level=ERROR") {
		t.Fatalf("a plain managed-hosts.yaml: exit %d, the host is there: %v, log: %s", code, has(cfg, "added-pc"), log)
	}
	link := func(path string) {
		t.Helper()
		elsewhere := filepath.Join(t.TempDir(), filepath.Base(path))
		if err := os.Rename(path, elsewhere); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, path); err != nil {
			t.Fatal(err)
		}
	}
	for what, prepare := range map[string]func(s *managed.Store, dir string){
		"managed-hosts.yaml is a link":          func(s *managed.Store, _ string) { link(s.Path()) },
		"the tokens file of the host is a link": func(_ *managed.Store, dir string) { link(filepath.Join(dir, "secrets", "agent-added-pc.json")) },
		"managed-hosts.yaml is a folder":        func(s *managed.Store, _ string) { _ = os.Remove(s.Path()); _ = os.Mkdir(s.Path(), 0o700) },
		"managed-hosts.yaml is cut off":         func(s *managed.Store, _ string) { _ = os.WriteFile(s.Path(), []byte("hosts: [{id: added"), 0o600) },
	} {
		p, code, cfg, log := build(prepare)
		if p != nil || code != 1 {
			t.Errorf("%s: the panel started (exit %d) without the hosts added in the panel (the host is there: %v); log: %s", what, code, has(cfg, "added-pc"), log)
			continue
		}
		if strings.Count(log, "level=ERROR") != 1 || !strings.Contains(log, "refusing to start: the hosts added in the panel (Add host) cannot be read") ||
			!strings.Contains(log, filepath.Join(cfg.DataDir, "managed-hosts.yaml")) {
			t.Errorf("%s: want one ERROR that refuses and names the file: %s", what, log)
		}
		if strings.Contains(what, "a link") && !strings.Contains(log, "is a symbolic link, not a plain file") {
			t.Errorf("%s: the ERROR does not say what is there: %s", what, log)
		}
	}
	// a host whose tokens are missing is one host less, as before: the panel starts and says which
	p, code, cfg, log := build(func(_ *managed.Store, dir string) {
		_ = os.Remove(filepath.Join(dir, "secrets", "agent-added-pc.json"))
	})
	if p == nil || code != 0 || has(cfg, "added-pc") || !strings.Contains(log, "managed host added-pc: tokens unreadable") {
		t.Fatalf("a host without its tokens file: exit %d, log: %s", code, log)
	}
}
