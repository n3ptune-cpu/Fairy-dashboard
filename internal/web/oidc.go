package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// oidcClient is one client of a Pocket ID: the issuer, the client id and secret, where Pocket ID sends the
// browser back to, and the scopes asked for. The panel has two: the sign-in client (oidc: in config.yaml) and
// the approval client "Fairy PC actions" (approval:, see approval.go). Both go through the same code: discovery,
// the authorization address, the code exchange and the verification of the ID token (exchangeCode, verifyIDToken).
// The values are read from the configuration when they are used.
type oidcClient struct {
	issuer   func() string // without a trailing slash
	clientID func() string
	scopes   func() []string
	redirect func() string // from config, never from the Host header
	secret   string
	mu       sync.Mutex
	prov     *oidc.Provider
	provFor  string // the issuer prov was fetched from
	http     *http.Client
}

func newOIDC(cfg *config.Panel, secrets config.Secrets) *oidcClient {
	return &oidcClient{secret: secrets.OIDCClientSecret, http: &http.Client{Timeout: 10 * time.Second},
		issuer:   func() string { return strings.TrimRight(cfg.OIDC.Issuer, "/") },
		clientID: func() string { return cfg.OIDC.ClientID },
		scopes:   func() []string { return cfg.OIDC.Scopes },
		redirect: func() string { return cfg.BaseURL() + "/auth/callback" }}
}

// newApprovalOIDC is the client "Fairy PC actions" (approval: in config.yaml). It asks for an ID token only:
// "profile" gives the machines a user name for their own log, nothing else is used.
func newApprovalOIDC(cfg *config.Panel, secrets config.Secrets) *oidcClient {
	return &oidcClient{secret: secrets.ApprovalClientSecret, http: &http.Client{Timeout: 10 * time.Second},
		issuer:   cfg.ApprovalIssuer,
		clientID: func() string { return cfg.Approval.ClientID },
		scopes:   func() []string { return []string{oidc.ScopeOpenID, "profile"} },
		redirect: cfg.ApprovalCallback}
}

func (o *oidcClient) ctx(parent context.Context) context.Context {
	return oidc.ClientContext(parent, o.http)
}

// provider fetches the discovery document once (retried on the next login if it failed).
func (o *oidcClient) provider(ctx context.Context) (*oidc.Provider, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	issuer := o.issuer()
	if o.prov != nil && o.provFor == issuer {
		return o.prov, nil
	}
	p, err := oidc.NewProvider(o.ctx(ctx), issuer)
	if err != nil {
		return nil, err
	}
	o.prov, o.provFor = p, issuer
	return p, nil
}

func (o *oidcClient) oauth(p *oidc.Provider) *oauth2.Config {
	ep := p.Endpoint()
	if o.secret == "" {
		ep.AuthStyle = oauth2.AuthStyleInParams // Pocket ID "public client" (PKCE only)
	}
	return &oauth2.Config{ClientID: o.clientID(), ClientSecret: o.secret, Endpoint: ep, RedirectURL: o.redirect(), Scopes: o.scopes()}
}

// exchangeCode exchanges the authorization code (with the PKCE verifier) at this client's token endpoint and
// returns the raw ID token of the answer. redact are the values that must never reach a log: it grows by the
// tokens of the answer.
func (o *oidcClient) exchangeCode(ctx context.Context, p *oidc.Provider, code, verifier string) (rawID string, tok *oauth2.Token, redact []string, lf *loginFailure) {
	redact = []string{code, verifier, o.secret}
	tok, err := o.oauth(p).Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return "", nil, redact, exchangeFailure(err, redact...)
	}
	rawID, _ = tok.Extra("id_token").(string)
	if rawID == "" {
		return "", nil, redact, &loginFailure{reason: "no id_token", attrs: []any{"error",
			"the token response has no id_token (is the openid scope requested?)"}}
	}
	return rawID, tok, append(redact, rawID, tok.AccessToken, tok.RefreshToken), nil
}

// verifyIDToken verifies a raw ID token for this client: the signature against the issuer's published keys,
// the issuer, the audience (this client's id) and the expiry.
func (o *oidcClient) verifyIDToken(ctx context.Context, p *oidc.Provider, rawID string, redact []string) (*oidc.IDToken, *loginFailure) {
	idt, err := p.Verifier(&oidc.Config{ClientID: o.clientID()}).Verify(ctx, rawID)
	if err != nil {
		return nil, &loginFailure{reason: "invalid id_token", attrs: []any{"error", safeErr(err, redact...)}}
	}
	return idt, nil
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
