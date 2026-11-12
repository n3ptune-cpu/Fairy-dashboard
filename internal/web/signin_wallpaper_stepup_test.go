package web

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// The owner's decision after r2-p1-02 ("sign-in wallpaper without Confirm it's you"): changing the picture on
// the sign-in page, which everyone sees before any sign-in, asks for "Confirm it's you" like the other admin
// actions. Upload and removal; the same window; the kept action runs by itself after the sign-in. A user's own
// background does not ask.
func TestSignInWallpaperChangeAsksToConfirm(t *testing.T) {
	a := signedIn(t) // an admin whose sign-in at Pocket ID is an hour old
	e := a.e
	e.s.uploadRL = newRateLimiter(1000)
	pngPic, jpg, _ := wallPictures(t)
	hdr := func() []string { return []string{"Cookie", a.sess, "X-Requested-With", "fairy"} }
	asked := func(r resp, action string) string {
		t.Helper()
		d, _ := r.body["detail"].(map[string]any)
		if r.code != 401 || d["code"] != "step_up_required" || d["pending"] != true || d["action"] != action || d["window_s"] != float64(180) ||
			d["message"] != "Confirm it's you: changes to the sign-in picture need a Pocket ID sign-in from the last 3 min." {
			t.Fatalf("must ask to confirm %q: %d %s", action, r.code, r.raw)
		}
		return pendingID(r)
	}
	// a file the panel will not take is refused at once, with the reason: nobody is asked for a passkey for it
	if r := e.wallUpload([]byte("<svg xmlns='http://www.w3.org/2000/svg'/>"), "image/svg+xml", hdr()...); r.code != 415 {
		t.Fatalf("an SVG with an old sign-in: %d %s", r.code, r.raw)
	}
	if r := e.wallUpload([]byte(`{"staged":"x"}`), "application/json", hdr()...); r.code != 415 { // the kept request's body is nothing a browser can send
		t.Fatalf("the name of a waiting picture sent as an upload: %d %s", r.code, r.raw)
	}
	for n, want := range map[int]string{97: "97 B", 1023: "1023 B", 1024: "1.0 KiB", 320100: "313 KiB", 8 << 20: "8.0 MiB"} {
		if got := byteText(n); got != want {
			t.Errorf("byteText(%d) = %q, want %q", n, got, want)
		}
	}
	// the upload
	id := asked(e.wallUpload(pngPic, "image/png", hdr()...), "change the sign-in picture (PNG, "+byteText(len(pngPic))+")")
	if r := e.do("GET", wallURL, nil); r.code != 204 {
		t.Fatal("the picture was stored before the confirmation:", r.code)
	}
	if n := len(e.auditEntries("", "settings.signin_wallpaper_upload")); n != 0 {
		t.Fatal("logged as a change before the confirmation")
	}
	if r := a.confirm(id, false); r.code != 303 {
		t.Fatalf("the confirmation: %d %s", r.code, r.raw)
	}
	if r := e.do("GET", wallURL, nil); r.code != 200 || !bytes.Equal(r.raw, pngPic) || r.hdr.Get("Content-Type") != "image/png" {
		t.Fatalf("after the confirmation the picture is not on the sign-in page: %d", r.code)
	}
	me := e.do("GET", "/api/me", nil, "Cookie", a.sess)
	if pr, _ := me.body["pending_result"].(map[string]any); pr == nil || pr["ok"] != true || pr["message"] != "done" || pr["status"] != float64(201) {
		t.Fatalf("the result the page shows: %v", me.body["pending_result"])
	}
	log := auditOutcomes(e.s.Cfg.AuditLog)
	for _, want := range []string{"auth.step_up required", "auth.step_up redirected to Pocket ID", "auth.step_up ok", "settings.signin_wallpaper_upload ok"} {
		if !strings.Contains(log, want) {
			t.Errorf("audit log has no %q:\n%s", want, log)
		}
	}
	if up := e.auditEntries("u1", "auth.step_up"); len(up) == 0 || !strings.Contains(fmt.Sprint(up), "change the sign-in picture (PNG") {
		t.Errorf("the confirmation's audit entry does not name the action: %v", up)
	}
	if len(e.s.wallStage.m) != 0 {
		t.Fatalf("%d pictures still wait", len(e.s.wallStage.m))
	}
	// inside the window: no question (another picture, at once)
	if r := e.wallUpload(jpg, "image/jpeg", hdr()...); r.code != 201 || r.body["type"] != "image/jpeg" || len(e.s.wallStage.m) != 0 {
		t.Fatalf("inside the window: %d %s", r.code, r.raw)
	}
	// the removal, after the window
	a.clk.advance(4 * time.Minute)
	id = asked(e.raw(probe{method: "DELETE", path: wallAPI}, hdr()...), "remove the sign-in picture")
	if r := e.do("GET", wallURL, nil); r.code != 200 {
		t.Fatal("the picture was removed before the confirmation:", r.code)
	}
	if r := a.confirm(id, false); r.code != 303 {
		t.Fatalf("the confirmation: %d %s", r.code, r.raw)
	}
	if r := e.do("GET", wallURL, nil); r.code != 204 {
		t.Fatal("after the confirmation the picture is still there:", r.code)
	}
	if !strings.Contains(auditOutcomes(e.s.Cfg.AuditLog), "settings.signin_wallpaper_delete ok") {
		t.Error("the removal is not in the audit log")
	}
	// the route asks also when no picture is set (one rule for the route; the page's button is off then), and
	// removing nothing is then not logged as a change
	a.clk.advance(4 * time.Minute)
	id = asked(e.raw(probe{method: "DELETE", path: wallAPI}, hdr()...), "remove the sign-in picture")
	if r := a.confirm(id, false); r.code != 303 || len(e.auditEntries("", "settings.signin_wallpaper_delete")) != 1 {
		t.Fatalf("removal with no picture set: %d, %d audit entries", r.code, len(e.auditEntries("", "settings.signin_wallpaper_delete")))
	}

	// a picture waits as long as a kept action (3 minutes) and no longer; and at most four wait at a time
	a.clk.advance(4 * time.Minute)
	id = asked(e.wallUpload(pngPic, "image/png", hdr()...), "change the sign-in picture (PNG, "+byteText(len(pngPic))+")")
	a.clk.advance(pendingMaxAge + time.Second)
	if _, _, r := a.idp.tryConfirmStart(e, id, "/", a.sess); r.code != 409 {
		t.Fatalf("a confirmation after more than 3 minutes: %d %s", r.code, r.raw)
	}
	for range maxStagedPictures + 2 {
		asked(e.wallUpload(pngPic, "image/png", hdr()...), "change the sign-in picture (PNG, "+byteText(len(pngPic))+")")
	}
	if n := len(e.s.wallStage.m); n != 1 { // one session keeps one action: the picture of the one before is let go of
		t.Fatalf("%d pictures wait for one session", n)
	}
	for i := range maxStagedPictures + 3 { // other admins' sessions: the room is bounded, the oldest goes
		e.s.wallStage.put(fmt.Sprint("session-", i), pngPic, a.clk.now())
	}
	if n := len(e.s.wallStage.m); n != maxStagedPictures {
		t.Fatalf("%d pictures wait (at most %d)", n, maxStagedPictures)
	}
	e.s.wallStage.m = nil
	if r := e.do("GET", wallURL, nil); r.code != 204 {
		t.Fatal("a picture was stored without a confirmation")
	}

	// step_up_per_action: every change needs its own sign-in, also right after one
	e.s.Cfg.StepUpPerAction = true
	r := e.wallUpload(pngPic, "image/png", hdr()...)
	d, _ := r.body["detail"].(map[string]any)
	if r.code != 401 || d["pending"] != true || d["per_action"] != true {
		t.Fatalf("per action: %d %s", r.code, r.raw)
	}
	if r := a.confirm(pendingID(r), false); r.code != 303 || e.do("GET", wallURL, nil).code != 200 {
		t.Fatalf("per action, the confirmation: %d", r.code)
	}
	if r := e.wallUpload(jpg, "image/jpeg", hdr()...); r.code != 401 {
		t.Fatalf("per action, the next change right after: %d", r.code)
	}
	e.s.Cfg.StepUpPerAction = false

	// a user's OWN background asks nothing, with a sign-in of any age
	vee := model.User{Sub: "u7", Name: model.P("Vee"), Groups: []string{"viewers"}}
	e.signIn(vee)
	if _, _, err := e.s.Access.SetRole("u7", "viewer", []string{}, "test", false); err != nil {
		t.Fatal(err)
	}
	old := e.staleCookie(vee, 24*time.Hour)
	if r := e.raw(probe{method: "POST", path: "/api/settings/background", body: pngPic, ctype: "image/png"}, "Cookie", old, "X-Requested-With", "fairy"); r.code != 201 {
		t.Fatalf("a user's own background with an old sign-in: %d %s", r.code, r.raw)
	}
	if r := e.raw(probe{method: "DELETE", path: "/api/settings/background"}, "Cookie", old, "X-Requested-With", "fairy"); r.code != 200 {
		t.Fatalf("removing one's own background with an old sign-in: %d %s", r.code, r.raw)
	}
}
