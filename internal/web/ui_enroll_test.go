package web

import (
	"strings"
	"testing"
)

// L12 (UI side): Re-pair of a host that is its own Proxmox node offers "the node was renamed"; the tick goes
// into the request body, and the stored node name is shown through esc().
func TestUIRepairOffersNodeRenamed(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`<input type="checkbox" id="td-renamed"> The node was renamed: accept another Proxmox node name than <b>${esc(h.proxmox.node)}</b>`,
		`const renamed = !!$("#td-renamed")?.checked;`,
		`JSON.stringify(renamed ? { confirm: v, node_renamed: true } : { confirm: v })`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// L24 (UI side): the Add host / Re-pair dialog shows the panel's warning (a newer agent is installed than the
// release the command installs), escaped.
func TestUIEnrollDialogShowsTheWarning(t *testing.T) {
	if want := "${cr.warning ? `<div class=\"note warn\" id=\"en-warn\">${esc(cr.warning)}</div>` : \"\"}"; !strings.Contains(appJS(t), want) {
		t.Errorf("app.js lacks %q", want)
	}
}

// M3 (UI side): the box of a host added with Add host says whether its pairing was verified.
func TestUIManagedHostShowsPairing(t *testing.T) {
	if want := `<span>Pairing</span><span>${h.pairing === "v2" ? "verified: the agent proved it has the join code" : '<span class="note">made by an older panel, not verified: Re-pair to replace it with a verified pairing</span>'}</span>`; !strings.Contains(appJS(t), want) {
		t.Errorf("app.js lacks %q", want)
	}
}

// M3 (UI side): while something that is not the agent answers at the machine's address, the dialog says so in
// a box of its own (the enrollment itself keeps "waiting"), in the first view and in every refresh, escaped.
func TestUIEnrollDialogSaysWhenSomethingElseAnswers(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`unverified_answer: "Not a Fairy agent: something answers at this address, but it could not prove it has the join code. It was not added."`,
		"const enrollHint = (e) => (ENR_HINT_BOX[e.hint] ? `<div class=\"warnbox\" id=\"en-hint\" role=\"alert\">${esc(ENR_HINT_BOX[e.hint])}</div>` : \"\");",
		"${enrollSteps(cr)}${enrollHint(cr)}<div class=\"note\" id=\"en-msg\">${esc(cr.message)}</div>",
		"${enrollSteps(e)}${enrollHint(e)}<div class=\"note\" id=\"en-msg\">${esc(e.message)}",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// The result of an action the panel ran after "Confirm it's you" (for Add host: the id whose code is then
// shown) is handed out once, by the first /api/me. A page that is going away must not ask for it when its
// stream breaks, and a page that is handed it anyway shows it. (In demo mode, where the sign-in takes no time,
// the page being left took the result in about half of the runs, and the Add host dialog never opened.)
func TestUIPageThatIsLeavingDoesNotTakeThePendingResult(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`window.addEventListener("pagehide", () => { leaving = true; if (stepUpOpen && !stepUpOpen.going) cancelStepUp(); });`,
		"  leaving = false;\n  if (e.persisted && stepUpOpen) { stepUpOpen.going = false;",
		"async function streamLost() {\n  if (leaving || stepUpOpen?.going) return;",
		"async function checkAccess() {\n  if (leaving || stepUpOpen?.going) return;",
		`try { showPendingResult((await api("/api/me")).pending_result); reachable = true; }`,
		"const me = await api(\"/api/me\");\n    showPendingResult(me.pending_result);",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// NEW-1 in the page: "Continue" for a kept action is a button that posts the confirmation (the id goes in the
// body), never a link that carries the id; the dialog shows what the panel says it will run; and a refusal for
// a browser or address the session has not confirmed from is explained and goes the plain way.
func TestUIConfirmIsAPostAndShowsWhatWillRun(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`api("/api/me/step-up", { method: "POST", body: JSON.stringify({ pending_id: String(id || ""), next: nextPath }) })`,
		`<button class="btn primary" id="su-go">Continue to Pocket ID</button>`,
		"<span class=\"note\">Fairy will run</span><b id=\"su-runs\">${esc(detail.action)}</b>",
		`const newClient = !kept && detail?.not_kept === "new_client";`,
		`if (e.status === 409) {`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	for _, bad := range []string{"pending=${", "&amp;pending=", "?pending="} {
		if strings.Contains(js, bad) {
			t.Errorf("app.js still builds a link that names a kept action (%q)", bad)
		}
	}
}
