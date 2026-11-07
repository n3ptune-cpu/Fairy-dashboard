package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nsxservices/fairy/internal/apierr"
	"github.com/nsxservices/fairy/internal/audit"
	"github.com/nsxservices/fairy/internal/config"
	"github.com/nsxservices/fairy/internal/model"
	"github.com/nsxservices/fairy/internal/normalise"
	"github.com/nsxservices/fairy/internal/powerctl"
	"github.com/nsxservices/fairy/internal/scheduler"
	"github.com/nsxservices/fairy/internal/settings"
)

func (s *Server) apiRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/me", s.h(s.me))
	m.HandleFunc("GET /api/audit", s.h(s.auditPage))
	m.HandleFunc("GET /api/systems", s.h(s.systems))
	m.HandleFunc("GET /api/systems/{id}", s.h(s.system))
	m.HandleFunc("GET /api/systems/{id}/processes", s.h(s.processes))
	m.HandleFunc("GET /api/summary", s.h(s.summary))
	m.HandleFunc("GET /api/power", s.h(s.power))
	m.HandleFunc("GET /api/power/history", s.h(s.powerHistory))
	m.HandleFunc("GET /api/ups", s.h(s.ups))
	m.HandleFunc("GET /api/ups/events", s.h(s.upsEvents))
	m.HandleFunc("GET /api/systems/{id}/fan", s.h(s.fanStatus))
	m.HandleFunc("POST /api/systems/{id}/fan/mode", s.h(s.fanMode))
	m.HandleFunc("POST /api/systems/{id}/fan/duty", s.h(s.fanDuty))
	m.HandleFunc("GET /api/settings", s.h(s.getSettings))
	m.HandleFunc("PUT /api/settings", s.h(s.putSettings))
	m.HandleFunc("POST /api/settings/background", s.h(s.uploadBackground))
	m.HandleFunc("GET /api/settings/background", s.h(s.getBackground))
	m.HandleFunc("DELETE /api/settings/background", s.h(s.deleteBackground))
	m.HandleFunc("GET /api/systems/{id}/power", s.h(s.powerCaps))
	m.HandleFunc("POST /api/systems/{id}/power", s.h(s.powerAction))
	m.HandleFunc("GET /api/systems/{id}/poll", s.h(s.getPoll))
	m.HandleFunc("PUT /api/systems/{id}/poll", s.h(s.putPoll))
	m.HandleFunc("GET /api/stream", s.h(s.stream))
}

// ---------------------------------------------------------------- guards
func (c *reqCtx) deny(u *model.User, status int, detail any, why string) error {
	c.audit(u, "auth.denied", c.r.URL.Path, map[string]any{"method": c.r.Method}, fmt.Sprintf("%d: %s", status, why))
	return herr(status, detail)
}

// checkCSRF: browsers need the custom header (forces a CORS preflight; no CORS is enabled) + Origin check.
// API tokens are not sent automatically by browsers, so they need neither.
func (c *reqCtx) checkCSRF(u *model.User) error {
	if u.Auth == "token" {
		return nil
	}
	if c.r.Header.Get(CSRFHeader) != CSRFValue {
		return c.deny(u, 403, "missing "+CSRFHeader+": "+CSRFValue+" header", "missing CSRF header")
	}
	if o := c.r.Header.Get("Origin"); o != "" {
		ou, err1 := url.Parse(o)
		bu, err2 := url.Parse(c.s.Cfg.BaseURL())
		if err1 != nil || err2 != nil || ou.Scheme != bu.Scheme || ou.Host != bu.Host {
			return c.deny(u, 403, "cross-origin request refused", "cross-origin ("+o+")")
		}
	}
	if sfs := c.r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" {
		return c.deny(u, 403, "cross-site request refused", "cross-site")
	}
	return nil
}

// controlGuard: CSRF, control group / control-scope token, admin kill switch, step-up, rate limit.
func (c *reqCtx) controlGuard(bucket *rateLimiter) (*model.User, error) {
	u, err := c.requireUser()
	if err != nil {
		return nil, err
	}
	if err := c.checkCSRF(u); err != nil {
		return nil, err
	}
	cfg := c.s.Cfg
	if !canControl(u, cfg.OIDC) {
		d := "not in a control group"
		if u.Auth == "token" {
			d = "this API token has read scope"
		}
		return nil, c.deny(u, 403, d, "not allowed to control")
	}
	if off := c.s.Poller.Admin.ControlDisabled(); off != nil {
		reason := off.Reason
		if reason == "" {
			reason = "no reason given"
		}
		return nil, c.deny(u, 423, fmt.Sprintf("control is disabled by an administrator (%s): %s", off.By, reason),
			"control disabled (kill switch)")
	}
	if age := c.authAge(u); age != nil && *age > float64(cfg.StepUpWindowS) {
		c.audit(u, "auth.step_up", c.r.URL.Path, map[string]any{"auth_age_s": math.Round(*age)}, "required")
		return nil, herr(401, map[string]any{"code": "step_up_required", "window_s": cfg.StepUpWindowS,
			"message": fmt.Sprintf("Confirm it's you: control actions need a Pocket ID sign-in from the last %d min.", cfg.StepUpWindowS/60)})
	}
	if !bucket.allow(u.Sub) {
		return nil, herr(429, "too many control requests; slow down")
	}
	return u, nil
}

