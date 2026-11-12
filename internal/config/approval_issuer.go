package config

import "net/url"

// ApprovalIssuerMustBeHTTPS is the short reason no machine is ready to be switched on while the approval issuer
// is not an https address (the Approval dialog, the Updates page, the config check and the log at start).
const ApprovalIssuerMustBeHTTPS = "the issuer must be https"

// ApprovalIssuerProblem is "" or ApprovalIssuerMustBeHTTPS: a machine takes an https issuer and nothing else
// (the agent's own check of what it is told to trust), so with approval.issuer (or the oidc.issuer it falls back
// to) at an http:// address the check cannot be switched on for any machine. The panel still starts: its
// sign-in and the machines that already check are not touched by it (audit 6, r4-p3-09: the dialog said
// "Ready" and Turn on then failed).
func (c *Panel) ApprovalIssuerProblem() string {
	if !c.ApprovalOn() {
		return ""
	}
	if u, err := url.Parse(c.ApprovalIssuer()); err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return ApprovalIssuerMustBeHTTPS
	}
	return ""
}
