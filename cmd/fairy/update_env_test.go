package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// Audit 6, r4-p4-01. `systemctl show -p Environment` writes every assignment through systemd's
// shell_maybe_quote. The lines here are what that function gives (systemd 255, the auditor's
// poc/09b-systemd-quote.txt), and the same with the four characters it puts a backslash before.

const shownExec = "ExecStart={ path=/usr/local/bin/fairy ; argv[]=/usr/local/bin/fairy serve --env-file  ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }\n"

func TestEnvironmentIsReadAsSystemdWritesIt(t *testing.T) {
	for _, c := range []struct{ shown, want string }{
		{`FAIRY_CONFIG_FILE=/etc/fairy/other.yaml`, "FAIRY_CONFIG_FILE=/etc/fairy/other.yaml"},
		{`"FAIRY_CONFIG_FILE=/etc/fairy/my dir/c.yaml"`, "FAIRY_CONFIG_FILE=/etc/fairy/my dir/c.yaml"},
		{`"FAIRY_LISTEN=[::1]:8080"`, "FAIRY_LISTEN=[::1]:8080"},
		{`FAIRY_LISTEN=10.0.0.20:8080`, "FAIRY_LISTEN=10.0.0.20:8080"},
		{`"FAIRY_CONFIG_FILE=/etc/fairy/c(1).yaml"`, "FAIRY_CONFIG_FILE=/etc/fairy/c(1).yaml"},
		{`"FAIRY_CONFIG_FILE=/etc/fairy/a&b.yaml"`, "FAIRY_CONFIG_FILE=/etc/fairy/a&b.yaml"},
		{`"FAIRY_CONFIG_FILE=/etc/fairy/it's.yaml"`, "FAIRY_CONFIG_FILE=/etc/fairy/it's.yaml"},
		{`"FAIRY_CONFIG_FILE=/etc/a\"b\\c\$d` + "\\`" + `e.yaml"`, "FAIRY_CONFIG_FILE=/etc/a\"b\\c$d`e.yaml"},
		{`A=b "FAIRY_CONFIG_FILE=/etc/fairy/my dir/c.yaml"  "FAIRY_LISTEN=[::1]:8080" Z=1`, "A=b|FAIRY_CONFIG_FILE=/etc/fairy/my dir/c.yaml|FAIRY_LISTEN=[::1]:8080|Z=1"},
		{``, ""},
	} {
		if kv, sure := unitEnv(c.shown); strings.Join(kv, "|") != c.want || !sure {
			t.Errorf("unitEnv(%s) = %q, sure %v; want %q", c.shown, strings.Join(kv, "|"), sure, c.want)
		}
	}
	// the file the service starts with, and the address it listens on
	u := parseUnit(shownExec + "Environment=\"FAIRY_CONFIG_FILE=/etc/fairy/my dir/c.yaml\"\nEnvironmentFiles=/nonexistent/fairy.env (ignore_errors=no)\n")
	if path, from, known := serviceConfig(u, config.DefaultConfigPath(), false); path != "/etc/fairy/my dir/c.yaml" || !known || from != "Environment=FAIRY_CONFIG_FILE of fairy.service" {
		t.Errorf("the config file: %q (%s), known %v", path, from, known)
	}
	u = parseUnit(shownExec + "Environment=\"FAIRY_LISTEN=[::1]:8080\"\n")
	if addr, from := healthAddr("", "/nonexistent/fairy.env", u); addr != "[::1]:8080" || from != "Environment=FAIRY_LISTEN of fairy.service" {
		t.Errorf("the health address: %q (%s)", addr, from)
	}
	u = parseUnit(shownExec + "Environment=\"FAIRY_LISTEN=[::]:8452\" A=b\n")
	if addr, _ := healthAddr("", "/nonexistent/fairy.env", u); addr != "127.0.0.1:8452" {
		t.Errorf("the health address for [::]: %q", addr)
	}
}

