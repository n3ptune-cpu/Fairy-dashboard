package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/configutil"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
)

var (
	envNameRe    = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	releaseTagRe = regexp.MustCompile(`^build-[0-9]{1,9}$`)
	prefixRe     = regexp.MustCompile(`^([a-z0-9][a-z0-9-]*-)?$`)
	tokenIDRe    = regexp.MustCompile(`^[^@\s]+@[^!\s]+![A-Za-z0-9_.-]+$`)
	macRe        = regexp.MustCompile(`^[0-9A-Fa-f]{2}([:-][0-9A-Fa-f]{2}){5}$|^[0-9A-Fa-f]{12}$`)
	bmcTargetRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	hostnameRe   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	clientIDRe   = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)
	siteRe       = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._'&-]{0,39}$`)
)

// ValidSite reports whether s is a valid site name: 1-40 letters, digits, spaces and . _ ' & -, starting with a
// letter or digit (no leading or trailing space).
func ValidSite(s string) bool { return siteRe.MatchString(s) && strings.TrimSpace(s) == s }

// problems collects every validation error so the user sees all of them at once.
type problems []string

func (p *problems) add(f string, a ...any) { *p = append(*p, fmt.Sprintf(f, a...)) }

// Validate applies defaults and checks the same rules as the prototype. All problems are reported together.
func (c *Panel) Validate() error {
	c.applyDefaults()
	var errs problems
	c.validateGeneral(&errs)
	c.validateApproval(&errs)
	c.parseTrustedProxies(&errs)
	c.validateProxmox(&errs)
	agents := c.validateAgents(&errs)
	c.validateSystems(&errs, agents)
	c.validateSites(&errs)
	for _, r := range []string{c.Updates.AgentRepo, c.Updates.PanelRepo} {
		if !release.ValidRepo(r) {
			errs.add("updates: %q is not a GitHub repository name like %s (not a URL)", r, release.AgentRepo)
		}
	}
	if a := c.Enroll.PanelAddress; a != "" && (net.ParseIP(a) == nil || net.ParseIP(a).To4() == nil) {
		errs.add("enroll.panel_address must be the panel's Tailscale IPv4 address, e.g. 100.64.0.10")
	}
	if t := c.Enroll.AgentRelease; t != "" && !releaseTagRe.MatchString(t) {
		errs.add("enroll.agent_release must be a Fairy-agent release tag like build-7")
	}
	for _, h := range c.PanelHosts {
		if !configutil.ValidID(h) {
			errs.add("panel_hosts: invalid id %q", h)
		}
	}
	if len(errs) > 0 {
		return errors.New("config invalid:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

// DefaultControlRatePerMin is how many control requests one admin may send in a minute when config.yaml does
// not say (control_rate_per_min). It was 6, which a person met by ordinary use: switching seven machines on
// for "Update unpowered hosts" in a row is seven requests. What an AGENT accepts in a minute (12, its own
// control_rate_per_min) and what a run sends to one agent (8, wake_update.agent_requests_per_min) are other
// limits and did not change.
const DefaultControlRatePerMin = 20

// applyDefaults fills in every top-level setting that was left out (zero).
func (c *Panel) applyDefaults() {
	def := func(v *float64, d float64) {
		if *v == 0 {
			*v = d
		}
	}
	defi := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	def(&c.Intervals.Agent, 1)
	def(&c.Intervals.Proxmox, 2)
	def(&c.Intervals.UPS, 2)
	def(&c.Intervals.History, 5)
	def(&c.Intervals.Stream, 1)
	def(&c.RequestTimeout, 4)
	def(&c.OfflineAfter, 60)
	if c.SessionMaxAge == 0 {
		c.SessionMaxAge = DefaultSessionMaxAge
	}
	if c.SessionIdleTimeout == 0 { // 4 h, or the whole session when that is shorter
		c.SessionIdleTimeout = min(DefaultSessionIdleTimeout, c.SessionMaxAge)
	}
	defi(&c.PowerHistoryS, 3600)
	defi(&c.ControlRatePerMin, DefaultControlRatePerMin)
	defi(&c.StepUpWindowS, 180)
	defi(&c.LoginRatePerMin, 20)
	if c.Updates.AgentRepo == "" {
		c.Updates.AgentRepo = release.AgentRepo
	}
	if c.Updates.PanelRepo == "" {
		c.Updates.PanelRepo = release.PanelRepo
	}
	defi(&c.WakeUpdate.OnlineTimeoutS, 300)
	defi(&c.WakeUpdate.SettleS, 60)
	defi(&c.WakeUpdate.ShutdownDelayS, 120)
	defi(&c.WakeUpdate.AgentRequestsPerMin, 8)
	defi(&c.Approval.RunApprovalWaitS, 900)
	defi(&c.AuditMaxMB, 100)
	defi(&c.AuditKeep, 5)
	if c.DataDir == "" {
		if runtime.GOOS == "windows" {
			c.DataDir = `C:\ProgramData\Fairy\data`
		} else {
			c.DataDir = "/var/lib/fairy"
		}
	}
	if c.AuditLog == "" {
		c.AuditLog = filepath.Join(c.DataDir, "audit.log")
	}
	if len(c.OIDC.Scopes) == 0 {
		c.OIDC.Scopes = []string{"openid", "profile", "email", "groups"}
	}
	if c.OIDC.GroupsClaim == "" {
		c.OIDC.GroupsClaim = "groups"
	}
	if c.TrustedProxies == nil {
		c.TrustedProxies = []string{"127.0.0.1/32", "::1/128"}
	}
}

// validateGeneral checks intervals, URLs, the OIDC client and the session / step-up timings.
func (c *Panel) validateGeneral(errs *problems) {
	for name, v := range map[string]float64{"agent": c.Intervals.Agent, "proxmox": c.Intervals.Proxmox, "ups": c.Intervals.UPS, "history": c.Intervals.History, "stream": c.Intervals.Stream} {
		if !inRange(v, 1, 300) {
			errs.add("intervals.%s must be 1-300 s", name)
		}
	}
	if !inRange(c.RequestTimeout, 0.1, 300) {
		errs.add("request_timeout must be 0.1-300 s")
	}
	if !inRange(c.OfflineAfter, 1, 7*86400) {
		errs.add("offline_after must be 1-604800 s")
	}
	if c.Listen != "" {
		if _, port, err := net.SplitHostPort(c.Listen); err != nil || port == "" {
			errs.add("listen must be host:port, e.g. 10.0.0.20:8080")
		}
	}
	if !isHTTPURL(c.PublicURL) {
		errs.add("public_url must be an http(s) URL")
	}
	if !isHTTPURL(c.OIDC.Issuer) {
		errs.add("oidc.issuer must be an http(s) URL")
	}
	if c.OIDC.ClientID == "" {
		errs.add("oidc.client_id is required")
	}
	if c.SessionMaxAge < 300 {
		errs.add("session_max_age must be at least 300 s (5m), got %s", c.SessionMaxAge)
	}
	// 0 was replaced by the default above. A negative or tiny idle timeout would sign everyone out at once.
	if c.SessionIdleTimeout < 60 {
		errs.add("session_idle_timeout must be at least 60 s (1m), got %s", c.SessionIdleTimeout)
	}
	if c.SessionIdleTimeout > c.SessionMaxAge {
		errs.add("session_idle_timeout (%s) must not be longer than session_max_age (%s)", c.SessionIdleTimeout, c.SessionMaxAge)
	}
	if c.StepUpWindowS < 30 || c.StepUpWindowS > 86400 {
		errs.add("step_up_window_s must be 30-86400")
	}
	if c.AuditMaxMB < -1 || c.AuditKeep < 1 || c.AuditKeep > 100 {
		errs.add("audit_max_mb must be -1 (no rotation) or a size in MiB, audit_keep 1-100")
	}
	w := c.WakeUpdate // 0 was replaced by the default above
	for _, f := range []struct {
		name   string
		v      int
		lo, hi int
	}{{"online_timeout_s", w.OnlineTimeoutS, 60, 1800}, {"settle_s", w.SettleS, 10, 600},
		{"shutdown_delay_s", w.ShutdownDelayS, 10, 3600}, {"agent_requests_per_min", w.AgentRequestsPerMin, 1, 30}} {
		if f.v < f.lo || f.v > f.hi {
			errs.add("wake_update.%s must be %d-%d", f.name, f.lo, f.hi)
		}
	}
}

// MaxApprovalUsers is how many users a machine's list of approvers takes (Fairy-agent: api.MaxTrustUsers).
const MaxApprovalUsers = 32

// ValidApprovalUser reports whether s can be a Pocket ID user id in a machine's list: 1-255 printable ASCII
// characters without a space (the machines refuse anything else).
func ValidApprovalUser(s string) bool {
	if len(s) == 0 || len(s) > 255 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// validateApproval checks the approval: block. An empty block is fine: the feature is then off.
func (c *Panel) validateApproval(errs *problems) {
	a := c.Approval
	if a.RunApprovalWaitS < 60 || a.RunApprovalWaitS > 3600 { // 0 was replaced by the default
		errs.add("approval.run_approval_wait_s must be 60-3600")
	}
	if a.ClientID == "" {
		if a.Issuer != "" || len(a.Users) > 0 {
			errs.add("approval: client_id is missing (the client id of \"Fairy PC actions\" in Pocket ID); without it the block does nothing")
		}
		return
	}
	if !clientIDRe.MatchString(a.ClientID) {
		errs.add("approval.client_id must be 1-128 characters of A-Z a-z 0-9 . _ ~ - (copy it from Pocket ID)")
	}
	if a.ClientID == c.OIDC.ClientID && c.ApprovalIssuer() == strings.TrimRight(c.OIDC.Issuer, "/") {
		errs.add("approval.client_id is the sign-in client (oidc.client_id): approvals need a client of their own in Pocket ID (\"Fairy PC actions\")")
	}
	if a.Issuer != "" && !isHTTPURL(a.Issuer) {
		errs.add("approval.issuer must be an http(s) URL (leave it out to use oidc.issuer)")
	}
	if len(a.Users) > MaxApprovalUsers {
		errs.add("approval.users: at most %d", MaxApprovalUsers)
	}
	seen := map[string]bool{}
	for _, u := range a.Users {
		switch {
		case !ValidApprovalUser(u):
			errs.add("approval.users: %q is not a Pocket ID user id (1-255 characters, no spaces)", u)
		case seen[u]:
			errs.add("approval.users: %q is listed twice", u)
		}
		seen[u] = true
	}
}

// inRange reports whether v is a number from lo to hi. Written as "inside", not as "below or above": every
// comparison with NaN is false, so `v < lo || v > hi` lets .nan through (and .inf must not pass either).
func inRange(v, lo, hi float64) bool { return v >= lo && v <= hi }

// isHTTPURL reports whether s is an absolute http:// or https:// URL with a host.
func isHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// parseTrustedProxies turns trusted_proxies (addresses or networks) into prefixes for Trusted.
func (c *Panel) parseTrustedProxies(errs *problems) {
	c.trusted = nil
	for _, t := range c.TrustedProxies {
		p, err := netip.ParsePrefix(t)
		if err != nil {
			a, err2 := netip.ParseAddr(t)
			if err2 != nil {
				errs.add("trusted_proxies: %q is not an address or network", t)
				continue
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		c.trusted = append(c.trusted, p.Masked())
	}
}

// validateProxmox applies the per-cluster defaults (id "pve", cluster NSX-TEYVAT, prefix "<id>-") and checks
// ids, URLs and the read-only / power token pair.
func (c *Panel) validateProxmox(errs *problems) {
	ids, prefixes := map[string]bool{}, map[string]bool{}
	for i := range c.Proxmox {
		p := &c.Proxmox[i]
		if p.ID == "" {
			p.ID = "pve"
		}
		if p.ClusterName == "" {
			p.ClusterName = "NSX-TEYVAT"
		}
		if p.IDPrefix == nil {
			v := p.ID + "-"
			p.IDPrefix = &v
		}
		if !configutil.ValidID(p.ID) || !prefixRe.MatchString(*p.IDPrefix) {
			errs.add("proxmox %q: invalid id or id_prefix", p.ID)
		}
		if ids[p.ID] || prefixes[*p.IDPrefix] {
			errs.add("proxmox entries need unique `id` and `id_prefix`")
		}
		ids[p.ID], prefixes[*p.IDPrefix] = true, true
		if len(p.URLs) == 0 {
			errs.add("proxmox %s: urls is required", p.ID)
		}
		if !tokenIDRe.MatchString(p.TokenID) {
			errs.add("proxmox %s: token_id must look like USER@REALM!TOKENID", p.ID)
		}
		if p.TokenSecretEnv != "" && !envNameRe.MatchString(p.TokenSecretEnv) {
			errs.add("proxmox %s: token_secret_env must be an env var name", p.ID)
		}
		if p.Managed && len(p.TokenSecret) < 16 {
			errs.add("proxmox %s: the token secret from Add host is missing (Re-pair the host)", p.ID)
		}
		if p.TLSServerName != "" && !hostnameRe.MatchString(p.TLSServerName) {
			errs.add("proxmox %s: tls_server_name must be a host name", p.ID)
		}
		if (p.PowerTokenID == "") != (p.PowerTokenSecretEnv == "") {
			errs.add("proxmox %s: power_token_id and power_token_secret_env go together", p.ID)
		}
		if p.PowerTokenID != "" && p.PowerTokenID == p.TokenID {
			errs.add("proxmox %s: power_token_id must be a different token from the read-only token_id", p.ID)
		}
		if p.PowerTokenID != "" && !tokenIDRe.MatchString(p.PowerTokenID) {
			errs.add("proxmox %s: power_token_id must look like USER@REALM!TOKENID", p.ID)
		}
		if p.NodePower && p.PowerTokenID == "" {
			errs.add("proxmox %s: node_power needs power_token_id", p.ID)
		}
	}
}

// validateAgents checks every agent entry and returns them by id (for the wol.relay check).
func (c *Panel) validateAgents(errs *problems) map[string]*Agent {
	agents := map[string]*Agent{}
	for i := range c.Agents {
		a := &c.Agents[i]
		if !configutil.ValidID(a.ID) {
			errs.add("agent %q: invalid id", a.ID)
		}
		if strings.HasPrefix(a.ID, "node-") || strings.HasPrefix(a.ID, "qemu-") || strings.HasPrefix(a.ID, "lxc-") {
			errs.add("agent ids must not start with node-/qemu-/lxc- (reserved for Proxmox)")
		}
		if agents[a.ID] != nil {
			errs.add("duplicate agent id %s", a.ID)
		}
		agents[a.ID] = a
		if u, err := url.Parse(a.URL); err != nil || u.Host == "" {
			errs.add("agent %s: url must be http(s)://host:port", a.ID)
		}
		if !a.Managed && !envNameRe.MatchString(a.TokenEnv) {
			errs.add("agent %s: token_env must be an env var name", a.ID)
		}
		if a.ControlTokenEnv != "" && !envNameRe.MatchString(a.ControlTokenEnv) {
			errs.add("agent %s: control_token_env must be an env var name", a.ID)
		}
		if a.FanControl != nil && (a.FanControl.Vendor != "supermicro" || !envNameRe.MatchString(a.FanControl.ControlTokenEnv)) {
			errs.add("agent %s: fan_control needs vendor: supermicro and control_token_env", a.ID)
		}
		if (a.PowerControl || a.WolRelay || a.SelfUpdate) && !a.HasControl() {
			errs.add("agent %s: power_control / wol_relay / self_update need control_token_env", a.ID)
		}
		if a.DWServiceURL != "" && !strings.HasPrefix(a.DWServiceURL, "https://") {
			errs.add("agent %s: dwservice_url must use https://", a.ID)
		}
	}
	return agents
}

// validateSystems checks the `systems:` overrides, including Wake-on-LAN (default port 9, relay agent).
func (c *Panel) validateSystems(errs *problems, agents map[string]*Agent) {
	for _, o := range c.Systems {
		if !configutil.ValidID(o.ID) {
			errs.add("systems: invalid id %q", o.ID)
		}
		if o.DWServiceURL != "" && !strings.HasPrefix(o.DWServiceURL, "https://") {
			errs.add("system %s: dwservice_url must use https://", o.ID)
		}
		if b := o.BMCPower; b != nil {
			if !bmcTargetRe.MatchString(b.Target) {
				errs.add("system %s: bmc_power.target must be the id of a target in the relay agent's bmc_relay (a-z, 0-9, -)", o.ID)
			}
			if ag := agents[b.Relay]; ag == nil || !ag.HasControl() {
				errs.add("system %s: bmc_power.relay %q must be an agent with control_token_env", o.ID, b.Relay)
			} else if b.Relay == o.ID {
				errs.add("system %s: bmc_power.relay cannot be the machine itself", o.ID)
			}
		}
		if o.Wol == nil {
			continue
		}
		w := o.Wol
		if len(w.AllMACs()) == 0 {
			errs.add("system %s: wol needs mac (or macs) like aa:bb:cc:dd:ee:ff", o.ID)
		}
		for _, m := range append([]string{w.MAC}, w.MACs...) {
			if m != "" && !macRe.MatchString(m) {
				errs.add("system %s: wol MAC %q must look like aa:bb:cc:dd:ee:ff", o.ID, m)
			}
		}
		if len(w.MACs) > 8 {
			errs.add("system %s: wol.macs: at most 8", o.ID)
		}
		if w.Port == 0 {
			w.Port = 9
		}
		if w.Port != 7 && w.Port != 9 {
			errs.add("system %s: wol.port must be 7 or 9", o.ID)
		}
		if w.Broadcast != "" && net.ParseIP(w.Broadcast).To4() == nil {
			errs.add("system %s: wol.broadcast must be an IPv4 address", o.ID)
		}
		if ag := agents[w.Relay]; ag == nil || !ag.WolRelay {
			errs.add("system %s: wol.relay %q must be an agent with wol_relay: true", o.ID, w.Relay)
		}
	}
}

// validateSites checks the optional `site:` defaults and oidc.bootstrap_admins.
func (c *Panel) validateSites(errs *problems) {
	check := func(where, site string) {
		if site != "" && !ValidSite(site) {
			errs.add("%s: invalid site %q (1-40 letters, digits, spaces and . _ ' & -)", where, site)
		}
	}
	for _, p := range c.Proxmox {
		check("proxmox "+p.ID, p.Site)
	}
	for _, a := range c.Agents {
		check("agent "+a.ID, a.Site)
	}
	for _, o := range c.Systems {
		check("system "+o.ID, o.Site)
	}
	for _, e := range c.OIDC.BootstrapAdmins {
		if !strings.Contains(e, "@") {
			errs.add("oidc.bootstrap_admins: %q is not an email address", e)
		}
	}
}

// Sites returns every site named in the config (`site:` defaults), sorted, without duplicates.
func (c *Panel) Sites() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, p := range c.Proxmox {
		add(p.Site)
	}
	for _, a := range c.Agents {
		add(a.Site)
	}
	for _, o := range c.Systems {
		add(o.Site)
	}
	sort.Strings(out)
	return out
}
