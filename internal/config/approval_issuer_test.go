package config

import "testing"

// A machine takes an https issuer only (audit 6, r4-p3-09): the panel says so wherever it says "ready".
func TestApprovalIssuerProblem(t *testing.T) {
	for _, c := range []struct {
		approval, oidc string
		client         string
		want           string
	}{
		{"https://id.example.test", "", "pc-actions", ""},
		{"https://id.example.test:1411/", "", "pc-actions", ""},
		{"HTTPS://id.example.test", "", "pc-actions", ""}, // a scheme has no case, for the machine's check either
		{"http://id.lan:1411", "", "pc-actions", ApprovalIssuerMustBeHTTPS},
		{"http://id.lan:1411", "https://id.example.test", "pc-actions", ApprovalIssuerMustBeHTTPS}, // approval.issuer counts, not oidc.issuer
		{"", "http://id.lan:1411", "pc-actions", ApprovalIssuerMustBeHTTPS},                        // taken from oidc.issuer
		{"", "https://id.example.test", "pc-actions", ""},
		{"https://", "", "pc-actions", ApprovalIssuerMustBeHTTPS},
		{"http://id.lan:1411", "", "", ""}, // no approval client: the block does nothing, and nothing is said
	} {
		p := &Panel{}
		p.Approval.Issuer, p.Approval.ClientID, p.OIDC.Issuer = c.approval, c.client, c.oidc
		if got := p.ApprovalIssuerProblem(); got != c.want {
			t.Errorf("approval.issuer %q, oidc.issuer %q, client %q: %q, want %q", c.approval, c.oidc, c.client, got, c.want)
		}
	}
	if ApprovalIssuerMustBeHTTPS != "the issuer must be https" {
		t.Error("the short reason changed: the browser runs and the guides name it")
	}
}
