package web

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// T-04: an account that signs in again and again (every sign-in with an empty cookie jar is a new session;
// a pending account can do it) must not cost anybody else anything. Before, the registry grew without a
// limit, and when the panel's in-memory session map was full it was emptied: every admin's "Confirm it's
// you" window and every kept action was gone.
func TestSignInFloodLeavesOtherAccountsAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		max     int // room in the in-memory map (4096 in the panel)
		signIns int
	}{
		{"more sign-ins than the registry keeps for one account", 64, adminstate.MaxSessionsPerAccount + 150},
		{"a map smaller than one account's sessions", 16, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			e.s.codec.live.max = tc.max
			csrf := func(ck string) []string { return []string{"Cookie", ck, "X-Requested-With", "fairy"} }
			alice := e.cookie(controller(), 0) // an admin, inside her confirmation window
			if r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, csrf(alice)...); r.code != 200 {
				t.Fatalf("alice's control action before the flood: %d %s", r.code, r.raw)
			}
			carolUser := controller() // another admin, whose confirmation is an hour old: her action is kept
			carolUser.Sub = "u3"
			carol := e.cookie(carolUser, time.Hour)
			if _, _, err := e.s.Access.SetRole(carolUser.Sub, "admin", []string{}, "test", true); err != nil {
				t.Fatal(err)
			}
			r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 7}, csrf(carol)...)
			detail, _ := r.body["detail"].(map[string]any)
			id, _ := detail["pending_id"].(string)
			if r.code != 401 || id == "" {
				t.Fatalf("carol's action was not kept: %d %s", r.code, r.raw)
			}

			bob := model.User{Sub: "u9", Name: model.P("Bob"), Groups: []string{"viewers"}} // no role: pending
			for range tc.signIns {
				c := &reqCtx{s: e.s, w: httptest.NewRecorder(), r: httptest.NewRequest("GET", "/auth/callback", nil)}
				if err := e.s.startSession(c, "", bob, e.s.nowSec(), e.s.nowSec()); err != nil {
					t.Fatal(err)
				}
			}

			if n := len(must(e.s.Poller.Admin.ListSessions())); n > adminstate.MaxSessionsPerAccount+2 {
				t.Fatalf("sessions.json holds %d sessions after %d sign-ins of one account", n, tc.signIns)
			}
			e.s.codec.live.mu.Lock()
			size := len(e.s.codec.live.m)
			e.s.codec.live.mu.Unlock()
			if size > tc.max {
				t.Fatalf("the in-memory map holds %d entries, its limit is %d", size, tc.max)
			}
			if r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 6}, csrf(alice)...); r.code != 200 {
				t.Fatalf("alice lost her confirmation window to another account's sign-ins: %d %s", r.code, r.raw)
			}
			// carol's "Continue": 409 nothing_kept would mean her kept action is gone. (The test panel has no
			// identity provider, so a kept action that is found is answered 502.)
			r = e.do("POST", stepUpPath, map[string]any{"pending_id": id, "next": "/"}, append(csrf(carol), "Content-Type", "application/json")...)
			if r.code == 409 || r.code == 401 {
				t.Fatalf("carol's kept action is gone: %d %s", r.code, r.raw)
			}
		})
	}
}
