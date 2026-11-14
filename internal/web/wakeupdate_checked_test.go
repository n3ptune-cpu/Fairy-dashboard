package web

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/access"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/poller"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/updates"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// Machines that check passkey approvals themselves in "Update unpowered hosts" runs, through the real routes,
// the real run, the real poller, and Fairy-agent's real gate and run watch on the machines' side
// (testutil.NewRunMachine); Pocket ID is testutil.Issuer. The farm's clock is everybody's clock: the panel's,
// the machines', the issuer's.

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type runRig struct {
	*approvalEnv
	f     *farm
	m     map[string]*testutil.Machine
	boot  map[string]time.Time
	log   *lockedBuf
	seen  []string        // every answer of the panel to the browser
	kept  []time.Duration // for how long each ticket was to be kept (the keeper's timer)
	relay map[string]int
}

// newRunRig is the farm with the given machines as ones that check approvals themselves and take runs. Each
// was on when the panel last saw it (so the panel has its check, its id and its "runs" on record) and is off
// now, like the farm's other machines; all of them are switched on for runs.
func newRunRig(t *testing.T, checked ...string) *runRig {
	t.Helper()
	r := &runRig{approvalEnv: newApprovalEnv(t), m: map[string]*testutil.Machine{}, boot: map[string]time.Time{}, log: &lockedBuf{}, relay: map[string]int{}}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(r.log, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	f := newFarm(t, r.env)
	r.f = f
	f.now = time.Now().UTC().Truncate(time.Second) // tokens are checked against real time by the panel's OIDC library
	r.clocks(r.env)
	r.s.Cfg.Approval.RunApprovalWaitS = 600 // the default
	r.s.Updates = &fakeSource{l: updates.Latest{Tag: "build-17", Version: farmNew, Build: model.P(17), Verified: true}}
	for _, id := range checked {
		m := r.gb.AddRuns(t, id)
		r.m[id] = m
		m.SetNow(func() time.Time { return f.now })
		m.SetUptime(func() time.Duration {
			r.b.Mu.Lock()
			defer r.b.Mu.Unlock()
			return f.now.Sub(f.booted[id])
		})
		m.PowerPending = func() bool {
			r.b.Mu.Lock()
			defer r.b.Mu.Unlock()
			_, ok := f.pending[id]
			return ok
		}
		m.OnSelfOff = func(d time.Duration) {
			r.b.Mu.Lock()
			defer r.b.Mu.Unlock()
			f.pending[id] = f.now.Add(d)
		}
		r.b.Mu.Lock()
		f.on[id], f.booted[id] = true, f.now.Add(-time.Hour)
		r.b.Mu.Unlock()
		r.boot[id] = f.booted[id]
	}
	f.tick()
	for _, id := range checked {
		r.require(t, r.m[id])
		// its run watch looks, as it does every second on a real machine: the run record is made in this boot (a
		// record that is first made in the boot of a ticket takes no ticket from before)
		r.m[id].Runs.Tick()
		f.tick() // the next answer after its id came on record: what it says about runs is kept with it
		if b := r.s.Poller.BindingOf(id); b.Record.Runs != 1 {
			t.Fatalf("%s: that it takes runs is not on record: %+v", id, b.Record)
		}
	}
	r.b.Mu.Lock()
	for _, id := range checked {
		f.on[id] = false
	}
	r.b.Mu.Unlock()
	f.optIn("pc-a", "pc-b")
	f.offline()
	return r
}

// clocks sets every clock of the panel e to the farm's.
func (r *runRig) clocks(e *env) {
	f := r.f
	e.s.codec.now = func() time.Time { return f.now }
	e.s.tickets.now = func() time.Time { return f.now }
	e.s.tickets.after = func(d time.Duration, _ func()) func() { r.kept = append(r.kept, d); return func() {} }
	r.iss.Set(func(i *testutil.Issuer) { i.Now = func() time.Time { return f.now } })
	e.s.Poller.Clock = func() time.Time { return f.now }
}

// tick is n seconds of the farm: a machine that was started gets a new agent process and a new boot, the
// machines' run watches look (a self-switch-off becomes the farm's pending shutdown), then the farm's second.
func (r *runRig) tick(n int) {
	for i := 0; i < n; i++ {
		for id, m := range r.m {
			r.b.Mu.Lock()
			on, booted := r.f.on[id], r.f.booted[id]
			r.b.Mu.Unlock()
			if !booted.Equal(r.boot[id]) {
				r.boot[id] = booted
				m.SetBoot("boot-" + booted.Format(time.RFC3339))
				m.Restart()
			}
			if on && !r.f.now.Before(booted) {
				m.Runs.Tick()
			}
		}
		r.f.tick()
	}
}

func (r *runRig) until(t *testing.T, what string, max int, cond func() bool) {
	t.Helper()
	for i := 0; i < max; i++ {
		if cond() {
			return
		}
		r.tick(1)
	}
	if !cond() {
		a, _ := r.s.WakeRuns.Snapshot()
		text := ""
		if a != nil {
			for _, h := range a.Hosts {
				text += fmt.Sprintf("\n  %s: %s (%s) arm=%s confirm=%s", h.ID, h.State, h.Text, h.Arm, h.Confirm)
			}
		}
		t.Fatalf("after %d s: not %s%s", max, what, text)
	}
}

func (r *runRig) req(method, path string, body any) resp {
	out := r.do(method, path, body, r.csrf...)
	r.seen = append(r.seen, string(out.raw))
	return out
}

func (r *runRig) host(id string) *wakerun.Host {
	a, hist := r.s.WakeRuns.Snapshot()
	if a == nil && len(hist) > 0 {
		a = hist[0]
	}
	if a == nil {
		return &wakerun.Host{}
	}
	return hostOf(a, id)
}

// approved sends a request that needs the machines' passkey approval, checks that the dialog was asked for,
// gives the passkey and returns the dialog's answer and the result of the replayed action.
func (r *runRig) approved(t *testing.T, method, path string, body any) (asked resp, result map[string]any) {
	t.Helper()
	asked = r.req(method, path, body)
	id, _, ok := approvalAsked(asked)
	if !ok {
		t.Fatalf("%s %s did not ask for the machines' approval: %d %s", method, path, asked.code, asked.raw)
	}
	back := r.approve(t, id)
	r.seen = append(r.seen, string(back.raw))
	if back.code != 303 {
		t.Fatalf("the way back from Pocket ID: %d %s", back.code, back.raw)
	}
	me := r.do("GET", "/api/me", nil, "Cookie", r.admin)
	r.seen = append(r.seen, string(me.raw))
	result, _ = me.body["pending_result"].(map[string]any)
	if result == nil {
		t.Fatal("no result of the approved action")
	}
	return asked, result
}

// sent are the requests of the given kind the panel sent to the machine id that reached the backend
// ("/update", "/power"), each with its body as the backend logs it.
func (r *runRig) sent(id, what string) []string {
	r.b.Mu.Lock()
	defer r.b.Mu.Unlock()
	var out []string
	for _, x := range r.b.Requests {
		if strings.HasPrefix(x, id+" POST "+what+" ") {
			out = append(out, x)
		}
	}
	return out
}

func askedPaths(m *testutil.Machine, path string) (n int) {
	for _, s := range m.Asked() {
		if s.Path == path {
			n++
		}
	}
	return n
}

// noTokenAnywhere fails when an ID token of the issuer (or a part of one) is in a file of the panel's data
// folder, in a log line, in an answer to the browser, in the audit log or in a notification.
func (r *runRig) noTokenAnywhere(t *testing.T) {
	t.Helper()
	if len(r.iss.Tokens()) == 0 {
		t.Fatal("no token was issued: this check checks nothing")
	}
	_ = filepath.WalkDir(r.s.Cfg.DataDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			raw, _ := os.ReadFile(path)
			r.noLeak(t, "the file "+filepath.Base(path), string(raw))
		}
		return nil
	})
	r.noLeak(t, "the log", r.log.String())
	r.noLeak(t, "an answer to the browser", strings.Join(r.seen, "\n"))
	r.noLeak(t, "the audit log", fmt.Sprint(r.auditEntries("", "")))
	r.noLeak(t, "the notifications", fmt.Sprint(r.s.Notes.List("", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events))
	a, hist := r.s.WakeRuns.Snapshot()
	r.noLeak(t, "the run", fmt.Sprintf("%+v %+v", a, hist))
}

