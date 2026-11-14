// Package enrollment is the panel side of "Add host": an admin's request becomes a pending enrollment with a
// one-time join code (shown once, inside the install command); a background loop asks the new agent
// POST /pair four times a minute (inside the agent's budget, see pace.go) until it answers, the code expires
// or the admin cancels. On success the host and its tokens go into the panel's managed store and the poller
// starts polling it at once.
//
// The panel speaks pairing v2 only (Fairy-agent pkg/enroll, v2.go): every request carries a new random value
// and a check value made with a key derived from the join code, and the answer is accepted only when it opens
// under a key bound to that request, which proves it was made by the machine the code was given to and keeps
// the tokens unreadable for anyone else. There is no fallback to the old exchange (a fixed proof, tokens in
// clear): an agent or a release that only knows that one is reported as too old and never paired. Only the
// key is kept, in memory.
package enrollment

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// States of an enrollment.
const (
	Waiting   = "waiting"   // waiting for the machine to run the command
	Paired    = "paired"    // tokens received, host added; waiting for its first /metrics
	Online    = "online"    // the host answers: done
	Failed    = "failed"    // the agent refused (e.g. too many wrong codes) or the host could not be added
	Expired   = "expired"   // 15 minutes passed
	Cancelled = "cancelled" // the admin cancelled it
)

// Request is the Add host dialog.
type Request struct {
	Name    string          `json:"name"`    // the machine's Tailscale (MagicDNS) name, also its display name
	Address string          `json:"address"` // optional: Tailscale IP or full name instead of Name
	Site    string          `json:"site"`
	OS      string          `json:"os"` // linux | windows
	Options managed.Options `json:"options"`
	Node    string          `json:"node"` // optional: the Proxmox node it runs on (node:)
	// ProxmoxNew: the machine is a standalone Proxmox VE node the panel does not poll yet; the command makes it
	// hand over a read-only API token at pairing (install.sh --proxmox, Fairy-agent v0.4.2).
	ProxmoxNew bool     `json:"proxmox_new"`
	WolMACs    []string `json:"wol_macs"`  // optional: to wake this machine
	WolRelay   string   `json:"wol_relay"` // the agent that sends the packets
	// BMCRelay / BMCTarget: optional, power it on through its BMC (a bmc_relay target of that agent).
	BMCRelay  string `json:"bmc_relay"`
	BMCTarget string `json:"bmc_target"`
	// NodeRenamed (Re-pair only): the admin says the Proxmox node this host is was renamed since it was added,
	// so the pairing answer may name another node than the stored one. Without it such an answer is refused.
	NodeRenamed bool `json:"-"`
}

// Enrollment is one pending (or finished) Add host / Re-pair.
type Enrollment struct {
	ID        string          `json:"id"`
	HostID    string          `json:"host_id"`
	Name      string          `json:"name"`
	URL       string          `json:"url"`
	OS        string          `json:"os"`
	Site      string          `json:"site"`
	Node      string          `json:"node,omitempty"`
	Options   managed.Options `json:"options"`
	Repair    bool            `json:"repair"`
	State     string          `json:"state"`
	Message   string          `json:"message"`
	Attempts  int             `json:"attempts"`
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt time.Time       `json:"expires_at"`
	CreatedBy string          `json:"created_by"`
	Done      bool            `json:"done"`
	// Warning (Re-pair): the machine runs a newer Fairy-agent than the release the command installs.
	Warning string `json:"warning,omitempty"`
	// Pairing is "v2" once the agent's answer was verified (it proved it has the join code).
	Pairing string `json:"pairing,omitempty"`
	// AgentHostname is what the machine that answered calls itself (from the verified answer; "" when the
	// agent hides it or sent something that is not a host name). The join code proves the answer comes from
	// a machine the command was run on; it does not prove that machine is the one at the address the admin
	// had in mind, so the dialog shows the name next to the address.
	AgentHostname string `json:"agent_hostname,omitempty"`
	// Hint says why the enrollment is still waiting, when the panel can tell: see the Hint constants.
	Hint string `json:"hint,omitempty"`

	key     []byte // the pairing v2 key of the join code (enroll.PairingKey); the code itself is not kept
	host    managed.Host
	renamed bool // Re-pair: the admin confirmed that the Proxmox node was renamed
	// installed is the agent version the machine ran before a Re-pair ("" when the panel never saw one).
	installed string
	// reported: what was already written to the audit log for this enrollment (once each).
	reported map[string]bool
	cancel   context.CancelFunc
	pairedAt time.Time
	held     *Created // the code and command, kept for one Reveal (made by the panel's replay after a step-up)
	holder   string   // who may Reveal it (see Hold)
}

// Why an enrollment is still waiting (Enrollment.Hint).
const (
	// HintAgentTooOld: what answers at the address is a Fairy-agent from before pairing v2 (or has no pairing
	// endpoint at all). It must be updated first; running the command does that, and the same code then pairs.
	HintAgentTooOld = "agent_too_old"
	// HintLegacyJoinCode: the agent knows pairing v2, but the join code was stored on the machine for the old
	// exchange (the command was not the panel's, or an older install script ran it).
	HintLegacyJoinCode = "legacy_join_code"
	// HintUnverified: something answered "paired" without proving it has the join code. It was not added.
	HintUnverified = "unverified_answer"
	// HintNotTailscale: the address is outside Tailscale's ranges and was not contacted.
	HintNotTailscale = "not_tailscale"
	// HintRateLimited: the agent answered that its budget of pairing requests for this minute is used up
	// (429). The panel asks less often for a while; the enrollment and its code are not affected.
	HintRateLimited = "rate_limited"
)

// Created is what the dialog shows once: the code and the commands.
type Created struct {
	Enrollment
	Code    string     `json:"code"`
	Command string     `json:"command"`
	Panel   string     `json:"panel_address"`
	Release Installers `json:"release"` // the Fairy-agent release the command installs (checked by the panel)
}

// Hooks connect the manager to the panel.
type Hooks struct {
	// Config is the current configuration (config.yaml + managed hosts) for validation.
	Config func() *config.Panel
	// Add starts polling a new host; Remove stops polling one (Re-pair replaces).
	Add    func(managed.Entry) error
	Remove func(id string)
	// Fresh, when set, is told about a host that is ADDED (not re-paired), after it was stored and before its
	// polling starts: what the panel still keeps under its id is from another machine and is not its own.
	Fresh func(h managed.Host)
	// Online reports whether the host answered its last poll (and the error if not).
	Online func(id string) (bool, string)
	// Event is told about results (audit log, notifications): kind is paired, online, failed, expired, cancelled.
	Event func(e Enrollment, kind, detail string)
	// AgentVersion is the version a polled host's agent reports now ("" when unknown). Optional: without it a
	// Re-pair goes by the version the host reported when it was paired.
	AgentVersion func(id string) string
}

