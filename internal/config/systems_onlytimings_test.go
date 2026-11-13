package config

import "testing"

// OnlyWakeUpdate: the one kind of hand-written entry that can stand next to an entry "Add host" made.
func TestAnEntryWithNothingButItsTimings(t *testing.T) {
	w := &SystemWakeUpdate{OffAfterS: 360}
	for _, c := range []struct {
		o    SystemOverride
		want bool
	}{
		{SystemOverride{ID: "pc-a", WakeUpdate: w}, true},
		{SystemOverride{ID: "pc-a", WakeUpdate: &SystemWakeUpdate{}}, true},
		{SystemOverride{ID: "pc-a"}, false},
		{SystemOverride{ID: "pc-a", WakeUpdate: w, Name: "PC A"}, false},
		{SystemOverride{ID: "pc-a", WakeUpdate: w, Wol: &Wol{}}, false},
		{SystemOverride{ID: "pc-a", WakeUpdate: w, BMCPower: &BMCPower{}}, false},
		{SystemOverride{ID: "pc-a", WakeUpdate: w, DWServiceURL: "https://x.example"}, false},
		{SystemOverride{ID: "pc-a", WakeUpdate: w, Labels: Labels{Site: "home"}}, false},
	} {
		if got := c.o.OnlyWakeUpdate(); got != c.want {
			t.Errorf("%+v: %v, want %v", c.o, got, c.want)
		}
	}
}
