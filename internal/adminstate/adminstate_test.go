package adminstate

import (
	"fmt"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
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

// seedSessions writes a sessions.json with n sessions per account, the way AddSession writes them; the first
// one of every account is the oldest. It returns the session ids per account, oldest first.
func seedSessions(t *testing.T, a *State, n map[string]int) map[string][]string {
	t.Helper()
	ids := map[string][]string{}
	base := now() - 3600
	_, err := statefile.Update(a.SessionsPath, func(d statefile.Data) error {
		ss := map[string]any{}
		d["sessions"] = ss
		for sub, count := range n {
			for i := range count {
				sid := fmt.Sprintf("sid-%s-%05d", sub, i)
				ss[sid] = map[string]any{"sub": sub, "gen": float64(SessionGen), "created_ts": base + float64(i)}
				ids[sub] = append(ids[sub], sid)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a.sessions.Refresh()
	return ids
}

func countBySub(t *testing.T, a *State) map[string]int {
	t.Helper()
	list, err := a.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, s := range list {
		out[s.Sub]++
	}
	return out
}

// T-04: one account cannot leave more than MaxSessionsPerAccount sessions behind. Each sign-in beyond that
// drops the account's oldest; nobody else's session is touched.
func TestAddSessionCapsTheSessionsOfOneAccount(t *testing.T) {
	a := New(t.TempDir())
	if err := a.AddSession("sid-of-alice", NewSession{Sub: "alice"}); err != nil {
		t.Fatal(err)
	}
	const signIns = MaxSessionsPerAccount + 30
	for i := range signIns {
		if err := a.AddSession(fmt.Sprintf("sid-bob-%03d", i), NewSession{Sub: "bob"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := countBySub(t, a); got["bob"] != MaxSessionsPerAccount || got["alice"] != 1 {
		t.Fatalf("after %d sign-ins of bob: %v", signIns, got)
	}
	iat := float64(time.Now().Unix())
	if !a.SessionValid("sid-of-alice", "alice", iat) {
		t.Fatal("alice lost her session to bob's sign-ins")
	}
	for i := range signIns { // the newest MaxSessionsPerAccount are the ones that stay
		want := i >= signIns-MaxSessionsPerAccount
		if got := a.SessionValid(fmt.Sprintf("sid-bob-%03d", i), "bob", iat); got != want {
			t.Fatalf("bob's sign-in %d of %d: valid=%v, want %v", i+1, signIns, got, want)
		}
	}
}

// T-04: a sessions.json that is already too large (an older release had no limit) shrinks at the next
// sign-in, and the registry as a whole has a limit too. When that is reached, the account with the most
// sessions gives up its oldest ones; an account with a few keeps them.
func TestAddSessionCapsAnOldRegistryAndTheTotal(t *testing.T) {
	a := New(t.TempDir())
	seedSessions(t, a, map[string]int{"flood": 20000, "alice": 2})
	if err := a.AddSession("sid-new", NewSession{Sub: "carol"}); err != nil {
		t.Fatal(err)
	}
	if got := countBySub(t, a); got["flood"] != MaxSessionsPerAccount || got["alice"] != 2 || got["carol"] != 1 {
		t.Fatalf("after one sign-in on a registry of 20002: %v", got)
	}

	// more accounts than fit: every account under its own limit, together over the total
	a = New(t.TempDir())
	n := map[string]int{"alice": 2}
	accounts := MaxSessionsTotal/MaxSessionsPerAccount + 5
	for i := range accounts {
		n[fmt.Sprintf("acct-%03d", i)] = MaxSessionsPerAccount
	}
	ids := seedSessions(t, a, n)
	if err := a.AddSession("sid-new", NewSession{Sub: "carol"}); err != nil {
		t.Fatal(err)
	}
	got := countBySub(t, a)
	total := 0
	for _, c := range got {
		total += c
	}
	if total != MaxSessionsTotal || got["alice"] != 2 || got["carol"] != 1 {
		t.Fatalf("total %d (limit %d), alice %d, carol %d", total, MaxSessionsTotal, got["alice"], got["carol"])
	}
	iat := float64(time.Now().Unix())
	for sub, sids := range ids { // what was dropped is the oldest of an account, never its newest
		if !a.SessionValid(sids[len(sids)-1], sub, iat) {
			t.Fatalf("%s lost its newest session", sub)
		}
	}
}
