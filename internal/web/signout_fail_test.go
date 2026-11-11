//go:build !windows

package web

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
)

// lockSessionsFile makes sessions.json unwritable for the panel while it stays readable (a full disk, a
// read-only data folder, a file that changed its owner): as root by giving the file to another user, which
// the state files refuse to write over; otherwise by taking the write permission off its folder.
func lockSessionsFile(t *testing.T, e *env) {
	t.Helper()
	p := e.s.Poller.Admin.SessionsPath
	if os.Geteuid() == 0 {
		if err := os.Chown(p, 65534, 65534); err != nil {
			t.Skip("cannot change the owner of sessions.json:", err)
		}
		t.Cleanup(func() { _ = os.Chown(p, 0, 0) })
		return
	}
	dir := filepath.Dir(p)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot make the data folder read-only:", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// T-29: signing out removes the session from sessions.json. When that file could not be written, the error
// was dropped: the answer was "done", the audit entry said "ok", and the session stayed valid, so a copy of
// the cookie elsewhere kept working after its owner had signed out. Now the sign-out says that it could not
// be recorded, it is logged and audited as failed, and the session is ended in the panel's memory all the
// same: its cookie stops working (until a restart reads the file again, which the message says).
func TestSignOutThatCannotBeRecordedStillEndsTheSession(t *testing.T) {
	e := setup(t)
	ck := e.cookie(controller(), 0)
	copyElsewhere := ck
	if r := e.do("GET", "/api/me", nil, "Cookie", copyElsewhere); r.code != 200 {
		t.Fatal(r.code)
	}
	lockSessionsFile(t, e)
	var log bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&log, nil)))
	r := e.do("POST", "/auth/logout", nil, "Cookie", ck, "X-Requested-With", "fairy", "Accept", "application/json")
	slog.SetDefault(old)

	if r.code != 503 || !strings.Contains(string(r.raw), "sessions.json") {
		t.Fatalf("the sign-out that could not be recorded answered %d %s", r.code, r.raw)
	}
	if sc := strings.Join(r.hdr.Values("Set-Cookie"), "\n"); !strings.Contains(sc, "Max-Age=0") {
		t.Fatalf("the browser's cookie is not cleared: %q", sc)
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", copyElsewhere); r.code != 401 {
		t.Fatalf("a copy of the cookie after the sign-out: %d", r.code)
	}
	if r := e.do("GET", "/api/stream", nil, "Cookie", copyElsewhere); r.code != 401 {
		t.Fatalf("the live stream with a copy of the cookie after the sign-out: %d", r.code)
	}
	if !strings.Contains(log.String(), "level=ERROR") || !strings.Contains(log.String(), "sessions.json") {
		t.Fatalf("nothing in the log: %s", log.String())
	}
	entries, _ := audit.Read(e.s.Cfg.AuditLog, "", "", "", nil, 5)
	found := false
	for _, en := range entries {
		if en["action"] == "auth.logout" {
			found = true
			if out, _ := en["outcome"].(string); !strings.HasPrefix(out, "failed") {
				t.Fatalf("the sign-out is audited as %q", out)
			}
		}
	}
	if !found {
		t.Fatal("no audit entry for the sign-out")
	}
	// the file could not be changed: it still lists the session, and the panel does not use it
	if n := len(must(e.s.Poller.Admin.ListSessions())); n != 1 {
		t.Fatalf("sessions.json lists %d sessions", n)
	}
}

// T-29: a sign-out that can be recorded answers as before.
func TestSignOutAnswersAsBefore(t *testing.T) {
	e := setup(t)
	ck := e.cookie(controller(), 0)
	if r := e.do("POST", "/auth/logout", nil, "Cookie", ck, "X-Requested-With", "fairy", "Accept", "application/json"); r.code != 204 {
		t.Fatal("fetch:", r.code)
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 401 {
		t.Fatal(r.code)
	}
	ck = e.cookie(controller(), 0)
	if r := e.do("POST", "/auth/logout", nil, "Cookie", ck, "X-Requested-With", "fairy"); r.code != 303 {
		t.Fatal("navigation:", r.code)
	}
}
