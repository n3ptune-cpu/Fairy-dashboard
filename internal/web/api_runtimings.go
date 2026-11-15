package web

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// "Run timings" (Updates page, the "Update unpowered hosts" box, admins): the six timings of a run, changed
// in the panel. config.yaml (wake_update: and approval.run_approval_wait_s) stays the base and is never
// written; a change is kept as one decision per timing in state.json (adminstate.RunTimings) and counts for
// the next run that starts (runtimes.go). "Reset" takes a decision out.
//
//	GET /api/admin/run-timings    each timing: the value in force, where it comes from, config.yaml's value,
//	                              its range, and which changes of it ask for a confirmation of their own
//	PUT /api/admin/run-timings    {"off_after_s": 360, "shutdown_delay_s": 30, "reset": ["settle_s"]}
//
// The request says only what changes: a timing by its key with its new value (whole seconds; requests a
// minute for the pacing), and under "reset" the keys that go back to config.yaml. A value that is
// config.yaml's own is not stored: it is the same as a reset. Nothing else can be sent: the six keys and
// "reset", decoded strictly. No address, token, power control or self-update has a field here.
//
// These numbers are not cosmetic (TIMINGS-DESIGN.md has the analysis):
//
//   - off_after_s decides when a run takes a machine for off, and so for one it woke itself, which is a
//     machine it switches off after the update. EVERY change of it needs its own "Confirm it's you"
//     (stepUpEach): a confirmation given for something else a minute ago does not count.
//   - shutdown_delay_s is the countdown somebody at the machine has to cancel: its own confirmation whenever
//     the value in force gets lower.
//   - online_timeout_s: a machine a person switches on while the run waits for it is treated as woken by the
//     run: its own confirmation whenever the value in force gets higher.
//   - the others need the usual confirmation window.
//
// The ranges are config.yaml's (config.RunTimings), checked here before anybody is asked to confirm, and
// again by the store's writer and reader and by the run's own floors.
//
// Who may: an admin's browser session (API tokens: 403, on GET too), the CSRF header and the panel's own
// origin, the confirmation above, the rate limit of admin changes. The kill switch does not apply: it stops
// device control, and no run starts while it is on. Every change is one entry in the audit log
// (wakeupdate.timings, each key with its old and its new value) and one in the bell.

const (
	runTimingsRoute  = "PUT /api/admin/run-timings"
	rtDenied         = "only admins can change the run timings"
	rtAuditAction    = "wakeupdate.timings"
	rtConfirmAlways  = "always" // every change of the value asks for its own confirmation
	rtConfirmLower   = "lower"  // a change that makes the value in force lower does
	rtConfirmHigher  = "higher" // a change that makes the value in force higher does
	rtStepUpWhat     = "changes to the run timings"
	rtStepUpEachWhat = "changes to when a run takes a machine for off or switches it off"
)

// rtConfirm says which changes of a timing need a confirmation of their own ("": none, the usual window).
var rtConfirm = map[string]string{
	config.KeyOffAfter:      rtConfirmAlways,
	config.KeyShutdownDelay: rtConfirmLower,
	config.KeyOnlineTimeout: rtConfirmHigher,
}

// rtLabel is what the dialog, "Confirm it's you" and the audit log call each timing (app.js has the same
// words: RT_ROWS; a test holds the two together).
var rtLabel = map[string]string{
	config.KeyOffAfter:        "Counts as off after",
	config.KeyOnlineTimeout:   "Wait for a woken machine",
	config.KeySettle:          "Pause before the shutdown",
	config.KeyShutdownDelay:   "Shutdown countdown",
	config.KeyRequestsPerMin:  "Requests a minute",
	config.KeyRunApprovalWait: "Time for the second passkey",
}

// rtReading is a value as a line says it: seconds with their reading ("360 s = 6 minutes", "90 s = 1 min
// 30 s", "45 s"), the pacing as its number. app.js: rtReading.
func rtReading(key string, v int) string {
	if key == config.KeyRequestsPerMin {
		return fmt.Sprintf("%d", v)
	}
	if v < 60 {
		return fmt.Sprintf("%d s", v)
	}
	h, m, s := v/3600, v%3600/60, v%60
	one := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	var words string
	switch {
	case m == 0 && s == 0:
		words = one(h, "hour")
	case h == 0 && s == 0:
		words = one(m, "minute")
	default:
		for _, p := range []struct {
			n    int
			unit string
		}{{h, "h"}, {m, "min"}, {s, "s"}} {
			if p.n > 0 {
				if words != "" {
					words += " "
				}
				words += fmt.Sprintf("%d %s", p.n, p.unit)
			}
		}
	}
	return fmt.Sprintf("%d s = %s", v, words)
}