// A line systemd would not write like this is not guessed at: the update stops before it replaces anything and
// asks for the file (--config), as it does for a command line it cannot read for certain.
func TestAnEnvironmentThatIsNotCertainStopsTheUpdate(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "unit.env")
	if err := os.WriteFile(envFile, []byte("FAIRY_CONFIG_FILE=/etc/fairy/from-file.yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	def := config.DefaultConfigPath()
	for name, shown := range map[string]string{
		"a quote that does not end":                     `"FAIRY_CONFIG_FILE=/etc/fairy/my dir/c.yaml`,
		"the line break of a value (the next line)":     `A=b "FAIRY_CONFIG_FILE=/etc/fairy/my`,
		"a quote in the middle of an assignment":        `FAIRY_CONFIG_FILE="/etc/fairy/my dir/c.yaml"`,
		"single quotes":                                 `'FAIRY_CONFIG_FILE=/etc/fairy/my dir/c.yaml'`,
		"the $'...' of an older systemd":                `$'FAIRY_CONFIG_FILE=/etc/fairy/a\nb.yaml'`,
		"a backslash that systemd does not write":       `"FAIRY_CONFIG_FILE=/etc/fairy/a\nb.yaml"`,
		"text after the closing quote":                  `"FAIRY_CONFIG_FILE=/etc/fairy/my dir/c".yaml`,
		"a backslash in an assignment without quotes":   `FAIRY_CONFIG_FILE=/etc/fairy/my\ dir/c.yaml`,
		"not certain in another assignment of the line": `FAIRY_CONFIG_FILE=/etc/fairy/c.yaml "B=x`,
	} {
		if _, sure := unitEnv(shown); sure {
			t.Errorf("%s: unitEnv(%s) is sure", name, shown)
		}
		u := parseUnit(shownExec + "Environment=" + shown + "\n")
		path, from, known := serviceConfig(u, def, false)
		if known || path != def || from != "the default; the Environment= systemd shows cannot be read for certain" || u.unknownConfig() != configEnvUnsure {
			t.Errorf("%s: %q (%s), known %v; the stop: %s", name, path, from, known, u.unknownConfig())
		}
		// the person names the file: then it is known
		if path, _, known := serviceConfig(u, "/etc/fairy/named.yaml", true); !known || path != "/etc/fairy/named.yaml" {
			t.Errorf("%s, with --config: %q, known %v", name, path, known)
		}
		// what wins over Environment= in the service's own order is still certain: --config of the unit, an EnvironmentFile=
		u = parseUnit(strings.Replace(shownExec, "--env-file ", "--config /etc/fairy/flag.yaml", 1) + "Environment=" + shown + "\n")
		if path, _, known := serviceConfig(u, def, false); !known || path != "/etc/fairy/flag.yaml" {
			t.Errorf("%s, with --config in the unit: %q, known %v", name, path, known)
		}
		u = parseUnit(shownExec + "Environment=" + shown + "\nEnvironmentFiles=" + envFile + " (ignore_errors=no)\n")
		if path, _, known := serviceConfig(u, def, false); !known || path != "/etc/fairy/from-file.yaml" {
			t.Errorf("%s, with an EnvironmentFile=: %q, known %v", name, path, known)
		}
	}
	if !strings.Contains(configEnvUnsure, "Nothing was changed. Name the file: fairy update --config ") {
		t.Errorf("the stop does not say the way out: %s", configEnvUnsure)
	}
}

// A flag given twice counts where it stands last, as for `fairy serve` (Go's flag package).
func TestAFlagGivenTwiceCountsWhereItStandsLast(t *testing.T) {
	dir := t.TempDir()
	for _, args := range []string{
		"--config /etc/fairy/a.yaml --config /etc/fairy/b.yaml",
		"--config=/etc/fairy/a.yaml --config /etc/fairy/b.yaml",
		"--config /etc/fairy/a.yaml -config=/etc/fairy/b.yaml",
		"--config /etc/fairy/a.yaml --listen 127.0.0.1:1 --config /etc/fairy/c.yaml --config=/etc/fairy/b.yaml",
	} {
		u := parseUnit("ExecStart={ path=/usr/local/bin/fairy ; argv[]=/usr/local/bin/fairy serve " + args + " ; ignore_errors=no }\n")
		fs := flag.NewFlagSet("serve", flag.ContinueOnError)
		serves := fs.String("config", "", "")
		fs.String("listen", "", "")
		if err := fs.Parse(u.Args[2:]); err != nil {
			t.Fatal(err)
		}
		if path, _, known := serviceConfig(u, config.DefaultConfigPath(), false); !known || path != *serves || path != "/etc/fairy/b.yaml" {
			t.Errorf("%s: the update checks %q (known %v), the service starts with %q", args, path, known, *serves)
		}
	}
	own := filepath.Join(dir, "own.env")
	if err := os.WriteFile(own, []byte("FAIRY_LISTEN=127.0.0.1:8703\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u := parseUnit("ExecStart={ path=/usr/local/bin/fairy ; argv[]=/usr/local/bin/fairy serve --listen 127.0.0.1:8701 --listen 127.0.0.1:8702 ; ignore_errors=no }\n")
	if addr, _ := healthAddr("", "", u); addr != "127.0.0.1:8702" {
		t.Errorf("--listen twice: %q", addr)
	}
	u = parseUnit("ExecStart={ path=/usr/local/bin/fairy ; argv[]=/usr/local/bin/fairy serve --env-file /nonexistent.env --env-file " + own + " ; ignore_errors=no }\n")
	if addr, _ := healthAddr("", "", u); addr != "127.0.0.1:8703" {
		t.Errorf("--env-file twice: %q", addr)
	}
}
