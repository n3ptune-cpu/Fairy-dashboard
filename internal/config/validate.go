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
	"strings"
)

var (
	systemIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	envNameRe  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	prefixRe   = regexp.MustCompile(`^([a-z0-9][a-z0-9-]*-)?$`)
	tokenIDRe  = regexp.MustCompile(`^[^@\s]+@[^!\s]+![A-Za-z0-9_.-]+$`)
	macRe      = regexp.MustCompile(`^[0-9A-Fa-f]{2}([:-][0-9A-Fa-f]{2}){5}$|^[0-9A-Fa-f]{12}$`)
)

func validID(s string) bool { return systemIDRe.MatchString(s) }

// Validate applies defaults and checks the same rules as the prototype.
func (c *Panel) Validate() error {
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
	defi(&c.SessionMaxAge, 7*86400)
	defi(&c.SessionIdleTimeout, 12*3600)
	defi(&c.PowerHistoryS, 3600)
	defi(&c.ControlRatePerMin, 6)
	defi(&c.StepUpWindowS, 300)
	defi(&c.LoginRatePerMin, 20)
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
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }
	for name, v := range map[string]float64{"agent": c.Intervals.Agent, "proxmox": c.Intervals.Proxmox, "ups": c.Intervals.UPS, "history": c.Intervals.History, "stream": c.Intervals.Stream} {
		if v < 1 || v > 300 {
			add("intervals.%s must be 1-300 s", name)
		}
	}
	if u, err := url.Parse(c.PublicURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		add("public_url must be an http(s) URL")
	}
	if u, err := url.Parse(c.OIDC.Issuer); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		add("oidc.issuer must be an http(s) URL")
	}
	if c.OIDC.ClientID == "" {
		add("oidc.client_id is required")
	}
	if c.SessionMaxAge < 300 {
		add("session_max_age must be >= 300")
	}
	if c.SessionIdleTimeout > c.SessionMaxAge {
		add("session_idle_timeout must not exceed session_max_age")
	}
	if c.StepUpWindowS < 30 || c.StepUpWindowS > 86400 {
		add("step_up_window_s must be 30-86400")
	}
	c.trusted = nil
	for _, t := range c.TrustedProxies {
		p, err := netip.ParsePrefix(t)
		if err != nil {
			a, err2 := netip.ParseAddr(t)
			if err2 != nil {
				add("trusted_proxies: %q is not an address or network", t)
				continue
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		c.trusted = append(c.trusted, p.Masked())
	}
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
		if !validID(p.ID) || !prefixRe.MatchString(*p.IDPrefix) {
			add("proxmox %q: invalid id or id_prefix", p.ID)
		}
		if ids[p.ID] || prefixes[*p.IDPrefix] {
			add("proxmox entries need unique `id` and `id_prefix`")
		}
		ids[p.ID], prefixes[*p.IDPrefix] = true, true
		if len(p.URLs) == 0 {
			add("proxmox %s: urls is required", p.ID)
		}
		if !tokenIDRe.MatchString(p.TokenID) {
			add("proxmox %s: token_id must look like USER@REALM!TOKENID", p.ID)
		}
		if p.TokenSecretEnv != "" && !envNameRe.MatchString(p.TokenSecretEnv) {
			add("proxmox %s: token_secret_env must be an env var name", p.ID)
		}
		if (p.PowerTokenID == "") != (p.PowerTokenSecretEnv == "") {
			add("proxmox %s: power_token_id and power_token_secret_env go together", p.ID)
		}
		if p.PowerTokenID != "" && p.PowerTokenID == p.TokenID {
			add("proxmox %s: power_token_id must be a different token from the read-only token_id", p.ID)
		}
		if p.PowerTokenID != "" && !tokenIDRe.MatchString(p.PowerTokenID) {
			add("proxmox %s: power_token_id must look like USER@REALM!TOKENID", p.ID)
		}
		if p.NodePower && p.PowerTokenID == "" {
			add("proxmox %s: node_power needs power_token_id", p.ID)
		}
	}
	agents := map[string]*Agent{}
	for i := range c.Agents {
		a := &c.Agents[i]
		if !validID(a.ID) {
			add("agent %q: invalid id", a.ID)
		}
		if strings.HasPrefix(a.ID, "node-") || strings.HasPrefix(a.ID, "qemu-") || strings.HasPrefix(a.ID, "lxc-") {
			add("agent ids must not start with node-/qemu-/lxc- (reserved for Proxmox)")
		}
		if agents[a.ID] != nil {
			add("duplicate agent id %s", a.ID)
		}
		agents[a.ID] = a
		if u, err := url.Parse(a.URL); err != nil || u.Host == "" {
			add("agent %s: url must be http(s)://host:port", a.ID)
		}
		if !envNameRe.MatchString(a.TokenEnv) {
			add("agent %s: token_env must be an env var name", a.ID)
		}
		if a.ControlTokenEnv != "" && !envNameRe.MatchString(a.ControlTokenEnv) {
			add("agent %s: control_token_env must be an env var name", a.ID)
		}
		if a.FanControl != nil && (a.FanControl.Vendor != "supermicro" || !envNameRe.MatchString(a.FanControl.ControlTokenEnv)) {
			add("agent %s: fan_control needs vendor: supermicro and control_token_env", a.ID)
		}
		if (a.PowerControl || a.WolRelay) && a.ControlEnv() == "" {
			add("agent %s: power_control / wol_relay need control_token_env", a.ID)
		}
		if a.DWServiceURL != "" && !strings.HasPrefix(a.DWServiceURL, "https://") {
			add("agent %s: dwservice_url must use https://", a.ID)
		}
	}
	for _, o := range c.Systems {
		if !validID(o.ID) {
			add("systems: invalid id %q", o.ID)
		}
		if o.DWServiceURL != "" && !strings.HasPrefix(o.DWServiceURL, "https://") {
			add("system %s: dwservice_url must use https://", o.ID)
		}
		if o.Wol != nil {
			w := o.Wol
			if !macRe.MatchString(w.MAC) {
				add("system %s: wol.mac must look like aa:bb:cc:dd:ee:ff", o.ID)
			}
			if w.Port == 0 {
				w.Port = 9
			}
			if w.Port != 7 && w.Port != 9 {
				add("system %s: wol.port must be 7 or 9", o.ID)
			}
			if w.Broadcast != "" && net.ParseIP(w.Broadcast).To4() == nil {
				add("system %s: wol.broadcast must be an IPv4 address", o.ID)
			}
			if ag := agents[w.Relay]; ag == nil || !ag.WolRelay {
				add("system %s: wol.relay %q must be an agent with wol_relay: true", o.ID, w.Relay)
			}
		}
	}
	for _, h := range c.PanelHosts {
		if !validID(h) {
			add("panel_hosts: invalid id %q", h)
		}
	}
	if len(errs) > 0 {
		return errors.New("config invalid:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}
