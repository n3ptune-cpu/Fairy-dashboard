// Package config loads the non-secret settings from /etc/fairy/config.yaml and the secrets from the environment
// (/etc/fairy/fairy.env is loaded at startup without overriding variables that are already set).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strings"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/power"
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
	// WakeUpdate: this machine's own timings for "Update unpowered hosts", where they differ from the
	// wake_update: block of the file.
	WakeUpdate *SystemWakeUpdate `yaml:"wake_update"`
}

// SystemWakeUpdate is the `wake_update:` block of a `systems:` entry: the two timings of "Update unpowered
// hosts" that can be set for one machine. A key that is left out (0 here) is the value of the file's own
// wake_update: block. For the agent of a Proxmox node the block counts under the node's id and under the
// agent's (poller.WakeUpdateOverride), like the entry's wol:.
type SystemWakeUpdate struct {
	// OffAfterS: this machine counts as off after this long without an answer (300-3600; see
	// WakeUpdate.OffAfterS for what a short time risks).
	OffAfterS int `yaml:"off_after_s"`
	// ShutdownDelayS: the countdown of the shutdown a run asks this machine for (10-3600).
	ShutdownDelayS int `yaml:"shutdown_delay_s"`
}

// UnmarshalYAML reads the block as strictly as the file's own wake_update: block (WakeUpdate.UnmarshalYAML):
// no other key, whole numbers, and a number that is written must be in its range (0 is not "the default").
func (w *SystemWakeUpdate) UnmarshalYAML(n *yaml.Node) error {
	if err := wakeUpdateKeys(n, "a systems: entry's wake_update", "off_after_s", "shutdown_delay_s"); err != nil {
		return err
	}
	type plain SystemWakeUpdate
	return n.Decode((*plain)(w))
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
	// AllowNonTailscale lets Add host and Re-pair pair with a machine at an address outside Tailscale's ranges
	// (100.64.0.0/10, fd7a:115c:a1e0::/48). Off by default: over Tailscale nobody outside the machine can listen
	// to the pairing or answer in its place; on another network the panel cannot know that.
	AllowNonTailscale bool `yaml:"allow_non_tailscale"`
}

// WakeUpdate is the `wake_update:` block: the timings of "Update unpowered hosts" (Updates page; README:
// "Update unpowered hosts"). Which machines take part is not set here: each one is switched on for it in the
// panel (state.json), by an admin, behind "Confirm it's you".
type WakeUpdate struct {
	// OnlineTimeoutS: a machine the run woke is given up on when its agent has not answered by then (default
	// 300, 60-1800). A woken machine has to start, get its network and bring Tailscale up before its agent
	// can answer, so the wait is generous; the run goes on the moment the agent answers.
	OnlineTimeoutS int `yaml:"online_timeout_s"`
	// SettleS is the pause between the result of the update and the shutdown request (default 60, 10-600).
	SettleS int `yaml:"settle_s"`
	// ShutdownDelayS is the countdown of that shutdown; it can be cancelled in the panel while it runs
	// (default 120, 10-3600).
	ShutdownDelayS int `yaml:"shutdown_delay_s"`
	// AgentRequestsPerMin is how many control requests a run sends to one agent per minute (default 8, 1-30).
	// An agent accepts 12 a minute for everything (its control_rate_per_min); the rest stays free for the
	// buttons of the page.
	AgentRequestsPerMin int `yaml:"agent_requests_per_min"`
	// OffAfterS: a machine counts as off after this long without any sign of life (default 900, 300-3600).
	// The default is the safe value, and a lower one is the owner's decision: a machine that restarts (for
	// Windows Update, or because its owner restarted it) is silent too, comes back by itself with a fresh
	// uptime, and a run that had taken it for off treats it as a machine it woke, and switches it off. With
	// 900 a restart would have to take a quarter of an hour for that; with 300, five minutes. It was a fixed
	// 15 minutes up to build 13 (wakerun.MinOff).
	OffAfterS int `yaml:"off_after_s"`
}

