package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/security"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/tokens"
)

type reqCtx struct {
	s     *Server
	w     http.ResponseWriter
	r     *http.Request
	sess  *sessionData
	dirty bool
	user  *model.User
	token *tokens.Record // the API token of this request, if any
	g     *access.Grant  // what the user may see and do (see grant)
	body  []byte         // a write request's body as received (see readBody)
	// bodyKept: body is the whole request body (false for a body readBody left alone: an upload)
	bodyKept bool
	// The API token of this request is checked once (tokenUser): the answer, kept for the handler.
	tokDone bool
	tokUser *model.User
	tokErr  error
}

func (c *reqCtx) session() *sessionData {
	if c.sess == nil {
		if s, ok := c.r.Context().Value(sessionKey).(*sessionData); ok {
			c.sess = s // the replay of a pending action uses the session that just signed in again
		} else {
			c.sess = c.s.codec.decode(c.r)
		}
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

// errorPage answers a browser navigation (e.g. the OIDC callback) with a small HTML page instead of JSON: what
// went wrong and a link back. Everything is escaped; the page has no script or inline style (strict CSP).
func (c *reqCtx) errorPage(status int, title, msg, back string) error {
	c.flush()
	c.w.Header().Set("Content-Type", "text/html; charset=utf-8")
	c.w.WriteHeader(status)
	e := html.EscapeString
	_, _ = fmt.Fprintf(c.w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>%s · Fairy</title>`+
		`<meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="%s">`+
		`</head><body><main class="error-page"><h1>%s</h1><p>%s</p><p><a class="btn" href="%s">Back to Fairy</a></p>`+
		`</main></body></html>`, e(title), e(c.s.staticURL("app.css")), e(title), e(msg), e(safeNext(back)))
	return nil
}

func (c *reqCtx) redirect(target string) {
	c.flush()
	http.Redirect(c.w, c.r, target, http.StatusSeeOther)
}

type handler func(c *reqCtx) error

// mayReadBody decides whether the request body may be read ahead (readBody keeps it for a refused step-up).
// A write to the API from someone who is not signed in (no valid session, no valid API token) is answered
// without reading or parsing any of its body: nobody without a sign-in gets the panel to take in 64 KiB.
func (c *reqCtx) mayReadBody() bool {
	if c.r.Method == http.MethodGet || c.r.Method == http.MethodHead || !strings.HasPrefix(c.r.URL.Path, "/api/") {
		return true
	}
	_, err := c.requireUser()
	return err == nil
}

func (s *Server) h(fn handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := &reqCtx{s: s, w: w, r: r}
		if c.mayReadBody() {
			c.readBody()
		} else if r.Header.Get("Authorization") == "" {
			c.fail(apierr.New(401, "not authenticated")) // what every handler would answer, before any of the body is read
			return
		} // else: a rejected API token. The handler answers (401, or 403 where tokens are not accepted), without the body.
		if err := fn(c); err != nil {
			c.fail(err)
			return
		}
		c.flush()
	}
}
