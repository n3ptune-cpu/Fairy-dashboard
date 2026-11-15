package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
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
	// Items: for an action about a list (the machines of an "Update unpowered hosts" run), the panel's
	// own line for each entry; for the accept of a machine id, what to compare first. Action is cut at
	// maxActionText; the dialog shows these in full. They are at most maxActionItems lines, each cut at
	// maxActionText, and one more line that says how many lines are not listed when there are more
	// (dialogItems). lines: every line in full, as the route made them: what a stamp of the lines is made from
	// (shownStampOf), so that a line the dialog does not list, or the part of a line it cuts, is compared too.
	Items  []string
	lines  []string
	At     time.Time
	Client client // who asked for it: only this client can arm it, and only this client's confirmation runs it
	Round  string // the state of the step-up round trip it is armed for ("": not armed)
	// Approval: a machine this request goes to checks an approval itself (approval.go). The request then runs
	// only after the round trip with the client "Fairy PC actions" that was made for exactly this plan, never
	// after an ordinary "Confirm it's you". nil: an ordinary kept action.
	Approval *approvalPlan
	Next     string // where the page returns to after the approval's round trip (set when it starts)
	// Stamp: for a route that has one (actionStamp), what the request rested on when it was kept and must
	// still rest on when it runs: the run timings in force for a start of "Update unpowered hosts"; for a
	// change of the run timings, the lines "Confirm it's you" shows for it (Items). The handler compares it in
	// the replay (keptStamp). Never shown, never sent to a browser.
	Stamp string
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
	resumedKey  ctxKey = iota // the request is the panel's own replay of a stored pending action
	sessionKey                // the session to use instead of the cookie (the replay's, fresh from the sign-in)
	approvalKey               // the verified approval the replay runs with (*approvalGrant, approval.go)
	stampKey                  // the kept action's Stamp, for a route that has one (keptStamp)
)

func (c *reqCtx) resumed() bool { return c.r.Context().Value(resumedKey) != nil }

// keptStamp is the stamp the kept action of this replay was given when it was kept (ok false: the request is
// no replay, or its route has no stamp).
func (c *reqCtx) keptStamp() (stamp string, ok bool) {
	stamp, ok = c.r.Context().Value(stampKey).(string)
	return stamp, ok
}

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
func (c *reqCtx) keepPending(u *model.User, plan *approvalPlan) (p *pendingAction, why string) {
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
		run: c.fn, Body: string(c.body), Label: label, Action: action, At: now, Client: c.client(), Approval: plan}
	if plan != nil && len(plan.items) > 0 { // the plan's own lines: what each machine would be told to trust
		for _, line := range plan.items {
			if len(p.Items) == maxPlanItems {
				break
			}
			p.Items = append(p.Items, truncate(safeTarget(line), maxActionText))
		}
	} else if list := actionList[c.r.Pattern]; list != nil {
		p.lines = shownItems(list(c, u))
		p.Items = dialogItems(p.lines)
	}
	if stamp := actionStamp[c.r.Pattern]; stamp != nil {
		p.Stamp = stamp(c, p)
	}
	c.s.codec.live.keepPending(key, p, now)
	return p, ""
}

// shownItems is every line of a kept action, in full: what the action is about, whatever the dialog lists of
// it. A stamp of the lines is made from these (shownStampOf), never from what the dialog shows.
func shownItems(lines []string) []string { return slices.Clone(lines) }

// dialogItems is the lines "Confirm it's you" lists: at most maxActionItems, each cut and cleaned like
// every text of that dialog. When there are more, one more line says how many are not listed: the dialog
// never looks complete when it is not.
func dialogItems(lines []string) []string {
	var out []string
	for i, line := range lines {
		if i == maxActionItems {
			rest := len(lines) - i
			if rest == 1 {
				out = append(out, "and 1 more line that is not listed here")
			} else {
				out = append(out, fmt.Sprintf("and %d more lines that are not listed here", rest))
			}
			break
		}
		out = append(out, truncate(safeTarget(line), maxActionText))
	}
	return out
}

// shownStampOf is the stamp of a kept action that rests on the lines of its confirmation ("what is
// confirmed is what is saved": Run timings, Edit host). Its first line is a hash of ALL lines in full, each
// with its length, so two lists that differ anywhere (in line 40 of 40, or after the 240 characters the
// dialog shows of a line) have different stamps. After it come the lines as the dialog lists them, for the
// audit entry of a refusal (confirmedLines). The handler makes the stamp again in the replay, from the lines
// the same request makes then, and compares the whole text.
func shownStampOf(lines []string) string {
	h := sha256.New()
	for _, l := range lines {
		fmt.Fprintf(h, "%d:%s\n", len(l), l)
	}
	return hex.EncodeToString(h.Sum(nil)) + "\n" + strings.Join(dialogItems(lines), "\n")
}

// confirmedLines is the lines of the confirmation a stamp of shownStampOf was made for, as the dialog listed
// them (none: an empty list).
func confirmedLines(stamp string) []string {
	_, listed, _ := strings.Cut(stamp, "\n")
	if listed == "" {
		return []string{}
	}
	return strings.Split(listed, "\n")
}

