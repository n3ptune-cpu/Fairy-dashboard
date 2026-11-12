package web

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/security"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/settings"
)

// The sign-in wallpaper: the picture behind the sign-in page. An admin uploads or removes it on the Settings
// page; the panel keeps one copy (settings.Store, signin.go) and shows it to everyone who can open the panel,
// because the sign-in page comes before any sign-in.
//
// It is made from the two things that were there: stored and checked like a user's uploaded background
// (api_settings.go), served like the custom logo (branding.go): without a sign-in, from memory, with an ETag.
// The public route takes nothing from the request that says which file to send: there is one picture or none.

// At most maxWallpapersPerIP downloads of the wallpaper are in flight for one client address and
// maxWallpapersTotal for the panel; one more is answered 429 and the sign-in page shows its plain background.
// A download gets backgroundWriteTimeout like a user's background: it may be 8 MiB on a slow link.
const (
	maxWallpapersPerIP = 4
	maxWallpapersTotal = 64
)

func (s *Server) wallpaperRoutes(m *http.ServeMux) {
	s.wall = &streamSlots{perUser: maxWallpapersPerIP, total: maxWallpapersTotal, open: map[string]int{}}
	m.HandleFunc("GET /branding/wallpaper", s.serveWallpaper)
	m.HandleFunc("GET /api/admin/signin-wallpaper", s.h(s.getWallpaper))
	m.HandleFunc("POST /api/admin/signin-wallpaper", s.h(s.uploadWallpaper))
	m.HandleFunc("DELETE /api/admin/signin-wallpaper", s.h(s.deleteWallpaper))
}

