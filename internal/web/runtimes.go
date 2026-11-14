package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// The timings of "Update unpowered hosts" IN FORCE: what config.yaml says, with what an admin changed in the
// panel laid over it ("Run timings", api_runtimings.go; the store is adminstate.RunTimings). Everything
// that shows a timing or starts a run asks here, never config.yaml alone: the page, the start dialog, the
// lines of "Confirm it's you", the numbers a passkey signs (wuTimes.ticket), and the snapshot a start hands
// to the run (wakerun.StartRequest.Settings).
//
// config.yaml's values are the run manager's own settings (s.WakeRuns.Settings, made once in New) and
// approval.run_approval_wait_s. They are never changed while the panel runs. The values in force are worked
// out at each use from them and from state.json as the panel reads it at that moment: there is no second
// copy that could say something else than the file `fairy admin config check` reads.

// wuTimes is one reading of the timings in force: the panel's five (as the run's Settings) and how long a
// run asks for its second passkey.
type wuTimes struct {
	set         wakerun.Settings
	confirmWait int // approval.run_approval_wait_s, seconds
}

// rtConfig is the value config.yaml gives a timing: what the panel was started with.
func (s *Server) rtConfig(key string) int {
	set := s.WakeRuns.Settings
	switch key {
	case config.KeyOffAfter:
		if set.OffAfter > 0 {
			return int(set.OffAfter / time.Second)
		}
		return int(wakerun.MinOff / time.Second)
	case config.KeyOnlineTimeout:
		return int(set.OnlineTimeout / time.Second)
	case config.KeySettle:
		return int(set.Settle / time.Second)
	case config.KeyShutdownDelay:
		return int(set.ShutdownDelay / time.Second)
	case config.KeyRequestsPerMin:
		return set.PerAgentMin
	case config.KeyRunApprovalWait:
		if v := s.Cfg.Approval.RunApprovalWaitS; v >= 60 && v <= 3600 {
			return v
		}
		return 600 // a configuration that was not made by config.Parse: the default
	}
	return 0
}

// withTiming is t with one timing set to v (a key that is none of the six: t).
func (t wuTimes) withTiming(key string, v int) wuTimes {
	d := time.Duration(v) * time.Second
	switch key {
	case config.KeyOffAfter:
		t.set.OffAfter, t.set.OffAfterS = d, v
	case config.KeyOnlineTimeout:
		t.set.OnlineTimeout, t.set.OnlineTimeoutS = d, v
	case config.KeySettle:
		t.set.Settle, t.set.SettleS = d, v
	case config.KeyShutdownDelay:
		t.set.ShutdownDelay, t.set.ShutdownDelayS = d, v
	case config.KeyRequestsPerMin:
		t.set.PerAgentMin = v
	case config.KeyRunApprovalWait:
		t.confirmWait = v
	}
	return t
}

// value is one timing of t in its unit (seconds; requests a minute).
func (t wuTimes) value(key string) int {
	switch key {
	case config.KeyOffAfter:
		return int(t.set.OffAfter / time.Second)
	case config.KeyOnlineTimeout:
		return int(t.set.OnlineTimeout / time.Second)
	case config.KeySettle:
		return int(t.set.Settle / time.Second)
	case config.KeyShutdownDelay:
		return int(t.set.ShutdownDelay / time.Second)
	case config.KeyRequestsPerMin:
		return t.set.PerAgentMin
	case config.KeyRunApprovalWait:
		return t.confirmWait
	}
	return 0
}

// wuTimesNow reads the timings in force once. A decision of the panel's store counts only when it is a value
// its key can have (the store's reader leaves out everything else; asked again here, so that a number below a
// floor cannot come into force whatever the reader is changed to).
func (s *Server) wuTimesNow() wuTimes { return s.wuTimesWith(s.Poller.Admin.RunTimings()) }

// wuTimesWith is the timings in force when the panel's store holds o.
func (s *Server) wuTimesWith(o adminstate.RunTimings) wuTimes {
	t := wuTimes{set: s.WakeRuns.Settings, confirmWait: s.rtConfig(config.KeyRunApprovalWait)}
	if t.set.OffAfter <= 0 {
		t.set.OffAfter, t.set.OffAfterS = wakerun.MinOff, int(wakerun.MinOff/time.Second)
	}
	for _, tm := range config.RunTimings() {
		if v, ok := o.Value(tm.Key); ok && tm.In(v) {
			t = t.withTiming(tm.Key, v)
		}
	}
	return t
}

