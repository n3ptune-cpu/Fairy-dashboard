package web

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
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
func (s *Server) wuTimesNow() wuTimes {
	t := wuTimes{set: s.WakeRuns.Settings, confirmWait: s.rtConfig(config.KeyRunApprovalWait)}
	if t.set.OffAfter <= 0 {
		t.set.OffAfter, t.set.OffAfterS = wakerun.MinOff, int(wakerun.MinOff/time.Second)
	}
	o := s.Poller.Admin.RunTimings()
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

// wakeUpdateStamp is the stamp of a start request as it is kept (pending.go: actionStamp).
func (c *reqCtx) wakeUpdateStamp() string {
	var b wuStartBody
	if !decodeStrictly(c.body, &b) {
		return ""
	}
	return c.s.wuStamp(c.s.wuTimesNow(), b)
}
