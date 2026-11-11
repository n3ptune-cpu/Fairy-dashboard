package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// A control action that needs a step-up sign-in is not lost and not asked for twice: when the step-up check
// refuses a browser's POST / PUT / DELETE, the request (already checked for CSRF, with the typed confirmation
// as the user sent it, as the router matched it) is kept on the server, in memory, for that session, and the
// "Confirm it's you" dialog of the page that sent it can have it run once after the sign-in at Pocket ID. The
// client never re-submits it.
//
// Who may make the panel run a kept action (NEW-1): only the client that asked for it.
//
//   - A request is kept only when it comes from the client the session was established for (the address and
//     browser its sign-in or last confirmation came back to, sessionData.Client). The same cookie used from
//     another address or browser is refused like before, but nothing of it is kept.
//   - The kept action records that client. The refused request's answer carries a random id ("pending_id") and
//     the panel's own description of what would run ("action"), which the dialog shows.
//   - "Continue" in the dialog is POST /api/me/step-up with that id in the body (CSRF header, same origin, from
//     that same client). Only this arms the action, for the one sign-in round trip the answer starts. The id
//     is never read from a URL: no link can name a kept action, and GET /auth/login always forgets it.
//   - The callback runs the action only when it was armed for this round trip and the request comes from that
//     same client, after the step-up itself succeeded. It runs through the same handler and every check (admin
//     role, kill switch, fresh sign-in, rate limit, typed confirmation, audit); the outcome is kept for
//     /api/me to show. The audit entries of the step-up name the action.
//
// A kept action can never run later by surprise: it is forgotten when it is used, when a confirmation fails,
// when any other sign-in starts for the session (a link on another site, a second tab), when it is asked for
// with the wrong id or by another client, on Cancel / Escape / closing the dialog (DELETE /api/me/pending),
// after pendingMaxAge, when the session ends, and at a restart (it is only in memory).

// pendingAction is a stored request: the request as the router matched it, not a text to route again. What runs
// after the sign-in is that route's handler with those path parameters and that body, so the action the admin
// confirmed is the action that runs (a path segment that held an encoded "?", "#" or "/" stays one segment).
type pendingAction struct {
	ID      string            // random, single-use: the dialog names the action with it when it asks to confirm
	Method  string            // the request's method
	Pattern string            // the route it was matched to ("DELETE /api/admin/users/{sub}")
	Vals    map[string]string // that route's path parameters, as the router decoded them
	URL     url.URL           // the request's URL as it was parsed once (path, query)
	run     handler           // that route's handler
	Body    string
	Label   string // what the page calls the action (the X-Fairy-Action header of the request)
	Action  string // what the panel itself says will run (describeAction): shown in the dialog and the audit log
	At      time.Time
	Client  client // who asked for it: only this client can arm it, and only this client's confirmation runs it
	Round   string // the state of the step-up round trip it is armed for ("": not armed)
}

