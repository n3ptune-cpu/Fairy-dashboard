package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
)

func TestTermSafeWriter(t *testing.T) {
	var b bytes.Buffer
	w := newTermSafe(&b)
	in := "ok \x1b[2J\x1b]0;pwned\x07 \x1b]52;c;ZXZpbA==\x07 a\rb\x08 \u009b31m \u202eevil\u202c caf\xc3"
	if _, err := w.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("\xa9 \xff tab\there\n")); err != nil { // é split across writes, then a bad byte
		t.Fatal(err)
	}
	got := b.String()
	if strings.ContainsAny(got, "\x1b\x07\r\x08\u009b\u202e\u202c") || got != "ok [2J]0;pwned ]52;c;ZXZpbA== ab 31m evil café � tab\there\n" {
		t.Fatalf("%q", got)
	}
}

// `fairy admin audit tail` prints audit entries whose outcomes carry agents' error texts: no escape sequences
// reach the terminal, and a damaged log (a huge line without newline, a half-written last line) still prints.
func TestAuditTailIsTerminalSafeAndBounded(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfg, []byte("public_url: https://p.example\ndata_dir: "+dir+"/data\naudit_log: "+dir+"/audit.log\noidc: {issuer: https://id.example, client_id: c}\n"), 0o600)
	log := filepath.Join(dir, "audit.log")
	if _, err := audit.Write(log, audit.Entry{Actor: "alice", Action: "power.shutdown", Target: "srv-winpc", Outcome: "first"}); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(log, os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.Write(bytes.Repeat([]byte("x"), 3<<20)) // 3 MiB without a newline
	_, _ = f.Write([]byte("\n"))
	f.Close()
	if _, err := audit.Write(log, audit.Entry{Actor: "alice\x1b[31m", Action: "power.shutdown", Target: "srv-winpc",
		Outcome: "error: agent said \x1b]0;owned\x07\x1b[2Jbye"}); err != nil {
		t.Fatal(err)
	}
	f, _ = os.OpenFile(log, os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.Write([]byte(`{"ts":"2026-11-09T00:00:00Z","actor":"half`)) // a line still being written
	f.Close()
	var out, errb bytes.Buffer
	code := Main([]string{"--config", cfg, "--env-file", dir + "/none.env", "audit", "tail", "-n", "5"}, &out, &errb)
	s := out.String()
	if code != 0 || strings.Contains(s, "\x1b") || strings.Contains(s, "\x07") || !strings.Contains(s, "first") ||
		!strings.Contains(s, "agent said ]0;owned[2Jbye") || strings.Count(s, "\n") != 2 {
		t.Fatalf("%d %q %q", code, s, errb.String())
	}
}
