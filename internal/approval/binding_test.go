package approval

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
)

const (
	idA = "machine-id-A-0123456789ab"
	idB = "machine-id-B-0123456789ab"
	idC = "machine-id-C-0123456789ab"
)

var t0 = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func pin(id string) Record {
	return Record{MachineID: id, How: PinTurnOn, At: t0, By: "nora", State: api.ApprovalRequired, StateAt: t0}
}

// keeps is the record of a host that an admin said keeps the id.
func keeps(id string) Record {
	r := pin(id)
	r.Holder = true
	return r
}

// shows is the answer of a machine that requires approvals and shows the id, asked after t0.
func shows(id string) Report {
	return Report{Heard: true, Block: true, State: api.ApprovalRequired, MachineID: id, Proto: api.ApprovalProto, Asked: t0.Add(time.Minute)}
}

func off() Report {
	return Report{Heard: true, Block: true, State: api.ApprovalOff, Proto: api.ApprovalProto, Asked: t0.Add(time.Minute)}
}

func noBlock() Report { return Report{Heard: true, Asked: t0.Add(time.Minute)} }

// unaccepted is the record of a host that showed the check on, with an id nobody accepted: nothing is on record.
func unaccepted() Record {
	return Record{State: api.ApprovalRequired, StateAt: t0, Unaccepted: true}
}

