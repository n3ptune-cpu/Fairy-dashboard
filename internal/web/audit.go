package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notify"
)

func sourceOf(u *model.User) string {
	switch u.Auth {
	case "token":
		return "token:" + model.Deref(u.TokenName)
	case "demo":
		return "demo"
	}
	return "ui"
}

func actorOf(u *model.User) string {
	if u.Auth == "token" {
		return model.Deref(u.TokenName) + " (token, owner " + model.Deref(u.TokenOwner) + ")"
	}
	for _, v := range []*string{u.Email, u.PreferredUsername, u.Name} {
		if v != nil && *v != "" {
			return *v
		}
	}
	return u.Sub
}

// controlActions are the audited actions that also send the optional ntfy / Discord notification: control
// actions and every change to users, roles and sites (including a new user waiting for access).
var controlActions = []string{"fan.", "power.", "poll.", "user.", "site.", "update.", "panel.", "host."}

func (c *reqCtx) audit(u *model.User, action, target string, params map[string]any, outcome string) {
	ip := c.ip()
	sub := u.Sub
	c.s.writeAudit(audit.Entry{Actor: actorOf(u), Sub: &sub, Action: action, Target: target,
		Params: params, Outcome: outcome, Source: sourceOf(u), IP: &ip})
	for _, p := range controlActions {
		if strings.HasPrefix(action, p) {
			b, _ := json.Marshal(params)
			notify.Notify("Fairy: "+action+" on "+target, actorOf(u)+" via "+sourceOf(u)+": "+outcome+" "+string(b))
			break
		}
	}
}

// auditAnon writes an entry about a request that is not (yet) signed in. Entries without a subject (rejected
// tokens, failed or rate-limited sign-ins) can be caused by anyone, so they are throttled per client IP (see
// auditThrottle); the dropped ones are summed up in an "audit.suppressed" entry.
func (c *reqCtx) auditAnon(action, outcome, actor string, sub *string, params map[string]any) {
	ip := c.ip()
	if sub == nil {
		ok, dropped, ips := c.s.anonAudit.allow(ip, time.Now())
		c.s.auditSuppressed(dropped, ips)
		if !ok {
			return
		}
	}
	if actor == "" {
		actor = "?"
	}
	src := "ui"
	if action == "auth.token_rejected" {
		src = "token"
	}
	c.s.writeAudit(audit.Entry{Actor: actor, Sub: sub, Action: action, Params: params, Outcome: outcome, Source: src, IP: &ip})
}

// auditSuppressed records how many anonymous entries the throttle dropped in the last minute.
func (s *Server) auditSuppressed(dropped, ips int) {
	if dropped == 0 {
		return
	}
	slog.Warn("audit: unauthenticated entries throttled", "dropped", dropped, "addresses", ips)
	s.writeAudit(audit.Entry{Actor: "fairy", Action: "audit.suppressed", Source: "fairy",
		Params:  map[string]any{"dropped": dropped, "addresses": ips, "per_ip_per_min": anonPerIP},
		Outcome: fmt.Sprintf("%d unauthenticated entries (rejected tokens, failed sign-ins) not written", dropped)})
}

// writeAudit appends an entry. A failure never blocks the request (sign-in must keep working), but it is
// logged loudly: at once, then at most once a minute with the number of entries lost.
func (s *Server) writeAudit(e audit.Entry) {
	_, err := audit.Write(s.Cfg.AuditLog, e)
	s.auditErrMu.Lock()
	defer s.auditErrMu.Unlock()
	if err == nil {
		return
	}
	s.auditLost++
	if time.Since(s.auditErrAt) >= time.Minute {
		slog.Error("AUDIT LOG WRITE FAILED: entries are being lost (disk full? wrong owner?)", "path", s.Cfg.AuditLog,
			"lost", s.auditLost, "error", err)
		s.auditErrAt, s.auditLost = time.Now(), 0
	}
}

// errorOutcome is the audit-log outcome for a failed control action: "error <status>: <detail>", with
// unexpected (non-API) errors recorded as the generic 500 the client sees.
func errorOutcome(err error) string {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return fmt.Sprintf("error %d: %v", ae.Status, ae.Detail)
	}
	return "error 500: internal error"
}
