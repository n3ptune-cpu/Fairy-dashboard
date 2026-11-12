// Package web is the HTTP server: the JSON API, Server-Sent Events, the embedded UI and the Pocket ID login.
//
// Everything is behind Pocket ID (OIDC Authorization Code + PKCE S256); the only other credentials are personal
// API tokens (CLI-created, Bearer). No password login of any kind.
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
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/enrollment"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notify"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/panelupdate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/settings"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/tokens"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/updates"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

//go:embed static
var staticFS embed.FS

// CSRFHeader: CSRFValue is the header every browser write must carry (it forces a CORS preflight, and no CORS
// is enabled).
const (
	CSRFHeader = "X-Requested-With"
	CSRFValue  = "fairy"
)

// CSP is the Content-Security-Policy of every answer except the /docs page itself (docsCSP). img-src allows https: so a user-chosen background
// URL can load (the backend never fetches it: no SSRF).
const CSP = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: https:; connect-src 'self'; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'"

const permissionsPolicy = "accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), " +
	"payment=(), usb=(), interest-cohort=()"

// uploadsPerMin is how many background images one user may upload per minute.
const uploadsPerMin = 6

// Server is the HTTP server: routes, middleware, sessions, OIDC client and rate limiters.
type Server struct {
	Cfg      *config.Panel
	Secrets  config.Secrets
	Poller   *poller.Poller
	Tokens   *tokens.Store
	Settings *settings.Store
	Access   *access.Store // users.json: roles, sites, site assignments
	Version  string
	Updates  updates.Source // the latest Fairy-agent release for the Updates page; nil = check off
	// PanelUpdates is the latest Fairy-dashboard release (the panel's own version is compared with it; the
	// panel is updated by hand with `fairy update`); nil = check off.
	PanelUpdates updates.Source
	checks       updateCheckState // Check for updates: once per updateCheckEvery for the whole panel
	// PanelRunner starts the panel's own update (fairy-panel-update.service); nil = not available here.
	PanelRunner panelupdate.Runner
	// ConfigCheck checks the config file this panel was started with, as the next start will ("": fine; else
	// why not, in one line). Asked before the panel's update is started; nil = not asked.
	ConfigCheck func() string
	// Add host: the managed hosts, the pending enrollments, and the managed hosts skipped at start (and why).
	Managed        *managed.Store
	Enroll         *enrollment.Manager
	ManagedSkipped []string
	// Notifications page: the events and the watcher that makes them from the poller's snapshots.
	Notes *notifications.Store
	Watch *notifications.Watcher
	// saved is closed when the watcher has written the notifications for the last time (Run, Saved).
	saved     chan struct{}
	savedOnce sync.Once
	// WakeRuns runs "Update unpowered hosts" (Updates page): one run at a time, in the background.
	WakeRuns *wakerun.Manager

	seenMu  sync.Mutex
	seen    map[string]time.Time // subject -> last "last seen" write
	seenErr atomic.Bool          // the last write of a session's last use to sessions.json failed (saveSeen)

	codec *sessionCodec
	oidc  *oidcClient
	// approval is the Pocket ID client "Fairy PC actions" (approval.go); it is used only when approval: is set.
	approval *oidcClient
	// approvalAnyPerson: the approval's "same person as the signed-in one" check is skipped. Only a test
	// build sets it (EnableTestMode, internal/web/testmode.go): it has no signed-in person. A release has no
	// code that sets it.
	approvalAnyPerson bool
	approvalKeys      issuerKeys // Pocket ID's signing keys as last fetched (api_approval.go)
	turnOnMu          sync.Mutex
	turnOnLast        *turnOnReport // how the last "Turn on" went, for the page (in memory)
	logo              *brandLogo    // branding.logo, read once at startup (nil = built-in logo)
	assetV            string        // ?v= of the /static/* URLs (assetVersion)
	index             []byte        // index.html with assetV filled in

	wall *streamSlots // downloads of the sign-in wallpaper in flight (signin_wallpaper.go)

	// demo: set ONLY by demo mode (loopback-bound); authentication is bypassed with this user.
	DemoUser     *model.User
	demoMu       sync.Mutex
	demoAuthTime time.Time
	demoView     *access.Grant // demo preview as another role (--demo-as or the switcher); nil = admin

	anonAudit  *auditThrottle // unauthenticated audit entries, per client IP
	auditErrMu sync.Mutex
	auditErrAt time.Time
	auditLost  int

	control, pollCtl, login, adminRL *rateLimiter
	approvalRL                       *rateLimiter // per user: control requests that ask machines for a challenge first
	settingsRL, uploadRL             *rateLimiter // per user: settings changes, background uploads
	stopRL                           *rateLimiter // per user: "Cancel run" and the cancel of a countdown (stopsPerMin)
	mux                              *http.ServeMux
	answerTimeout                    time.Duration  // how long a client may take over one answer (answerWriteTimeout; tests shorten it)
	backgrounds                      streamSlots    // background downloads in flight (api_settings.go)
	wallStage                        stagedPictures // sign-in pictures that wait for "Confirm it's you" (signin_wallpaper.go)
	agentReads                       *agentReads    // read requests passed through to an agent, in flight (agentreads.go)
}