// The decision about which machine a host is (audit 5, r1-x1-01): every row is one state of the panel's record
// and of what the hosts show, and what must come of it for the host "b".
func TestEvaluate(t *testing.T) {
	holder := pin(idA)
	holder.Holder = true
	for name, c := range map[string]struct {
		recs   map[string]Record
		reps   map[string]Report
		want   string
		with   []string
		checks bool
	}{
		"nothing known":                       {nil, nil, BindOK, nil, false},
		"an agent without the block":          {nil, map[string]Report{"b": noBlock()}, BindOK, nil, false},
		"the check is off, nothing on record": {nil, map[string]Report{"b": off()}, BindOK, nil, false},
		"on record and shown, the same id":    {map[string]Record{"b": pin(idB)}, map[string]Report{"b": shows(idB)}, BindOK, nil, true},
		"on record, silent since the start":   {map[string]Record{"b": pin(idB)}, nil, BindOK, nil, true},
		// not on record (audit 5, round 3, r3-x1-01): the check is on and no id was issued by the panel or
		// accepted by an admin. Nothing the host shows changes that; and once it was seen so, "no check" is an alarm
		"required, nothing on record":                           {nil, map[string]Report{"b": shows(idB)}, BindNoRecord, nil, true},
		"required, seen before, nothing on record":              {map[string]Record{"b": unaccepted()}, map[string]Report{"b": shows(idB)}, BindNoRecord, nil, true},
		"seen with the check, silent since the start":           {map[string]Record{"b": unaccepted()}, nil, BindOK, nil, true},
		"seen with the check, said off, silent since a restart": {map[string]Record{"b": {State: api.ApprovalOff, StateAt: t0, Unaccepted: true}}, nil, BindOK, nil, true},
		"seen with the check, and now the block is gone":        {map[string]Record{"b": unaccepted()}, map[string]Report{"b": noBlock()}, BindVanished, nil, true},
		"seen with the check, and now it says off":              {map[string]Record{"b": unaccepted()}, map[string]Report{"b": off()}, BindVanished, nil, true},
		"seen with the check, broken now":                       {map[string]Record{"b": unaccepted()}, map[string]Report{"b": {Heard: true, Block: true, State: api.ApprovalBroken, Proto: 2, Asked: t0.Add(time.Minute)}}, BindOK, nil, true},
		"an id from an earlier build's first sight, demoted": {map[string]Record{"b": Record{MachineID: idB, How: PinFirstSight, At: t0, By: "Fairy", State: api.ApprovalRequired, StateAt: t0}.Demote()},
			map[string]Report{"b": shows(idB)}, BindNoRecord, nil, true},
		"shows another id than the one on record": {map[string]Record{"b": pin(idB)}, map[string]Report{"b": shows(idC)}, BindChanged, nil, true},
		"the block is gone":                       {map[string]Record{"b": pin(idB)}, map[string]Report{"b": noBlock()}, BindVanished, nil, true},
		"it says off":                             {map[string]Record{"b": pin(idB)}, map[string]Report{"b": off()}, BindVanished, nil, true},
		"it said off BEFORE the id came on record (right after Turn on)": {map[string]Record{"b": pin(idB)},
			map[string]Report{"b": {Heard: true, Block: true, State: api.ApprovalOff, Proto: 2, Asked: t0.Add(-time.Second)}}, BindOK, nil, true},
		"scheme 1 (the test build of 2026-11-11)": {nil, map[string]Report{"b": {Heard: true, Block: true, State: api.ApprovalRequired, Proto: 1, Asked: t0}}, BindTooOld, nil, true},
		"scheme 2 without an id":                  {nil, map[string]Report{"b": {Heard: true, Block: true, State: api.ApprovalRequired, Proto: 2, Asked: t0}}, BindTooOld, nil, true},
		"on record, and now an old agent":         {map[string]Record{"b": pin(idB)}, map[string]Report{"b": {Heard: true, Block: true, State: api.ApprovalRequired, Proto: 1, Asked: t0.Add(time.Minute)}}, BindTooOld, nil, true},
		"broken trust file, id on record":         {map[string]Record{"b": pin(idB)}, map[string]Report{"b": {Heard: true, Block: true, State: api.ApprovalBroken, Proto: 2, Asked: t0.Add(time.Minute)}}, BindOK, nil, true},
		// one id, two hosts: in every combination of "on record" and "shown"
		"two records with one id": {map[string]Record{"a": pin(idA), "b": pin(idA)}, nil, BindDuplicate, []string{"a"}, true},
		"a record and another host's shown id": {map[string]Record{"b": pin(idB)},
			map[string]Report{"a": shows(idB), "b": shows(idB)}, BindDuplicate, []string{"a"}, true},
		"a shown id and another host's record": {map[string]Record{"a": pin(idA)},
			map[string]Report{"a": shows(idA), "b": shows(idA)}, BindDuplicate, []string{"a"}, true},
		// two hosts without a record that show one id: both are "not on record" (the first accept decides)
		"two shown ids, nothing on record": {nil, map[string]Report{"a": shows(idA), "b": shows(idA)}, BindNoRecord, []string{"a"}, true},
		"two shown ids, both seen before":  {map[string]Record{"a": unaccepted(), "b": unaccepted()}, map[string]Report{"a": shows(idA), "b": shows(idA)}, BindNoRecord, []string{"a"}, true},
		"three hosts": {map[string]Record{"a": pin(idB), "b": pin(idB), "c": pin(idC)},
			map[string]Report{"c": shows(idB)}, BindDuplicate, []string{"a", "c"}, true},
		"the laundered Turn on: b was switched on, a now shows b's id": {map[string]Record{"b": pin(idB)},
			map[string]Report{"a": shows(idB), "b": shows(idB)}, BindDuplicate, []string{"a"}, true},
		"a host that shows this host's id while this host is silent": {map[string]Record{"b": pin(idB)},
			map[string]Report{"a": shows(idB)}, BindDuplicate, []string{"a"}, true},
		// the admin's word
		"the other host keeps the id":       {map[string]Record{"a": holder, "b": pin(idA)}, map[string]Report{"a": shows(idA), "b": shows(idA)}, BindDuplicate, []string{"a"}, true},
		"this host keeps the id":            {map[string]Record{"b": holder, "a": pin(idA)}, map[string]Report{"a": shows(idA), "b": shows(idA)}, BindOK, nil, true},
		"both were said to keep it":         {map[string]Record{"a": holder, "b": holder}, nil, BindDuplicate, []string{"a"}, true},
		"it keeps one id and shows another": {map[string]Record{"b": holder, "a": pin(idA)}, map[string]Report{"b": shows(idC)}, BindChanged, nil, true},
		// (G-01) the other host keeps an id too, but ANOTHER one, and shows this host's: the admin's word for this
		// host counts; and the other way round, the host that shows a kept id that is not its own is flagged
		"this host keeps its id, the other keeps another id and shows this one": {map[string]Record{"b": keeps(idB), "a": holder},
			map[string]Report{"a": shows(idB), "b": shows(idB)}, BindOK, nil, true},
		"this host keeps one id and shows another host's kept id": {map[string]Record{"b": keeps(idB), "a": holder},
			map[string]Report{"a": shows(idA), "b": shows(idA)}, BindDuplicate, []string{"a"}, true},
		// a host that shows a new id of its own is "changed", also while its old id is still another host's
		"new id of its own, the old one is shared": {map[string]Record{"a": pin(idA), "b": pin(idA)}, map[string]Report{"b": shows(idC)}, BindChanged, nil, true},
		"new id that is another host's":            {map[string]Record{"a": pin(idA), "b": pin(idB)}, map[string]Report{"b": shows(idA)}, BindDuplicate, []string{"a"}, true},
	} {
		got := Evaluate("b", c.recs, c.reps)
		if got.Status != c.want || !reflect.DeepEqual(got.With, c.with) || got.Checks() != c.checks || got.OK() != (c.want == BindOK) {
			t.Errorf("%s: status %q with %v checks %v, want %q with %v checks %v", name, got.Status, got.With, got.Checks(), c.want, c.with, c.checks)
		}
	}
	// the other side of the same picture: when b is flagged for sharing a's id, a is flagged too (unless a
	// keeps the id by the admin's word)
	recs := map[string]Record{"a": pin(idA), "b": pin(idB)}
	reps := map[string]Report{"a": shows(idA), "b": shows(idA)}
	if a, b := Evaluate("a", recs, reps), Evaluate("b", recs, reps); a.Status != BindDuplicate || b.Status != BindDuplicate {
		t.Errorf("one id on two hosts must flag both: a %q, b %q", a.Status, b.Status)
	}
	recs["a"] = holder
	if a, b := Evaluate("a", recs, reps), Evaluate("b", recs, reps); a.Status != BindOK || b.Status != BindDuplicate {
		t.Errorf("after the admin's word a is in order and b stays flagged: a %q, b %q", a.Status, b.Status)
	}
	reps["b"] = shows(idB) // b shows an id of its own again
	if a, b := Evaluate("a", recs, reps), Evaluate("b", recs, reps); a.Status != BindOK || b.Status != BindOK {
		t.Errorf("b shows its own id again: a %q, b %q", a.Status, b.Status)
	}
}

