package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/devices"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// "Turn on": switching the machine's own check of approvals on (release item 1.1).
//
// The panel never does this by itself. An admin sees which machines are ready (their agent knows the check
// and has no trust yet) and presses Turn on, for one machine or for several. For each chosen machine the
// panel puts together what the machine would trust from then on: the Pocket ID, the client "Fairy PC actions",
// the signing key(s) the panel fetched from that Pocket ID, and the users who may approve. The dialog shows
// exactly that, per machine, before anything is asked. Then ONE passkey approval covers all of them: each
// machine gave a challenge for the hash of its own proposal, and takes the proposal only together with an
// approval that verifies against it. So a wrong client id, an old key, a user id that is not the approver's,
// or a machine whose clock is off is found out while the machine is still open, not after it locked.
//
// After that the machine is locked: the panel has no request that changes or removes the trust. That is done
// on the machine, as administrator, with `fairy-agent helper-trust`.

func (s *Server) approvalRoutes() {
	s.mux.HandleFunc("GET /api/admin/approval", s.h(s.approvalPage))
	s.mux.HandleFunc("POST /api/admin/approval/turn-on", s.h(s.approvalTurnOn))
	s.mux.HandleFunc("POST /api/admin/approval/accept/{id}", s.h(s.approvalAccept))
}

// MaxTurnOn is how many machines one confirmation switches on (one entry each; api.MaxApprovalEntries).
const MaxTurnOn = api.MaxApprovalEntries

// approvalMachine is one agent on the approval page.
type approvalMachine struct {
	ID         string          `json:"id"` // the agent's id
	SystemID   string          `json:"system_id"`
	Name       string          `json:"name"`
	Online     bool            `json:"online"`
	State      string          `json:"state"` // off | required | broken | unknown; "": the agent does not know the check
	EnforcedBy string          `json:"enforced_by"`
	Ready      bool            `json:"ready"`         // Turn on can be tried now
	Why        string          `json:"why,omitempty"` // why not
	Users      []approval.User `json:"users"`         // who would be allowed to approve on it
	// StaleKeys: the machine requires approvals and none of the keys it has pinned is one Pocket ID
	// publishes now: every approval will be refused there until the keys are fetched again on the machine.
	StaleKeys bool `json:"stale_keys,omitempty"`
	// Binding is the panel's decision about which machine this is ("" in order; internal/approval,
	// binding.go), BindingText one sentence about it, With the names of the hosts that have the same id.
	Binding     string   `json:"binding,omitempty"`
	BindingText string   `json:"binding_text,omitempty"`
	With        []string `json:"with,omitempty"`
	// CanAccept: an admin can accept what the machine shows now (POST /api/admin/approval/accept). Accept says
	// what that is: "first" (the id of a machine that has none on record), "id" (a new id), "keep" (the id it
	// has on record, which another host shows too) or "off" (its check is gone). HeldBy: it cannot be accepted, because these hosts keep the id it shows (one id
	// is accepted for one host only): the machine needs an id of its own first.
	CanAccept bool     `json:"can_accept,omitempty"`
	Accept    string   `json:"accept,omitempty"`
	HeldBy    []string `json:"held_by,omitempty"`
	// The record (machines.json), for an admin in a browser only: the id on record and the id the machine
	// shows now, how and when the id came on record, and by whom. An API token gets none of them.
	MachineID string     `json:"machine_id,omitempty"`
	ShownID   string     `json:"shown_id,omitempty"`
	Pinned    string     `json:"pinned,omitempty"`
	PinnedAt  *time.Time `json:"pinned_at,omitempty"`
	PinnedBy  string     `json:"pinned_by,omitempty"`
	// LastKnown: it has not answered since the panel started; State is from the panel's record.
	LastKnown bool `json:"last_known,omitempty"`
	// OtherTrust: it trusts other values than this panel would set for it now (another Pocket ID, client, key
	// or list of users: set by somebody else, changed on the machine, or the panel's own list changed since).
	OtherTrust bool   `json:"other_trust,omitempty"`
	SetBy      string `json:"set_by,omitempty"`
	SetAt      string `json:"set_at,omitempty"`
}

// turnOnResult is how one machine answered a Turn on.
type turnOnResult struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Outcome: accepted | proof_refused | already_set | offline (it was not sent the trust: it did not answer
	// before) | refused | not_asked | no_answer (it was sent the trust and gave no answer: it may be written)
	Outcome string `json:"outcome"`
	Text    string `json:"text"`
	// More is the machine's own longer answer, for the info button of the row (audit 5, r1-b2-07): the row
	// itself keeps one short sentence.
	More string `json:"more,omitempty"`
}

type turnOnReport struct {
	At      time.Time      `json:"at"`
	By      string         `json:"by"`
	Results []turnOnResult `json:"results"`
}

