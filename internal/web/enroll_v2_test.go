package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/enrollment"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// M3 through the API. The pairing answer is faked at the manager's seam here; the exchange itself is tested in
// internal/enrollment (panel half) and internal/agentcontract (against the real agent handler).

// A Fairy-agent release without pairing v2 is refused: 409, no code, no command, audited.
func TestAddHostRefusesAReleaseWithoutPairingV2(t *testing.T) {
	e := setup(t)
	e.s.control = newRateLimiter(100)
	e.s.Enroll.PanelAddr = "100.88.109.47"
	old := testInstallers
	old.PairV2SH, old.PairV2PS1 = false, false
	e.s.Enroll.Scripts = old
	hdr := []string{"Cookie", e.cookie(controller(), 0), "X-Requested-With", "fairy"}
	for _, os := range []string{"linux", "windows"} {
		r := e.do("POST", "/api/admin/hosts", map[string]any{"name": "box-" + os, "os": os}, hdr...)
		if r.code != 409 || !strings.Contains(string(r.raw), "is too old for this panel") || !strings.Contains(string(r.raw), "pairing v2") ||
			r.body["code"] != nil || r.body["command"] != nil {
			t.Fatal(os, r.code, string(r.raw))
		}
	}
	if len(e.s.Enroll.List()) != 0 {
		t.Fatal("an enrollment was made")
	}
	if !strings.Contains(auditOutcomes(e.s.Cfg.AuditLog), "host.add refused: Fairy-agent release build-7 (Fairy-agent 7-6155fe6) is too old for this panel") {
		t.Fatal(auditOutcomes(e.s.Cfg.AuditLog))
	}
}

// What the dialog and the audit log show while an old agent or an impostor answers, and after a verified
// pairing. The enrollment is never ended by those answers, and the same code pairs afterwards.
func TestAddHostHintsAuditAndVerifiedMark(t *testing.T) {
	e := setup(t)
	e.s.Enroll.Interval = 10 * time.Millisecond
	e.s.control = newRateLimiter(100)
	e.s.Enroll.PanelAddr = "100.88.109.47"
	var phase atomic.Int32 // 0: an agent from before v2, 1: an impostor, 2: the real agent
	var keys atomic.Value
	e.s.Enroll.Pair = func(_ context.Context, _ string, key []byte) (int, enroll.Response, string, error) {
		if prev, _ := keys.Load().(string); prev != "" && prev != string(key) {
			t.Error("the pairing key changed during one enrollment")
		}
		keys.Store(string(key))
		switch phase.Load() {
		case 0:
			return 400, enroll.Response{}, "bad_request", nil
		case 1:
			return 200, enroll.Response{}, "", &enrollment.Unverified{Why: "it is not a pairing v2 answer"}
		}
		return 200, enroll.Response{AgentVersion: "20-abcdef0", Hostname: "BOX-REAL", Platform: "linux", Token: "real-read-token-xxxxxxxxxxxxxxxxxxxx",
			ControlToken: "real-control-token-yyyyyyyyyyyyyyyyy"}, "", nil
	}
	e.b.Mu.Lock()
	e.b.Metrics["box"] = func() (jsonx.M, error) { return testutil.AgentMetrics(), nil }
	e.b.Mu.Unlock()
	c := e.cookie(controller(), 0)
	r := e.do("POST", "/api/admin/hosts", map[string]any{"name": "box", "os": "linux", "options": map[string]any{"power_control": true}},
		"Cookie", c, "X-Requested-With", "fairy")
	if r.code != 201 || !strings.Contains(r.body["command"].(string), " --pair-v2") {
		t.Fatal(r.code, string(r.raw))
	}
	id, code := r.body["id"].(string), r.body["code"].(string)
	get := func(want func(map[string]any) bool) map[string]any {
		t.Helper()
		var b map[string]any
		for i := 0; i < 300; i++ {
			time.Sleep(15 * time.Millisecond)
			e.s.Poller.PollOnce(context.Background())
			if b = e.do("GET", "/api/admin/enrollments/"+id, nil, "Cookie", c).body; want(b) {
				return b
			}
		}
		t.Fatalf("never reached: %v", b)
		return nil
	}
	b := get(func(b map[string]any) bool { return b["hint"] == "agent_too_old" })
	if b["state"] != "waiting" || b["done"] != false || !strings.Contains(b["message"].(string), "too old for this panel") || b["pairing"] != nil {
		t.Fatal(b)
	}
	phase.Store(1)
	b = get(func(b map[string]any) bool { return b["hint"] == "unverified_answer" })
	if b["state"] != "waiting" || b["done"] != false {
		t.Fatal(b)
	}
	// the owner is told plainly, in the answer the dialog shows: what answered is not the agent, where, why,
	// that nothing was added, and what to do. No code and no key in it.
	msg := b["message"].(string)
	for _, want := range []string{"not added: something answers at box, but it is not a Fairy agent that has this join code", "(it is not a pairing v2 answer)",
		"Nothing it sent was used", "check the name and the address", "make a new code"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message lacks %q: %s", want, msg)
		}
	}
	if norm, _ := enroll.Normalize(code); strings.Contains(string(must(json.Marshal(b))), code) || strings.Contains(string(must(json.Marshal(b))), norm) {
		t.Fatal("the join code is in the enrollment's state:", b)
	}
	if _, err := os.Stat(filepath.Join(e.s.Cfg.DataDir, "managed-hosts.yaml")); !os.IsNotExist(err) {
		t.Fatal("something was stored before a verified answer", err)
	}
	if _, ok := e.s.Poller.AgentByID("box"); ok {
		t.Fatal("the host is polled before a verified answer")
	}
	phase.Store(2)
	b = get(func(b map[string]any) bool { return b["done"] == true })
	if b["state"] != "online" || b["pairing"] != "v2" || b["hint"] != nil || b["agent_hostname"] != "BOX-REAL" ||
		!strings.Contains(b["message"].(string), "(it calls itself BOX-REAL, at box)") {
		t.Fatal(b)
	}
	page := e.do("GET", "/api/admin/hosts", nil, "Cookie", c)
	if m := page.body["managed"].([]any); len(m) != 1 || m[0].(map[string]any)["pairing"] != "v2" {
		t.Fatal(string(page.raw))
	}
	out := auditOutcomes(e.s.Cfg.AuditLog)
	for _, want := range []string{"host.pair agent_too_old: http://box:9101 answered 400 to a pairing v2 request", "host.pair unverified: an answer from http://box:9101 did not prove the join code",
		"host.pair paired: 20-abcdef0", "host.pair online"} {
		if strings.Count(out, want) != 1 {
			t.Errorf("the audit log has %q %d times:\n%s", want, strings.Count(out, want), out)
		}
	}
	raw := string(must(os.ReadFile(e.s.Cfg.AuditLog)))
	norm, _ := enroll.Normalize(code)
	if !strings.Contains(raw, `"pairing":"v2"`) || !strings.Contains(raw, `"agent_hostname":"BOX-REAL"`) || strings.Contains(raw, code) || strings.Contains(raw, norm) || strings.Contains(raw, "real-read-token") ||
		strings.Contains(raw, keys.Load().(string)) {
		t.Fatal("the audit log lacks the verified mark, or holds a secret")
	}
}

