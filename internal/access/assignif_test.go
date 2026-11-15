package access

import "testing"

// AssignIf takes back an assignment only while it is still the one the caller stored: what somebody else
// stored since is left, and the caller is told what it is.
func TestAssignIfWritesOnlyOverTheCallersOwnAssignment(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, n := range []string{"Alice", "Bob"} {
		if err := s.CreateSite(n); err != nil {
			t.Fatal(err)
		}
	}
	stored := func() string { return s.Load().Assign["pc-a"] }
	for _, c := range []struct {
		what                string
		now, site, was      string // stored now; what to put; what the caller stored
		wantStored          string
		wantDone            bool
		wantStatus          int
		wantReturnedOnLeave string
	}{
		{"still the caller's: back to none", "Alice", "", "Alice", "", true, 0, ""},
		{"still the caller's: back to another site", "Alice", "Bob", "Alice", "Bob", true, 0, ""},
		{"somebody gave it to another site", "Bob", "", "Alice", "Bob", false, 0, "Bob"},
		{"somebody took the assignment out", "", "Bob", "Alice", "", false, 0, ""},
		{"the caller stored none, and none is stored", "", "Alice", "", "Alice", true, 0, ""},
		{"a site that is not there any more", "Alice", "Carol", "Alice", "Alice", false, 422, ""},
	} {
		if _, err := s.Assign("pc-a", c.now); err != nil {
			t.Fatal(c.what, err)
		}
		got, done, err := s.AssignIf("pc-a", c.site, c.was)
		if done != c.wantDone || status(err) != c.wantStatus || stored() != c.wantStored {
			t.Errorf("%s: AssignIf = %q, %v, %v; stored %q, want %q", c.what, got, done, err, stored(), c.wantStored)
		}
		if err == nil && ((done && got != c.site) || (!done && got != c.wantReturnedOnLeave)) {
			t.Errorf("%s: AssignIf says %q is stored", c.what, got)
		}
	}
	if _, _, err := s.AssignIf("pc a", "Alice", ""); status(err) != 422 {
		t.Errorf("a target with a space: %v", err)
	}
}
