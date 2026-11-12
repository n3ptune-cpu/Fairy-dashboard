package web

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

func (s *Server) me(c *reqCtx) error {
	u, g, err := c.grant()
	if err != nil {
		return err
	}
	me := model.Me{User: *u, CanControl: g.Admin(), Demo: s.DemoUser != nil,
		ControlDisabled: s.Poller.Admin.ControlDisabled(), StepUpWindowS: s.Cfg.StepUpWindowS, AuthAgeS: c.authAge(u),
		Role: g.Role, Sites: g.Sites, AllSites: g.SeesAll(), CanManageUsers: g.Admin() && u.Auth != "token", Docs: s.Cfg.EnableDocs}
	if me.Sites == nil {
		me.Sites = []string{}
	}
	me.ApprovalClient = s.Cfg.ApprovalOn()
	if s.DemoUser != nil {
		me.DemoView = g.String()
		me.DemoViews = s.demoViews()
	}
	if u.Auth != "token" { // the outcome of the action this client just confirmed: shown once, to it alone
		me.PendingResult = s.codec.live.takeResult(c.pendingKey(), c.client(), s.now())
	}
	c.json(200, me)
	return nil
}

var actionRe = regexp.MustCompile(`^[a-z_.]+$`)

func (s *Server) auditPage(c *reqCtx) error {
	u, g, err := c.grant()
	if err != nil {
		return err
	}
	if !g.Admin() {
		return c.deny(u, 403, "the audit log is visible to admins only", "audit read denied")
	}
	q := c.r.URL.Query()
	user, action, host := q.Get("q"), q.Get("action"), q.Get("host")
	if len(user) > 100 || len(action) > 40 || (action != "" && !actionRe.MatchString(action)) || len(host) > 80 {
		return apierr.New(422, "invalid filter")
	}
	limit, err := queryInt(q, "limit", 50, 1, 200)
	if err != nil {
		return err
	}
	var before *int64
	if b := q.Get("before"); b != "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n < 0 {
			return apierr.New(422, "before must be a non-negative integer")
		}
		before = &n
	}
	entries, next := audit.Read(s.Cfg.AuditLog, user, action, host, before, limit)
	if u.Auth == "token" { // a machine id is for an admin in a browser: a token reads the entries without its characters
		auditForToken(entries)
	}
	c.json(200, model.AuditPage{Entries: entries, Next: next})
	return nil
}

var kinds = map[string]bool{"node": true, "qemu": true, "lxc": true, "agent": true}

var statuses = map[string]bool{"online": true, "offline": true, "stopped": true, "stale": true, "paused": true, poller.StatusError: true}

func (s *Server) systems(c *reqCtx) error {
	v, err := c.view()
	if err != nil {
		return err
	}
	q := c.r.URL.Query()
	kind, status, group := q.Get("kind"), q.Get("status"), q.Get("group")
	if (kind != "" && !kinds[kind]) || (status != "" && !statuses[status]) {
		return apierr.New(422, "invalid kind or status filter")
	}
	items := []model.System{}
	for _, x := range v.Systems() {
		if (kind == "" || x.Kind == kind) && (status == "" || x.Status == status) && (!q.Has("group") || model.Deref(x.Group) == group) {
			items = append(items, x)
		}
	}
	c.json(200, model.SystemList{GeneratedAt: time.Now().UTC(), Systems: items})
	return nil
}

func (s *Server) system(c *reqCtx) error {
	x, err := c.find(c.r.PathValue("id"))
	if err != nil {
		return err
	}
	c.json(200, x)
	return nil
}

func (s *Server) processes(c *reqCtx) error {
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
		return apierr.New(422, "sort must be cpu, mem or gpu")
	}
	agent, ok := s.Poller.AgentFor(id)
	if !ok {
		where := "inside the guest"
		if x.Kind == "node" {
			where = "on the node itself"
		}
		return apierr.New(404, fmt.Sprintf("Per-process data for %s needs the Fairy agent running %s: the Proxmox API does "+
			"not expose guest processes. Install the agent and set `links_to: %s` in config.yaml.", id, where, id))
	}
	release, err := c.agentRead(agent.ID)
	if err != nil {
		return err
	}
	defer release()
	pl, err := s.Poller.FetchProcesses(c.r.Context(), agent, id, limit, sortBy)
	if he := (*devices.HTTPStatusError)(nil); errors.As(err, &he) { // it answered: 424, not 502 (apierr.Upstream)
		return apierr.New(apierr.Upstream, "agent error: "+devices.Describe(err))
	}
	if err != nil {
		return apierr.New(502, "agent unreachable: "+devices.Describe(err))
	}
	c.json(200, pl)
	return nil
}

func (s *Server) summary(c *reqCtx) error {
	v, err := c.view()
	if err != nil {
		return err
	}
	c.json(200, v.Summary())
	return nil
}

func (s *Server) power(c *reqCtx) error {
	v, err := c.view()
	if err != nil {
		return err
	}
	c.json(200, v.Power())
	return nil
}

func (s *Server) powerHistory(c *reqCtx) error {
	v, err := c.view()
	if err != nil {
		return err
	}
	c.json(200, v.PowerHistory(c.r.URL.Query().Get("source")))
	return nil
}

func (s *Server) ups(c *reqCtx) error {
	v, err := c.view()
	if err != nil {
		return err
	}
	hist := true
	if v := strings.ToLower(c.r.URL.Query().Get("history")); v == "false" || v == "0" || v == "no" || v == "off" {
		hist = false
	}
	c.json(200, v.UPS(hist))
	return nil
}

func (s *Server) upsEvents(c *reqCtx) error {
	v, err := c.view()
	if err != nil {
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
	evs := v.UPSEvents(after)
	out := []model.UpsEvent{}
	for i := len(evs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, evs[i])
	}
	c.json(200, out)
	return nil
}
