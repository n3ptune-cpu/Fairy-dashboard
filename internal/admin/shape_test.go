package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
)

// shapeCLI is `fairy admin` on a data folder whose state.json a test writes. read gives a new reader of that
// folder each time (a reader keeps what it read for a moment; the command is another one).
func shapeCLI(t *testing.T, state string) (run func(args ...string) (int, string), read func() *adminstate.State, auditLog string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	auditLog = filepath.Join(dir, "audit.log")
	if err := os.WriteFile(cfg, []byte("public_url: https://p.example\ndata_dir: "+dir+"/data\naudit_log: "+auditLog+"\noidc: {issuer: https://id.example, client_id: c}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	read = func() *adminstate.State { return adminstate.New(filepath.Join(dir, "data")) }
	if err := os.WriteFile(read().StatePath, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	run = func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(append([]string{"--config", cfg, "--env-file", dir + "/none.env"}, args...), &out, &errb)
		return code, out.String() + errb.String()
	}
	return run, read, auditLog
}

// Second review of fix7a (R7A-05 b): `fairy admin host <id> power-control enable|disable` while
// "power_control_disabled" in state.json is not the list Fairy writes. Power control is off for every host
// then. The command wrote the list anew with the one host it was given, and every other host was ON again.
// It now changes nothing, says so, and says how to put the file right; nothing is entered in the audit log.
func TestHostPowerControlRefusesToWriteOverAnOddList(t *testing.T) {
	for _, value := range []string{`true`, `false`, `null`, `"pc-a"`, `{"pc-a": true}`, `0`, `[{"id": "pc-a"}]`, `[["pc-a"]]`, `["pc-a", 5]`} {
		for _, word := range []string{"disable", "enable"} {
			state := `{"power_control_disabled": ` + value + `, "poll_overrides": {"nas": 30}}`
			run, read, auditLog := shapeCLI(t, state)
			code, out := run("host", "pc-a", "power-control", word)
			st := read()
			after, _ := os.ReadFile(st.StatePath)
			if code != 1 || string(after) != state {
				t.Errorf("%s, %s: exit %d, state.json now: %s\n%s", value, word, code, after, out)
			}
			if !st.PowerDisabled("pc-a") || !st.PowerDisabled("pc-b") {
				t.Errorf("%s, %s: power control is on for pc-a: %v, for pc-b: %v", value, word, !st.PowerDisabled("pc-a"), !st.PowerDisabled("pc-b"))
			}
			for _, part := range []string{"is not a list of host ids", "power control is off for every host", "nothing was changed",
				`To put it right, edit that file so that the line reads "power_control_disabled": []`, "then run this command again", st.StatePath} {
				if !strings.Contains(out, part) {
					t.Errorf("%s, %s: the message lacks %q:\n%s", value, word, part, out)
				}
			}
			if strings.Contains(out, "Power control for") {
				t.Errorf("%s, %s: the command says that it was done:\n%s", value, word, out)
			}
			if entries, _ := audit.Read(auditLog, "", "", "", nil, 50); len(entries) != 0 {
				t.Errorf("%s, %s: audit entries for a command that changed nothing: %v", value, word, entries)
			}
		}
	}
	// the way out the message names: with the list put right the command works, and the other hosts keep their state
	run, read, _ := shapeCLI(t, `{"power_control_disabled": ["pc-b"]}`)
	if code, out := run("host", "pc-a", "power-control", "disable"); code != 0 || !read().PowerDisabled("pc-a") || !read().PowerDisabled("pc-b") || read().PowerDisabled("pc-c") {
		t.Errorf("a list of ids: exit %d: %s", code, out)
	}
	if code, out := run("host", "pc-a", "power-control", "enable"); code != 0 || read().PowerDisabled("pc-a") || !read().PowerDisabled("pc-b") {
		t.Errorf("a list of ids, enable: exit %d: %s", code, out)
	}
	run, read, _ = shapeCLI(t, `{"power_control_disabled": []}`)
	if code, out := run("host", "pc-a", "power-control", "disable"); code != 0 || !read().PowerDisabled("pc-a") || read().PowerDisabled("pc-b") {
		t.Errorf("an empty list: exit %d: %s", code, out)
	}
}

// `fairy admin control enable` gets the owner out of a kill switch of every shape (it takes the key out,
// whatever is under it), and leaves the rest of state.json as it is: also a per-host power list of an odd
// shape, which keeps power control off for every host until it is put right.
func TestControlEnableGetsOutOfEveryShapeOfTheKillSwitch(t *testing.T) {
	for _, value := range []string{`{"by": "root (cli)", "at": "2026-11-12T10:00:00Z", "reason": "x"}`, `{}`, `{"by": 5}`, `true`, `false`, `null`, `""`, `"on"`, `0`, `1`,
		`[]`, `["root"]`, `[{"by": "x"}]`, `{"control_disabled": true}`} {
		for _, power := range []string{`["pc-b"]`, `true`} {
			run, read, _ := shapeCLI(t, `{"control_disabled": `+value+`, "power_control_disabled": `+power+`, "poll_overrides": {"nas": 30}}`)
			if read().ControlDisabled() == nil {
				t.Fatalf("control_disabled %s: set-up: control is on", value)
			}
			code, out := run("control", "enable")
			st := read()
			if code != 0 || !strings.Contains(out, "Control enabled again.") || st.ControlDisabled() != nil {
				t.Errorf("control_disabled %s: `control enable`: exit %d, control still off: %v\n%s", value, code, st.ControlDisabled() != nil, out)
			}
			after, _ := os.ReadFile(st.StatePath)
			if d, _ := st.StateData(); d["control_disabled"] != nil || d["poll_overrides"] == nil || d["power_control_disabled"] == nil {
				t.Errorf("control_disabled %s: state.json now: %s", value, after)
			}
			if !st.PowerDisabled("pc-b") || st.PowerDisabled("pc-a") != (power == `true`) {
				t.Errorf("control_disabled %s, power list %s: after `control enable` power control is off for pc-b: %v, for pc-a: %v", value, power, st.PowerDisabled("pc-b"), st.PowerDisabled("pc-a"))
			}
		}
	}
}