func (r *runRig) confirmNote() (active bool, text string, n int) {
	for _, e := range r.s.Notes.List("", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events {
		if e.Kind == wuConfirmNote {
			n++
			active, text = e.Active, e.Text
		}
	}
	return active, text, n
}

// The whole way of a mixed run: PC-A checks approvals itself, PC-B does not. One passkey starts the run and
// signs PC-A's ticket; PC-B goes through the run as it always did, without waiting for anybody; PC-A is armed
// when it is up, the banner asks, the second passkey confirms it, and it is updated and switched off with
// requests that name the run. The token of neither passkey is anywhere it must not be.
func TestMixedRunWithAMachineThatChecksApprovals(t *testing.T) {
	r := newRunRig(t, "pc-a")
	body := map[string]any{"hosts": []string{"pc-a", "pc-b"}}
	page := r.req("GET", "/api/admin/wake-update", nil)
	if h := wuHostRow(t, page, "pc-a"); h["choice"] != "wake" || h["checked"] != true || h["approval"] != "required" || h["no_run"] != nil {
		t.Fatalf("pc-a can be chosen, as a machine with the check: %v", h)
	}
	if h := wuHostRow(t, page, "pc-b"); h["choice"] != "wake" || h["checked"] != nil {
		t.Fatalf("pc-b is as it was: %v", h)
	}
	// Pocket ID's clock is half a minute behind: the token is "issued" 30 s before it reaches the panel
	r.iss.Set(func(i *testutil.Issuer) { i.Now = func() time.Time { return r.f.now.Add(-30 * time.Second) } })
	asked, res := r.approved(t, "POST", "/api/admin/wake-update", body)
	r.iss.Set(func(i *testutil.Issuer) { i.Now = func() time.Time { return r.f.now } })
	// the ticket is kept for what is left of its 420 s by the token's own "issued at", not for 420 s from now
	if len(r.kept) != 1 || r.kept[0] != 390*time.Second {
		t.Fatalf("the ticket is kept for %v", r.kept)
	}
	// the dialog names the numbers of the ticket, the version and the countdown for the machine with the check
	d := asked.body["detail"].(map[string]any)
	items := fmt.Sprint(d["items"])
	for _, want := range []string{"PC-A: wake; signs its ticket: once up it switches itself off after 960 s (notice 120 s) unless a second passkey confirms: update to " + farmNew + ", shut down in 120 s. Found on: nothing is sent",
		"PC-B: wake, update, switch off again (skipped if it turns out to be on)"} {
		if !strings.Contains(items, want) {
			t.Errorf("the start dialog lacks %q:\n%s", want, items)
		}
	}
	if msg := fmt.Sprint(d["message"]); !strings.Contains(msg, "signs a ticket, good for 7 minutes") || !strings.Contains(msg, "second passkey") || !strings.Contains(msg, "10 minutes to give it") {
		t.Errorf("the start dialog's sentence: %s", msg)
	}
	if res["ok"] != true {
		t.Fatalf("the start: %v", res)
	}
	run, _ := r.s.WakeRuns.Snapshot()
	if run == nil || run.Ticket == nil || *run.Ticket != (wakerun.Ticket{ValidS: 420, WaitS: 960, NoticeS: 120, WithinS: 1560, ConfirmWaitS: 600}) {
		t.Fatalf("the run and the numbers that were signed: %+v", run)
	}
	if held, open := r.s.tickets.held(); held != run.ID || open != 1 {
		t.Fatalf("the ticket is kept for pc-a: %q %d", held, open)
	}
	// PC-A comes up and is armed: one request, and the ticket is gone from the panel
	r.until(t, "pc-a armed", 200, func() bool { return r.host("pc-a").Arm == wakerun.ArmArmed })
	if n := askedPaths(r.m["pc-a"], api.PathRunArm); n != 1 {
		t.Fatalf("%d arm requests", n)
	}
	if held, open := r.s.tickets.held(); held != "" || open != 0 {
		t.Fatalf("the ticket is still kept after its one use: %q %d", held, open)
	}
	if st := r.m["pc-a"].Gate.StatusNow().Run; st == nil || st.ID != run.ID || st.State != api.RunArmed {
		t.Fatalf("the machine's own word: %+v", st)
	}
	// the banner: asked once no machine with the check is on its way up; nothing was asked of the machine yet
	r.until(t, "the run asking for its confirmation", 60, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a != nil && a.Approval == "needed" })
	w := r.req("GET", "/api/admin/wake-update/waiting", nil)
	if w.body["run"] != run.ID || fmt.Sprint(w.body["hosts"]) != "[PC-A]" || w.body["left_s"].(float64) < 590 || len(w.body["armed"].([]any)) != 1 {
		t.Fatalf("the banner's data: %s", w.raw)
	}
	if active, text, n := r.confirmNote(); !active || n != 1 || !strings.Contains(text, "Fairy woke PC-A for the run") || !strings.Contains(text, "confirm within 10 minutes") ||
		!strings.Contains(text, "Not confirmed: not updated, and a machine that is armed switches itself off again.") {
		t.Fatalf("the notification: %v %d %q", active, n, text)
	}
	if n := askedPaths(r.m["pc-a"], api.PathApprovalChallenge); n != 0 {
		t.Fatalf("%d challenges were asked before anybody clicked Confirm", n)
	}
	if len(r.sent("pc-a", "/update"))+len(r.sent("pc-a", "/power")) != 0 {
		t.Fatalf("something was sent to pc-a before its confirmation: %v %v", r.sent("pc-a", "/update"), r.sent("pc-a", "/power"))
	}
	// PC-B is not held up by any of this: it is updated and counting down, or off, with the plain requests
	r.until(t, "pc-b updated and asked to shut down", 400, func() bool { return len(r.sent("pc-b", "/power")) > 0 })
	for _, x := range append(r.sent("pc-b", "/update"), r.sent("pc-b", "/power")...) {
		if strings.Contains(x, "run:") || strings.Contains(x, "approval") {
			t.Fatalf("pc-b's request is not the plain one: %s", x)
		}
	}
	// the second passkey: the click asks the challenge, the dialog names the exact request, one confirmation
	asked, res = r.approved(t, "POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{})
	d = asked.body["detail"].(map[string]any)
	if items := fmt.Sprint(d["items"]); !strings.Contains(items, "PC-A: update to "+farmNew+", then shut down in 120 s (good for 26 minutes) (checked on the machine)") {
		t.Errorf("the confirmation's dialog: %s", items)
	}
	if res["ok"] != true || res["message"] != "confirmed on 1 of 1 machine" || fmt.Sprint(res["items"]) != "[PC-A: confirmed]" {
		t.Fatalf("the confirmation: %v", res)
	}
	if n := askedPaths(r.m["pc-a"], api.PathRunGo); n != 1 {
		t.Fatalf("%d confirmations were sent", n)
	}
	r.tick(1) // the run's events are handed on with its next step
	if active, _, _ := r.confirmNote(); active {
		t.Fatal("the notification still asks after the confirmation")
	}
	// a run is confirmed once
	if again := r.req("POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{}); again.code != 409 || !strings.Contains(string(again.raw), "this run was confirmed already") {
		t.Fatalf("a second confirmation: %d %s", again.code, again.raw)
	}
	r.until(t, "the run over", 900, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a == nil })
	upd, pow := r.sent("pc-a", "/update"), r.sent("pc-a", "/power")
	if len(upd) != 1 || !strings.Contains(upd[0], "run:"+run.ID) || strings.Contains(upd[0], "approval") {
		t.Fatalf("pc-a's update names the run, once: %v", upd)
	}
	if len(pow) != 1 || !strings.Contains(pow[0], "run:"+run.ID) || !strings.Contains(pow[0], "delay_s:120") || !strings.Contains(pow[0], "action:shutdown") {
		t.Fatalf("pc-a's shutdown names the run and has exactly the countdown that was confirmed, once: %v", pow)
	}
	for _, id := range []string{"pc-a", "pc-b"} {
		if h := r.host(id); h.State != wakerun.StOff || h.Update != wakerun.UpdOK {
			t.Fatalf("%s: %s %s %q", id, h.State, h.Update, h.Text)
		}
	}
	if st := r.auditEntries("", "wakeupdate.confirm"); len(st) != 2 || st[1]["outcome"] != "confirmed on 1 of 1 machine" {
		t.Fatalf("the audit entries of the confirmation: %v", st)
	}
	if n := len(r.auditEntries("", "wakeupdate.arm")); n != 1 {
		t.Fatalf("%d audit entries of the arming", n)
	}
	r.noTokenAnywhere(t)
}

