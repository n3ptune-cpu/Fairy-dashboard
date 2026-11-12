package admin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// r3-p1-09 (audit 5, round 3; the auditor's poc/cli_list.sh). `fairy admin sessions list` printed, under
// "SIGNED IN", the time the entry was made, which a "Confirm it's you" renews: a session signed in 5 hours ago
// showed "2m ago", while the Sessions page showed the sign-in. And it listed every entry of sessions.json as a
// session, also one that ended three days ago (an entry stays up to 30 days).
func TestSessionsListShowsTheSignInAndMarksEndedSessions(t *testing.T) {
	e := newCLI(t)
	now := float64(time.Now().Unix())
	iso := func(ts float64) string { return time.Unix(int64(ts), 0).UTC().Format(time.RFC3339) }
	rec := func(sub, mail string, created, login, seen float64) map[string]any {
		m := map[string]any{"sub": sub, "name": strings.Split(mail, "@")[0], "email": mail, "ip": "203.0.113.7", "gen": 2,
			"created": iso(created), "created_ts": created, "ua": "Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0"}
		if login > 0 {
			m["login_ts"], m["login_ip"] = login, "203.0.113.7"
		}
		if seen > 0 {
			m["seen_ts"], m["seen_ip"] = seen, "203.0.113.7"
		}
		return m
	}
	file := map[string]any{"sessions": map[string]any{
		// A: signed in 5 h ago, id renewed by a confirmation 2 min ago, used 1 min ago (one day, 4 h idle: open)
		"AAAAaaaa1111bbbb2222cccc": rec("sub-a", "ada@example.test", now-120, now-5*3600, now-60),
		// B: signed in and last used 3 days ago: long ended
		"BBBBbbbb3333cccc4444dddd": rec("sub-b", "bob@example.test", now-3*86400, now-3*86400, now-3*86400),
		// C: signed in 6 h ago, not used for 5 h: idled out
		"CCCCcccc5555dddd6666eeee": rec("sub-c", "cy@example.test", now-6*3600, now-6*3600, now-5*3600),
		// D: an entry from before the sign-in time and the last use were written: made 10 min ago, nothing else known
		"DDDDdddd7777eeee8888ffff": rec("sub-d", "di@example.test", now-600, 0, 0),
	}}
	b, _ := json.Marshal(file)
	if err := os.MkdirAll(e.data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.data, "sessions.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := e.run("", "sessions", "list")
	if code != 0 {
		t.Fatal(code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "SESSION USER IP SIGNED IN LAST USED STATE" {
		t.Fatalf("headings: %q\n%s", lines[0], out)
	}
	row := func(prefix string) string {
		for _, l := range lines {
			if strings.HasPrefix(l, prefix) {
				return regexp.MustCompile(`\s{2,}`).ReplaceAllString(strings.TrimSpace(l), " | ")
			}
		}
		t.Fatalf("no row for %s:\n%s", prefix, out)
		return ""
	}
	for prefix, want := range map[string]string{
		"AAAAaaaa…": `^AAAAaaaa… \| ada@example\.test \| 203\.0\.113\.7 \| 5\.0h ago \| 6\ds ago \| open$`,
		"BBBBbbbb…": `^BBBBbbbb… \| bob@example\.test \| 203\.0\.113\.7 \| 3d ago \| 3d ago \| ended \(signed in too long ago\)$`,
		"CCCCcccc…": `^CCCCcccc… \| cy@example\.test \| 203\.0\.113\.7 \| 6\.0h ago \| 5\.0h ago \| ended \(not used\)$`,
		"DDDDdddd…": `^DDDDdddd… \| di@example\.test \| 203\.0\.113\.7 \| 10m ago \| not known \| open$`,
	} {
		if got := row(prefix); !regexp.MustCompile(want).MatchString(got) {
			t.Errorf("row:\n got  %s\n want %s", got, want)
		}
	}
	if !strings.Contains(out, "ended: the panel no longer takes this session (session_max_age, session_idle_timeout).") {
		t.Errorf("the list does not say what \"ended\" means:\n%s", out)
	}
	// an ended entry can still be removed by its handle, as before
	if code, out := e.run("", "sessions", "revoke", "BBBBbbbb…"); code != 0 || !strings.Contains(out, "Revoked 1 session") {
		t.Fatal(code, out)
	}
	if _, out := e.run("", "sessions", "list"); strings.Contains(out, "BBBBbbbb") || strings.Contains(out, "signed in too long ago") {
		t.Fatalf("after the revoke:\n%s", out)
	}
}
