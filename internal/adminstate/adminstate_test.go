package adminstate

import (
	"testing"
	"time"
)

// T-22: SessionValid asks for the account as well. A session id is valid for the subject it was registered
// for and for no other, and never for an empty one.
func TestSessionValidChecksTheSubject(t *testing.T) {
	a := New(t.TempDir())
	if err := a.AddSession("sid-of-alice-0001", NewSession{Sub: "alice"}); err != nil {
		t.Fatal(err)
	}
	iat := float64(time.Now().Unix())
	if !a.SessionValid("sid-of-alice-0001", "alice", iat) {
		t.Fatal("the session of the account it was issued to is not valid")
	}
	for _, sub := range []string{"bob", "", "Alice", "alice "} {
		if a.SessionValid("sid-of-alice-0001", sub, iat) {
			t.Fatalf("alice's session id is valid for subject %q", sub)
		}
	}
	if a.SessionValid("sid-unknown", "alice", iat) {
		t.Fatal("an unknown session id is valid")
	}
}
