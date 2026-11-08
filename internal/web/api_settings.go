package web

import (
	"io"
	"os"

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
	c.audit(u, "settings.update", "", map[string]any{"background": body.Background.Kind, "dim": body.Dim,
		"blur": body.Blur, "card_opacity": body.CardOpacity}, "ok")
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

func (s *Server) getBackground(c *reqCtx) error {
	u, err := c.requireSession()
	if err != nil {
		return err
	}
	p, mime := s.Settings.BackgroundPath(u.Sub)
	if p == "" {
		return apierr.New(404, "no background uploaded")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return apierr.New(404, "no background uploaded")
	}
	c.flush()
	h := c.w.Header()
	h.Set("Content-Type", mime)
	h.Set("Cache-Control", "private, max-age=86400")
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("Content-Disposition", "inline")
	_, _ = c.w.Write(b)
	return nil
}

func (s *Server) deleteBackground(c *reqCtx) error {
	u, err := c.sessionCSRF()
	if err != nil {
		return err
	}
	c.audit(u, "settings.background_delete", "", map[string]any{}, "ok")
	out, err := s.Settings.DeleteBackground(u.Sub)
	if err != nil {
		return err
	}
	c.json(200, out)
	return nil
}
