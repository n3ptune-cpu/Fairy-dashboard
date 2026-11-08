package web

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/n3ptune-cpu/Fairy/internal/apierr"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/security"
)

type reqCtx struct {
	s     *Server
	w     http.ResponseWriter
	r     *http.Request
	sess  *sessionData
	dirty bool
	user  *model.User
}

func (c *reqCtx) session() *sessionData {
	if c.sess == nil {
		c.sess = c.s.codec.decode(c.r)
	}
	return c.sess
}

func (c *reqCtx) clearSession() { c.sess = &sessionData{}; c.dirty = true }

func (c *reqCtx) flush() {
	if c.dirty {
		c.s.codec.write(c.w, c.sess)
		c.dirty = false
	}
}

func (c *reqCtx) ip() string { return security.ClientIP(c.r, c.s.Cfg.Trusted()) }

func (c *reqCtx) json(status int, v any) {
	c.flush()
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("encoding response", "path", c.r.URL.Path, "error", err)
		status, b = 500, []byte(`{"detail":"internal error"}`)
	}
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(status)
	_, _ = c.w.Write(b)
}

func (c *reqCtx) fail(err error) {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		c.json(ae.Status, map[string]any{"detail": ae.Detail})
		return
	}
	// No stack traces or internals to clients; details go to the server log only.
	slog.Error("unhandled error", "method", c.r.Method, "path", c.r.URL.Path, "error", err)
	c.json(500, map[string]any{"detail": "internal error"})
}

func (c *reqCtx) redirect(target string) {
	c.flush()
	http.Redirect(c.w, c.r, target, http.StatusSeeOther)
}

type handler func(c *reqCtx) error

func (s *Server) h(fn handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := &reqCtx{s: s, w: w, r: r}
		if err := fn(c); err != nil {
			c.fail(err)
			return
		}
		c.flush()
	}
}
