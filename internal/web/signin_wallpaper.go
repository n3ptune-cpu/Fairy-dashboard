package web

import (
	"bytes"
	"errors"
	"io"
	"net/http"
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
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", cacheNoCache)
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"detail":"no sign-in wallpaper"}`))
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
// a change also needs the CSRF header and the same origin.
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

// uploadWallpaper stores the request's body as the sign-in wallpaper. What the browser calls the file (its
// name, its Content-Type) is not looked at: the content says what it is (settings.SignInType).
func (s *Server) uploadWallpaper(c *reqCtx) error {
	u, err := c.wallpaperAdmin(true)
	if err != nil {
		return err
	}
	if !s.uploadRL.allow(u.Sub) {
		return apierr.New(429, "too many uploads; try again in a minute")
	}
	if c.r.ContentLength < 0 { // a body of unknown length is read ahead in part only (readBody): never stored cut off
		return apierr.New(411, "the upload needs a Content-Length")
	}
	if c.r.ContentLength > settings.MaxBackgroundBytes {
		return apierr.Errf(413, "picture larger than %d MiB", settings.MaxBackgroundBytes>>20)
	}
	data, err := io.ReadAll(io.LimitReader(c.r.Body, settings.MaxBackgroundBytes+1))
	if err != nil || int64(len(data)) != c.r.ContentLength {
		return apierr.New(400, "unreadable body")
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
	if !s.settingsRL.allow(u.Sub) {
		return apierr.New(429, "too many settings changes; slow down")
	}
	was := s.Settings.SignIn() != nil
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
