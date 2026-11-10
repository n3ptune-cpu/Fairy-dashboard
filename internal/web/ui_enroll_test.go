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
