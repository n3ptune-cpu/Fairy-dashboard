package web

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/n3ptune-cpu/Fairy/internal/audit"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/notify"
)

func sourceOf(u *model.User) string {
	switch u.Auth {
	case "token":
		return "token:" + deref(u.TokenName)
	case "demo":
		return "demo"
	}
	return "ui"
}

func actorOf(u *model.User) string {
	if u.Auth == "token" {
		return deref(u.TokenName) + " (token, owner " + deref(u.TokenOwner) + ")"
	}
	for _, v := range []*string{u.Email, u.PreferredUsername, u.Name} {
		if v != nil && *v != "" {
			return *v
		}
	}
	return u.Sub
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

var controlActions = []string{"fan.", "power.", "poll."}

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