// rtSpan is a number of seconds inside a sentence ("6 minutes", "1 min 30 s", "45 seconds").
func rtSpan(v int) string {
	if v < 60 {
		if v == 1 {
			return "1 second"
		}
		return fmt.Sprintf("%d seconds", v)
	}
	_, words, _ := strings.Cut(rtReading("", v), " = ")
	return words
}

// rtConsequence is the sentence that says what a change means for the machines, for the three timings whose
// change can switch a machine off or shorten the time to stop that ("": the timing has none). v is the value
// after the change. The sentence of off_after_s is what a run does (internal/wakerun, steps.go): a machine
// that gave nothing for that long counts as off and is sent the wake; the run switches off only a machine
// that then comes up FRESHLY STARTED (its uptime begins after the wake), which is one that was off, or one
// that was in a restart. A machine that was on and out of reach, or asleep, answers with an older uptime and
// is left on. It is in the dialog, in "Confirm it's you" and in the audit entry, under the line of a
// change that asks for its own confirmation (every change of off_after_s, a shorter countdown, a longer wait
// for a woken machine). app.js: rtConsequence.
func rtConsequence(key string, v int) string {
	switch key {
	case config.KeyOffAfter:
		return fmt.Sprintf("A machine that is silent for longer than %s counts as off. If it was restarting and comes up during the run, the run can switch it off after its update.", rtSpan(v))
	case config.KeyShutdownDelay:
		return fmt.Sprintf("Somebody at a machine has %s to cancel the run's shutdown.", rtSpan(v))
	case config.KeyOnlineTimeout:
		return fmt.Sprintf("A machine somebody switches on while the run waits for it (up to %s) is treated as woken by the run, and switched off after its update.", rtSpan(v))
	}
	return ""
}

func init() {
	actionText[runTimingsRoute] = "change the run timings"
	actionList[runTimingsRoute] = (*reqCtx).runTimingsItems
	actionStamp[runTimingsRoute] = func(_ *reqCtx, p *pendingAction) string { return rtShown(p.lines) }
	apiDoc = append(apiDoc,
		struct{ method, path, summary string }{"GET", "/api/admin/run-timings", "Run timings: the six timings of \"Update unpowered hosts\" (off_after_s, online_timeout_s, settle_s, shutdown_delay_s, agent_requests_per_min, run_approval_wait_s), each with the value in force, where it comes from (config.yaml or changed in the panel), config.yaml's value, its range, and `confirm` (always | lower | higher | \"\": which changes ask for their own confirmation); `confirm_wait_s`: the time for the second passkey a run started now would use; `active`: a run is going and keeps its own numbers (admins, browser session only)"},
		struct{ method, path, summary string }{"PUT", "/api/admin/run-timings", "Run timings: change timings of \"Update unpowered hosts\": {off_after_s, online_timeout_s, settle_s, shutdown_delay_s, agent_requests_per_min, run_approval_wait_s: whole numbers in config.yaml's ranges, reset: [keys]}; only what is named changes. Stored as one decision per key laid over config.yaml in state.json (config.yaml is not written; a value equal to config.yaml's and a reset take the decision out); counts from the next run, a run that is going keeps its numbers. Admins, browser session only (API tokens get 403), CSRF header, step-up; its own confirmation every time for any change of off_after_s, for a lower shutdown_delay_s and for a higher online_timeout_s. 422: an unknown key, a value that is no whole number in its range, a key twice or both set and reset, nothing to change. Audited as wakeupdate.timings"})
}

func (s *Server) runTimingsRoutes() {
	// What the reader leaves out of state.json is said in the log (once) when the panel starts.
	_, _ = s.Poller.Admin.RunTimingsRead()
	s.mux.HandleFunc("GET /api/admin/run-timings", s.h(s.runTimingsGet))
	s.mux.HandleFunc("PUT /api/admin/run-timings", s.h(s.runTimingsPut))
}

// runTimingsReq is everything "Run timings" can be asked to change: the six keys, and the keys to reset.
type runTimingsReq struct {
	OffAfterS           *int     `json:"off_after_s"`
	OnlineTimeoutS      *int     `json:"online_timeout_s"`
	SettleS             *int     `json:"settle_s"`
	ShutdownDelayS      *int     `json:"shutdown_delay_s"`
	AgentRequestsPerMin *int     `json:"agent_requests_per_min"`
	RunApprovalWaitS    *int     `json:"run_approval_wait_s"`
	Reset               []string `json:"reset"`
}

