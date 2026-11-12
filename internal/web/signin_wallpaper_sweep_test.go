package web

import (
	"testing"
	"time"
)

// Audit 6, r4-p3-08. A sign-in picture that waits for "Confirm it's you" is taken out of memory when the
// confirmation comes. After Cancel, Escape or a dialog that is left open nothing took it: it stayed until the
// next upload or a restart (up to four pictures of 8 MiB), although nothing can reach it after three minutes.
// The server's minute tick lets go of it now.
func TestAStagedSignInPictureIsDroppedWhenItsTimeIsOver(t *testing.T) {
	a := signedIn(t) // an admin whose sign-in at Pocket ID is an hour old: the upload asks to confirm
	e := a.e
	e.s.uploadRL = newRateLimiter(1000)
	pngPic, _, _ := wallPictures(t)
	r := e.wallUpload(pngPic, "image/png", "Cookie", a.sess, "X-Requested-With", "fairy")
	if d, _ := r.body["detail"].(map[string]any); r.code != 401 || d["code"] != "step_up_required" || d["pending"] != true {
		t.Fatalf("the upload must ask to confirm: %d %s", r.code, r.raw)
	}
	waiting := func() int {
		e.s.wallStage.mu.Lock()
		defer e.s.wallStage.mu.Unlock()
		return len(e.s.wallStage.m)
	}
	if waiting() != 1 {
		t.Fatalf("the set-up: %d pictures wait, want 1", waiting())
	}
	// nobody confirms. While its time runs the picture waits ...
	a.clk.advance(pendingMaxAge - time.Second)
	e.s.minutely(time.Now())
	if waiting() != 1 {
		t.Fatalf("a picture whose time is not over was dropped (%d wait)", waiting())
	}
	// ... and when it is over, the next minute tick lets go of it: no upload and no restart is needed
	a.clk.advance(2 * time.Second)
	e.s.minutely(time.Now())
	if n := waiting(); n != 0 {
		t.Fatalf("%d picture(s) still in memory after %v without a confirmation", n, pendingMaxAge+time.Second)
	}
	if r := e.do("GET", wallURL, nil); r.code != 204 {
		t.Fatal("a picture was stored without a confirmation")
	}
}

// The tick is the one the server already has (the summary of throttled audit entries): Serve calls minutely.
func TestTheMinuteTickSweepsTheStagedPictures(t *testing.T) {
	e := setup(t)
	now := e.s.now()
	e.s.wallStage.put("s1", []byte("old"), now.Add(-pendingMaxAge-time.Second))
	e.s.wallStage.put("s2", []byte("new"), now)
	e.s.minutely(time.Now())
	if len(e.s.wallStage.m) != 1 {
		t.Fatalf("%d pictures wait, want the new one only", len(e.s.wallStage.m))
	}
	for _, p := range e.s.wallStage.m {
		if string(p.data) != "new" {
			t.Errorf("the picture that stayed: %q", p.data)
		}
	}
}
