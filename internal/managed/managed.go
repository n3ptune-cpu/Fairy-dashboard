// Package managed is the panel's own list of hosts added with "Add host": managed-hosts.yaml (no secrets) and
// secrets/<id>.json (the agent's tokens, 0600) in the panel's data folder (/var/lib/fairy), which the service
// may write, unlike /etc/fairy/config.yaml. At start they are merged into the config: config.yaml wins when
// both name the same id, and every entry goes through the same validation as a hand-written one.
package managed

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/configutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// FileName is the list of managed hosts in the data folder.
const FileName = "managed-hosts.yaml"

// Options are what the Add host dialog asked the agent to do (kept for Re-pair's command).
type Options struct {
	PowerControl bool   `yaml:"power_control,omitempty" json:"power_control"`
	SelfUpdate   bool   `yaml:"self_update,omitempty" json:"self_update"`
	WolRelay     bool   `yaml:"wol_relay,omitempty" json:"wol_relay"`
	UPS          bool   `yaml:"ups,omitempty" json:"ups"`
	UPSName      string `yaml:"ups_name,omitempty" json:"ups_name"`
	CoreTemp     bool   `yaml:"coretemp,omitempty" json:"coretemp"`
	IPMI         bool   `yaml:"ipmi,omitempty" json:"ipmi"`
}

// Control reports whether an option needs the control token.
func (o Options) Control() bool { return o.PowerControl || o.SelfUpdate || o.WolRelay }

// Host is one managed host (no secrets).
type Host struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name" json:"name"`
	URL  string `yaml:"url" json:"url"`
	Site string `yaml:"site,omitempty" json:"site"`
	OS   string `yaml:"os" json:"os"` // linux | windows
	Node string `yaml:"node,omitempty" json:"node"`
	// SystemID is the id the host shows as: its own id, or the Proxmox node's it is attached to (node:).
	SystemID string      `yaml:"system_id,omitempty" json:"system_id"`
	Options  Options     `yaml:"options" json:"options"`
	Wol      *config.Wol `yaml:"wol,omitempty" json:"wol,omitempty"` // waking this host: its MACs and the relay agent
	// Proxmox: the host is a standalone Proxmox VE node whose read-only API token came with the pairing
	// (Add host "Yes: a new node"); its secret is in the host's secrets file, its CA next to it.
	Proxmox *PVE `yaml:"proxmox,omitempty" json:"proxmox,omitempty"`
	// ProxmoxWanted: Add host asked the node for its token (install.sh --proxmox); kept for Re-pair.
	ProxmoxWanted bool `yaml:"proxmox_wanted,omitempty" json:"proxmox_wanted"`
	// BMCPower: powering it on through its BMC, by a relay agent's bmc_relay target.
	BMCPower *config.BMCPower `yaml:"bmc_power,omitempty" json:"bmc_power,omitempty"`
	AddedAt  time.Time        `yaml:"added_at" json:"added_at"`
	AddedBy  string           `yaml:"added_by,omitempty" json:"added_by"`
	// What the agent said when it paired.
	AgentVersion string `yaml:"agent_version,omitempty" json:"agent_version"`
	Hostname     string `yaml:"hostname,omitempty" json:"hostname"`
	// ControlPaired: the agent handed over a control token (a control feature is on).
	ControlPaired bool `yaml:"control_paired,omitempty" json:"control_paired"`
	// Pairing is how the host's current tokens were handed over: PairingV2 when the agent's answer was
	// verified with the join code, "" for a host paired by an older panel (the old exchange could not tell
	// the agent from anything else that answered; Re-pair replaces such a pairing with a verified one).
	Pairing string `yaml:"pairing,omitempty" json:"pairing,omitempty"`
}

// PairingV2 marks a host whose pairing answer was verified (pairing v2).
const PairingV2 = "v2"

// Secret is a managed host's tokens.
type Secret struct {
	Token        string `json:"token"`
	ControlToken string `json:"control_token,omitempty"`
	// PVETokenSecret is the Proxmox API token's secret of a node added with Add host.
	PVETokenSecret string `json:"pve_token_secret,omitempty"`
}