// Nobody confirms. The machine was armed, so it switches itself off again without the panel, and the report
// says "not confirmed, not updated" and "off" only because the run saw it go. The panel sent it no update and
// no shutdown.
func TestNobodyConfirmsAndTheMachineSwitchesItselfOff(t *testing.T) {
	r := newRunRig(t, "pc-a")
	_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if res["ok"] != true {
		t.Fatal(res)
	}
	r.until(t, "the run asking", 300, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a != nil && a.Approval == "needed" })
	r.until(t, "the question over", 700, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a == nil || a.Approval == "over" })
	if w := r.req("GET", "/api/admin/wake-update/waiting", nil); w.body["run"] != nil || len(w.body["armed"].([]any)) != 1 {
		t.Fatalf("the banner no longer asks, and still names the armed machine: %s", w.raw)
	}
	if active, _, _ := r.confirmNote(); active {
		t.Fatal("the notification still asks")
	}
	if late := r.req("POST", "/api/admin/wake-update/"+r.host("pc-a").ID+"/confirm", map[string]any{}); late.code != 409 {
		t.Fatalf("a confirmation for something that is no run: %d", late.code)
	}
	r.until(t, "the run over", 1500, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a == nil })
	h := r.host("pc-a")
	if h.State != wakerun.StOff || h.Text != "It came online after the wake. Nobody confirmed within 10 min: not confirmed, not updated. It switched itself off again." {
		t.Fatalf("pc-a: %s %q", h.State, h.Text)
	}
	if n := len(r.sent("pc-a", "/update")) + len(r.sent("pc-a", "/power")); n != 0 {
		t.Fatalf("the panel sent pc-a %d requests without a confirmation", n)
	}
	if n := askedPaths(r.m["pc-a"], api.PathApprovalChallenge) + askedPaths(r.m["pc-a"], api.PathRunGo); n != 0 {
		t.Fatalf("%d challenges or confirmations without a click", n)
	}
	if w := r.req("GET", "/api/admin/wake-update/waiting", nil); w.body["armed"] != nil {
		t.Fatalf("a machine that is off is still named as armed: %s", w.raw)
	}
	r.noTokenAnywhere(t)
}

