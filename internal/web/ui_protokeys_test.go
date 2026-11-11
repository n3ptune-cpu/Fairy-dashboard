package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// r1-p3-01 (audit 5): a site named "constructor" (or "toString", "__proto__", ...: every name an admin may
// give a site is a valid one) stopped the page for everyone who could see a host of that site. The page
// grouped hosts in a plain object, and a plain object already "has" those names: groups["constructor"] was a
// function, nothing was assigned, and .push failed in every render. The same lookups by host id showed a
// host named "constructor" as "Added with Add host" and broke its menus.
//
// Every object the page looks up by a name of the server's is now one without a prototype.
func TestUIMapsByServerNamesHaveNoPrototype(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const dict = (entries) => { const d = Object.create(null);`,
		`const table = (o) => Object.freeze(Object.assign(Object.create(null), o));`,
		`const own = (o, k) => (o != null && Object.hasOwn(o, k) ? o[k] : undefined);`,
		`byId: dict()`, `upsHist: dict()`, `poll: dict()`, `caps: dict()`, `sites: dict()`, `open: dict()`, `managed: dict()`,
		`const groups = dict();`, `state.byId = dict(state.systems.map(`, `state.managed = dict((d.managed || []).map(`,
		`state.sites = dict(Object.entries(`, `upd.bySys = dict();`, `busy: dict(), bySys: dict()`, `const wu = { data: null, error: null, open: dict() };`,
		`const fanMsg = dict();`, `const sources = dict();`, `own(h.sources, s.id)`, `html: dict()`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// no map by name is made as a plain object any more, and the page's own tables are not plain either
	for _, re := range []string{`(byId|upsHist|poll|caps|sites|open|managed|busy|bySys|html): \{\}`, `Object\.fromEntries\(`, `const groups = \{\}`,
		`state\.(byId|sites|managed|poll|caps|upsHist|open) = \{`, `(?m)^const [A-Z][A-Z_]+ = \{ [a-z_]+: `} {
		if m := regexp.MustCompile(re).FindAllString(js, -1); len(m) != 0 {
			t.Errorf("app.js still has a plain object looked up by name: %v", m)
		}
	}
	if n := len(regexp.MustCompile(`(?m)^const [A-Z][A-Z_]+ = table\(\{`).FindAllString(js, -1)); n != 14 {
		t.Errorf("want the page's 14 tables made with table(), found %d", n)
	}

	t.Run("in node, with those names", func(t *testing.T) {
		node, err := exec.LookPath("node")
		if err != nil {
			t.Skip("node is not installed: the page's own functions are not run here")
		}
		// the page's own definitions, cut out of app.js: the helpers, the state, and what groups hosts by site
		var parts []string
		for _, re := range []string{`(?m)^const dict = .*$`, `(?m)^const table = .*$`, `(?m)^const own = .*$`, `(?ms)^const state = \{.*?^\};$`,
			`(?m)^const isHost = .*$`, `(?m)^const siteOf = .*$`, `(?ms)^const hostsBySite = .*?^\};$`, `(?m)^function indexSystems\(\) .*$`,
			`(?m)^const WU_STATE = .*\n.*\n.*$`, `(?m)^const ROLE_LABEL = .*$`} {
			m := regexp.MustCompile(re).FindString(js)
			if m == "" {
				t.Fatalf("app.js has no %s any more", re)
			}
			parts = append(parts, m)
		}
		script := `"use strict";
` + strings.Join(parts, "\n") + `
const names = ["constructor", "toString", "valueOf", "hasOwnProperty", "__proto__", "isPrototypeOf", "__defineGetter__", "Other", "Lab"];
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
// one host per site name; the host's id is such a name too
state.systems = names.map((n, i) => ({ id: names[names.length - 1 - i], name: "host-" + i, kind: "agent" }));
state.sites = dict(state.systems.map((s, i) => [s.id, names[i]]));
indexSystems();
const groups = hostsBySite();
if (groups.length !== names.length) fail("sites: " + groups.map(([n]) => n).join(","));
for (const n of names) {
  const g = groups.find(([site]) => site === n);
  if (!g || !Array.isArray(g[1]) || g[1].length !== 1) fail("the site " + n + ": " + JSON.stringify(g));
  if (state.byId[n]?.kind !== "agent") fail("the host with the id " + n + " is not found by its id");
}
if (groups[groups.length - 1][0] !== "Other") fail("Other is last");
// a host that is in none of the maps is in none of them, whatever its id
for (const n of names) for (const k of ["managed", "caps", "poll", "upsHist", "open"]) if (state[k][n] !== undefined) fail(k + "[" + n + "] is " + typeof state[k][n]);
if (siteOf({ id: "constructor-not-there", group: "G" }) !== "G" || siteOf({ id: "toString" }) === undefined) fail("siteOf");
// a host with such an id and no site of its own falls back to its group, not to something inherited
state.sites = dict();
if (siteOf({ id: "constructor", group: "G" }) !== "G" || siteOf({ id: "__proto__" }) !== "Other") fail("siteOf without a site: " + siteOf({ id: "constructor", group: "G" }));
// what came as JSON keeps its prototype: own() does not look there
const sources = JSON.parse('{"desk": 12, "__proto__": 7}');
if (own(sources, "constructor") !== undefined || own(sources, "desk") !== 12 || own(sources, "__proto__") !== 7 || own(null, "x") !== undefined) fail("own");
// the page's fixed tables, asked with a word they do not have
if (WU_STATE["constructor"] !== undefined || WU_STATE.off[1] !== "done · off again" || ROLE_LABEL["toString"] !== undefined || !Object.isFrozen(WU_STATE)) fail("tables");
if (Object.getPrototypeOf(state.byId) !== null || Object.getPrototypeOf(dict()) !== null) fail("prototype");
if (({}).polluted !== undefined || Object.prototype.hasOwnProperty.call(Object.prototype, "host-0")) fail("Object.prototype was changed");
if (!process.exitCode) console.log("ok " + groups.length + " sites");
`
		file := filepath.Join(t.TempDir(), "protokeys.js")
		if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(node, file).CombinedOutput()
		if err != nil || !strings.HasPrefix(string(out), "ok 9 sites") {
			t.Fatalf("node: %v\n%s", err, out)
		}
	})
}