// PVE is a managed Proxmox VE node (no secret): the panel's proxmox: entry for it.
type PVE struct {
	ID         string `yaml:"id" json:"id"`                   // the proxmox: entry's id
	IDPrefix   string `yaml:"id_prefix" json:"id_prefix"`     // <site>-<node>-: keeps its system ids unique
	Node       string `yaml:"node" json:"node"`               // the node's name
	URL        string `yaml:"url" json:"url"`                 // https://<Tailscale name>:8006
	ServerName string `yaml:"server_name" json:"server_name"` // the name its certificate is issued for
	TokenID    string `yaml:"token_id" json:"token_id"`       // fairy@pve!panel-<node>
	CA         bool   `yaml:"ca" json:"ca"`                   // the node's own CA is kept (else public CAs)
}

// TokenName is the token's name on the node (for `pveum user token remove fairy@pve <name>`).
func (p PVE) TokenName() string {
	_, n, _ := strings.Cut(p.TokenID, "!")
	return n
}

// Agent is the host as a config.yaml agents: entry.
func (h Host) Agent(sec Secret) config.Agent {
	a := config.Agent{ID: h.ID, Name: h.Name, URL: h.URL, Node: h.Node, Managed: true, Token: sec.Token,
		ControlToken: sec.ControlToken}
	a.Site = h.Site
	if h.Site != "" {
		g := h.Site
		a.Group = &g
	}
	if sec.ControlToken != "" {
		a.PowerControl, a.SelfUpdate, a.WolRelay = h.Options.PowerControl, h.Options.SelfUpdate, h.Options.WolRelay
	}
	if h.Proxmox != nil && h.SystemID != "" { // its own node: linked by id, not by name (names repeat across sites)
		a.Node, a.LinksTo = "", h.SystemID
	}
	if h.lostNode() {
		a.Node = ""
	}
	return a
}

// lostNode reports a host that was added as "a new Proxmox node" and has no node entry of its own (any more):
// a Re-pair was answered without a Proxmox token. Its node name is the machine's own word from its first
// pairing (Add host never lets an admin choose a node for such a host), so it is never used to link the host
// to a node by name: the node of that name may belong to another cluster (T-48). An older release left
// the name and the old node id in managed-hosts.yaml for such a host; this is how they are read.
func (h Host) lostNode() bool { return h.ProxmoxWanted && h.Proxmox == nil }

// Shown is the id the host shows as in the panel.
func (h Host) Shown() string {
	if h.SystemID != "" && !h.lostNode() {
		return h.SystemID
	}
	return h.ID
}

// Override is the host's systems: entry (Wake-on-LAN, BMC power-on), nil without either.
func (h Host) Override() *config.SystemOverride {
	o := &config.SystemOverride{ID: h.Shown()}
	if h.Wol != nil && len(h.Wol.AllMACs()) > 0 {
		w := *h.Wol
		o.Wol = &w
	}
	if h.BMCPower != nil {
		b := *h.BMCPower
		o.BMCPower = &b
	}
	if o.Wol == nil && o.BMCPower == nil {
		return nil
	}
	return o
}

// Entry builds the configuration entries for a host and its tokens.
func (h Host) Entry(sec Secret) Entry {
	return Entry{Host: h, Agent: h.Agent(sec), Override: h.Override()}
}

// ProxmoxEntry is the node's proxmox: entry (nil unless the host is a managed Proxmox node).
func (h Host) ProxmoxEntry(sec Secret, caFile string) *config.Proxmox {
	p := h.Proxmox
	if p == nil {
		return nil
	}
	prefix := p.IDPrefix
	c := &config.Proxmox{ID: p.ID, ClusterName: p.Node, IDPrefix: &prefix, Standalone: true, Site: h.Site,
		URLs: []string{p.URL}, TokenID: p.TokenID, TLSServerName: p.ServerName, Managed: true, TokenSecret: sec.PVETokenSecret}
	if h.Site != "" {
		g := h.Site
		c.Group = &g
	}
	if p.CA {
		c.CAFile = caFile
	}
	return c
}

