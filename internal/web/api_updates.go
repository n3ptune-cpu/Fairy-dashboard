package web

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/powerctl"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/updates"
)

// The Updates page (admins only): each agent's version and self-update state next to the latest Fairy-agent
// release, and Update buttons. Reading needs the admin role; Update is a control action (CSRF, admin, kill
// switch, step-up, rate limit, audit) and only reaches agents with `self_update: true` in config.yaml, whose
// own config.yaml enables it too. The agent then downloads, verifies and installs the release by itself.

type updateRow struct {
	ID           string         `json:"id"`
	SystemID     string         `json:"system_id"` // the host it reports as (itself, or the Proxmox node it runs on)
	Name         string         `json:"name"`
	Status       string         `json:"status"`
	Version      *string        `json:"version"`
	Build        *int           `json:"build"`
	Configured   bool           `json:"configured"`    // self_update: true in the panel's config.yaml
	AgentEnabled bool           `json:"agent_enabled"` // the agent reports self_update enabled
	SelfUpdate   map[string]any `json:"self_update"`   // the agent's state block (state, message, target, last)
	Behind       *bool          `json:"behind"`        // older than the latest release (nil: unknown)
	CanUpdate    bool           `json:"can_update"`
	Why          string         `json:"why,omitempty"`
	// VersionLastSeen: Version and Build are what the agent reported when the panel last heard from it, not a
	// reading of now: the agent does not answer (it is off, or unreachable). After a restart of the panel that
	// version is the one kept in state.json (poller.LastAgentVersion).
	VersionLastSeen bool `json:"version_last_seen,omitempty"`
	// Approval is the state of the machine's own check of approvals (off | required | broken | unknown; ""
	// for an agent without it): "required" means the Update button asks for a passkey, "broken" that the
	// machine refuses the update until its trust file is repaired on the machine.
	Approval string `json:"approval,omitempty"`
}

type updatesPage struct {
	Latest       updates.Latest `json:"latest"`
	PanelVersion string         `json:"panel_version"`
	PanelLatest  updates.Latest `json:"panel_latest"` // the latest Fairy-dashboard release
	PanelBehind  *bool          `json:"panel_behind"` // nil: unknown (local build, or no answer)
	Behind       int            `json:"behind"`       // agents behind the latest release, + 1 if the panel is
	Agents       []updateRow    `json:"agents"`
	// Check for updates: when an admin last pressed it (nil: not since the panel started), seconds until it may
	// be pressed again (0: now), and whether that check is still running.
	LastCheck    *time.Time `json:"last_check"`
	CheckRetryS  int        `json:"check_retry_s"`
	CheckRunning bool       `json:"check_running"`
}

func (s *Server) updateRoutes() {
	s.mux.HandleFunc("GET /api/admin/updates", s.h(s.updatesList))
	s.mux.HandleFunc("POST /api/admin/updates", s.h(s.updateAll))
	s.mux.HandleFunc("POST /api/admin/updates/{id}", s.h(s.updateOne))
	s.mux.HandleFunc("POST /api/admin/update-check", s.h(s.updateCheck))
	s.mux.HandleFunc("GET /api/admin/panel-update", s.h(s.panelUpdateGet))
	s.mux.HandleFunc("POST /api/admin/panel-update", s.h(s.panelUpdatePost))
}

func (s *Server) latest() updates.Latest {
	if s.Updates == nil {
		return updates.Latest{Disabled: true, Error: "release check is off (updates.check_latest: false)"}
	}
	return s.Updates.Latest()
}

func (s *Server) panelLatest() updates.Latest {
	if s.PanelUpdates == nil {
		return updates.Latest{Disabled: true, Error: "release check is off (updates.check_latest: false)"}
	}
	return s.PanelUpdates.Latest()
}

func (c *reqCtx) requireAdmin(what string) (*model.User, error) {
	u, g, err := c.grant()
	if err != nil {
		return nil, err
	}
	if !g.Admin() {
		return nil, c.deny(u, 403, what+" is for admins only", "updates denied")
	}
	return u, nil
}

