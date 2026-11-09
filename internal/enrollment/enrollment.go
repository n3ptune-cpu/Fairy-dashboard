// Package enrollment is the panel side of "Add host": an admin's request becomes a pending enrollment with a
// one-time join code (shown once, inside the install command); a background loop asks the new agent
// POST /pair with the code's proof every few seconds until it answers, the code expires or the admin cancels.
// On success the host and its tokens go into the panel's managed store and the poller starts polling it at
// once. Only the proof is kept, in memory.
package enrollment

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
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
	Name     string          `json:"name"`    // the machine's Tailscale (MagicDNS) name, also its display name
	Address  string          `json:"address"` // optional: Tailscale IP or full name instead of Name
	Site     string          `json:"site"`
	OS       string          `json:"os"` // linux | windows
	Options  managed.Options `json:"options"`
	Node     string          `json:"node"`      // optional: the Proxmox node it runs on (node:)
	WolMACs  []string        `json:"wol_macs"`  // optional: to wake this machine
	WolRelay string          `json:"wol_relay"` // the agent that sends the packets
	// BMCRelay / BMCTarget: optional, power it on through its BMC (a bmc_relay target of that agent).
	BMCRelay  string `json:"bmc_relay"`
	BMCTarget string `json:"bmc_target"`
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

	proof    string
	host     managed.Host
	cancel   context.CancelFunc
	pairedAt time.Time
	held     *Created // the code and command, kept for one Reveal (made by the panel's replay after a step-up)
}

// Created is what the dialog shows once: the code and the commands.
type Created struct {
	Enrollment
	Code    string `json:"code"`
	Command string `json:"command"`
	Panel   string `json:"panel_address"`
}

// Hooks connect the manager to the panel.
type Hooks struct {
	// Config is the current configuration (config.yaml + managed hosts) for validation.
	Config func() *config.Panel
	// Add starts polling a new host; Remove stops polling one (Re-pair replaces).
	Add    func(managed.Entry) error
	Remove func(id string)
	// Online reports whether the host answered its last poll (and the error if not).
	Online func(id string) (bool, string)
	// Event is told about results (audit log, notifications): kind is paired, online, failed, expired, cancelled.
	Event func(e Enrollment, kind, detail string)
}

// Manager holds the enrollments (in memory: a panel restart drops pending ones; make a new code).
type Manager struct {
	Store       *managed.Store
	Hooks       Hooks
	Pair        func(ctx context.Context, url, proof string) (int, enroll.Response, string, error)
	Now         func() time.Time
	Interval    time.Duration // between pairing attempts (default 3 s)
	OnlineWait  time.Duration // how long "paired" waits for the first good poll (default 90 s)
	PanelAddr   string        // the panel's Tailscale IPv4 address, for the command
	PanelSource string        // where PanelAddr came from: "config", "auto" (detected) or "demo"
	PanelVia    string        // for "auto": how it was found ("interface tailscale0", "Tailscale LocalAPI")
	InstallBase string        // https://github.com/<agent repo>/releases/latest/download

	mu    sync.Mutex
	items map[string]*Enrollment
}