// The ticket is as old as it may get before the machine is up: it is not sent, the machine is not armed and
// stays on, and the sentence says so.
func TestTicketTooOldWhenTheMachineIsUp(t *testing.T) {
	r := newRunRig(t, "pc-a")
	r.f.wakes = false // the first wake packets do nothing
	_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if res["ok"] != true {
		t.Fatal(res)
	}
	r.tick(200)
	r.s.tickets.now = func() time.Time { return r.f.now.Add(10 * time.Minute) } // the panel's steady clock says: older than valid_s
	r.f.wakes = true
	r.until(t, "pc-a waiting, not armed", 400, func() bool { return r.host("pc-a").State == wakerun.StApproval })
	h := r.host("pc-a")
	if h.Arm != wakerun.ArmNotSent || !strings.Contains(h.Text, "it was not armed (the ticket of the run's start was older than it may be when the machine was up): it stays ON unless you confirm") {
		t.Fatalf("pc-a: %s %q", h.Arm, h.Text)
	}
	if n := askedPaths(r.m["pc-a"], api.PathRunArm); n != 0 {
		t.Fatalf("%d arm requests with a ticket that was too old", n)
	}
	if held, _ := r.s.tickets.held(); held != "" {
		t.Fatal("the old ticket is still kept")
	}
}

// What the machine says "no" to is reported with the reason, and nothing asks for another passkey by itself:
// a challenge refused as "too late" ends the click without a dialog; a confirmation refused after the passkey
// is in the result and in the row, and no update and no shutdown follows.
func TestRefusedConfirmationIsReportedNotAskedAgain(t *testing.T) {
	r := newRunRig(t, "pc-a")
	_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if res["ok"] != true {
		t.Fatal(res)
	}
	run, _ := r.s.WakeRuns.Snapshot()
	r.until(t, "the run asking", 300, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a != nil && a.Approval == "needed" })
	tokens := len(r.iss.Tokens())
	r.gb.Lie("pc-a", func(path string) (int, jsonx.M) {
		if path == api.PathApprovalChallenge {
			return 409, jsonx.M{"code": api.CodeRunRefused, "reason": api.RunReasonTooLate, "error": "run refused (too_late)"}
		}
		return 0, nil
	})
	no := r.req("POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{})
	if no.code != 409 || !strings.Contains(string(no.raw), "The confirmation was not asked for, because no machine of the run takes one now: PC-A (PC-A does not take the run's confirmation: its wait was over: it is switching itself off). Nothing was sent.") {
		t.Fatalf("a challenge refused as too late: %d %s", no.code, no.raw)
	}
	// the challenge is given, the confirmation itself is refused
	r.gb.Lie("pc-a", func(path string) (int, jsonx.M) {
		if path == api.PathRunGo {
			return 409, jsonx.M{"code": api.CodeRunRefused, "reason": api.RunReasonOver, "error": "run refused (run_over)"}
		}
		return 0, nil
	})
	_, res = r.approved(t, "POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{})
	if res["ok"] != false || res["message"] != "confirmed on 0 of 1 machine" ||
		fmt.Sprint(res["items"]) != "[PC-A: NOT accepted (this run is over on it: it was cancelled or finished there, or the machine was started again)]" {
		t.Fatalf("the result of a refused confirmation: %v", res)
	}
	if len(r.iss.Tokens()) != tokens+1 {
		t.Fatalf("%d passkeys were asked for; one click, one passkey", len(r.iss.Tokens())-tokens)
	}
	r.gb.Lie("pc-a", nil)
	if again := r.req("POST", "/api/admin/wake-update/"+run.ID+"/confirm", map[string]any{}); again.code != 409 {
		t.Fatalf("a second confirmation of the run: %d %s", again.code, again.raw)
	}
	r.until(t, "the run over", 1500, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a == nil })
	h := r.host("pc-a")
	if h.Confirm != wakerun.ConfirmRefused || !strings.Contains(h.Text, "Its confirmation was not accepted (this run is over on it") || !strings.HasSuffix(h.Text, "): not confirmed, not updated. It switched itself off again.") {
		t.Fatalf("pc-a: %s %q", h.Confirm, h.Text)
	}
	if n := len(r.sent("pc-a", "/update")) + len(r.sent("pc-a", "/power")); n != 0 {
		t.Fatalf("%d requests to a machine whose confirmation was refused", n)
	}
}

