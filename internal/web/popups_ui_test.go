package web

import (
	"regexp"
	"strings"
	"testing"
)

// The page's part, pinned in its source (the browser checks are in the report): where the pop-ups are in the
// page, that an agent's words go in as text only, that a pop-up never takes the focus, the limits, the
// settings section, and the stylesheet's promises.
func TestUIPopups(t *testing.T) {
	js := appJS(t)
	index, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(index)
	box := `<div id="toasts" class="toasts" role="status" aria-live="polite" aria-atomic="false" aria-relevant="additions text" aria-label="New notifications" hidden></div>`
	if !strings.Contains(page, box) || strings.Index(page, box) < strings.Index(page, `id="shell"`) || strings.Index(page, box) < strings.Index(page, `id="login"`) {
		t.Error("index.html lacks the pop-ups' place (a polite live region, hidden, outside the sign-in view)")
	}
	start := strings.Index(js, "// ------------------------------------------------------------ pop-ups for new notifications")
	settings := strings.Index(js, "function popupsPanel() {")
	end := strings.Index(js, "// ------------------------------------------------------------ Add host (admins)")
	if start < 0 || settings < start || end < settings {
		t.Fatal("app.js: the pop-up section is not where this test looks for it")
	}
	sec := js[start:settings] // everything that shows a pop-up
	for _, never := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "createContextualFragment", "DOMParser", "esc("} {
		if strings.Contains(sec, never) {
			t.Errorf("the pop-ups must be built with createElement and textContent only: found %q", never)
		}
	}
	if m := regexp.MustCompile(`<[a-zA-Z/!]`).FindAllString(sec, -1); len(m) != 0 {
		t.Errorf("the pop-up code holds markup (%v): it must build its elements with createElement", m)
	}
	for _, want := range []string{
		`const POP_MAX = 3, POP_WAIT = 5, POP_STALE_MS = 60000;`,
		`const s = document.createElement("span"); s.className = cls; s.textContent = text; return s;`, // every text of a pop-up
		`a.href = href; a.title = text;`,
		`if (pop.after === null || d.last < pop.after) { pop.after = d.last; return; }`,  // where the page starts: nothing from before
		`.filter((e) => Number.isSafeInteger(e?.id) && e.id > pop.after);`,               // nothing twice
		`const popOpen = () => !!state.me && state.me.role !== "pending" && !signedOut;`, // never on the sign-in view
		`const popPaused = () => pop.hover || pop.focus || document.hidden;`,
		`if (pop.wait.length > POP_WAIT) {`,
		`if (e.key !== "Escape" || !all.length || e.cancelBubble) return;`,        // Escape is the info panel's first,
		`if (!mine && (state.menu || acctOpen() || !$("#modal").hidden)) return;`, // and a menu's or a dialog's
		`box.classList.toggle("behind", `,
	} {
		if !strings.Contains(sec, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// the only focus() moves on from a pop-up the user closed with the keyboard; none is called when one arrives
	if n := strings.Count(sec, ".focus()"); n != 1 || !strings.Contains(sec, `const box = $("#toasts"), focused = byKey && it.el.contains(document.activeElement);`) ||
		!strings.Contains(sec, `if (focused && all.length) all[all.length - 1].el.querySelector(".toast-x").focus();`) {
		t.Errorf("a pop-up must never take the focus: %d focus() calls", n)
	}
	for _, want := range []string{
		`stepUpOpen = null; state.menu = null; closeInfo(false); popClear();`, // signed out: none stays
		`es.addEventListener("notes_new", (e) => onNewNotes(JSON.parse(e.data)));`,
		`pop.after !== null ? ` + "`notes_after=${enc(pop.after)}`",
		`state.settings.popups = popPrefs();`,
		`<h3>Pop-ups${infoBtn("set-pop", "Pop-ups", infoList([`,
		`<input type="range" id="pop-secs" min="3" max="30" step="1" value="${esc(p.seconds)}"`,
		`<option value="critical" ${p.level === "critical" ? "selected" : ""}>Critical only</option>`,
		`<button class="btn" type="button" id="pop-test" ${off}>Show a test pop-up</button>`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// the settings section has no paragraph of its own: the explanation is behind the info button
	if set := js[settings:end]; strings.Contains(set[strings.Index(set, `]))}</h3>`):], `class="note`) {
		t.Error("the Pop-ups section must not carry an explanatory paragraph")
	}
	cssB, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssB)
	for _, want := range []string{
		`.toasts { position: fixed; right: 20px; bottom: 20px; z-index: 52;`, // over a dialog's backdrop (50), under the test banner (1000)
		`.toasts.behind { z-index: 45; }`,                                    // under an open dialog
		`max-height: calc(100vh - var(--banner-h) - 28px);`,                  // never as high as the banner
		`@media (prefers-reduced-motion: reduce) { .toast { animation: none; } }`,
		`@media (max-width: 760px) { .toasts { left: 8px; right: 8px; bottom: 8px; width: auto; }`,
		`.modal { position: fixed; inset: 0; z-index: 50;`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
}
