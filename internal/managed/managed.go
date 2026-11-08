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
	AddedAt  time.Time   `yaml:"added_at" json:"added_at"`
	AddedBy  string      `yaml:"added_by,omitempty" json:"added_by"`
	// What the agent said when it paired.
	AgentVersion string `yaml:"agent_version,omitempty" json:"agent_version"`
	Hostname     string `yaml:"hostname,omitempty" json:"hostname"`
	// ControlPaired: the agent handed over a control token (a control feature is on).
	ControlPaired bool `yaml:"control_paired,omitempty" json:"control_paired"`
}

// Secret is a managed host's tokens.
type Secret struct {
	Token        string `json:"token"`
	ControlToken string `json:"control_token,omitempty"`
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
	return a
}

// Shown is the id the host shows as in the panel.
func (h Host) Shown() string {
	if h.SystemID != "" {
		return h.SystemID
	}
	return h.ID
}

// Override is the host's systems: entry (Wake-on-LAN), nil without MACs.
func (h Host) Override() *config.SystemOverride {
	if h.Wol == nil || len(h.Wol.AllMACs()) == 0 {
		return nil
	}
	w := *h.Wol
	return &config.SystemOverride{ID: h.Shown(), Wol: &w}
}

// Entry builds the configuration entries for a host and its tokens.
func (h Host) Entry(sec Secret) Entry {
	return Entry{Host: h, Agent: h.Agent(sec), Override: h.Override()}
}

var idRe = regexp.MustCompile(`[^a-z0-9]+`)

// IDFor makes a system id from a host name ("NSX-FURINA" -> "nsx-furina").
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
	b, err := os.ReadFile(s.Path())
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
	return d.Hosts, nil
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
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

// Entry is a host ready to be added to the configuration: the agent and (with Wake-on-LAN) its systems: entry.
type Entry struct {
	Host     Host
	Agent    config.Agent
	Override *config.SystemOverride
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
		e := h.Entry(sec)
		if err := Check(cfg, e); err != nil {
			skipped = append(skipped, fmt.Sprintf("managed host %s skipped: %v", h.ID, oneLine(err)))
			continue
		}
		cfg.Agents = append(cfg.Agents, e.Agent)
		if e.Override != nil {
			cfg.Systems = append(cfg.Systems, *e.Override)
		}
		added = append(added, e)
	}
	return added, skipped, nil
}

func oneLine(err error) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(err.Error(), "config invalid:", "")), " ")
}