// An id that an earlier build took on record at first sight is not on record (audit 5, round 3, r3-x1-01):
// the record loses the id and everything about how it came, and keeps that the host showed the check. A record
// from Turn on or from an admin's accept is left as it is.
func TestDemote(t *testing.T) {
	old := Record{MachineID: idB, How: PinFirstSight, At: t0, By: "Fairy", State: api.ApprovalRequired, StateAt: t0, Holder: true, Alarm: BindDuplicate}
	want := Record{State: api.ApprovalRequired, StateAt: t0, Unaccepted: true, Alarm: BindDuplicate}
	if got := old.Demote(); got != want {
		t.Errorf("a first-sight record: %+v, want %+v", got, want)
	}
	for _, how := range []string{PinTurnOn, PinAccepted} {
		r := pin(idB)
		r.How, r.Holder = how, true
		if got := r.Demote(); got != r {
			t.Errorf("%s: changed to %+v", how, got)
		}
	}
	if got := unaccepted().Demote(); got != unaccepted() {
		t.Errorf("a record without an id: %+v", got)
	}
}

// A machine id the panel makes up is one a machine takes, and no two are the same.
func TestNewMachineID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, err := NewMachineID(nil)
		if err != nil || api.CheckMachineID(id) != nil || len(id) != 22 || seen[id] {
			t.Fatalf("id %q: %v", id, err)
		}
		seen[id] = true
	}
	if id, err := NewMachineID(bytes.NewReader(make([]byte, 16))); err != nil || id != strings.Repeat("A", 22) {
		t.Fatalf("16 zero bytes: %q %v", id, err)
	}
	if _, err := NewMachineID(bytes.NewReader(make([]byte, 5))); err == nil {
		t.Fatal("a random source that runs dry must be an error, not a short id")
	}
	if ShortID(idA) == idA || !strings.HasPrefix(idA, strings.TrimSuffix(ShortID(idA), "…")) {
		t.Fatal("the short form:", ShortID(idA))
	}
}

// A machine id in the form to compare by eye (second review of passkey-g, G-02): the first eight and the last
// six characters, as the Approval box shows it; never the id in full; the audit log's form stays the first six.
func TestCompareID(t *testing.T) {
	const id = "AbCdEfGh-0123456789_uVwXyZ"
	if got := CompareID(id); got != "AbCdEfGh…uVwXyZ" {
		t.Errorf("CompareID: %q", got)
	}
	if got := ShortID(id); got != "AbCdEf…" {
		t.Errorf("ShortID: %q", got)
	}
	for n := api.MinMachineID; n <= api.MaxMachineID; n++ { // every length a machine id can have
		id := strings.Repeat("x", n)
		if got := CompareID(id); got == id || len([]rune(got)) != 15 {
			t.Errorf("an id of %d characters: %q", n, got)
		}
	}
	if CompareID("short") != "short" || CompareID("") != "" {
		t.Error("a text that is no machine id is left as it is")
	}
}

