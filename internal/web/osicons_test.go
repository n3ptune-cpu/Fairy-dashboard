package web

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/normalise"
)

// osMarks reads static/osicons.js: the key and the path of every mark. The file may hold nothing else: comment
// lines, "use strict", the one assignment, and one `key: "path",` line per mark.
func osMarks(t *testing.T) map[string]string {
	t.Helper()
	b, err := staticFS.ReadFile("static/osicons.js")
	if err != nil {
		t.Fatal(err)
	}
	mark := regexp.MustCompile(`^  ([a-z][a-z0-9]*): "([^"]*)",$`)
	out, body := map[string]string{}, false
	for i, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		switch {
		case !body && (strings.HasPrefix(line, "// ") || line == `"use strict";`):
		case !body && line == "window.FAIRY_OS_ICONS = Object.freeze({":
			body = true
		case body && line == "});":
			body = false
		case body && mark.MatchString(line):
			m := mark.FindStringSubmatch(line)
			if _, dup := out[m[1]]; dup {
				t.Errorf("osicons.js: %s twice", m[1])
			}
			out[m[1]] = m[2]
		default:
			t.Errorf("osicons.js line %d is not a comment, the assignment or one mark: %.60q", i+1, line)
		}
	}
	return out
}

// The marks are path data and nothing else: only the letters and numbers of an SVG path (no quote, no "<", no
// "url(", no address can be in one), each starts with a move, and the whole file stays small. The page has a
// mark for every key the panel can send, and no others.
func TestOSMarksArePathsOnly(t *testing.T) {
	marks := osMarks(t)
	path := regexp.MustCompile(`^[Mm][MmLlHhVvCcSsQqTtAaZz0-9eE .,-]+$`)
	var keys []string
	for k, d := range marks {
		keys = append(keys, k)
		if !path.MatchString(d) || len(d) > 6000 {
			t.Errorf("%s: not plain path data (%d bytes): %.40q", k, len(d), d)
		}
	}
	want := slices.Clone(normalise.OSIconKeys)
	sort.Strings(keys)
	sort.Strings(want)
	if !slices.Equal(keys, want) {
		t.Errorf("osicons.js has the marks %v\nthe panel sends the keys   %v", keys, want)
	}
	if marks["windows"] != marks["windows11"] { // a Windows of an unknown version: the Windows 11 mark
		t.Error(`"windows" must be the Windows 11 mark`)
	}
	b, _ := staticFS.ReadFile("static/osicons.js")
	if len(b) > 40<<10 {
		t.Errorf("osicons.js is %d bytes", len(b))
	}
}

// The page loads the marks before app.js (same asset version, deferred, from the panel itself) and they are
// served like the other static files; the Content-Security-Policy stays as it is (TestIndexReferencesVersionedAssets).
func TestOSMarksAreLoadedByThePage(t *testing.T) {
	e := setup(t)
	v := e.s.assetV
	page := string(e.do("GET", "/", nil).raw)
	marks, app := `<script src="/static/osicons.js?v=`+v+`" defer></script>`, `<script src="/static/app.js?v=`+v+`" defer></script>`
	if i, j := strings.Index(page, marks), strings.Index(page, app); i < 0 || j < 0 || i > j {
		t.Fatal("index.html must load osicons.js before app.js", i, j)
	}
	r := e.do("GET", "/static/osicons.js?v="+v, nil)
	if r.code != 200 || !strings.Contains(r.hdr.Get("Content-Type"), "javascript") || r.hdr.Get("Cache-Control") != cacheImmutable ||
		!strings.Contains(string(r.raw), "window.FAIRY_OS_ICONS = Object.freeze({") {
		t.Fatal("osicons.js", r.code, r.hdr.Get("Content-Type"), r.hdr.Get("Cache-Control"))
	}
}

