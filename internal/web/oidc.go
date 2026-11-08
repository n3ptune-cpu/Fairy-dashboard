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

	"github.com/n3ptune-cpu/Fairy/internal/config"
)

type oidcClient struct {
	cfg    *config.Panel
	secret string
	mu     sync.Mutex
	prov   *oidc.Provider
	http   *http.Client
}

func newOIDC(cfg *config.Panel, secrets config.Secrets) *oidcClient {
	return &oidcClient{cfg: cfg, secret: secrets.OIDCClientSecret, http: &http.Client{Timeout: 10 * time.Second}}
}

func (o *oidcClient) ctx(parent context.Context) context.Context {
	return oidc.ClientContext(parent, o.http)
}

// provider fetches the discovery document once (retried on the next login if it failed).
func (o *oidcClient) provider(ctx context.Context) (*oidc.Provider, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.prov != nil {
		return o.prov, nil
	}
	p, err := oidc.NewProvider(o.ctx(ctx), strings.TrimRight(o.cfg.OIDC.Issuer, "/"))
	if err != nil {
		return nil, err
	}
	o.prov = p
	return p, nil
}

func (o *oidcClient) oauth(p *oidc.Provider) *oauth2.Config {
	ep := p.Endpoint()
	if o.secret == "" {
		ep.AuthStyle = oauth2.AuthStyleInParams // Pocket ID "public client" (PKCE only)
	}
	return &oauth2.Config{ClientID: o.cfg.OIDC.ClientID, ClientSecret: o.secret, Endpoint: ep,
		RedirectURL: o.cfg.BaseURL() + "/auth/callback", // from config, never from the Host header
		Scopes:      o.cfg.OIDC.Scopes}
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