// stopsPerMin is how many cancels (of an "Update unpowered hosts" run, of a shutdown countdown) one user may
// send in a minute. They have a budget of their own, apart from the control requests (control_rate_per_min):
// a cancel only stops something, and it must work right after an admin used the minute's control requests
// (r2-p2-09). Generous on purpose; every cancel is in the audit log.
const stopsPerMin = 60

// New builds the server. demoUser != nil enables demo mode (caller guarantees a loopback listener).
func New(cfg *config.Panel, secrets config.Secrets, p *poller.Poller, demoUser *model.User, version string) *Server {
	s := &Server{Cfg: cfg, Secrets: secrets, Poller: p, Tokens: tokens.NewStore(cfg.DataDir),
		Settings: settings.NewStore(cfg.DataDir), Access: access.NewStore(cfg.DataDir), Version: version,
		seen:     map[string]time.Time{},
		codec:    newCodec(secrets.SessionSecret, cfg.SessionMaxAge, !secrets.InsecureCookies),
		DemoUser: demoUser, demoAuthTime: time.Now().Add(-time.Hour), // first control action shows step-up
		control: newRateLimiter(cfg.ControlRatePerMin), pollCtl: newRateLimiter(cfg.ControlRatePerMin * 2),
		login: newRateLimiter(cfg.LoginRatePerMin), adminRL: newRateLimiter(60), anonAudit: newAuditThrottle(),
		approvalRL: newRateLimiter(cfg.ControlRatePerMin * 2),
		settingsRL: newRateLimiter(30), uploadRL: newRateLimiter(uploadsPerMin), answerTimeout: answerWriteTimeout,
		stopRL:      newRateLimiter(stopsPerMin),
		agentReads:  newAgentReads(),
		backgrounds: streamSlots{perUser: maxBackgroundsPerUser, total: maxBackgroundsTotal, open: map[string]int{}}}
	s.codec.live.account = p.Admin.SessionAccount // whose session an id is, for making room (liveSessions.prune)
	s.oidc = newOIDC(cfg, secrets)
	s.approval = newApprovalOIDC(cfg, secrets)
	s.logo = loadLogo(cfg.Branding.Logo)
	s.Managed = managed.NewStore(cfg.DataDir)
	s.Notes = notifications.Open(filepath.Join(cfg.DataDir, "notifications.json"))
	s.saved = make(chan struct{})
	s.Notes.Push = notify.Notify
	s.Watch = &notifications.Watcher{Store: s.Notes}
	addr, src, via := cfg.Enroll.PanelAddress, "config", ""
	if addr == "" {
		d := enrollment.DetectPanelAddress()
		addr, src, via = d.Addr, "auto", d.Via
	}
	// No release key here: cmd/fairy sets the real one (a build without it cannot check the install script).
	s.Enroll = enrollment.New(s.Managed, s.enrollHooks(), addr, enrollment.PinnedVerifier(cfg.Updates.AgentRepo, cfg.Enroll.AgentRelease, nil))
	s.Enroll.PanelSource, s.Enroll.PanelVia = src, via
	s.Enroll.AllowNonTailscale = cfg.Enroll.AllowNonTailscale
	wu := cfg.WakeUpdate
	if wu.OnlineTimeoutS == 0 { // a configuration that was not made by config.Parse: the defaults
		wu = config.WakeUpdate{OnlineTimeoutS: 300, SettleS: 60, ShutdownDelayS: 120, AgentRequestsPerMin: 8}
	}
	s.WakeRuns = wakerun.New(filepath.Join(cfg.DataDir, "wake-update.json"),
		wakerun.NewSettings(wu.OnlineTimeoutS, wu.SettleS, wu.ShutdownDelayS, wu.AgentRequestsPerMin), runOps{s})
	s.WakeRuns.Event = s.wakeRunEvent
	if p.OnMachine == nil { // the record of machine ids: its alarms go to the audit log and the Notifications list
		p.OnMachine = s.machineEvent
	}
	p.MachinesLoaded() // records of hosts that are gone from the configuration were dropped at the start: said once
	// A run that was going when the panel stopped is reported here, once (audit log and a notification), and
	// never continued. A journal that cannot be read is logged; no run starts until it is fixed.
	_ = s.WakeRuns.Load()
	if err := s.Access.EnsureSites(cfg.Sites()); err != nil { // sites named in config.yaml exist on the Users page
		slog.Error("users.json write failed", "error", err)
	}
	s.routes()
	return s
}