// wakeUpdateRanges: lowest, highest and default value of each key of the block (Validate has the same limits).
var wakeUpdateRanges = map[string][3]int{"online_timeout_s": {60, 1800, 300}, "settle_s": {10, 600, 60},
	"shutdown_delay_s": {10, 3600, 120}, "agent_requests_per_min": {1, 30, 8}, "off_after_s": {300, 3600, 900}}

// UnmarshalYAML reads the block as the plain fields above, with two things the plain way does not do: a
// number with a fraction is refused (YAML's decoder cuts it off: settle_s: 30.5 became 30 without a word; the
// session timings refuse one too), and a key this block does not have is refused here, because the strict
// mode of the whole file does not reach into a block that reads itself.
func (w *WakeUpdate) UnmarshalYAML(n *yaml.Node) error {
	if err := wakeUpdateKeys(n, "wake_update", "online_timeout_s", "settle_s", "shutdown_delay_s", "agent_requests_per_min", "off_after_s"); err != nil {
		return err
	}
	type plain WakeUpdate
	return n.Decode((*plain)(w))
}

// wakeUpdateKeys checks a wake_update: block (the file's own, or the one of a systems: entry) before it is
// decoded: only the keys named, whole numbers, and no 0.
func wakeUpdateKeys(n *yaml.Node, block string, keys ...string) error {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if !slices.Contains(keys, k.Value) {
			return fmt.Errorf("line %d: field %s not found in %s", k.Line, k.Value, block)
		}
		var f float64
		if v.Kind == yaml.ScalarNode && v.Decode(&f) == nil && f != math.Trunc(f) {
			return fmt.Errorf("line %d: %s.%s: %s is not a whole number", v.Line, block, k.Value, v.Value)
		}
		// A number that is written must be in its range, 0 included: here 0 is not "the default" (the
		// example gives ranges that begin at 1, 10 and 60, and a 0 that silently ran as 120 seconds is
		// not what was written; r2-p4-13). For the default the key is left out; a key with no value
		// (YAML's null) counts as left out.
		var i int
		if v.Kind == yaml.ScalarNode && v.ShortTag() != "!!null" && v.Decode(&i) == nil && i == 0 {
			r := wakeUpdateRanges[k.Value]
			return fmt.Errorf("line %d: %s.%s: %s is not in %d-%d (leave the key out for the default, %d)", v.Line, block, k.Value, v.Value, r[0], r[1], r[2])
		}
	}
	return nil
}

// Approval is the `approval:` block: the Pocket ID client "Fairy PC actions", with which a person approves a fan,
// power or update action (or the switching on of that check) for the machine itself to verify. It is a client
// of its own, not the sign-in client of `oidc:`: a machine accepts only tokens made for this one. Left out
// (no client_id): the panel asks no machine for an approval, and machines that require one refuse.
type Approval struct {
	// ClientID is the client id Pocket ID shows for "Fairy PC actions" (1-128 characters of A-Z a-z 0-9 . _ ~ -).
	// Its secret, if it is not a public client, is FAIRY_APPROVAL_CLIENT_SECRET in the environment.
	ClientID string `yaml:"client_id"`
	// Issuer is the Pocket ID the client belongs to. Left out: oidc.issuer.
	Issuer string `yaml:"issuer"`
	// Users are Pocket ID user ids ("sub") that may approve on the machines, in addition to the panel's admins
	// and a host's owner: they go into the list a machine is given when the check is switched on for it.
	Users []string `yaml:"users"`
	// Owners says which Pocket ID user a host's owner is, by the owner label of config.yaml (`owner: Straw` on an
	// agents: or systems: entry): owners: {Straw: "<Pocket ID user id>"}. That user may then approve on the
	// machines with that owner label, in addition to the admins. The panel does not guess it from names.
	Owners map[string]string `yaml:"owners"`
	// RunApprovalWaitS is how long a run of "Update unpowered hosts" waits for the approval it asks for
	// once the woken machines are online (default 900, 60-3600).
	RunApprovalWaitS int `yaml:"run_approval_wait_s"`
}

// Intervals are the default polling intervals in seconds (each 1-300).
type Intervals struct {
	Agent   float64 `yaml:"agent"`
	Proxmox float64 `yaml:"proxmox"`
	UPS     float64 `yaml:"ups"`
	History float64 `yaml:"history"`
	Stream  float64 `yaml:"stream"`
}

