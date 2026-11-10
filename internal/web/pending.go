package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// A control action that needs a step-up sign-in is not lost and not asked for twice: when the step-up check
// refuses a browser's POST / PUT / DELETE, the request (method, path, body: already checked for CSRF, with the
// typed confirmation as the user sent it) is kept on the server, for that session only, under a random id that
// only the refused request's answer carries ("pending_id"). The "Confirm it's you" link names that id
// (/auth/login?step_up=1&pending=<id>), the login round trip remembers it, and right after the sign-in at
// Pocket ID succeeds for the same session the panel runs that one action once, server-side, through the same
// handler and every check (admin role, kill switch, fresh sign-in, rate limit, typed confirmation, audit), and
// keeps the outcome for /api/me to show. The client never re-submits it.
//
// A kept action can never run later by surprise: it is forgotten when it is used, when a confirmation fails,
// when a step-up starts that does not name it (a link on another site), on Cancel / Escape / closing the
// dialog (DELETE /api/me/pending), after pendingMaxAge, when the session ends, and at a restart (it is only in
// memory, never in the cookie, so a copy of the cookie cannot run it either).

// pendingAction is a stored request.
type pendingAction struct {
	ID     string // random, single-use: names this action in the step-up link
	Method string
	Path   string
	Body   string
	Label  string
	At     time.Time
}

func (p *pendingAction) expired(now time.Time) bool { return now.Sub(p.At) > pendingMaxAge }

const (
	pendingMaxAge  = 3 * time.Minute
	pendingMaxBody = 32 << 10         // kept on the server (not in the cookie), so only bounded for memory
	bodySniffMax   = 2048             // a body that is not JSON is read ahead only when it is at most this long
	labelHeader    = "X-Fairy-Action" // optional: what the UI calls the action ("Shut down PROJECT REBUILD")
)

type ctxKey int

const (
	resumedKey ctxKey = iota // the request is the panel's own replay of a stored pending action
	sessionKey               // the session to use instead of the cookie (the replay's, fresh from the sign-in)
)

func (c *reqCtx) resumed() bool { return c.r.Context().Value(resumedKey) != nil }

// pendingKey is where this request's session keeps its pending action: the session id (demo mode has no
// sessions: one key).
func (c *reqCtx) pendingKey() string {
	if c.s.DemoUser != nil {
		return demoKey
	}
	return c.session().SID
}

// keepPending stores this request as the session's pending action (called when step-up refuses it) and returns
// its id ("" when the request cannot be kept).
func (c *reqCtx) keepPending(u *model.User) string {
	if c.resumed() || (u.Auth != "session" && u.Auth != "demo") || c.r.Method == http.MethodGet ||
		c.r.Method == http.MethodHead || !c.bodyKept || len(c.body) > pendingMaxBody {
		return ""
	}
	key := c.pendingKey()
	if key == "" {
		return ""
	}
	label := actionLabel(c.r.Header.Get(labelHeader))
	if label == "" {
		label = c.r.Method + " " + c.r.URL.Path
	}
	now := c.s.now()
	p := &pendingAction{ID: randToken(18), Method: c.r.Method, Path: c.r.URL.Path, Body: string(c.body), Label: label, At: now}
	c.s.codec.live.keepPending(key, p, now)
	return p.ID
}

// actionLabel reads the X-Fairy-Action header: what the UI calls the action, shown in "Confirm it's you" and in
// the result. The UI sends it percent-encoded (encodeURIComponent), because a browser refuses to put a character
// outside Latin-1 (a Cyrillic or Chinese host name, an emoji) into a header at all; a plain value, as an older
// cached page sends it, is still taken. After decoding it must be valid text of at most 120 characters with no
// line break or other control character, else "" (the caller then uses method and path).
func actionLabel(h string) string {
	h = strings.TrimSpace(h)
	if d, err := url.PathUnescape(h); err == nil {
		h = strings.TrimSpace(d)
	}
	if h == "" || len(h) > 480 || !utf8.ValidString(h) || utf8.RuneCountInString(h) > 120 {
		return ""
	}
	for _, r := range h {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' || r == utf8.RuneError {
			return ""
		}
	}
	return h
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

// resumePending runs p, the action that waited for the step-up sign-in the caller has just confirmed for this
// session (callback or demo), and stores its outcome in the session. p was taken out of the store by the caller
// (takePending), so it runs at most once; nil runs nothing.
func (s *Server) resumePending(c *reqCtx, p *pendingAction) {
	if p == nil || !strings.HasPrefix(p.Path, "/api/") {
		return
	}
	sess := c.session()
	c.dirty = true
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
	if c.r.Method == http.MethodGet || c.r.Method == http.MethodHead || c.r.Body == nil || c.r.Body == http.NoBody {
		c.bodyKept = true // nothing to keep
		return
	}
	if strings.HasPrefix(c.r.Header.Get("Content-Type"), "application/json") || c.r.ContentLength <= bodySniffMax {
		b, _ := io.ReadAll(io.LimitReader(c.r.Body, 64<<10))
		c.body, c.bodyKept = b, true
		c.r.Body = io.NopCloser(bytes.NewReader(b))
	}
}

// dropPending forgets the session's pending action (Cancel, Escape or closing the "Confirm it's you" dialog),
// so a later sign-in runs nothing.
func (s *Server) dropPending(c *reqCtx) error {
	u, err := c.requireUser()
	if err != nil {
		return err
	}
	if err := c.checkCSRF(u); err != nil {
		return err
	}
	if u.Auth != "token" {
		s.codec.live.dropPending(c.pendingKey())
	}
	c.json(200, map[string]any{"ok": true})
	return nil
}