// SetDemoView makes demo mode show the panel as another role would see it ("viewer:Bob", "pending", "admin").
func (s *Server) SetDemoView(v string) error {
	g, err := access.ParseView(v)
	if err != nil {
		return err
	}
	s.demoMu.Lock()
	defer s.demoMu.Unlock()
	if g.Admin() {
		s.demoView = nil
	} else {
		s.demoView = &g
	}
	return nil
}

// warnOversizePictures says once, at start, that pictures stored before there was a limit on their size in
// pixels are larger than an upload may be now (r2-p3-02). Nothing is deleted or hidden.
func (s *Server) warnOversizePictures() {
	signIn, backgrounds := s.Settings.OversizePictures()
	if !signIn && backgrounds == 0 {
		return
	}
	slog.Warn("stored pictures are larger than an upload may be now: they are kept and shown; a browser needs 4 bytes of memory "+
		"for every pixel, so replace them with smaller ones in Settings", "sign_in_picture", signIn, "backgrounds", backgrounds,
		"max_side_px", settings.MaxPictureSide, "max_megapixels", settings.MaxPicturePixels/1_000_000)
}

// DeploymentWarnings returns startup sanity warnings (behind nginx at e.g. https://panel.example.com).
func DeploymentWarnings(cfg *config.Panel, secrets config.Secrets) []string {
	var out []string
	if u, err := url.Parse(cfg.PublicURL); err != nil || u.Scheme != "https" {
		out = append(out, "public_url "+cfg.BaseURL()+" is not https: OIDC redirects and Secure cookies need https "+
			"(set public_url to e.g. https://panel.example.com)")
	}
	if secrets.InsecureCookies {
		out = append(out, "FAIRY_INSECURE_COOKIES is set: the session cookie is NOT Secure. Local http testing only.")
	}
	out = append(out, config.EnvProblems()...) // a comment behind a value in fairy.env (names only, never a value)
	if len(cfg.OIDC.AllowedGroups) == 0 && len(cfg.OIDC.AllowedEmails) == 0 {
		out = append(out, "oidc.allowed_groups is empty: every Pocket ID user who can reach this client may sign in")
	}
	return out
}

// answerWriteTimeout is how long a client may take over one answer, counted from the answer's first byte
// (T-33). The server has no WriteTimeout: that one counts from the request, so it would cut a handler that
// waits for an agent, and the live stream. Instead every answer gets a write deadline when it starts
// (statusWriter); an answer that is not taken by then ends its connection, so a client that does not read
// cannot hold a connection, its goroutine and its buffers for as long as it likes.
//
// Two handlers set their own deadline and are left alone by this one: the live stream, before every write
// (stream.go), and the background download, which may be 8 MiB (api_settings.go).
const answerWriteTimeout = 60 * time.Second

// statusWriter is the ResponseWriter every handler gets: it knows whether an answer was started, and it sets
// the answer's write deadline.
type statusWriter struct {
	http.ResponseWriter
	wrote    bool
	timeout  time.Duration // answerWriteTimeout
	deadline bool          // this answer has its write deadline (set at its first byte, or by the handler itself)
}