// The arming gets no answer although it arrived. It is not sent again; the machine's own word shows it as
// armed, and the run goes by that.
func TestUnansweredArmIsNotSentAgain(t *testing.T) {
	r := newRunRig(t, "pc-a")
	arms := 0
	r.gb.Relay("pc-a", func(path string, body any) (int, jsonx.M) {
		if path != api.PathRunArm {
			return 0, nil
		}
		arms++
		r.gb.Relay("pc-a", nil)
		r.gb.Forward("pc-a", path, body) // it arrives
		r.gb.Relay("pc-a", func(path string, _ any) (int, jsonx.M) {
			if path == api.PathRunArm {
				arms++
			}
			return 0, nil
		})
		return 504, jsonx.M{} // and the answer is lost on the way
	})
	_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if res["ok"] != true {
		t.Fatal(res)
	}
	r.until(t, "pc-a waiting", 300, func() bool { return r.host("pc-a").State == wakerun.StApproval })
	if h := r.host("pc-a"); h.Arm != wakerun.ArmUnanswered {
		t.Fatalf("pc-a: %s %q", h.Arm, h.Text)
	}
	r.until(t, "the row saying that it is armed", 60, func() bool {
		return strings.HasPrefix(r.host("pc-a").Text, "awake and armed: it switches itself off at")
	})
	r.tick(120)
	if arms != 1 {
		t.Fatalf("the arming was sent %d times", arms)
	}
}

// Keep on: the cancel, then the machine's own word. "Stays ON" only when it shows its run as cancelled; a
// cancel that is not the agent's "ok" is "NOT cancelled", with the time the machine may go.
func TestKeepOn(t *testing.T) {
	r := newRunRig(t, "pc-a")
	_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if res["ok"] != true {
		t.Fatal(res)
	}
	r.until(t, "pc-a armed", 300, func() bool { return r.host("pc-a").Arm == wakerun.ArmArmed })
	at := r.host("pc-a").SelfOffAt.UTC().Format("15:04 UTC")
	// Keep on is a control action like the cancel of a run: it wants a sign-in from the last minutes
	r.admin = r.cookie(controller(), 0)
	r.csrf = []string{"Cookie", r.admin, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	r.b.Mu.Lock()
	r.b.Reply = func(id, path string) (int, jsonx.M, error) {
		if id == "pc-a" && path == "/power" {
			return 503, jsonx.M{"error": "the helper is not answering", "code": "helper_stopped"}, nil
		}
		return 0, nil, nil
	}
	r.b.Mu.Unlock()
	no := r.req("POST", "/api/systems/pc-a/wake-update/keep-on", map[string]any{})
	if no.code == 200 || !strings.Contains(string(no.raw), "NOT cancelled (") || !strings.Contains(string(no.raw), "PC-A may switch itself off at "+at+". Try again, or stop it at the machine.") {
		t.Fatalf("a cancel that was refused: %d %s\n%s", no.code, no.raw, lastLines(r.log.String(), 3))
	}
	r.b.Mu.Lock()
	r.b.Reply = nil
	r.b.Mu.Unlock()
	// the cancel is answered, and the machine still shows the run (an agent that is not honest about its cancel)
	r.gb.Stale("pc-a", jsonx.M{"state": "required", "enforced_by": "agent", "proto": 2.0, "machine_id": machineIDOf("pc-a"), "issuer": r.iss.URL(), "client_id": testApprovalClient,
		"users": 1.0, "runs": 1.0, "run": jsonx.M{"id": "00000000000000aa", "state": "armed"}})
	// (the block names another run on purpose: any run that is live on the machine counts)
	no = r.req("POST", "/api/systems/pc-a/wake-update/keep-on", map[string]any{})
	if no.code == 200 || !strings.Contains(string(no.raw), "NOT cancelled: PC-A answered the cancel and still shows its run as armed. PC-A may switch itself off") {
		t.Fatalf("a cancel after which the machine still shows the run: %d %s", no.code, no.raw)
	}
	r.gb.Stale("pc-a", nil)
	// the real thing
	yes := r.req("POST", "/api/systems/pc-a/wake-update/keep-on", map[string]any{})
	if yes.code != 200 || yes.body["stays_on"] != true || yes.body["message"] != "PC-A stays ON: its switch-off was called off (it shows its run as cancelled)." {
		t.Fatalf("keep on: %d %s", yes.code, yes.raw)
	}
	r.until(t, "the run over", 300, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a == nil })
	if h := r.host("pc-a"); h.State != wakerun.StLeftOn || !strings.Contains(h.Text, "its switch-off was called off") || !strings.Contains(h.Text, "It is ON.") {
		t.Fatalf("pc-a: %s %q", h.State, h.Text)
	}
	if n := len(r.auditEntries("", "wakeupdate.keep_on")); n != 3 {
		t.Fatalf("%d audit entries of Keep on", n)
	}
	if w := r.req("GET", "/api/admin/wake-update/waiting", nil); w.body["armed"] != nil {
		t.Fatalf("a machine that was kept on is still named as armed: %s", w.raw)
	}
}