// Manager holds the enrollments (in memory: a panel restart drops pending ones; make a new code).
type Manager struct {
	Store *managed.Store
	Hooks Hooks
	// Pair asks the machine once (pairing v2, see HTTPPair): the status, the agent's answer on 200 (only when
	// it was verified with key), the agent's error code otherwise. Tests and the demo replace it.
	Pair func(ctx context.Context, url string, key []byte) (int, enroll.Response, string, error)
	Now  func() time.Time
	// Interval is the time between two pairing requests to one machine (default PairInterval, 15 s; pace.go).
	Interval    time.Duration
	OnlineWait  time.Duration // how long "paired" waits for the first good poll (default 90 s)
	PanelAddr   string        // the panel's Tailscale IPv4 address, for the command
	PanelSource string        // where PanelAddr came from: "config", "auto" (detected) or "demo"
	PanelVia    string        // for "auto": how it was found ("interface tailscale0", "Tailscale LocalAPI")
	// Scripts checks the pinned Fairy-agent release's install scripts (signed SHA256SUMS) before any command
	// is shown; nil or a failing check refuses Add host / Re-pair.
	Scripts ScriptSource
	// AllowNonTailscale (enroll.allow_non_tailscale in config.yaml) lets Add host and Re-pair pair with a
	// machine at an address outside Tailscale's ranges. Off by default: see tailnet.go.
	AllowNonTailscale bool

	mu       sync.Mutex
	items    map[string]*Enrollment
	making   map[string]bool      // host ids for which Create is between its "already waiting?" check and its entry
	nextAsk  map[string]time.Time // per host URL: when the next pairing request may go out (turn, holdOff)
	commitMu sync.Mutex           // a pairing's store + start of polling vs Cancel / CancelHost (taken before mu)
}

// New builds a manager with the real HTTP pairing call. scripts checks the install scripts (see Verifier).
func New(store *managed.Store, hooks Hooks, panelAddr string, scripts ScriptSource) *Manager {
	m := &Manager{Store: store, Hooks: hooks, Now: time.Now, Interval: PairInterval,
		OnlineWait: 90 * time.Second, PanelAddr: panelAddr, Scripts: scripts, items: map[string]*Enrollment{}, making: map[string]bool{}}
	m.Pair = func(ctx context.Context, url string, key []byte) (int, enroll.Response, string, error) {
		return httpPair(ctx, m.pairClient(), url, key)
	}
	return m
}

// pairClient is the client of this manager's pairing requests: to Tailscale addresses only, unless the
// configuration allows others.
func (m *Manager) pairClient() *http.Client {
	if m.AllowNonTailscale {
		return pairClientAny
	}
	return pairClientTailnet
}

