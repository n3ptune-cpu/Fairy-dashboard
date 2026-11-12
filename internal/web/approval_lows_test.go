package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// r1-b2-06 (audit 5): in the Turn on dialog the list of people who may approve broke into lines that started
// with ";": the reason of each person is a .note, and .opts .opt .note is a block.
func TestTurnOnDialogKeepsEachApproverInOneLine(t *testing.T) {
	if css := appCSS(t); !strings.Contains(css, ".opts .opt .ap-users .note { display: inline; }") {
		t.Error("the reason of an approver is a block of its own again")
	}
}

// r1-b2-07 (audit 5): a refusal after the round trip was a message of six lines that was gone after 5 seconds,
// and the Turn on results were sentences set flush right. A long refusal now shows its first sentence without
// the machine's own words; the whole text is behind "Details" and stays until it is closed.
func TestALongRefusalStaysUntilItIsClosed(t *testing.T) {
	js, css := appJS(t), appCSS(t)
	for _, want := range []string{
		"if (bad && !details) [msg, details] = flashParts(String(msg));",
		"${r.more ? infoBtn(`apr-${r.id}`, `What ${r.name} answered`, `<p>${esc(r.more)}</p>`) : \"\"}",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if !strings.Contains(css, ".list-row.ap-res > .note { text-align: left; }") {
		t.Error("the Turn on results are flush right again")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own function is not run here")
	}
	fn := regexp.MustCompile(`(?ms)^const FLASH_LONG = .*?^function flashParts\(msg\) \{.*?^\}$`).FindString(js)
	if fn == "" {
		t.Fatal("app.js has no flashParts any more")
	}
	script := `"use strict";
` + fn + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const broken = "CHARLIE cannot check approvals right now (CHARLIE says: approval unavailable: the trust file cannot be used (an administrator repairs it on the machine: fairy-agent helper-trust show)). Until that is repaired on the machine it refuses fan, power and update requests.";
const refused = "Shut down BRAVO: failed · BRAVO did not accept the approval (BRAVO says: approval refused (no_challenge): no entry holds an open challenge of this machine for exactly this request (used already, expired)). Nothing was done. A new approval is needed: press the button again.";
let [short, more] = flashParts(broken);
if (short !== "CHARLIE cannot check approvals right now." || more !== broken) fail(short);
[short, more] = flashParts(refused);
if (short !== "Shut down BRAVO: failed · BRAVO did not accept the approval." || more !== refused) fail(short);
[short, more] = flashParts("PC-A is offline");
if (short !== "PC-A is offline" || more !== null) fail("a short one");
[short, more] = flashParts("x".repeat(300));
if (short.length !== 300 || more !== null) fail("nothing to cut");
// brackets that never close (a text that was clipped): the sentence still ends before them
[short, more] = flashParts("BRAVO did not accept the approval (BRAVO says: approval refused (clock): the token was issued " + "y".repeat(200));
if (short !== "BRAVO did not accept the approval" || !more) fail("clipped: " + short);
if (!process.exitCode) console.log("ok");
`
	file := filepath.Join(t.TempDir(), "flash.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, file).CombinedOutput(); err != nil || !strings.HasPrefix(string(out), "ok") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
