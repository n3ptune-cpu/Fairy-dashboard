package web

import (
	"fmt"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
)

// A host that was renamed in config.yaml (audit 6, r4-x1-03). Its record stayed in machines.json under the old
// agent id, and after the restart the machine under its new id read "the same machine id as old-name, which
// keeps it": no button, the accept refused, every action refused, and old-name has no page. Now the old record
// is dropped at the start, with an entry in the audit log, and what the owner sees is "Not on record" with the
// button "Accept this machine's id".
func TestARenamedHostIsNotOnRecordAndCanBeAccepted(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	// what machines.json holds when the machine was called "old-name" in config.yaml at the time of the accept
	st := adminstate.New(a.s.Cfg.DataDir)
	recs, err := st.Machines()
	if err != nil || recs["laptop"].MachineID != machineIDOf("laptop") {
		t.Fatalf("machines.json: %v %+v", err, recs)
	}
	if err := st.SetMachines(map[string]approval.Record{"old-name": recs["laptop"]}, []string{"laptop"}); err != nil {
		t.Fatal(err)
	}
	s := a.restarted(t)
	b := s.Poller.BindingOf("laptop")
	if b.Status != approval.BindNoRecord || len(b.Held) != 0 || len(b.With) != 0 {
		t.Fatalf("laptop after the restart: %+v", b)
	}
	ag, _ := s.Poller.AgentByID("laptop")
	if m := s.machineFor(ag, nil); !m.CanAccept || m.Accept != "first" || len(m.HeldBy) != 0 || m.MachineID != "" || m.ShownID != machineIDOf("laptop") {
		t.Fatalf("its row on the approval page: %+v", m)
	}
	var dropped []string
	for _, e := range a.auditEntries("", "approval.record") {
		dropped = append(dropped, fmt.Sprint(e["target"], " | ", e["outcome"], " | ", e["params"]))
	}
	if len(dropped) != 1 || dropped[0] != "old-name | dropped: the panel has no host with this id any more | map[how:accepted on_record:"+approval.ShortID(machineIDOf("laptop"))+"]" {
		t.Fatalf("the audit log: %q", dropped)
	}
	if after, _ := st.Machines(); after["old-name"].MachineID != "" || after["old-name"].State != "" {
		t.Fatalf("machines.json still holds the old record: %+v", after)
	}
	if what, _, err := s.Poller.AcceptMachine("laptop", "test admin", poller.AcceptWant{MachineID: machineIDOf("laptop")}); what != "id" || err != nil {
		t.Fatalf("the accept: %q %v", what, err)
	}
	if b := s.Poller.BindingOf("laptop"); !b.OK() || b.Record.How != approval.PinAccepted {
		t.Fatalf("after the accept: %+v", b)
	}
	// one more restart: nothing more is dropped or said
	a.restarted(t)
	if n := len(a.auditEntries("", "approval.record")); n != 1 {
		t.Fatalf("%d entries about a dropped record after a second restart", n)
	}
}
