package wakerun

import (
	"os"
	"strings"
	"testing"
)

// A run keeps two texts for who started it: By, the user name, which the page, the run's sentences and the
// machines get, and ByActor, what the audit log names that person by, for the entries the run writes later
// without a request. Both are in the journal, so the entries of a run that the panel's restart interrupts are
// still written under the right name.
func TestARunKeepsTheAuditTextOfWhoStartedIt(t *testing.T) {
	w := newWorld(t)
	w.off("pc-a")
	w.start(StartRequest{Hosts: []string{"pc-a"}, By: "Juniper", ByActor: "owner@example.com", BySub: "sub-n"})
	a, _ := w.m.Snapshot()
	if a == nil || a.By != "Juniper" || a.ByActor != "owner@example.com" || a.BySub != "sub-n" {
		t.Fatalf("the run: %+v", a)
	}
	raw, err := os.ReadFile(w.m.path)
	if err != nil || !strings.Contains(string(raw), `"by": "Juniper"`) || !strings.Contains(string(raw), `"by_actor": "owner@example.com"`) {
		t.Fatalf("the journal: %v %s", err, raw)
	}
	r := w.finish()
	if r.By != "Juniper" || r.ByActor != "owner@example.com" {
		t.Errorf("the finished run: by %q, for the audit log %q", r.By, r.ByActor)
	}
	// what the machine was told is the user name
	for _, h := range r.Hosts {
		for _, l := range h.Log {
			if strings.Contains(l.Text, "@") {
				t.Errorf("a line of the run names an address: %s", l.Text)
			}
		}
	}
	// a run started without it (an older version's request) has none: its By is used
	w2 := newWorld(t)
	w2.off("pc-a")
	w2.start(StartRequest{Hosts: []string{"pc-a"}, By: "Ann", BySub: "sub-ann"})
	if a, _ := w2.m.Snapshot(); a == nil || a.ByActor != "" {
		t.Errorf("a run without the audit text: %+v", a)
	}
}