// set is the value the request gives a key (nil: it does not name it).
func (r runTimingsReq) set(key string) *int {
	switch key {
	case config.KeyOffAfter:
		return r.OffAfterS
	case config.KeyOnlineTimeout:
		return r.OnlineTimeoutS
	case config.KeySettle:
		return r.SettleS
	case config.KeyShutdownDelay:
		return r.ShutdownDelayS
	case config.KeyRequestsPerMin:
		return r.AgentRequestsPerMin
	case config.KeyRunApprovalWait:
		return r.RunApprovalWaitS
	}
	return nil
}

// check is everything about a request that can be said without looking at what is stored: is there anything
// in it, is every value one its key can have, is no key reset twice or both set and reset. A request that
// fails here is answered 422 before the admin is asked to confirm it.
func (r runTimingsReq) check() error {
	bad := func(format string, a ...any) error { return apierr.New(422, fmt.Sprintf(format, a...)) }
	if len(r.Reset) > len(config.RunTimings()) {
		return bad("reset: too many entries")
	}
	named := 0
	seen := map[string]bool{}
	for _, k := range r.Reset {
		if _, ok := config.RunTimingOf(k); !ok {
			return bad("reset: no such timing")
		}
		if seen[k] {
			return bad("reset: %s is there twice", k)
		}
		seen[k] = true
		named++
	}
	for _, tm := range config.RunTimings() {
		v := r.set(tm.Key)
		if v == nil {
			continue
		}
		if seen[tm.Key] {
			return bad("%s is both set and reset", tm.Key)
		}
		if !tm.In(*v) {
			return bad("%s must be %d-%d", tm.Key, tm.Min, tm.Max)
		}
		named++
	}
	if named == 0 {
		return bad("nothing to change")
	}
	return nil
}

// runTimingsBody reads the request of a PUT: strict JSON that says each thing once (decodeBody), every name
// spelled exactly (exactNames), and a request that can be meant (check).
func (c *reqCtx) runTimingsBody(req *runTimingsReq) error {
	b, err := io.ReadAll(io.LimitReader(c.r.Body, 64<<10))
	if err != nil {
		return apierr.New(400, "unreadable body")
	}
	c.r.Body = io.NopCloser(bytes.NewReader(b))
	if err := c.decodeBody(req); err != nil {
		return err
	}
	if !exactNames(b) {
		return apierr.New(422, "invalid request body: a field is not spelled exactly as the API names it")
	}
	return req.check()
}

// rtChange is one timing a request changes: for the audit log and the lines.
type rtChange struct {
	Key        string `json:"key"`
	Before     int    `json:"before"`      // the value in force before
	After      int    `json:"after"`       // ... and after
	BeforeFrom string `json:"before_from"` // "config.yaml" or "the panel"
	AfterFrom  string `json:"after_from"`
	// own: this change needs a confirmation of its own (rtConfirm)
	own bool
}

// rtPlan is what a request does to the stored decisions o: the changes, one line for each (and under it the
// consequence sentence where the timing has one), and whether any of them needs its own confirmation. It
// changes o. A key whose stored decision and value in force both stay as they are is no change.
func (s *Server) rtPlan(o *adminstate.RunTimings, req runTimingsReq) (changes []rtChange, lines []string, own bool) {
	from := func(stored bool) string {
		if stored {
			return "the panel"
		}
		return "config.yaml"
	}
	for _, tm := range config.RunTimings() {
		cfg := s.rtConfig(tm.Key)
		before, stored := o.Value(tm.Key)
		if !stored || !tm.In(before) {
			before, stored = cfg, false
		}
		v, reset := req.set(tm.Key), slices.Contains(req.Reset, tm.Key)
		if v == nil && !reset {
			continue
		}
		after, keep := cfg, false
		if v != nil && *v != cfg {
			after, keep = *v, true
		}
		if after == before && keep == stored {
			continue
		}
		if keep {
			o.Set(tm.Key, after)
		} else {
			o.Drop(tm.Key)
		}
		ch := rtChange{Key: tm.Key, Before: before, After: after, BeforeFrom: from(stored), AfterFrom: from(keep)}
		switch rtConfirm[tm.Key] {
		case rtConfirmAlways:
			ch.own = after != before
		case rtConfirmLower:
			ch.own = after < before
		case rtConfirmHigher:
			ch.own = after > before
		}
		own = own || ch.own
		changes = append(changes, ch)
		line := rtLabel[tm.Key] + ": "
		switch {
		case !keep && after == before:
			line += "back to config.yaml: nothing changes now (" + rtReading(tm.Key, after) + ")"
		case !keep:
			line += "back to config.yaml: " + rtReading(tm.Key, after) + " (was " + rtReading(tm.Key, before) + ")"
		default:
			line += rtReading(tm.Key, after) + " (was " + rtReading(tm.Key, before) + ")"
		}
		lines = append(lines, line)
		if c := rtConsequence(tm.Key, after); c != "" && ch.own { // said where the change is the kind that asks for its own confirmation
			lines = append(lines, c)
		}
	}
	return changes, lines, own
}

