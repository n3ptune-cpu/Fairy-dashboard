package web

import (
	"regexp"
	"strings"
	"testing"
)

// The sockets of a plug on the Power page (PLUGS-DESIGN.md, 5.2): under the plug's name, in the plug's own
// order, a dot for the state and the socket's name. What hangs on a socket is the name's tooltip, when the
// viewer has that row. A socket an admin cannot switch has a lock, and the panel's sentence for why is behind
// it; somebody who is no admin gets no sentence and so no lock. A state the page does not know is "no state",
// never "on" or "off".
func TestUIPowerPageShowsTheSocketsOfAPlug(t *testing.T) {
	runPowerPageScript(t, "sockets", `
state.systems = [{ id: "desk-pc", name: "Desk PC" }];
state.byId = { "desk-pc": state.systems[0] };
const k = (id, o) => ({ id, name: id, state: "on", switchable: false, ...o });
const bar = src("relay/desk-bar", { name: "Desk powerbar", sockets: [
  k("ch1", { name: "Lamp", why: "not_allowed", note: "Not switchable. It is not in the allowed list on the machine that reads this plug." }),
  k("ch2", { name: "Printer", state: "off", why: "read_only" }),
  k("ch3", { name: "Desk PC socket", system_id: "desk-pc", lock: "panel", why: "panel", note: "Locked. This socket powers the panel." }),
  k("ch4", { name: "Gone", state: "unavailable", why: "read_only", system_id: "hidden-row" }),
  k("ch5", { name: "New", state: "unknown", why: "read_only" }),
  k("ch6", { name: "<b>x</b>", state: "constructor", why: "x", note: "<img src=x>" }) ] });
state.power = { total_w: 10, sources: [bar, src("relay/one", { name: "Plain plug" })] };
const html = pagePower();
const rows = html.split('<tr class="row">').slice(1);
if (rows.length !== 2) fail("rows: " + rows.length);
if (rows[1].includes("socks") || rows[1].includes("sock-dot")) fail("a plug without sockets has a socket line: " + rows[1]);
const socks = rows[0].split('<span class="sock" ').slice(1);
if (socks.length !== 6) fail("sockets: " + socks.length);
if (words(rows[0]).indexOf("Desk powerbar via agent relay Lamp Printer Desk PC socket Gone New") !== 0) fail("the names, in the plug's order: " + words(rows[0]));
const dot = (h) => (h.match(/<span class="sock-dot ([a-z]+)" role="img" aria-label="([^"]*)" title="([^"]*)"><\/span>/) || []).slice(1).join("|");
const want = ["on|On|On", "off|Off|Off", "on|On|On",
  "none|Home Assistant does not reach this socket right now|Home Assistant does not reach this socket right now",
  "none|Home Assistant has no state for this socket right now|Home Assistant has no state for this socket right now",
  "none|Home Assistant has no state for this socket right now|Home Assistant has no state for this socket right now"];
socks.forEach((h, i) => { if (dot(h) !== want[i]) fail("the dot of socket " + (i + 1) + ": " + dot(h) + " in " + h); });
// the lock: only with the panel's sentence, which is its name, its tooltip and the text behind it
const locks = socks.map((h) => (h.match(/<button class="info-btn sock-lock" type="button" data-info="([^"]*)" aria-label="([^"]*)" title="([^"]*)"/) || []).slice(1).join("|"));
if (locks[0] !== "sock-p:relay/desk-bar/ch1|Not switchable. It is not in the allowed list on the machine that reads this plug.|Not switchable. It is not in the allowed list on the machine that reads this plug.") fail("the lock of a socket that is not allowed: " + locks[0]);
if (locks[2] !== "sock-p:relay/desk-bar/ch3|Locked. This socket powers the panel.|Locked. This socket powers the panel.") fail("the lock of the panel's socket: " + locks[2]);
if (locks[1] || locks[3] || locks[4]) fail("a lock without a sentence: " + JSON.stringify(locks));
if (!info.html["sock-p:relay/desk-bar/ch3"].includes('<div class="info-t">Desk PC socket</div><p>Locked. This socket powers the panel.</p>')) fail("behind the lock: " + info.html["sock-p:relay/desk-bar/ch3"]);
// what hangs on it: the name's tooltip, only for a row this viewer has
if (!socks[2].includes('<span class="sock-name" title="Powers Desk PC">Desk PC socket</span>')) fail("the tooltip of a linked socket: " + socks[2]);
if (!socks[3].includes('<span class="sock-name">Gone</span>') || !socks[0].includes('<span class="sock-name">Lamp</span>')) fail("a tooltip without a row: " + socks[3] + socks[0]);
// a name and a sentence are text, never markup
if (/<b>x<\/b>|<img/.test(html) || /<img/.test(info.html["sock-p:relay/desk-bar/ch6"])) fail("markup from a socket's name or note");
if (!socks[5].includes("&lt;b&gt;x&lt;/b&gt;") || !info.html["sock-p:relay/desk-bar/ch6"].includes("&lt;img src=x&gt;")) fail("the escaped name and note: " + socks[5]);
// a socket that can be switched has no lock, whatever else its row says
bar.sockets[0] = k("ch1", { name: "Lamp", switchable: true, note: "left over" });
if (pagePower().split('<span class="sock" ')[1].includes("sock-lock")) fail("a lock on a socket that can be switched");
// sockets that are no list are not drawn
bar.sockets = "ch1";
if (pagePower().includes("sock-dot")) fail("sockets that are no list");

// the host's Power panel: one line for each socket that feeds this host, with the plug's name
bar.sockets = [k("ch3", { name: "Desk PC socket", system_id: "desk-pc", lock: "panel", why: "panel", note: "Locked. This socket powers the panel." }),
  k("ch4", { name: "Other", system_id: "other-pc" }), k("ch5", { name: "Loose" })];
state.power.sources.push(src("relay/pc-plug", { name: "PC plug", system_id: "desk-pc", sockets: [k("main", { name: "PC plug", state: "off", system_id: "desk-pc", why: "read_only" })] }),
  src("relay/dead", { name: "Dead", sockets: [k("main", { name: "Dead plug", state: "unavailable", system_id: "desk-pc", why: "read_only" })] }));
const lines = hostSockets("desk-pc").split('<div class="list-row wall-sock"').slice(1);
if (lines.length !== 3) fail("the host's socket lines: " + lines.length);
if (words("<" + lines[0]) !== "Socket · Desk PC socket Desk powerbar on") fail("a powerbar's socket: " + words("<" + lines[0]));
if (words("<" + lines[1]) !== "Socket · PC plug PC plug off") fail("a plug's only socket: " + words("<" + lines[1]));
if (words("<" + lines[2]) !== "Socket · Dead plug Dead no state") fail("a socket without a state: " + words("<" + lines[2]));
if (!lines[0].includes('data-info="sock-h:relay/desk-bar/ch3"') || lines[1].includes("sock-lock")) fail("the lock in the host's panel: " + lines[0] + lines[1]);
if (hostSockets("other-row") !== "" || hostSockets("") !== "" || hostSockets(undefined) !== "") fail("socket lines for a row nothing feeds");
state.power = null;
if (hostSockets("desk-pc") !== "") fail("socket lines without a power answer");
`)
}