// Every alarm has one sentence for people, and it names the host.
func TestBindingText(t *testing.T) {
	for _, st := range []string{BindChanged, BindDuplicate, BindVanished, BindTooOld, BindNoRecord} {
		if txt := BindingText("PC-B", st, []string{"PC-A"}); !strings.Contains(txt, "PC-B") || len(txt) > 260 || strings.Contains(txt, idA) {
			t.Errorf("%s: %q", st, txt)
		}
	}
	if txt := BindingText("PC-B", BindDuplicate, []string{"PC-A", "PC-C"}); !strings.Contains(txt, "PC-A and PC-C") {
		t.Error(txt)
	}
	if BindingText("PC-B", BindOK, nil) != "" || BindingText("PC-B", BindNoStore, nil) == "" {
		t.Error("in order: no text; an unreadable record: a text")
	}
}

// One id is accepted for one host only (audit 5, r2-b2-02): which host keeps an id that two hosts share, so
// that the other one cannot be accepted for it.
func TestHeld(t *testing.T) {
	holder := pin(idA)
	holder.Holder = true
	for name, c := range map[string]struct {
		recs         map[string]Record
		reps         map[string]Report
		heldA, heldB string
		statusB      string
	}{
		"b shows a's id, a has it on record":      {map[string]Record{"a": pin(idA), "b": pin(idB)}, map[string]Report{"a": shows(idA), "b": shows(idA)}, "[]", "[a]", BindDuplicate},
		"b shows a's id and has no record":        {map[string]Record{"a": pin(idA)}, map[string]Report{"a": shows(idA), "b": shows(idA)}, "[]", "[a]", BindDuplicate},
		"both have it on record: on a par":        {map[string]Record{"a": pin(idA), "b": pin(idA)}, map[string]Report{"a": shows(idA), "b": shows(idA)}, "[]", "[]", BindDuplicate},
		"both on record, a by an admin's word":    {map[string]Record{"a": holder, "b": pin(idA)}, map[string]Report{"a": shows(idA), "b": shows(idA)}, "[]", "[a]", BindDuplicate},
		"the same, and b says off now":            {map[string]Record{"a": holder, "b": pin(idA)}, map[string]Report{"a": shows(idA), "b": off()}, "[]", "[a]", BindDuplicate},
		"b went on to an id of its own":           {map[string]Record{"a": holder, "b": pin(idA)}, map[string]Report{"a": shows(idA), "b": shows(idC)}, "[]", "[]", BindChanged},
		"both show a new id nobody has on record": {map[string]Record{"a": pin(idA), "b": pin(idB)}, map[string]Report{"a": shows(idC), "b": shows(idC)}, "[]", "[]", BindDuplicate},
		// audit 5, round 3: a host with no record never gets an id that is on record for another host, however
		// that host came by it; two hosts with no record are both "not on record" and neither keeps anything
		"b was seen before and shows a's id":     {map[string]Record{"a": pin(idA), "b": unaccepted()}, map[string]Report{"a": shows(idA), "b": shows(idA)}, "[]", "[a]", BindDuplicate},
		"b shows a's id while a shows a new one": {map[string]Record{"a": pin(idA)}, map[string]Report{"a": shows(idC), "b": shows(idA)}, "[]", "[a]", BindDuplicate},
		"neither has a record":                   {nil, map[string]Report{"a": shows(idA), "b": shows(idA)}, "[]", "[]", BindNoRecord},
		// (G-01) a keeps idA and b keeps idB; b shows a's id: a keeps it, b cannot be accepted for it
		"b keeps another id and shows a's kept id": {map[string]Record{"a": holder, "b": keeps(idB)}, map[string]Report{"a": shows(idA), "b": shows(idA)}, "[]", "[a]", BindDuplicate},
	} {
		a, b := Evaluate("a", c.recs, c.reps), Evaluate("b", c.recs, c.reps)
		if got := []string{strings.Join(append([]string{}, a.Held...), ","), strings.Join(append([]string{}, b.Held...), ",")}; "["+got[0]+"]" != c.heldA || "["+got[1]+"]" != c.heldB || b.Status != c.statusB {
			t.Errorf("%s: a held by %v, b held by %v (%s); want %s, %s (%s)", name, a.Held, b.Held, b.Status, c.heldA, c.heldB, c.statusB)
		}
		if len(a.Held) > 0 && len(b.Held) > 0 {
			t.Errorf("%s: each of the two is said to keep the id", name)
		}
	}
	if txt := HeldText("PC-B", []string{"PC-A"}); !strings.Contains(txt, "PC-B has the same machine id as PC-A, which keeps it") || !strings.Contains(txt, "helper-trust set --new-machine-id") {
		t.Error(txt)
	}
}