type approvalAdminPage struct {
	Configured bool                `json:"configured"`
	Issuer     string              `json:"issuer"`
	ClientID   string              `json:"client_id"`
	Callback   string              `json:"callback"`
	Keys       []model.ApprovalKey `json:"keys"` // what Pocket ID publishes now (what a machine would pin)
	KeysError  string              `json:"keys_error,omitempty"`
	Machines   []approvalMachine   `json:"machines"`
	Ready      int                 `json:"ready"`
	Required   int                 `json:"required"`
	Broken     int                 `json:"broken"`
	Max        int                 `json:"max"`
	// AnyPerson: a test build. The panel does not compare the approver with the signed-in person, and its
	// admins are not put on the machines' lists (the test admin is no Pocket ID user).
	AnyPerson bool          `json:"any_person,omitempty"`
	Last      *turnOnReport `json:"last"` // the last Turn on since the panel started
}

// issuerKeys is what the panel last fetched from Pocket ID's key set.
type issuerKeys struct {
	mu   sync.Mutex
	keys []api.TrustKey
	err  string
	at   time.Time
	from string
}

const (
	issuerKeysFresh = 5 * time.Minute  // the page shows keys at most this old
	issuerKeysRetry = 30 * time.Second // after a failure
)

// pocketKeys returns the signing keys Pocket ID publishes, as a machine would pin them. maxAge 0 fetches now
// (a Turn on always proposes what Pocket ID publishes at that moment).
func (s *Server) pocketKeys(ctx context.Context, maxAge time.Duration) ([]api.TrustKey, error) {
	k := &s.approvalKeys
	k.mu.Lock()
	defer k.mu.Unlock()
	issuer := s.Cfg.ApprovalIssuer()
	age := s.now().Sub(k.at)
	if k.from == issuer && maxAge > 0 && ((k.err == "" && k.keys != nil && age < maxAge) || (k.err != "" && age < issuerKeysRetry)) {
		if k.err != "" {
			return nil, apierr.New(502, k.err)
		}
		return k.keys, nil
	}
	keys, err := s.fetchPocketKeys(ctx)
	k.at, k.from = s.now(), issuer
	if err != nil {
		k.keys, k.err = nil, textsafe.Clip(err.Error(), 200)
		return nil, apierr.New(502, k.err)
	}
	k.keys, k.err = keys, ""
	return keys, nil
}