// wildcardRe finds the path parameters of a route pattern ("{id}", "{path...}").
var wildcardRe = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)(?:\.\.\.)?\}`)

// pathValues returns the path parameters the router set on r for the route it matched.
func pathValues(r *http.Request) map[string]string {
	vals := map[string]string{}
	for _, m := range wildcardRe.FindAllStringSubmatch(r.Pattern, -1) {
		vals[m[1]] = r.PathValue(m[1])
	}
	return vals
}

func (p *pendingAction) expired(now time.Time) bool { return now.Sub(p.At) > pendingMaxAge }

const (
	pendingMaxAge  = 3 * time.Minute
	pendingMaxBody = 32 << 10         // kept on the server (not in the cookie), so only bounded for memory
	bodySniffMax   = 2048             // a body that is not JSON is read ahead only when it is at most this long
	labelHeader    = "X-Fairy-Action" // optional: what the UI calls the action ("Shut down OFFICE PC")
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

// Why a refused request was not kept (the "not_kept" field of the step_up_required answer).
const (
	notKeptClient = "new_client" // not the client the session was established for: sign in again, then repeat it
	notKeptBody   = "too_large"  // an upload, or a body over pendingMaxBody
)

// keepPending stores this request as the session's pending action (called when step-up refuses it) and returns
// it. nil when the request is not kept; why says so for the two cases the page explains (notKept...).
func (c *reqCtx) keepPending(u *model.User) (p *pendingAction, why string) {
	if c.resumed() || (u.Auth != "session" && u.Auth != "demo") || c.r.Method == http.MethodGet ||
		c.r.Method == http.MethodHead || c.fn == nil || c.r.Pattern == "" || !strings.HasPrefix(c.r.URL.Path, "/api/") {
		return nil, ""
	}
	if !c.bodyKept || len(c.body) > pendingMaxBody {
		return nil, notKeptBody
	}
	key := c.pendingKey()
	if key == "" {
		return nil, ""
	}
	if !c.established() {
		// A copy of the cookie on another address or in another browser, or the owner's own browser after its
		// address changed. Nothing is kept for it, and what the session's own client has waiting is left alone.
		return nil, notKeptClient
	}
	label := actionLabel(c.r.Header.Get(labelHeader))
	if label == "" {
		label = c.r.Method + " " + c.r.URL.Path
	}
	now := c.s.now()
	vals := pathValues(c.r)
	action := describeAction(c.r, vals, c.body)
	if detail := actionDetail[c.r.Pattern]; detail != nil {
		if d := detail(c, u); d != "" {
			action = truncate(safeTarget(action+" ("+d+")"), maxActionText)
		}
	}
	p = &pendingAction{ID: randToken(18), Method: c.r.Method, Pattern: c.r.Pattern, Vals: vals, URL: *c.r.URL,
		run: c.fn, Body: string(c.body), Label: label, Action: action, At: now, Client: c.client()}
	c.s.codec.live.keepPending(key, p, now)
	return p, ""
}

// actionText says, per route, what a kept request does. describeAction fills in the path parameters.
var actionText = map[string]string{
	"POST /api/systems/{id}/power":        "power action on {id}",
	"POST /api/systems/{id}/fan/mode":     "fan mode of {id}",
	"POST /api/systems/{id}/fan/duty":     "fan speed of {id}",
	"POST /api/systems/{id}/helper/stop":  "stop the helper on {id}",
	"POST /api/systems/{id}/helper/start": "start the helper on {id}",
	"PUT /api/systems/{id}/poll":          "polling of {id}",
	"POST /api/admin/hosts":               "Add host",
	"POST /api/admin/hosts/{id}/repair":   "Re-pair {id}",
	"DELETE /api/admin/hosts/{id}":        "remove host {id}",
	"DELETE /api/admin/enrollments/{id}":  "cancel the Add host {id}",
	"POST /api/admin/updates":             "update all agents",
	"POST /api/admin/updates/{id}":        "update the agent on {id}",
	"POST /api/admin/panel-update":        "update the panel",
	"PUT /api/admin/users/{sub}":          "change user {sub}",
	"DELETE /api/admin/users/{sub}":       "remove user {sub}",
	"POST /api/admin/sites":               "create a site",
	"PUT /api/admin/sites/{name}":         "change site {name}",
	"DELETE /api/admin/sites/{name}":      "delete site {name}",
	"PUT /api/admin/assign":               "move a system to a site",
	"POST /api/me/sessions/revoke":        "sign out another session of yours",
	"POST /api/me/sessions/revoke-others": "sign out all your other sessions",
	"POST /api/admin/sessions/revoke":     "sign out a session",
}

// actionDetail is, per route, what the panel adds to the description of a kept request when the request's own
// text does not say which thing it is about: a sign-out names its session by a handle the page cannot read.
var actionDetail = map[string]func(c *reqCtx, u *model.User) string{
	"POST /api/me/sessions/revoke":    (*reqCtx).sessionDetail,
	"POST /api/admin/sessions/revoke": (*reqCtx).sessionDetail,
}

// actionFields are the body fields describeAction adds: the ones that say which action, role or object it is.
// Nothing else of a body is ever shown or logged (a body can carry a typed confirmation, never a secret today,
// but the list keeps it that way).
var actionFields = []string{"action", "mode", "role", "name", "target", "site"}

const maxActionText = 240

// describeAction is the panel's own description of a kept request: what the admin's confirmation will run. It
// is made from the matched route, its path parameters and a few named body fields, never from what the page
// says the action is called (the label), so it is true also when the page is not the panel's own. Control
// characters are percent-encoded (safeTarget); it holds no secret.
func describeAction(r *http.Request, vals map[string]string, body []byte) string {
	text, ok := actionText[r.Pattern]
	if !ok {
		text = r.Method + " " + r.URL.Path
	}
	if ok {
		text = wildcardRe.ReplaceAllStringFunc(text, func(m string) string {
			return truncate(vals[strings.TrimSuffix(strings.Trim(m, "{}"), "...")], 80)
		})
	}
	var fields map[string]any
	if json.Unmarshal(body, &fields) == nil {
		var extra []string
		for _, k := range actionFields {
			if v, ok := fields[k].(string); ok && v != "" {
				extra = append(extra, k+": "+truncate(v, 60))
			}
		}
		if len(extra) > 0 {
			text += " (" + strings.Join(extra, ", ") + ")"
		}
	}
	return truncate(safeTarget(text), maxActionText)
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
// session (callback or demo), and keeps its outcome for the confirming client's next /api/me. p was taken out of the store by the caller
// (takePending), so it runs at most once; nil runs nothing.
func (s *Server) resumePending(c *reqCtx, p *pendingAction) {
	if p == nil || p.run == nil {
		return
	}
	sess := c.session()
	ctx := context.WithValue(context.WithValue(c.r.Context(), resumedKey, true), sessionKey, sess)
	// The request is put together from what the router made of the original one. Nothing is parsed or routed a
	// second time: the kept handler is called directly, with the kept path parameters.
	u := p.URL
	req := (&http.Request{Method: p.Method, URL: &u, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(p.Body)), ContentLength: int64(len(p.Body)), Host: c.r.Host,
		RemoteAddr: c.r.RemoteAddr, Pattern: p.Pattern}).WithContext(ctx)
	for k, v := range p.Vals {
		req.SetPathValue(k, v)
	}
	for _, h := range []string{"X-Forwarded-For", "X-Real-IP", "User-Agent"} {
		if v := c.r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(CSRFHeader, CSRFValue)
	rec := &recorder{h: http.Header{}}
	s.h(p.run)(rec, req)
	res := &model.PendingResult{Label: p.Label, Status: rec.status, OK: rec.status >= 200 && rec.status < 300, Path: p.URL.Path}
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
	// kept on the server for the client that confirmed, until its page asks /api/me (once; see takeResult)
	s.codec.live.putResult(c.pendingKey(), res, c.client(), s.now())
	slog.Info("ran the action that waited for a step-up sign-in", "route", p.Pattern, "status", rec.status)
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
