package web

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/model"
)

func userFromClaims(claims map[string]any, groupsClaim string) model.User {
	str := func(k string) *string {
		if v, ok := claims[k].(string); ok && v != "" {
			return &v
		}
		return nil
	}
	u := model.User{Sub: fmt.Sprint(claims["sub"]), Name: str("name"), PreferredUsername: str("preferred_username"),
		Email: str("email"), Picture: str("picture"), Groups: []string{}}
	if u.Name == nil {
		u.Name = str("display_name")
	}
	if gs, ok := claims[groupsClaim].([]any); ok {
		for _, g := range gs {
			u.Groups = append(u.Groups, fmt.Sprint(g))
		}
	}
	return u
}

func isAllowed(u model.User, o config.OIDC) bool {
	if len(o.AllowedGroups) > 0 && !slices.ContainsFunc(u.Groups, func(g string) bool { return slices.Contains(o.AllowedGroups, g) }) {
		return false
	}
	if len(o.AllowedEmails) > 0 {
		e := strings.ToLower(deref(u.Email))
		if !slices.ContainsFunc(o.AllowedEmails, func(x string) bool { return strings.ToLower(x) == e }) {
			return false
		}
	}
	return true
}

func canControl(u *model.User, o config.OIDC) bool {
	if u.Auth == "token" {
		return deref(u.TokenScope) == "control"
	}
	return len(o.ControlGroups) > 0 && slices.ContainsFunc(u.Groups, func(g string) bool { return slices.Contains(o.ControlGroups, g) })
}

// safeNext only allows same-site relative redirects (no //evil.com, no schemes).
func safeNext(t string) string {
	if t == "" || !strings.HasPrefix(t, "/") || strings.HasPrefix(t, "//") || strings.Contains(t, "\\") {
		return "/"
	}
	u, err := url.Parse(t)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return t
}