func (s *Server) updateRow(c *reqCtx, a config.Agent, latest updates.Latest) updateRow {
	r := updateRow{ID: a.ID, SystemID: a.ID, Name: a.DisplayName(), Status: "offline", Configured: a.SelfUpdate}
	if x := s.Poller.RelaySystem(a.ID); x != nil {
		r.SystemID = x.ID
	}
	info, seen := s.Poller.AgentInfo(a.ID)
	if info.Online {
		r.Status = "online"
	}
	if ap := s.Poller.ApprovalOf(a.ID); ap != nil {
		r.Approval = ap.State
	}
	v := info.Version
	if !seen { // no answer since the panel started: what it said before that, if anything
		v = s.Poller.LastAgentVersion(a.ID)
	}
	if v != "" {
		r.Version, r.VersionLastSeen = &v, !info.Online
		if n, ok := release.BuildNumber(v); ok {
			r.Build = &n
		}
	}
	if seen {
		r.AgentEnabled, r.SelfUpdate = info.Enabled, info.SelfUpdate
	}
	if r.Build != nil && latest.Build != nil {
		b := *r.Build < *latest.Build
		r.Behind = &b
	}
	switch {
	case !a.SelfUpdate:
		r.Why = "not enabled in the panel (self_update: true in this agent's entry)"
	case !info.Online:
		r.Why = "agent offline"
	case !r.AgentEnabled:
		r.Why = "not enabled on the agent (self_update: enabled: true in its config.yaml), or the agent is older than self-update"
	default:
		r.CanUpdate = true
		if st, _ := r.SelfUpdate["state"].(string); st == "downloading" || st == "applying" {
			r.CanUpdate, r.Why = false, "update in progress"
		} else if st == "unavailable" {
			r.CanUpdate = false
			r.Why, _ = r.SelfUpdate["message"].(string)
		} else if r.Behind != nil && !*r.Behind {
			r.CanUpdate, r.Why = false, "already up to date"
		}
	}
	return r
}

func (s *Server) updatesList(c *reqCtx) error {
	if _, err := c.requireAdmin("the Updates page"); err != nil {
		return err
	}
	c.json(200, s.updatesPage(c))
	return nil
}

// updatesPage is the Updates page's data (the caller has checked that c is an admin).
func (s *Server) updatesPage(c *reqCtx) updatesPage {
	latest := s.latest()
	page := updatesPage{Latest: latest, PanelVersion: s.Version, PanelLatest: s.panelLatest(), Agents: []updateRow{}}
	if n, ok := release.BuildNumber(s.Version); ok && page.PanelLatest.Build != nil {
		b := n < *page.PanelLatest.Build
		page.PanelBehind = &b
		if b {
			page.Behind++
		}
	}
	for _, a := range s.Poller.Agents() {
		r := s.updateRow(c, a, latest)
		if r.Behind != nil && *r.Behind {
			page.Behind++
		}
		page.Agents = append(page.Agents, r)
	}
	sort.SliceStable(page.Agents, func(i, j int) bool { return page.Agents[i].Name < page.Agents[j].Name })
	page.LastCheck, page.CheckRetryS, page.CheckRunning = s.checks.status(time.Now())
	return page
}

// updateParams are the audit entry's details for one agent's update.
func updateParams(row updateRow, latest updates.Latest, all bool) map[string]any {
	params := map[string]any{"from": model.Deref(row.Version), "latest": latest.Version}
	if latest.Version == "" {
		params["latest"] = latest.Tag
	}
	if all {
		params["all"] = true
	}
	return params
}

// callUpdate asks one agent to update itself (POST /update with the control token). Safe to run concurrently:
// it only reads the request context.
func (c *reqCtx) callUpdate(u *model.User, a config.Agent, ap *api.Approval) (map[string]any, error) {
	// who asked, in the one form the panel names a person by (actorOf): the audit log, a run's "started by",
	// "Last Turn on · by" and the countdown's "requested by" used to show two (audit 5, r1-b2-10)
	body := map[string]any{"requested_by": actorOf(u)}
	if ap != nil { // a machine that checks approvals itself: the approval made for its update
		body["approval"] = ap
	}
	return c.agentCall(a, "POST", "/update", body)
}