// A restart of the panel while a machine is armed: the ticket is gone, the run is reported as interrupted
// with the time the machine switches itself off, the banner names it, a warning is in the list, and Keep on
// works without a run.
func TestPanelRestartWhileAMachineIsArmed(t *testing.T) {
	r := newRunRig(t, "pc-a")
	_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if res["ok"] != true {
		t.Fatal(res)
	}
	r.until(t, "pc-a armed", 300, func() bool { return r.host("pc-a").Arm == wakerun.ArmArmed })
	at := r.host("pc-a").SelfOffAt.UTC().Format("15:04:05 UTC")
	// the panel starts again on the same folders
	p, err := poller.New(r.s.Cfg, r.s.Secrets, r.gb)
	if err != nil {
		t.Fatal(err)
	}
	p.Clock = func() time.Time { return r.f.now }
	p.PollOnce(context.Background())
	s := New(r.s.Cfg, r.s.Secrets, p, nil, "test")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e2 := &env{t: t, s: s, srv: srv, b: r.b}
	r.env, r.f = e2, r.f.restarted(e2)
	r.clocks(e2)
	s.control, s.approvalRL = newRateLimiter(1000), newRateLimiter(1000)
	r.admin = e2.cookie(controller(), 0)
	r.csrf = []string{"Cookie", r.admin, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	if held, _ := s.tickets.held(); held != "" {
		t.Fatal("a new panel holds a ticket")
	}
	active, hist := s.WakeRuns.Snapshot()
	if active != nil || len(hist) != 1 || hist[0].State != wakerun.RunInterrupted {
		t.Fatalf("the run after the restart: %+v %+v", active, hist)
	}
	h := hostOf(hist[0], "pc-a")
	want := "The panel restarted while it waited for the run's second confirmation. It was armed: it switches itself off at about " + at +
		", with a notice of 2 min, unless that is cancelled (Keep on in the panel, or at the machine). A run is never continued, so it can no longer be confirmed. Not confirmed, not updated."
	if h.Text != want || !h.MayGoOff {
		t.Fatalf("pc-a after the restart:\n%q\nwant\n%q", h.Text, want)
	}
	w := r.req("GET", "/api/admin/wake-update/waiting", nil)
	armed, _ := w.body["armed"].([]any)
	if w.body["run"] != nil || len(armed) != 1 || armed[0].(map[string]any)["name"] != "PC-A" || armed[0].(map[string]any)["sure"] != true {
		t.Fatalf("the banner after the restart: %s", w.raw)
	}
	warned := false
	for _, e := range s.Notes.List("", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events {
		warned = warned || (e.Kind == "wakeupdate_interrupted" && e.Severity == notifications.Warning && strings.Contains(e.Text, "It was armed: it switches itself off at about "+at))
	}
	if !warned {
		t.Fatal("no warning about the interrupted run that names the armed machine and its time")
	}
	yes := r.req("POST", "/api/systems/pc-a/wake-update/keep-on", map[string]any{})
	if yes.code != 200 || yes.body["stays_on"] != true {
		t.Fatalf("keep on after the restart: %d %s", yes.code, yes.raw)
	}
	if st := r.m["pc-a"].Gate.StatusNow().Run; st == nil || st.State != api.RunCancelled {
		t.Fatalf("the machine's own word after Keep on: %+v", st)
	}
	r.noTokenAnywhere(t)
}

// Who is in no run, and why: an agent from before runs with the check on (the sentence names the version that
// can), and two hosts with one machine id, whoever was said to keep it. Refused before anybody is asked for a
// passkey; nothing is sent.
func TestMachinesWithTheCheckThatNoRunTakes(t *testing.T) {
	r := newRunRig(t, "pc-a", "pc-b")
	start := func(body any) resp { return r.req("POST", "/api/admin/wake-update", body) }
	page := func(id string) map[string]any { return wuHostRow(t, r.req("GET", "/api/admin/wake-update", nil), id) }
	// v0.8.0: the check, and no "runs". What it last said is what counts, and it says so while it is on.
	r.b.Mu.Lock()
	r.f.on["pc-a"], r.f.booted["pc-a"] = true, r.f.now.Add(-time.Hour)
	r.b.Mu.Unlock()
	r.boot["pc-a"] = r.f.booted["pc-a"]
	block := r.m["pc-a"].Block()
	delete(block, "runs")
	r.gb.Stale("pc-a", block)
	r.tick(2)
	const old = "needs Fairy-agent v0.9.0 or newer to be in a run (this machine checks a passkey approval itself before an update or a shutdown, " +
		"and its agent, or on Windows its running helper, cannot take a run's ticket yet; use Update and Shut down on its own page)"
	if h := page("pc-a"); h["choice"] != "" || h["no_run"] != true || h["checked"] != nil || h["why"] != old {
		t.Fatalf("pc-a with an agent from before runs: %v", h)
	}
	for _, body := range []any{map[string]any{"hosts": []string{"pc-a"}}, map[string]any{"update_only": []string{"pc-a"}}} {
		if no := start(body); no.code != 400 || no.body["detail"] != "PC-A "+old {
			t.Fatalf("a start with it: %d %s", no.code, no.raw)
		}
	}
	// back on v0.9.0, and off again
	r.gb.Stale("pc-a", nil)
	r.tick(2)
	r.b.Mu.Lock()
	r.f.on["pc-a"] = false
	r.b.Mu.Unlock()
	r.f.offline()
	if h := page("pc-a"); h["choice"] != "wake" || h["checked"] != true {
		t.Fatalf("pc-a on v0.9.0: %v", h)
	}
	// PC-B comes on and shows PC-A's machine id (as if both had been set up with one trust file)
	r.b.Mu.Lock()
	r.f.on["pc-b"], r.f.booted["pc-b"] = true, r.f.now.Add(-time.Hour)
	r.b.Mu.Unlock()
	r.boot["pc-b"] = r.f.booted["pc-b"]
	block = r.m["pc-b"].Block()
	block["machine_id"] = machineIDOf("pc-a")
	r.gb.Stale("pc-b", block)
	r.tick(2)
	if shared := r.s.Poller.SharedMachineID("pc-a"); fmt.Sprint(shared) != "[pc-b]" {
		t.Fatalf("SharedMachineID: %v", shared)
	}
	// An admin says that PC-A is the one that keeps the id: for every other approval PC-A is in order again.
	// Not for a run: its ticket answers no challenge, and the id is all that says which machine it is for.
	if what, _, err := r.s.Poller.AcceptMachine("pc-a", "test admin", poller.AcceptWant{MachineID: machineIDOf("pc-a")}); err != nil {
		t.Fatalf("the accept: %q %v", what, err)
	}
	if b := r.s.Poller.BindingOf("pc-a"); !b.OK() || !b.Record.Holder {
		t.Fatalf("pc-a keeps the id: %+v", b)
	}
	if h := page("pc-a"); h["why"] != "in no run: PC-B has the same machine id on record or shows it, and a run's ticket is tied to the machine id alone: no ticket is signed while two hosts share one (see their pages)" {
		t.Fatalf("pc-a, which keeps the id: %v", h)
	}
	before := len(r.iss.Tokens())
	for _, id := range []string{"pc-a", "pc-b"} {
		h := page(id)
		if h["choice"] != "" || h["no_run"] != true || !strings.HasPrefix(fmt.Sprint(h["why"]), "in no run: ") {
			t.Fatalf("%s while two hosts have one machine id: %v", id, h)
		}
		if no := start(map[string]any{"hosts": []string{id}}); no.code != 400 || !strings.Contains(string(no.raw), "is in no run: ") {
			t.Fatalf("a start with %s: %d %s", id, no.code, no.raw)
		}
	}
	if a, hist := r.s.WakeRuns.Snapshot(); a != nil || len(hist) != 0 || len(r.iss.Tokens()) != before {
		t.Fatal("a run was started, or a passkey asked for")
	}
}

// The elevated helper is what counts a ticket's wait on Windows. While a machine is armed, the panel does not
// stop it.
func TestHelperStopRefusedWhileArmed(t *testing.T) {
	r := newRunRig(t, "pc-a")
	x := model.System{ID: "pc-a", Name: "PC-A"}
	if why := r.s.helperStopRefusal(x); why != "" {
		t.Fatalf("no run: %q", why)
	}
	_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
	if res["ok"] != true {
		t.Fatal(res)
	}
	r.until(t, "pc-a armed", 300, func() bool { return r.host("pc-a").Arm == wakerun.ArmArmed })
	want := "PC-A is in a run of \"Update unpowered hosts\" (armed, or may be), and its helper is what counts the run's wait and switches this PC off again: " +
		"it is not stopped while that is so. Keep the machine on first (Keep on, Updates page), or wait until the run is over on it. Nothing was sent."
	if why := r.s.helperStopRefusal(x); why != want {
		t.Fatalf("armed by the run's word:\n%q", why)
	}
	// by the machine's own word alone (the panel was restarted, the run is long over)
	x.Approval = &model.Approval{Run: &model.ApprovalRun{ID: "00000000000000aa", State: api.RunOffPending}}
	x.ID = "other"
	if why := r.s.helperStopRefusal(x); !strings.Contains(why, "(off_pending)") {
		t.Fatalf("counting down by its own word: %q", why)
	}
	x.Approval.Run.State = api.RunCancelled
	if why := r.s.helperStopRefusal(x); why != "" {
		t.Fatalf("cancelled: %q", why)
	}
}

// lastLines are the last n lines of the log, for a failure's message.
func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

// A machine with the check that is ON and chosen for an update only: the passkey of the start answers its own
// challenge for that update, the approval goes with the one update request and nowhere else, and the machine
// is never switched off. No ticket is signed for it, and no second passkey is asked for.
func TestUpdateOnlyMachineWithTheCheck(t *testing.T) {
	r := newRunRig(t, "pc-a")
	r.b.Mu.Lock()
	r.f.on["pc-a"], r.f.booted["pc-a"] = true, r.f.now.Add(-time.Hour)
	r.b.Mu.Unlock()
	r.boot["pc-a"] = r.f.booted["pc-a"]
	r.tick(3)
	if h := wuHostRow(t, r.req("GET", "/api/admin/wake-update", nil), "pc-a"); h["choice"] != "update_only" || h["checked"] != true {
		t.Fatalf("pc-a, on: %v", h)
	}
	asked, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"update_only": []string{"pc-a"}})
	if items := fmt.Sprint(asked.body["detail"].(map[string]any)["items"]); items != "[PC-A: update only, with this passkey (it checks the approval itself); it is on and is never switched off]" {
		t.Fatalf("the dialog: %s", items)
	}
	if res["ok"] != true {
		t.Fatal(res)
	}
	run, _ := r.s.WakeRuns.Snapshot()
	if run == nil || run.Ticket != nil {
		t.Fatalf("a run without a ticket: %+v", run)
	}
	if n := askedPaths(r.m["pc-a"], api.PathApprovalChallenge); n != 1 {
		t.Fatalf("%d challenges for the one update", n)
	}
	r.until(t, "the run over", 300, func() bool { a, _ := r.s.WakeRuns.Snapshot(); return a == nil })
	if h := r.host("pc-a"); h.State != wakerun.StStayedOn || h.Update != wakerun.UpdOK {
		t.Fatalf("pc-a: %s %s %q", h.State, h.Update, h.Text)
	}
	approved := 0
	for _, s := range r.m["pc-a"].Passed() {
		if s.Path == "/update" && s.Approved {
			approved++
		} else if s.Path == "/update" || s.Path == "/power" {
			t.Fatalf("a request without its approval got through: %+v", s.Path)
		}
	}
	if approved != 1 || len(r.sent("pc-a", "/power")) != 0 || askedPaths(r.m["pc-a"], api.PathRunArm)+askedPaths(r.m["pc-a"], api.PathRunGo) != 0 {
		t.Fatalf("one approved update and nothing else: %d %v", approved, r.sent("pc-a", "/power"))
	}
	if held, _ := r.s.tickets.held(); held != "" {
		t.Fatal("the approval of the start is still kept")
	}
	if a, _, n := r.confirmNote(); a || n != 0 {
		t.Fatal("a second passkey was asked for")
	}
	// (the update request itself carries the approval to the machine: that is where it belongs, and the test
	// backend's own list of requests is not a place of the panel's)
	r.noTokenAnywhere(t)
}

