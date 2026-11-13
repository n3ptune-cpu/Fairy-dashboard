package web

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// A host that was renamed in config.yaml (audit 6, r4-x1-03). Its record stayed in machines.json under the old
// agent id, and after the restart the machine under its new id read "the same machine id as old-name, which
// keeps it": no button, the accept refused, every action refused, and old-name has no page. Now the old record
// does not count from the start on, and what the owner sees is "Not on record" with the button "Accept this
// machine's id". (The old record is set aside, not deleted: review of passkey-k, RK-01. It was deleted at the
// start, with an entry in the audit log, also for a host that was only missing at that one start.)
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
	if len(dropped) != 0 { // nothing was deleted, so the audit log has nothing about a deleted record
		t.Fatalf("the audit log: %q", dropped)
	}
	if after, _ := st.Machines(); after["old-name"].MachineID != machineIDOf("laptop") {
		t.Fatalf("machines.json lost the record that was set aside: %+v", after)
	}
	if what, _, err := s.Poller.AcceptMachine("laptop", "test admin", poller.AcceptWant{MachineID: machineIDOf("laptop")}); what != "id" || err != nil {
		t.Fatalf("the accept: %q %v", what, err)
	}
	if b := s.Poller.BindingOf("laptop"); !b.OK() || b.Record.How != approval.PinAccepted {
		t.Fatalf("after the accept: %+v", b)
	}
	// one more restart: nothing is dropped or said, and the accepted id stays laptop's (the record that is set
	// aside keeps no id: laptop is not flagged as one of two hosts with one id)
	s = a.restarted(t)
	if n := len(a.auditEntries("", "approval.record")); n != 0 {
		t.Fatalf("%d entries about a dropped record after a second restart", n)
	}
	if b := s.Poller.BindingOf("laptop"); !b.OK() || len(b.With) != 0 || len(b.Held) != 0 {
		t.Fatalf("laptop after a second restart: %+v", b)
	}
}

// RK-01 (review of passkey-k): a host added in the panel whose tokens file cannot be read at one start is
// skipped at that start (managed.Merge: "Re-pair it"), so it is not in the configuration when the record is
// read. Its record was deleted from machines.json then, and after the file was repaired the machine was "Not
// on record" for good. The record is only set aside for that start: with the file restored the host has it
// again, as "turn-on" by the admin of then.
func TestAnAddedHostThatWasSkippedAtOneStartKeepsItsRecord(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	st := managed.NewStore(cfg.DataDir)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	h := managed.Host{ID: "nas", Name: "NAS", URL: "http://nas:9101", OS: "linux", AddedAt: time.Now().UTC()}
	if err := st.Put(h, managed.Secret{Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	const nasID = "bmFzLW1hY2hpbmUtaWQtMDAwMQ"
	at := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	ad := adminstate.New(cfg.DataDir)
	if err := ad.SetMachines(map[string]approval.Record{
		"nas":   {MachineID: nasID, How: approval.PinTurnOn, At: at, By: "nora", State: api.ApprovalRequired, StateAt: at},
		"tower": {MachineID: "dG93ZXItbWFjaGluZS1pZC0wMQ", How: approval.PinAccepted, At: at, By: "nora", State: api.ApprovalRequired, StateAt: at},
	}, nil); err != nil {
		t.Fatal(err)
	}
	start := func() (*poller.Poller, []string) {
		c := *cfg
		_, skipped, err := st.Merge(&c)
		if err != nil {
			t.Fatal(err)
		}
		p, err := poller.New(&c, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, testutil.NewBackend())
		if err != nil {
			t.Fatal(err)
		}
		p.MachinesLoaded()
		return p, skipped
	}
	onFile := func() approval.Record {
		recs, err := ad.Machines()
		if err != nil {
			t.Fatal(err)
		}
		return recs["nas"]
	}
	// start 1: everything can be read
	p, skipped := start()
	if b := p.BindingOf("nas"); len(skipped) != 0 || !b.Known || b.Record.MachineID != nasID {
		t.Fatalf("start 1: skipped %v, nas %+v", skipped, b)
	}
	// start 2: the host's tokens file is damaged (a half-written restore): the host is skipped
	sec := filepath.Join(cfg.DataDir, "secrets", "agent-nas.json")
	good, err := os.ReadFile(sec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sec, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, skipped = start()
	if len(skipped) != 1 || !strings.Contains(skipped[0], "nas") {
		t.Fatalf("start 2 skipped: %v", skipped)
	}
	if b := p.BindingOf("nas"); b.Known {
		t.Fatalf("start 2: the record of a host the panel does not have counts: %+v", b)
	}
	if r := onFile(); r.MachineID != nasID || r.How != approval.PinTurnOn || r.By != "nora" {
		t.Fatalf("start 2 took the record out of machines.json: %+v", r)
	}
	// start 3: the tokens file is restored
	if err := os.WriteFile(sec, good, 0o600); err != nil {
		t.Fatal(err)
	}
	p, skipped = start()
	if b := p.BindingOf("nas"); len(skipped) != 0 || !b.Known || b.Record.MachineID != nasID || b.Record.How != approval.PinTurnOn {
		t.Fatalf("start 3: skipped %v, nas %+v: one start with an unreadable tokens file lost the record of a host that is still in the list of added hosts", skipped, b)
	}
}
