package main

import (
	"bytes"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
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
