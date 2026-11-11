package web

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/oauth2"
)

// Why a sign-in failed, for the server log only (the callback's page says no more than "could not check Pocket
// ID's answer", callbackpage.go). These
// helpers never log the authorization code, the PKCE verifier, the client secret, tokens or a response body:
// only RFC 6749's error / error_description, the HTTP status and error messages with URLs cut down to
// scheme://host/path.

// loginFailure is a failed callback step: a short reason for the log and the audit trail, an optional short
// addition to the audit entry (an OAuth error code) and log-only detail.
type loginFailure struct {
	reason string
	audit  string
	attrs  []any
	code   string // Pocket ID's own error code as it was sent, when the failure is one it reported (for the page)
}

// staleState is the reason of a callback that is not the answer to this browser's current round trip.
const staleState = "mismatching_state"

func (f *loginFailure) auditOutcome() string { return "failed: " + f.why() }

// why is the reason with its addition, without the word "failed".
func (f *loginFailure) why() string {
	if f.audit != "" {
		return f.reason + " (" + f.audit + ")"
	}
	return f.reason
}

// exchangeFailure describes a failed code exchange. A *oauth2.RetrieveError contributes only its OAuth error
// fields and the HTTP status (its Error() would include the raw response body).
func exchangeFailure(err error, redact ...string) *loginFailure {
	f := &loginFailure{reason: "token exchange"}
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		f.attrs = []any{"error", safeErr(err, redact...)}
		return f
	}
	if re.ErrorCode != "" {
		f.audit = oauthCode(re.ErrorCode)
		f.attrs = append(f.attrs, "error", f.audit)
	} else {
		f.attrs = append(f.attrs, "error", "no OAuth error in the response")
	}
	if re.ErrorDescription != "" {
		f.attrs = append(f.attrs, "error_description", safeText(re.ErrorDescription, 200, redact...))
	}
	if re.Response != nil {
		f.attrs = append(f.attrs, "http_status", re.Response.StatusCode)
	}
	return f
}

// oauthCode keeps an RFC 6749 error code to its own character set (it ends up in the audit trail).
func oauthCode(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, s)
	return clip(s, 60)
}

var urlRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)

// statusBodyRe matches go-oidc's "<status>: <response body>" errors (userinfo, discovery).
var statusBodyRe = regexp.MustCompile(`^(?:oidc: )?([1-5][0-9]{2} [^:]{0,60}): `)

// safeErr is err's message with URLs reduced to scheme://host/path, response bodies after an HTTP status
// dropped, control characters removed, the redact values replaced and the result clipped to 200 characters.
func safeErr(err error, redact ...string) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if m := statusBodyRe.FindStringSubmatch(s); m != nil {
		s = "HTTP " + m[1]
	}
	return safeText(s, 200, redact...)
}

func safeText(s string, n int, redact ...string) string {
	for _, r := range redact {
		if len(r) >= 4 {
			s = strings.ReplaceAll(s, r, "[redacted]")
		}
	}
	s = urlRe.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "[url]"
		}
		return u.Scheme + "://" + u.Host + u.EscapedPath()
	})
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	return clip(s, n)
}

// clip cuts s to at most n bytes without splitting a UTF-8 sequence.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "..."
}