// "200" from the machine is taken for "armed" only when it says so for this run with the ticket's wait: any
// other answer is neither "armed" nor "not armed", and the machine's own word decides.
func TestArmAnswerForSomethingElseIsNotTakenForArmed(t *testing.T) {
	for name, answer := range map[string]jsonx.M{
		"another run":  {"run": "00000000000000aa", "state": "armed", "off_in_s": 960.0},
		"another wait": {"state": "armed", "off_in_s": 3600.0},
		"not armed":    {"state": "confirmed", "off_in_s": 960.0},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRunRig(t, "pc-a")
			_, res := r.approved(t, "POST", "/api/admin/wake-update", map[string]any{"hosts": []string{"pc-a"}})
			if res["ok"] != true {
				t.Fatal(res)
			}
			run, _ := r.s.WakeRuns.Snapshot()
			if answer["run"] == nil {
				answer["run"] = run.ID
			}
			r.gb.Lie("pc-a", func(path string) (int, jsonx.M) {
				if path == api.PathRunArm {
					return 200, answer
				}
				return 0, nil
			})
			r.until(t, "pc-a waiting", 300, func() bool { return r.host("pc-a").State == wakerun.StApproval })
			if h := r.host("pc-a"); h.Arm != wakerun.ArmUnanswered || h.SelfOffAt != nil {
				t.Fatalf("pc-a: %s %q", h.Arm, h.Text)
			}
		})
	}
}

