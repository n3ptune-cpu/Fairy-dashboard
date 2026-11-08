// Package config: non-secret settings from /etc/fairy/config.yaml, secrets from the environment
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
