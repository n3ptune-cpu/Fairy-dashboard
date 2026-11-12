package web

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"unicode"
	"unicode/utf8"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
)

// The callback is an address the browser itself goes to, coming from Pocket ID. A refusal there used to be one
// line of JSON ({"detail":"login failed"}) with nothing to click. Every refusal is now the panel's small error
// page (errorPage: no script, everything escaped) with what happened in a sentence and one link back.
//
// What the sentences say is kept as coarse as the JSON was: which check of the ID token failed stays in the
// server log and the audit log only (oidclog.go). The one value that comes from outside, Pocket ID's `error`
// parameter, is shown only when it looks like an error code (shownCode: lower-case letters and underscores, as
// the codes of RFC 6749 and OpenID Connect are) and escaped by errorPage like everything else. Anybody can
// make a link to the callback with a text of their choosing in that parameter, and it needs no cookie: a
// sentence is not put on the panel's page (R7B-03). The log and the audit entry keep what was sent.

// The refusals of the callback, as the page says them.
const (
	cbSignIn     = "Sign-in failed"
	cbConfirm    = "Could not confirm it's you"
	cbNotAllowed = "Not allowed"

	cbErrorAnswer = "Pocket ID answered with an error. Go back and try again."
	cbStale       = "This sign-in was started more than 10 minutes ago, in another browser, or its address was opened a second time. Go back and try again."
	cbUnchecked   = "Fairy could not check Pocket ID's answer (an answer is good only once). Go back and try again; if it happens again, the panel's log says why."
	cbUnreachable = "Fairy could not reach Pocket ID. Try again in a moment."
	cbNoGroups    = "Fairy could not read this account's groups from Pocket ID, so it could not check the account. Sign in again in a moment."
	cbRefused     = "Pocket ID signed you in, but this account may not use this panel. An administrator decides that with the allowed groups and e-mail addresses in Fairy's config."

	// A repeat of a confirmation's callback from the cookie it was started with (steprepeat.go).
	// The sentence says what the first callback knows of the kept action: it answered with a 2xx (sent), with a
	// 4xx (refused, by the panel or by the machine: not carried out), with anything else (an error: the request
	// may have arrived), or it has not answered yet.
	cbRepeat        = "Already confirmed"
	cbRepeatRan     = "The action was already sent. It is not sent again. Sign in again to go on."
	cbRepeatRefused = "The action was not carried out: it was refused. It is not tried again. Sign in again to go on."
	cbRepeatUnsure  = "The action ended with an error: Fairy does not know whether it went through. It is not sent again. Sign in again to go on."
	cbRepeatAtWork  = "The action is on its way. It is not sent again. Sign in again to go on."
	cbRepeatNone    = "This confirmation was already used. Sign in again to go on."
)

// callbackPage is where a refused callback ends: title is cbSignIn for a sign-in and cbConfirm for a
// confirmation ("Confirm it's you"), the link goes back to the page the round trip started from.
type callbackPage struct {
	c     *reqCtx
	title string
	back  string
}

func (c *reqCtx) callbackPage(sess *sessionData) callbackPage {
	p := callbackPage{c: c, title: cbSignIn, back: safeNext(sess.Next)}
	if sess.StepUp {
		p.title = cbConfirm
	}
	return p
}

func (p callbackPage) refuse(status int, msg string) error {
	return p.c.errorPage(status, p.title, msg, p.back)
}

// limited is the refusal by the limit on sign-in attempts of one address, with the time to wait.
func (p callbackPage) limited(l *rateLimiter, key string) error {
	wait := max(l.wait(key), 1)
	p.c.w.Header().Set("Retry-After", strconv.Itoa(wait))
	return p.refuse(429, fmt.Sprintf("Too many sign-in attempts from this address. Try again in %d s.", wait))
}

// repeated is the answer to the repeat of a confirmation's callback that was accepted, from the cookie the
// round trip was started with: the page says so and sets no cookie. ran: a kept action was run by the first;
// status: what it answered with (0: not yet).
func (p callbackPage) repeated(ran bool, status int) error {
	p.title = cbRepeat
	switch {
	case !ran:
		return p.refuse(409, cbRepeatNone)
	case status == 0:
		return p.refuse(409, cbRepeatAtWork)
	case status >= 200 && status < 300:
		return p.refuse(409, cbRepeatRan)
	case status >= 400 && status < 500:
		return p.refuse(409, cbRepeatRefused)
	}
	return p.refuse(409, cbRepeatUnsure)
}

// shownCode is what an error code of Pocket ID must look like to be shown: access_denied, login_required,
// interaction_required and the like.
var shownCode = regexp.MustCompile(`^[a-z_]{1,40}$`)

// failed is the refusal of a callback whose answer from Pocket ID did not pass: code is Pocket ID's own error
// code when it sent one (as it was sent), stale says that the round trip is not this browser's current one.
func (p callbackPage) failed(code string, stale bool) error {
	switch {
	case shownCode.MatchString(code):
		return p.refuse(400, "Pocket ID answered with an error ("+code+"). Go back and try again.")
	case code != "":
		return p.refuse(400, cbErrorAnswer)
	case stale:
		return p.refuse(400, cbStale)
	}
	return p.refuse(400, cbUnchecked)
}

// err is the page for an error of the panel itself on the way (a state file that cannot be written): the
// sentence the JSON answer had, as a sentence. Anything that is not such an error is left to the caller.
func (p callbackPage) err(err error) error {
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		return err
	}
	detail, ok := ae.Detail.(string)
	if !ok {
		return err
	}
	return p.refuse(ae.Status, sentence(detail))
}

// sentence makes "sign-in could not be recorded (…); see the server log" start with a capital and end with a
// full stop.
func sentence(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	s = string(unicode.ToUpper(r)) + s[n:]
	if last, _ := utf8.DecodeLastRuneInString(s); last != '.' && last != '!' && last != '?' {
		s += "."
	}
	return s
}