// What the page does with a host's os_icon: the mark replaces the plain glyph in the Overview row and stands
// before the name on the host's page; without one (or with a key this page has no mark for) the row keeps the
// plain glyph it always had. The name the panel sends is the mark's name and tooltip, escaped. The mark takes the
// colour of the text around it and is never coloured by the host's status.
func TestUIShowsTheSystemMark(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const o = s.os_icon, marks = window.FAIRY_OS_ICONS;`,
		`typeof o.key === "string" && Object.hasOwn(marks, o.key) ? marks[o.key] : "";`, // only a mark of this page's own file
		`fill="currentColor" role="img" aria-label="${esc(o.name)}"><title>${esc(o.name)}</title><path d="${esc(d)}"/></svg>`,
		`const hostIcon = (s) => osIcon(s, 18) || (s.kind === "node" ? I.cluster : I.machine);`, // the plain glyphs stay for an unknown system
		`<span class="host-ico">${hostIcon(s)}</span>`,                                          // the Overview row
		`<span class="dot ${cssName(s.status)}"></span>${osIcon(s, 22)}<h2>${esc(s.name)}</h2>`, // the host's page: the dot stays the status
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if strings.Contains(js, `class="os-ico ${`) || regexp.MustCompile(`os-ico[^"]*\$\{[^}]*status`).MatchString(js) {
		t.Error("the system mark must not take a class from the host's data (its colour is not a status)")
	}
	if n := strings.Count(js, "osIcon("); n != 2 {
		t.Errorf("osIcon is used in %d places, want 2 (the host's glyph and the host page's title)", n)
	}
	css, _ := staticFS.ReadFile("static/app.css")
	for _, want := range []string{".os-ico { display: block; flex: none; }", ".detail-head .dh-title .os-ico { color: var(--muted); }"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
	if regexp.MustCompile(`\.(ok|warn|bad|online|offline|stale|error)[^{]*\.os-ico|\.os-ico\.(ok|warn|bad|online|offline|stale|error)`).Match(css) {
		t.Error("app.css colours the system mark by a status")
	}
}

// The API tells the page which mark a host gets: the test panel's laptop runs Ubuntu, its Proxmox nodes are
// Proxmox VE, a guest has none, and neither has the tower, which has not answered since the panel started (so
// nothing is known about its system: the page keeps the plain glyph).
func TestSystemsCarryTheirSystemMark(t *testing.T) {
	e := setup(t)
	r := e.do("GET", "/api/systems", nil, "Cookie", e.cookie(controller(), 0))
	if r.code != 200 {
		t.Fatal(r.code, string(r.raw))
	}
	got := map[string]string{}
	for _, x := range r.body["systems"].([]any) {
		s := x.(map[string]any)
		icon, has := s["os_icon"]
		if !has {
			t.Fatalf("%v: no os_icon field", s["id"])
		}
		key := ""
		if m, ok := icon.(map[string]any); ok {
			key = m["key"].(string) + "/" + m["name"].(string)
		}
		switch s["kind"] {
		case "node":
			if key != "proxmox/Proxmox VE" {
				t.Errorf("node %v: %q", s["id"], key)
			}
		case "qemu", "lxc":
			if icon != nil {
				t.Errorf("guest %v: %v", s["id"], icon)
			}
		default:
			got[s["id"].(string)] = key
		}
	}
	if tower, known := got["tower"]; got["laptop"] != "ubuntu/Ubuntu" || !known || tower != "" {
		t.Errorf("agents: %v", got)
	}
}

// THIRD-PARTY-ICONS.md names every mark of osicons.js (its source and licence), and says what the marks are
// used for.
func TestEveryMarkIsInTheIconsNotice(t *testing.T) {
	b, err := os.ReadFile("../../THIRD-PARTY-ICONS.md")
	if err != nil {
		t.Fatal(err)
	}
	notice := string(b)
	for k := range osMarks(t) {
		if !strings.Contains(notice, "| `"+k+"` |") {
			t.Errorf("THIRD-PARTY-ICONS.md has no row for %s", k)
		}
	}
	for _, want := range []string{"trademarks of their owners", "only to show which system a host runs", "CC0 1.0", "CC-BY-SA-3.0", "CC-BY-SA-4.0", "CC-BY-SA-2.5", "CC-BY-4.0"} {
		if !strings.Contains(notice, want) {
			t.Errorf("THIRD-PARTY-ICONS.md lacks %q", want)
		}
	}
}