func (c *reqCtx) sessionCSRF() (*model.User, error) {
	u, err := c.requireSession()
	if err != nil {
		return nil, err
	}
	return u, c.checkCSRF(u)
}

func (c *reqCtx) find(id string) (model.System, error) {
	for _, s := range c.s.Poller.Snapshot() {
		if s.ID == id {
			return s, nil
		}
	}
	return model.System{}, herr(404, "system not found")
}

// decodeBody: strict JSON (unknown fields rejected), at most 64 KiB.
func (c *reqCtx) decodeBody(v any) error {
	b, err := io.ReadAll(io.LimitReader(c.r.Body, 64<<10))
	if err != nil {
		return herr(400, "unreadable body")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return herr(422, "invalid request body")
	}
	return nil
}

func queryInt(q url.Values, k string, def, lo, hi int) (int, error) {
	v := q.Get(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, herr(422, fmt.Sprintf("%s must be an integer between %d and %d", k, lo, hi))
	}
	return n, nil
}

// ---------------------------------------------------------------- read endpoints
func (s *Server) me(c *reqCtx) error {
	u, err := c.requireUser()
	if err != nil {
		return err
	}
	c.json(200, model.Me{User: *u, CanControl: canControl(u, s.Cfg.OIDC), Demo: s.DemoUser != nil,
		ControlDisabled: s.Poller.Admin.ControlDisabled(), StepUpWindowS: s.Cfg.StepUpWindowS, AuthAgeS: c.authAge(u)})
	return nil
}

var actionRe = regexp.MustCompile(`^[a-z_.]+$`)

func (s *Server) auditPage(c *reqCtx) error {
	u, err := c.requireUser()
	if err != nil {
		return err
	}
	if !canControl(u, s.Cfg.OIDC) {
		return c.deny(u, 403, "the audit log is visible to the control group only", "audit read denied")
	}
	q := c.r.URL.Query()
	user, action, host := q.Get("q"), q.Get("action"), q.Get("host")
	if len(user) > 100 || len(action) > 40 || (action != "" && !actionRe.MatchString(action)) || len(host) > 80 {
		return herr(422, "invalid filter")
	}
	limit, err := queryInt(q, "limit", 50, 1, 200)
	if err != nil {
		return err
	}
	var before *int64
	if b := q.Get("before"); b != "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n < 0 {
			return herr(422, "before must be a non-negative integer")
		}
		before = &n
	}
	entries, next := audit.Read(s.Cfg.AuditLog, user, action, host, before, limit)
	c.json(200, map[string]any{"entries": entries, "next": next})
	return nil
}

var kinds = map[string]bool{"node": true, "qemu": true, "lxc": true, "agent": true}
var statuses = map[string]bool{"online": true, "offline": true, "stopped": true, "stale": true, "paused": true}

func (s *Server) systems(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	q := c.r.URL.Query()
	kind, status, group := q.Get("kind"), q.Get("status"), q.Get("group")
	if (kind != "" && !kinds[kind]) || (status != "" && !statuses[status]) {
		return herr(422, "invalid kind or status filter")
	}
	items := []model.System{}
	for _, x := range s.Poller.Snapshot() {
		if (kind == "" || x.Kind == kind) && (status == "" || x.Status == status) && (!q.Has("group") || deref(x.Group) == group) {
			items = append(items, x)
		}
	}
	c.json(200, model.SystemList{GeneratedAt: time.Now().UTC(), Systems: items})
	return nil
}

func (s *Server) system(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	x, err := c.find(c.r.PathValue("id"))
	if err != nil {
		return err
	}
	c.json(200, x)
	return nil
}

