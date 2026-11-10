package admin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/health"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/tokens"
)

// hostile is a field value that tries to print more lines: line feeds, a carriage return, a tab, an escape
// sequence, a bell, the Unicode line ends and the other C0 / C1 line characters.
const hostile = "evil\nFORGED 1\r\nFORGED 2\tcol\x1b[2J\x07\u2028FORGED 3\u2029\v\f\u0085end"

// oneLinePerRow fails when out is not exactly `lines` lines, when a line starts with forged text, or when any
// character a terminal acts on (other than the CLI's own line ends) is left.
func oneLinePerRow(t *testing.T, what, out string, lines int) {
	t.Helper()
	for _, r := range out {
		if r != '\n' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x2028 || r == 0x2029) {
			t.Fatalf("%s: control character %U in the output:\n%q", what, r, out)
		}
	}
	got := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(got) != lines {
		t.Fatalf("%s: %d lines, want %d:\n%s", what, len(got), lines, out)
	}
	for _, l := range got {
		if strings.HasPrefix(strings.TrimSpace(l), "FORGED") {
			t.Fatalf("%s: a forged line:\n%s", what, out)
		}
	}
	if !strings.Contains(out, `evil\nFORGED 1`) {
		t.Fatalf("%s: the line break is not shown as \\n:\n%s", what, out)
	}
}

