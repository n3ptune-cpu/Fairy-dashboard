package web

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/audit"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// nextMinute ends the throttle's current minute and writes its summaries, as the panel's one-minute ticker does.
func (e *env) nextMinute() {
	e.s.anonAudit.mu.Lock()
	e.s.anonAudit.window = e.s.anonAudit.window.Add(-61 * time.Second)
	e.s.anonAudit.mu.Unlock()
	e.s.auditSuppressed(e.s.anonAudit.flush(time.Now()))
}

func (e *env) auditEntries(user, action string) []map[string]any {
	entries, _ := audit.Read(e.s.Cfg.AuditLog, user, action, "", nil, 5000)
	return entries
}

// L5: every refused request of a signed-in user wrote one audit line, without limit, so one account (even one
// still waiting for access) could push the earlier history out of the log. Now the first deniedPerSub per
// subject and minute are written and the rest become one summary entry that names the subject.
func TestRefusedRequestsAreThrottledPerSubject(t *testing.T) {
	e := setup(t)
	pending := viewer // signs in without a control group: waits for access
	pc := e.cookie(pending, 0)
	for i := 0; i < 400; i++ {
		// no CSRF header: refused and audited as auth.denied
		if r := e.do("PUT", fmt.Sprintf("/api/systems/laptop/poll?n=%d", i), map[string]any{"interval_s": 5}, "Cookie", pc); r.code != 403 {
			t.Fatal(r.code, string(r.raw))
		}
	}
	denied := e.auditEntries("u1", "auth.denied")
	if len(denied) != deniedPerSub {
		t.Fatalf("400 refused requests wrote %d auth.denied entries, want %d", len(denied), deniedPerSub)
	}
	first := denied[len(denied)-1] // Read is newest first
	if first["sub"] != "u1" || first["target"] != "/api/systems/laptop/poll" || !strings.Contains(first["outcome"].(string), "missing CSRF header") {
		t.Fatal("the first refusal must be recorded as before", first)
	}
	if n := len(e.auditEntries("", "audit.suppressed")); n != 0 {
		t.Fatal("summary before the minute ended", n)
	}

	// another subject has its own budget: a flood by one account does not hide what others are refused
	admin := controller()
	ac := e.cookie(admin, 0)
	if r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, "Cookie", ac); r.code != 403 {
		t.Fatal(r.code)
	}
	if n := len(e.auditEntries("u2", "auth.denied")); n != 1 {
		t.Fatal("the other subject's refusal", n)
	}
	// what a user did is never throttled: 20 changes of their own settings are 20 entries
	for i := 0; i < 20; i++ {
		if r := e.do("PUT", "/api/settings", map[string]any{"theme": "black", "background": map[string]any{"kind": "none"}, "dim": 50, "blur": 10, "card_opacity": 70}, "Cookie", pc, "X-Requested-With", "fairy"); r.code != 200 {
			t.Fatal(r.code, string(r.raw))
		}
	}
	if n := len(e.auditEntries("u1", "settings.update")); n != 20 {
		t.Fatal("settings.update entries", n)
	}

	e.nextMinute()
	sum := e.auditEntries("", "audit.suppressed")
	if len(sum) != 1 {
		t.Fatalf("want one summary entry, got %d: %v", len(sum), sum)
	}
	p := sum[0]["params"].(map[string]any)
	if sum[0]["sub"] != "u1" || p["sub"] != "u1" || p["dropped"] != 390.0 || p["action"] != "auth.denied" ||
		p["actor"] != denied[0]["actor"] || !strings.Contains(sum[0]["outcome"].(string), "390 refused requests") {
		t.Fatal("the summary must name the subject and the number dropped", sum[0])
	}
	if got := e.auditEntries("u1", "audit.suppressed"); len(got) != 1 { // the Audit page's user filter finds it
		t.Fatal("summary not found by subject", got)
	}
	e.nextMinute()
	if n := len(e.auditEntries("", "audit.suppressed")); n != 1 {
		t.Fatal("a summary is written once", n)
	}
	// the next minute records again
	e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, "Cookie", pc)
	if n := len(e.auditEntries("u1", "auth.denied")); n != deniedPerSub+1 {
		t.Fatal("next minute", n)
	}
}

// An API token's refused requests are throttled like a user's (its subject is the token).
func TestRefusedTokenRequestsAreThrottled(t *testing.T) {
	e := setup(t)
	raw, rec, err := e.s.Tokens.Create("reader", "system", "read", "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if r := e.do("PUT", "/api/systems/laptop/poll", map[string]any{"interval_s": 5}, "Authorization", "Bearer "+raw); r.code != 403 {
			t.Fatal(r.code, string(r.raw))
		}
	}
	if n := len(e.auditEntries("token:"+rec.ID, "auth.denied")); n != deniedPerSub {
		t.Fatal("token refusals written", n)
	}
	e.nextMinute()
	sum := e.auditEntries("token:"+rec.ID, "audit.suppressed")
	if len(sum) != 1 || sum[0]["params"].(map[string]any)["dropped"] != 40.0 {
		t.Fatal(sum)
	}
}