// The join code is revealed (once, after a step-up) to the browser session that made it: an API token is
// refused before anything is looked up.
func TestJoinCodeIsNotRevealedToAPITokens(t *testing.T) {
	e := setup(t)
	e.s.Enroll.PanelAddr = "100.88.109.47"
	e.s.control = newRateLimiter(100)
	e.s.Enroll.Pair = func(ctx context.Context, _ string, _ []byte) (int, enroll.Response, string, error) {
		<-ctx.Done()
		return 0, enroll.Response{}, "", ctx.Err()
	}
	c := e.cookie(controller(), 0)
	r := e.do("POST", "/api/admin/hosts", map[string]any{"name": "box", "os": "linux"}, "Cookie", c, "X-Requested-With", "fairy")
	if r.code != 201 {
		t.Fatal(r.code, string(r.raw))
	}
	id := r.body["id"].(string)
	cr, _ := e.s.Enroll.Get(id)
	e.s.Enroll.Hold(enrollment.Created{Enrollment: cr, Code: r.body["code"].(string), Command: r.body["command"].(string)})
	ctl, _, _ := e.s.Tokens.Create("ops", "system", "control", "cli:test", 0)
	t1 := e.do("GET", "/api/admin/enrollments/"+id+"?reveal=1", nil, "Authorization", "Bearer "+ctl)
	if t1.code != 403 || strings.Contains(string(t1.raw), r.body["code"].(string)) || t1.body["command"] != nil {
		t.Fatal(t1.code, string(t1.raw))
	}
	// the progress itself stays readable for a token of an admin, without the code
	if p := e.do("GET", "/api/admin/enrollments/"+id, nil, "Authorization", "Bearer "+ctl); p.code != 200 || p.body["code"] != nil || p.body["command"] != nil {
		t.Fatal(p.code, string(p.raw))
	}
	// the browser still gets it, once
	if b := e.do("GET", "/api/admin/enrollments/"+id+"?reveal=1", nil, "Cookie", c); b.code != 200 || b.body["code"] != r.body["code"] {
		t.Fatal(b.code, string(b.raw))
	}
	if b := e.do("GET", "/api/admin/enrollments/"+id+"?reveal=1", nil, "Cookie", c); b.code != 404 {
		t.Fatal("revealed twice", b.code)
	}
	e.s.Enroll.Cancel(id)
}