// The session timings a config.yaml gets when it does not set them: a session ends one day after its sign-in,
// and after four hours in which nobody used the page. (Until the release with the Sessions page they were 7 days
// and 12 hours.) A sign-in that is copied, a session cookie taken from a browser, is good for that long at
// most. When only session_max_age is set and it is shorter than four hours, the idle timeout is the whole
// session.
const (
	DefaultSessionMaxAge      Seconds = 86400
	DefaultSessionIdleTimeout Seconds = 14400
)

// Panel is the whole config.yaml. Parse fills in defaults and validates it.
type Panel struct {
	PublicURL          string           `yaml:"public_url"`
	Listen             string           `yaml:"listen"` // host:port (--listen and $FAIRY_LISTEN win); default 127.0.0.1:8080
	Intervals          Intervals        `yaml:"intervals"`
	BoostWhileViewing  *bool            `yaml:"boost_while_viewing"`
	RequestTimeout     float64          `yaml:"request_timeout"`
	OfflineAfter       float64          `yaml:"offline_after"`
	SessionMaxAge      Seconds          `yaml:"session_max_age"`      // seconds, or "1d", "36h" (DefaultSessionMaxAge)
	SessionIdleTimeout Seconds          `yaml:"session_idle_timeout"` // seconds, or "4h", "30m" (DefaultSessionIdleTimeout)
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
	// WakeUpdate: the timings of "Update unpowered hosts" (all optional).
	WakeUpdate WakeUpdate `yaml:"wake_update"`
	// Approval: the Pocket ID client for approvals the machines verify themselves (optional).
	Approval Approval `yaml:"approval"`

	trusted []netip.Prefix
	ignored []string
}

// Ignored returns one line for every entry of config.yaml that was read and is not used: a power source of a
// type that is no longer supported (tuya). `fairy serve` logs the lines at start, `fairy admin config check`
// prints them; they are never an error.
func (c *Panel) Ignored() []string { return c.ignored }

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

// ApprovalOn reports whether the panel has the Pocket ID client for approvals (approval.client_id).
func (c *Panel) ApprovalOn() bool { return c.Approval.ClientID != "" }

// ApprovalIssuer is the Pocket ID of the approval client: approval.issuer, else oidc.issuer; without a trailing
// slash, as the sign-in uses it.
func (c *Panel) ApprovalIssuer() string {
	if c.Approval.Issuer != "" {
		return strings.TrimRight(c.Approval.Issuer, "/")
	}
	return strings.TrimRight(c.OIDC.Issuer, "/")
}

// ApprovalCallback is the callback address to register for the client "Fairy PC actions" in Pocket ID.
func (c *Panel) ApprovalCallback() string { return c.BaseURL() + ApprovalCallbackPath }

// ApprovalCallbackPath is where Pocket ID sends the browser back to after an approval.
const ApprovalCallbackPath = "/auth/approval/callback"

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

// Peek reads the two settings `fairy update` needs from a config.yaml that Load refuses: where the panel
// listens and which repository its releases come from. Nothing is validated and unknown keys are passed over,
// so a file written for a newer release (a key this program does not know yet) still says where the panel
// listens. An error means the file is not YAML at all: no release can read it.
func Peek(path string) (listen, panelRepo string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var v struct {
		Listen  string `yaml:"listen"`
		Updates struct {
			PanelRepo string `yaml:"panel_repo"`
		} `yaml:"updates"`
	}
	if err := yaml.Unmarshal(data, &v); err != nil {
		return "", "", fmt.Errorf("config: %w", err)
	}
	return strings.TrimSpace(v.Listen), strings.TrimSpace(v.Updates.PanelRepo), nil
}

// Parse decodes YAML strictly (unknown keys are errors) and validates.
func Parse(data []byte) (*Panel, error) {
	var c Panel
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("config: %w", err)
	}
	// A power source of a type that is no longer read (tuya) is taken out here, the one place every command
	// loads the file through, and named by Ignored: it must never stop the panel from starting.
	c.PowerSources, c.ignored = power.DropRemoved(c.PowerSources)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