func TestDeniedThrottleBudgets(t *testing.T) {
	th := newAuditThrottle()
	now := time.Now()
	for i := 0; i < deniedPerSub; i++ {
		if ok, _, _ := th.allowDenied("a", "A", now); !ok {
			t.Fatal("within the budget", i)
		}
	}
	for i := 0; i < 5; i++ {
		if ok, _, _ := th.allowDenied("a", "A", now); ok {
			t.Fatal("over the budget")
		}
	}
	if ok, _, _ := th.allowDenied("b", "B", now); !ok {
		t.Fatal("budgets are per subject")
	}
	if got := th.takeDenied(); got != nil {
		t.Fatal("no summary inside the minute", got)
	}
	// the anonymous budget is separate: refused requests do not use it up, and the other way round
	if ok, _, _ := th.allow("1.2.3.4", now); !ok {
		t.Fatal("anonymous entry")
	}
	th.flush(now.Add(61 * time.Second))
	got := th.takeDenied()
	if len(got) != 1 || got[0] != (deniedSummary{Sub: "a", Actor: "A", Dropped: 5}) {
		t.Fatal(got)
	}
	if th.takeDenied() != nil {
		t.Fatal("a summary is handed out once")
	}
	// more subjects than deniedKeys in one minute: the map stays bounded, the rest share one budget
	now = now.Add(2 * time.Minute)
	written := 0
	for i := 0; i < deniedKeys+500; i++ {
		if ok, _, _ := th.allowDenied(fmt.Sprintf("s%d", i), "x", now); ok {
			written++
		}
	}
	if len(th.denied) != deniedKeys+1 || written != deniedKeys+deniedPerSub {
		t.Fatal(len(th.denied), written)
	}
	th.flush(now.Add(61 * time.Second))
	got = th.takeDenied()
	if len(got) != 1 || got[0].Sub != "" || got[0].Dropped != 500-deniedPerSub {
		t.Fatal(got)
	}
}

// The target of a refused request is the path the client asked for: it is written without control characters
// and cut short.
func TestAuditTargetIsEscapedAndClipped(t *testing.T) {
	for in, want := range map[string]string{
		"/api/systems/laptop/poll":       "/api/systems/laptop/poll",
		"srv-tower":                      "srv-tower",
		"pve1/qemu/100":                  "pve1/qemu/100",
		"/api/x\nFORGED line\r\t\x1b[2J": "/api/x%0AFORGED line%0D%09%1B[2J",
		"a\u2028b\u202ec\u0085d\x7f":     "a%E2%80%A8b%E2%80%AEc%C2%85d%7F",
		"bad\xff\xc3utf8":                "bad%FF%C3utf8",
		"Büro größe 100%":                "Büro größe 100%",
		"":                               "",
	} {
		if got := safeTarget(in); got != want {
			t.Errorf("safeTarget(%q) = %q, want %q", in, got, want)
		}
	}
	long := safeTarget("/api/" + strings.Repeat("a", 5000))
	if len(long) > maxTarget+len("…") || !strings.HasSuffix(long, "…") {
		t.Fatal(len(long), long)
	}
	if got := safeTarget(strings.Repeat("\n", 5000)); len(got) > maxTarget+3+len("…") {
		t.Fatal(len(got))
	}

	e := setup(t)
	c := e.cookie(viewer, 0)
	// the audit page is for admins: a pending user's request is refused and audited with the path asked for
	path := "/api/systems/x%0AFORGED%20entry%0D%09%1B%5B2J" + strings.Repeat("y", 3000) + "/poll"
	if r := e.do("PUT", path, map[string]any{"interval_s": 5}, "Cookie", c); r.code != 403 {
		t.Fatal(r.code, string(r.raw))
	}
	got := e.auditEntries("u1", "auth.denied")
	if len(got) != 1 {
		t.Fatal(got)
	}
	target := got[0]["target"].(string)
	if !strings.HasPrefix(target, "/api/systems/x%0AFORGED entry%0D%09%1B[2Jyyy") || len(target) > maxTarget+len("…") ||
		strings.ContainsAny(target, "\n\r\t\x1b") {
		t.Fatalf("%q", target)
	}
}

