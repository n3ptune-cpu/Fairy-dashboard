package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// N6: when an agent refuses a control request, its own error text is what the page shows and what the audit
// log keeps. It is a machine's text: megabytes of it, with escape sequences, line breaks and a direction
// override, used to go into both as sent. Now 300 bytes of it arrive, cleaned. (Over HTTP the answer is
// cleaned once more where it comes in, see the poller's test; this is the handlers' own bound.)
func TestAnAgentsErrorTextIsCleanedAndCut(t *testing.T) {
	e := setup(t)
	e.s.control = newRateLimiter(100)
	c := e.cookie(controller(), 0)
	hdr := []string{"Cookie", c, "X-Requested-With", "fairy"}
	e.helperPoll("laptop", "running", helperAll...)
	e.helperPoll("tower", "running", helperAll...)
	evil := "refused\x1b[31m\x00\r\nfake: line" + string(rune(0x202e)) + string(rune(0x9b)) + "2J " + strings.Repeat("é", 1<<19)
	e.b.Reply = func(string, string) (int, jsonx.M, error) { return 500, jsonx.M{"error": evil}, nil }
	for name, r := range map[string]resp{
		"helper": e.do("POST", "/api/systems/laptop/helper/stop", map[string]any{"confirm": "laptop"}, hdr...),
		"fan":    e.do("POST", "/api/systems/tower/fan/mode", map[string]any{"mode": "full"}, hdr...),
		"power":  e.do("POST", "/api/systems/laptop/power", map[string]any{"action": "reboot", "delay_s": 60, "confirm": "laptop"}, hdr...),
	} {
		detail := fmt.Sprint(r.body["detail"])
		if r.code != 424 || !strings.HasPrefix(detail, "refused  fake: line é") || len(detail) > 310 || textsafe.Clean(detail) != detail {
			t.Errorf("%s: %d, %d bytes: %.80q", name, r.code, len(detail), detail)
		}
	}
	n := 0
	for _, a := range e.auditEntries("", "") {
		out := fmt.Sprint(a["outcome"])
		if !strings.Contains(out, "refused") {
			continue
		}
		n++
		if len(out) > 340 || textsafe.Clean(out) != out {
			t.Errorf("audit %v: %d bytes: %.80q", a["action"], len(out), out)
		}
	}
	if n != 3 {
		t.Fatalf("%d audit entries with the agent's text", n)
	}
}