// New builds a manager with the real HTTP pairing call.
func New(store *managed.Store, hooks Hooks, panelAddr, agentRepo string) *Manager {
	return &Manager{Store: store, Hooks: hooks, Pair: HTTPPair, Now: time.Now, Interval: 3 * time.Second,
		OnlineWait: 90 * time.Second, PanelAddr: panelAddr, InstallBase: "https://github.com/" + agentRepo + "/releases/latest/download",
		items: map[string]*Enrollment{}}
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
		return managed.Host{}, invalid("name: the machine's Tailscale name (letters, digits and -), e.g. nsx-furina")
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
	if req.OS != "windows" {
		o.CoreTemp = false
	}
	id := managed.IDFor(req.Name)
	if id == "" {
		return managed.Host{}, invalid("name: cannot make an id from it")
	}
	h := managed.Host{ID: id, Name: req.Name, URL: "http://" + net.JoinHostPort(addr, "9101"), Site: req.Site, OS: req.OS,
		Options: o}
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
func (m *Manager) Create(req Request, existing *managed.Host, by string, nodeSystemID func(string) string) (Created, error) {
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
	cfg := m.Hooks.Config()
	if existing == nil {
		// the id and Wake-on-LAN must pass now; the tokens are filled in after pairing
		e := h.Entry(managed.Secret{Token: strings.Repeat("x", 32), ControlToken: strings.Repeat("y", 32)})
		if err := managed.Check(cfg, e); err != nil {
			return Created{}, invalid("%s", strings.TrimSpace(strings.ReplaceAll(err.Error(), "config invalid:\n  -", "")))
		}
	}
	m.mu.Lock()
	for _, x := range m.items {
		if x.HostID == h.ID && !x.Done {
			m.mu.Unlock()
			return Created{}, invalid("an Add host for %s is already waiting: cancel it first", h.Name)
		}
	}
	m.mu.Unlock()
	code, err := enroll.NewCode()
	if err != nil {
		return Created{}, err
	}
	proof, _ := enroll.Proof(code)
	now := m.Now().UTC()
	idb := make([]byte, 8)
	_, _ = rand.Read(idb)
	e := &Enrollment{ID: hex.EncodeToString(idb), HostID: h.ID, Name: h.Name, URL: h.URL, OS: h.OS, Site: h.Site,
		Node: h.Node, Options: h.Options, Repair: existing != nil, State: Waiting,
		Message: "waiting for the machine: run the command there", CreatedAt: now, ExpiresAt: now.Add(enroll.TTL),
		CreatedBy: by, proof: proof, host: h}
	h.AddedBy = by
	e.host = h
	ctx, cancel := context.WithDeadline(context.Background(), e.ExpiresAt)
	e.cancel = cancel
	m.mu.Lock()
	m.items[e.ID] = e
	m.prune(now)
	cp := m.copyOf(e)
	m.mu.Unlock()
	go m.run(ctx, e)
	return Created{Enrollment: cp, Code: code, Command: Command(m.InstallBase, h, code, m.PanelAddr), Panel: m.PanelAddr}, nil
}

// Command is the one line to run on the machine.
func Command(base string, h managed.Host, code, panel string) string {
	o := h.Options
	if h.OS == "windows" {
		var b strings.Builder
		fmt.Fprintf(&b, `iwr -useb %s/install.ps1 -OutFile $env:TEMP\fairy-install.ps1; powershell -ExecutionPolicy Bypass -File $env:TEMP\fairy-install.ps1 -Join %s -PanelAddress %s`, base, code, panel)
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
		return b.String()
	}
	sudo := "sudo "
	if h.Node != "" { // a Proxmox node: a root shell, usually without sudo
		sudo = ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "curl -fsSL %s/install.sh | %ssh -s -- --join %s --panel %s", base, sudo, code, panel)
	for _, f := range []struct {
		on   bool
		flag string
	}{{o.PowerControl, "--power-control"}, {o.SelfUpdate, "--self-update"}, {o.WolRelay, "--wol"}, {o.IPMI, "--ipmi"}} {
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

func (m *Manager) copyOf(e *Enrollment) Enrollment {
	c := *e
	c.proof, c.cancel, c.held = "", nil, nil
	return c
}

// Hold keeps a new enrollment's code and command for one Reveal by its creator: the panel made it by itself
// right after a step-up sign-in, so the browser has not seen the code yet.
func (m *Manager) Hold(cr Created) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.items[cr.ID]; ok && !e.Done {
		c := cr
		e.held = &c
	}
}

// Reveal returns a held code and command once, to the user who made the enrollment, while it still waits.
func (m *Manager) Reveal(id, by string) (Created, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.items[id]
	if !ok || e.held == nil || e.CreatedBy != by || e.Done {
		return Created{}, false
	}
	c := *e.held
	e.held = nil
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

// Cancel stops a waiting enrollment.
func (m *Manager) Cancel(id string) bool {
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

func (m *Manager) set(e *Enrollment, state, msg string, done bool) Enrollment {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.Done && state != Online { // cancelled meanwhile
		return m.copyOf(e)
	}
	e.State, e.Message, e.Done = state, msg, done
	return m.copyOf(e)
}

func (m *Manager) run(ctx context.Context, e *Enrollment) {
	defer e.cancel()
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	for {
		m.mu.Lock()
		done := e.Done
		e.Attempts++
		m.mu.Unlock()
		if done {
			return
		}
		actx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, resp, code, err := m.Pair(actx, e.URL, e.proof)
		cancel()
		switch {
		case err == nil && status == 200:
			m.paired(e, resp)
			return
		case err != nil:
			m.set(e, Waiting, "waiting for the machine ("+devices.Describe(err)+")", false)
		case status == 404:
			m.set(e, Waiting, "the agent answers but has no join code yet: run the command (or it expired there)", false)
		case code == "wrong_address":
			m.set(e, Waiting, "the agent refuses: the panel address in the command ("+m.PanelAddr+") is not where the panel connects from; set enroll.panel_address", false)
		case code == "wrong_code":
			m.set(e, Waiting, "the agent has a different join code (an older command?): run this one", false)
		case status == 429:
			m.set(e, Waiting, "the agent asks to slow down", false)
		default:
			m.set(e, Waiting, fmt.Sprintf("the agent answered %d", status), false)
		}
		select {
		case <-ctx.Done():
			m.mu.Lock()
			fin := !e.Done
			if fin {
				e.State, e.Message, e.Done = Expired, "the code expired before the machine ran the command: make a new one", true
			}
			cp := m.copyOf(e)
			m.mu.Unlock()
			if fin {
				m.Hooks.Event(cp, "expired", "")
			}
			return
		case <-t.C:
		}
	}
}

func (m *Manager) paired(e *Enrollment, resp enroll.Response) {
	h := e.host
	h.AddedAt = m.Now().UTC()
	h.AgentVersion, h.Hostname, h.ControlPaired = clip(resp.AgentVersion, 64), clip(resp.Hostname, 64), resp.ControlToken != ""
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
	entry := h.Entry(sec)
	if e.Repair {
		m.Hooks.Remove(h.ID)
	} else if err := managed.Check(m.Hooks.Config(), entry); err != nil {
		fail("cannot add it: " + err.Error())
		return
	}
	if err := m.Store.Put(h, sec); err != nil {
		fail("cannot save it: " + err.Error())
		return
	}
	if err := m.Hooks.Add(entry); err != nil {
		fail("saved, but polling did not start (" + err.Error() + "): restart the panel")
		return
	}
	note := ""
	if wanted := e.Options.Control(); wanted && sec.ControlToken == "" {
		note = " (the agent sent no control token: power / update / Wake-on-LAN stay off)"
	}
	m.mu.Lock()
	e.pairedAt = m.Now()
	m.mu.Unlock()
	cp := m.set(e, Paired, "paired: tokens received, waiting for the first reading"+note, false)
	m.Hooks.Event(cp, "paired", resp.AgentVersion)
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

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// HTTPPair is POST <url>/pair {"proof"} (never through a proxy, like every agent request). It returns the
// status, the answer on 200, and the agent's error code otherwise.
func HTTPPair(ctx context.Context, url, proof string) (int, enroll.Response, string, error) {
	var resp enroll.Response
	b, _ := json.Marshal(enroll.Request{Proof: proof})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+enroll.Path, bytes.NewReader(b))
	if err != nil {
		return 0, resp, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := pairClient.Do(req)
	if err != nil {
		return 0, resp, "", err
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return r.StatusCode, resp, "", err
	}
	if r.StatusCode != 200 {
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &e)
		return r.StatusCode, resp, e.Code, nil
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return r.StatusCode, resp, "", errors.New("invalid answer from the agent")
	}
	return 200, resp, "", nil
}

var pairClient = devices.LANClient(0)

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
