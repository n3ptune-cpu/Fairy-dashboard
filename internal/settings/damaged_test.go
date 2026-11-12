package settings

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Audit 5, round 3 (r3-p2-07): a damaged settings.json (cut off, 0 bytes) was read as "no settings" without a
// word, and the next save by any user wrote a file with that one user over it: everybody else's settings were
// gone, and no copy was kept. It is moved aside like the other files the panel may start over, with one ERROR;
// everybody's settings start with the defaults.
func TestADamagedSettingsFileIsKeptAside(t *testing.T) {
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	// what two users had saved
	whole := t.TempDir()
	s := NewStore(whole)
	for _, sub := range []string{"ann", "bob"} {
		st := Defaults()
		st.Dim = 33
		if _, err := s.Put(sub, st); err != nil {
			t.Fatal(err)
		}
	}
	good, err := os.ReadFile(filepath.Join(whole, "settings.json"))
	if err != nil || s.Get("bob").Dim != 33 || logs.Len() != 0 {
		t.Fatal(err, logs.String())
	}
	for what, content := range map[string]string{"cut off": string(good[:len(good)/2]), "empty": "", "blanks": "\n \n", "not an object": "[1, 2]", "null": "null"} {
		logs.Reset()
		dir := t.TempDir()
		p := filepath.Join(dir, "settings.json")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		s := NewStore(dir)
		s.Check() // the panel's start
		copies, _ := filepath.Glob(p + ".corrupt-*")
		if len(copies) != 1 {
			t.Fatalf("%s: kept copies: %v\n%s", what, copies, logs.String())
		}
		if b, _ := os.ReadFile(copies[0]); string(b) != content {
			t.Fatalf("%s: the kept copy holds %q", what, b)
		}
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s: the damaged file is still in place", what)
		}
		log := logs.String()
		if strings.Count(log, "level=ERROR") != 1 || !strings.Contains(log, "settings.json cannot be used: every user's settings start over with the defaults. The file is kept as "+copies[0]) {
			t.Fatalf("%s: want one ERROR that names the copy: %s", what, log)
		}
		// everybody starts with the defaults, a save works, and nothing more is said
		logs.Reset()
		if got := s.Get("bob"); got.Dim != Defaults().Dim {
			t.Fatalf("%s: bob's settings after the start: %+v", what, got)
		}
		st := Defaults()
		st.Dim = 33
		if _, err := s.Put("ann", st); err != nil {
			t.Fatal(err)
		}
		var saved map[string]json.RawMessage
		b, _ := os.ReadFile(p)
		if json.Unmarshal(b, &saved) != nil || len(saved) != 1 || s.Get("ann").Dim != 33 || logs.Len() != 0 {
			t.Fatalf("%s: after one save the file holds %s; log: %s", what, b, logs.String())
		}
		if c, _ := filepath.Glob(p + ".corrupt-*"); len(c) != 1 {
			t.Fatalf("%s: kept copies after a save: %v", what, c)
		}
	}
	// without the start's look, the first request finds it
	logs.Reset()
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(p, []byte("{cut"), 0o600); err != nil {
		t.Fatal(err)
	}
	s = NewStore(dir)
	if _, err := s.Put("ann", Defaults()); err != nil {
		t.Fatal(err)
	}
	if c, _ := filepath.Glob(p + ".corrupt-*"); len(c) != 1 || strings.Count(logs.String(), "level=ERROR") != 1 {
		t.Fatalf("a save over a damaged file: kept %v, log: %s", c, logs.String())
	}
	// a file that is fine, and one that is not there, say nothing
	logs.Reset()
	NewStore(whole).Check()
	NewStore(t.TempDir()).Check()
	if c, _ := filepath.Glob(filepath.Join(whole, "settings.json.corrupt-*")); len(c) != 0 || logs.Len() != 0 {
		t.Fatalf("a good file: kept %v, log: %s", c, logs.String())
	}
	// not a plain file (a link to the settings somewhere else): moved aside as the link it is, and said
	if runtime.GOOS != "windows" {
		logs.Reset()
		dir := t.TempDir()
		p := filepath.Join(dir, "settings.json")
		if err := os.Symlink(filepath.Join(whole, "settings.json"), p); err != nil {
			t.Fatal(err)
		}
		s := NewStore(dir)
		s.Check()
		c, _ := filepath.Glob(p + ".corrupt-*")
		if len(c) != 1 || strings.Count(logs.String(), "level=ERROR") != 1 || !strings.Contains(logs.String(), "symbolic link") {
			t.Fatalf("settings.json as a link: kept %v, log: %s", c, logs.String())
		}
		if st, err := os.Lstat(c[0]); err != nil || st.Mode()&os.ModeSymlink == 0 {
			t.Fatal("the kept copy is not the link")
		}
		if b, _ := os.ReadFile(filepath.Join(whole, "settings.json")); string(b) != string(good) {
			t.Fatal("the file the link pointed to was changed")
		}
		if got := s.Get("bob"); got.Dim != Defaults().Dim {
			t.Fatalf("the linked settings were read: %+v", got)
		}
	}
}