// runTimingsView is the answer of GET, and of PUT after the change.
func (s *Server) runTimingsView(o adminstate.RunTimings, ignored []string) map[string]any {
	rows := []map[string]any{}
	t := s.wuTimesNow()
	for _, tm := range config.RunTimings() {
		cfg := s.rtConfig(tm.Key)
		row := map[string]any{"key": tm.Key, "value": cfg, "from": "config", "config": cfg, "default": tm.Default, "min": tm.Min, "max": tm.Max,
			"confirm": rtConfirm[tm.Key], "by": nil, "at": nil}
		if v, ok := o.Value(tm.Key); ok && tm.In(v) {
			row["value"], row["from"] = v, "panel"
			if m := o.Marks[tm.Key]; m.By != "" {
				row["by"] = m.By
			}
			if m := o.Marks[tm.Key]; !m.At.IsZero() {
				row["at"] = m.At.UTC().Format(time.RFC3339)
			}
		}
		rows = append(rows, row)
	}
	active, _ := s.WakeRuns.Snapshot()
	if ignored == nil {
		ignored = []string{}
	}
	return map[string]any{"rev": o.Rev, "timings": rows, "confirm_wait_s": t.ticket().ConfirmWaitS, "active": active != nil,
		"ignored": capLines(ignored), "ignored_count": o.Ignored,
		// how many machines that check passkey approvals themselves follow the panel's countdown, and the most
		// a run takes for them: the dialog says one line when its countdown is above that
		"checked_hosts": s.wuHeldFollowers(), "checked_max_s": int(runMaxDelay / time.Second)}
}

// runTimingsRead: who may open the dialog: an admin with a browser session (API tokens: 403).
func (c *reqCtx) runTimingsRead() (*model.User, error) {
	u, err := c.requireSession()
	if err != nil {
		return nil, err
	}
	if _, g, _ := c.grant(); !g.Admin() {
		return nil, c.deny(u, 403, rtDenied, "not an admin")
	}
	return u, nil
}

func (s *Server) runTimingsGet(c *reqCtx) error {
	if _, err := c.runTimingsRead(); err != nil {
		return err
	}
	o, ignored := s.Poller.Admin.RunTimingsRead()
	c.json(200, s.runTimingsView(o, ignored))
	return nil
}

// rtShown is the stamp of a kept change: made from every line of its confirmation, in full (shownStampOf).
// The replay makes the lines again, from what is stored at the moment of the write, and saves only when the
// stamp is the same (runTimingsPut): every line has the value before and the value after, so the same lines
// are the same change, with the same sentences about what it means and the same kind of confirmation.
func rtShown(lines []string) string { return shownStampOf(lines) }

// rtNotShown is the answer to a confirmed change that is no longer the change its confirmation showed.
const rtNotShown = "The run timings were changed by somebody else after you pressed Save, so what you confirmed is not what would be saved now. Nothing was saved: look at the dialog again and press Save."

// errRtNotShown: the lines the write would make are not the lines that were confirmed.
var errRtNotShown = errors.New("not the change that was confirmed")

// runTimingsItems is the panel's own line for each thing a kept request would change, for "Confirm it's you"
// (pending.go: actionList): the lines rtPlan makes, on a copy of what is stored now.
func (c *reqCtx) runTimingsItems(*model.User) []string {
	var req runTimingsReq
	if !decodeStrictly(c.body, &req) || !exactNames(c.body) || req.check() != nil {
		return nil
	}
	o := c.s.Poller.Admin.RunTimings().Clone()
	_, lines, _ := c.s.rtPlan(&o, req)
	return lines
}

