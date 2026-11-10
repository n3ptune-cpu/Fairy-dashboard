package web

import (
	"regexp"
	"strings"
	"testing"
)

func appJS(t *testing.T) string {
	t.Helper()
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// I34: the page puts server values into HTML only through esc() / enc() / cssName() / safeHref(), also where the
// server only ever sends a number or a fixed word. This pins the places that used to rely on the type, and the
// four link targets that used to take any kind of address.
func TestUIDoesNotRelyOnServerTypes(t *testing.T) {
	js := appJS(t)
	for _, raw := range []string{
		`class="dot ${s.status}"`, `class="chip ${s.kind}"`, `#${s.vmid}<`, `${t.cores} cores`, `<b>${sm.counts.`, `${sm.counts.online}<small>`,
		`${j.floor_s}s`, `· ${s.cores} cores`, `${s.load_avg.join(`, `${f.rpm} rpm`, `${x.rpm} rpm`, `<td>${p.pid}</td>`, `data-key="p${p.pid}"`,
		`<b>${floor}%</b>`, `min="${floor}"`, `value="${v}" data-zone`, `">${v}%</b>`, `">${st}</span>`, `data-key="e${e.seq}"`,
		`data-key="a${e.id}"`, `data-key="n${e.id}"`, `data-id="${e.id}"`, `<option value="${r}"`, `>${ROLE_LABEL[r]}</option>`,
		`${x.hosts}</td>`, `${x.users}</td>`, `value="${s.dim}"`, `${s.dim}%`, `value="${s.blur}"`, `${s.blur}px</output>`,
		`value="${s.card_opacity}"`, `${s.card_opacity}%`, `?v=${s.background.version}`, `(est. from ${u.nominal_w} W`, `<b>${n}</b> active`,
	} {
		if strings.Contains(js, raw) {
			t.Errorf("app.js inserts a server value without escaping: %s", raw)
		}
	}
	// link targets from the server go through safeHref (http and https only)
	if m := regexp.MustCompile(`href="\$\{esc\([^)]*(url|URL)[^)]*\)\}"`).FindAllString(js, -1); len(m) != 0 {
		t.Errorf("link targets from the server must use safeHref, not esc: %v", m)
	}
	if n := strings.Count(js, `href="${safeHref(`); n != 4 {
		t.Errorf("want the 4 server link targets behind safeHref, found %d", n)
	}
	for _, want := range []string{`const safeHref = `, `x.protocol === "https:" || x.protocol === "http:"`, `const cssName = `} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}
