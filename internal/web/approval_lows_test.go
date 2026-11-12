package web

import (
	"fmt"
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

// r1-b2-08 (audit 5): with two tabs, or after a restart of the panel while somebody was at Pocket ID, the
// answer page said "it was cancelled, it is more than 3 minutes old, or this answer was used before": neither
// of the two real causes. And "Back to Fairy" led to the Overview: the kept action, which knew the page, is
// gone. The page names the causes, and the tab itself remembers the page it left.
func TestAnApprovalWithNothingWaitingNamesTheCauses(t *testing.T) {
	a := newApprovalEnv(t)
	a.require(t, a.laptop, a.tower)
	r := a.post("/api/systems/laptop/power", a.shutdownBody())
	id, _, ok := approvalAsked(r)
	if !ok {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	// the first tab goes to Pocket ID
	go1 := a.post(stepUpPath, map[string]any{"pending_id": id, "next": "/#/system/laptop"})
	if go1.code != 200 {
		t.Fatalf("Continue: %d %s", go1.code, go1.raw)
	}
	// "another tab": a second action of the same session takes the place of the first
	r2 := a.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"})
	if _, _, ok := approvalAsked(r2); !ok {
		t.Fatalf("%d %s", r2.code, r2.raw)
	}
	back := a.iss.Approve(t, fmt.Sprint(go1.body["url"]))
	cb := a.do("GET", back.RequestURI(), nil, "Cookie", a.admin)
	if cb.code != 400 || !strings.Contains(string(cb.raw), "Another tab of yours started an action meanwhile (only one waits at a time), or the panel was restarted") ||
		!strings.Contains(string(cb.raw), "Nothing was done.") {
		t.Fatalf("the first tab's answer: %d %s", cb.code, cb.raw)
	}
	js := appJS(t)
	for _, want := range []string{
		`try { sessionStorage.setItem(BACK_KEY, JSON.stringify({ ts: Date.now(), hash: location.hash })); } catch { /* ignore */ }`,
		`if (b && !location.hash && typeof b.hash === "string" && b.hash.startsWith("#/") && Date.now() - b.ts <= 10 * 60 * 1000) history.replaceState(null, "", b.hash);`,
		"  backToLeftPage();\n  route();",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// r1-b2-09 (audit 5): the Audit page cut the details of an entry after escaping them, 64 characters into the
// escaped text: `machines=["ALPHA-OFF&qu`. Approval entries are the first that carry lists. They are cut before
// they are escaped, and a click shows all of them.
func TestAuditDetailsAreCutBeforeTheyAreEscaped(t *testing.T) {
	js := appJS(t)
	if strings.Contains(js, ").slice(0, 64)}") {
		t.Error("app.js cuts escaped text again")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the page's own function is not run here")
	}
	var parts []string
	for _, re := range []string{`(?m)^const dict = .*$`, `(?m)^const esc = .*$`, `(?m)^const audit = .*$`, `(?ms)^const AUDIT_DET = .*?^function auditDetails\(e\) \{.*?^\}$`} {
		m := regexp.MustCompile(re).FindString(js)
		if m == "" {
			t.Fatalf("app.js has no %s any more", re)
		}
		parts = append(parts, m)
	}
	script := `"use strict";
` + strings.Join(parts, "\n") + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const e = { id: 7, params: { runs: "update all agents", machines: ["ALPHA-OFF", "BRAVO-REQ", "CHARLIE-BROKEN", "DELTA"], entries: 4 } };
let h = auditDetails(e);
if (/&[a-z#0-9]*(…|<)/.test(h) || !h.includes("…</button>") || !h.includes('title="runs=update all agents · machines=[&quot;ALPHA-OFF&quot;,&quot;BRAVO-REQ&quot;,&quot;CHARLIE-BROKEN&quot;,&quot;DELTA&quot;] · entries=4"')) fail("cut: " + h);
audit.open[7] = true;
h = auditDetails(e);
if (!h.includes("&quot;DELTA&quot;] · entries=4</button>") || h.includes("…") || !h.includes('aria-expanded="true"')) fail("open: " + h);
if (auditDetails({ id: 8, params: { delay_s: 10 } }) !== '<div class="note">delay_s=10</div>' || auditDetails({ id: 9 }) !== "") fail("short");
if (!process.exitCode) console.log("ok");
`
	file := filepath.Join(t.TempDir(), "auditdet.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, file).CombinedOutput(); err != nil || !strings.HasPrefix(string(out), "ok") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// r1-b2-10 (audit 5): "Last Turn on · by nora@example.test", a run "started by nora@example.test", the Audit
// page the same, and a countdown "requested by Nora Admin": the request to the machine carried the display
// name. It carries the form of the audit log now, for a shutdown and for an update.
func TestTheMachineIsToldWhoAskedInTheAuditLogsForm(t *testing.T) {
	a := newApprovalEnv(t)
	a.selfUpdating(t)
	u := controller()
	name, mail := "Nora Admin", "nora@example.test"
	u.Name, u.Email, u.EmailVerified = &name, &mail, true
	fresh := a.cookie(u, 0)
	hdr := []string{"Cookie", fresh, "X-Requested-With", "fairy", "Content-Type", "application/json"}
	if r := a.do("POST", "/api/systems/laptop/power", a.shutdownBody(), hdr...); r.code != 200 {
		t.Fatalf("shutdown: %d %s", r.code, r.raw)
	}
	if r := a.do("POST", "/api/admin/updates/laptop", map[string]any{}, hdr...); r.code != 200 {
		t.Fatalf("update: %d %s", r.code, r.raw)
	}
	p := a.laptop.Passed()
	if len(p) != 2 || p[0].Path != "/power" || p[0].Body["requested_by"] != mail || p[1].Path != "/update" || p[1].Body["requested_by"] != mail {
		t.Fatalf("what the machine was told: %+v", p)
	}
	if e := a.auditEntries("", "power.shutdown"); len(e) != 1 || e[0]["actor"] != mail {
		t.Fatalf("the audit entry: %v", e)
	}
}