// What the strip shows (which machines a run woke, and when they switch themselves off) is for admins: the
// stream sends run_wait to nobody else, and the route answers nobody else.
func TestRunWaitIsForAdminsOnly(t *testing.T) {
	e := setup(t)
	if _, body := e.streamBody("/api/stream", "Cookie", e.cookie(controller(), 0)); !strings.Contains(body, "event: run_wait\ndata: {") {
		t.Fatalf("an admin's stream has no run_wait event:\n%.300s", body)
	}
	e.cookie(viewer, 0) // seen once, then given a role: a viewer of everything
	if _, _, err := e.s.Access.SetRole(viewer.Sub, access.RoleViewer, []string{}, "test", true); err != nil {
		t.Fatal(err)
	}
	if _, body := e.streamBody("/api/stream", "Cookie", e.cookie(viewer, 0)); strings.Contains(body, "run_wait") || !strings.Contains(body, "event: summary") {
		t.Fatalf("a viewer's stream has a run_wait event (or nothing at all):\n%.300s", body)
	}
	if r := e.do("GET", "/api/admin/wake-update/waiting", nil, "Cookie", e.cookie(viewer, 0)); r.code != 403 {
		t.Fatalf("a viewer reads what the strip shows: %d", r.code)
	}
	if r := e.do("GET", "/api/admin/wake-update/waiting", nil, "Cookie", e.cookie(controller(), 0)); r.code != 200 || r.body["run"] != nil || r.body["armed"] != nil {
		t.Fatalf("an admin, with no run: %d %s", r.code, r.raw)
	}
}

// Between the dialog of a start and the passkey, an admin accepts another machine id for a machine of the
// list. The approval was made for the id that was on record when the dialog was shown: the replay starts
// nothing, and says that the machines are not as they were.
func TestStartIsForTheMachineIDsTheDialogWasMadeFor(t *testing.T) {
	r := newRunRig(t, "pc-a")
	r.b.Mu.Lock()
	r.f.on["pc-a"], r.f.booted["pc-a"] = true, r.f.now.Add(-time.Hour)
	r.b.Mu.Unlock()
	r.boot["pc-a"] = r.f.booted["pc-a"]
	r.tick(3)
	asked := r.req("POST", "/api/admin/wake-update", map[string]any{"update_only": []string{"pc-a"}})
	id, _, ok := approvalAsked(asked)
	if !ok {
		t.Fatalf("the start: %d %s", asked.code, asked.raw)
	}
	const other = "another-machine-id-0123456789"
	block := r.m["pc-a"].Block()
	block["machine_id"] = other
	r.gb.Stale("pc-a", block)
	r.tick(2)
	if what, _, err := r.s.Poller.AcceptMachine("pc-a", "test admin", poller.AcceptWant{MachineID: other}); err != nil {
		t.Fatalf("the accept: %q %v", what, err)
	}
	if b := r.s.Poller.BindingOf("pc-a"); !b.OK() || b.Record.MachineID != other {
		t.Fatalf("the new id is on record and in order: %+v", b)
	}
	if back := r.approve(t, id); back.code != 303 {
		t.Fatalf("the way back: %d %s", back.code, back.raw)
	}
	res := r.result(t)
	if res["ok"] != false || res["message"] != "The machines of this run are not as they were when you confirmed (PC-A). Nothing was started: press the button again." {
		t.Fatalf("the start after the id changed: %v", res)
	}
	if a, hist := r.s.WakeRuns.Snapshot(); a != nil || len(hist) != 0 {
		t.Fatal("a run was started")
	}
	if held, _ := r.s.tickets.held(); held != "" {
		t.Fatal("an approval is kept for a run that was not started")
	}
	if n := len(r.m["pc-a"].Passed()); n != 0 {
		t.Fatalf("%d requests got through to the machine", n)
	}
}