var (
	nameRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,62}$`)
	addrRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)
	upsRe       = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	macRe       = regexp.MustCompile(`^[0-9A-Fa-f]{2}([:-][0-9A-Fa-f]{2}){5}$`)
	bmcTargetRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
)

// Validation is a user error in the dialog (400).
type Validation struct{ Msg string }

func (v *Validation) Error() string { return v.Msg }

func invalid(f string, a ...any) error { return &Validation{Msg: fmt.Sprintf(f, a...)} }

// hostEntry checks the dialog and builds the host the enrollment will add.
func (m *Manager) hostEntry(req Request, existing *managed.Host, nodeSystemID func(string) string) (managed.Host, error) {
	if existing != nil {
		h := *existing
		return h, nil
	}
	req.Name = strings.TrimSpace(req.Name)
	if !nameRe.MatchString(req.Name) {
		return managed.Host{}, invalid("name: the machine's Tailscale name (letters, digits and -), e.g. agent-1")
	}
	addr := strings.ToLower(req.Name)
	if a := strings.TrimSpace(req.Address); a != "" {
		if net.ParseIP(a) == nil && !addrRe.MatchString(a) {
			return managed.Host{}, invalid("address: a Tailscale IP (100.x.y.z) or name")
		}
		addr = a
	}
	if req.OS != "linux" && req.OS != "windows" {
		return managed.Host{}, invalid("os must be linux or windows")
	}
	if req.Site != "" && !config.ValidSite(req.Site) {
		return managed.Host{}, invalid("site: 1-40 letters, digits, spaces and . _ ' & -")
	}
	o := req.Options
	if o.UPSName != "" {
		if !upsRe.MatchString(o.UPSName) {
			return managed.Host{}, invalid("UPS name: letters, digits, _ . - (as in upsc -l)")
		}
		o.UPS = true
	}
	if req.ProxmoxNew {
		if req.OS != "linux" || strings.TrimSpace(req.Node) != "" {
			return managed.Host{}, invalid("a new Proxmox node is a Linux machine, not attached to another node")
		}
	}
	if req.OS != "windows" {
		o.CoreTemp = false
	}
	id := managed.IDFor(req.Name)
	if id == "" {
		return managed.Host{}, invalid("name: cannot make an id from it")
	}
	h := managed.Host{ID: id, Name: req.Name, URL: "http://" + net.JoinHostPort(addr, "9101"), Site: req.Site, OS: req.OS,
		Options: o, ProxmoxWanted: req.ProxmoxNew}
	if n := strings.TrimSpace(req.Node); n != "" {
		h.Node = n
		h.SystemID = nodeSystemID(n)
		if h.SystemID == "" {
			return managed.Host{}, invalid("node %q is not a Proxmox node the panel knows", n)
		}
	}
	if len(req.WolMACs) > 0 {
		if len(req.WolMACs) > 8 {
			return managed.Host{}, invalid("at most 8 MAC addresses")
		}
		for _, mac := range req.WolMACs {
			if !macRe.MatchString(strings.TrimSpace(mac)) {
				return managed.Host{}, invalid("MAC %q must look like aa:bb:cc:dd:ee:ff", mac)
			}
		}
		if req.WolRelay == "" {
			return managed.Host{}, invalid("Wake-on-LAN: choose the agent that sends the packets (on the same network)")
		}
		macs := make([]string, len(req.WolMACs))
		for i, x := range req.WolMACs {
			macs[i] = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(x), "-", ":"))
		}
		h.Wol = &config.Wol{MACs: macs, Relay: req.WolRelay}
	}
	if r, t := strings.TrimSpace(req.BMCRelay), strings.TrimSpace(req.BMCTarget); r != "" || t != "" {
		if r == "" || !bmcTargetRe.MatchString(t) {
			return managed.Host{}, invalid("BMC power-on: choose the relay agent and give its bmc_relay target id (a-z, 0-9, -)")
		}
		h.BMCPower = &config.BMCPower{Relay: r, Target: t}
	}
	return h, nil
}

// Create starts an Add host (existing == nil) or a Re-pair of a managed host. nodeSystemID maps a Proxmox node
// name to its system id ("" = unknown). The code is returned once and not kept.
func (m *Manager) Create(ctx context.Context, req Request, existing *managed.Host, by string, nodeSystemID func(string) string) (Created, error) {
	h, err := m.hostEntry(req, existing, nodeSystemID)
	if err != nil {
		return Created{}, err
	}
	if m.PanelAddr == "" {
		return Created{}, invalid("the panel does not know its own Tailscale address: set enroll: {panel_address: 100.x.y.z} in config.yaml")
	}
	if a, err := netip.ParseAddr(m.PanelAddr); err != nil || !a.Is4() {
		return Created{}, invalid("the panel only found a Tailscale IPv6 address (%s), and agents pair over IPv4 only: give this machine a Tailscale IPv4 or set enroll: {panel_address: 100.x.y.z} in config.yaml", m.PanelAddr)
	}
	if a := literalOutsideTailscale(h.URL); a != "" && !m.AllowNonTailscale {
		return Created{}, invalid("%s is not a Tailscale address (100.64.0.0/10 or fd7a:115c:a1e0::/48). The panel pairs over Tailscale only: "+
			"give the machine's Tailscale name or address, or set enroll: {allow_non_tailscale: true} in config.yaml to pair over another network", a)
	}
	cfg := m.Hooks.Config()
	if existing == nil {
		// the id and Wake-on-LAN must pass now; the tokens are filled in after pairing
		e := h.Entry(managed.Secret{Token: strings.Repeat("x", 32), ControlToken: strings.Repeat("y", 32)})
		if err := managed.Check(cfg, e); err != nil {
			return Created{}, invalid("%s", strings.TrimSpace(strings.ReplaceAll(err.Error(), "config invalid:\n  -", "")))
		}
	}
	// One Add host per host at a time, decided here in one step (N4): the host is either waiting already, or
	// another request for it is between this check and its own entry in the list (making the code means
	// asking the release first, which takes time). The mark is taken off when this call returns, by which time
	// its entry is in the list or it has failed.
	m.mu.Lock()
	busy := m.making[h.ID]
	for _, x := range m.items {
		busy = busy || (x.HostID == h.ID && !x.Done)
	}
	if busy {
		m.mu.Unlock()
		return Created{}, invalid("an Add host for %s is already waiting: cancel it first", h.Name)
	}
	if m.making == nil {
		m.making = map[string]bool{}
	}
	m.making[h.ID] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.making, h.ID)
		m.mu.Unlock()
	}()
	if m.Scripts == nil {
		return Created{}, &Unavailable{Msg: "Add host is off: the panel cannot check the Fairy-agent install script"}
	}
	inst, err := m.Scripts.Installers(ctx) // before any code exists: no command without a checked script
	if err != nil {
		return Created{}, err
	}
	if !inst.valid() {
		return Created{}, &Unavailable{Msg: "the checked Fairy-agent release has an unusable download address or hash"}
	}
	if lacks := inst.Lacks(h.OS); len(lacks) > 0 {
		// No command is made for a release that could only pair the old way (the panel would never accept
		// what that agent answers) or only take the join code on its command line.
		return Created{}, &TooOld{Msg: fmt.Sprintf("Fairy-agent release %s is too old for this panel: its install script for %s lacks %s. "+
			"The panel does not pair the old, unverified way and does not put the join code on a command line. "+
			"Use a newer release: set enroll: {agent_release: build-N} in config.yaml, or update the panel", releaseName(inst), osName(h.OS), strings.Join(lacks, "; "))}
	}
	code, err := enroll.NewCode()
	if err != nil {
		return Created{}, err
	}
	key, err := enroll.PairingKey(code)
	if err != nil {
		return Created{}, err
	}
	now := m.Now().UTC()
	idb := make([]byte, 8)
	_, _ = rand.Read(idb)
	e := &Enrollment{ID: hex.EncodeToString(idb), HostID: h.ID, Name: h.Name, URL: h.URL, OS: h.OS, Site: h.Site,
		Node: h.Node, Options: h.Options, Repair: existing != nil, State: Waiting,
		Message: "waiting for the machine: run the command there", CreatedAt: now, ExpiresAt: now.Add(enroll.TTL),
		CreatedBy: by, key: key, host: h, renamed: existing != nil && req.NodeRenamed, reported: map[string]bool{}}
	if existing != nil {
		e.installed = existing.AgentVersion
		if m.Hooks.AgentVersion != nil {
			if v := m.Hooks.AgentVersion(h.ID); v != "" {
				e.installed = v
			}
		}
		e.Warning = olderRelease(e.installed, inst)
	}
	h.AddedBy = by
	e.host = h
	ctx, cancel := context.WithDeadline(context.Background(), e.ExpiresAt)
	e.cancel = cancel
	command := Command(inst, h, code, m.PanelAddr)
	if command == "" { // cannot happen for a code and an address the panel made itself: no enrollment without a command
		cancel()
		return Created{}, errors.New("the install command could not be made")
	}
	m.mu.Lock()
	m.items[e.ID] = e
	m.prune(now)
	cp := m.copyOf(e)
	m.mu.Unlock()
	go m.run(ctx, e)
	return Created{Enrollment: cp, Code: code, Command: command, Panel: m.PanelAddr, Release: inst}, nil
}

// Command is the one line to run on the machine. It downloads the install script of the checked release,
// refuses to run it unless its SHA-256 is the one the panel verified against the signed SHA256SUMS, and runs it
// against that same release (never "latest"), with the fingerprint of the verified SHA256SUMS and the pairing
// v2 option.
//
// The join code is on no program's command line (any local user can read those while the installer runs):
//   - Linux: `printf '%s' 'CODE' | [sudo ]env ... sh "$f" ... --join-stdin`. printf is a builtin of the shell
//     the line is pasted into, so the code is an argument of no process; install.sh reads it from standard
//     input and hands it to `fairy-agent join` the same way.
//   - Windows: `$env:FAIRY_JOIN_CODE='CODE'` in the PowerShell window; install.ps1 takes it from there,
//     removes it, and gives it to `fairy-agent join` through that one program's environment.
//
// On Windows the script never touches the disk before it runs: the line downloads it into memory, checks
// the SHA-256 of exactly those bytes and runs them as a script block, so there is no file that another
// program could swap between the check and the run. The run sits inside the check ("if (hash -eq ...) { run }
// else { throw }"), so a check that cannot be made runs nothing.
//
// There is one form per system. Command returns "" for a release whose script cannot be used this way
// (Installers.Lacks) and for a code or panel address that is not what the panel itself makes; Create refuses
// those before it gets here.
func Command(in Installers, h managed.Host, code, panel string) string {
	if len(in.Lacks(h.OS)) > 0 || !in.valid() || !codeRe.MatchString(code) || !panelRe.MatchString(panel) ||
		(h.Options.UPSName != "" && !upsRe.MatchString(h.Options.UPSName)) {
		return ""
	}
	o := h.Options
	var b strings.Builder
	if h.OS == "windows" {
		// The run is inside the "if" whose condition is the check, and nothing follows that statement. In
		// PowerShell an exception from a .NET call ends only the statement it is in: with the check and the run as
		// two statements, a hash expression that throws (SHA256::Create() under the FIPS policy on an old .NET
		// Framework) skipped the check and ran the unchecked bytes. Now an error in the condition ends the one
		// statement and runs neither branch; a wrong hash goes to "else" and throws. try / finally takes the join
		// code out of the window's environment also when the script never starts. (The line is the one in the
		// agent's guide, AGENT.md "Add a host from the panel"; both must change together.)
		fmt.Fprintf(&b, `$u='%s'; $b=(New-Object Net.WebClient).DownloadData("$u/%s"); `+
			`if ((([Security.Cryptography.SHA256]::Create().ComputeHash($b) | ForEach-Object { $_.ToString('x2') }) -join '') -eq '%s') `+
			`{ $env:%s='%s'; try { & ([scriptblock]::Create([Text.Encoding]::UTF8.GetString($b))) -ReleaseUrl $u -SumsSha256 %s -PanelAddress %s %s`,
			in.Base, InstallPS1, in.PS1, JoinCodeEnv, code, in.Sums, panel, FlagPairV2PS1)
		for _, f := range []struct {
			on   bool
			flag string
		}{{o.PowerControl, "-PowerControl"}, {o.SelfUpdate, "-SelfUpdate"}, {o.WolRelay, "-Wol"}, {o.CoreTemp, "-CoreTemp"}, {o.IPMI, "-Ipmi"}} {
			if f.on {
				b.WriteString(" " + f.flag)
			}
		}
		if o.UPSName != "" {
			b.WriteString(" -UpsName " + o.UPSName)
		} else if o.UPS {
			b.WriteString(" -Ups")
		}
		fmt.Fprintf(&b, ` } finally { Remove-Item Env:%s -ErrorAction SilentlyContinue } } `+
			`else { throw 'install.ps1 is not the release the panel checked: not run' }`, JoinCodeEnv)
		return b.String()
	}
	sudo := "sudo "
	if h.Node != "" || h.ProxmoxWanted { // a Proxmox node: a root shell, usually without sudo
		sudo = ""
	}
	fmt.Fprintf(&b, `f=$(mktemp) && curl -fsSL --proto '=https' -o "$f" %s/%s && echo "%s  $f" | sha256sum -c - && `+
		`printf '%%s' '%s' | %senv FAIRY_RELEASE_URL=%s sh "$f" --sums-sha256 %s %s %s --panel %s`,
		in.Base, InstallSH, in.SH, code, sudo, in.Base, in.Sums, FlagJoinStdinSH, FlagPairV2SH, panel)
	for _, f := range []struct {
		on   bool
		flag string
	}{{o.PowerControl, "--power-control"}, {o.SelfUpdate, "--self-update"}, {o.WolRelay, "--wol"}, {o.IPMI, "--ipmi"},
		{h.ProxmoxWanted, "--proxmox"}} {
		if f.on {
			b.WriteString(" " + f.flag)
		}
	}
	if o.UPSName != "" {
		b.WriteString(" --ups-name " + o.UPSName)
	} else if o.UPS {
		b.WriteString(" --ups")
	}
	return b.String()
}

// What goes into the command between quotes: a join code as enroll.NewCode makes it and the panel's IPv4
// address, nothing else.
var (
	codeRe  = regexp.MustCompile(`^[A-Z2-7]{4}(-[A-Z2-7]{4}){7}$`)
	panelRe = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)
)

// olderRelease is the warning for a Re-pair whose command installs an older Fairy-agent than the machine runs
// ("" when it does not, or when either build number is unknown). The install scripts the panel accepts are
// written to keep a newer program (Fairy-agent v0.6.0 on; on Windows only when the program folder passes the
// installer's own check), but the panel cannot see what happened on the machine: it says what it knows before,
// and paired() says afterwards when the agent that answers is an older build than before.
func olderRelease(installed string, in Installers) string {
	have, ok1 := release.BuildNumber(installed)
	get, ok2 := release.BuildNumber(in.Version)
	if !ok1 || !ok2 || have <= get {
		return ""
	}
	return fmt.Sprintf("This machine runs Fairy-agent %s, which is newer than the release this command installs (%s, Fairy-agent %s). "+
		"The installer is meant to keep the newer program and only renew the settings and the pairing. Check the agent's version on the Updates page "+
		"afterwards: if it is older than before, update it there. To install the same or a newer release instead, set enroll: {agent_release: build-%d} "+
		"(or newer) in config.yaml and make a new code.",
		clip(installed, 64), in.Tag, in.Version, have)
}

func (m *Manager) copyOf(e *Enrollment) Enrollment {
	c := *e
	c.key, c.cancel, c.held, c.holder, c.reported = nil, nil, nil, "", nil
	return c
}

func releaseName(in Installers) string {
	if in.Version != "" {
		return in.Tag + " (Fairy-agent " + in.Version + ")"
	}
	return in.Tag
}

func osName(os string) string {
	if os == "windows" {
		return "Windows"
	}
	return "Linux"
}

// Hold keeps a new enrollment's code and command for one Reveal by holder: the panel made it by itself right
// after a step-up sign-in, so the browser has not seen the code yet. holder is the caller's name for who
// confirmed (the web layer: the account and the browser and address the confirmation came back to); only a
// Reveal with the same holder gets the code. An empty holder holds nothing.
func (m *Manager) Hold(cr Created, holder string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.items[cr.ID]; ok && !e.Done && holder != "" {
		c := cr
		e.held, e.holder = &c, holder
	}
}

// Reveal returns a held code and command once, to the holder it is kept for, while the enrollment still waits.
// A Reveal by anyone else gets nothing and leaves the code where it is.
func (m *Manager) Reveal(id, holder string) (Created, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.items[id]
	if !ok || e.held == nil || e.Done || holder == "" || subtle.ConstantTimeCompare([]byte(e.holder), []byte(holder)) != 1 {
		return Created{}, false
	}
	c := *e.held
	e.held, e.holder = nil, ""
	c.Enrollment = m.copyOf(e)
	return c, true
}

// prune forgets finished enrollments older than an hour. Caller holds m.mu.
func (m *Manager) prune(now time.Time) {
	for id, e := range m.items {
		if e.Done && now.Sub(e.ExpiresAt) > time.Hour {
			delete(m.items, id)
		}
	}
}

// Get returns one enrollment.
func (m *Manager) Get(id string) (Enrollment, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.items[id]
	if !ok {
		return Enrollment{}, false
	}
	return m.copyOf(e), true
}

// List returns every enrollment, newest first.
func (m *Manager) List() []Enrollment {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Enrollment{}
	for _, e := range m.items {
		out = append(out, m.copyOf(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Cancel stops a waiting enrollment (also one whose pairing answer is being stored: see commit).
func (m *Manager) Cancel(id string) bool {
	m.commitMu.Lock()
	defer m.commitMu.Unlock()
	m.mu.Lock()
	e, ok := m.items[id]
	if !ok || e.Done || e.State != Waiting {
		m.mu.Unlock()
		return false
	}
	e.State, e.Message, e.Done = Cancelled, "cancelled", true
	cp := m.copyOf(e)
	e.cancel()
	m.mu.Unlock()
	m.Hooks.Event(cp, "cancelled", "")
	return true
}

// commit stores a paired host's tokens and starts polling it, unless the enrollment was cancelled meanwhile
// (Cancel, or Remove of the host while its Re-pair was pairing): commitMu makes a cancel either wait for a
// commit in progress (and the caller then removes the result) or stop it before anything is stored.
func (m *Manager) commit(e *Enrollment, h managed.Host, sec managed.Secret, entry managed.Entry, caPEM []byte, fail func(string)) bool {
	m.commitMu.Lock()
	defer m.commitMu.Unlock()
	m.mu.Lock()
	cancelled := e.Done
	m.mu.Unlock()
	if cancelled {
		return false
	}
	if e.Repair {
		// The same checks as Add host, against the configuration without the host as it was stored: what the
		// machine answers now (its Proxmox node above all) must not collide with anything else the panel has.
		// Only then is the old pairing forgotten; a refused answer leaves the host as it was.
		if err := managed.Check(withoutHost(m.Hooks.Config(), e.host), entry); err != nil {
			fail("cannot re-pair it: " + checkText(err) + ". Nothing was changed on the panel; make a new code to try again")
			return false
		}
		m.Hooks.Remove(h.ID)
	} else if err := managed.Check(m.Hooks.Config(), entry); err != nil {
		fail("cannot add it: " + err.Error())
		return false
	}
	if caPEM != nil {
		if err := m.Store.PutCA(h.ID, caPEM); err != nil {
			fail("cannot save the node's CA: " + err.Error())
			return false
		}
	} else if e.Repair { // re-paired without a Proxmox grant: the old node's CA goes with its old token
		if err := os.Remove(m.Store.CAPath(h.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			fail("cannot delete the node's old CA: " + err.Error())
			return false
		}
	}
	// Store.Put replaces the secrets file: the tokens of the previous pairing (read, control, Proxmox) are gone
	// from disk, and Hooks.Remove above made the poller forget them.
	if err := m.Store.Put(h, sec); err != nil {
		fail("cannot save it: " + err.Error())
		return false
	}
	if !e.Repair && m.Hooks.Fresh != nil {
		m.Hooks.Fresh(h)
	}
	if err := m.Hooks.Add(entry); err != nil {
		fail("saved, but polling did not start (" + err.Error() + "): restart the panel")
		return false
	}
	return true
}

// withoutHost is cfg without the entries a managed host has in it (its agent, its systems: entry, its Proxmox
// node): what a Re-pair's answer is checked against. Entries from config.yaml with the same ids stay, so a
// host that config.yaml shadows is refused like a new one would be.
func withoutHost(cfg *config.Panel, old managed.Host) *config.Panel {
	c := *cfg
	c.Agents = slices.DeleteFunc(slices.Clone(cfg.Agents), func(a config.Agent) bool { return a.Managed && a.ID == old.ID })
	if o := old.Override(); o != nil {
		c.Systems = slices.DeleteFunc(slices.Clone(cfg.Systems), func(x config.SystemOverride) bool { return x.ID == o.ID })
	}
	if old.Proxmox != nil {
		c.Proxmox = slices.DeleteFunc(slices.Clone(cfg.Proxmox), func(x config.Proxmox) bool { return x.Managed && x.ID == old.Proxmox.ID })
	}
	return &c
}

// checkText is a managed.Check error as one line for the dialog.
func checkText(err error) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(err.Error(), "config invalid:\n  -", "")), " ")
}

// CancelHost cancels every unfinished enrollment of a host (Remove): one that is pairing right now either
// stops before it stores anything or has finished storing when CancelHost returns.
func (m *Manager) CancelHost(hostID string) {
	m.commitMu.Lock()
	m.mu.Lock()
	var gone []Enrollment
	for _, e := range m.items {
		if e.HostID == hostID && !e.Done {
			e.State, e.Message, e.Done = Cancelled, "cancelled: the host was removed", true
			e.cancel()
			gone = append(gone, m.copyOf(e))
		}
	}
	m.mu.Unlock()
	m.commitMu.Unlock()
	for _, cp := range gone {
		m.Hooks.Event(cp, "cancelled", "")
	}
}

func (m *Manager) set(e *Enrollment, state, msg string, done bool) Enrollment {
	return m.setHint(e, state, msg, "", done)
}

// setHint is set with the reason the enrollment is still waiting (cleared by every other state change).
func (m *Manager) setHint(e *Enrollment, state, msg, hint string, done bool) Enrollment {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.Done && state != Online { // cancelled meanwhile
		return m.copyOf(e)
	}
	e.State, e.Message, e.Hint, e.Done = state, msg, hint, done
	return m.copyOf(e)
}

// report writes one audit entry of this kind for the enrollment (the first time only: the loop asks every few
// seconds and would otherwise fill the log).
func (m *Manager) report(e *Enrollment, kind, detail string) {
	m.mu.Lock()
	first := !e.reported[kind] && !e.Done
	if first {
		e.reported[kind] = true
	}
	cp := m.copyOf(e)
	m.mu.Unlock()
	if first {
		m.Hooks.Event(cp, kind, detail)
	}
}

func (m *Manager) run(ctx context.Context, e *Enrollment) {
	defer e.cancel()
	// However the enrollment ends (paired, expired, cancelled): the pairing key and a code still held for the
	// browser leave the panel's memory with it. The finished entry stays listed for an hour without them.
	defer func() {
		m.mu.Lock()
		clear(e.key)
		e.key, e.held, e.holder = nil, nil, ""
		m.mu.Unlock()
	}()
	limited := 0 // answers in a row that said the agent's pairing budget is used up
	for {
		// This address's turn (pace.go): at once for the first request, then inside the agent's budget.
		for wait := m.turn(e.URL); wait > 0; wait = m.turn(e.URL) {
			tm := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				tm.Stop()
				m.expire(e)
				return
			case <-tm.C:
			}
		}
		if ctx.Err() != nil {
			m.expire(e)
			return
		}
		m.mu.Lock()
		done := e.Done
		e.Attempts++
		m.mu.Unlock()
		if done {
			return
		}
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, resp, code, err := m.Pair(actx, e.URL, e.key) // e.key is only touched by this goroutine
		cancel()
		var elsewhere *NotTailscale
		var unverified *Unverified
		if err != nil || status != 429 {
			limited = 0
		}
		// Only a verified answer pairs. Everything else leaves the enrollment waiting with its code intact:
		// nothing below ends it, and nothing below makes the panel ask in another way.
		switch {
		case err == nil && status == 200:
			m.paired(e, resp)
			return
		case errors.As(err, &elsewhere):
			m.setHint(e, Waiting, "not contacted: "+hostOf(e.URL)+" leads to "+elsewhere.Addr+", which is not a Tailscale address. The panel pairs over Tailscale only "+
				"(to pair over another network: enroll: {allow_non_tailscale: true} in config.yaml)", HintNotTailscale, false)
		case errors.As(err, &unverified):
			// Something answered "paired" without proving it has the join code: an old agent's answer in
			// clear, another program on that port, a replayed or altered answer. Never adopted.
			m.report(e, "unverified", "an answer from "+e.URL+" did not prove the join code ("+unverified.Why+"): not added")
			m.setHint(e, Waiting, notAgentMessage(hostOf(e.URL), unverified.Why), HintUnverified, false)
		case err != nil:
			m.set(e, Waiting, "waiting for the machine ("+devices.Describe(err)+")", false)
		case status == 404 && code == "pairing_inactive":
			m.set(e, Waiting, "the agent answers but has no join code yet: run the command (or it expired there)", false)
		case status == 400 && code == "wrong_version":
			m.setHint(e, Waiting, "the agent on the machine holds a join code for the old pairing (stored by another command or an older install script). "+
				"The panel does not pair that way: run the command shown here on the machine", HintLegacyJoinCode, false)
		case status == 400 || status == 404:
			// An agent from before pairing v2 answers 400 to the request (it expects the old fixed proof); one
			// from before Add host has no /pair at all. The panel does not fall back to the old exchange. It
			// keeps asking: the command installs a newer agent, and this same code then pairs.
			m.report(e, "agent_too_old", fmt.Sprintf("%s answered %d to a pairing v2 request: a Fairy-agent from before pairing v2; not paired the old way", e.URL, status))
			m.setHint(e, Waiting, tooOldMessage, HintAgentTooOld, false)
		case code == "wrong_address":
			m.set(e, Waiting, "the agent refuses: the panel address in the command ("+m.PanelAddr+") is not where the panel connects from; set enroll.panel_address", false)
		case code == "wrong_code":
			m.set(e, Waiting, "the agent has a different join code (an older command?): run this one", false)
		case status == 429:
			// The agent's budget of pairing requests for this minute is used up. The panel's own pace stays
			// under it, so something else is asking that agent too (another panel, a second enrollment under
			// another name for the same machine). The panel asks less often until the agent answers again;
			// the enrollment and its code stay as they are. What was found out before stands.
			limited++
			again := m.holdOff(e.URL, limited)
			if hint, msg := m.hintOf(e); hint == HintAgentTooOld || hint == HintLegacyJoinCode {
				m.setHint(e, Waiting, msg, hint, false)
			} else {
				m.setHint(e, Waiting, fmt.Sprintf("the agent has answered all the pairing requests it takes in one minute (something else may be asking it too): "+
					"the panel asks again in about %d s. Nothing is lost: the code stays valid", int(math.Ceil(again.Seconds()))), HintRateLimited, false)
			}
		default:
			m.set(e, Waiting, fmt.Sprintf("the agent answered %d", status), false)
		}
	}
}

// expire ends an enrollment whose time is up (or that was cancelled meanwhile: then nothing changes).
func (m *Manager) expire(e *Enrollment) {
	m.mu.Lock()
	fin := !e.Done
	if fin {
		e.State, e.Message, e.Done = Expired, "the code expired before the machine ran the command: make a new one", true
		if e.Hint == HintUnverified { // the hint stays as it always did; the last message says it too
			e.Message = notAgentExpired(hostOf(e.URL))
		}
	}
	cp := m.copyOf(e)
	m.mu.Unlock()
	if fin {
		m.Hooks.Event(cp, "expired", "")
	}
}

// shownNameRe is a computer name the panel shows as the machine's own word: DNS and Windows names.
var shownNameRe = regexp.MustCompile(`^[A-Za-z0-9_]([A-Za-z0-9._-]{0,62}[A-Za-z0-9_])?$`)

// shownHostname is the agent's hostname when it looks like one, "" otherwise (it is shown and logged).
func shownHostname(h string) string {
	if !shownNameRe.MatchString(h) {
		return ""
	}
	return h
}

// notAgentMessage is what the dialog shows while something answers "paired" at the machine's address without
// proving it has the join code. The enrollment keeps waiting (the real agent can still pair with the same
// code), so the text must say plainly that what answered is not the agent, that nothing of it was used, and
// what to do. It names the address and the reason (Unverified.Why, fixed texts of this package) and nothing
// of what was sent or received.
func notAgentMessage(host, why string) string {
	return "not added: something answers at " + host + ", but it is not a Fairy agent that has this join code (" + why + "). " +
		"Nothing it sent was used, and the panel keeps waiting. " +
		"If the command has not run on the machine yet, another program or another machine answers at that address: check the name and the address. " +
		"If it has run, cancel this, make a new code and run the new command"
}

// notAgentExpired is the last message of an enrollment that ran out while only such answers came.
func notAgentExpired(host string) string {
	return "the code expired and nothing was added: until the end, what answered at " + host + " was not a Fairy agent that has the join code. " +
		"Check the name and the address, then make a new code"
}

// tooOldMessage is what the dialog shows while an agent from before pairing v2 answers.
const tooOldMessage = "the Fairy-agent on that machine is too old for this panel: it does not know pairing v2, and the panel does not pair the old, " +
	"unverified way. Update it first: run the command shown here on the machine. It installs the newer agent, and the panel pairs with " +
	"this same code as soon as that agent is up"

func (m *Manager) hintOf(e *Enrollment) (hint, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return e.Hint, e.Message
}

// paired takes a verified answer: only run calls it, and only with what Pair returned for status 200 without
// an error (HTTPPair returns that only for an answer that opened under the join code's key).
func (m *Manager) paired(e *Enrollment, resp enroll.Response) {
	h := e.host
	h.AddedAt = m.Now().UTC()
	h.Pairing = managed.PairingV2
	// The answer is verified (it was made with the join code), but its texts are still a machine's word: the
	// version is kept only when it is a version and the name only when it is a computer name. Anything else is
	// left empty, never cut or cleaned into something the machine did not say.
	version := ""
	if release.ValidVersion(resp.AgentVersion) {
		version = resp.AgentVersion
	}
	h.AgentVersion, h.Hostname, h.ControlPaired = version, shownHostname(resp.Hostname), resp.ControlToken != ""
	sec := managed.Secret{Token: resp.Token, ControlToken: resp.ControlToken}
	fail := func(msg string) {
		cp := m.set(e, Failed, msg, true)
		m.Hooks.Event(cp, "failed", msg)
	}
	if len(sec.Token) < 24 || (sec.ControlToken != "" && (len(sec.ControlToken) < 24 || sec.ControlToken == sec.Token)) {
		fail("the agent sent unusable tokens")
		return
	}
	if h.Options.Control() && sec.ControlToken == "" {
		h.Options.PowerControl, h.Options.SelfUpdate, h.Options.WolRelay = false, false, false
	}
	pveNote := ""
	var caPEM []byte
	if h.ProxmoxWanted {
		pve, ca, err := pveFromGrant(h, resp.Proxmox)
		switch {
		case err != nil:
			fail("the node's Proxmox token is unusable: " + err.Error())
			return
		case pve == nil:
			pveNote = " (the node sent no Proxmox API token: a cluster, or the command could not make one; it was added as a plain host)"
			// A plain host has no node. On a Re-pair the host still carries the node name the machine gave
			// itself at its first pairing and the id of the node it was then; kept, the name would link its
			// agent BY NAME to whatever node of any cluster is called the same (T-48).
			h.Proxmox, h.Node, h.SystemID = nil, "", ""
		case e.Repair && e.host.Proxmox != nil && !strings.EqualFold(pve.Node, e.host.Proxmox.Node) && !e.renamed:
			// Re-pair: the machine was added as one Proxmox node and now names another. It may have been
			// renamed, or something else answers in its place; either way it is the admin's call.
			fail(fmt.Sprintf("the machine now says it is Proxmox node %q, but it was added as %q. Nothing was changed on the panel. "+
				"If the node was renamed, Re-pair again and tick \"the node was renamed\"", clip(pve.Node, 80), e.host.Proxmox.Node))
			return
		default:
			h.Proxmox, h.Node, h.SystemID = pve, pve.Node, pve.IDPrefix+"node-"+normalise.Slug(pve.Node)
			sec.PVETokenSecret, caPEM = resp.Proxmox.TokenSecret, ca
		}
	}
	entry := m.Store.EntryOf(h, sec)
	if !m.commit(e, h, sec, entry, caPEM, fail) {
		return
	}
	note := pveNote
	if wanted := e.Options.Control(); wanted && sec.ControlToken == "" {
		note += " (the agent sent no control token: power / update / Wake-on-LAN stay off)"
	}
	detail := version
	if was, ok1 := release.BuildNumber(e.installed); ok1 && e.Repair {
		if now, ok2 := release.BuildNumber(version); ok2 && now < was {
			// the command put an older build over the one that ran before: say so, here and in the audit log
			note += fmt.Sprintf(" (the agent is now %s; before the Re-pair it was %s: an older build was installed, update it on the Updates page)",
				detail, clip(e.installed, 64))
			detail += " (was " + clip(e.installed, 64) + ": an older build was installed)"
		}
	}
	who := ""
	if name := shownHostname(resp.Hostname); name != "" {
		who = " (it calls itself " + name + ", at " + hostOf(e.URL) + ")"
	}
	m.mu.Lock()
	e.pairedAt = m.Now()
	e.Pairing, e.AgentHostname = managed.PairingV2, shownHostname(resp.Hostname)
	m.mu.Unlock()
	note = who + note
	cp := m.set(e, Paired, "paired: the agent proved it has the join code; tokens received, waiting for the first reading"+note, false)
	m.Hooks.Event(cp, "paired", detail)
	deadline := time.NewTimer(m.OnlineWait)
	defer deadline.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last := ""
	for {
		if ok, why := m.Hooks.Online(h.ID); ok {
			cp := m.set(e, Online, "online"+note, true)
			m.Hooks.Event(cp, "online", "")
			return
		} else if why != "" {
			last = why
		}
		select {
		case <-deadline.C:
			msg := "paired and added, but it does not answer yet"
			if last != "" {
				msg += " (" + last + ")"
			}
			m.set(e, Paired, msg, true)
			return
		case <-tick.C:
		}
	}
}

var (
	pveTokenIDRe = regexp.MustCompile(`^fairy@pve!panel-[a-z0-9-]{1,40}$`)
	pveSecretRe  = regexp.MustCompile(`^[A-Za-z0-9-]{16,128}$`)
	pveNodeRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,62}$`)
	hostNameRe   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

