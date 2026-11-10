package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// A control action that needs a step-up sign-in is not lost and not asked for twice: when the step-up check
// refuses a browser's POST / PUT / DELETE, the request (method, path, body: already checked for CSRF, with the
// typed confirmation as the user sent it) is kept in the signed session cookie. Right after the sign-in at
// Pocket ID succeeds for the same session, the panel runs it once, server-side, through the same handler and
// every check (admin role, kill switch, fresh sign-in, rate limit, typed confirmation, audit), and keeps the
// outcome for /api/me to show. The client never re-submits it; a pending action older than pendingMaxAge, or
// from another session, is dropped.

// pendingAction is a stored request.
type pendingAction struct {
	Method string `json:"m"`
	Path   string `json:"p"`
	Body   string `json:"b,omitempty"`
	Label  string `json:"l,omitempty"`
	At     int64  `json:"t"`
	SID    string `json:"s,omitempty"`
}

const (
	pendingMaxAge  = 10 * time.Minute
	pendingMaxBody = 2048
	labelHeader    = "X-Fairy-Action" // optional: what the UI calls the action ("Shut down PROJECT REBUILD")
)

type ctxKey int

const (
	resumedKey ctxKey = iota // the request is the panel's own replay of a stored pending action
	sessionKey               // the session to use instead of the cookie (the replay's, fresh from the sign-in)
)

func (c *reqCtx) resumed() bool { return c.r.Context().Value(resumedKey) != nil }

// keepPending stores this request as the session's pending action (called when step-up refuses it).
func (c *reqCtx) keepPending(u *model.User) bool {
	if c.resumed() || (u.Auth != "session" && u.Auth != "demo") || c.r.Method == http.MethodGet || len(c.body) > pendingMaxBody {
		return false
	}
	label := strings.TrimSpace(c.r.Header.Get(labelHeader))
	if len(label) > 120 || strings.ContainsAny(label, "\r\n") {
		label = ""
	}
	if label == "" {
		label = c.r.Method + " " + c.r.URL.Path
	}
	s := c.session()
	s.Pending = &pendingAction{Method: c.r.Method, Path: c.r.URL.Path, Body: string(c.body), Label: label,
		At: c.s.now().Unix(), SID: s.SID}
	c.dirty = true
	return true
}

// recorder is a minimal ResponseWriter for the replay.
type recorder struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.h }
func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}
func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	if r.body.Len() < 64<<10 {
		r.body.Write(b)
	}
	return len(b), nil
}

// resumePending runs the session's pending action after a successful step-up sign-in (callback or demo) and
// stores its outcome in the session. The caller has just confirmed the sign-in for this session.
func (s *Server) resumePending(c *reqCtx) {
	sess := c.session()
	p := sess.Pending
	if p == nil {
		return
	}
	sess.Pending = nil
	c.dirty = true
	if s.now().Sub(time.Unix(p.At, 0)) > pendingMaxAge || p.SID != sess.SID || !strings.HasPrefix(p.Path, "/api/") {
		return
	}
	ctx := context.WithValue(context.WithValue(c.r.Context(), resumedKey, true), sessionKey, sess)
	req, err := http.NewRequestWithContext(ctx, p.Method, p.Path, strings.NewReader(p.Body))
	if err != nil {
		return
	}
	req.RemoteAddr = c.r.RemoteAddr
	for _, h := range []string{"X-Forwarded-For", "X-Real-IP", "User-Agent"} {
		if v := c.r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(CSRFHeader, CSRFValue)
	rec := &recorder{h: http.Header{}}
	s.mux.ServeHTTP(rec, req)
	res := &model.PendingResult{Label: p.Label, Status: rec.status, OK: rec.status >= 200 && rec.status < 300, Path: p.Path}
	var body map[string]any
	_ = json.Unmarshal(rec.body.Bytes(), &body)
	switch d := body["detail"].(type) {
	case string:
		res.Message = d
	case map[string]any:
		res.Message, _ = d["message"].(string)
	}
	if res.Message == "" {
		res.Message, _ = body["message"].(string)
	}
	if id, ok := body["id"].(string); ok && res.OK && len(id) <= 64 {
		res.Ref = id // e.g. the Add host the action made
	}
	if res.Message == "" && res.OK {
		res.Message = "done"
	}
	if len(res.Message) > 300 {
		res.Message = res.Message[:300]
	}
	sess.PendingResult = res
	slog.Info("ran the action that waited for a step-up sign-in", "path", p.Path, "status", rec.status)
}

// readBody keeps a write request's body (at most 64 KiB) so a refused step-up can store it.
func (c *reqCtx) readBody() {
	if c.r.Method == http.MethodGet || c.r.Method == http.MethodHead || c.r.Body == nil {
		return
	}
	if strings.HasPrefix(c.r.Header.Get("Content-Type"), "application/json") || c.r.ContentLength <= pendingMaxBody {
		b, _ := io.ReadAll(io.LimitReader(c.r.Body, 64<<10))
		c.body = b
		c.r.Body = io.NopCloser(bytes.NewReader(b))
	}
}

// dropPending forgets the session's pending action (Cancel in the "Confirm it's you" dialog), so a later
// sign-in runs nothing.
func (s *Server) dropPending(c *reqCtx) error {
	u, err := c.requireUser()
	if err != nil {
		return err
	}
	if err := c.checkCSRF(u); err != nil {
		return err
	}
	if sess := c.session(); sess.Pending != nil {
		sess.Pending = nil
		c.dirty = true
	}
	c.json(200, map[string]any{"ok": true})
	return nil
}
