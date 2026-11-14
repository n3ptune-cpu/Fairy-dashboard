package web

import (
	"slices"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// The store's reader leaves a number out that its key cannot have, so the checks behind it are never reached
// through a file. Each of them is asked here by itself, with decisions handed over directly: whatever the
// reader is changed to one day, a number below a floor is not in force, not in the dialog's data, and not the
// "was" of a change.

// hostileTimings is decisions no reader hands out: every key below its range, and above it.
func hostileTimings(low bool) adminstate.RunTimings {
	var o adminstate.RunTimings
	for _, tm := range config.RunTimings() {
		if low {
			o.Set(tm.Key, tm.Min-1)
		} else {
			o.Set(tm.Key, tm.Max+1)
		}
	}
	return o
}

func TestTheTimingsInForceTakeNoNumberOutsideItsRange(t *testing.T) {
	e := setup(t)
	plain := e.s.wuTimesWith(adminstate.RunTimings{})
	for _, low := range []bool{true, false} {
		got := e.s.wuTimesWith(hostileTimings(low))
		for _, tm := range config.RunTimings() {
			if got.value(tm.Key) != plain.value(tm.Key) || got.value(tm.Key) != tm.Default {
				t.Errorf("low %v: %s in force is %d, config.yaml's is %d", low, tm.Key, got.value(tm.Key), plain.value(tm.Key))
			}
		}
		if got.ticket() != plain.ticket() || got.shown() != plain.shown() {
			t.Errorf("low %v: the ticket numbers or the page's stamp follow a number outside its range", low)
		}
	}
	// one good decision next to the bad ones counts
	o := hostileTimings(true)
	o.Set(config.KeySettle, 90)
	if got := e.s.wuTimesWith(o); got.value(config.KeySettle) != 90 || got.value(config.KeyOffAfter) != 900 || got.value(config.KeyShutdownDelay) != 120 {
		t.Errorf("a good decision next to bad ones: %+v", got)
	}
}

func TestTheDialogsDataTakesNoNumberOutsideItsRange(t *testing.T) {
	e := setup(t)
	for _, low := range []bool{true, false} {
		view := e.s.runTimingsView(hostileTimings(low), nil)
		rows, _ := view["timings"].([]map[string]any)
		if len(rows) != len(config.RunTimings()) {
			t.Fatalf("the rows: %v", view["timings"])
		}
		for i, tm := range config.RunTimings() {
			if rows[i]["value"] != tm.Default || rows[i]["from"] != "config" || rows[i]["by"] != nil {
				t.Errorf("low %v: the dialog's row of %s: %v", low, tm.Key, rows[i])
			}
		}
	}
}

func TestAChangesWasIsNeverANumberOutsideItsRange(t *testing.T) {
	e := setup(t)
	six, ten := 360, 600
	for _, low := range []bool{true, false} {
		// the value config.yaml has, sent over a stored number its key cannot have: nothing in force changes,
		// so there is no change, no line and nothing to confirm
		o := hostileTimings(low)
		cfg := 900
		if changes, lines, own := e.s.rtPlan(&o, runTimingsReq{OffAfterS: &cfg}); len(changes) != 0 || len(lines) != 0 || own {
			t.Errorf("low %v: config.yaml's value over a number outside the range: %v %q %v", low, changes, lines, own)
		}
		// a real change: its "was" is config.yaml's value, the one that was in force
		o = hostileTimings(low)
		changes, lines, own := e.s.rtPlan(&o, runTimingsReq{OffAfterS: &six, ShutdownDelayS: &ten})
		if len(changes) != 2 || changes[0].Before != 900 || changes[0].BeforeFrom != "config.yaml" || changes[1].Before != 120 || !own ||
			!slices.Contains(lines, "Counts as off after: 360 s = 6 minutes (was 900 s = 15 minutes)") || !slices.Contains(lines, "Shutdown countdown: 600 s = 10 minutes (was 120 s = 2 minutes)") {
			t.Errorf("low %v: a change over numbers outside the range: %+v %q", low, changes, lines)
		}
		// (a countdown that gets longer than config.yaml's 120 s: no sentence; with the stored 9 s or 3601 s
		// taken for the value in force it would be another line, and for 3601 s a shorter countdown)
		if strings.Contains(strings.Join(lines, "\n"), "to cancel the run's shutdown") {
			t.Errorf("low %v: the countdown's sentence under a longer countdown: %q", low, lines)
		}
	}
}

// The numbers a ticket was signed with are compared with the numbers of the run that is about to start, by
// themselves (the stamp of a kept start would refuse first; this is the check behind it).
func TestAnApprovedStartIsHeldToTheNumbersItsTicketsWereSignedWith(t *testing.T) {
	e := setup(t)
	b := wuStartBody{Hosts: []string{"pc-a"}}
	signed := e.s.wuTimesWith(adminstate.RunTimings{}).ticket()
	grant := func() *approvalGrant {
		return &approvalGrant{plan: &approvalPlan{run: &runStart{id: "0123456789abcdef", list: wakerun.ListHash(b.Hosts, b.UpdateOnly, b.UpdateIfOn), numbers: signed,
			arm: map[string]api.ApprovalRequest{}, update: map[string]api.ApprovalRequest{}}}}
	}
	if rs, err := e.s.wuStartApproved(grant(), b, map[string]wuHost{"pc-a": {ID: "pc-a", Name: "PC-A"}}, signed); err != nil || rs == nil {
		t.Fatalf("the same numbers: %v", err)
	}
	for what, change := range map[string]func(*wakerun.Ticket){
		"wait_s":       func(n *wakerun.Ticket) { n.WaitS += 60 },
		"within_s":     func(n *wakerun.Ticket) { n.WithinS++ },
		"confirm wait": func(n *wakerun.Ticket) { n.ConfirmWaitS += 300 },
		"notice_s":     func(n *wakerun.Ticket) { n.NoticeS-- },
		"valid_s":      func(n *wakerun.Ticket) { n.ValidS++ },
	} {
		now := signed
		change(&now)
		if rs, err := e.s.wuStartApproved(grant(), b, map[string]wuHost{"pc-a": {ID: "pc-a", Name: "PC-A"}}, now); rs != nil || err == nil || !strings.Contains(err.Error(), wuTimingsChanged) {
			t.Errorf("%s differs from what was signed: %v %v", what, rs, err)
		}
	}
}

// A machine's row on the page says its own countdown where it is not the PANEL'S IN FORCE (not where it is
// not config.yaml's): with the panel's countdown changed to 30 s, a machine whose own is config.yaml's 120 s
// has it in its row, and one whose own is 30 s has none.
func TestAMachinesRowComparesItsOwnTimingsWithThePanelsInForce(t *testing.T) {
	a := signedIn(t)
	e := a.e
	newFarm(t, e)
	e.s.Poller.Override("pc-a").WakeUpdate = &config.SystemWakeUpdate{OffAfterS: 900, ShutdownDelayS: 120} // config.yaml's own defaults, written out for pc-a
	e.s.Poller.Override("pc-b").WakeUpdate = &config.SystemWakeUpdate{OffAfterS: 360, ShutdownDelayS: 30}
	e.rtSet(map[string]int{config.KeyOffAfter: 360, config.KeyShutdownDelay: 30})
	page := e.do("GET", "/api/admin/wake-update", nil, "Cookie", a.sess)
	if pa := wuHostRow(t, page, "pc-a"); pa["off_after_s"] != 900.0 || pa["shutdown_delay_s"] != 120.0 {
		t.Errorf("pc-a keeps 900 s and 120 s while the panel has 360 s and 30 s; its row: %v", pa)
	}
	if pb := wuHostRow(t, page, "pc-b"); pb["off_after_s"] != nil || pb["shutdown_delay_s"] != nil {
		t.Errorf("pc-b's own are the panel's in force; its row: %v", pb)
	}
}

// "Edit host": the dialog's data takes no stored number its key cannot have for a machine's own timing.
func TestEditHostsDataTakesNoNumberOutsideItsRange(t *testing.T) {
	h := &hoHost{run: map[string]hoRunBase{config.KeyOffAfter: {config: 600, panel: 900}, config.KeyShutdownDelay: {panel: 120}}}
	for _, e := range []adminstate.RunEdit{{OffAfterS: 60, ShutdownDelayS: 5}, {OffAfterS: 3601, ShutdownDelayS: 3601}, {OffAfterS: -1, ShutdownDelayS: -1}} {
		if v, from := h.runValue(e, config.KeyOffAfter); v != 600 || from != "config" {
			t.Errorf("%+v: off_after_s %d from %s", e, v, from)
		}
		if v, from := h.runValue(e, config.KeyShutdownDelay); v != 120 || from != "run" {
			t.Errorf("%+v: shutdown_delay_s %d from %s", e, v, from)
		}
	}
	if v, from := h.runValue(adminstate.RunEdit{OffAfterS: 300}, config.KeyOffAfter); v != 300 || from != "panel" {
		t.Errorf("the floor itself: %d from %s", v, from)
	}
}