// The places the sockets are drawn in, and their look: under a source's name, and in the host's Power panel
// after the wall power. The theme is square; a socket's dot is round like every status dot.
func TestUISocketsAreOnThePowerPageAndInTheHostsPanel(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`<div class="note">via agent ${esc(s.via)}</div>` + "`" + ` : ""}${sockets(s)}</td>`,
		`${wallPower(lp)}${wallMore(lp)}${hostSockets(s.id)}`,
		`A plug's sockets are under its name`,
	} {
		if strings.Count(js, want) != 1 {
			t.Errorf("app.js lacks (or has twice) %q", want)
		}
	}
	b, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	for _, want := range []string{"\n.socks {", "\n.sock {", "\n.sock-dot {", "\n.sock-dot.on {", "\n.sock-dot.none {", "\n.info-btn.sock-lock {", "\nbody.unreachable .sock-dot.on {"} {
		if strings.Count(css, want) != 1 {
			t.Errorf("app.css lacks (or has twice) %q", want)
		}
	}
	// while the panel is not reached, nothing on the page says "on" in green
	if !regexp.MustCompile(`body\.unreachable \.sock-dot\.on \{ background: var\(--off\); box-shadow: none; \}`).MatchString(css) {
		t.Error("a socket's green dot stays green while the panel is not reached")
	}
}