// updateTarget is the update of a's agent as a machine that checks approvals itself is asked to approve it.
func (s *Server) updateTarget(a config.Agent) approvalTarget {
	name := a.DisplayName()
	if x := s.Poller.RelaySystem(a.ID); x != nil {
		name = x.Name
	}
	return approvalTarget{agent: a, name: name, req: api.ApprovalRequest{Verb: api.ApproveApplyUpdate}}
}

func (s *Server) updateOne(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	u, err := c.controlAllowed(true)
	if err != nil {
		return err
	}
	id := c.r.PathValue("id")
	// A machine that checks approvals itself is asked first (approval_control.go); any other is confirmed and
	// sent to as before.
	var target *approvalTarget
	if a, ok := s.Poller.AgentByID(id); ok && a.SelfUpdate && c.wantsTarget(a.ID) {
		t := s.updateTarget(a)
		target = &t
	}
	ap, err := c.confirmOne(u, "control actions", target)
	if err != nil {
		if target != nil && !isStepUp(err) {
			c.audit(u, "update.agent", id, nil, errorOutcome(err))
		}
		return err
	}
	if !s.control.allow(u.Sub) {
		return c.limited(u, s.control, "too many control requests")
	}
	a, ok := s.Poller.AgentByID(id)
	if !ok || !a.SelfUpdate {
		return apierr.New(404, "self-update is not configured for this agent (self_update: true in its agents: entry)")
	}
	latest := s.latest()
	params := updateParams(s.updateRow(c, a, latest), latest, false)
	if ap != nil {
		params["approval"] = "sent" // the machine checks it; the token itself is never written anywhere
	}
	data, err := c.callUpdate(u, a, ap)
	if err != nil {
		if ap == nil && refusalCode(err) == api.CodeApprovalRequired {
			// the machine wants an approval the panel did not know about (its state changed since the last poll)
			if ask := c.askAfterRefusal(u, s.updateTarget(a)); ask != nil {
				return ask
			}
		}
		c.audit(u, "update.agent", id, params, errorOutcome(err))
		return err
	}
	c.audit(u, "update.agent", id, params, "ok")
	s.updateEvent(a, params)
	// The agent took the request; how the update goes is not known yet (it downloads, verifies, restarts, and
	// can fail at each step). The message says that, also after "Confirm it's you", where an answer without
	// words was shown as "done" next to a row that said "failed" (audit 5, r2-b2-04).
	c.json(200, map[string]any{"ok": true, "result": data, "message": updateStartedText})
	return nil
}

// updateStartedText is what a single "Update" answers when the agent took the request.
const updateStartedText = "started. Its row shows how it goes."

type updateResult struct {
	ID     string         `json:"id"`
	OK     bool           `json:"ok"`
	Error  string         `json:"error,omitempty"`
	Result map[string]any `json:"result,omitempty"`
	// Name is the machine's name; State what became of its update: started, refused (the machine said no),
	// not_sent (the panel sent it nothing: its check cannot be used, its binding is an alarm, it was not part
	// of the approval), offline (the request did not leave the panel: no connection to it could be had; or it
	// stopped answering after the dialog listed it), no_answer (the request left and no answer of the agent
	// came back: the update may have started), skipped (it was not part of this press: up to date, busy, not
	// answering when the button was pressed). Why says it in a few words.
	Name  string `json:"name"`
	State string `json:"state"`
	Why   string `json:"why,omitempty"`
}

// Outcomes of one machine in "Update all".
const (
	updStarted = "started"
	updRefused = "refused"
	updNotSent = "not_sent"
	updOffline = "offline"
	updSkipped = "skipped"
	// updNoAnswer: the request got no answer of the agent (powerctl.Unanswered: a timeout, a connection that
	// broke, 502 and 504, an answer that is not the agent's). It may have arrived, so the machine is counted
	// neither with the ones that started nor with the ones that did not: a row "offline" under "1 not" said
	// that its update had not started, which nobody knows (R2; the single Update says "no answer (http 504):
	// it may have arrived" for the same answer).
	updNoAnswer = "no_answer"
)