// serveWallpaper is GET /branding/wallpaper: the stored picture, no sign-in needed. It has no parameters: the
// only thing read from the request's address is whether ?v= names the picture that is stored now, and that
// decides nothing but how long a browser may keep the answer. The type is the one the content was found to
// be when it was stored (PNG, JPEG or WebP), never one a client sent.
func (s *Server) serveWallpaper(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	p := s.Settings.SignIn()
	if p == nil {
		// No picture is set (the default): 204, an answer without a picture, as for the logo (serveLogo). The
		// sign-in page's style sheet asks on every load; a 404 was an error line in the browser's console each
		// time (r3-p3-04).
		h.Set("Cache-Control", cacheNoCache)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	release, ok := s.wall.acquire(security.ClientIP(r, s.Cfg.Trusted()))
	if !ok {
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "no-store")
		h.Set("Retry-After", "2")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"detail":"too many downloads of the sign-in wallpaper at the same time"}`))
		return
	}
	defer release()
	h.Set("Content-Type", p.Type)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("ETag", `"`+p.Version+`"`)
	if r.URL.Query().Get("v") == p.Version { // this address names this very picture: it never changes
		h.Set("Cache-Control", cacheImmutable)
	} else { // kept by the browser, asked about every time (304), so a new picture shows at once
		h.Set("Cache-Control", cacheNoCache)
	}
	timeout := backgroundWriteTimeout
	if s.answerTimeout < answerWriteTimeout { // a test shortened the deadlines
		timeout = s.answerTimeout
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout))
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(p.Data)) // answers If-None-Match, HEAD and Range
}

// wallpaperAdmin: an admin with a browser session (API tokens are refused, like on the settings endpoints);
// a change also needs the CSRF header and the same origin, and then "Confirm it's you" (wallpaperStepUp).
func (c *reqCtx) wallpaperAdmin(write bool) (*model.User, error) {
	u, err := c.requireSession()
	if err != nil {
		return nil, err
	}
	if write {
		if err := c.checkCSRF(u); err != nil {
			return nil, err
		}
	}
	if _, g, _ := c.grant(); !g.Admin() {
		return nil, c.deny(u, 403, "only admins can change the sign-in wallpaper", "not an admin")
	}
	return u, nil
}

// wallpaperState is what the Settings page gets: whether a picture is set, and which.
func wallpaperState(p *settings.SignInPicture) map[string]any {
	out := map[string]any{"set": p != nil, "type": nil, "bytes": 0, "version": nil, "max_bytes": settings.MaxBackgroundBytes}
	if p != nil {
		out["type"], out["bytes"], out["version"] = p.Type, len(p.Data), p.Version
	}
	return out
}

func (s *Server) getWallpaper(c *reqCtx) error {
	if _, err := c.wallpaperAdmin(false); err != nil {
		return err
	}
	c.json(200, wallpaperState(s.Settings.SignIn()))
	return nil
}

// A change of the sign-in picture asks for "Confirm it's you", as the other admin actions do (the owner's
// decision after r2-p1-02: the picture is shown to everyone who can open the panel's address, before any
// sign-in, and a session cookie alone could replace it). The same window and the same kept-action flow
// (pending.go): a request that is refused is kept and runs by itself right after the sign-in.
//
// An upload is too large to be kept as a request (pendingMaxBody), so what is kept for it is a small request
// that names the picture, and the picture waits beside it: checked already, in memory, for as long as a kept
// action lives, at most maxStagedPictures at a time. The name is a random value that never leaves the panel.
const wallpaperStepUp = "changes to the sign-in picture"

// maxStagedPictures bounds the pictures that wait for a confirmation (8 MiB each at most; admins only).
const maxStagedPictures = 4

type stagedPicture struct {
	data  []byte
	at    time.Time
	owner string // the session that asked: it keeps one action, so one picture waits for it
}

// stagedPictures holds the sign-in pictures that wait for "Confirm it's you", by their name.
type stagedPictures struct {
	mu sync.Mutex
	m  map[string]stagedPicture
}

// put keeps data for the session owner and returns its name. Pictures that waited too long go, and the one
// that waited for the same session; when the room is full, the oldest.
func (s *stagedPictures) put(owner string, data []byte, now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]stagedPicture{}
	}
	oldest := ""
	for k, p := range s.m {
		if now.Sub(p.at) > pendingMaxAge || p.owner == owner {
			delete(s.m, k)
		} else if oldest == "" || p.at.Before(s.m[oldest].at) {
			oldest = k
		}
	}
	if len(s.m) >= maxStagedPictures {
		delete(s.m, oldest)
	}
	name := randToken(18)
	s.m[name] = stagedPicture{data, now, owner}
	return name
}

// take hands the picture out, once (nil: it waited too long, was pushed out, or the panel restarted).
func (s *stagedPictures) take(name string, now time.Time) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[name]
	delete(s.m, name)
	if !ok || now.Sub(p.at) > pendingMaxAge {
		return nil
	}
	return p.data
}

// peek is take without taking (for the dialog's description of the kept action).
func (s *stagedPictures) peek(name string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[name].data
}

// stagedName reads the kept request of an upload: {"staged": name}.
func stagedName(body []byte) string {
	var b struct {
		Staged string `json:"staged"`
	}
	if !decodeStrictly(body, &b) || len(b.Staged) > 64 {
		return ""
	}
	return b.Staged
}

// wallpaperDetail is what the "Confirm it's you" dialog adds to "change the sign-in picture": which picture.
func (c *reqCtx) wallpaperDetail(*model.User) string {
	data := c.s.wallStage.peek(stagedName(c.body))
	if data == nil {
		return ""
	}
	kind := map[string]string{"image/png": "PNG", "image/jpeg": "JPEG", "image/webp": "WebP"}[settings.SignInType(data)]
	return kind + ", " + byteText(len(data))
}

// byteText is a size in the page's own units (fmtBytes in app.js).
func byteText(n int) string {
	units := []string{"B", "KiB", "MiB"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if v >= 100 || i == 0 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// uploadWallpaper stores the request's body as the sign-in wallpaper. What the browser calls the file (its
// name, its Content-Type) is not looked at: the content says what it is (settings.SignInType).
func (s *Server) uploadWallpaper(c *reqCtx) error {
	u, err := c.wallpaperAdmin(true)
	if err != nil {
		return err
	}
	var data []byte
	if c.resumed() { // the panel's own replay right after "Confirm it's you": the picture that waited
		if data = s.wallStage.take(stagedName(c.body), s.now()); data == nil {
			return apierr.New(409, "the picture is no longer waiting for the confirmation (it waits 3 minutes): upload it again")
		}
		if err := c.stepUp(u, wallpaperStepUp); err != nil {
			return err
		}
	} else {
		if !s.uploadRL.allow(u.Sub) {
			return apierr.New(429, "too many uploads; try again in a minute")
		}
		if c.r.ContentLength < 0 { // a body of unknown length is read ahead in part only (readBody): never stored cut off
			return apierr.New(411, "the upload needs a Content-Length")
		}
		if c.r.ContentLength > settings.MaxBackgroundBytes {
			return apierr.Errf(413, "picture larger than %d MiB", settings.MaxBackgroundBytes>>20)
		}
		data, err = io.ReadAll(io.LimitReader(c.r.Body, settings.MaxBackgroundBytes+1))
		if err != nil || int64(len(data)) != c.r.ContentLength {
			return apierr.New(400, "unreadable body")
		}
		// A file the panel will not take is refused now, with the reason, not after the passkey.
		if _, err := settings.CheckSignIn(data); err != nil {
			return err
		}
		// What is kept for the confirmation is the request "store the picture named ...", and the picture waits.
		name := s.wallStage.put(c.pendingKey(), data, s.now())
		c.body, c.bodyKept = []byte(`{"staged":"`+name+`"}`), true
		err := c.stepUp(u, wallpaperStepUp)
		if kept := s.codec.live.pendingBody(c.pendingKey()); err == nil || kept != string(c.body) {
			s.wallStage.take(name, s.now()) // the sign-in is fresh, or the request was not kept: nothing waits
		}
		if err != nil {
			return err
		}
	}
	p, err := s.Settings.SaveSignIn(data)
	if err != nil {
		var ae *apierr.Error
		if !errors.As(err, &ae) {
			c.audit(u, "settings.signin_wallpaper_upload", "", map[string]any{"bytes": len(data)}, "failed: not saved")
		}
		return err
	}
	c.audit(u, "settings.signin_wallpaper_upload", "", map[string]any{"type": p.Type, "bytes": len(p.Data)}, "ok")
	c.json(201, wallpaperState(p))
	return nil
}

func (s *Server) deleteWallpaper(c *reqCtx) error {
	u, err := c.wallpaperAdmin(true)
	if err != nil {
		return err
	}
	if err := c.stepUp(u, wallpaperStepUp); err != nil {
		return err
	}
	was := s.Settings.SignIn() != nil
	if !s.settingsRL.allow(u.Sub) {
		return c.limited(u, s.settingsRL, "too many settings changes")
	}
	if err := s.Settings.DeleteSignIn(); err != nil {
		c.audit(u, "settings.signin_wallpaper_delete", "", map[string]any{}, "failed: not removed")
		return err
	}
	if was { // removing nothing is not a change
		c.audit(u, "settings.signin_wallpaper_delete", "", map[string]any{}, "ok")
	}
	c.json(200, wallpaperState(nil))
	return nil
}