// ticket is the numbers the two passkeys of a run sign, from the timings t:
//
//   - valid_s: approval.TicketValidS, always.
//   - confirm wait: approval.run_approval_wait_s (default 600): how long the banner asks once the machines are up,
//     at most (the run ends its question earlier when a machine that waits would switch itself off before that).
//   - wait_s: how long an armed machine waits before it switches itself off: the longest another machine of
//     the run may still need to come up (wake_update.online_timeout_s), the confirm wait, and a minute; the
//     machines take at most an hour, and the confirm wait gives way to that.
//   - notice_s: selfOffNoticeS.
//   - within_s: how long a confirmed machine lets the run's one update and one shutdown through: 25 minutes
//     and the settle time, at most the 30 minutes a machine takes.
//
// A timing that was changed in the panel goes through exactly these clamps, like one of config.yaml.
func (t wuTimes) ticket() wakerun.Ticket {
	confirm := t.confirmWait
	if confirm < 60 || confirm > 3600 {
		confirm = 600
	}
	online := int(t.set.OnlineTimeout / time.Second)
	wait := online + confirm + 60
	if wait > api.RunWaitMaxS {
		wait = api.RunWaitMaxS
		confirm = max(60, min(confirm, wait-online-60))
	}
	return wakerun.Ticket{ValidS: approval.TicketValidS, WaitS: max(wait, api.RunWaitMinS), NoticeS: selfOffNoticeS,
		WithinS: min(api.RunWithinMaxS, 1500+int(t.set.Settle/time.Second)), ConfirmWaitS: confirm}
}

// wuStamp names the timings a start would run with: the panel's six as t has them, and for every machine of
// the lists its own two (0: it has none, the panel's count). A start that is kept for "Confirm it's you"
// carries the stamp of that moment (pending.go, actionStamp), and the panel's replay after the sign-in
// refuses to start when it is no longer true (wakeUpdateStart): the numbers the dialog and the confirmation
// showed, and the ones a ticket was signed with, are the numbers the run uses.
func (s *Server) wuStamp(t wuTimes, b wuStartBody) string {
	var sb strings.Builder
	for _, tm := range config.RunTimings() {
		fmt.Fprintf(&sb, "%s=%d;", tm.Key, t.value(tm.Key))
	}
	for _, id := range slices.Concat(b.Hosts, b.UpdateOnly) {
		own, delay := s.wuTimings(id)
		fmt.Fprintf(&sb, "%q=%d/%d;", id, int(own/time.Second), int(delay/time.Second))
	}
	return sb.String()
}

// What the page SHOWED. The page of "Update unpowered hosts" gets two kinds of short stamps with its data:
// one of the panel's six timings (timings_stamp of the page) and one for every machine, of its own two
// (timings_stamp of its row). The start dialog is drawn from one answer of the page's data; with the start it
// sends the stamps of that answer for the machines it lists (wuStartBody.Timings: the panel's, then each
// listed machine's, joined with "."). The start makes the same text from its one reading of what is in force
// and starts nothing when it is another one (wuTimingsNotShown): the numbers a run uses are the numbers its
// start dialog showed, also when somebody changed them while the dialog was open. A stamp is a hash cut to
// 12 hex digits: it says nothing, and it only has to differ when a number differs.
func shownStamp(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:6])
}

// shown is the stamp of the panel's six timings as t has them.
func (t wuTimes) shown() string {
	var sb strings.Builder
	for _, tm := range config.RunTimings() {
		fmt.Fprintf(&sb, "%s=%d;", tm.Key, t.value(tm.Key))
	}
	return shownStamp(sb.String())
}

// wuHostShown is the stamp of the own two timings of the machine shown under id (0: it has none).
func (s *Server) wuHostShown(id string) string {
	own, delay := s.wuTimings(id)
	return shownStamp(fmt.Sprintf("%q=%d/%d;", id, int(own/time.Second), int(delay/time.Second)))
}

// wuShown is what a start dialog that showed the timings t and the machines of b sends as "timings".
func (s *Server) wuShown(t wuTimes, b wuStartBody) string {
	parts := []string{t.shown()}
	for _, id := range slices.Concat(b.Hosts, b.UpdateOnly) {
		parts = append(parts, s.wuHostShown(id))
	}
	return strings.Join(parts, ".")
}