// actionStamp is, per route, what a kept request rests on besides its own text: a value the handler works
// out again in the replay and compares (pendingAction.Stamp). p is the action as it is kept (its Items are
// made). A start of "Update unpowered hosts" rests on the run timings in force: a run must not start with
// other timings than the ones its dialog and its confirmation showed (runtimes.go). A change of the run
// timings rests on the lines its confirmation shows: what is confirmed is what is saved (api_runtimings.go).
// A change of "Edit host" does too (api_hostoptions.go).
var actionStamp = map[string]func(c *reqCtx, p *pendingAction) string{
	"POST /api/admin/wake-update": func(c *reqCtx, _ *pendingAction) string { return c.wakeUpdateStamp() },
}

// actionText says, per route, what a kept request does. describeAction fills in the path parameters.
var actionText = map[string]string{
	"POST /api/systems/{id}/power":               "power action on {id}",
	"POST /api/systems/{id}/fan/mode":            "fan mode of {id}",
	"POST /api/systems/{id}/fan/duty":            "fan speed of {id}",
	"POST /api/systems/{id}/helper/stop":         "stop the helper on {id}",
	"POST /api/systems/{id}/helper/start":        "start the helper on {id}",
	"PUT /api/systems/{id}/poll":                 "polling of {id}",
	"POST /api/admin/hosts":                      "Add host",
	"POST /api/admin/hosts/{id}/repair":          "Re-pair {id}",
	"DELETE /api/admin/hosts/{id}":               "remove host {id}",
	"DELETE /api/admin/enrollments/{id}":         "cancel the Add host {id}",
	"POST /api/admin/updates":                    "update all agents",
	"POST /api/admin/updates/{id}":               "update the agent on {id}",
	"POST /api/admin/panel-update":               "update the panel",
	"PUT /api/admin/users/{sub}":                 "change user {sub}",
	"DELETE /api/admin/users/{sub}":              "remove user {sub}",
	"POST /api/admin/sites":                      "create a site",
	"PUT /api/admin/sites/{name}":                "change site {name}",
	"DELETE /api/admin/sites/{name}":             "delete site {name}",
	"PUT /api/admin/assign":                      "move a system to a site",
	"POST /api/me/sessions/revoke":               "sign out another session of yours",
	"POST /api/me/sessions/revoke-others":        "sign out all your other sessions",
	"POST /api/admin/sessions/revoke":            "sign out a session",
	"POST /api/admin/signin-wallpaper":           "change the sign-in picture",
	"DELETE /api/admin/signin-wallpaper":         "remove the sign-in picture",
	"POST /api/admin/wake-update":                "Update unpowered hosts",
	"POST /api/admin/approval/turn-on":           "switch on the check of approvals on the machine itself",
	"POST /api/admin/approval/accept/{id}":       "accept what {id} shows about its own check of approvals now",
	"DELETE /api/admin/wake-update/{id}":         "cancel the run of \"Update unpowered hosts\"",
	"PUT /api/systems/{id}/wake-update":          "\"Update unpowered hosts\" for {id}",
	"POST /api/admin/wake-update/{id}/confirm":   "confirm the update and the shutdown of the machines the run woke",
	"POST /api/systems/{id}/wake-update/keep-on": "keep {id} on (call off its switch-off after a run)",
	"POST /api/power/socket":                     "switch a plug socket",
}

// actionDetail is, per route, what the panel adds to the description of a kept request when the request's own
// text does not say which thing it is about: a sign-out names its session by a handle the page cannot read.
var actionDetail = map[string]func(c *reqCtx, u *model.User) string{
	"POST /api/me/sessions/revoke":    (*reqCtx).sessionDetail,
	"POST /api/admin/sessions/revoke": (*reqCtx).sessionDetail,
	// which picture: its kind and size, as the panel found them (signin_wallpaper.go)
	"POST /api/admin/signin-wallpaper": (*reqCtx).wallpaperDetail,
	// which machines, by the panel's names for them, and the value that names the list (api_wakeupdate.go)
	"POST /api/admin/wake-update":        (*reqCtx).wakeUpdateDetail,
	"DELETE /api/admin/wake-update/{id}": (*reqCtx).wakeUpdateCancelDetail,
	"PUT /api/systems/{id}/wake-update":  (*reqCtx).wakeUpdateSwitchDetail,
	// which id, or that the check is off (api_machines.go)
	"POST /api/admin/approval/accept/{id}": (*reqCtx).approvalAcceptDetail,
	// which socket, in which direction, and what hangs on it (api_plug.go)
	"POST /api/power/socket": (*reqCtx).plugDetail,
}

// actionList is, per route, the panel's own line for each thing a kept request is about, when it is about a
// list: the dialog shows every line, however long the list is (the one-line description is cut). The accept of
// a machine id has one line: what to compare first (api_machines.go).
var actionList = map[string]func(c *reqCtx, u *model.User) []string{
	"POST /api/admin/wake-update":          (*reqCtx).wakeUpdateItems,
	"POST /api/admin/approval/accept/{id}": (*reqCtx).approvalAcceptItems,
}