// pveFromGrant checks the Proxmox token a node sent with its pairing answer and makes the managed proxmox:
// entry: id prefix <site>-<node>- (unique among the panel's clusters; checked again by managed.Check), the API
// at the address the panel reaches the agent at (port 8006), the certificate checked against the node's own CA
// for the node's name. nil without a grant.
func pveFromGrant(h managed.Host, g *enroll.ProxmoxGrant) (*managed.PVE, []byte, error) {
	if g == nil {
		return nil, nil, nil
	}
	switch {
	case !pveTokenIDRe.MatchString(g.TokenID):
		return nil, nil, fmt.Errorf("token id %q is not fairy@pve!panel-<node>", clip(g.TokenID, 80))
	case !pveSecretRe.MatchString(g.TokenSecret):
		return nil, nil, errors.New("no usable token secret")
	case !pveNodeRe.MatchString(g.Node):
		return nil, nil, fmt.Errorf("node name %q", clip(g.Node, 80))
	case g.ServerName != "" && !hostNameRe.MatchString(g.ServerName):
		return nil, nil, fmt.Errorf("certificate name %q", clip(g.ServerName, 80))
	}
	var ca []byte
	if strings.TrimSpace(g.CAPEM) != "" {
		ca = []byte(g.CAPEM)
		if len(ca) > 64<<10 || !x509.NewCertPool().AppendCertsFromPEM(ca) {
			return nil, nil, errors.New("the node's CA certificate does not parse")
		}
	}
	prefix := normalise.Slug(g.Node) + "-"
	if s := normalise.Slug(h.Site); s != "" {
		prefix = s + "-" + prefix
	}
	if len(prefix) > 50 {
		prefix = strings.Trim(prefix[:49], "-") + "-"
	}
	u, err := url.Parse(h.URL)
	if err != nil || u.Hostname() == "" {
		return nil, nil, errors.New("the agent's address does not parse")
	}
	apiURL := "https://" + net.JoinHostPort(u.Hostname(), "8006")
	server := g.ServerName
	if server == "" {
		server = g.Node
	}
	return &managed.PVE{ID: strings.TrimSuffix(prefix, "-"), IDPrefix: prefix, Node: g.Node, URL: apiURL, ServerName: server,
		TokenID: g.TokenID, CA: ca != nil}, ca, nil
}