func (s *Server) processes(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	id := c.r.PathValue("id")
	x, err := c.find(id)
	if err != nil {
		return err
	}
	q := c.r.URL.Query()
	limit, err := queryInt(q, "limit", 25, 1, 200)
	if err != nil {
		return err
	}
	sortBy := q.Get("sort")
	if sortBy == "" {
		sortBy = "cpu"
	}
	if sortBy != "cpu" && sortBy != "mem" && sortBy != "gpu" {
		return herr(422, "sort must be cpu, mem or gpu")
	}
	agent, ok := s.Poller.AgentFor(id)
	if !ok {
		where := "inside the guest"
		if x.Kind == "node" {
			where = "on the node itself"
		}
		return herr(404, fmt.Sprintf("Per-process data for %s needs the Fairy agent running %s: the Proxmox API does "+
			"not expose guest processes. Install the agent and set `links_to: %s` in config.yaml.", id, where, id))
	}
	pl, err := s.Poller.FetchProcesses(c.r.Context(), agent, id, limit, sortBy)
	if err != nil {
		return herr(502, "agent unreachable: "+normalise.Describe(err))
	}
	c.json(200, pl)
	return nil
}

func (s *Server) summary(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	c.json(200, s.Poller.Summary())
	return nil
}

func (s *Server) power(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	c.json(200, s.Poller.Power.Overview())
	return nil
}

func (s *Server) powerHistory(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	c.json(200, s.Poller.Power.HistoryView(c.r.URL.Query().Get("source")))
	return nil
}

func (s *Server) ups(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	hist := true
	if v := strings.ToLower(c.r.URL.Query().Get("history")); v == "false" || v == "0" || v == "no" || v == "off" {
		hist = false
	}
	c.json(200, s.Poller.UPS.Overview(hist))
	return nil
}

func (s *Server) upsEvents(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	q := c.r.URL.Query()
	limit, err := queryInt(q, "limit", 100, 1, 500)
	if err != nil {
		return err
	}
	after, err := queryInt(q, "after", 0, math.MinInt32, math.MaxInt32)
	if err != nil {
		return err
	}
	evs := s.Poller.UPS.EventsSince(after)
	out := []model.UpsEvent{}
	for i := len(evs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, evs[i])
	}
	c.json(200, out)
	return nil
}

// ---------------------------------------------------------------- fan control
func (c *reqCtx) fanAgent(id string) (config.Agent, error) {
	if _, err := c.find(id); err != nil {
		return config.Agent{}, err
	}
	a, ok := c.s.Poller.AgentFor(id)
	if !ok || a.FanControl == nil {
		return config.Agent{}, herr(404, "fan control is not configured for this system")
	}
	return a, nil
}

func (c *reqCtx) agentCall(a config.Agent, method, path string, body any) (map[string]any, error) {
	status, data, err := c.s.Poller.AgentRequest(c.r.Context(), a, method, path, body)
	if err != nil {
		return nil, herr(502, "agent unreachable: "+normalise.Describe(err))
	}
	if status == 401 { // never surface as our own 401 (that means "log in again")
		return nil, herr(502, "agent rejected the control token")
	}
	if status >= 400 {
		st := 502
		if status == 400 || status == 404 || status == 409 {
			st = status
		}
		msg := "agent error"
		if e, ok := data["error"]; ok {
			msg = fmt.Sprint(e)
		}
		return nil, herr(st, msg)
	}
	return data, nil
}

func (s *Server) fanStatus(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	id := c.r.PathValue("id")
	a, err := c.fanAgent(id)
	if err != nil {
		return err
	}
	data, err := c.agentCall(a, "GET", "/fan/status", nil)
	if err != nil {
		return err
	}
	data["system_id"] = id
	b, _ := json.Marshal(data)
	var fs model.FanStatus
	if err := json.Unmarshal(b, &fs); err != nil {
		return herr(502, "agent returned an invalid fan status")
	}
	model.Normalize(&fs)
	c.json(200, fs)
	return nil
}

type fanModeReq struct {
	Mode string `json:"mode"`
}

type fanDutyReq struct {
	Zone    *int `json:"zone"`
	Percent *int `json:"percent"`
}

func (s *Server) fanWrite(c *reqCtx, action string, body any, params map[string]any) error {
	u, err := c.controlGuard(s.control)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	a, err := c.fanAgent(id)
	if err != nil {
		return err
	}
	path := "/fan/mode"
	if action == "fan.duty" {
		path = "/fan/duty"
	}
	data, err := c.agentCall(a, "POST", path, body)
	if err != nil {
		he := err.(*httpErr)
		c.audit(u, action, id, params, fmt.Sprintf("error %d: %v", he.Status, he.Detail))
		return err
	}
	c.audit(u, action, id, params, "ok")
	c.json(200, model.ControlResult{OK: true, Result: data})
	return nil
}

func (s *Server) fanMode(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	var b fanModeReq
	if err := c.decodeBody(&b); err != nil {
		return err
	}
	switch b.Mode {
	case "standard", "full", "optimal", "heavyio":
	default:
		return herr(422, "mode must be standard, full, optimal or heavyio")
	}
	return s.fanWrite(c, "fan.mode", b, map[string]any{"mode": b.Mode})
}

