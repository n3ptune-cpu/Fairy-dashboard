package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/approval"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// controlWrites counts what reached the machines beyond a question: every POST but the one that asks for a
// challenge (an update, a power action, a fan change, a trust).
func controlWrites(machines ...*testutil.Machine) (n int) {
	for _, m := range machines {
		for _, s := range m.Asked() {
			if s.Path != api.PathApprovalChallenge {
				n++
			}
		}
	}
	return n
}

// No control write without "Confirm it's you" (R5), as a table (audit 6, r4-x1-01).
//
// Two machines, each in one of three states: without the check, with the check on and in order, with the check
// on and flagged (it shows another machine id than the one on record). An admin whose sign-in is an hour old
// (outside the panel's window, as a copied cookie is) presses every button that sends something: a single
// Update, a shutdown, a fan change, "Update all", Turn on for both. Whatever the states are, the answer is one
// of four, and in none of them anything was sent:
//
//   - confirm:  the ordinary "Confirm it's you" (401, the request is kept);
//   - approval: the dialog of the machine's own approval (401 with its plan);
//   - refused:  the panel's own "no" (409), nothing is kept;
//   - nothing:  200, and the answer says that nothing was started (every machine the request could go to is
//     flagged, so there is nothing to confirm).
//
// The two rows marked (*) answered 200 and started the other machine's update: "Update all" returned before
// anybody was asked when every machine WITH the check was flagged, and then still sent to the machines
// without it.
func TestNoControlWriteWithoutConfirmation(t *testing.T) {
	const (
		none, on, flagged                   = "no check", "check on", "flagged"
		confirm, approve, refused, nothing_ = "confirm", "approval", "refused", "nothing"
	)
	rows := []struct {
		laptop, tower                               string
		updateOne, power, fan, updateAll, turnOnTwo string
	}{
		{none, none, confirm, confirm, confirm, confirm, approve},
		{on, none, approve, approve, confirm, approve, approve},
		{flagged, none, refused, refused, confirm, confirm, approve}, // (*)
		{none, on, confirm, confirm, approve, approve, approve},
		{none, flagged, confirm, confirm, refused, confirm, approve}, // (*)
		{on, on, approve, approve, approve, approve, refused},
		{on, flagged, approve, approve, refused, approve, refused},
		{flagged, on, refused, refused, approve, approve, refused},
		{flagged, flagged, refused, refused, refused, nothing_, refused},
	}
	for _, row := range rows {
		t.Run(row.laptop+"+"+row.tower, func(t *testing.T) {
			a := newApprovalEnv(t)
			a.selfUpdating(t)
			for m, state := range map[*testutil.Machine]string{a.laptop: row.laptop, a.tower: row.tower} {
				if state == none {
					continue
				}
				a.require(t, m)
				if state == flagged {
					a.setTrustID(t, m, "another-id-"+m.ID+"-0123456789")
					a.poll()
					if b := a.s.Poller.BindingOf(m.ID); b.Status != approval.BindChanged {
						t.Fatalf("%s is not flagged: %+v", m.ID, b)
					}
				}
			}
			shutdown := a.shutdownBody()
			for _, c := range []struct {
				what, path string
				body       any
				want       string
			}{
				{"Update of laptop", "/api/admin/updates/laptop", map[string]any{}, row.updateOne},
				{"shutdown of laptop", "/api/systems/laptop/power", shutdown, row.power},
				{"fan mode of tower", "/api/systems/tower/fan/mode", map[string]any{"mode": "full"}, row.fan},
				{"Update all", "/api/admin/updates", map[string]any{}, row.updateAll},
				{"Turn on for both", "/api/admin/approval/turn-on", map[string]any{"hosts": []string{"laptop", "tower"}}, row.turnOnTwo},
			} {
				if r := a.do("DELETE", "/api/me/pending", nil, a.csrf...); r.code != 200 { // what the press before left waiting
					t.Fatal(r.code)
				}
				r := a.post(c.path, c.body)
				d, _ := r.body["detail"].(map[string]any)
				_, _, asked := approvalAsked(r)
				got := fmt.Sprintf("%d %s", r.code, r.raw)
				switch {
				case asked:
					got = approve
				case r.code == 401 && d["code"] == "step_up_required" && d["approval"] == nil && d["pending"] == true:
					got = confirm
				case r.code == 409 && keptID(a.env, a.admin) == "":
					got = refused
				case r.code == 200 && r.body["all_ok"] == false && !strings.Contains(fmt.Sprint(r.body["lines"]), "started"):
					got = nothing_
				}
				if got != c.want {
					t.Errorf("%s: %s, want %s", c.what, got, c.want)
				}
				if n := controlWrites(a.laptop, a.tower); n != 0 {
					t.Fatalf("%s: %d control requests reached a machine without a confirmation: laptop %q, tower %q",
						c.what, n, sentPaths(a.laptop.Asked()), sentPaths(a.tower.Asked()))
				}
			}
		})
	}
}

// The same press from a copy of the admin's cookie in another browser (the auditor's proof): the panel knows no
// confirmation of that client. With the one machine that has the check flagged, "Update all" is asked to
// confirm like the single Update is, and keeps nothing for that client.
func TestUpdateAllFromACopiedCookieIsAskedToConfirm(t *testing.T) {
	a := newApprovalEnv(t)
	a.selfUpdating(t)
	a.require(t, a.laptop)
	a.setTrustID(t, a.laptop, "another-id-laptop-0123456789")
	a.poll()
	fresh := a.cookie(controller(), 0) // signed in this second, in the browser the session belongs to
	copied := []string{"Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json", "User-Agent", "another-browser/1.0"}
	for _, path := range []string{"/api/admin/updates", "/api/admin/updates/tower"} {
		r := a.do("POST", path, map[string]any{}, copied...)
		d, _ := r.body["detail"].(map[string]any)
		if r.code != 401 || d["code"] != "step_up_required" || d["not_kept"] != notKeptClient {
			t.Fatalf("%s from the copied cookie: %d %s", path, r.code, r.raw)
		}
	}
	if n := controlWrites(a.laptop, a.tower); n != 0 {
		t.Fatalf("%d control requests reached a machine: tower %q", n, sentPaths(a.tower.Asked()))
	}
	if n := len(a.auditEntries("", stepUpAction)); n != 2 {
		t.Fatalf("%d entries about a confirmation that was asked for, want 2", n)
	}
	// and the owner's own browser, inside the panel's window, is not asked twice: the machine without the check
	// starts, the flagged one is named
	r := a.do("POST", "/api/admin/updates", map[string]any{}, "Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json")
	if lines := fmt.Sprint(r.body["lines"]); r.code != 200 || r.body["message"] != "1 started, 1 not." || !strings.Contains(lines, "tower: started") ||
		!strings.Contains(lines, "laptop: not sent (laptop shows another machine id") {
		t.Fatalf("inside the window: %d %s", r.code, r.raw)
	}
	if p := a.tower.Passed(); len(p) != 1 || p[0].Path != "/update" || controlWrites(a.laptop) != 0 {
		t.Fatalf("what was sent: tower %q, laptop %q", sentPaths(a.tower.Asked()), sentPaths(a.laptop.Asked()))
	}
}