// maxActionItems is more than a run has machines (wakerun.MaxHosts). maxPlanItems is more than a Turn on has
// lines (MaxTurnOn machines, the keys and the two lines around them).
const (
	maxActionItems = 32
	maxPlanItems   = 80
	maxResultItems = 128 // the lines of a result (one per machine of "Update all")
)

// decodeStrictly decodes a kept request's body the way its handler will: one reading only (strictBody), no
// unknown fields.
func decodeStrictly(b []byte, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return strictBody(b) && dec.Decode(v) == nil
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
// (takePending), so it runs at most once; nil runs nothing. It returns the status the action answered with (0:
// nothing ran).
//
// A kept action that needs a machine's approval never runs here: an ordinary confirmation is not what it was
// kept for (resumePendingWith runs it, after the approval's own round trip).
func (s *Server) resumePending(c *reqCtx, p *pendingAction) int {
	if p != nil && p.Approval != nil {
		slog.Warn("a kept action that needs a machine's approval was not run: the confirmation that came back is not its approval", "route", p.Pattern)
		return 0
	}
	return s.resumePendingWith(c, p, nil)
}

// resumePendingWith is resumePending with the verified approval the replay carries (nil: none).
func (s *Server) resumePendingWith(c *reqCtx, p *pendingAction, grant *approvalGrant) int {
	if p == nil || p.run == nil {
		return 0
	}
	sess := c.session()
	// Not cancelled with the callback's request: a browser that reloads the callback, or a connection that
	// breaks, must not stop the request to the machine half-way (it may have taken it, and the audit log would
	// then hold an error for an action that happened; r3-p1-01). The action's own time limits still apply.
	ctx := context.WithValue(context.WithValue(context.WithoutCancel(c.r.Context()), resumedKey, true), sessionKey, sess)
	if grant != nil {
		ctx = context.WithValue(ctx, approvalKey, grant)
	}
	if actionStamp[p.Pattern] != nil {
		ctx = context.WithValue(ctx, stampKey, p.Stamp)
	}
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
	// An action about several machines says what happened on each (Update all: "lines"), and whether all of it
	// worked ("all_ok"). "done" is said only for an action that says nothing itself.
	if lines, ok := body["lines"].([]any); ok {
		for _, l := range lines {
			if s, ok := l.(string); ok && len(res.Items) < maxResultItems {
				res.Items = append(res.Items, truncate(safeTarget(s), maxActionText))
			}
		}
	}
	if all, ok := body["all_ok"].(bool); ok && !all {
		res.OK = false
	}
	// A switch of a plug's socket that the machine could not confirm is not a plain "done" (api_plug.go).
	if confirmed, ok := body["confirmed"].(bool); ok && !confirmed && res.OK {
		res.Warn = true
	}
	if res.Message == "" && res.OK {
		res.Message = "done"
	}
	res.Message = cutMessage(res.Message)
	// kept on the server for the client that confirmed, until its page asks /api/me (once; see takeResult)
	s.codec.live.putResult(c.pendingKey(), res, c.client(), s.now())
	slog.Info("ran the action that waited for a step-up sign-in", "route", p.Pattern, "status", rec.status)
	return rec.status
}

// readBody keeps a write request's body (at most 64 KiB) so a refused step-up can store it.
func (c *reqCtx) readBody() {
	if c.r.Method == http.MethodGet || c.r.Method == http.MethodHead || c.r.Body == nil || c.r.Body == http.NoBody {
		c.bodyKept = true // nothing to keep
		return
	}
	// A body that is not JSON is read ahead only when it says that it is short. One of unknown length (no
	// Content-Length: a chunked upload from a script or a proxy) is left for its handler, which reads it up to
	// its own limit; it used to be read here to 64 KiB and the rest dropped, and the upload was then refused
	// as "not a complete picture" (r3-p1-08: -1 is not "at most 2048").
	if strings.HasPrefix(c.r.Header.Get("Content-Type"), "application/json") || (c.r.ContentLength >= 0 && c.r.ContentLength <= bodySniffMax) {
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

// maxResultMessage bounds the sentence of a kept action's result. It is long enough for the longest sentence the
// panel makes (an Add host whose release check failed names the release, the address and the network error:
// about 400 bytes); it used to be 300, which cut that one in the middle of a word.
const maxResultMessage = 1000

// cutMessage keeps a result's sentence whole up to maxResultMessage bytes. A longer one ends at the last
// space before the limit (never inside a character) with "…".
func cutMessage(m string) string {
	if len(m) <= maxResultMessage {
		return m
	}
	cut := maxResultMessage
	for cut > 0 && !utf8.RuneStart(m[cut]) {
		cut--
	}
	if i := strings.LastIndexByte(m[:cut], ' '); i > cut-80 {
		cut = i
	}
	return m[:cut] + "…"
}
