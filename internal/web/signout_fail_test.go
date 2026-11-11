//go:build !windows

package web

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// lockSessionsFile makes sessions.json unwritable for the panel while it stays readable (a full disk, a
// read-only data folder, a file that changed its owner): as root by giving the file to another user, which
// the state files refuse to write over; otherwise by taking the write permission off its folder.
func lockSessionsFile(t *testing.T, e *env) {
	t.Helper()
	lockStateFile(t, e.s.Poller.Admin.SessionsPath)
}

// lockStateFile is lockSessionsFile for any state file. Without root the whole data folder is locked.
func lockStateFile(t *testing.T, p string) {
	t.Helper()
	if os.Geteuid() == 0 {
		if err := os.Chown(p, 65534, 65534); err != nil {
			t.Skip("cannot change the owner of the file:", err)
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

// signInAgain is what the OIDC callback does for a browser that already holds the session sid: a new session
// replaces it.
func (e *env) signInAgain(sid string, u model.User) error {
	c := &reqCtx{s: e.s, w: httptest.NewRecorder(), r: httptest.NewRequest("GET", "/auth/callback", nil)}
	return e.s.startSession(c, sid, u, e.s.nowSec(), e.s.nowSec())
}

// T-29, found in review: a new sign-in ended the session it replaces BEFORE it registered the new one. With
// sessions.json unwritable the old session was ended in memory and the new one could not be registered: a
// signed-in person who went through /auth/login again (own click, or a link on another site) was signed out
// and could not sign in again until the file was fixed. The old session is ended only after the new one is
// recorded: a sign-in that fails leaves the working session alone.
func TestASignInThatCannotBeRecordedLeavesTheWorkingSessionAlone(t *testing.T) {
	e := setup(t)
	u := controller()
	ck := e.cookie(u, 0)
	sid := must(e.s.Poller.Admin.ListSessions())[0].SID
	lockSessionsFile(t, e)
	if err := e.signInAgain(sid, u); err == nil {
		t.Fatal("the sign-in worked although sessions.json cannot be written")
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 200 {
		t.Fatalf("the sign-in failed and the session that worked is gone too: %d", r.code)
	}
}

// A sign-in that is recorded still ends the session it replaces, as before.
func TestASignInThatIsRecordedEndsTheSessionItReplaces(t *testing.T) {
	e := setup(t)
	u := controller()
	ck := e.cookie(u, 0)
	sid := must(e.s.Poller.Admin.ListSessions())[0].SID
	if err := e.signInAgain(sid, u); err != nil {
		t.Fatal(err)
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 401 {
		t.Fatalf("the replaced session still works: %d", r.code)
	}
	if ss := must(e.s.Poller.Admin.ListSessions()); len(ss) != 1 || ss[0].SID == sid {
		t.Fatalf("sessions.json after the new sign-in: %+v", ss)
	}
}

// When users.json cannot be written the sign-in fails after the new session was registered. As before, the
// new session is taken back, the browser's cookie is cleared and the session it had is ended: nothing stays
// registered that no browser holds.
func TestASignInThatFailsAtTheUsersFileLeavesNoSessionBehind(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to lock users.json alone, without sessions.json")
	}
	e := setup(t)
	u := controller()
	ck := e.cookie(u, 0)
	sid := must(e.s.Poller.Admin.ListSessions())[0].SID
	lockStateFile(t, e.s.Access.Path)
	if err := e.signInAgain(sid, u); err == nil {
		t.Fatal("the sign-in worked although users.json cannot be written")
	}
	if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 401 {
		t.Fatalf("the old session after the failed sign-in: %d", r.code)
	}
	if ss := must(e.s.Poller.Admin.ListSessions()); len(ss) != 0 {
		t.Fatalf("sessions.json after the failed sign-in: %+v", ss)
	}
}

// endedCount is how many sessions the panel holds as ended in memory only.
func (e *env) endedCount() int {
	l := e.s.codec.live
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ended)
}

// T-29, found in review: the list of sessions ended in memory took the id of ANY genuinely signed cookie that
// was too old, also ids sessions.json does not list, with one ERROR line each. Someone without a role who had
// kept 4097 old cookies of their own could push a signed-out admin session off the list (4096 entries), and a
// copy of the admin's cookie worked again. Now an id the file does not list is not put on the list, and a
// cookie that is too old is not either (it can never be accepted again: its issue time is signed), so only
// sessions the file still lists get there, and nothing is logged for the rest.
func TestOldCookiesOfAnotherAccountDoNotBringASignedOutSessionBack(t *testing.T) {
	e := setup(t)
	base := time.Now()
	e.s.codec.now = func() time.Time { return base }
	bob := model.User{Sub: "u9", Name: model.P("Bob"), Groups: []string{"viewers"}}
	e.signIn(bob)
	var old []string // genuinely signed cookies of sessions that are not registered (any more)
	for range maxEnded + 1 {
		rec := httptest.NewRecorder()
		e.s.codec.write(rec, &sessionData{SID: randToken(18), User: &bob, IAT: base.Unix(), Seen: base.Unix()})
		old = append(old, strings.Split(rec.Header().Get("Set-Cookie"), ";")[0])
	}
	// one of bob's too old sessions is still registered, as a real one is for up to 30 days
	listedOld := e.staleCookie(bob, 0)
	late := base.Add(time.Duration(e.s.Cfg.SessionMaxAge+3600) * time.Second)
	e.s.codec.now = func() time.Time { return late } // bob's cookies are too old now

	victim := e.cookie(controller(), 0)
	if r := e.do("GET", "/api/me", nil, "Cookie", victim); r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	lockSessionsFile(t, e)
	logged := captureLog(t)
	if r := e.do("POST", "/auth/logout", nil, "Cookie", victim, "X-Requested-With", "fairy", "Accept", "application/json"); r.code != 503 {
		t.Fatalf("sign-out: %d", r.code)
	}
	e.s.codec.now = func() time.Time { return late.Add(2 * time.Second) } // the other account comes after the sign-out
	for i, ck := range append(old, listedOld, listedOld, listedOld) {
		if r := e.do("GET", "/api/me", nil, "Cookie", ck); r.code != 401 {
			t.Fatalf("old cookie %d: %d", i, r.code)
		}
	}
	log := logged.take()
	if r := e.do("GET", "/api/me", nil, "Cookie", victim); r.code != 401 {
		t.Fatalf("the signed-out session works again after %d requests with old cookies of another account: %d", len(old), r.code)
	}
	if n := e.endedCount(); n != 1 {
		t.Fatalf("the list of ended sessions holds %d entries, want only the signed-out one", n)
	}
	if n := strings.Count(log, "level=ERROR"); n != 1 {
		t.Fatalf("%d ERROR lines, want one (the sign-out)", n)
	}
}

// What dropSession does with an id while sessions.json cannot be written: an id the file does not list is
// forgotten without a word; a listed one is ended and logged once, however often it is asked for; and while
// the file cannot even be read, an id is ended (the panel cannot know that it is not listed).
func TestDropSessionWhileTheFileCannotBeWritten(t *testing.T) {
	e := setup(t)
	a := e.s.Poller.Admin
	if err := a.AddSession("listed-session-0001", adminstate.NewSession{Sub: "u2"}); err != nil {
		t.Fatal(err)
	}
	lockSessionsFile(t, e)
	logged := captureLog(t)
	if err := e.s.dropSession("not-listed-session-1", "a test session"); err != nil || e.endedCount() != 0 {
		t.Fatalf("an id the file does not list: err %v, %d ended", err, e.endedCount())
	}
	for range 3 {
		if err := e.s.dropSession("listed-session-0001", "a test session"); err == nil {
			t.Fatal("no error for a listed session that could not be removed")
		}
	}
	log := logged.take()
	if !e.s.codec.live.isEnded("listed-session-0001") || e.endedCount() != 1 {
		t.Fatalf("the listed session is not ended in memory (%d ended)", e.endedCount())
	}
	if n := strings.Count(log, "level=ERROR"); n != 1 {
		t.Fatalf("%d ERROR lines for one session, want 1", n)
	}

	// unreadable: the file is not JSON any more
	if os.Geteuid() == 0 {
		_ = os.Chown(a.SessionsPath, 0, 0)
	} else {
		_ = os.Chmod(filepath.Dir(a.SessionsPath), 0o700)
	}
	if err := os.WriteFile(a.SessionsPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.s.dropSession("cannot-be-checked-01", "a test session"); err == nil || !e.s.codec.live.isEnded("cannot-be-checked-01") {
		t.Fatalf("with an unreadable file the id must be ended in memory: err %v", err)
	}
}

// A session that was ended in memory leaves the list when it can leave the file after all.
func TestAnEndedSessionLeavesTheListOnceTheFileIsWritable(t *testing.T) {
	e := setup(t)
	a := e.s.Poller.Admin
	if err := a.AddSession("listed-session-0001", adminstate.NewSession{Sub: "u2"}); err != nil {
		t.Fatal(err)
	}
	p := a.SessionsPath
	if os.Geteuid() == 0 {
		if err := os.Chown(p, 65534, 65534); err != nil {
			t.Skip(err)
		}
	} else if err := os.Chmod(filepath.Dir(p), 0o500); err != nil {
		t.Skip(err)
	}
	logged := captureLog(t)
	err := e.s.dropSession("listed-session-0001", "a test session")
	_ = logged.take()
	if os.Geteuid() == 0 {
		_ = os.Chown(p, 0, 0)
	} else {
		_ = os.Chmod(filepath.Dir(p), 0o700)
	}
	if err == nil || e.endedCount() != 1 {
		t.Fatalf("not ended in memory: err %v, %d ended", err, e.endedCount())
	}
	if err := e.s.dropSession("listed-session-0001", "a test session"); err != nil {
		t.Fatal(err)
	}
	if n := e.endedCount(); n != 0 {
		t.Fatalf("%d entries left on the list", n)
	}
	if n := len(must(a.ListSessions())); n != 0 {
		t.Fatalf("sessions.json still lists %d", n)
	}
}
