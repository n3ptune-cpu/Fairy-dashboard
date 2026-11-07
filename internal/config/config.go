// Package config: non-secret settings from /etc/fairy/config.yaml, secrets from the environment
// (/etc/fairy/fairy.env is loaded at startup without overriding variables that are already set).
package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	systemIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	envNameRe  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	prefixRe   = regexp.MustCompile(`^([a-z0-9][a-z0-9-]*-)?$`)
	tokenIDRe  = regexp.MustCompile(`^[^@\s]+@[^!\s]+![A-Za-z0-9_.-]+$`)
	macRe      = regexp.MustCompile(`^[0-9A-Fa-f]{2}([:-][0-9A-Fa-f]{2}){5}$|^[0-9A-Fa-f]{12}$`)
)

// Default locations.
func DefaultConfigPath() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\Fairy\config.yaml`
	}
	return "/etc/fairy/config.yaml"
}

func DefaultEnvFile() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\Fairy\fairy.env`
	}
	return "/etc/fairy/fairy.env"
}

type OIDC struct {
	Issuer        string   `yaml:"issuer"`
	ClientID      string   `yaml:"client_id"`
	Scopes        []string `yaml:"scopes"`
	GroupsClaim   string   `yaml:"groups_claim"`
	AllowedGroups []string `yaml:"allowed_groups"`
	AllowedEmails []string `yaml:"allowed_emails"`
	ControlGroups []string `yaml:"control_groups"`
}

type Proxmox struct {
	ID                  string   `yaml:"id"`
	ClusterName         string   `yaml:"cluster_name"`
	IDPrefix            *string  `yaml:"id_prefix"`
	Standalone          bool     `yaml:"standalone"`
	Group               *string  `yaml:"group"`
	Owner               *string  `yaml:"owner"`
	URLs                []string `yaml:"urls"`
	TokenID             string   `yaml:"token_id"`
	TokenSecretEnv      string   `yaml:"token_secret_env"`
	VerifyTLS           *bool    `yaml:"verify_tls"`
	CAFile              string   `yaml:"ca_file"`
	IncludeTemplates    bool     `yaml:"include_templates"`
	PowerTokenID        string   `yaml:"power_token_id"`
	PowerTokenSecretEnv string   `yaml:"power_token_secret_env"`
	NodePower           bool     `yaml:"node_power"`
}

func (p Proxmox) Prefix() string {
	if p.IDPrefix == nil {
		return ""
	}
	return *p.IDPrefix
}

func (p Proxmox) Verify() bool { return p.VerifyTLS == nil || *p.VerifyTLS }

type FanControl struct {
	Vendor          string `yaml:"vendor"`
	ControlTokenEnv string `yaml:"control_token_env"`
}

type Labels struct {
	Group *string  `yaml:"group"`
	Owner *string  `yaml:"owner"`
	Tags  []string `yaml:"tags"`
}

type Agent struct {
	Labels                `yaml:",inline"`
	ID                    string              `yaml:"id"`
	Name                  string              `yaml:"name"`
	URL                   string              `yaml:"url"`
	TokenEnv              string              `yaml:"token_env"`
	DWServiceURL          string              `yaml:"dwservice_url"`
	VerifyTLS             *bool               `yaml:"verify_tls"`
	LinksTo               string              `yaml:"links_to"`
	Node                  string              `yaml:"node"`
	PowerLinks            map[string]string   `yaml:"power_links"`
	PowerExcludeFromTotal []string            `yaml:"power_exclude_from_total"`
	UPSPowers             map[string][]string `yaml:"ups_powers"`
	UPSNames              map[string]string   `yaml:"ups_names"`
	FanControl            *FanControl         `yaml:"fan_control"`
	ControlTokenEnv       string              `yaml:"control_token_env"`
	PowerControl          bool                `yaml:"power_control"`
	WolRelay              bool                `yaml:"wol_relay"`
}

// ControlEnv is the env var holding this agent's separate WRITE token.
func (a Agent) ControlEnv() string {
	if a.ControlTokenEnv != "" {
		return a.ControlTokenEnv
	}
	if a.FanControl != nil {
		return a.FanControl.ControlTokenEnv
	}
	return ""
}

func (a Agent) DisplayName() string {
	if a.Name != "" {
		return a.Name
	}
	return a.ID
}

type Wol struct {
	MAC         string `yaml:"mac"`
	Relay       string `yaml:"relay"`
	Broadcast   string `yaml:"broadcast"`
	Port        int    `yaml:"port"`
	PasswordEnv string `yaml:"password_env"`
}

