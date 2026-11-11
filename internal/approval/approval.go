// Package approval is the panel's half of the passkey approval a machine verifies itself (release item 1.1; the
// machine's half is Fairy-agent's pkg/approval, the contract is its pkg/agent/api).
//
// A machine whose "approval" state is "required" runs a fan change, a shutdown / restart / sleep or an update
// of its agent only together with an approval: an ID token of the Pocket ID client "Fairy PC actions" whose
// nonce is made from a list of entries, one per (the machine's challenge, the exact request). The panel asks
// each machine for its challenge, sends the person to Pocket ID once with the nonce of the whole list, checks
// the answer for its own audit trail, and hands token and list to the machines. The machines do not trust the
// panel: each verifies the token offline against the key it has pinned.
//
// This package holds what can be said without HTTP: the list and its nonce (made with the agent module's own
// functions, so panel and machine cannot drift apart), the panel's own checks of the token's claims, and the
// trust a machine is proposed when the check is switched on for it. The round trip is in internal/web.
package approval

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
)

// Target is one request to one machine.
type Target struct {
	AgentID string // the agent the request is sent to (the challenge is only good there)
	Name    string // the machine's name, for people
	Request api.ApprovalRequest
}

// Bound is a Target after its machine was asked: the canonical text of the request and the machine's
// challenge for exactly it.
type Bound struct {
	Target
	Canonical string
	Challenge string
}

// Plan is what one approval covers: the entries in the order the nonce was made from, each with the machine
// and request it belongs to. It is made before the person is sent to Pocket ID and never changes afterwards.
type Plan struct {
	bound   []Bound
	entries []api.ApprovalEntry
	nonce   string
}

// NewPlan makes the plan of bound, in that order. It refuses what a machine would refuse: no entry, more than
// api.MaxApprovalEntries, a malformed challenge, the same challenge twice, a request without a canonical text
// or whose canonical text is not the one given.
func NewPlan(bound []Bound) (*Plan, error) {
	p := &Plan{bound: slices.Clone(bound)}
	for _, b := range bound {
		canon, err := b.Request.Canonical()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", b.Name, err)
		}
		if canon != b.Canonical {
			return nil, fmt.Errorf("%s: the request text %q is not the one of the request", b.Name, b.Canonical)
		}
		p.entries = append(p.entries, api.ApprovalEntry{Challenge: b.Challenge, Request: canon})
	}
	nonce, err := api.ApprovalNonce(p.entries)
	if err != nil {
		return nil, err
	}
	p.nonce = nonce
	return p, nil
}

// Nonce is the nonce of the one Pocket ID request that approves the plan.
func (p *Plan) Nonce() string { return p.nonce }

// Entries is the whole list, as every machine of the plan gets it. The caller must not change it.
func (p *Plan) Entries() []api.ApprovalEntry { return p.entries }

// Bound is the plan's requests, in the order of Entries.
func (p *Plan) Bound() []Bound { return slices.Clone(p.bound) }

// Covers reports whether the plan holds exactly one entry for this request to this agent. The comparison is by
// the canonical text: a request that differs in any field has another text.
func (p *Plan) Covers(agentID string, req api.ApprovalRequest) bool {
	canon, err := req.Canonical()
	if err != nil {
		return false
	}
	n := 0
	for _, b := range p.bound {
		if b.AgentID == agentID && b.Canonical == canon {
			n++
		}
	}
	return n == 1
}

// MaxAuthAge is how old the authentication at Pocket ID may be when its answer comes back to the panel (the
// same two minutes "Confirm it's you" allows).
const MaxAuthAge = 2 * time.Minute

// Claims are the claims of an ID token the panel looks at itself, after its signature, issuer, audience and
// expiry were verified against the issuer's keys.
type Claims struct {
	Nonce    string
	Sub      string
	AuthTime float64 // Unix seconds; 0: the token has none
	AMR      []string
}

// Want is what the claims must say.
type Want struct {
	Nonce string // the plan's
	Sub   string // the signed-in person's Pocket ID user id
	// AnyPerson: do not compare Sub (a test build has no signed-in person). Never set in a release.
	AnyPerson bool
	Now       time.Time
}

// Failure is why the panel does not take an approval. Reason is a short word for the audit log; the text is
// for the person.
type Failure struct {
	Reason string
	Text   string
}

func (f *Failure) Error() string { return f.Reason + ": " + f.Text }

// Reasons of a Failure.
const (
	ReasonSize     = "token_size"
	ReasonNonce    = "nonce"
	ReasonPerson   = "user"
	ReasonNoAuth   = "auth_time_missing"
	ReasonAuthTime = "auth_time"
	ReasonAMR      = "amr"
)

// CheckSize refuses a token no machine would take (api.MaxApprovalToken), before anything is done with it.
func CheckSize(rawToken string) *Failure {
	if len(rawToken) == 0 || len(rawToken) > api.MaxApprovalToken {
		return &Failure{ReasonSize, fmt.Sprintf("Pocket ID's answer is %d bytes; a machine takes at most %d.", len(rawToken), api.MaxApprovalToken)}
	}
	return nil
}