func (s *Server) fetchPocketKeys(ctx context.Context) ([]api.TrustKey, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := s.approval.provider(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot reach Pocket ID (%s): %s", s.Cfg.ApprovalIssuer(), safeErr(err))
	}
	var doc struct {
		JWKS string `json:"jwks_uri"`
	}
	if err := p.Claims(&doc); err != nil || doc.JWKS == "" {
		return nil, errors.New("the discovery document of Pocket ID names no key set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, doc.JWKS, nil)
	if err != nil {
		return nil, errors.New("the key set address of Pocket ID cannot be used")
	}
	resp, err := s.approval.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the key set of Pocket ID cannot be fetched: %s", safeErr(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, approval.MaxJWKS+1))
	if err != nil || resp.StatusCode != 200 {
		return nil, fmt.Errorf("the key set of Pocket ID cannot be fetched (HTTP %d)", resp.StatusCode)
	}
	return approval.TrustKeys(raw)
}

func shownKeys(keys []api.TrustKey) []model.ApprovalKey {
	out := []model.ApprovalKey{}
	for i, tp := range approval.Thumbprints(keys) {
		out = append(out, model.ApprovalKey{Kid: keys[i].Kid, Alg: keys[i].Alg, Thumbprint: tp})
	}
	return out
}

// approvalUsersFor is who may approve on the machine of agent a when the check is switched on for it now: the
// panel's admins, the host's owner if the panel knows which Pocket ID user that is, and approval.users.
//
// "Knows" is deliberate: the owner label of config.yaml is a name for people. It stands for a Pocket ID user
// only when approval.owners says so, or when the label itself is a user's id or their verified e-mail address.
// Nobody gets onto a machine's list because their display name matches a label.
func (s *Server) approvalUsersFor(a config.Agent) []approval.User {
	users := s.Access.Load().Users
	var admins []approval.User
	if !s.approvalAnyPerson { // a test build's one admin is no Pocket ID user
		for sub, u := range users {
			if u.Role == access.RoleAdmin && config.ValidApprovalUser(sub) {
				admins = append(admins, approval.User{Sub: sub, Name: nameOf(u)})
			}
		}
	}
	var owner *approval.User
	label := model.Deref(a.Owner)
	if x := s.Poller.RelaySystem(a.ID); x != nil && model.Deref(x.Owner) != "" {
		label = model.Deref(x.Owner)
	}
	if label != "" {
		why := "owner (" + label + ")"
		if sub := s.Cfg.Approval.Owners[label]; sub != "" {
			owner = &approval.User{Sub: sub, Name: label, Why: why}
			if u, ok := users[sub]; ok {
				owner.Name = nameOf(u)
			}
		} else {
			for sub, u := range users {
				verified := u.EmailVerified != nil && *u.EmailVerified
				if config.ValidApprovalUser(sub) && u.Role != access.RolePending && (sub == label || (verified && u.Email != "" && strings.EqualFold(u.Email, label))) {
					owner = &approval.User{Sub: sub, Name: nameOf(u), Why: why}
				}
			}
		}
	}
	return approval.Users(admins, owner, s.Cfg.Approval.Users)
}

func nameOf(u access.User) string {
	if u.Name != "" {
		return u.Name
	}
	return u.Email
}

// machineFor describes one agent for the approval page; keys are Pocket ID's current ones (nil: unknown).
func (s *Server) machineFor(a config.Agent, keys []api.TrustKey) approvalMachine {
	m := approvalMachine{ID: a.ID, SystemID: a.ID, Name: a.DisplayName(), Users: []approval.User{}}
	if x := s.Poller.RelaySystem(a.ID); x != nil {
		m.SystemID, m.Name = x.ID, x.Name
	}
	info, _ := s.Poller.AgentInfo(a.ID)
	m.Online = info.Online
	ap := s.Poller.ApprovalOf(a.ID)
	b := s.Poller.BindingOf(a.ID)
	if !b.OK() {
		m.Binding, m.With = b.Status, s.machineNames(b.With)
		m.BindingText = approval.BindingText(m.Name, b.Status, m.With)
		if len(b.Held) > 0 {
			m.BindingText = approval.HeldText(m.Name, s.machineNames(b.Held))
		}
		switch {
		case b.Status == approval.BindVanished && b.Report.Heard:
			m.CanAccept, m.Accept = true, "off"
		case len(b.Held) > 0:
			// another host keeps its id: nothing to accept here until it shows an id of its own
			m.HeldBy = s.machineNames(b.Held)
		case (b.Status == approval.BindChanged || b.Status == approval.BindDuplicate) && b.Report.Block && b.Report.MachineID != "":
			m.CanAccept, m.Accept = true, "id"
			if b.Record.MachineID == b.Report.MachineID {
				m.Accept = "keep" // it shows the id it has on record (the original of a pair): nothing about it is new
			}
		case b.Status == approval.BindNoRecord && b.Report.Block && b.Report.MachineID != "":
			// not on record: Fairy did not turn this check on. The id it shows comes on record by this accept only.
			m.CanAccept, m.Accept = true, "first"
		}
	}
	if b.Known && b.Record.MachineID != "" {
		at := b.Record.At
		m.MachineID, m.Pinned, m.PinnedBy = b.Record.MachineID, b.Record.How, b.Record.By
		if !at.IsZero() {
			m.PinnedAt = &at
		}
	}
	m.ShownID = b.Report.MachineID
	switch {
	case ap == nil:
		m.Why = "its agent does not know this check (older than the release with it, or a Windows agent without the helper)"
		return m
	case m.Binding != "" && !(b.Status == approval.BindVanished && ap.State == api.ApprovalOff && !ap.LastKnown):
		// an alarm: nothing is switched on until it is resolved. The one exception is the machine whose trust
		// was cleared on the machine and that says "off" now: Turn on is how its check comes back.
		m.State, m.EnforcedBy, m.LastKnown, m.Why = ap.State, ap.EnforcedBy, ap.LastKnown, m.BindingText
		return m
	case ap.State == api.ApprovalOff && b.OK() && b.Known && b.Record.MachineID != "":
		// An id is on record and the machine's last answer still says "off": that answer is from before the id
		// came on record (a Turn on a moment ago; an answer from after it that says "off" is the alarm "check
		// gone", above). The record counts: the machine is on, never "ready", until its next answer (audit 6,
		// r4-b1-01: the Updates page listed the machines it had just called "switched on" as "off, ready").
		m.State, m.EnforcedBy, m.Why = api.ApprovalRequired, ap.EnforcedBy, "already on: it is changed only on the machine (fairy-agent helper-trust)"
		return m
	case ap.State == api.ApprovalRequired:
		m.State, m.EnforcedBy, m.LastKnown, m.Why = ap.State, ap.EnforcedBy, ap.LastKnown, "already on: it is changed only on the machine (fairy-agent helper-trust)"
		m.SetBy, m.SetAt = ap.SetBy, ap.SetAt
		if keys != nil && !ap.LastKnown {
			now := approval.Thumbprints(keys)
			m.StaleKeys = !slices.ContainsFunc(ap.Keys, func(k model.ApprovalKey) bool { return slices.Contains(now, k.Thumbprint) })
			if users := s.approvalUsersFor(a); ap.TrustHash != "" && len(users) > 0 {
				m.OtherTrust = ap.TrustHash != api.TrustHash(approval.Proposal(s.Cfg.ApprovalIssuer(), s.Cfg.Approval.ClientID, keys, users))
			}
		}
		return m
	case ap.State != api.ApprovalOff:
		m.State, m.EnforcedBy, m.LastKnown = ap.State, ap.EnforcedBy, ap.LastKnown
		m.Why = "its check cannot be used (" + orText(ap.Error, ap.State) + "): repaired on the machine"
		if helperTooOld(ap) {
			m.Why = "its running helper is older than its agent: restart the helper (Stop, then Start, next to the host name)"
		}
		return m
	}
	m.State, m.EnforcedBy = ap.State, ap.EnforcedBy
	if m.Users = s.approvalUsersFor(a); m.Users == nil {
		m.Users = []approval.User{}
	}
	switch {
	case !a.HasControl():
		m.Why = "the panel has no control token for it"
	case ap.Proto < api.ApprovalProto:
		m.Why = fmt.Sprintf("agent too old (approval scheme %d; this panel uses %d): install the current agent on it", ap.Proto, api.ApprovalProto)
	case ap.Proto != api.ApprovalProto:
		m.Why = fmt.Sprintf("it uses version %d of the approval scheme; this panel knows version %d", ap.Proto, api.ApprovalProto)
	case !m.Online:
		m.Why = "it is not answering right now"
	case len(m.Users) == 0:
		m.Why = "nobody would be allowed to approve on it: set approval.users in config.yaml"
	case len(m.Users) > api.MaxTrustUsers:
		m.Why = fmt.Sprintf("%d users would be allowed; a machine takes at most %d", len(m.Users), api.MaxTrustUsers)
	case s.Cfg.ApprovalIssuerProblem() != "":
		// no machine takes an issuer that is not https: it was "Ready", and Turn on then failed (audit 6, r4-p3-09)
		m.Why = s.Cfg.ApprovalIssuerProblem()
	default:
		m.Ready = true
	}
	return m
}

// ApprovalWarnings is what the log says at start about the approval: block (fairy serve, and the test build):
// an issuer no machine accepts, in the words of the dialog's "Not ready" and of `fairy admin config check`.
func ApprovalWarnings(cfg *config.Panel) []string {
	if why := cfg.ApprovalIssuerProblem(); why != "" {
		return []string{"approval: " + why + "; it is " + cfg.ApprovalIssuer() + ". The machines only accept an https issuer: no machine is " +
			"ready to be switched on for the check until approval.issuer in config.yaml is an https:// address"}
	}
	return nil
}

func orText(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// approvalPage is the approval page's data (admins): the client, the keys Pocket ID publishes now, every
// agent with its state, and how the last Turn on went.
func (s *Server) approvalPage(c *reqCtx) error {
	u, err := c.requireAdmin("the approval settings")
	if err != nil {
		return err
	}
	page := approvalAdminPage{Configured: s.Cfg.ApprovalOn(), Keys: []model.ApprovalKey{},
		Machines: []approvalMachine{}, Max: MaxTurnOn, AnyPerson: s.approvalAnyPerson}
	var keys []api.TrustKey
	if page.Configured {
		page.Issuer, page.ClientID, page.Callback = s.Cfg.ApprovalIssuer(), s.Cfg.Approval.ClientID, s.Cfg.ApprovalCallback()
		var err error
		if keys, err = s.pocketKeys(c.r.Context(), issuerKeysFresh); err != nil {
			page.KeysError = fmt.Sprint(err.(*apierr.Error).Detail)
		} else {
			page.Keys = shownKeys(keys)
		}
	}
	for _, a := range s.Poller.Agents() {
		m := s.machineFor(a, keys)
		if u.Auth == "token" { // the ids are for an admin in a browser: a token learns none
			m.MachineID, m.ShownID = "", ""
		}
		if !page.Configured && m.Ready {
			m.Ready, m.Why = false, "the panel has no approval: block in config.yaml (a separate Pocket ID client for PC actions)"
		}
		if m.Ready && keys == nil {
			m.Ready, m.Why = false, "Pocket ID's keys cannot be fetched right now"
		}
		switch {
		case m.Ready:
			page.Ready++
		case m.State == api.ApprovalRequired:
			page.Required++
		case m.State == api.ApprovalBroken:
			page.Broken++
		}
		page.Machines = append(page.Machines, m)
	}
	sort.SliceStable(page.Machines, func(i, j int) bool { return page.Machines[i].Name < page.Machines[j].Name })
	s.turnOnMu.Lock()
	page.Last = s.turnOnLast
	s.turnOnMu.Unlock()
	c.json(200, page)
	return nil
}

type turnOnReq struct {
	Hosts []string `json:"hosts"`
}

func (r *turnOnReq) validate() error {
	if len(r.Hosts) == 0 || len(r.Hosts) > MaxTurnOn {
		return apierr.Errf(422, "hosts: 1 to %d machines", MaxTurnOn)
	}
	seen := map[string]bool{}
	for _, id := range r.Hosts {
		if id == "" || len(id) > 64 || seen[id] {
			return apierr.New(422, "hosts: each machine's id once")
		}
		seen[id] = true
	}
	return nil
}

// approvalTurnOn is POST /api/admin/approval/turn-on {hosts: [agent ids]}: see the top of this file. Admins in
// a browser only (an API token cannot give a passkey), CSRF, the kill switch. The first pass asks the machines
// and then the admin (401 with the dialog's lines); the replay after the approval sends each machine its
// proposal with the approval and answers with one result per machine.
func (s *Server) approvalTurnOn(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	var b turnOnReq
	if err := c.decodeBody(&b); err != nil {
		return err
	}
	if err := b.validate(); err != nil {
		return err
	}
	u, err := c.controlAllowed(true)
	if err != nil {
		return err
	}
	if !s.Cfg.ApprovalOn() {
		return apierr.New(409, "this panel has no Pocket ID client for approvals (approval: in config.yaml, with the Client ID of a separate "+
			"client for PC actions): the check cannot be switched on from here")
	}
	if why := s.Cfg.ApprovalIssuerProblem(); why != "" {
		// what the page says for every machine ("Not ready: the issuer must be https"), for a caller that did not look
		return apierr.New(409, "The check cannot be switched on from this panel: "+why+" (it is "+textsafe.Clip(s.Cfg.ApprovalIssuer(), 200)+
			"; approval.issuer in config.yaml). Nothing was sent.")
	}
	if g := c.approved(); g != nil {
		return s.turnOnRun(c, u, g, b.Hosts)
	}
	if c.resumed() {
		return apierr.New(409, "the check is switched on only with its own approval. Nothing was done: press Turn on again.")
	}
	if !s.approvalRL.allow(u.Sub) {
		return c.limited(u, s.approvalRL, "too many control requests")
	}
	keys, err := s.pocketKeys(c.r.Context(), 0)
	if err != nil {
		return err
	}
	var (
		bound   []approval.Bound
		refused []turnOnResult
		trust   = map[string]api.TrustInit{}
		items   = []string{fmt.Sprintf("Pocket ID: %s · Client ID %s", s.Cfg.ApprovalIssuer(), s.Cfg.Approval.ClientID)}
		asked   []turnOnAsked // the machines of the plan, with who may approve on each: the dialog's lines
	)
	for i, k := range shownKeys(keys) {
		items = append(items, fmt.Sprintf("Key %d the machines will pin: %s · thumbprint %s", i+1, k.Kid, k.Thumbprint))
	}
	for _, id := range b.Hosts {
		a, ok := s.Poller.AgentByID(id)
		if !ok {
			return apierr.New(400, "unknown machine: "+textsafe.Clip(id, 64))
		}
		m := s.machineFor(a, keys)
		if !m.Ready {
			refused = append(refused, turnOnResult{ID: id, Name: m.Name, Outcome: notReadyOutcome(m), Text: m.Why})
			continue
		}
		in := approval.Proposal(s.Cfg.ApprovalIssuer(), s.Cfg.Approval.ClientID, keys, m.Users)
		// The machine's id is made up HERE, before the machine is asked anything, and goes into the proposal
		// and into the entry the passkey signs. It comes on record when the machine has taken the proposal
		// (sendTrust). No answer of the machine has a say in it.
		mid, err := approval.NewMachineID(nil)
		if err != nil {
			return apierr.New(500, "no machine id could be made up: "+textsafe.Clip(err.Error(), 100))
		}
		in.MachineID = mid
		if err := approval.CheckProposal(in); err != nil {
			return apierr.New(409, "what "+m.Name+" would be told to trust is not something a machine accepts: "+textsafe.Clip(err.Error(), 200)+
				". Nothing was sent.")
		}
		ch, _, err := c.challenge(approvalTarget{agent: a, name: m.Name, req: api.TrustSetRequest(in), machineID: mid})
		if err != nil || ch == nil {
			refused = append(refused, turnOnResult{ID: id, Name: m.Name, Outcome: challengeOutcome(err), Text: errText(err, "the machine gave no challenge")})
			continue
		}
		bound = append(bound, *ch)
		trust[id] = in
		asked = append(asked, turnOnAsked{name: m.Name, by: orText(m.EnforcedBy, "agent"), users: m.Users})
	}
	if len(bound) == 0 {
		for _, r := range refused {
			c.audit(u, "approval.turn_on", r.ID, map[string]any{"result": r.Outcome}, "error: "+r.Outcome+": "+r.Text)
		}
		s.turnOnDone(u, refused)
		return apierr.New(409, "The check could not be switched on for any of the chosen machines. "+resultLine(refused))
	}
	var notOn []string
	for _, r := range refused {
		notOn = append(notOn, fmt.Sprintf("%s: NOT switched on (%s)", r.Name, r.Text))
	}
	const locked = "After this each of these machines is locked: what it trusts is changed only on the machine itself (fairy-agent helper-trust)."
	items = append(items, turnOnLines(asked, maxPlanItems-len(items)-len(notOn)-1)...)
	items = append(append(items, notOn...), locked)
	plan, err := approval.NewPlan(bound)
	if err != nil {
		return apierr.New(apierr.Upstream, "the machines' challenges cannot be used: "+textsafe.Clip(err.Error(), 200))
	}
	ap := newApprovalPlan(plan)
	ap.trust, ap.items, ap.skipped = trust, items, map[string]turnOnResult{}
	for _, r := range refused {
		ap.skipped[r.ID] = r
	}
	ap.message = "Confirm it's you: one passkey switches the check on for " + andNames(ap.machines()) + ". Each machine verifies it before it locks."
	return c.askApproval(u, ap)
}

// turnOnRun is the replay after the approval: every machine of the plan is sent what was shown in the dialog
// (the proposal kept with the plan, never one put together again) with the approval, and answers for itself.
func (s *Server) turnOnRun(c *reqCtx, u *model.User, g *approvalGrant, hosts []string) error {
	if !s.control.allow(u.Sub) {
		return c.limited(u, s.control, "too many control requests")
	}
	var results []turnOnResult
	for _, id := range hosts {
		a, ok := s.Poller.AgentByID(id)
		in, planned := g.plan.trust[id]
		name := id
		if ok {
			name = s.machineFor(a, nil).Name
		}
		if !ok || !planned {
			res := turnOnResult{ID: id, Name: name, Outcome: "not_asked", Text: "it was not part of what was approved; nothing was sent to it"}
			if r, why := g.plan.skipped[id]; why { // it could not be asked before the approval: that reason stands
				res = r
			}
			c.audit(u, "approval.turn_on", id, map[string]any{"result": res.Outcome}, "error: "+res.Outcome+": "+res.Text)
			results = append(results, res)
			continue
		}
		res := s.sendTrust(c, a, name, in, g)
		if res.Outcome == "accepted" {
			// on record from this moment: the id the panel made up and the passkey signed, not what was answered
			pinned := "on record"
			if err := s.Poller.PinMachine(a.ID, in.MachineID, approval.PinTurnOn, actorOf(u)); err != nil {
				res.Text += " Fairy could not write its machine id down (" + textsafe.Clip(err.Error(), 120) + "): it keeps it in memory and tries again."
				pinned = "on record (kept in memory; machines.json could not be written: " + textsafe.Clip(err.Error(), 120) + ")"
			}
			// which id came on record for which host, how, and by whom (an accept writes approval.accept)
			c.audit(u, "approval.machine_id", id, map[string]any{"how": approval.PinTurnOn, "machine_id": approval.ShortID(in.MachineID)}, pinned)
		}
		params := map[string]any{"issuer": in.Issuer, "client_id": in.ClientID, "keys": approval.Thumbprints(in.Keys), "users": in.Users,
			"machine_id": approval.ShortID(in.MachineID), "result": res.Outcome}
		outcome := "ok"
		if res.Outcome != "accepted" {
			outcome = "error: " + res.Outcome + ": " + res.Text
		}
		c.audit(u, "approval.turn_on", id, params, outcome)
		results = append(results, res)
	}
	// Every machine is asked again at once, not only the ones that were switched on: if one of them handed the
	// proposal on to another machine of this panel, that machine shows the id now, and the two are flagged as
	// one id on two hosts before anybody approves anything else.
	for _, a := range s.Poller.Agents() {
		s.Poller.Scheduler.Kick("agent:" + a.ID)
	}
	s.turnOnDone(u, results)
	ok, unknown := 0, 0
	for _, r := range results {
		switch r.Outcome {
		case "accepted":
			ok++
		case "no_answer":
			unknown++
		}
	}
	msg := fmt.Sprintf("%d of %d machines now check approvals themselves. %s", ok, len(results), resultLine(results))
	if unknown > 0 {
		// a machine that gave no answer may have taken the trust: the count is of the ones that said so, and
		// the sentence does not claim a number nobody knows
		msg = fmt.Sprintf("%d of %d machines confirmed that they check approvals themselves now; not known for %d (no answer). %s", ok, len(results), unknown, resultLine(results))
	}
	status := 200
	if ok == 0 {
		status = 409
	}
	c.json(status, map[string]any{"results": results, "message": textsafe.Clip(msg, 290), "detail": map[string]any{"message": textsafe.Clip(msg, 290)}})
	return nil
}

// trustNoAnswer is what a Turn on says for a machine that was sent its trust and gave no answer: not "refused"
// and not "nothing was written", because nobody knows.
const trustNoAnswer = "no answer: it may have been written; look at the machine's Approval."

// agentsOwn reports whether an answer with an error status is the agent's own "no": the agent answers every
// refusal with a JSON object that has its reason in "error" or a "code". 502 and 504 are never that: they are
// what a gateway answers after it passed the request on, and what the agent itself answers when its helper was
// sent the request and did not answer in time (Fairy-agent, helperUnanswered: "it may be written all the same").
func agentsOwn(status int, code, text string) bool {
	return status >= 400 && status != 502 && status != 504 && (code != "" || text != "")
}

// trustTaken is the panel's own look at a machine that was sent its trust and gave no answer, before anything
// final is said about it: it asks the machine for its state now. true only when the machine shows the check on
// with the machine id the panel made up for it and with the hash of exactly this proposal: then it took the
// trust, and the id is on record like after an answer. Everything else (no answer, no check yet, another
// check) is "not known": the helper may still write it, and the machine's Approval shows what becomes of it.
func (s *Server) trustTaken(c *reqCtx, a config.Agent, in api.TrustInit) bool {
	ap, ok := s.Poller.ApprovalNow(c.r.Context(), a.ID)
	return ok && ap != nil && ap.State == api.ApprovalRequired && ap.Proto == api.ApprovalProto &&
		ap.MachineID == in.MachineID && ap.TrustHash != "" && ap.TrustHash == api.TrustHash(in)
}

// sendTrust sends one machine its proposal with the approval (POST /approval/trust) and reads its answer.
func (s *Server) sendTrust(c *reqCtx, a config.Agent, name string, in api.TrustInit, g *approvalGrant) turnOnResult {
	res := turnOnResult{ID: a.ID, Name: name}
	ap, err := g.For(a.ID, in.MachineID, api.TrustSetRequest(in)) // the id of the proposal that was shown and approved
	if err != nil {
		res.Outcome, res.Text = "not_asked", "the approval is not for what this machine would be told to trust; nothing was sent to it"
		return res
	}
	in.Approval = ap
	status, data, err := s.Poller.AgentRequest(c.r.Context(), a, "POST", api.PathApprovalTrust, in)
	code, text := jsonx.S(data, "code"), textsafe.Clip(jsonx.S(data, "error"), maxAgentError)
	switch {
	case err != nil || status < 200 || (status >= 300 && !agentsOwn(status, code, text)):
		// The request left the panel and no answer of the agent came back: a timeout, a broken connection, a
		// gateway's page, or the agent's own 504 (its helper has the trust and did not answer in time). It used
		// to read "refused ... Nothing was written." for the last two, which nobody can know. The machine is
		// asked for its state before anything final is said.
		if s.trustTaken(c, a, in) {
			res.Outcome, res.Text = "accepted", "it gave no answer to the request, and its state shows that it took the trust: it checks approvals itself now. It is locked."
			break
		}
		res.Outcome, res.Text = "no_answer", trustNoAnswer
		if err != nil {
			res.More = "It did not answer (" + devices.Describe(err) + ")."
		} else if text != "" {
			res.More = text
		} else {
			res.More = fmt.Sprintf("It answered HTTP %d, which is not an answer of the agent.", status)
		}
	case status == 200 && jsonx.S(data, "state") == api.ApprovalRequired:
		res.Outcome, res.Text = "accepted", "it verified the approval against what it was proposed and now checks approvals itself. It is locked."
		if got := jsonx.S(data, "machine_id"); got != in.MachineID {
			res.Text += " It answered with another machine id than the one Fairy gave it: look at its page."
		}
	case status == 200:
		res.Outcome, res.Text = "refused", "it answered without saying that the check is on now (state: "+textsafe.Clip(jsonx.S(data, "state"), 20)+")"
	case code == api.CodeTrustAlreadySet:
		res.Outcome, res.Text = "already_set", "it already has a trust file: changed only on the machine (fairy-agent helper-trust show)"
	case code == api.CodeTrustProofRefused || code == api.CodeTrustProofRequired:
		res.Outcome, res.Text, res.More = "proof_refused", "it did not accept the proof"+proofReason(text)+". Nothing was written: Turn on can be pressed again.", text
	default:
		res.Outcome = "refused"
		res.Text = strings.TrimSuffix(orText(text, fmt.Sprintf("it answered HTTP %d", status)), ".")
		if !strings.HasSuffix(res.Text, "Nothing was written") { // the machine's own words end with it already
			res.Text += ". Nothing was written"
		}
		res.Text += "."
		if msg, ok := approval.ErrorText(name, code, text, s.unavailableWhy(a.ID)); ok {
			res.Text = msg
		}
	}
	return res
}

// proofReason is the reason word of a machine's "proof refused (<reason>): ..." in brackets, "" when its answer
// has none.
func proofReason(text string) string {
	if rest, ok := strings.CutPrefix(text, "proof refused ("); ok {
		if reason, _, ok := strings.Cut(rest, ")"); ok && reason != "" && len(reason) <= 40 {
			return " (" + reason + ")"
		}
	}
	return ""
}

// turnOnDone keeps the report of the last Turn on for the page and logs it.
func (s *Server) turnOnDone(u *model.User, results []turnOnResult) {
	rep := &turnOnReport{At: s.now().UTC(), By: actorOf(u), Results: results}
	s.turnOnMu.Lock()
	s.turnOnLast = rep
	s.turnOnMu.Unlock()
	for _, r := range results {
		slog.Info("approval: Turn on", "machine", r.ID, "result", r.Outcome)
	}
}

func notReadyOutcome(m approvalMachine) string {
	switch {
	case m.State == api.ApprovalRequired:
		return "already_set"
	case !m.Online && m.State == api.ApprovalOff:
		return "offline"
	}
	return "refused"
}

// challengeOutcome is the result word for a machine that could not be asked for its challenge.
func challengeOutcome(err error) string {
	var ae *apierr.Error
	if e, ok := err.(*apierr.Error); ok {
		ae = e
	}
	switch {
	case ae != nil && ae.Status == 502:
		return "offline"
	case ae != nil && strings.Contains(fmt.Sprint(ae.Detail), "already has"):
		return "already_set"
	}
	return "refused"
}

func errText(err error, def string) string {
	if e, ok := err.(*apierr.Error); ok {
		return fmt.Sprint(e.Detail)
	}
	return def
}

// turnOnAsked is one machine of a Turn on as "Confirm it's you" shows it: its name, what checks there, and who
// may approve on it from then on (the users of the proposal the passkey signs for it).
type turnOnAsked struct {
	name, by string
	users    []approval.User
}

// userLine is one person as the dialog shows them: the name, the Pocket ID user id IN FULL, and why they are on
// the list. One line each, so that no line is cut in the middle of a list.
func userLine(u approval.User) string {
	name := u.Name
	if name == "" {
		name = "(not a user of this panel)"
	}
	return fmt.Sprintf("· %s [%s, %s]", name, u.Sub, u.Why)
}

// turnOnLines are the lines of "Confirm it's you" that say who may approve on which machine (audit 6,
// r4-b1-03). It was one line per machine with all its people in it, and a line of the dialog is cut at 240
// bytes: with five ids in approval.users the confirmation named two people and cut the third in its middle,
// for the list that is locked into the machine afterwards.
//
// Now each person has a line of their own, with the user id in full, under the machine it is for and a line that
// says how many they are. Machines that get the very same list stand together above it (several to a line), so
// the list is not repeated for every machine. budget is how many lines the dialog has room for: when that is not enough the
// last line says how many are left out and what to do, never a silent cut.
func turnOnLines(asked []turnOnAsked, budget int) []string {
	type group struct {
		machines []string
		users    []approval.User
	}
	var groups []*group
	byList := map[string]*group{}
	for _, a := range asked {
		key := ""
		for _, u := range a.users {
			key += u.Sub + "\x00" + u.Name + "\x00" + u.Why + "\x01"
		}
		g := byList[key]
		if g == nil {
			g = &group{users: a.users}
			byList[key] = g
			groups = append(groups, g)
		}
		g.machines = append(g.machines, fmt.Sprintf("%s (checked by its %s)", a.name, a.by))
	}
	var lines []string
	for _, g := range groups {
		// the machines of one list, several to a line while a line stays well under what the dialog cuts
		line := ""
		for _, m := range g.machines {
			if line != "" && len(line)+2+len(m) > 200 {
				lines, line = append(lines, line), ""
			}
			if line != "" {
				line += "; "
			}
			line += m
		}
		lines = append(lines, line)
		who := "1 person may approve"
		if len(g.users) != 1 {
			who = fmt.Sprintf("%d people may approve", len(g.users))
		}
		if len(g.machines) == 1 {
			lines = append(lines, who+" on it:")
		} else {
			lines = append(lines, fmt.Sprintf("%s on each of these %d machines:", who, len(g.machines)))
		}
		for _, u := range g.users {
			lines = append(lines, userLine(u))
		}
	}
	if budget < 2 {
		budget = 2
	}
	if len(lines) > budget {
		left := len(lines) - (budget - 1)
		lines = append(lines[:budget-1], fmt.Sprintf("%d more lines do not fit here. Cancel, and turn on fewer machines at a time to read every list.", left))
	}
	return lines
}

func resultLine(results []turnOnResult) string {
	var out []string
	for _, r := range results {
		out = append(out, r.Name+": "+strings.ReplaceAll(r.Outcome, "_", " "))
	}
	return strings.Join(out, "; ")
}
