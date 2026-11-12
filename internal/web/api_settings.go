package web

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/settings"
)

func (s *Server) getSettings(c *reqCtx) error {
	u, err := c.requireSession()
	if err != nil {
		return err
	}
	c.json(200, s.Settings.Get(u.Sub))
	return nil
}

func (s *Server) putSettings(c *reqCtx) error {
	u, err := c.sessionCSRF()
	if err != nil {
		return err
	}
	if !s.settingsRL.allow(u.Sub) {
		return apierr.New(429, "too many settings changes; slow down")
	}
	raw, err := io.ReadAll(io.LimitReader(c.r.Body, 64<<10))
	if err != nil {
		return apierr.New(400, "unreadable body")
	}
	body, err := settings.Decode(raw)
	if err != nil {
		return err
	}
	cur := s.Settings.Get(u.Sub)
	if body.Background.Kind == "upload" {
		if p, _ := s.Settings.BackgroundPath(u.Sub); p == "" {
			return apierr.New(400, "no uploaded background; POST /api/settings/background first")
		}
		body.Background.Version = cur.Background.Version // server-owned
	}
	popups := "off"
	if body.Popups.Enabled {
		popups = fmt.Sprintf("%d s, %s", body.Popups.Seconds, body.Popups.Level)
	}
	c.audit(u, "settings.update", "", map[string]any{"background": body.Background.Kind, "dim": body.Dim,
		"blur": body.Blur, "card_opacity": body.CardOpacity, "popups": popups}, "ok")
	out, err := s.Settings.Put(u.Sub, body)
	if err != nil {
		return err
	}
	c.json(200, out)
	return nil
}

// uploadBackground stores a background image (at most settings.MaxBackgroundBytes). Only users with a role
// may upload (a user waiting for access may change the other settings but not store files), at most
// uploadsPerMin times a minute.
func (s *Server) uploadBackground(c *reqCtx) error {
	u, err := c.sessionCSRF()
	if err != nil {
		return err
	}
	if _, g, _ := c.grant(); g.Pending() {
		return apierr.New(403, pendingDetail)
	}
	if !s.uploadRL.allow(u.Sub) {
		return apierr.New(429, "too many uploads; try again in a minute")
	}
	if c.r.ContentLength > settings.MaxBackgroundBytes {
		return apierr.New(413, "image too large")
	}
	data, err := io.ReadAll(io.LimitReader(c.r.Body, settings.MaxBackgroundBytes+1)) // bounded even without Content-Length
	if err != nil {
		return apierr.New(400, "unreadable body")
	}
	if len(data) > settings.MaxBackgroundBytes {
		return apierr.New(413, "image too large")
	}
	mime, err := settings.ValidateUpload(c.r.Header.Get("Content-Type"), data)
	if err != nil {
		return err
	}
	c.audit(u, "settings.background_upload", "", map[string]any{"type": mime, "bytes": len(data)}, "ok")
	out, err := s.Settings.SaveBackground(u.Sub, mime, data)
	if err != nil {
		return err
	}
	c.json(201, out)
	return nil
}

// The background image is a file of up to 8 MiB that every signed-in user may upload and fetch (T-09). It is
// sent from the open file, never read into memory as a whole. One user has at most maxBackgroundsPerUser
// downloads in flight (a page fetches it once and the browser keeps it for a day), the panel
// maxBackgroundsTotal; one more is answered 429. A download has backgroundWriteTimeout to be taken, instead of
// the general answerWriteTimeout: it is the one large answer, and a phone on a slow link may need longer.
const (
	maxBackgroundsPerUser  = 4
	maxBackgroundsTotal    = 64
	backgroundWriteTimeout = 3 * time.Minute
)

func (s *Server) getBackground(c *reqCtx) error {
	u, err := c.requireSession()
	if err != nil {
		return err
	}
	p, mime := s.Settings.BackgroundPath(u.Sub)
	if p == "" {
		return apierr.New(404, "no background uploaded")
	}
	release, ok := s.backgrounds.acquire(u.Sub)
	if !ok {
		c.w.Header().Set("Retry-After", "2")
		return apierr.New(429, "too many downloads of the background image at the same time; try again in a moment")
	}
	defer release()
	f, err := os.Open(p)
	if err != nil {
		return apierr.New(404, "no background uploaded")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return apierr.New(404, "no background uploaded")
	}
	c.used(200)
	c.flush()
	h := c.w.Header()
	h.Set("Content-Type", mime)
	// no-cache: the browser may keep the picture but asks before every use, and the question needs the session
	// (answered 304 from the file's time). With a max-age the picture stayed readable from the browser's cache for a
	// day after sign-out, for the next person at a shared browser profile.
	h.Set("Cache-Control", "private, no-cache")
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("Content-Disposition", "inline")
	timeout := backgroundWriteTimeout
	if s.answerTimeout < answerWriteTimeout { // a test shortened the deadlines
		timeout = s.answerTimeout
	}
	_ = http.NewResponseController(c.w).SetWriteDeadline(time.Now().Add(timeout))
	http.ServeContent(c.w, c.r, "", st.ModTime(), f) // streams; answers Range and If-Modified-Since
	return nil
}

func (s *Server) deleteBackground(c *reqCtx) error {
	u, err := c.sessionCSRF()
	if err != nil {
		return err
	}
	if !s.settingsRL.allow(u.Sub) { // audited below, and open to every signed-in user: limited like the other settings changes
		return apierr.New(429, "too many settings changes; slow down")
	}
	c.audit(u, "settings.background_delete", "", map[string]any{}, "ok")
	out, err := s.Settings.DeleteBackground(u.Sub)
	if err != nil {
		return err
	}
	c.json(200, out)
	return nil
}