// Check is the panel's own check of a verified token's claims: it was made for this plan (the nonce), by the
// person who is signed in, with an authentication that is fresh and was a passkey. The machines check all of
// this again, and more; the panel checks it to fail early and for its audit trail.
func Check(c Claims, w Want) *Failure {
	if c.Nonce == "" || subtle.ConstantTimeCompare([]byte(c.Nonce), []byte(w.Nonce)) != 1 {
		return &Failure{ReasonNonce, "Pocket ID's answer is not for this action (another approval was started in this browser, or the answer is an old one)."}
	}
	if !w.AnyPerson && (c.Sub == "" || w.Sub == "" || subtle.ConstantTimeCompare([]byte(c.Sub), []byte(w.Sub)) != 1) {
		return &Failure{ReasonPerson, "The approval was given by another Pocket ID user than the one signed in to Fairy."}
	}
	if c.AuthTime <= 0 {
		return &Failure{ReasonNoAuth, "Pocket ID did not say when you signed in (the ID token has no auth_time), so nobody can tell that this approval is fresh."}
	}
	now := float64(w.Now.Unix())
	if c.AuthTime-now > MaxAuthAge.Seconds() {
		return &Failure{ReasonAuthTime, "Pocket ID says you signed in at a time that is still in the future: its clock and the panel's differ by more than two minutes."}
	}
	if now-c.AuthTime > MaxAuthAge.Seconds() {
		return &Failure{ReasonAuthTime, "Pocket ID did not ask for your passkey again (your last sign-in there is more than two minutes old). " +
			"The client \"Fairy PC actions\" needs \"Requires Re-Authentication\" switched on in Pocket ID."}
	}
	if !slices.Contains(c.AMR, "phr") {
		return &Failure{ReasonAMR, "Pocket ID does not say that a passkey was used for this approval (no \"phr\" in amr): the machines refuse it."}
	}
	return nil
}

// ErrNotCovered: the request that is about to be sent is not one the approval was given for.
var ErrNotCovered = errors.New("this request is not the one that was approved")

// For returns the approval to send with req to agentID: the token and the whole list. It refuses a request the
// plan does not cover, so nothing that was changed after the approval is ever sent with it.
func For(p *Plan, token, agentID string, req api.ApprovalRequest) (*api.Approval, error) {
	if p == nil || token == "" || !p.Covers(agentID, req) {
		return nil, ErrNotCovered
	}
	return &api.Approval{Token: token, Entries: p.entries}, nil
}

// refusedHint is one short sentence on what to do about the reason a machine gave for "approval refused
// (<reason>)" (the protocol's reasons). Short on purpose: the page shows the result of a kept action in a
// few lines.
func refusedHint(text string) string {
	reason := ""
	if rest, ok := strings.CutPrefix(text, "approval refused ("); ok {
		reason, _, _ = strings.Cut(rest, ")")
	}
	switch reason {
	case "user":
		return " You are not on that machine's list of people who may approve (it is changed on the machine: fairy-agent helper-trust)."
	case "clock", "auth_time", "expired":
		return " Compare the machine's clock with the real time (more than a minute off is refused), then press the button again."
	case "token_key", "token_signature":
		return " Pocket ID's signing key is not the one the machine has pinned (on the machine: fairy-agent helper-trust set --fetch-keys)."
	case "issuer", "audience":
		return " The machine trusts another Pocket ID or another client than this panel asks (on the machine: fairy-agent helper-trust show)."
	}
	return " A new approval is needed: press the button again."
}

// ErrorText is what the page says when a machine refuses with one of the approval protocol's codes: what it
// means and what to do. name is the machine, text the machine's own words. ok is false for any other code.
func ErrorText(name, code, text string) (msg string, ok bool) {
	said := ""
	if text != "" {
		said = " (" + name + " says: " + text + ")"
	}
	switch code {
	case api.CodeApprovalRequired:
		return name + " asks for a passkey approval for this now, and none was sent" + said + ". Nothing was done: press the button again.", true
	case api.CodeApprovalRefused:
		return name + " did not accept the approval" + said + ". Nothing was done." + refusedHint(text), true
	case api.CodeApprovalUnavailable:
		return name + " cannot check approvals right now" + said + ". Until that is repaired on the machine it refuses fan, power and update " +
			"requests (on the machine: fairy-agent helper-trust show; on Windows also: is the helper running?).", true
	case api.CodeApprovalDisabled:
		return name + " has nothing that checks approvals (a Windows agent without its helper)" + said + ".", true
	case api.CodeHelperTooOld:
		return "the helper on " + name + " is older than its agent (an update that has not finished): restart the helper, then try again" + said + ".", true
	}
	return "", false
}
