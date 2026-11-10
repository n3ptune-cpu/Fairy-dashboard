// Package config loads the non-secret settings from /etc/fairy/config.yaml and the secrets from the environment
// (/etc/fairy/fairy.env is loaded at startup without overriding variables that are already set).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultConfigPath is the platform's default config file (/etc/fairy/config.yaml on Linux).
func DefaultConfigPath() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\Fairy\config.yaml`
	}
	return "/etc/fairy/config.yaml"
}

// PathFromEnv is the config file to use when --config is not given: $FAIRY_CONFIG_FILE, or DefaultConfigPath.
func PathFromEnv() string {
	if p := os.Getenv("FAIRY_CONFIG_FILE"); p != "" {
		return p
	}
	return DefaultConfigPath()
}

// DefaultEnvFile is the platform's default secrets file (/etc/fairy/fairy.env on Linux).
func DefaultEnvFile() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\Fairy\fairy.env`
	}
	return "/etc/fairy/fairy.env"
}

// OIDC is the Pocket ID client and who may sign in (allowed groups / emails). Who may see and do what is
// managed in the panel (users.json); ControlGroups and BootstrapAdmins only decide who becomes an admin the
// first time they sign in.
type OIDC struct {
	Issuer        string   `yaml:"issuer"`
	ClientID      string   `yaml:"client_id"`
	Scopes        []string `yaml:"scopes"`
	GroupsClaim   string   `yaml:"groups_claim"`
	AllowedGroups []string `yaml:"allowed_groups"`
	AllowedEmails []string `yaml:"allowed_emails"`
	ControlGroups []string `yaml:"control_groups"`
	// BootstrapAdmins are email addresses that become admin on their first sign-in (like control_groups).
	BootstrapAdmins []string `yaml:"bootstrap_admins"`
}

// Proxmox is one `proxmox:` cluster (or standalone host): API URLs for failover, the read-only token and the
// optional separate power token.
type Proxmox struct {
	ID             string   `yaml:"id"`
	ClusterName    string   `yaml:"cluster_name"`
	IDPrefix       *string  `yaml:"id_prefix"`
	Standalone     bool     `yaml:"standalone"`
	Group          *string  `yaml:"group"`
	Owner          *string  `yaml:"owner"`
	Site           string   `yaml:"site"` // default site of its nodes and guests (the Users page can override)
	URLs           []string `yaml:"urls"`
	TokenID        string   `yaml:"token_id"`
	TokenSecretEnv string   `yaml:"token_secret_env"`
	VerifyTLS      *bool    `yaml:"verify_tls"`
	CAFile         string   `yaml:"ca_file"`
	// TLSServerName is the name the node's certificate must be issued for, when it differs from the URL's
	// host (Proxmox's own certificate lists the node's name, not its Tailscale name).
	TLSServerName       string `yaml:"tls_server_name"`
	IncludeTemplates    bool   `yaml:"include_templates"`
	PowerTokenID        string `yaml:"power_token_id"`
	PowerTokenSecretEnv string `yaml:"power_token_secret_env"`
	NodePower           bool   `yaml:"node_power"`

	// Managed is set for a node added in the panel (Add host, managed-hosts.yaml): its token secret comes from
	// the panel's state folder (TokenSecret) instead of an environment variable. Never read from config.yaml.
	Managed     bool   `yaml:"-" json:"-"`
	TokenSecret string `yaml:"-" json:"-"`
}

// Prefix is the id prefix that keeps system ids unique across clusters ("" for the legacy single cluster).
func (p Proxmox) Prefix() string {
	if p.IDPrefix == nil {
		return ""
	}
	return *p.IDPrefix
}

// Verify reports whether the cluster's TLS certificate is verified (the default).
func (p Proxmox) Verify() bool { return p.VerifyTLS == nil || *p.VerifyTLS }

// FanControl enables BMC fan control through an agent (only vendor "supermicro").
type FanControl struct {
	Vendor          string `yaml:"vendor"`
	ControlTokenEnv string `yaml:"control_token_env"`
}

// Labels are optional grouping fields shared by agents and `systems:` overrides. Site is the default site
// (who may see the system); a site assigned on the Users page overrides it.
type Labels struct {
	Group *string  `yaml:"group"`
	Owner *string  `yaml:"owner"`
	Tags  []string `yaml:"tags"`
	Site  string   `yaml:"site"`
}

// Agent is one `agents:` entry: a Fairy agent reached over HTTP with a read token (and optionally a separate
// control token for fans, power and Wake-on-LAN).
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
	// SelfUpdate shows the agent on the Updates page with an Update button (the agent needs
	// self_update.enabled too). Needs control_token_env.
	SelfUpdate bool `yaml:"self_update"`

	// Managed is set for hosts added in the panel (Add host, managed-hosts.yaml): their tokens come from the
	// panel's state folder (Token, ControlToken) instead of environment variables. Never read from config.yaml.
	Managed      bool   `yaml:"-" json:"-"`
	Token        string `yaml:"-" json:"-"`
	ControlToken string `yaml:"-" json:"-"`
}

// HasControl reports whether the panel holds a control token for this agent (by env var, or managed).
func (a Agent) HasControl() bool { return a.ControlEnv() != "" || (a.Managed && a.ControlToken != "") }

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

// DisplayName is the agent's name, or its id when no name is configured.
func (a Agent) DisplayName() string {
	if a.Name != "" {
		return a.Name
	}
	return a.ID
}

// Wol is a system's Wake-on-LAN setting: the magic packet is sent by a relay agent on the same LAN. A machine
// with several network cards lists them all in macs (mac and macs may both be given); Wake sends to each.
type Wol struct {
	MAC         string   `yaml:"mac"`
	MACs        []string `yaml:"macs"`
	Relay       string   `yaml:"relay"`
	Broadcast   string   `yaml:"broadcast"`
	Port        int      `yaml:"port"`
	PasswordEnv string   `yaml:"password_env"`
}