// NEW-4: two entries were outside the throttle. A user who is still waiting for access wrote one audit line
// per DELETE /api/settings/background (no rate limit), and an admin session with an old sign-in wrote one
// "auth.step_up required" line per refused control request (written before the rate limit).
func TestBackgroundDeleteAndStepUpRequiredAreLimited(t *testing.T) {
	e := setup(t)
	pend := e.cookie(viewer, 0) // role: pending
	const n = 300
	limited := 0
	for i := 0; i < n; i++ {
		switch r := e.do("DELETE", "/api/settings/background", nil, "Cookie", pend, "X-Requested-With", "fairy"); r.code {
		case 200:
		case 429:
			limited++
		default:
			t.Fatal(r.code, string(r.raw))
		}
	}
	if got := len(e.auditEntries("u1", "settings.background_delete")); got != 30 || limited != n-30 {
		t.Fatalf("%d of %d settings.background_delete entries written, %d requests limited (want 30 and %d)", got, n, limited, n-30)
	}
	stale := e.staleCookie(controller(), time.Hour)
	e.signIn(controller())
	for i := 0; i < n; i++ {
		if r := e.poll(stale); !stepUpRequired(r) {
			t.Fatal(r.code, string(r.raw))
		}
	}
	if got := len(e.auditEntries("u2", "auth.step_up")); got != deniedPerSub {
		t.Fatalf("%d of %d auth.step_up entries written, want %d", got, n, deniedPerSub)
	}
	e.nextMinute()
	sum := e.auditEntries("u2", "audit.suppressed")
	if len(sum) != 1 || !strings.Contains(sum[0]["outcome"].(string), fmt.Sprintf("%d refused requests", n-deniedPerSub)) {
		t.Fatal("the dropped entries must be summed up for the subject:", sum)
	}
	// the confirmation itself is always on record: what was asked for, and that it ran
	a := arm(t)
	for i := 0; i < 40; i++ {
		a.e.poll(a.sess) // 40 more refusals in the same minute
	}
	id := a.shutdown()
	if r := a.confirm(id, false); r.code != 303 || a.powerCalls() != 1 {
		t.Fatal(r.code, a.powerCalls())
	}
	out := auditOutcomes(a.e.s.Cfg.AuditLog)
	for _, want := range []string{"auth.step_up redirected to Pocket ID", "auth.step_up ok", "power.shutdown ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("a flood of refusals hid %q", want)
		}
	}
}

// The class behind NEW-4: no write route may let one account write audit entries without a limit. Every write
// of the route matrix is sent 150 times by a user waiting for access, by a viewer and by an admin whose
// sign-in is old; none may add more lines than a rate limit or the refusal throttle allows in a minute.
func TestNoRouteWritesAuditEntriesWithoutALimit(t *testing.T) {
	const tries, most = 150, 45 // the widest limit on an audited write is 30 a minute, plus the refusals' 10
	lines := func(e *env) int {
		b, _ := os.ReadFile(e.s.Cfg.AuditLog)
		return strings.Count(string(b), "\n")
	}
	for _, wr := range matrixRoutes {
		method, _, _ := strings.Cut(wr.pattern, " ")
		for _, who := range []string{"a user waiting for access", "a viewer", "an admin whose sign-in is old"} {
			e := setup(t)
			e.helperPoll("laptop", "running", helperAll...)
			_ = e.s.Access.EnsureSites([]string{"Home"})
			admin := e.cookie(controller(), 0)
			name, _ := e.do("GET", "/api/systems/laptop", nil, "Cookie", admin).body["name"].(string)
			q := probe{method: method, path: wr.path, body: wr.body, ctype: wr.ctype}
			switch wr.body {
			case "POWER":
				q.body = map[string]any{"action": "shutdown", "delay_s": 10, "confirm": name}
			case "CONFIRM":
				q.body = map[string]any{"confirm": name}
			case "SETTINGS":
				q.body = map[string]any{}
			}
			var ck string
			switch who {
			case "a user waiting for access":
				ck = e.cookie(model.User{Sub: "u8", Name: model.P("Pat"), Groups: []string{"viewers"}}, 0)
			case "a viewer":
				ck = e.cookie(model.User{Sub: "u7", Name: model.P("Vee"), Groups: []string{"viewers"}}, 0)
				if _, _, err := e.s.Access.SetRole("u7", "viewer", []string{"Home"}, "test", false); err != nil {
					t.Fatal(err)
				}
			default:
				ck = e.staleCookie(controller(), time.Hour)
			}
			before := lines(e)
			for i := 0; i < tries; i++ {
				e.raw(q, "Cookie", ck, "X-Requested-With", "fairy")
			}
			if got := lines(e) - before; got > most {
				t.Errorf("%s, %s: %d requests wrote %d audit lines (at most %d)", wr.pattern, who, tries, got, most)
			}
		}
	}
}
