package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

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
var controlActions = []string{"fan.", "power.", "helper_", "poll.", "user.", "site.", "update.", "panel.", "host."}

// deniedAction is the entry every refused request of a signed-in user or API token writes (reqCtx.deny).
const deniedAction = "auth.denied"

// maxTarget is the longest target written to the audit log (bytes). Host ids, site names and the panel's own
// paths are far shorter; the target of a refused request is whatever address the client asked for.
const maxTarget = 200

// safeTarget is a target as it is written to the audit log: control characters, the Unicode line ends, bidi
// overrides and bytes that are not UTF-8 are percent-encoded (a line feed becomes %0A), and it is cut to
// maxTarget bytes. The target of a refused request is the requested path, which the client chooses.
func safeTarget(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < maxTarget; {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n <= 1, r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0, r == 0x2028, r == 0x2029,
			r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f, r == 0x061c:
			for _, c := range []byte(s[i : i+n]) {
				fmt.Fprintf(&b, "%%%02X", c)
			}
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
		if i < len(s) && b.Len() >= maxTarget {
			b.WriteString("…")
		}
	}
	return b.String()
}

func (c *reqCtx) audit(u *model.User, action, target string, params map[string]any, outcome string) {
	ip := c.ip()
	sub := u.Sub
	if action == deniedAction { // refusals can be produced without limit: throttled per subject (auditthrottle.go)
		ok, dropped, ips := c.s.anonAudit.allowDenied(sub, actorOf(u), time.Now())
		c.s.auditSuppressed(dropped, ips)
		if !ok {
			return
		}
	}
	c.s.writeAudit(audit.Entry{Actor: actorOf(u), Sub: &sub, Action: action, Target: safeTarget(target),
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

// auditSuppressed records what the throttle dropped in the last minute: one entry for the anonymous entries
// (how many, from how many addresses), and one entry per signed-in subject whose refused requests were dropped.
// The subject's entry carries their sub, so the Audit page's user filter finds it next to their other entries.
func (s *Server) auditSuppressed(dropped, ips int) {
	if dropped > 0 {
		slog.Warn("audit: unauthenticated entries throttled", "dropped", dropped, "addresses", ips)
		s.writeAudit(audit.Entry{Actor: "fairy", Action: "audit.suppressed", Source: "fairy",
			Params:  map[string]any{"dropped": dropped, "addresses": ips, "per_ip_per_min": anonPerIP},
			Outcome: fmt.Sprintf("%d unauthenticated entries (rejected tokens, failed sign-ins) not written", dropped)})
	}
	for _, d := range s.anonAudit.takeDenied() {
		slog.Warn("audit: refused requests of one account throttled", "sub", d.Sub, "actor", d.Actor, "dropped", d.Dropped)
		e := audit.Entry{Actor: "fairy", Action: "audit.suppressed", Source: "fairy",
			Params: map[string]any{"dropped": d.Dropped, "action": deniedAction, "sub": d.Sub, "actor": d.Actor,
				"per_subject_per_min": deniedPerSub},
			Outcome: fmt.Sprintf("%d refused requests (%s) by %s not written", d.Dropped, deniedAction, d.Actor)}
		if d.Sub != "" {
			sub := d.Sub
			e.Sub = &sub
		}
		s.writeAudit(e)
	}
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