// shortWhy is an error in a few words for a line of a list: its first sentence, without the machine's own words
// in brackets ("(<name> says: ...)"; of an "approval refused (<reason>)" the reason stays). The whole text is in
// the audit entry.
func shortWhy(err error) string {
	text := "it did not work"
	var ae *apierr.Error
	if errors.As(err, &ae) {
		text = fmt.Sprint(ae.Detail)
	}
	if i := strings.Index(text, " says: "); i >= 0 {
		if open := strings.LastIndex(text[:i], " ("); open >= 0 {
			depth, end := 0, len(text)
			for j := open + 1; j < len(text); j++ { // the brackets can have brackets of their own
				if text[j] == '(' {
					depth++
				} else if text[j] == ')' {
					if depth--; depth == 0 {
						end = j + 1
						break
					}
				}
			}
			reason := ""
			if rest, ok := strings.CutPrefix(text[i+len(" says: "):end], "approval refused ("); ok {
				if r, _, ok := strings.Cut(rest, ")"); ok && r != "" && len(r) <= 40 {
					reason = " (" + r + ")"
				}
			}
			text = text[:open] + reason + text[end:]
		}
	}
	if i := strings.Index(text, ". "); i > 20 {
		text = text[:i]
	}
	return textsafe.Clip(strings.TrimSuffix(text, "."), 160)
}

// updateSummary is what "Update all" says when it is over (audit 5, r1-b2-02: it used to say "done" also when
// machines had refused or were sent nothing): one sentence with the counts, one line per machine, and whether
// every update that was to start did start.
func updateSummary(results []updateResult) (message string, lines []string, allOK bool) {
	n := map[string]int{}
	for _, r := range results {
		n[r.State]++
		line := r.Name + ": " + strings.ReplaceAll(r.State, "_", " ")
		if r.Why != "" {
			line += " (" + r.Why + ")"
		}
		lines = append(lines, line)
	}
	not, lost := n[updRefused]+n[updNotSent]+n[updOffline], n[updNoAnswer]
	allOK = not == 0 && lost == 0 && n[updStarted] > 0
	switch {
	case not == 0 && lost == 0 && n[updStarted] == 0:
		message = fmt.Sprintf("No update was started: %d skipped.", n[updSkipped])
	case not == 0 && lost == 0:
		message = fmt.Sprintf("Update started on %d %s.", n[updStarted], plural(n[updStarted], "agent"))
	case lost == 0:
		message = fmt.Sprintf("%d started, %d not.", n[updStarted], not)
	case not == 0:
		// a machine that gave no answer may have started: the sentence does not claim a number nobody knows
		message = fmt.Sprintf("%d started, not known for %d (no answer).", n[updStarted], lost)
	default:
		message = fmt.Sprintf("%d started, %d not, not known for %d (no answer).", n[updStarted], not, lost)
	}
	if n[updSkipped] > 0 && n[updStarted]+not+lost > 0 {
		message = strings.TrimSuffix(message, ".") + fmt.Sprintf(", %d skipped.", n[updSkipped])
	}
	return message, lines, allOK
}

