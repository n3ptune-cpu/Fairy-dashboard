package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Audit 6, r4-p4-02. `fairy update` started from a copy of the program (an old download: `sudo ./fairy update`)
// replaced the copy, restarted fairy.service and said "Done: fairy N is running", with /usr/local/bin/fairy and
// the panel as they were. Now the run stops before anything else when the program that was started is not the
// file fairy.service starts, and the check after the restart does not pass for a service that runs another
// program than the one that was put in place.

func TestTheUpdateRefusesAProgramThatIsNotTheServices(t *testing.T) {
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return real
	}
	installed := write("bin/fairy", "fairy 100")
	cp := write("dl/fairy", "fairy 100")
	link := filepath.Join(dir, "fairy-link")
	if err := os.Symlink(installed, link); err != nil {
		t.Skip("no symbolic links here:", err)
	}
	hard := filepath.Join(dir, "fairy-hard")
	if err := os.Link(installed, hard); err != nil {
		t.Skip("no hard links here:", err)
	}
	shown := func(path string) string { // what systemd shows of a unit that starts path
		return parseUnit("ExecStart={ path=" + path + " ; argv[]=" + path + " serve --config /etc/fairy/config.yaml --env-file  ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n").Path
	}
	if got := shown("/usr/local/bin/fairy"); got != "/usr/local/bin/fairy" {
		t.Fatalf("the program of the unit as shipped: %q", got)
	}
	// the copy: refused, in one sentence that names both files and the way out
	why := notTheServices(cp, shown(installed))
	want := "this program (" + cp + ") is not the one fairy.service runs (" + installed + "): an update of it would leave the panel as it is. " +
		"Nothing was changed. Run the installed one: sudo " + installed + " update"
	if why != want {
		t.Errorf("a copy of the program:\n got %q\nwant %q", why, want)
	}
	if strings.Contains(why, "\n") {
		t.Error("more than one line")
	}
	// the installed program, however the unit names it
	for name, unit := range map[string]string{"its path": installed, "a link to it": link, "the same file under another name": hard, "a path that is not clean": filepath.Dir(installed) + "/./fairy"} {
		if why := notTheServices(installed, shown(unit)); why != "" {
			t.Errorf("the installed program, the unit names %s: refused: %s", name, why)
		}
	}
	// a unit whose program is not there is not this program either; and without systemd's answer nothing is said here
	if why := notTheServices(cp, shown(filepath.Join(dir, "gone/fairy"))); why == "" {
		t.Error("a unit that starts a file that does not exist: not refused")
	}
	if why := notTheServices(cp, parseUnit("").Path); why != "" {
		t.Errorf("systemd did not say: %s", why)
	}
}

// update() asks before it takes its lock (the lock file is written next to the program that was started) and
// before anything is downloaded; the check after the restart looks at the program the service runs.
func TestTheUpdateLooksAtTheServicesProgramBeforeItChangesAnything(t *testing.T) {
	b, err := os.ReadFile("update.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	ask, lock, run := strings.Index(src, "notTheServices(exe, unit.Path)"), strings.Index(src, "lockUpdate(updateLockPath(exe))"), strings.Index(src, "cli.Run(ctx)")
	if ask < 0 || lock < 0 || run < 0 || ask > lock || lock > run {
		t.Errorf("update() must ask notTheServices before lockUpdate and cli.Run (at %d, %d, %d)", ask, lock, run)
	}
	_, healthy, ok := strings.Cut(src, "func systemdInstaller(")
	if active, other := strings.Index(healthy, `"is-active", "--quiet", "fairy.service"`), strings.Index(healthy, "serviceRunsOther(pid, target)"); !ok || active < 0 || other < active {
		t.Error("the health check after the restart no longer looks at the program fairy.service runs (serviceRunsOther)")
	}
}

func TestTheHealthCheckSeesWhichProgramAProcessRuns(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc/<pid>/exe")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if other, seen := serviceRunsOther(os.Getpid(), self); other || !seen {
		t.Errorf("this process and its own program: other %v, seen %v", other, seen)
	}
	// the program was replaced (a new file under the same name), and the process still runs the old one
	dir := t.TempDir()
	replaced := filepath.Join(dir, "fairy")
	if err := os.WriteFile(replaced, []byte("fairy 101"), 0o755); err != nil {
		t.Fatal(err)
	}
	if other, seen := serviceRunsOther(os.Getpid(), replaced); !other || !seen {
		t.Errorf("this process and another file: other %v, seen %v", other, seen)
	}
	// what cannot be looked at is not "another program": the check is then what it was
	if other, seen := serviceRunsOther(os.Getpid(), filepath.Join(dir, "gone")); other || seen {
		t.Errorf("a target that is not there: other %v, seen %v", other, seen)
	}
	if other, seen := serviceRunsOther(1<<30, self); other || seen {
		t.Errorf("a process that is not there: other %v, seen %v", other, seen)
	}
}
