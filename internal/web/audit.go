package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/n3ptune-cpu/Fairy/internal/apierr"
	"github.com/n3ptune-cpu/Fairy/internal/audit"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/notify"
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
var controlActions = []string{"fan.", "power.", "poll.", "user.", "site."}

func (c *reqCtx) audit(u *model.User, action, target string, params map[string]any, outcome string) {
	ip := c.ip()
	sub := u.Sub
	if _, err := audit.Write(c.s.Cfg.AuditLog, audit.Entry{Actor: actorOf(u), Sub: &sub, Action: action, Target: target,
		Params: params, Outcome: outcome, Source: sourceOf(u), IP: &ip}); err != nil {
		slog.Error("audit log write failed", "error", err)
	}
	for _, p := range controlActions {
		if strings.HasPrefix(action, p) {
			b, _ := json.Marshal(params)
			notify.Notify("Fairy: "+action+" on "+target, actorOf(u)+" via "+sourceOf(u)+": "+outcome+" "+string(b))
			break
		}
	}
}

func (c *reqCtx) auditAnon(action, outcome, actor string, sub *string, params map[string]any) {
	ip := c.ip()
	if actor == "" {
		actor = "?"
	}
	src := "ui"
	if action == "auth.token_rejected" {
		src = "token"
	}
	if _, err := audit.Write(c.s.Cfg.AuditLog, audit.Entry{Actor: actor, Sub: sub, Action: action, Params: params,
		Outcome: outcome, Source: src, IP: &ip}); err != nil {
		slog.Error("audit log write failed", "error", err)
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
