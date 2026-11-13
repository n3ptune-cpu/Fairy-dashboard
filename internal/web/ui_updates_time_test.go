package web

import (
	"regexp"
	"strings"
	"testing"
)

// The relative times on the Updates page ("last update OK, 1m ago", "published ..", "Last checked on GitHub ..",
// a run's "started .. / over ..", "sent ..", the panel update's "started ..") move by themselves: each one is
// written as <span data-ago="time">text</span>, and the page's one ticker rewrites every such span once a
// second from the time in it, with the function that wrote it. It is the ticker the Sessions page uses. A time
// written into this page without the span would stand still until the next reload: this test is against that.
func TestUIUpdatesPageTimesTick(t *testing.T) {
	js := appJS(t)
	// the one ticker, and the one function that words a relative time
	const ticker = `document.querySelectorAll("[data-ago]").forEach((el) => { const v = ago(el.dataset.ago); if (el.textContent !== v) el.textContent = v; });`
	if strings.Count(js, ticker) != 1 || strings.Count(js, `querySelectorAll("[data-ago]")`) != 1 {
		t.Fatal("app.js must have exactly one ticker for [data-ago]")
	}
	if i := strings.Index(js, ticker); !strings.Contains(js[i-200:i], "setInterval(() => {") || !strings.Contains(js[i:i+400], "}, 1000);") {
		t.Error("the ticker does not run once a second")
	}
	if named := regexp.MustCompile(`(?m)^(?:const|function|let) \w*[Aa]go\w*\b`).FindAllString(js, -1); len(named) != 1 || named[0] != "const ago" {
		t.Errorf("app.js must have one function for relative times (ago), not a second one: %v", named)
	}
	// everything the Updates page is made of
	fn := func(name string) string {
		from := strings.Index(js, "\nfunction "+name+"(")
		if from < 0 {
			t.Fatalf("app.js has no function %s", name)
		}
		end := strings.Index(js[from+1:], "\n}\n")
		return js[from : from+1+end]
	}
	spans := 0
	inSpan := regexp.MustCompile("<span data-ago=\"\\$\\{(?:esc\\()?([A-Za-z_.]+)\\)?\\}\">\\$\\{ago\\(([A-Za-z_.]+)\\)\\}</span>")
	for _, name := range []string{"updState", "pageUpdates", "updCheckBtn", "updCheckLine", "wuSteps", "wuHostRow", "wuRunBox", "wuLastLine", "wakeUpdateBox", "panelUpdateBox"} {
		src := fn(name)
		ticking := inSpan.FindAllStringSubmatch(src, -1)
		for _, m := range ticking {
			if m[1] != m[2] {
				t.Errorf("%s: the span's time is %s, its first text is of %s", name, m[1], m[2])
			}
		}
		spans += len(ticking)
		if n := strings.Count(src, "ago("); n != len(ticking) {
			t.Errorf("%s: %d relative times, %d of them in a <span data-ago>: the others stand still", name, n, len(ticking))
		}
		// no other way of wording "how long ago" on this page
		for _, bad := range []string{"secsSince(", "fmtUptime(", "Date.now() -"} {
			for _, line := range strings.Split(src, "\n") {
				if !strings.Contains(line, bad) {
					continue
				}
				ok := strings.Contains(line, `secsSince(at) < 10 ? "just now"`) || // the first 10 s after a check; the page is made anew every 2 s
					strings.Contains(line, "data-countdown=") || // a countdown: the ticker's other half
					strings.Contains(line, "Date.now() - new Date(st.updated_at) < 3600e3") // whether to show the steps at all
				if !ok {
					t.Errorf("%s words a time by itself (%s): %s", name, bad, strings.TrimSpace(line))
				}
			}
		}
	}
	if spans < 6 {
		t.Errorf("only %d ticking times found on the Updates page: the pattern no longer matches", spans)
	}
	// the one this was reported for
	if !strings.Contains(fn("updState"), "${su.last.ok ? \"last update OK\" : \"last update failed\"}${at ? `, <span data-ago=\"${at}\">${ago(at)}</span>` : \"\"}") {
		t.Error("\"last update OK, .. ago\" is not a ticking time")
	}
}