func (s *Server) fanDuty(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	var b fanDutyReq
	if err := c.decodeBody(&b); err != nil {
		return err
	}
	if b.Zone == nil || (*b.Zone != 0 && *b.Zone != 1) || b.Percent == nil || *b.Percent < 0 || *b.Percent > 100 {
		return herr(422, "zone must be 0 or 1 and percent 0-100")
	}
	return s.fanWrite(c, "fan.duty", map[string]any{"zone": *b.Zone, "percent": *b.Percent},
		map[string]any{"zone": *b.Zone, "percent": *b.Percent})
}

// ---------------------------------------------------------------- per-user UI settings
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
	raw, err := io.ReadAll(io.LimitReader(c.r.Body, 64<<10))
	if err != nil {
		return herr(400, "unreadable body")
	}
	body, err := settings.Decode(raw)
	if err != nil {
		return err
	}
	cur := s.Settings.Get(u.Sub)
	if body.Background.Kind == "upload" {
		if p, _ := s.Settings.BackgroundPath(u.Sub); p == "" {
			return herr(400, "no uploaded background; POST /api/settings/background first")
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

func (s *Server) uploadBackground(c *reqCtx) error {
	u, err := c.sessionCSRF()
	if err != nil {
		return err
	}
	if c.r.ContentLength > settings.MaxBackgroundBytes {
		return herr(413, "image too large")
	}
	data, err := io.ReadAll(io.LimitReader(c.r.Body, settings.MaxBackgroundBytes+1)) // bounded even without Content-Length
	if err != nil {
		return herr(400, "unreadable body")
	}
	if len(data) > settings.MaxBackgroundBytes {
		return herr(413, "image too large")
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
		return herr(404, "no background uploaded")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return herr(404, "no background uploaded")
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

// ---------------------------------------------------------------- power control
func (s *Server) powerCaps(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	x, err := c.find(c.r.PathValue("id"))
	if err != nil {
		return err
	}
	c.json(200, powerctl.Capabilities(s.Poller, x))
	return nil
}

func (s *Server) powerAction(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	var req powerctl.Request
	if err := c.decodeBody(&req); err != nil {
		return err
	}
	if err := req.Validate(); err != nil {
		return err
	}
	u, err := c.controlGuard(s.control)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	x, err := c.find(id)
	if err != nil {
		return err
	}
	params := map[string]any{"action": req.Action, "delay_s": *req.DelayS}
	who := deref(u.Name)
	if who == "" {
		who = deref(u.Email)
	}
	if who == "" {
		who = u.Sub
	}
	res, err := powerctl.Execute(c.r.Context(), s.Poller, x, req, who)
	if err != nil {
		st, detail := 500, "internal error"
		if ae, ok := err.(*apierr.Error); ok {
			st, detail = ae.Status, ae.Detail
		}
		c.audit(u, "power."+req.Action, id, params, fmt.Sprintf("error %d: %s", st, detail))
		return err
	}
	params["via"] = res.Via
	c.audit(u, "power."+req.Action, id, params, "ok")
	c.json(200, res)
	return nil
}

// ---------------------------------------------------------------- polling rate
func (s *Server) getPoll(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	id := c.r.PathValue("id")
	if _, err := c.find(id); err != nil {
		return err
	}
	c.json(200, s.Poller.PollStatus(id))
	return nil
}

func (s *Server) putPoll(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	var body struct {
		IntervalS any `json:"interval_s"`
	}
	if err := c.decodeBody(&body); err != nil {
		return err
	}
	var ov *scheduler.Override
	var num *float64
	switch v := body.IntervalS.(type) {
	case float64:
		num = &v
		ov = &scheduler.Override{Seconds: v}
	case string:
		switch v {
		case "paused":
			ov = &scheduler.Override{Paused: true}
		case "default":
		default:
			return herr(422, `interval_s must be seconds, "paused" or "default"`)
		}
	default:
		return herr(422, `interval_s must be seconds, "paused" or "default"`)
	}
	u, err := c.controlGuard(s.pollCtl)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	if _, err := c.find(id); err != nil {
		return err
	}
	if num != nil && (*num < 1 || *num > 300) {
		c.audit(u, "poll.set", id, map[string]any{"interval_s": *num}, "error 422: out of range")
		return herr(422, "interval_s must be between 1 and 300 seconds")
	}
	if err := s.Poller.SetPoll(id, ov); err != nil {
		return err
	}
	c.audit(u, "poll.set", id, map[string]any{"interval_s": body.IntervalS}, "ok")
	c.json(200, s.Poller.PollStatus(id))
	return nil
}
