package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// r1-b1-04 (audit 5, bug run): while the panel could not be reached, open pages went on showing every host
// "ONLINE · live" with green dots and no notice, for as long as the outage lasted. Now, after ten seconds
// without a word from the panel, the page asks it (GET /healthz); no answer: one strip on every page ("Panel
// not reachable · last data Ns ago"), no "live" label, grey dots, until the panel answers again.
func TestUISaysWhenThePanelCannotBeReached(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const NET_QUIET = 10000, NET_KNOCK = 5000;`,
		`fetch("/healthz", { cache: "no-store", credentials: "same-origin", signal: ac.signal })).status < 500`,
		`document.body.classList.add("unreachable");`, `document.body.classList.remove("unreachable");`,
		`data-key="net-down" role="status">${I.recheck}<b>Panel not reachable</b>`,
		`<span>last data <span data-ago="${esc(new Date(net.data).toISOString())}">${esc(ago(net.data))}</span></span></div>`,
		// every event of the stream, its opening and every answered request say that the panel is there
		`for (const ev of ["open", "systems", "systems_delta", "summary", "ups", "notes", "notes_new", "ups_event", "power"]) es.addEventListener(ev, () => netHeard(ev !== "open"));`,
		`if (res.status < 500) netHeard(false);`,
		"setInterval(() => {\n  netKnock();",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	b, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`body.unreachable .dot, body.unreachable .dot.online, body.unreachable .dot.stale, body.unreachable .dot.offline, body.unreachable .dot.error { background: var(--off); box-shadow: none; }`,
		`body.unreachable .live { display: none; }`,
		`body.unreachable .chip.ok { color: var(--muted); border-color: var(--line2); }`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("app.css lacks %q", want)
		}
	}

	t.Run("in node, with a clock", func(t *testing.T) {
		node, err := exec.LookPath("node")
		if err != nil {
			t.Skip("node is not installed: the page's own functions are not run here")
		}
		var parts []string
		for _, re := range []string{`(?m)^const NET_QUIET = .*$`, `(?m)^const net = .*$`, `(?ms)^function netHeard\(data\) \{.*?^\}$`, `(?ms)^async function netKnock\(\) \{.*?^\}$`} {
			m := regexp.MustCompile(re).FindString(js)
			if m == "" {
				t.Fatalf("app.js has no %s any more", re)
			}
			parts = append(parts, m)
		}
		script := `"use strict";
let clock = 1000000, answer = 200, asked = 0, alerts = 0, connects = 0;
Date.now = () => clock;
const classes = new Set();
const document = { body: { classList: { add: (c) => classes.add(c), remove: (c) => classes.delete(c) } } };
globalThis.fetch = async (path) => { asked++; if (path !== "/healthz") throw new Error("asked " + path); if (answer === 0) throw new Error("no answer"); return { status: answer }; };
const state = { me: { role: "admin" }, es: {}, watch: null };
let signedOut = false, leaving = false, streamRetry = null, streamBackoff = 12000;
const renderAlerts = () => { alerts++; };
const connect = () => { connects++; };
` + strings.Join(parts, "\n") + `
const fail = (m) => { console.log("FAIL " + m); process.exitCode = 1; };
const tick = async (s) => { for (let i = 0; i < s; i++) { clock += 1000; await netKnock(); } };  // the page's 1 s ticker
(async () => {
  // the stream talks: nobody is asked
  for (let i = 0; i < 30; i++) { netHeard(true); await tick(1); }
  if (asked !== 0 || net.down) fail("asked while the stream talks: " + asked);
  // the stream is quiet but the panel is there (nothing changes on a quiet panel): one knock per ten seconds, no notice
  await tick(60);
  if (asked !== 6 || net.down || classes.size || alerts) fail("a quiet panel: asked " + asked + " times in 60 s, down " + net.down);
  // the panel is gone: the notice within ten seconds of silence plus one knock, and the data's time stays
  const data = net.data;
  netHeard(false); answer = 0; asked = 0;
  await tick(9);
  if (net.down) fail("the notice came before ten seconds of silence: asked " + asked + ", heard " + (clock - net.heard) / 1000 + " s ago");
  await tick(1);
  if (!net.down || !classes.has("unreachable") || alerts !== 1 || asked !== 1) fail("after 10 s without an answer: down " + net.down + ", asked " + asked);
  if (net.data !== data) fail("the time of the last data moved while nothing came");
  await tick(30);
  if (asked !== 7 || alerts !== 1) fail("while it is down the page knocks every 5 s: " + asked + " knocks in 40 s, " + alerts + " renders");
  // a proxy in front of the panel answers 502: that is not the panel
  answer = 502; await tick(5);
  if (!net.down) fail("a 502 of the proxy took the notice away");
  // the panel is back: one knock takes the notice away, and a stream that waits for a retry connects at once
  answer = 200; state.es = null;
  await tick(5);
  if (net.down || classes.has("unreachable") || alerts !== 2 || connects !== 1 || streamBackoff !== 0) fail("back: down " + net.down + ", connects " + connects + ", backoff " + streamBackoff);
  // ... or the stream itself comes back first
  answer = 0; state.es = {}; await tick(15);
  if (!net.down) fail("down again");
  netHeard(true);
  if (net.down || classes.size || connects !== 1) fail("the stream's first event did not take the notice away");
  // signed out, on the way to Pocket ID, or before the first sign-in: the page asks nobody
  asked = 0; signedOut = true; await tick(30); signedOut = false; leaving = true; await tick(30); leaving = false; state.me = null; await tick(30);
  if (asked !== 0 || net.down) fail("asked while signed out or leaving: " + asked);
  // a 401 or 403 is the panel's own answer (the session ended): reachable
  state.me = {}; answer = 401; await tick(1);
  if (net.down || asked !== 1) fail("a 401 counted as unreachable");
  if (!process.exitCode) console.log("ok");
})();
`
		file := filepath.Join(t.TempDir(), "net.js")
		if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(node, file).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "ok" {
			t.Fatalf("node: %v\n%s", err, out)
		}
	})
}
