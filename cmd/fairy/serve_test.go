package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// The env file is loaded before the environment is read: FAIRY_LISTEN / FAIRY_LOG_LEVEL / FAIRY_CONFIG_FILE
// from fairy.env count. --listen beats FAIRY_LISTEN, which beats `listen:` in config.yaml.
func TestEnvFileThenFlagsThenConfig(t *testing.T) {
	for _, k := range []string{"FAIRY_LISTEN", "FAIRY_LOG_LEVEL", "FAIRY_CONFIG_FILE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	dir := t.TempDir()
	envFile := filepath.Join(dir, "fairy.env")
	_ = os.WriteFile(envFile, []byte("FAIRY_LISTEN=10.0.0.20:8080\nFAIRY_LOG_LEVEL=debug\nFAIRY_CONFIG_FILE=/x/config.yaml\n"), 0o600)
	f, ok := parseServeFlags([]string{"--env-file", envFile})
	if !ok {
		t.Fatal("flags")
	}
	if err := f.loadEnv(); err != nil {
		t.Fatal(err)
	}
	f.resolve()
	cfg := &config.Panel{Listen: "10.9.9.9:1"}
	if f.listenAddr(cfg) != "10.0.0.20:8080" || f.logLevel != "debug" || f.cfgPath != "/x/config.yaml" {
		t.Fatal("env file values ignored", f.listenAddr(cfg), f.logLevel, f.cfgPath)
	}
	f, _ = parseServeFlags([]string{"--env-file", envFile, "--listen", "127.0.0.1:9"})
	if f.listenAddr(cfg) != "127.0.0.1:9" {
		t.Fatal("--listen wins")
	}
	os.Unsetenv("FAIRY_LISTEN")
	if f, _ := parseServeFlags(nil); f.listenAddr(cfg) != "10.9.9.9:1" || f.listenAddr(&config.Panel{}) != defaultListen {
		t.Fatal("listen: from config, then the default")
	}
	// the default env file may be unreadable (systemd loads it as root); an explicit one may not
	if os.Geteuid() != 0 {
		_ = os.Chmod(envFile, 0)
		if f, _ := parseServeFlags(nil); func() error { f.envFile = envFile; return f.loadEnv() }() != nil {
			t.Fatal("unreadable default env file must not stop the panel")
		}
		if f, _ := parseServeFlags([]string{"--env-file", envFile}); f.loadEnv() == nil {
			t.Fatal("unreadable explicit env file must fail")
		}
	}
}

func TestProxyWarning(t *testing.T) {
	cfg := &config.Panel{}
	if err := cfg.Validate(); err == nil { // defaults applied even though the config is incomplete
		t.Fatal("empty config validates")
	}
	if proxyWarning("10.0.0.20:8080", cfg) == "" {
		t.Fatal("LAN listener with loopback-only trusted_proxies must warn")
	}
	if proxyWarning("127.0.0.1:8080", cfg) != "" {
		t.Fatal("loopback listener")
	}
	cfg.TrustedProxies = []string{"10.0.0.5/32"}
	_ = cfg.Validate()
	if len(cfg.Trusted()) != 1 || cfg.Trusted()[0] != netip.MustParsePrefix("10.0.0.5/32") || proxyWarning("10.0.0.20:8080", cfg) != "" {
		t.Fatal("nginx trusted: no warning")
	}
}

// T-05: `fairy serve` does not start with the example session secret (or one character repeated), and the
// log line says what to do. A real secret gets past this check as before (here: to the missing config.yaml).
func TestServeRefusesTheExampleSessionSecret(t *testing.T) {
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	f := serveFlags{cfgPath: filepath.Join(t.TempDir(), "config.yaml")}
	start := func(secret string) (int, string) {
		var log bytes.Buffer
		slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
		t.Setenv("FAIRY_SESSION_SECRET", secret)
		p, code := setupPanel(f)
		if p != nil {
			t.Fatal("a panel without config.yaml")
		}
		return code, log.String()
	}
	for _, weak := range []string{"REPLACE_WITH_AT_LEAST_32_RANDOM_CHARACTERS", strings.Repeat("x", 40)} {
		code, log := start(weak)
		if code != 1 || !strings.Contains(log, "FAIRY_SESSION_SECRET") || !strings.Contains(log, "openssl rand -base64 48") ||
			strings.Contains(log, "config not found") {
			t.Fatalf("serve with %q: exit %d, log:\n%s", weak, code, log)
		}
	}
	code, log := start("b0pV3m2x9QmJt8sYw1Zr5uKc7NfHd4Ea6LgTi0Oq2WsXy3Rv8BnMk1Jh5GfDc7Vb")
	if code != 1 || strings.Contains(log, "FAIRY_SESSION_SECRET") || !strings.Contains(log, "config not found") {
		t.Fatalf("serve with a real secret: exit %d, log:\n%s", code, log)
	}
}

// r2-p4-10 (audit 5): the panel does not start on users.json or state.json that exist and are empty or cut off
// (read as nothing they would be "no users yet" and "no kill switch"); a fresh install, where the files do not
// exist, starts.
//
// Second review of that round (note 2): sessions.json and tokens.json in that state do not keep the panel
// down. Read as nothing they mean "nobody is signed in" and "no token is valid". The damaged file is moved aside
// as <name>.corrupt-<UTC time> (kept, and out of the way of the next write), and one ERROR names it.
func TestStartAndDamagedStateFiles(t *testing.T) {
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	check := func(dir string) (bool, string) {
		logs.Reset()
		ok := checkStateFiles(dir)
		return ok, logs.String()
	}
	if ok, log := check(t.TempDir()); !ok || log != "" {
		t.Fatalf("a fresh data folder must be fine: %s", log)
	}
	broken := map[string]string{"empty": "", "blanks only": " \n\t\n", "cut off": `{"users": {"sub-1": {"role": "adm`, "not an object": "[1, 2]\n"}
	for _, name := range []string{"users.json", "state.json", "sessions.json", "tokens.json"} {
		for what, content := range broken {
			dir := t.TempDir()
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			ok, log := check(dir)
			now, _ := os.ReadFile(p)
			copies, _ := filepath.Glob(p + ".corrupt*")
			asideName, asideErr := "", error(os.ErrNotExist)
			var aside []byte
			if len(copies) == 1 {
				asideName = copies[0]
				aside, asideErr = os.ReadFile(asideName)
			}
			switch name {
			case "users.json", "state.json": // refused, the file untouched, the message names it
				if ok || string(now) != content || asideErr == nil || !strings.Contains(log, "refusing to start") || !strings.Contains(log, p) {
					t.Fatalf("%s, %s: start allowed=%v, file now %q, moved aside=%v, log: %s", name, what, ok, now, asideErr == nil, log)
				}
				first := strings.Contains(log, "restore it from a backup; moving it aside starts with no users, and the first person to sign in becomes admin")
				if first != (name == "users.json") || (name == "users.json" && strings.Contains(log, "or move it aside to start that file over")) {
					t.Fatalf("%s, %s: the remedy: %s", name, what, log)
				}
			default: // the panel starts; the damaged file is kept under another name; one ERROR names it
				if !ok || now != nil || asideErr != nil || string(aside) != content {
					t.Fatalf("%s, %s: start allowed=%v, file now %q, kept copy %q (%v), log: %s", name, what, ok, now, aside, asideErr, log)
				}
				if !regexp.MustCompile(`^` + regexp.QuoteMeta(p) + `\.corrupt-\d{8}T\d{6}Z$`).MatchString(asideName) {
					t.Fatalf("%s, %s: the kept copy is named %s", name, what, asideName)
				}
				if strings.Count(log, "level=ERROR") != 1 || !strings.Contains(log, name+" is damaged: the panel starts without it") || !strings.Contains(log, asideName) {
					t.Fatalf("%s, %s: want one ERROR that names the file and its copy: %s", name, what, log)
				}
				if ok, log := check(dir); !ok || log != "" { // and the next start has nothing to say
					t.Fatalf("%s, %s: the start after that: %v %s", name, what, ok, log)
				}
			}
			if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if ok, log := check(dir); !ok {
				t.Fatalf("%s holding {} is a valid empty state: %s", name, log)
			}
		}
	}
	// what is not "damaged" still stops the start for all four: a file the panel must not read at all
	if runtime.GOOS != "windows" {
		for _, name := range []string{"users.json", "state.json", "sessions.json", "tokens.json"} {
			dir := t.TempDir()
			if err := os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "elsewhere"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if ok, log := check(dir); ok || !strings.Contains(log, "refusing to start") {
				t.Fatalf("%s as a symbolic link: start allowed=%v: %s", name, ok, log)
			}
			if m, _ := filepath.Glob(filepath.Join(dir, name+".corrupt*")); len(m) != 0 {
				t.Fatalf("%s as a symbolic link was moved aside", name)
			}
		}
	}
}

// Audit 5, round 3 (r3-p4-09, r3-p2-10): the kept copy of a damaged sessions.json or tokens.json.
//
//   - A second damaged file replaced the first kept copy without a word (the copy was <name>.corrupt, and the
//     log said "is kept").
//   - When <name>.corrupt was a folder the move failed and the panel stayed down: the case the keeping aside
//     was made for.
//
// The copy has the time in its name and never replaces anything; and when the file cannot be moved at all the
// panel starts all the same, with an ERROR that says that the damaged file is still in place.
func TestStartAndTheKeptCopyOfADamagedStateFile(t *testing.T) {
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	check := func(dir string) (bool, string) {
		logs.Reset()
		ok := checkStateFiles(dir)
		return ok, logs.String()
	}
	copies := func(p string) map[string]string {
		out := map[string]string{}
		m, _ := filepath.Glob(p + ".corrupt*")
		for _, name := range m {
			if st, err := os.Lstat(name); err == nil && st.Mode().IsRegular() {
				b, _ := os.ReadFile(name)
				out[name] = string(b)
			}
		}
		return out
	}
	for _, name := range []string{"sessions.json", "tokens.json"} {
		// damaged twice (the second time a second later, or in the same second): both copies are kept
		dir := t.TempDir()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p+".corrupt", []byte("a copy the release before this one kept"), 0o600); err != nil {
			t.Fatal(err)
		}
		for i, content := range []string{"FIRST damaged copy", "", "{cut"} {
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			ok, log := check(dir)
			if !ok || strings.Count(log, "level=ERROR") != 1 || !strings.Contains(log, "The damaged file is kept as "+p+".corrupt-") {
				t.Fatalf("%s, damaged for the %d. time: start allowed=%v, log: %s", name, i+1, ok, log)
			}
			if _, err := os.Lstat(p); !os.IsNotExist(err) {
				t.Fatalf("%s is still in place after it was kept aside", name)
			}
		}
		got := copies(p)
		var kept []string
		for _, c := range got {
			kept = append(kept, c)
		}
		sort.Strings(kept)
		if want := []string{"", "FIRST damaged copy", "a copy the release before this one kept", "{cut"}; strings.Join(kept, "|") != strings.Join(want, "|") {
			t.Fatalf("%s: the kept copies hold %q, want %q", name, kept, want)
		}

		// <name>.corrupt is a folder, and so is the name with this second's time (and the next seconds'): the
		// panel starts, and the damaged file is kept under the next free name
		dir = t.TempDir()
		p = filepath.Join(dir, name)
		if err := os.Mkdir(p+".corrupt", 0o700); err != nil {
			t.Fatal(err)
		}
		for s := 0; s < 4; s++ {
			if err := os.Mkdir(p+".corrupt-"+time.Now().UTC().Add(time.Duration(s)*time.Second).Format("20060102T150405Z"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ok, log := check(dir)
		if got := copies(p); !ok || len(got) != 1 || strings.Count(log, "level=ERROR") != 1 || !regexp.MustCompile(regexp.QuoteMeta(p)+`\.corrupt-\d{8}T\d{6}Z-2"`).MatchString(log) {
			t.Fatalf("%s with folders where the copy would go: start allowed=%v, kept %v, log: %s", name, ok, got, log)
		}

		// it cannot be moved at all (every name is taken): the panel starts all the same, says that the damaged
		// file is still in place, and does not use it
		dir = t.TempDir()
		p = filepath.Join(dir, name)
		for s := 0; s < 4; s++ {
			stamp := p + ".corrupt-" + time.Now().UTC().Add(time.Duration(s)*time.Second).Format("20060102T150405Z")
			for i := 1; i <= 9; i++ {
				taken := stamp
				if i > 1 {
					taken = fmt.Sprintf("%s-%d", stamp, i)
				}
				if err := os.Mkdir(taken, 0o700); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := os.WriteFile(p, []byte("{cut off"), 0o600); err != nil {
			t.Fatal(err)
		}
		ok, log = check(dir)
		if !ok || strings.Count(log, "level=ERROR") != 1 || !strings.Contains(log, name+" is damaged and could not be moved aside: the panel starts without it") ||
			!strings.Contains(log, "THE DAMAGED FILE IS STILL IN PLACE at "+p) || strings.Contains(log, "refusing to start") {
			t.Fatalf("%s that cannot be moved aside: start allowed=%v, log: %s", name, ok, log)
		}
		if b, _ := os.ReadFile(p); string(b) != "{cut off" {
			t.Fatalf("%s that could not be moved now holds %q", name, b)
		}
	}
	// users.json and state.json are never moved: they stop the start (TestStartAndDamagedStateFiles)
	for _, name := range []string{"users.json", "state.json"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if ok, _ := check(dir); ok {
			t.Fatalf("an empty %s did not stop the start", name)
		}
		if m, _ := filepath.Glob(filepath.Join(dir, name+".corrupt*")); len(m) != 0 {
			t.Fatalf("%s was moved aside: %v", name, m)
		}
	}
}
