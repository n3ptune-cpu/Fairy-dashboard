package web

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/agent/api"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
)

// Five small flaws in texts (review of passkey-k, RK-08), each held where it is made.
func TestTheFiveTextsOfTheReview(t *testing.T) {
	js := appJS(t)
	// 1. "... or an update of its agent. the agent itself verifies it.": a sentence that began with a small
	// letter, and "– verifies it." for a machine that does not say who checks
	for _, want := range []string{
		"${a.enforced_by === \"helper\" || a.enforced_by === \"agent\" ? `It is verified by <b>${esc(approvalBy(a))}</b>.` : \"It is verified on the machine.\"}",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if strings.Contains(js, "</b> verifies it.") {
		t.Error("app.js still begins a sentence with the checker's name")
	}
	// 2. "The machine says: <its words>." got a second full stop when the machine's words end with one
	if strings.Count(js, "esc(noStop(a.error))") != 2 || strings.Contains(js, "The machine says: <b>${esc(a.error)}") || strings.Contains(js, "The machine says: ${esc(a.error)}") {
		t.Error("app.js: the machine's words are put into the panel's sentence with their own full stop")
	}
	if _, err := exec.LookPath("node"); err == nil {
		i := strings.Index(js, "const noStop = ")
		if i < 0 {
			t.Fatal("app.js has no noStop")
		}
		line := js[i : i+strings.Index(js[i:], "\n")]
		out, err := exec.Command("node", "-e", line+`
			const got = ["trust file: not readable.", "ends with two.. ", "no stop", "", null].map(noStop);
			console.log(JSON.stringify(got));`).CombinedOutput()
		if want := `["trust file: not readable","ends with two","no stop","",""]`; err != nil || strings.TrimSpace(string(out)) != want {
			t.Errorf("noStop: %s %v, want %s", out, err, want)
		}
	}
	// 3. the chip of "offline" (the machine was sent nothing) read "no answer", and the line of "no_answer"
	// (it was sent the trust; not known) read "tower: no answer": two outcomes with one word
	if !strings.Contains(js, `offline: ["bad", "offline"]`) || !strings.Contains(js, `no_answer: ["warn", "not known"]`) {
		t.Error("app.js: the chips of a Turn on's offline and no_answer")
	}
	if got := resultLine([]turnOnResult{{Name: "laptop", Outcome: "accepted"}, {Name: "tower", Outcome: "no_answer"}, {Name: "nas", Outcome: "offline"}, {Name: "pc", Outcome: "proof_refused"}}); got != "laptop: accepted; tower: not known; nas: offline; pc: proof refused" {
		t.Errorf("the line of a Turn on's results: %q", got)
	}
	// 4. the tooltip of "Switch on" for a machine with the check had two colons in a row
	if !strings.Contains(js, "h.approval ? `No run can take it for now (${h.why || \"it checks approvals itself\"})`") || strings.Contains(js, "No run can take it for now: ${h.why") {
		t.Error("app.js: the tooltip of Switch on for a machine that checks approvals itself")
	}
	// 5. "No request was sent to it" after the question itself was sent
	a := newApprovalEnv(t)
	a.require(t, a.laptop)
	a.gb.Lie("laptop", func(path string) (int, jsonx.M) {
		if path == api.PathApprovalChallenge {
			return 504, nil
		}
		return 0, nil
	})
	r := a.post("/api/systems/laptop/power", a.shutdownBody())
	if got := fmt.Sprint(r.body["detail"]); r.code != 502 || got != "laptop gave no answer to the question about an approval (HTTP 504). Nothing else was sent to it: try again." {
		t.Errorf("the question without an answer: %d %q", r.code, got)
	}
}