// AllMACs is mac followed by macs, without duplicates (compared case-insensitively, - and : alike).
func (w Wol) AllMACs() []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range append([]string{w.MAC}, w.MACs...) {
		k := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(m), "-", ":"))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, strings.TrimSpace(m))
	}
	return out
}

// SystemOverride is a `systems:` entry: display name, labels, DWService link and Wake-on-LAN for one system id.
type SystemOverride struct {
	Labels       `yaml:",inline"`
	ID           string    `yaml:"id"`
	Name         string    `yaml:"name"`
	DWServiceURL string    `yaml:"dwservice_url"`
	Wol          *Wol      `yaml:"wol"`
	BMCPower     *BMCPower `yaml:"bmc_power"`
}

// BMCPower powers a machine on through its BMC, by an always-on agent that can reach the BMC (Fairy-agent
// v0.4.2 bmc_relay): the relay agent's id and the target id in that agent's bmc_relay.targets.
type BMCPower struct {
	Relay  string `yaml:"relay" json:"relay"`
	Target string `yaml:"target" json:"target"`
}

// Branding is the optional look of the panel: Logo is a PNG, JPEG, WebP or GIF file (at most 1 MiB, no SVG)
// shown instead of the built-in logo and used as the favicon.
type Branding struct {
	Logo string `yaml:"logo"`
}

// Updates is the `updates:` block: where the panel looks for new releases (Updates page, `fairy update`).
type Updates struct {
	// CheckLatest asks GitHub for the newest Fairy-agent release (at most every 10 minutes, only while an admin
	// has the Updates page open). Default true; false when the panel has no internet access.
	CheckLatest *bool  `yaml:"check_latest"`
	AgentRepo   string `yaml:"agent_repo"` // default n3ptune-cpu/Fairy-agent
	PanelRepo   string `yaml:"panel_repo"` // default n3ptune-cpu/Fairy-dashboard (`fairy update`)
}

// Check reports whether the latest agent release is looked up on GitHub.
func (u Updates) Check() bool { return u.CheckLatest == nil || *u.CheckLatest }

// Enroll is the `enroll:` block (Add host).
type Enroll struct {
	// PanelAddress is the panel's Tailscale IPv4 address as the agents see it: the install command passes it to
	// the agent, which then answers pairing only from there. Optional: by default the panel finds it at start
	// (the tailscale* interface's 100.64.0.0/10 address, else tailscaled's LocalAPI; see enrollment.Detector).
	PanelAddress string `yaml:"panel_address"`
	// AgentRelease is the Fairy-agent release tag (build-<n>) whose install script Add host uses. Optional: by
	// default the release this panel was built against. Either way the panel checks the script with its
	// release key before it shows a command.
	AgentRelease string `yaml:"agent_release"`
}

// Intervals are the default polling intervals in seconds (each 1-300).
type Intervals struct {
	Agent   float64 `yaml:"agent"`
	Proxmox float64 `yaml:"proxmox"`
	UPS     float64 `yaml:"ups"`
	History float64 `yaml:"history"`
	Stream  float64 `yaml:"stream"`
}

// Panel is the whole config.yaml. Parse fills in defaults and validates it.
type Panel struct {
	PublicURL          string           `yaml:"public_url"`
	Listen             string           `yaml:"listen"` // host:port (--listen and $FAIRY_LISTEN win); default 127.0.0.1:8080
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
	AuditMaxMB         int              `yaml:"audit_max_mb"` // rotate audit_log at this size (default 100; -1 = never)
	AuditKeep          int              `yaml:"audit_keep"`   // rotated files kept (default 5)
	ControlRatePerMin  int              `yaml:"control_rate_per_min"`
	PanelHosts         []string         `yaml:"panel_hosts"`
	TrustedProxies     []string         `yaml:"trusted_proxies"`
	StepUpWindowS      int              `yaml:"step_up_window_s"`
	StepUpPromptLogin  bool             `yaml:"step_up_prompt_login"`
	// StepUpPerAction (default off): a "Confirm it's you" sign-in authorises exactly the one action it was asked
	// for and nothing afterwards; step_up_window_s is then not used for control actions.
	StepUpPerAction bool     `yaml:"step_up_per_action"`
	LoginRatePerMin int      `yaml:"login_rate_per_min"`
	Branding        Branding `yaml:"branding"`
	Updates         Updates  `yaml:"updates"`
	Enroll          Enroll   `yaml:"enroll"`

	trusted []netip.Prefix
}

// ProxmoxList accepts either a single mapping (legacy: unprefixed ids) or a list (prefix "<id>-").
type ProxmoxList []Proxmox

// UnmarshalYAML accepts either a single mapping (legacy: unprefixed ids) or a list.
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

// AuditRotation is the audit log's rotation size in bytes (0 = off) and the number of old files to keep.
func (c *Panel) AuditRotation() (int64, int) {
	if c.AuditMaxMB < 0 {
		return 0, c.AuditKeep
	}
	return int64(c.AuditMaxMB) << 20, c.AuditKeep
}

// BaseURL is public_url without a trailing slash.
func (c *Panel) BaseURL() string { return strings.TrimRight(c.PublicURL, "/") }

// Boost reports whether boost-while-viewing is on (the default).
func (c *Panel) Boost() bool { return c.BoostWhileViewing == nil || *c.BoostWhileViewing }

// Trusted returns trusted_proxies as parsed network prefixes.
func (c *Panel) Trusted() []netip.Prefix { return c.trusted }

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
