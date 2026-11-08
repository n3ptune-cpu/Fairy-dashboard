// Package web: the HTTP server. Everything is behind Pocket ID (OIDC Authorization Code + PKCE S256); the only
// other credentials are personal API tokens (CLI-created, Bearer). No password login of any kind.
package web

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy/internal/config"
	"github.com/n3ptune-cpu/Fairy/internal/model"
	"github.com/n3ptune-cpu/Fairy/internal/poller"
	"github.com/n3ptune-cpu/Fairy/internal/settings"
	"github.com/n3ptune-cpu/Fairy/internal/tokens"
)

//go:embed static
var staticFS embed.FS

const (
	CSRFHeader = "X-Requested-With"
	CSRFValue  = "fairy"
)

// CSP: img-src allows https: so a user-chosen background URL can load (the backend never fetches it: no SSRF).
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: https:; connect-src 'self'; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'"

const permissionsPolicy = "accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), " +
	"payment=(), usb=(), interest-cohort=()"

type Server struct {
	Cfg      *config.Panel
	Secrets  config.Secrets
	Poller   *poller.Poller
	Tokens   *tokens.Store
	Settings *settings.Store
	Version  string

	codec *sessionCodec
	oidc  *oidcClient

	// demo: set ONLY by demo mode (loopback-bound); authentication is bypassed with this user.
	DemoUser     *model.User
	demoMu       sync.Mutex
	demoAuthTime time.Time

	control, pollCtl, login *rateLimiter
	mux                     *http.ServeMux
}

// New builds the server. demoUser != nil enables demo mode (caller guarantees a loopback listener).
func New(cfg *config.Panel, secrets config.Secrets, p *poller.Poller, demoUser *model.User, version string) *Server {
	s := &Server{Cfg: cfg, Secrets: secrets, Poller: p, Tokens: tokens.NewStore(cfg.DataDir),
		Settings: settings.NewStore(cfg.DataDir), Version: version,
		codec:    newCodec(secrets.SessionSecret, cfg.SessionMaxAge, !secrets.InsecureCookies),
		DemoUser: demoUser, demoAuthTime: time.Now().Add(-time.Hour), // first control action shows step-up
		control: newRateLimiter(cfg.ControlRatePerMin), pollCtl: newRateLimiter(cfg.ControlRatePerMin * 2),
		login: newRateLimiter(cfg.LoginRatePerMin)}
	s.oidc = newOIDC(cfg, secrets)
	s.routes()
	return s
}

// DeploymentWarnings: startup sanity checks (behind nginx at e.g. https://panel.example.com).
func DeploymentWarnings(cfg *config.Panel, secrets config.Secrets) []string {
	var out []string
	if u, err := url.Parse(cfg.PublicURL); err != nil || u.Scheme != "https" {
		out = append(out, "public_url "+cfg.BaseURL()+" is not https: OIDC redirects and Secure cookies need https "+
			"(set public_url to e.g. https://panel.example.com)")
	}
	if secrets.InsecureCookies {
		out = append(out, "FAIRY_INSECURE_COOKIES is set: the session cookie is NOT Secure. Local http testing only.")
	}
	if len(cfg.OIDC.AllowedGroups) == 0 && len(cfg.OIDC.AllowedEmails) == 0 {
		out = append(out, "oidc.allowed_groups is empty: every Pocket ID user who can reach this client may sign in")
	}
	return out
}

type statusWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *statusWriter) WriteHeader(code int) { w.wrote = true; w.ResponseWriter.WriteHeader(code) }
func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func isLoopback(addr net.Addr) bool {
	if addr == nil {
		return false
	}
	ap, err := netip.ParseAddrPort(addr.String())
	return err == nil && ap.Addr().Unmap().IsLoopback()
}

// Handler returns the full middleware chain.
func (s *Server) Handler() http.Handler {
	https := strings.HasPrefix(s.Cfg.PublicURL, "https://")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.DemoUser != nil { // demo serves only requests that arrived on a loopback socket
			if la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr); !isLoopback(la) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"detail":"demo mode only serves localhost"}`))
				return
			}
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", permissionsPolicy)
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/docs") { // Swagger UI loads its assets from a CDN
			h.Set("Content-Security-Policy", CSP)
		}
		if strings.HasPrefix(r.URL.Path, "/api") || strings.HasPrefix(r.URL.Path, "/auth") {
			h.Set("Cache-Control", "no-store")
		}
		if https {
			h.Set("Strict-Transport-Security", "max-age=63072000")
		}
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				slog.Error("panic", "method", r.Method, "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
				if !sw.wrote {
					sw.Header().Set("Content-Type", "application/json")
					sw.WriteHeader(500)
					_, _ = sw.Write([]byte(`{"detail":"internal error"}`))
				}
			}
		}()
		s.mux.ServeHTTP(sw, r)
	})
}

func (s *Server) routes() {
	m := http.NewServeMux()
	s.mux = m
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(sub)
	m.Handle("GET /static/", http.StripPrefix("/static/", files))
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := fs.ReadFile(sub, "index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	})
	s.authRoutes(m)
	s.apiRoutes(m)
	if s.Cfg.EnableDocs {
		s.docsRoutes(m)
	}
	notFound := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"detail":"Not Found"}`))
	}
	m.HandleFunc("/", notFound)
}

// Run serves on addr until ctx ends.
func (s *Server) Run(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second,
		IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
