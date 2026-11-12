package web

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// r3-p1-13 (audit 5, round 3). Commit 7e952ae made the limit on control requests say how long to wait ("too
// many control requests; try again in 42 s", Retry-After, a line in the audit log). The start of "Update
// unpowered hosts" (TestWakeUpdateStartAndTheLimitOnControlRequests) and five more limits still answered
// "...; slow down" with no time and no header. Every limit a signed-in person can meet now answers the same
// way.
func TestEveryLimitSaysHowLongToWait(t *testing.T) {
	e := setup(t)
	admin := e.cookie(controller(), 0) // signed in a moment ago: no "Confirm it's you" is due
	w := []string{"Cookie", admin, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	refusal := func(name string, r resp, what string) {
		t.Helper()
		m := regexp.MustCompile(`^` + what + `; try again in (\d+) s$`).FindStringSubmatch(fmt.Sprint(r.body["detail"]))
		if r.code != 429 || m == nil || r.hdr.Get("Retry-After") != m[1] {
			t.Errorf("%s: %d %s (Retry-After %q); want %q with the seconds, in the text and in Retry-After", name, r.code, strings.TrimSpace(string(r.raw)), r.hdr.Get("Retry-After"), what+"; try again in N s")
		}
	}
	settings := map[string]any{"theme": "black", "background": map[string]any{"kind": "none", "url": nil, "version": 0}, "dim": 50, "blur": 10, "card_opacity": 70}

	// changes to users and sites, and model names: one limiter (adminRL)
	e.s.adminRL = newRateLimiter(1)
	if r := e.do("POST", "/api/admin/sites", map[string]any{"name": "Attic"}, w...); r.code >= 300 {
		t.Fatalf("the first change: %d %s", r.code, r.raw)
	}
	refusal("a second site in the same minute", e.do("POST", "/api/admin/sites", map[string]any{"name": "Cellar"}, w...), "too many changes")
	refusal("Set model name", e.do("PUT", "/api/systems/laptop/custom-model", map[string]any{"custom_model": "ThinkPad"}, w...), "too many changes")

	// settings: saving, removing the own background, removing the sign-in picture (settingsRL)
	e.s.settingsRL = newRateLimiter(1)
	if r := e.do("PUT", "/api/settings", settings, w...); r.code != 200 {
		t.Fatalf("the first save: %d %s", r.code, r.raw)
	}
	refusal("a second save of the settings", e.do("PUT", "/api/settings", settings, w...), "too many settings changes")
	refusal("removing the own background", e.do("DELETE", "/api/settings/background", nil, w...), "too many settings changes")
	refusal("removing the sign-in picture", e.do("DELETE", "/api/admin/signin-wallpaper", nil, w...), "too many settings changes")

	// each refusal has its line in the audit log, as the control limit's has
	n := 0
	for _, en := range e.auditEntries("u2", "auth.denied") {
		if o, _ := en["outcome"].(string); o == "429: too many changes" || o == "429: too many settings changes" {
			n++
		}
	}
	if n == 0 {
		t.Error("the refusals are not in the audit log")
	}

	// and no answer of the panel says "slow down" any more (the two sign-out routes and the start of a run are
	// behind a confirmation of their own; they use the same two functions)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if code, _, _ := strings.Cut(line, "//"); strings.Contains(code, "slow down") {
				t.Errorf("%s:%d still answers \"slow down\": %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
