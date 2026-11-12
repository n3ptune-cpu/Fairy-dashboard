package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// r3-p4-01: a mistake inside a `ups:` or `power_sources:` entry. Load accepts the file (the two lists are
// free-form there); LoadFull, the one check `fairy serve`, `fairy admin config check` and `fairy update` use,
// refuses it and names the line of the entry.
func TestLoadFullChecksTheDeviceLists(t *testing.T) {
	const head = "public_url: https://panel.example.com\noidc: {issuer: \"https://id.example.com\", client_id: fairy}\n" // lines 1-2
	for _, c := range []struct{ name, yaml, line, want string }{
		{"the auditor's UPS (type: nutt)", "power_sources: []\nups:\n  - {id: rack-ups, type: nutt, host: 127.0.0.1}\n",
			"line 5 (ups, entry 1)", `unknown type "nutt"`},
		{"the auditor's plug (type: shely)", "power_sources:\n  - {id: plug-1, type: shely, url: 'http://127.0.0.1'}\nups: []\n",
			"line 4 (power_sources, entry 1)", "shely"},
		{"the second entry of two, in block form", "ups:\n  - id: a\n    type: nut\n    host: 10.0.0.1\n  - id: b\n    type: nut\n",
			"line 7 (ups, entry 2)", "host is required"},
		{"a key the type does not have", "ups:\n  - {id: a, type: nut, host: 10.0.0.1, hots: x}\n", "line 4 (ups, entry 1)", "hots"},
		{"the same id twice: named at the second", "power_sources:\n  - {id: p, type: shelly, url: 'http://10.0.0.1'}\n  - {id: p, type: shelly, url: 'http://10.0.0.2'}\n",
			"line 5 (power_sources, entry 2)", "duplicate id"},
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(head+c.yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err != nil {
			t.Fatalf("%s: setup: Load alone is expected to accept the file: %v", c.name, err)
		}
		_, err := LoadFull(path)
		if err == nil || !strings.Contains(err.Error(), c.line) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: LoadFull = %v, want a refusal with %q and %q", c.name, err, c.line, c.want)
		}
		if strings.Count(err.Error(), "line ") != 1 {
			t.Errorf("%s: one line is named, the file's: %v", c.name, err)
		}
		if cfg, perr := Parse([]byte(head + c.yaml)); perr != nil || cfg.CheckDevices() == nil {
			t.Errorf("%s: CheckDevices (no file) must refuse it too: %v", c.name, perr)
		}
	}
	good := head + "power_sources:\n  - {id: p, type: shelly, url: 'http://10.0.0.1'}\nups:\n  - {id: rack, type: nut, host: 10.0.0.2}\n"
	c, err := ParseFull([]byte(good))
	if err != nil || len(c.UPS) != 1 || len(c.PowerSources) != 1 {
		t.Fatalf("a good file: %v", err)
	}
	if _, err := ParseFull([]byte(head + "wake_updte: {}\n")); err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("what Load refuses, LoadFull refuses in Load's words: %v", err)
	}
	if _, err := LoadFull(filepath.Join(t.TempDir(), "none.yaml")); !os.IsNotExist(err) {
		t.Fatalf("a missing file: %v", err)
	}
}
