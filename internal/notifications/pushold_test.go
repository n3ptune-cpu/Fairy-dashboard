package notifications

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// An event whose text names a person the way an older version did (AddOld) goes out through PushOld when one
// is set, every other event through Push: whoever sends them on reads the two kinds of text differently.
// Without a Push there is no channel, and nothing goes out through either; without a PushOld every event goes
// through Push, as before. The line about too many events of one source is a text of now.
func TestOldEventsArePushedApart(t *testing.T) {
	var now, old []string
	s := Open(filepath.Join(t.TempDir(), "n.json"))
	s.Push = func(title, _ string) { now = append(now, title) }
	s.PushOld = func(title, _ string) { old = append(old, title) }
	s.Add(Event{Kind: "a", Severity: Warning, SystemID: "m1", Text: "written now"})
	s.AddOld(Event{Kind: "b", Severity: Warning, SystemID: "m1", Text: "stored by an older version"})
	s.Add(Event{Kind: "c", Severity: Info, SystemID: "m1", Text: "nothing to push"})
	s.AddOld(Event{Kind: "d", Severity: Info, SystemID: "m1", Text: "nothing to push either"})
	if !reflect.DeepEqual(now, []string{"Fairy: written now"}) || !reflect.DeepEqual(old, []string{"Fairy: stored by an older version"}) {
		t.Errorf("through Push: %q; through PushOld: %q", now, old)
	}
	// the notice that the next ones are not pushed is the panel's own text of now, whichever event it follows
	now, old = nil, nil
	for i := 0; i < 12; i++ {
		s.AddOld(Event{Kind: fmt.Sprint("flood-", i), Severity: Warning, SystemID: "m2", Text: "old"})
	}
	notices := 0
	for _, title := range now {
		if title == "Fairy: old" {
			t.Errorf("an old event went out through Push: %q", now)
		}
		notices++
	}
	if notices != 1 || len(old) == 0 || len(old) >= 12 {
		t.Errorf("12 old events of one source: %d through PushOld, through Push %q", len(old), now)
	}
	for _, title := range old {
		if title != "Fairy: old" {
			t.Errorf("through PushOld: %q", old)
		}
	}
	// no channel: nothing, also not through PushOld
	now, old = nil, nil
	s.Push = nil
	s.AddOld(Event{Kind: "e", Severity: Warning, SystemID: "m3", Text: "no channel"})
	if len(now)+len(old) != 0 {
		t.Errorf("without a Push: %q %q", now, old)
	}
	// no PushOld: everything through Push
	s.Push, s.PushOld = func(title, _ string) { now = append(now, title) }, nil
	s.AddOld(Event{Kind: "f", Severity: Warning, SystemID: "m4", Text: "one way out"})
	if !reflect.DeepEqual(now, []string{"Fairy: one way out"}) {
		t.Errorf("without a PushOld: %q", now)
	}
}
