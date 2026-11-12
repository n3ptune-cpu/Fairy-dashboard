package main

import (
	"bytes"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
// as <name>.corrupt (kept, and out of the way of the next write), and one ERROR names it.
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
			aside, asideErr := os.ReadFile(p + ".corrupt")
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
				if strings.Count(log, "level=ERROR") != 1 || !strings.Contains(log, name+" is damaged: the panel starts without it") || !strings.Contains(log, p+".corrupt") {
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
			if _, err := os.Lstat(filepath.Join(dir, name+".corrupt")); err == nil {
				t.Fatalf("%s as a symbolic link was moved aside", name)
			}
		}
	}
}