func TestCellShowsLineBreaksAndTabs(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                    "plain",
		"a\nb":                     `a\nb`,
		"a\r\nb":                   `a\r\nb`,
		"a\tb":                     `a\tb`,
		"a\vb\fc":                  `a\vb\fc`,
		"a\u0085b\u2028c\u2029d":   `a\u0085b\u2028c\u2029d`,
		"a\x1b[31mb\x07\x08\x00":   "a[31mb",
		"caf\xc3 \u202eevil\u202c": "caf\uFFFD evil",
		`C:\new\table`:             `C:\new\table`,
		hostile:                    `evil\nFORGED 1\r\nFORGED 2\tcol[2J\u2028FORGED 3\u2029\v\f\u0085end`,
		"":                         "",
	} {
		if got := cell(in); got != want {
			t.Errorf("cell(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every field `fairy admin` prints in a row or a sentence may hold text someone else chose. Whatever it holds,
// the output has one line per row: line breaks and tabs are printed as \n, \r, \t.
func TestEveryPrintedFieldStaysOnItsLine(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	cfg := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfg, []byte("public_url: https://p.example\ndata_dir: "+data+"\noidc: {issuer: https://id.example, client_id: c}\n"), 0o600)
	run := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		code := Main(append([]string{"--config", cfg, "--env-file", dir + "/none.env"}, args...), &out, &errb)
		return code, out.String() + errb.String()
	}
	logPath := filepath.Join(data, "audit.log")
	ip := hostile

	// audit tail: the outcome is an agent's error text; actor and target come from the identity provider and
	// the requested address. Three complete fake entries inside one outcome stay one row.
	fake := "error 502: agent said\n" +
		"2026-11-10 12:00:00  ui               alice@example.com      user.set_role              bob@example.com            ok\n" +
		"2026-11-10 12:00:01  ui               alice@example.com      power.shutdown             srv-winpc                   ok\n" +
		"2026-11-10 12:00:02  cli              root                     control.enable             -                            ok"
	if _, err := audit.Write(logPath, audit.Entry{Actor: "alice", Action: "fan.mode", Target: "srv-winpc", Outcome: fake}); err != nil {
		t.Fatal(err)
	}
	code, out := run("audit", "tail", "-n", "5")
	if got := strings.Split(strings.TrimRight(out, "\n"), "\n"); code != 0 || len(got) != 1 ||
		!strings.Contains(got[0], `agent said\n2026-11-10 12:00:00  ui`) || !strings.Contains(got[0], `control.enable`) {
		t.Fatalf("three fake entries in one outcome must print as one row: %d\n%s", code, out)
	}
	sub := hostile
	if _, err := audit.Write(logPath, audit.Entry{Actor: hostile, Sub: &sub, Action: hostile, Target: hostile, Outcome: hostile,
		Source: hostile, IP: &ip, Params: map[string]any{"x": hostile}}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"ts":"evil\nFORGED 1\r\nFORGED 2","actor":"a","action":"b","target":"c","outcome":"d","source":"ui","ip":"1.2.3.4"}` + "\n")
	f.Close()
	code, out = run("audit", "tail", "-n", "5")
	if code != 0 {
		t.Fatal(code, out)
	}
	oneLinePerRow(t, "audit tail", out, 3)
	for _, field := range []string{"source", "actor", "action", "target", "outcome", "ip", "sub"} { // each field alone
		e := map[string]any{"ts": "2026-11-10T12:00:00.000000+00:00", "source": "ui", "actor": "a", "action": "b", "target": "c",
			"outcome": "ok", "ip": "1.2.3.4", "sub": "s"}
		e[field] = hostile
		oneLinePerRow(t, "audit entry, field "+field, fmtEntry(e), 1)
	}
	code, out = run("audit", "tail", "-n", "5", "--json") // --json is unchanged: one JSON document per line
	if code != 0 || strings.Count(out, "\n") != 3 || !strings.Contains(out, `evil\nFORGED 1\r\nFORGED 2\tcol`) || strings.Contains(out, "\x1b") {
		t.Fatalf("%d %q", code, out)
	}

	// health: source names, states and error texts are chosen by agents and devices; the kill switch's reason
	// is free text.
	now := time.Now().UTC()
	if err := health.Write(data, health.Report{
		StartedAt: hostile, WrittenAt: now.Format(time.RFC3339Nano),
		Systems:      map[string]int{"online": 1},
		Sources:      []model.SourceStatus{{Name: hostile, OK: false, LastError: model.P(hostile), LastSuccess: &now}},
		PowerSources: []health.Source{{ID: hostile, Status: hostile, Error: model.P(hostile)}},
		UPS:          []health.Source{{ID: hostile, Status: hostile, Error: model.P(hostile)}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := adminstate.New(data).SetControlDisabled(true, hostile, hostile); err != nil {
		t.Fatal(err)
	}
	code, out = run("health")
	if code != 0 {
		t.Fatal(code, out)
	}
	oneLinePerRow(t, "health", out, 3+2+3) // panel, control, systems; table head and rule; three sources
	for _, w := range []string{`power:evil\nFORGED 1`, `ups:evil\nFORGED 1`, `control: DISABLED by evil\nFORGED 1`} {
		if !strings.Contains(out, w) {
			t.Fatalf("health: missing %q in\n%s", w, out)
		}
	}

	// users and sites: subject, name and e-mail are what the identity provider sent.
	st := access.NewStore(data)
	if err := st.CreateSite("Lab"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Seen(hostile, hostile, hostile, access.RolePending, hostile); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Seen("sub-ok", "ok@example.com", "Ok", access.RoleAdmin, "test"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetRole(hostile, access.RoleViewer, []string{"Lab"}, hostile, false); err != nil {
		t.Fatal(err)
	}
	code, out = run("users", "list")
	if code != 0 {
		t.Fatal(code, out)
	}
	oneLinePerRow(t, "users list", out, 2+2+3) // table, an empty line, two lines about "(unverified)"
	code, out = run("sites", "list")
	if code != 0 {
		t.Fatal(code, out)
	}
	oneLinePerRow(t, "sites list", out, 2+1+3) // table with one site, then an empty line and two notes

	// tokens and sessions: the owner is a subject, the session's name, e-mail and address were sent by a client.
	if _, _, err := tokens.NewStore(data).CreateForSites(hostile, hostile, "read", hostile, 0, []string{"Lab"}); err != nil {
		t.Fatal(err)
	}
	code, out = run("tokens", "list")
	if code != 0 {
		t.Fatal(code, out)
	}
	oneLinePerRow(t, "tokens list", out, 2+1)
	if err := adminstate.New(data).AddSession("0123456789abcdef", adminstate.NewSession{Sub: hostile, Name: model.P(hostile),
		Email: model.P(hostile), IP: hostile}); err != nil {
		t.Fatal(err)
	}
	code, out = run("sessions", "list")
	if code != 0 {
		t.Fatal(code, out)
	}
	oneLinePerRow(t, "sessions list", out, 2+1)

	// sentences about one user: a role change and a removal print the account found, then what was done. An
	// address that accounts only claim lists them instead.
	if _, _, err := st.Seen("sub-h2", "x"+hostile, hostile, access.RolePending, hostile); err != nil {
		t.Fatal(err)
	}
	code, out = run("users", "set-role", "x"+hostile, "viewer")
	if code != 1 {
		t.Fatal(code, out)
	}
	oneLinePerRow(t, "users set-role by a claimed address", out, 1+1+3+1) // count, the account, three lines of advice, the error
	code, out = run("users", "set-role", hostile, "pending")
	if code != 0 {
		t.Fatal(code, out)
	}
	oneLinePerRow(t, "users set-role", out, 2)
	code, out = run("users", "set-role", hostile, "admin", "--yes")
	if code != 0 {
		t.Fatal(code, out)
	}
	oneLinePerRow(t, "users set-role admin", out, 2)
	stdin = strings.NewReader("no\n")
	t.Cleanup(func() { stdin = os.Stdin })
	code, out = run("users", "set-role", "sub-h2", "admin")
	if code != 1 {
		t.Fatal(code, out)
	}
	oneLinePerRow(t, "users set-role admin, asked", out, 2) // the account; the question, whose line the typed answer ends
	code, out = run("users", "remove", hostile)
	if code != 0 {
		t.Fatal(code, out)
	}
	oneLinePerRow(t, "users remove", out, 3)
}

// L4 (audit part): `audit tail` names the account by its id next to what the account calls itself, so an entry
// of an account that only claims an address cannot be taken for the owner of that address.
func TestAuditTailShowsTheAccountID(t *testing.T) {
	base := map[string]any{"ts": "2026-11-10T12:00:00.000000+00:00", "source": "ui", "action": "settings.update", "target": "",
		"outcome": "ok", "ip": "203.0.113.9"}
	with := func(kv ...string) map[string]any {
		e := map[string]any{}
		for k, v := range base {
			e[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			e[kv[i]] = kv[i+1]
		}
		return e
	}
	impostor := fmtEntry(with("actor", "owner@example.com (unverified)", "sub", "impostor-sub"))
	owner := fmtEntry(with("actor", "owner@example.com", "sub", "3f6c1d2e-owner-0001"))
	if !strings.HasSuffix(impostor, "  id impostor-sub") || !strings.Contains(impostor, "owner@example.com (unve") ||
		!strings.HasSuffix(owner, "  id 3f6c1d2e-owner-0001") || impostor == owner {
		t.Fatalf("\n%s\n%s", impostor, owner)
	}
	// an entry written before this release has the bare address: the id still tells the two apart
	old := fmtEntry(with("actor", "owner@example.com", "sub", "impostor-sub"))
	if !strings.HasSuffix(old, "  id impostor-sub") {
		t.Fatal(old)
	}
	// no id to show: an entry without a subject (a failed sign-in), or one whose actor is the subject itself
	if s := fmtEntry(with("actor", "?")); strings.Contains(s, "  id ") {
		t.Fatal(s)
	}
	if s := fmtEntry(with("actor", "u-77", "sub", "u-77")); strings.Contains(s, "  id ") {
		t.Fatal(s)
	}
}
