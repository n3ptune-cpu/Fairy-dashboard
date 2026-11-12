package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// restarted is the panel started again on the same folders, with the same machines behind it: a new poller
// and server that read machines.json and write to the same audit log. It asks every machine twice.
func (a *approvalEnv) restarted(t *testing.T) *Server {
	t.Helper()
	p, err := poller.New(a.s.Cfg, a.s.Secrets, a.gb)
	if err != nil {
		t.Fatal(err)
	}
	s := New(a.s.Cfg, a.s.Secrets, p, nil, "test")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	p.PollOnce(context.Background())
	p.PollOnce(context.Background())
	return s
}

// An alarm is written to the audit log when it changes, not at every start of the panel (audit 5, r2-b2-09:
// 17 lines for 5 changes). What was written last is kept with the host's record.
func TestAStandingAlarmIsWrittenOnceAcrossRestarts(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop, a.tower)
	lines := func() []string {
		var out []string
		for _, e := range a.auditEntries("", "approval.alarm") {
			out = append(out, fmt.Sprint(e["target"], " ", e["outcome"]))
		}
		return out
	}
	want := func(when string, s *Server, status string, n int, last string) {
		t.Helper()
		got := lines()
		if len(got) != n || (n > 0 && got[0] != last) { // the newest entry is the first
			t.Fatalf("%s: the alarms in the audit log: %v (want %d, the last %q)", when, got, n, last)
		}
		if b := s.Poller.BindingOf("laptop"); b.Status != status {
			t.Fatalf("%s: laptop: %+v", when, b)
		}
		recs, err := adminstate.New(a.s.Cfg.DataDir).Machines()
		if wantAlarm := status; err != nil || recs["laptop"].Alarm != wantAlarm {
			t.Fatalf("%s: machines.json: %v, alarm %q", when, err, recs["laptop"].Alarm)
		}
	}
	a.setTrustID(t, a.laptop, otherID)
	a.poll()
	a.poll()
	want("the alarm", a.s, approval.BindChanged, 1, "laptop alarm: changed")
	// two restarts while it holds: it still holds, and nothing is written
	s := a.restarted(t)
	want("after a restart", s, approval.BindChanged, 1, "laptop alarm: changed")
	s = a.restarted(t)
	want("after a second restart", s, approval.BindChanged, 1, "laptop alarm: changed")
	if st, _ := s.Poller.BindingOf("tower"), 0; !st.OK() {
		t.Fatalf("tower: %+v", st)
	}
	// it becomes another alarm: written, once
	a.clearTrust(t, a.laptop)
	s.Poller.PollOnce(context.Background())
	want("another alarm", s, approval.BindVanished, 2, "laptop alarm: vanished")
	s = a.restarted(t)
	want("after a restart with the other alarm", s, approval.BindVanished, 2, "laptop alarm: vanished")
	// it is put right on the machine while the panel is down: the end is written, once
	a.setTrustID(t, a.laptop, machineIDOf("laptop"))
	s = a.restarted(t)
	want("put right while the panel was down", s, approval.BindOK, 3, "laptop cleared")
	s = a.restarted(t)
	want("and a restart later", s, approval.BindOK, 3, "laptop cleared")
	// one id on two hosts: one alarm per host; a restart, in which the two are heard one after the other, writes none
	a.setTrustID(t, a.tower, machineIDOf("laptop"))
	s.Poller.PollOnce(context.Background())
	if got := lines(); len(got) != 5 {
		t.Fatalf("one id on two hosts: %v", got)
	}
	for i := 0; i < 3; i++ {
		s = a.restarted(t)
		if got := lines(); len(got) != 5 || s.Poller.BindingOf("tower").Status != approval.BindDuplicate || s.Poller.BindingOf("laptop").Status != approval.BindDuplicate {
			t.Fatalf("restart %d with the pair: %v", i+1, got)
		}
	}
}