func (s *Server) runTimingsPut(c *reqCtx) error {
	// Who asks comes first: a browser session, the page's header, an admin. Nobody else is told anything
	// about what they sent.
	u, err := c.adminSession(rtDenied)
	if err != nil {
		return err
	}
	var req runTimingsReq
	if err := c.runTimingsBody(&req); err != nil { // its form and its ranges, before the admin is asked to confirm it
		return err
	}
	// What it would change, on a copy of what is stored now: that decides which confirmation is asked for. A
	// request that changes nothing asks for none.
	cur, _ := s.Poller.Admin.RunTimingsRead()
	probe := cur.Clone()
	planned, _, own := s.rtPlan(&probe, req)
	if len(planned) == 0 {
		out := s.runTimingsView(cur, nil)
		out["ok"], out["changed"], out["message"] = true, []string{}, "nothing to change"
		c.json(200, out)
		return nil
	}
	if own {
		// its own confirmation, every time: only the panel's replay of this very request passes
		err = c.stepUpEach(u, rtStepUpEachWhat)
	} else {
		err = c.stepUp(u, rtStepUpWhat)
	}
	if err != nil {
		return err
	}
	if !s.adminRL.allow(u.Sub) {
		return c.limited(u, s.adminRL, "too many changes")
	}
	// One locked read-modify-write: the request is applied to what state.json holds at that moment. What it
	// changes there can differ from what was planned above (another admin saved in between).
	//
	// The panel's replay of a kept request saves only what its confirmation showed: the lines are made again
	// here, inside the lock, and when they are not the lines the admin confirmed (the stamp of the kept
	// action, rtShown), nothing is written and the answer says to look again. So a change of off_after_s, a
	// shorter countdown or a longer wait is never made on a confirmation that showed something else, and the
	// audit entry below holds exactly what was shown and saved.
	//
	// A request that passed on the usual window (no replay) and would now make a change that needs its own
	// confirmation is not made either: the admin is asked, for what the request does now.
	var changes []rtChange
	var lines []string
	var after adminstate.RunTimings
	var changed bool
	shown, replay := c.keptStamp()
	confirmed := own // (the replay of a kept change is past stepUpEach below whatever kind was asked for)
	if s.rtBeforeWrite != nil {
		s.rtBeforeWrite()
	}
	for try := 0; ; try++ {
		after, changed, err = s.Poller.Admin.EditRunTimings(actorOf(u), func(o *adminstate.RunTimings) error {
			var needsOwn bool
			changes, lines, needsOwn = s.rtPlan(o, req)
			if replay && rtShown(shownItems(lines)) != shown {
				return errRtNotShown
			}
			if needsOwn && !confirmed {
				return errRtAskAgain
			}
			return nil
		})
		if err != errRtAskAgain || try > 0 {
			break
		}
		if err := c.stepUpEach(u, rtStepUpEachWhat); err != nil {
			return err
		}
		confirmed = true // nobody is asked to confirm in this mode of the panel (stepUpCheck), or this is the replay
	}
	if err == errRtNotShown {
		c.audit(u, rtAuditAction, "run timings", map[string]any{"confirmed": confirmedLines(shown)},
			"error 409: the run timings were changed after the confirmation was asked for; nothing was saved")
		return apierr.New(409, rtNotShown)
	}
	if err != nil {
		c.audit(u, rtAuditAction, "run timings", map[string]any{"changes": planned}, "failed: not saved")
		return err
	}
	out := s.runTimingsView(after, nil)
	out["ok"], out["changed"] = true, []string{}
	if !changed {
		out["message"] = "nothing to change"
		c.json(200, out)
		return nil
	}
	active, _ := s.WakeRuns.Snapshot()
	params := map[string]any{"changes": changes, "lines": lines, "rev": after.Rev}
	if active != nil {
		params["run_going"] = active.ID // that run keeps the numbers it started with
	}
	c.audit(u, rtAuditAction, "run timings", params, "ok")
	if s.Notes != nil {
		s.Notes.Add(notifications.Event{Kind: "run_timings", Type: "update", Severity: notifications.Info, Host: wuNoteHost,
			Text: "run timings changed by " + actorOf(u)})
	}
	out["message"], out["changed"] = "saved", lines
	if active != nil {
		out["message"] = "saved; the run that is going keeps its numbers"
	}
	c.json(200, out)
	return nil
}

// errRtAskAgain: what a request would change now needs its own confirmation, and it has none.
var errRtAskAgain = errors.New("this change needs its own confirmation")