// clip is a text from a machine as the panel repeats it in a message: cleaned (textsafe.Clean) and cut to n
// bytes on a character boundary.
func clip(s string, n int) string { return textsafe.Cut(s, n) }

// hostOf is the host part of a host URL, for messages.
func hostOf(hostURL string) string {
	if u, err := url.Parse(hostURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return hostURL
}

// Unverified is a "paired" answer (status 200) that did not prove the join code: it does not open under the
// key bound to the request that was just sent. Such an answer is never used, and never retried another way.
type Unverified struct {
	Why string // for the dialog and the audit log; never part of the answer itself
}

func (u *Unverified) Error() string { return "the pairing answer was not verified: " + u.Why }

// HTTPPair is one pairing v2 request to a Tailscale address (see tailnet.go; never through a proxy, like every
// agent request): POST <url>/pair with a new nonce and its MAC under the join code's key. It returns the status,
// the agent's answer on 200, and the agent's error code otherwise.
//
// A 200 is only ever returned without an error for an answer that enroll.OpenAnswer accepted: encrypted and
// authenticated under a key made from the join code and this request's nonce. Anything else that arrives with
// status 200 (the old answer with tokens in clear, an answer to another request, a body that is too large) is
// an *Unverified error and an empty Response. There is no second attempt with the old exchange.
func HTTPPair(ctx context.Context, url string, key []byte) (int, enroll.Response, string, error) {
	return httpPair(ctx, pairClientTailnet, url, key)
}

// HTTPPairAny is HTTPPair without the Tailscale address check (enroll.allow_non_tailscale, and tests on
// loopback).
func HTTPPairAny(ctx context.Context, url string, key []byte) (int, enroll.Response, string, error) {
	return httpPair(ctx, pairClientAny, url, key)
}

// HTTPPairVia is HTTPPair through a transport of the caller's, without the Tailscale address check: for tests
// that hand the request to an agent's handler inside the test process (so that the agent's clock is the
// test's). The panel itself never calls it. Redirects are not followed, as everywhere.
func HTTPPairVia(ctx context.Context, rt http.RoundTripper, url string, key []byte) (int, enroll.Response, string, error) {
	return httpPair(ctx, &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, url, key)
}

func httpPair(ctx context.Context, client *http.Client, url string, key []byte) (int, enroll.Response, string, error) {
	var none enroll.Response
	ask, err := enroll.NewRequestV2(key) // a new nonce every time: an answer to an earlier request never opens
	if err != nil {
		return 0, none, "", err
	}
	b, err := json.Marshal(ask)
	if err != nil {
		return 0, none, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+enroll.Path, bytes.NewReader(b))
	if err != nil {
		return 0, none, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := client.Do(req)
	if err != nil {
		return 0, none, "", err
	}
	defer r.Body.Close()
	// The largest answer an agent sends is enroll.MaxAnswerBody; one byte more is read to tell "exactly that"
	// from "more".
	body, err := io.ReadAll(io.LimitReader(r.Body, enroll.MaxAnswerBody+1))
	if err != nil {
		return r.StatusCode, none, "", err
	}
	if r.StatusCode != 200 {
		var e struct {
			Code string `json:"code"`
		}
		if len(body) <= maxPairError {
			_ = json.Unmarshal(body, &e)
		}
		return r.StatusCode, none, clip(e.Code, 40), nil
	}
	if len(body) > enroll.MaxAnswerBody {
		return 200, none, "", &Unverified{Why: fmt.Sprintf("the answer is larger than a pairing answer can be (%d KiB)", enroll.MaxAnswerBody>>10)}
	}
	resp, err := enroll.OpenAnswer(key, ask, body)
	switch {
	case errors.Is(err, enroll.ErrNotV2):
		return 200, none, "", &Unverified{Why: "it is not a pairing v2 answer"}
	case err != nil:
		return 200, none, "", &Unverified{Why: "it was not made with this join code for this request"}
	}
	return 200, resp, "", nil
}

// maxPairError is the largest error answer the panel reads a code from.
const maxPairError = 4 << 10

// KnownMAC normalises MAC text from the dialog ("aa-bb-..", spaces, commas).
func KnownMAC(list string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == ';' || r == '\n' }) {
		if f = strings.TrimSpace(f); f != "" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}