// wuTimingsNotShown is the answer to a start whose dialog showed other timings than the ones in force.
const wuTimingsNotShown = "The run timings are not the ones this dialog showed any more: somebody changed them. Nothing was started: close this, open it again and look at the numbers."

// wuTimingLines is the lines of a start's "Confirm it's you" about the two timings that decide what a run
// switches off: the panel's, and for every machine of the wake list that has its own, its own.
func (s *Server) wuTimingLines(b wuStartBody, hosts map[string]wuHost, t wuTimes) []string {
	if len(b.Hosts) == 0 {
		return nil // nothing is woken, so nothing is taken for off or switched off
	}
	panelOff, panelDelay := t.value(config.KeyOffAfter), t.value(config.KeyShutdownDelay)
	lines := []string{fmt.Sprintf("A machine counts as off after %s without an answer; shutdown countdown %s", rtSpan(panelOff), rtSpan(panelDelay))}
	for _, id := range b.Hosts {
		own, delay := s.wuTimings(id)
		off, d := int(own/time.Second), int(delay/time.Second)
		if (off == 0 || off == panelOff) && (d == 0 || d == panelDelay) {
			continue
		}
		if off == 0 {
			off = panelOff
		}
		if d == 0 {
			d = panelDelay
		}
		lines = append(lines, fmt.Sprintf("%s, its own: counts as off after %s; shutdown countdown %s", hosts[id].Name, rtSpan(off), rtSpan(d)))
	}
	return lines
}

// wakeUpdateStamp is the stamp of a start request as it is kept (pending.go: actionStamp): made from the
// handler's one reading of the timings (reqCtx.wu), which the lines of the confirmation and the numbers of a
// ticket are made from too.
func (c *reqCtx) wakeUpdateStamp() string {
	var b wuStartBody
	if !decodeStrictly(c.body, &b) {
		return ""
	}
	return c.s.wuStamp(c.wuRead(), b)
}

// wuRead is the start handler's one reading of the timings in force; for a request the handler has not read
// them for (none today), what is in force now.
func (c *reqCtx) wuRead() wuTimes {
	if c.wu != nil {
		return *c.wu
	}
	return c.s.wuTimesNow()
}

// wuStartAt tells a test where a start stands (Server.wuStartStep). Nothing listens in the running panel.
func (s *Server) wuStartAt(step string) {
	if s.wuStartStep != nil {
		s.wuStartStep(step)
	}
}

// shutdownDelayWith is the countdown of the shutdown a run with the timings t asks the machine shown under id
// for: its own, else t's.
func (s *Server) shutdownDelayWith(t wuTimes, id string) time.Duration {
	if _, own := s.wuTimings(id); own > 0 {
		return own
	}
	return t.set.ShutdownDelay
}

// shutdownDelayFrom says where the countdown shutdownDelayWith returns comes from, for a sentence that tells
// an admin which number to change: the machine's own (a decision of "Edit host", or the wake_update: block of
// its systems: entry), or the panel's ("Run timings", or config.yaml).
func (s *Server) shutdownDelayFrom(t wuTimes, id string) string {
	_, block := s.Poller.ConfigWakeUpdate(id)
	_, own := s.Poller.WakeUpdateOverride(id)
	switch {
	case own > 0 && own != block:
		return "its own, set in Edit host"
	case own > 0:
		return "its own, wake_update.shutdown_delay_s of its systems: entry in config.yaml"
	case t.set.ShutdownDelay != s.WakeRuns.Settings.ShutdownDelay:
		return "the panel's, set in Run timings"
	}
	return "the panel's, wake_update.shutdown_delay_s in config.yaml"
}

// wuHeldToMaxDelay reports whether a start that wakes h holds h's shutdown countdown to runMaxDelay: the
// machine checks passkey approvals itself and can be woken by a run (wuCheck refuses such a start).
func wuHeldToMaxDelay(h wuHost) bool {
	return h.Checked && h.CanJoin && h.OptedIn && h.CanWake && !h.NoRun
}

// wuHeldFollowers counts the machines a start holds to runMaxDelay that have no countdown of their own: the
// ones the panel's countdown counts for. The Run timings dialog says so when its countdown is above that.
func (s *Server) wuHeldFollowers() int {
	n := 0
	for _, x := range s.Poller.Snapshot() {
		if h, ok := s.wuHostOf(x); ok && wuHeldToMaxDelay(h) {
			if _, own := s.wuTimings(x.ID); own == 0 {
				n++
			}
		}
	}
	return n
}