var idRe = regexp.MustCompile(`[^a-z0-9]+`)

// IDFor makes a system id from a host name ("SRV-WINPC" -> "srv-winpc").
func IDFor(name string) string {
	id := strings.Trim(idRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(id) > 60 {
		id = strings.Trim(id[:60], "-")
	}
	for _, p := range []string{"node-", "qemu-", "lxc-"} { // reserved for Proxmox ids
		if strings.HasPrefix(id, p) {
			id = "host-" + id
		}
	}
	if !configutil.ValidID(id) {
		return ""
	}
	return id
}

// Store reads and writes the managed hosts. Safe for concurrent use; the file is replaced in one step.
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore uses dataDir (/var/lib/fairy).
func NewStore(dataDir string) *Store { return &Store{dir: dataDir} }

// Path is managed-hosts.yaml.
func (s *Store) Path() string { return filepath.Join(s.dir, FileName) }

func (s *Store) secretPath(id string) string {
	return filepath.Join(s.dir, "secrets", "agent-"+id+".json")
}

// CAPath is where a managed Proxmox node's CA certificate is kept.
func (s *Store) CAPath(id string) string {
	return filepath.Join(s.dir, "secrets", "pve-"+id+"-ca.pem")
}

// EntryOf is h.Entry plus its proxmox: entry (with the CA file of this store).
func (s *Store) EntryOf(h Host, sec Secret) Entry {
	e := h.Entry(sec)
	e.Proxmox = h.ProxmoxEntry(sec, s.CAPath(h.ID))
	return e
}

// PutCA keeps a managed Proxmox node's CA certificate (0600).
func (s *Store) PutCA(id string, pemData []byte) error {
	if !configutil.ValidID(id) {
		return fmt.Errorf("invalid host id %q", id)
	}
	return writeAtomic(s.CAPath(id), pemData)
}

type fileData struct {
	Hosts []Host `yaml:"hosts"`
}

const header = "# Hosts added in the Fairy panel (Add host). Written by the panel: change them there (Re-pair, Remove).\n" +
	"# Their tokens are in secrets/ next to this file. A host with the same id in /etc/fairy/config.yaml wins.\n"

// Load returns the managed hosts (none when the file does not exist).
func (s *Store) Load() ([]Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *Store) load() ([]Host, error) {
	b, err := statefile.ReadFile(s.Path(), statefile.MaxBytes) // a plain file of a sane size, or it is not read
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d fileData
	if err := yaml.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path(), err)
	}
	for i := range d.Hosts { // the two texts a machine supplied (a file of an earlier release kept them as sent)
		h := &d.Hosts[i]
		h.AgentVersion, h.Hostname = textsafe.Cut(h.AgentVersion, 64), textsafe.Cut(h.Hostname, 64)
	}
	return d.Hosts, nil
}

// writeAtomic replaces one of the store's files (0600): statefile.WriteAtomic, the panel's one way to do that.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return statefile.WriteAtomic(path, data, 0o600)
}

func (s *Store) save(hosts []Host) error {
	b, err := yaml.Marshal(fileData{Hosts: hosts})
	if err != nil {
		return err
	}
	return writeAtomic(s.Path(), append([]byte(header), b...))
}