type SystemOverride struct {
	Labels       `yaml:",inline"`
	ID           string `yaml:"id"`
	Name         string `yaml:"name"`
	DWServiceURL string `yaml:"dwservice_url"`
	Wol          *Wol   `yaml:"wol"`
}

type Intervals struct {
	Agent   float64 `yaml:"agent"`
	Proxmox float64 `yaml:"proxmox"`
	UPS     float64 `yaml:"ups"`
	History float64 `yaml:"history"`
	Stream  float64 `yaml:"stream"`
}

type Panel struct {
	PublicURL          string           `yaml:"public_url"`
	Intervals          Intervals        `yaml:"intervals"`
	BoostWhileViewing  *bool            `yaml:"boost_while_viewing"`
	RequestTimeout     float64          `yaml:"request_timeout"`
	OfflineAfter       float64          `yaml:"offline_after"`
	SessionMaxAge      int              `yaml:"session_max_age"`
	SessionIdleTimeout int              `yaml:"session_idle_timeout"`
	EnableDocs         bool             `yaml:"enable_docs"`
	OIDC               OIDC             `yaml:"oidc"`
	Proxmox            ProxmoxList      `yaml:"proxmox"`
	Agents             []Agent          `yaml:"agents"`
	Systems            []SystemOverride `yaml:"systems"`
	PowerSources       []map[string]any `yaml:"power_sources"`
	UPS                []map[string]any `yaml:"ups"`
	PowerHistoryS      int              `yaml:"power_history_s"`
	DataDir            string           `yaml:"data_dir"`
	AuditLog           string           `yaml:"audit_log"`
	ControlRatePerMin  int              `yaml:"control_rate_per_min"`
	PanelHosts         []string         `yaml:"panel_hosts"`
	TrustedProxies     []string         `yaml:"trusted_proxies"`
	StepUpWindowS      int              `yaml:"step_up_window_s"`
	StepUpPromptLogin  bool             `yaml:"step_up_prompt_login"`
	LoginRatePerMin    int              `yaml:"login_rate_per_min"`

	trusted []netip.Prefix
}

// ProxmoxList accepts either a single mapping (legacy: unprefixed ids) or a list (prefix "<id>-").
type ProxmoxList []Proxmox

func (l *ProxmoxList) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.MappingNode {
		var p Proxmox
		if err := n.Decode(&p); err != nil {
			return err
		}
		if p.IDPrefix == nil {
			empty := ""
			p.IDPrefix = &empty
		}
		*l = ProxmoxList{p}
		return nil
	}
	var list []Proxmox
	if err := n.Decode(&list); err != nil {
		return err
	}
	*l = list
	return nil
}

func (c *Panel) BaseURL() string         { return strings.TrimRight(c.PublicURL, "/") }
func (c *Panel) Boost() bool             { return c.BoostWhileViewing == nil || *c.BoostWhileViewing }
func (c *Panel) Trusted() []netip.Prefix { return c.trusted }
func (o OIDC) DiscoveryURL() string {
	return strings.TrimRight(o.Issuer, "/") + "/.well-known/openid-configuration"
}

// Load reads and validates a config file.
func Load(path string) (*Panel, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes YAML strictly (unknown keys are errors) and validates.
func Parse(data []byte) (*Panel, error) {
	var c Panel
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

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

// ---------------------------------------------------------------- secrets / environment

// LoadEnvFile reads KEY=VALUE lines (optional quotes, # comments) into the environment without overriding
// variables that are already set. A missing file is not an error.
func LoadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set && envNameRe.MatchString(k) {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

// EnvSecret resolves a secret referenced by env-var name.
func EnvSecret(name, what string, minLen int) (string, error) {
	if name == "" {
		return "", nil
	}
	v := os.Getenv(name)
	if len(v) < max(minLen, 1) {
		return "", fmt.Errorf("%s: env var %s is missing or shorter than %d chars", what, name, max(minLen, 1))
	}
	return v, nil
}

// Secrets read from FAIRY_* environment variables.
type Secrets struct {
	SessionSecret      string
	OIDCClientSecret   string
	ProxmoxTokenSecret string
	InsecureCookies    bool
}

func LoadSecrets() (Secrets, error) {
	s := Secrets{
		SessionSecret:      os.Getenv("FAIRY_SESSION_SECRET"),
		OIDCClientSecret:   os.Getenv("FAIRY_OIDC_CLIENT_SECRET"),
		ProxmoxTokenSecret: os.Getenv("FAIRY_PROXMOX_TOKEN_SECRET"),
		InsecureCookies:    truthy(os.Getenv("FAIRY_INSECURE_COOKIES")),
	}
	if len(s.SessionSecret) < 32 {
		return s, errors.New("FAIRY_SESSION_SECRET is missing or shorter than 32 characters")
	}
	return s, nil
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