func (s *Server) updateAll(c *reqCtx) error {
	if _, err := c.requireUser(); err != nil {
		return err
	}
	u, err := c.controlAllowed(true)
	if err != nil {
		return err
	}
	latest := s.latest()
	type job struct {
		a      config.Agent
		params map[string]any
		res    updateResult
		err    error
		target int           // its place in targets (-1: its machine is not asked about approvals)
		ap     *api.Approval // the approval made for its update (nil: none)
	}
	var jobs []*job
	out := []updateResult{}
	// The machines that check approvals themselves are asked first, all of them, and ONE approval covers their
	// updates (approval_control.go). The others are confirmed and sent to as before; in a mixed batch the one
	// approval is also the confirmation for them.
	var targets []approvalTarget
	var others []approvalTarget // what goes to machines without the check: listed in the dialog, kept with the plan
	for _, a := range s.Poller.Agents() {
		if !a.SelfUpdate {
			continue
		}
		row := s.updateRow(c, a, latest)
		if !row.CanUpdate {
			res := updateResult{ID: a.ID, Name: s.machineNames([]string{a.ID})[0], Error: "skipped: " + row.Why, State: updSkipped, Why: row.Why}
			// A machine that does not answer. When the confirmation's dialog listed it and it stopped answering
			// before the kept request ran, its update did not start: "offline", counted with the ones that did
			// not. When it was never part of this press (the button counts the machines that can update, the
			// dialog lists the same ones), it is "skipped": "Update all (4)" does not answer "4 started, 1 not"
			// about a fifth machine that was off all along (audit 6, r4-b1-13).
			if g := c.approved(); row.Status != "online" && g != nil && g.plan.shown(a.ID, s.updateTarget(a).req) {
				res.State = updOffline
			}
			out = append(out, res)
			continue
		}
		j := &job{a: a, params: updateParams(row, latest, true), target: -1}
		if t := s.updateTarget(a); c.wantsTarget(a.ID) {
			j.target = len(targets)
			targets = append(targets, t)
		} else {
			others = append(others, t)
		}
		jobs = append(jobs, j)
	}
	confirmed, err := c.confirmControl(u, "control actions", targets, others)
	if err != nil {
		return err
	}
	if !s.control.allow(u.Sub) {
		return c.limited(u, s.control, "too many control requests")
	}
	if len(jobs) == 0 && len(out) == 0 {
		return apierr.New(404, "no agent has self_update: true in config.yaml")
	}
	var wg sync.WaitGroup
	for _, j := range jobs {
		if j.target >= 0 {
			if oc := confirmed[j.target]; oc.err != nil { // its machine cannot be sent to (a broken trust file, no approval for it)
				j.res, j.err = updateResult{ID: j.a.ID, State: updNotSent}, oc.err
				continue
			} else if oc.approval != nil {
				j.ap = oc.approval
				j.params["approval"] = "sent" // the machine checks it; the token itself is never written anywhere
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := c.callUpdate(u, j.a, j.ap)
			j.res, j.err = updateResult{ID: j.a.ID, OK: err == nil, Result: data, State: updStarted}, err
			var ae *apierr.Error
			var lost *powerctl.Unanswered
			switch {
			case err == nil:
			case errors.As(err, &lost): // no answer of the agent: it may have arrived (agentCall, unanswered)
				j.res.State = updNoAnswer
			case errors.As(err, &ae) && ae.Status == 502: // the request did not leave the panel (agentCall)
				j.res.State = updOffline
			default:
				j.res.State = updRefused
			}
		}()
	}
	wg.Wait()
	for _, j := range jobs { // audit entries one after the other, after the calls
		outcome := "ok"
		j.res.Name = s.machineNames([]string{j.a.ID})[0]
		if j.err != nil {
			outcome = errorOutcome(j.err)
			j.res.Error, j.res.Why = outcome, shortWhy(j.err)
			var lost *powerctl.Unanswered
			if errors.As(j.err, &lost) { // the line says what is not known, not "no answer (no answer ...)"
				if lost.Short != "" {
					j.res.Why = lost.Short
				}
				j.res.Why += ": the update may have started"
			}
		}
		c.audit(u, "update.agent", j.a.ID, j.params, outcome)
		if j.err == nil {
			s.updateEvent(j.a, j.params)
		}
		out = append(out, j.res)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	msg, lines, allOK := updateSummary(out)
	c.json(200, map[string]any{"results": out, "message": msg, "lines": lines, "all_ok": allOK})
	return nil
}

func (s *Server) updateEvent(a config.Agent, params map[string]any) {
	sys := a.ID
	name := a.DisplayName()
	if x := s.Poller.RelaySystem(a.ID); x != nil {
		sys, name = x.ID, x.Name
	}
	s.Notes.Add(notifications.Event{Kind: "update_started", Type: "update", Severity: notifications.Info, SystemID: sys,
		Host: name, Text: fmt.Sprintf("agent update started (%v to %v)", params["from"], params["latest"])})
}
