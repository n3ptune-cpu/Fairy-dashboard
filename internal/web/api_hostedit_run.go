package web

import (
	"fmt"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// "Edit host", a machine's own run timings: the two timings of "Update unpowered hosts" that one machine can
// have of its own, in the dialog and the route of api_hostoptions.go.
//
//	PUT /api/admin/hosts/{id}/options   ... "wake_update": {"off_after_s": 600, "shutdown_delay_s": 30},
//	                                         "reset": [{"field": "off_after_s"}, {"field": "shutdown_delay_s"}]
//
//   - off_after_s: this machine counts as off after this long without an answer (300 to 3600);
//   - shutdown_delay_s: the countdown of the shutdown a run asks this machine for (10 to 3600).
//
// The ranges are config.yaml's (config.MachineRunTimings), checked before anybody is asked to confirm
// anything, again when the decision is written (adminstate.RunEdit.Check), and again where a decision is
// used (poller: runWith; the run: wakerun.Host.setTimings). Nothing else of a run can be set for one machine.
//
// Where they are kept: decisions in state.json next to the machine's name and Wake-on-LAN
// (adminstate.HostEdit.Run), laid over what counts for the machine without them: the wake_update: block of
// its systems: entry in config.yaml, or where it has none, the panel's own timing ("Run timings" on the
// Updates page). A value that is what counts anyway is not stored, and takes a stored decision out; Reset
// takes a decision out. The number that was sent is always the number that counts for the machine afterwards.
//
// Who may: as for the rest of "Edit host", and for these two a confirmation of its own, every time
// (stepUpEach), whatever the change is. With a short off_after_s a machine that is on, and only silent for a
// while, is taken for off, and a run that "woke" it switches it off after its update; with a short countdown
// nobody at the machine can stop that. Both are numbers an admin may want; neither may ride on a confirmation
// given for something else a minute ago. The rule is made from the form of the request alone, before
// anything is read: it cannot be raced by a change of config.yaml's or the panel's values in between.
//
// A run takes a machine's two timings once, when it starts (wakerun.Manager.Start): a change made here while
// a run is going counts from the next run. A start that was kept for "Confirm it's you" carries the timings
// it was asked with (runtimes.go: wuStamp, which has every listed machine's own two): changed in between, the
// start is refused.

// hoRunReq is what a request says a machine's own run timings shall be. A field that is not sent is not
// changed.
type hoRunReq struct {
	OffAfterS      *int `json:"off_after_s"`
	ShutdownDelayS *int `json:"shutdown_delay_s"`
}

func (r *hoRunReq) value(key string) *int {
	switch {
	case r == nil:
		return nil
	case key == config.KeyOffAfter:
		return r.OffAfterS
	case key == config.KeyShutdownDelay:
		return r.ShutdownDelayS
	}
	return nil
}

// hoRunField reports whether field is one of the two timings (a reset names them like that, without an id).
func hoRunField(field string) bool {
	for _, t := range config.MachineRunTimings() {
		if t.Key == field {
			return true
		}
	}
	return false
}

// touchesRun reports whether the request says anything about the machine's own run timings (it then needs
// its own confirmation).
func (r hostOptionsReq) touchesRun() bool {
	if r.WakeUpdate != nil {
		return true
	}
	for _, t := range config.MachineRunTimings() {
		if r.resets(t.Key) {
			return true
		}
	}
	return false
}

// checkRun is the part of check about the machine's own run timings: each value is one its key can have.
func (r hostOptionsReq) checkRun() error {
	if r.WakeUpdate == nil {
		return nil
	}
	any := false
	for _, t := range config.MachineRunTimings() {
		v := r.WakeUpdate.value(t.Key)
		if v == nil {
			continue
		}
		switch any = true; {
		case r.resets(t.Key):
			return apierr.New(422, "wake_update: "+t.Key+": a new value and a reset at once")
		case !t.In(*v):
			return apierr.Errf(422, "wake_update: %s must be %d to %d", t.Key, t.Min, t.Max)
		}
	}
	if !any {
		return apierr.New(422, "wake_update: nothing is given")
	}
	return nil
}

// stepUpWhat is what a save of "Edit host" is called in "Confirm it's you", and whether this request needs a
// confirmation of its own (each): anything about Wake-on-LAN (api_hostedit.go) and anything about the
// machine's run timings do.
func (r hostOptionsReq) stepUpWhat() (what string, each bool) {
	switch wol, run := r.touchesWol(), r.touchesRun(); {
	case wol && run:
		return "changes to a host's Wake-on-LAN and run timings", true
	case wol:
		return "changes to a host's Wake-on-LAN", true
	case run:
		return "changes to a host's run timings", true
	}
	return "changes to a host", false
}

// hoRunBase is what counts for a machine's timing without a decision of "Edit host": config is what the
// wake_update: block of its systems: entry sets (0: nothing), panel is the panel's own timing in force.
type hoRunBase struct{ config, panel int }

// value is the timing without a decision, and where it comes from: "config" (config.yaml's entry for the
// machine) or "run" (the panel's: "Run timings").
func (b hoRunBase) value() (int, string) {
	if b.config > 0 {
		return b.config, "config"
	}
	return b.panel, "run"
}

// hostRunBases collects hoRunBase for the two timings of the machine shown under row, as the panel reads its
// run timings now: for the dialog's data and for the lines of a confirmation. A save does not use this
// reading (hostRunBasesWith).
func (s *Server) hostRunBases(row string) map[string]hoRunBase {
	return s.hostRunBasesWith(row, s.Poller.Admin.RunTimings())
}

// hostRunBasesWith is hostRunBases when the panel's store of run timings holds panel. A save asks with what
// state.json holds inside the lock of its own write (adminstate.EditHostOptionsWith): whether a number is
// "what counts anyway" (not stored) or a decision (stored) is then decided against the panel's timing that
// is in the file at the moment of the write, not one that was read before and changed since.
func (s *Server) hostRunBasesWith(row string, panel adminstate.RunTimings) map[string]hoRunBase {
	off, delay := s.Poller.ConfigWakeUpdate(row)
	times := s.wuTimesWith(panel)
	return map[string]hoRunBase{
		config.KeyOffAfter:      {config: off, panel: times.value(config.KeyOffAfter)},
		config.KeyShutdownDelay: {config: delay, panel: times.value(config.KeyShutdownDelay)},
	}
}

// runValue is the timing that counts for the machine under the decisions e, and where it comes from: "panel"
// (changed here), "config" or "run" (hoRunBase.value).
func (h *hoHost) runValue(e adminstate.RunEdit, key string) (int, string) {
	if t, ok := config.RunTimingOf(key); ok && e.Value(key) != 0 && t.In(e.Value(key)) {
		return e.Value(key), "panel"
	}
	return h.run[key].value()
}

// The words of a line about a machine's own run timing, which the dialog makes the same way (app.js:
// ehRunPlan; a test holds the two together).
const (
	hoBackRun     = "back to Run timings: "
	hoNothingNow  = "nothing changes now"
	hoFromRun     = "run timings"
	hoRunGoing    = "; the run that is going keeps its numbers"
	hoRunAuditKey = "run_going"
)

// hoRunFrom is where a timing comes from, in the audit log's words.
func hoRunFrom(from string) string {
	switch from {
	case "panel":
		return hoFrom(true)
	case "config":
		return hoFrom(false)
	}
	return hoFromRun
}

// hoRunConsequence is what a machine's own timing means for it, said under a change of the kind that the
// panel's own "Run timings" asks a confirmation of its own for: any change of off_after_s, and a shorter
// countdown. The same sentence is in "Confirm it's you" and in the audit log.
func hoRunConsequence(key, name string, v int) string {
	switch key {
	case config.KeyOffAfter:
		return fmt.Sprintf("If %s is silent for longer than %s, it counts as off. If it was restarting and comes up during a run, the run switches it off after its update.", name, rtSpan(v))
	case config.KeyShutdownDelay:
		return fmt.Sprintf("Somebody at %s has %s to cancel a run's shutdown.", name, rtSpan(v))
	}
	return ""
}

// applyRun makes the changes of req about the machine's own run timings in e (was: the entry before the
// request). name is what the machine is called once the request is made. It returns one line for each
// timing that changes, with the sentence about what it means under it, and the changes for the audit log.
func (h *hoHost) applyRun(req hostOptionsReq, was adminstate.HostEdit, e *adminstate.HostEdit, name string) (lines []string, changes []hoChange) {
	for _, t := range config.MachineRunTimings() {
		base, baseFrom := h.run[t.Key].value()
		v, reset := req.WakeUpdate.value(t.Key), req.resets(t.Key)
		switch {
		case reset:
			e.Run.Set(t.Key, 0)
		case v == nil:
			continue
		case *v == base:
			e.Run.Set(t.Key, 0) // what counts anyway: nothing to keep (and the same as a Reset)
		default:
			e.Run.Set(t.Key, *v)
		}
		if was.Run.Value(t.Key) == e.Run.Value(t.Key) {
			continue
		}
		before, beforeFrom := h.runValue(was.Run, t.Key)
		after, afterFrom := h.runValue(e.Run, t.Key)
		label := rtLabel[t.Key]
		switch {
		case afterFrom == "panel":
			lines = append(lines, fmt.Sprintf("%s: %s (was %s)", label, rtReading(t.Key, after), rtReading(t.Key, before)))
		default:
			back := hoBack
			if baseFrom == "run" {
				back = hoBackRun
			}
			if after == before {
				lines = append(lines, fmt.Sprintf("%s: %s%s (%s)", label, back, hoNothingNow, rtReading(t.Key, after)))
			} else {
				lines = append(lines, fmt.Sprintf("%s: %s%s (was %s)", label, back, rtReading(t.Key, after), rtReading(t.Key, before)))
			}
		}
		if (t.Key == config.KeyOffAfter && after != before) || (t.Key == config.KeyShutdownDelay && after < before) {
			lines = append(lines, hoRunConsequence(t.Key, name, after))
		}
		var stored any
		if d := e.Run.Value(t.Key); d != 0 {
			stored = d
		}
		changes = append(changes, hoChange{Host: h.key, Field: t.Key, Before: before, After: after,
			BeforeFrom: hoRunFrom(beforeFrom), AfterFrom: hoRunFrom(afterFrom), Stored: stored})
	}
	return lines, changes
}

// runView is the "run" part of the dialog's data about the machine: its two timings as they count, where
// each comes from, what would count without a decision, and the range.
func (h *hoHost) runView(e adminstate.HostEdit) map[string]any {
	rows := []map[string]any{}
	for _, t := range config.MachineRunTimings() {
		v, from := h.runValue(e.Run, t.Key)
		base, baseFrom := h.run[t.Key].value()
		row := map[string]any{"key": t.Key, "value": v, "from": from, "base": base, "base_from": baseFrom, "min": t.Min, "max": t.Max, "by": nil, "at": nil}
		if from == "panel" {
			if row["by"] = strOrNil(e.By); !e.At.IsZero() {
				row["at"] = e.At.UTC().Format(time.RFC3339)
			}
		}
		rows = append(rows, row)
	}
	return map[string]any{"timings": rows}
}

// changesRun reports whether a save changed one of the machine's own run timings.
func changesRun(changes []hoChange) bool {
	for _, c := range changes {
		if c.Host != "" && hoRunField(c.Field) {
			return true
		}
	}
	return false
}
