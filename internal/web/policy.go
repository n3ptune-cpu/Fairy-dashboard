package web

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
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
	switch v := claims["email_verified"].(type) { // some providers send the string "true"
	case bool:
		u.EmailVerified = v
	case string:
		u.EmailVerified = strings.EqualFold(v, "true")
	}
	if gs, ok := claims[groupsClaim].([]any); ok {
		for _, g := range gs {
			u.Groups = append(u.Groups, fmt.Sprint(g))
		}
	}
	return u
}

// emailIn reports whether the user's email is in list (case-insensitive). Only a verified email counts: an
// address a user could set on their own account must never decide who signs in or becomes admin.
func emailIn(u *model.User, list []string) bool {
	e := strings.ToLower(model.Deref(u.Email))
	if e == "" || !u.EmailVerified {
		return false
	}
	return slices.ContainsFunc(list, func(x string) bool { return strings.ToLower(x) == e })
}

func isAllowed(u model.User, o config.OIDC) bool {
	if len(o.AllowedGroups) > 0 && !slices.ContainsFunc(u.Groups, func(g string) bool { return slices.Contains(o.AllowedGroups, g) }) {
		return false
	}
	return emailAllowed(u, o)
}

// emailAllowed is isAllowed's e-mail rule alone: what can be said of an account whose groups are not known.
func emailAllowed(u model.User, o config.OIDC) bool {
	return len(o.AllowedEmails) == 0 || emailIn(&u, o.AllowedEmails)
}

func canControl(u *model.User, o config.OIDC) bool {
	if u.Auth == "token" {
		return model.Deref(u.TokenScope) == "control"
	}
	return len(o.ControlGroups) > 0 && slices.ContainsFunc(u.Groups, func(g string) bool { return slices.Contains(o.ControlGroups, g) })
}

// maxNext is the longest "next" the panel keeps. The page's own addresses are a few dozen characters. The value
// goes into the signed cookie of the sign-in round trip: 5000 characters made a cookie of over 7 kB, which a
// browser drops (it keeps the old one), and the way back then failed as "started in another browser" (audit 6,
// r4-p1-05).
const maxNext = 512

// safeNext only allows same-site relative redirects (no //evil.com, no schemes), and none longer than maxNext:
// anything else is the start page.
func safeNext(t string) string {
	if t == "" || len(t) > maxNext || !strings.HasPrefix(t, "/") || strings.HasPrefix(t, "//") || strings.Contains(t, "\\") {
		return "/"
	}
	u, err := url.Parse(t)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return t
}