// Put adds or replaces a host and its tokens (the tokens first, so a host is never listed without them).
func (s *Store) Put(h Host, sec Secret) error {
	if !configutil.ValidID(h.ID) {
		return fmt.Errorf("invalid host id %q", h.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hosts, err := s.load()
	if err != nil {
		return err
	}
	b, _ := json.Marshal(sec)
	if err := writeAtomic(s.secretPath(h.ID), b); err != nil {
		return err
	}
	hosts = slices.DeleteFunc(hosts, func(x Host) bool { return x.ID == h.ID })
	return s.save(append(hosts, h))
}

// Remove forgets a host and its tokens.
func (s *Store) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	hosts, err := s.load()
	if err != nil {
		return err
	}
	n := len(hosts)
	hosts = slices.DeleteFunc(hosts, func(x Host) bool { return x.ID == id })
	if len(hosts) == n {
		return os.ErrNotExist
	}
	if err := s.save(hosts); err != nil {
		return err
	}
	if err := os.Remove(s.secretPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(s.CAPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Secret reads a host's tokens.
func (s *Store) Secret(id string) (Secret, error) {
	var sec Secret
	if !configutil.ValidID(id) {
		return sec, fmt.Errorf("invalid host id %q", id)
	}
	b, err := os.ReadFile(s.secretPath(id))
	if err != nil {
		return sec, err
	}
	if err := json.Unmarshal(b, &sec); err != nil {
		return sec, fmt.Errorf("%s: %w", s.secretPath(id), err)
	}
	if len(sec.Token) < 24 || (sec.ControlToken != "" && (len(sec.ControlToken) < 24 || sec.ControlToken == sec.Token)) {
		return sec, fmt.Errorf("%s: tokens too short or equal", s.secretPath(id))
	}
	return sec, nil
}

// Entry is a host ready to be added to the configuration: the agent, (with Wake-on-LAN / BMC power-on) its
// systems: entry and (a Proxmox node) its proxmox: entry.
type Entry struct {
	Host     Host
	Agent    config.Agent
	Override *config.SystemOverride
	Proxmox  *config.Proxmox
}

// Check validates one host against cfg exactly like a hand-written agents: (and systems:) entry, on a copy of
// cfg: the id must be new, the URL, site, node, Wake-on-LAN MACs and relay valid.
func Check(cfg *config.Panel, e Entry) error {
	for _, a := range cfg.Agents {
		if a.ID == e.Agent.ID {
			return fmt.Errorf("an agent with id %s is already configured", a.ID)
		}
	}
	c := *cfg
	c.Agents = append(slices.Clone(cfg.Agents), e.Agent)
	c.Systems = slices.Clone(cfg.Systems)
	if e.Override != nil {
		for _, o := range cfg.Systems {
			if o.ID == e.Override.ID {
				return fmt.Errorf("systems: in config.yaml already has an entry for %s", o.ID)
			}
		}
		c.Systems = append(c.Systems, *e.Override)
	}
	c.Proxmox = slices.Clone(cfg.Proxmox)
	if e.Proxmox != nil {
		for _, x := range cfg.Proxmox {
			if x.ID == e.Proxmox.ID {
				return fmt.Errorf("a proxmox entry with id %s is already configured", x.ID)
			}
			if x.Prefix() == e.Proxmox.Prefix() {
				return fmt.Errorf("the proxmox entry %s already uses the id prefix %q", x.ID, x.Prefix())
			}
		}
		c.Proxmox = append(c.Proxmox, *e.Proxmox)
	}
	return c.Validate()
}

// Merge loads the managed hosts and adds the valid ones to cfg (config.yaml wins on id conflicts). It returns
// the entries added and one line per host that was skipped (for the log and `fairy admin config check`).
func (s *Store) Merge(cfg *config.Panel) ([]Entry, []string, error) {
	hosts, err := s.Load()
	if err != nil {
		return nil, nil, err
	}
	var added []Entry
	var skipped []string
	for _, h := range hosts {
		sec, err := s.Secret(h.ID)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("managed host %s: tokens unreadable (%v): Re-pair it", h.ID, err))
			continue
		}
		e := s.EntryOf(h, sec)
		if err := Check(cfg, e); err != nil {
			skipped = append(skipped, fmt.Sprintf("managed host %s skipped: %v", h.ID, oneLine(err)))
			continue
		}
		cfg.Agents = append(cfg.Agents, e.Agent)
		if e.Override != nil {
			cfg.Systems = append(cfg.Systems, *e.Override)
		}
		if e.Proxmox != nil {
			cfg.Proxmox = append(cfg.Proxmox, *e.Proxmox)
		}
		added = append(added, e)
	}
	return added, skipped, nil
}

func oneLine(err error) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(err.Error(), "config invalid:", "")), " ")
}