// arm sets the write deadline of this answer, once. A deadline stays on the connection, so each answer sets
// its own, also one that comes after a quiet minute on the same connection.
func (w *statusWriter) arm() {
	if !w.deadline && w.timeout > 0 {
		w.deadline = true
		_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.timeout))
	}
}

func (w *statusWriter) WriteHeader(code int) {
	w.wrote = true
	w.arm()
	w.ResponseWriter.WriteHeader(code)
}
func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	w.arm()
	return w.ResponseWriter.Write(b)
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// SetWriteDeadline is what http.ResponseController calls for a handler that sets its own deadline (the live
// stream, a download). The answer's general deadline then does not replace it.
func (w *statusWriter) SetWriteDeadline(t time.Time) error {
	w.deadline = true
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(t)
}

func isLoopback(addr net.Addr) bool {
	if addr == nil {
		return false
	}
	ap, err := netip.ParseAddrPort(addr.String())
	return err == nil && ap.Addr().Unmap().IsLoopback()
}

// loopbackName reports whether a request's Host header names this machine itself: 127.0.0.1 (any 127.x), ::1 or
// localhost, with or without a port. Demo mode and the test build answer no other name.
func loopbackName(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	return err == nil && a.IsLoopback()
}

// demoOtherName sends a page that is opened under another loopback name of the demo than the one in its
// public_url (http://localhost:8090 or http://[::1]:8090 when the demo is http://127.0.0.1:8090) on to that
// one address, and reports whether it did. Such a page loaded and then failed at every change with
// "cross-origin request refused" (r2-p1-04): the write check compares the page's origin with public_url, and
// that check is not loosened. Only the opening of a page is redirected (a GET or HEAD of "/" or of a document
// the browser navigates to); API requests and files are answered as before. Without a loopback public_url
// (FAIRY_DEMO_URL names something else) there is no such rule. The test build has the same rule of its own.
func (s *Server) demoOtherName(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.URL.Path != "/" && r.Header.Get("Sec-Fetch-Dest") != "document" {
		return false
	}
	u, err := url.Parse(s.Cfg.BaseURL())
	if err != nil || u.Host == "" || hostIsOf(r.Host, s.Cfg.BaseURL()) || !loopbackName(u.Host) {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.Scheme+"://"+u.Host+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	return true
}

// Handler returns the full middleware chain.
func (s *Server) Handler() http.Handler {
	https := strings.HasPrefix(s.Cfg.PublicURL, "https://")
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: rw, timeout: s.answerTimeout}
		// A handler that returns without writing is answered by net/http after it: that write has a deadline too.
		defer sw.arm()
		var w http.ResponseWriter = sw
		// Demo serves only requests that arrived on a loopback socket, and only under a loopback name: a web
		// page elsewhere that points a name of its own at 127.0.0.1 (DNS rebinding) would otherwise read the
		// demo as if it were its own site.
		if s.DemoUser != nil {
			if la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr); !isLoopback(la) || !loopbackName(r.Host) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"detail":"demo mode only serves localhost"}`))
				return
			}
			if s.demoOtherName(w, r) {
				return
			}
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Permissions-Policy", permissionsPolicy)
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Content-Security-Policy", CSP) // every answer; the /docs page replaces it with its own (docsCSP)
		if strings.HasPrefix(r.URL.Path, "/api") || strings.HasPrefix(r.URL.Path, "/auth") {
			h.Set("Cache-Control", "no-store")
		}
		if https {
			h.Set("Strict-Transport-Security", "max-age=63072000")
		}
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
	s.assetV = assetVersion(s.Version, sub)
	s.index = renderIndex(sub, s.assetV)
	m.Handle("GET /static/", s.serveStatic(sub))
	m.HandleFunc("GET /branding/logo", s.serveLogo)
	s.wallpaperRoutes(m)
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", cacheNoCache)
		_, _ = w.Write(s.index)
	})
	s.authRoutes(m)
	s.apiRoutes(m)
	if s.Cfg.EnableDocs {
		s.docsRoutes(m)
	}
	if s.DemoUser != nil {
		s.demoRoutes()
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
	// No WriteTimeout here: every answer gets its write deadline in Handler (answerWriteTimeout).
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second,
		IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	go func() {
		defer s.savedOnce.Do(func() { close(s.saved) }) // also when the watcher panics: nobody waits for ever
		s.watch(ctx)
	}()
	go s.WakeRuns.Loop(ctx)
	go func() { // write the throttled-entries summary even when the flood stops
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				s.minutely(now)
			}
		}
	}()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// minutely is what the server does once a minute by itself: the summary of throttled audit entries, and the
// sign-in pictures that waited for a confirmation longer than a kept action lives.
func (s *Server) minutely(now time.Time) {
	s.auditSuppressed(s.anonAudit.flush(now))
	s.wallStage.sweep(s.now())
}

// Saved is closed when the server has written what it still had to write after the context of Run ended: the
// notifications of the last seconds (the watcher's last Notes.Flush). Run itself returns as soon as the
// listener is closed, and nothing waited for that write: a stop ended the process in the middle of it about
// every second time. The event was lost ("panel update started by ...", a host that went offline: after the
// restart the watcher only learns the state, so the alert is not made again), and the temporary file of the
// write stayed in the data folder (audit 5, r3-p4-04). `fairy serve` waits for this, with a limit.
func (s *Server) Saved() <-chan struct{} { return s.saved }

// watch turns the poller's snapshots into notifications (every 2 s) and saves them (every 10 s, and at the end).
func (s *Server) watch(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	seq := 0 // from the panel's start: a UPS server that did not answer the very first poll is in there (the watcher's first call takes only that)
	last := time.Now()
	s.notePanelUpdateResult()
	s.warnOversizePictures()
	vers, lastAt := map[string]string{}, map[string]float64{}
	for {
		select {
		case <-ctx.Done():
			_ = s.Notes.Flush()
			return
		case now := <-t.C:
			evs := s.Poller.UPS.EventsSince(seq)
			if len(evs) > 0 {
				seq = evs[len(evs)-1].Seq
			}
			s.Watch.Observe(now, s.Poller.Snapshot(), evs)
			s.Watch.ForgetUPS(s.upsSourceKnown) // an alert of a UPS source that was taken out of the configuration ends
			s.watchAgentUpdates(vers, lastAt)
			if now.Sub(last) >= 10*time.Second {
				last = now
				if err := s.Notes.Flush(); err != nil {
					slog.Warn("notifications: cannot save", "error", err)
				}
			}
		}
	}
}

// upsSourceKnown reports whether the panel still has the source of a UPS: a `ups:` entry of config.yaml, an
// agent (it reports the UPSes of its machine), or anything the UPS page still shows under that source.
func (s *Server) upsSourceKnown(source string) bool {
	for _, u := range s.Poller.UPS.Sources {
		if u.ID == source {
			return true
		}
	}
	if _, ok := s.Poller.AgentByID(source); ok {
		return true
	}
	for _, st := range s.Poller.UPS.Overview(false).UPS {
		if st.ID == source || strings.HasPrefix(st.ID, source+"/") {
			return true
		}
	}
	return false
}

// watchAgentUpdates makes a notification when an agent comes back with another version, or reports a failed
// self-update (rolled back).
func (s *Server) watchAgentUpdates(vers map[string]string, lastAt map[string]float64) {
	for _, a := range s.Poller.Agents() {
		info, seen := s.Poller.AgentInfo(a.ID)
		if !seen || info.Version == "" {
			continue
		}
		sys, name := a.ID, a.DisplayName()
		if x := s.Poller.RelaySystem(a.ID); x != nil {
			sys, name = x.ID, x.Name
		}
		if prev, ok := vers[a.ID]; ok && prev != info.Version {
			s.Notes.Add(notifications.Event{Kind: "update_done", Type: "update", Severity: notifications.Info, SystemID: sys,
				Host: name, Text: "agent now runs " + info.Version + " (was " + prev + ")"})
		}
		vers[a.ID] = info.Version
		last, _ := info.SelfUpdate["last"].(map[string]any)
		at, _ := last["at"].(float64)
		if at == 0 {
			continue
		}
		if prev, ok := lastAt[a.ID]; ok && at > prev {
			if okv, _ := last["ok"].(bool); !okv {
				msg, _ := last["message"].(string)
				s.Notes.Add(notifications.Event{Kind: "update_failed", Type: "update", Severity: notifications.Warning,
					SystemID: sys, Host: name, Text: "agent update failed: " + msg})
			}
		}
		lastAt[a.ID] = at
	}
}
