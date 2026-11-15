package web

import (
	"regexp"
	"strings"
	"testing"
)

// The sockets of a plug on the Power page (PLUGS-DESIGN.md, 5.2): under the plug's name, in the plug's own
// order, a sign and the socket's name. What hangs on a socket is the name's tooltip, when the viewer has that
// row. A socket an admin cannot switch has a lock where the switch would be, and the panel's sentence for why
// is behind it; then a dot for the state. Somebody who is no admin gets no sentence, and so no lock: the dot
// alone. A state the page does not know is "no state", never "on" or "off".
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
// the lock is in the switch's place: in front of the dot and the name
if (!/^[^>]*><button class="info-btn sock-lock"[^>]*><\/button><span class="sock-dot on"[^>]*><\/span><span class="sock-name">Lamp<\/span><\/span>/.test(socks[0])) fail("the order of lock, dot and name: " + socks[0]);
if (!/^[^>]*><span class="sock-dot off"[^>]*><\/span><span class="sock-name">Printer<\/span><\/span>/.test(socks[1])) fail("a socket of somebody who is no admin: " + socks[1]);
if (!info.html["sock-p:relay/desk-bar/ch3"].includes('<div class="info-t">Desk PC socket</div><p>Locked. This socket powers the panel.</p>')) fail("behind the lock: " + info.html["sock-p:relay/desk-bar/ch3"]);
// what hangs on it: the name's tooltip, only for a row this viewer has
if (!socks[2].includes('<span class="sock-name" title="Powers Desk PC">Desk PC socket</span>')) fail("the tooltip of a linked socket: " + socks[2]);
if (!socks[3].includes('<span class="sock-name">Gone</span>') || !socks[0].includes('<span class="sock-name">Lamp</span>')) fail("a tooltip without a row: " + socks[3] + socks[0]);
// a name and a sentence are text, never markup
if (/<b>x<\/b>|<img/.test(html) || /<img/.test(info.html["sock-p:relay/desk-bar/ch6"])) fail("markup from a socket's name or note");
if (!socks[5].includes("&lt;b&gt;x&lt;/b&gt;") || !info.html["sock-p:relay/desk-bar/ch6"].includes("&lt;img src=x&gt;")) fail("the escaped name and note: " + socks[5]);
// a socket that can be switched has no lock, whatever else its row says
bar.sockets[0] = k("ch1", { name: "Lamp", switchable: true, tag: "t", note: "left over" });
if (pagePower().split('<span class="sock" ')[1].includes("sock-lock")) fail("a lock on a socket that can be switched");
bar.sockets[0] = k("ch1", { name: "Lamp", switchable: true, note: "left over" });  // "switchable" without a tag: no switch, and no reason that is true
if (/sock-lock|class="switch/.test(pagePower().split('<span class="sock" ')[1])) fail("a row that says switchable and has no tag: " + pagePower().split('<span class="sock" ')[1]);
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
if (words("<" + lines[0]) !== "Desk PC socket · Desk powerbar on") fail("a powerbar's socket: " + words("<" + lines[0]));
if (words("<" + lines[1]) !== "PC plug off") fail("a plug's only socket: " + words("<" + lines[1]));
if (words("<" + lines[2]) !== "Dead plug · Dead no state") fail("a socket without a state: " + words("<" + lines[2]));
if (!lines[0].includes('data-info="sock-h:relay/desk-bar/ch3"') || lines[1].includes("sock-lock")) fail("the lock in the host's panel: " + lines[0] + lines[1]);
// the socket's name, then the plug's, muted; on the right the lock (or, for somebody without one, the dot), then the word
if (!lines[0].includes('<span class="sock-what"><span class="sock-name">Desk PC socket</span><span class="note">· Desk powerbar</span></span><span class="sock"><button class="info-btn sock-lock"') ||
  !/<\/button><span class="sock-word">on<\/span><\/span><\/div>$/.test(lines[0]) || lines[0].includes("sock-dot")) fail("a locked socket's line: " + lines[0]);
if (!lines[1].includes('<span class="sock-what"><span class="sock-name">PC plug</span></span><span class="sock"><span class="sock-dot off"') || !lines[1].endsWith('<span class="sock-word">off</span></span></div>')) fail("a plug's only socket, for somebody who is no admin: " + lines[1]);
if (/<b>|<img/.test(hostSockets("desk-pc"))) fail("markup in the host's lines");
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

// The switch (PLUGS-DESIGN.md, 4.3 and 4.7): where the panel says an admin can switch a socket, and gave the
// tag of its row, the sign is the panel's switch (.switch): a button with role="switch", aria-checked for what
// the machine reported, the socket's name and state as its name, and what a click asks for as its tooltip. A
// socket without a state has a lock in its place that says so. Nothing else on the page is a switch: not a row
// without a tag, not one whose "switchable" is anything but true. While a request is on its way the switch is
// disabled and has the spinner; it never shows a state the machine did not report.
func TestUISocketSwitchIsDrawnOnlyWhereThePanelSaysSo(t *testing.T) {
	runPowerPageScript(t, "switch-draw", `
const k = (id, o) => ({ id, name: id, state: "on", switchable: true, tag: "t-" + id, ...o });
const bar = src("relay/desk-bar", { name: "Desk powerbar", sockets: [
  k("ch1", { name: "Lamp" }), k("ch2", { name: "Printer", state: "off" }), k("ch3", { name: "Gone", state: "unavailable" }), k("ch4", { name: "New", state: "weird" }),
  k("ch5", { tag: "" }), k("ch6", { tag: undefined }), k("ch7", { switchable: "true" }), k("ch8", { switchable: 1 }), k("ch9", { switchable: false, why: "read_only" }),
  k("c<10", { name: '"<b>q</b>', state: "off" }) ] });
state.power = { total_w: 1, sources: [bar] };
const socks = pagePower().split('<span class="sock" ').slice(1);
if (socks.length !== 10) fail("sockets: " + socks.length);
const sw = (h) => (h.match(/<button class="switch( busy)?" type="button" role="switch" aria-checked="(true|false)" data-act="sock" data-source="([^"]*)" data-socket="([^"]*)" data-to="(on|off)" title="([^"]*)" aria-label="([^"]*)"( disabled)?>(<span class="spinner"><\/span>)?<\/button>/) || []).slice(1).map((x) => x || "").join("|");
if (sw(socks[0]) !== "|true|relay/desk-bar|ch1|off|On. Click to switch off.|Lamp: on||") fail("a socket that is on: " + socks[0]);
if (sw(socks[1]) !== "|false|relay/desk-bar|ch2|on|Off. Click to switch on.|Printer: off||") fail("a socket that is off: " + socks[1]);
// the switch is the sign: no dot beside it, and the name follows it
if (socks[0].includes("sock-dot") || !/<\/button><span class="sock-name">Lamp<\/span><\/span>/.test(socks[0])) fail("a dot beside a switch: " + socks[0]);
for (const [i, word] of [[2, "Home Assistant does not reach this socket right now"], [3, "Home Assistant has no state for this socket right now"]]) {
  if (!socks[i].includes('<button class="info-btn sock-lock" type="button" data-info="sock-p:relay/desk-bar/ch' + (i + 1) + '" aria-label="' + word + '" title="' + word + '"') ||
    !socks[i].includes('<span class="sock-dot none"') || /class="switch|data-act="sock"|role="switch"/.test(socks[i])) fail("a socket without a state: " + socks[i]);
}
for (const i of [4, 5, 6, 7, 8]) if (/class="switch|data-act|role="switch"|sock-lock/.test(socks[i]) || !socks[i].includes('<span class="sock-dot on"')) fail("a switch the panel did not offer (socket " + (i + 1) + "): " + socks[i]);
if (sw(socks[9]) !== "|false|relay/desk-bar|c&lt;10|on|Off. Click to switch on.|&quot;&lt;b&gt;q&lt;/b&gt;: off||") fail("ids and names are text: " + socks[9]);
if ((pagePower().match(/data-to="/g) || []).length !== 3 || /data-to="(?!on"|off")/.test(pagePower()) || /toggle/i.test(pagePower())) fail("a direction that is not on or off");
// while a request for a socket is on its way, its switch takes no second click
sock.busy["relay/desk-bar/ch1"] = true;
const busy = pagePower().split('<span class="sock" ').slice(1);
if (sw(busy[0]) !== ' busy|true|relay/desk-bar|ch1|off|On. Click to switch off.|Lamp: on| disabled|<span class="spinner"></span>' || /disabled|spinner|busy/.test(sw(busy[1]))) fail("the switch of a socket that is being switched: " + busy[0]);
// the host's panel has the same switch
bar.sockets = [k("ch1", { name: "Lamp", system_id: "desk-pc" })];
delete sock.busy["relay/desk-bar/ch1"];
if (!/<span class="sock"><button class="switch" type="button" role="switch" aria-checked="true" data-act="sock" data-source="relay\/desk-bar" data-socket="ch1" data-to="off"[^>]*><\/button><span class="sock-word">on<\/span><\/span>/.test(hostSockets("desk-pc"))) fail("the switch in the host's panel: " + hostSockets("desk-pc"));
`)
}

// A click on a switch: one request, POST /api/power/socket, with the plug, the socket, the direction the
// button showed and the tag of the row as the page has it now. Nothing else is in it, and the direction is "on"
// or "off" or nothing is sent. The first answer of a real panel is "Confirm it's you" (the kept action is run
// by the panel after the passkey); every sentence shown is the panel's.
func TestUISocketSwitchSendsOneExactRequest(t *testing.T) {
	runPowerPageScript(t, "switch-send", `
const k = (id, o) => ({ id, name: id, state: "on", switchable: true, tag: "t-" + id, ...o });
const bar = src("relay/desk-bar", { name: "Desk powerbar", sockets: [k("ch1", { name: "Lamp" }), k("ch2", { name: "Printer", state: "off" }),
  k("ch3", { name: "Locked", switchable: false, why: "panel" }), k("ch4", { name: "No tag", tag: "" })] });
state.power = { total_w: 1, sources: [bar, src("relay/other", { sockets: [k("ch1", { name: "Other" })] })] };
const click = (data, o = {}) => { const t = { dataset: data, disabled: false, ...o }; clicks.forEach((fn) => fn({ target: { closest: (sel) => (sel === '[data-act="sock"]' && data.act === "sock" ? t : null) } })); };
const tick = () => new Promise((r) => setTimeout(r, 0));
(async () => {
  if (clicks.length !== 1) fail("click listeners of the sockets: " + clicks.length);
  // 1. the request, and "Confirm it's you" in answer
  answer = async () => { const e = new Error("step-up"); e.stepUp = true; e.detail = { code: "step_up_required", pending: true, pending_id: "p1" }; throw e; };
  click({ act: "sock", source: "relay/desk-bar", socket: "ch1", to: "off" });
  if (sent.length !== 1) fail("requests after one click: " + sent.length);
  const [path, opts, label] = sent[0];
  if (path !== "/api/power/socket" || opts.method !== "POST" || opts.body !== '{"source":"relay/desk-bar","socket":"ch1","to":"off","tag":"t-ch1"}') fail("the request: " + path + " " + JSON.stringify(opts));
  if (label !== "Switch off Lamp (Desk powerbar)") fail("the action's name for the panel: " + label);
  if (!sock.busy["relay/desk-bar/ch1"]) fail("the socket is not marked while its request is on its way");
  click({ act: "sock", source: "relay/desk-bar", socket: "ch1", to: "off" });  // a second click meanwhile
  if (sent.length !== 1) fail("a second request while the first is on its way");
  await tick();
  if (stepUps.length !== 1 || flashes.length !== 0) fail("after the panel asked for a confirmation: " + JSON.stringify([stepUps, flashes]));
  const [pending, detail] = stepUps[0];
  if (JSON.stringify(pending) !== '{"kind":"plug","label":"Switch off Lamp (Desk powerbar)","method":"POST","path":"/api/power/socket","body":{"source":"relay/desk-bar","socket":"ch1","to":"off"}}') fail("what the dialog is given: " + JSON.stringify(pending));
  if (detail.pending_id !== "p1") fail("the panel's detail: " + JSON.stringify(detail));
  if (sock.busy["relay/desk-bar/ch1"]) fail("the socket stays marked after the answer");

  // 2. an answer with the result (a panel that asks for no confirmation would give it at once)
  answer = async () => ({ ok: true, to: "on", confirmed: true, message: "Printer is on." });
  click({ act: "sock", source: "relay/desk-bar", socket: "ch2", to: "on" });
  await tick();
  if (sent[1][1].body !== '{"source":"relay/desk-bar","socket":"ch2","to":"on","tag":"t-ch2"}') fail("the second request: " + sent[1][1].body);
  if (JSON.stringify(flashes.pop()) !== '["Printer is on.",false]' || done.length !== 1) fail("the result line");
  answer = async () => ({ ok: true, to: "on", confirmed: false, message: "Sent. Printer does not show on yet." });
  click({ act: "sock", source: "relay/desk-bar", socket: "ch2", to: "on" });
  await tick();
  if (JSON.stringify(flashes.pop()) !== '["Sent. Printer does not show on yet.",true]') fail("a switch that is not confirmed is shown as a warning");
  answer = async () => ({ ok: true });
  click({ act: "sock", source: "relay/desk-bar", socket: "ch2", to: "on" });
  await tick();
  if (JSON.stringify(flashes.pop()) !== '["Printer: sent",true]') fail("an answer without a sentence is never shown as done");

  // 3. a refusal, and no answer: the panel's sentence, as a failure
  for (const msg of ["Wait 4 s before you switch Lamp again.", "No answer from RELAY. It may have been switched: look at the socket.", "Panel not reachable. No answer: it is not known whether this was done."]) {
    answer = async () => { const e = new Error(msg); e.status = 409; throw e; };
    click({ act: "sock", source: "relay/desk-bar", socket: "ch1", to: "off" });
    await tick();
    if (JSON.stringify(flashes.pop()) !== JSON.stringify([msg, true])) fail("the line for: " + msg);
    if (sock.busy["relay/desk-bar/ch1"]) fail("the socket stays marked after a refusal");
  }

  // 4. what is never sent
  const before = sent.length;
  for (const d of [{ source: "relay/desk-bar", socket: "ch1", to: "toggle" }, { source: "relay/desk-bar", socket: "ch1", to: "" }, { source: "relay/desk-bar", socket: "ch1" },
    { source: "relay/desk-bar", socket: "ch1", to: "ON" }, { source: "relay/desk-bar", socket: "ch3", to: "off" }, { source: "relay/desk-bar", socket: "ch4", to: "off" },
    { source: "relay/desk-bar", socket: "nope", to: "off" }, { source: "relay/nope", socket: "ch1", to: "off" }, { source: "__proto__", socket: "ch1", to: "off" }, { socket: "ch1", to: "off" }]) {
    click({ act: "sock", ...d });
    await tick();
  }
  click({ act: "sock", source: "relay/desk-bar", socket: "ch1", to: "off" }, { disabled: true });
  click({ act: "other", source: "relay/desk-bar", socket: "ch1", to: "off" });
  await tick();
  if (sent.length !== before || flashes.length !== 0) fail("a request that must not be sent: " + JSON.stringify(sent.slice(before)));

  // 5. the tag is the row's as the page has it at the click, not the one of an older drawing
  bar.sockets[0].tag = "t-new";
  answer = async () => ({ ok: true, confirmed: true, message: "Lamp is off." });
  click({ act: "sock", source: "relay/desk-bar", socket: "ch1", to: "off" });
  await tick();
  if (sent[sent.length - 1][1].body !== '{"source":"relay/desk-bar","socket":"ch1","to":"off","tag":"t-new"}') fail("the tag at the click: " + sent[sent.length - 1][1].body);
  for (const [, o] of sent) { const b = JSON.parse(o.body); if (Object.keys(b).join() !== "source,socket,to,tag" || (b.to !== "on" && b.to !== "off")) fail("a body with more than one socket and one direction: " + o.body); }
  if (!renders) fail("the page is not drawn again around a request");
})().catch((e) => fail("the script: " + (e && e.stack || e)));
`)
}

// The rest of the page that knows of the switch: the result of the kept action after "Confirm it's you" is the
// panel's sentence alone (a switch that is not confirmed as a warning), "Resume" asks anew instead of sending
// the old request again, the audit page can filter the plug entries, and the sentences about a machine whose
// check is broken name plugs too.
func TestUISocketSwitchInTheRestOfThePage(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		"if (pr.path === \"/api/power/socket\") { flash(pr.message || `${pr.label}: ${pr.ok ? \"sent\" : `failed · HTTP ${pr.status}`}`, !pr.ok || pr.warn === true); return; }",
		`else if (p.kind === "plug") await sockSwitch(p.body?.source, p.body?.socket, p.body?.to);`,
		`["fan", "Fans"], ["plug", "Plugs"],`,
		`Every switch, on or off, asks for your passkey, and that machine checks it itself.`,
	} {
		if strings.Count(js, want) != 1 {
			t.Errorf("app.js lacks (or has twice) %q", want)
		}
	}
	if n := strings.Count(js, "refuses fan, power, update and plug requests"); n != 4 {
		t.Errorf("the sentences about a machine that refuses: %d of 4 name plugs", n)
	}
	if strings.Contains(js, "refuses fan, power and update requests") {
		t.Error("a sentence about a machine that refuses still leaves plugs out")
	}
	// the page never asks for a toggle, and has one place that sends a switch
	if regexp.MustCompile(`(?i)["'\x60]toggle["'\x60]`).MatchString(js) {
		t.Error("app.js names a toggle")
	}
	if n := strings.Count(js, `api("/api/power/socket"`); n != 1 {
		t.Errorf("places that send a switch: %d", n)
	}
	b, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\n.switch.busy::after { content: none; }", "\n.switch.busy .spinner {", "\n.info-btn.sock-lock { width: 36px; height: 20px;",
		"\nbody.unreachable .switch[data-act=\"sock\"][aria-checked=\"true\"]::after { background: var(--faint); }", "\n.list-row.wall-sock .sock-word { min-width: 52px; }"} {
		if strings.Count(string(b), want) != 1 {
			t.Errorf("app.css lacks (or has twice) %q", want)
		}
	}
	// a socket's switch is the panel's one switch: there is no second look for it
	if strings.Contains(string(b), "sock-sw") || strings.Contains(js, "sock-sw") {
		t.Error("the sockets have a switch of their own again (sock-sw)")
	}
	if n := strings.Count(js, `role="switch"`); n != 1 {
		t.Errorf("places that draw a switch button: %d", n)
	}
}

// "Still running" and "Cut power anyway" (PLUGS-DESIGN.md, 4.5): the panel refuses "off" for a socket whose
// machine is not off (409 machine_running), and the page shows a dialog. Its title is the fact, with the one
// name the panel has for the machine; a small line above it says which socket; one short line says what going
// on costs, and the panel's own sentence is behind the info button. Cancel has the focus. "Shut down NAME"
// opens the power dialog of that host, when this user can shut it down from here. "Cut power anyway" is the
// red button with the passkey's mark: the machine's name has to be typed, and the same switch is sent again
// with cut: true and what was typed. The page's own typedDialog is run here, with a page that is only as much
// as the two dialogs touch.
func TestUISocketOfARunningMachineIsNotJustSwitchedOff(t *testing.T) {
	js := appJS(t)
	typed := regexp.MustCompile(`(?ms)^function typedDialog\(.*?^\}$`).FindString(js)
	if typed == "" {
		t.Fatal("app.js has no typedDialog any more")
	}
	runPowerPageScript(t, "switch-running", typed+`
const modal = { hidden: true, html: "", onclick: null }, page = { els: {} };
Object.defineProperty(modal, "innerHTML", { get() { return this.html; }, set(v) { this.html = v; page.els = {}; } });
const $ = (sel) => {
  if (sel === "#modal") return modal;
  const id = sel.slice(1), m = new RegExp('<[a-z]+[^>]* id="' + id + '"[^>]*>').exec(modal.html);
  return m ? (page.els[id] ||= { id, value: "", textContent: "", disabled: / disabled/.test(m[0]), focus() {} }) : null;
};
const location = { hash: "#/power" };
const closed = [], closeModal = () => { closed.push(1); modal.hidden = true; modal.innerHTML = ""; };
const capsAsked = [], refreshCaps = async (id) => { capsAsked.push(id); };
const opened = [], openPowerDialog = (s, action) => { opened.push([s.id, action]); };
const focused = [], focusAtTop = (sel) => { focused.push(sel); };
state.caps = {};
const k = (id, o) => ({ id, name: id, state: "on", switchable: true, tag: "t-" + id, ...o });
const bar = src("relay/desk-bar", { name: "Desk powerbar", sockets: [k("ch1", { name: "PC socket", system_id: "desk-pc" }), k("ch2", { name: "Lamp", state: "off" })] });
state.power = { total_w: 1, sources: [bar] };
state.systems = [{ id: "desk-pc", name: "Desk PC" }];
state.byId = { "desk-pc": state.systems[0] };
const click = (data) => clicks.forEach((fn) => fn({ target: { closest: () => ({ dataset: { act: "sock", ...data }, disabled: false }) } }));
const tick = () => new Promise((r) => setTimeout(r, 5));
const runs = (o = {}) => async () => { const e = new Error("Desk PC is still running. Shut it down first."); e.status = 409;
  e.detail = { code: "machine_running", message: e.message, machine: "Desk PC", system_id: "desk-pc", ...o }; throw e; };
const text = (h) => h.replace(/<[^>]+>/g, " ").replace(/\s+/g, " ").trim();
const off = () => click({ source: "relay/desk-bar", socket: "ch1", to: "off" });
(async () => {
  // 1. the refusal: the dialog with the panel's sentence; nothing else was sent, nothing flashed
  state.caps["desk-pc"] = { actions: [{ action: "shutdown", available: true }, { action: "reboot", available: true }] };
  answer = runs();
  off(); await tick();
  if (sent.length !== 1 || flashes.length !== 0 || modal.hidden) fail("after the refusal: " + JSON.stringify([sent.length, flashes, modal.hidden]));
  if (text(modal.html) !== "Desk powerbar · PC socket Desk PC is still running Cutting power now can lose unsaved work. Shut it down first. Cancel Shut down Desk PC Cut power anyway") fail("the dialog: " + text(modal.html));
  if (!modal.html.includes('<div class="note" id="sr-sock">Desk powerbar · PC socket</div>') || !modal.html.includes('<h3 id="sr-t">Desk PC is still running</h3>')) fail("the socket's line and the title: " + modal.html);
  if (!/<button class="btn" type="button" id="sr-cancel">Cancel<\/button><button class="btn" type="button" id="sr-down">Shut down Desk PC<\/button>\s*<button class="btn danger-solid" type="button" id="sr-cut"><svg class="i-shield"><\/svg>Cut power anyway<\/button>/.test(modal.html)) fail("the buttons: " + modal.html);
  if (focused.pop() !== "#sr-cancel" || capsAsked.length !== 0) fail("the focus is not on Cancel, or the host's actions were asked for again");
  // one name for the machine: the socket's own name is only in the socket's line
  if ((text(modal.html).match(/PC socket/g) || []).length !== 1 || /DESK|was not switched/.test(modal.html)) fail("the names in the dialog: " + text(modal.html));
  // the panel's own sentence, and what "Cut power anyway" is, are behind the info button
  if (!/<p id="sr-msg">Cutting power now can lose unsaved work\. Shut it down first\.<button class="info-btn" type="button" data-info="sock-run"/.test(modal.html)) fail("the one line and its info button: " + modal.html);
  if (info.html["sock-run"] !== '<button class="info-x" type="button" aria-label="Close"></button><div class="info-t">Cut power anyway</div><p>Desk PC is still running. Shut it down first.</p><p>It is like pulling the plug: nothing is shut down first. It asks for the machine&#39;s name, then for your passkey.</p>'.replace("&#39;", "'")) fail("behind the info button: " + info.html["sock-run"]);
  if (sock.busy["relay/desk-bar/ch1"]) fail("the socket stays marked behind the dialog");
  // 2. Shut down: the host's own power dialog
  $("#sr-down").onclick();
  if (JSON.stringify(opened) !== '[["desk-pc","shutdown"]]' || closed.length !== 1 || sent.length !== 1) fail("Shut down: " + JSON.stringify([opened, closed, sent.length]));
  // 3. a host this user cannot shut down from here, one whose row this user does not have, and control that is
  // switched off: no "Shut down"
  const can = { actions: [{ action: "shutdown", available: true }] };
  for (const [caps, detail, asked, why] of [[{ actions: [{ action: "shutdown", available: false }] }, {}, 0, ""], [undefined, {}, 1, ""], [undefined, { system_id: undefined }, 0, ""],
    [undefined, { system_id: "hidden" }, 0, ""], [can, {}, 0, "Read-only: only admins can use control actions."]]) {
    state.caps = caps ? { "desk-pc": caps } : {}; capsAsked.length = 0; modal.hidden = true; modal.innerHTML = ""; whyNot = why;
    answer = runs(detail);
    off(); await tick();
    if (modal.hidden || /sr-down|Shut down/.test(modal.html) || !modal.html.includes('id="sr-cut"') || focused.pop() !== "#sr-cancel") fail("a host that cannot be shut down from here: " + modal.html);
    if (capsAsked.length !== asked) fail("the host's actions asked for: " + capsAsked.length + " of " + asked);
  }
  whyNot = "";
  // 3a. the title says which kind of "not off" the panel found
  for (const [st, title] of [["running", "Desk PC is still running"], ["asleep", "Desk PC is asleep, not off"], ["unknown", "Desk PC may still be running"], [undefined, "Desk PC is still running"],
    ["constructor", "Desk PC is still running"], ["<b>", "Desk PC is still running"]]) {
    modal.hidden = true; modal.innerHTML = "";
    answer = runs({ state: st });
    off(); await tick();
    if (!modal.html.includes('<h3 id="sr-t">' + title + "</h3>")) fail("the title for " + st + ": " + modal.html);
  }
  // 4. Cut power anyway: the name has to be typed
  const n = sent.length;
  $("#sr-cut").onclick();
  if (text(modal.html) !== "Cut power to Desk PC? This is like pulling the plug. Nothing is shut down first: unsaved work is lost and files can be damaged. Type Desk PC to confirm Cancel Cut power") fail("the cut dialog: " + text(modal.html));
  const input = $("#td-in"), go = $("#td-go");
  if (!go.disabled || !/id="td-go" disabled>Cut power</.test(modal.html)) fail("Cut power can be pressed before the name is typed");
  for (const [v, ok] of [["", false], ["Desk", false], ["Desk PC!", false], ["PC socket", false], ["desk pc", true], [" Desk PC ", true], ["Desk PC", true]]) {
    input.value = v; input.oninput();
    if (go.disabled === ok) fail("typed " + JSON.stringify(v) + ": Cut power is " + (go.disabled ? "off" : "on"));
  }
  if (sent.length !== n) fail("typing sent something");
  // 4a. the request: the same switch, with cut and what was typed; "Confirm it's you" takes the dialog's place
  answer = async () => { const e = new Error("step-up"); e.stepUp = true; e.detail = { code: "step_up_required", pending: true, pending_id: "p2", signs: "cut the power of Desk PC while it is running: switch off PC socket (Desk powerbar)" }; throw e; };
  stepUps.length = 0; closed.length = 0;
  await go.onclick();
  if (sent.length !== n + 1 || sent[n][1].body !== '{"source":"relay/desk-bar","socket":"ch1","to":"off","tag":"t-ch1","cut":true,"confirm":"Desk PC"}') fail("the cut request: " + sent[n]?.[1].body);
  if (sent[n][2] !== "Cut power to Desk PC: switch off PC socket (Desk powerbar)") fail("the cut's name for the panel: " + sent[n][2]);
  if (stepUps.length !== 1 || JSON.stringify(stepUps[0][0].body) !== '{"source":"relay/desk-bar","socket":"ch1","to":"off"}' || stepUps[0][0].kind !== "plug") fail("what Resume would ask anew: " + JSON.stringify(stepUps[0]));
  if (closed.length !== 0 || flashes.length !== 0) fail("the page closed the confirmation or showed a result");
  // 4b. the panel's "no" stays in the dialog, and the button works again
  answer = async () => { const e = new Error("Type Desk PC to confirm cutting its power. Nothing was switched."); e.status = 422; throw e; };
  go.disabled = true;
  await go.onclick();
  if ($("#td-err").textContent !== "Type Desk PC to confirm cutting its power. Nothing was switched." || go.disabled) fail("a refusal of the cut: " + JSON.stringify([$("#td-err").textContent, go.disabled]));
  // 4c. an answer with the result (a panel that asks for no confirmation would give it at once)
  answer = async () => ({ ok: true, confirmed: true, message: "PC socket is off." });
  await go.onclick();
  if (closed.length !== 1 || JSON.stringify(flashes.pop()) !== '["PC socket is off.",false]') fail("the cut's result");
  // 4d. a socket that cannot be switched any more by the time the name is typed: nothing is sent
  answer = runs(); off(); await tick(); $("#sr-cut").onclick();
  const m = sent.length;
  bar.sockets[0].switchable = false;
  await $("#td-go").onclick();
  if (sent.length !== m || $("#td-err").textContent !== "This socket cannot be switched from here any more. Nothing was switched.") fail("a cut for a socket that is locked meanwhile: " + $("#td-err").textContent);
  bar.sockets[0].switchable = true; modal.hidden = true; modal.innerHTML = "";

  // 5. what opens no dialog: the same word in answer to "on", another refusal, a refusal that names no machine,
  // and another dialog that is open
  for (const [to, sock2, ans, line] of [["on", "ch2", runs(), "Desk PC is still running. Shut it down first."],
    ["off", "ch1", async () => { const e = new Error("Wait 3 s before you switch PC socket again."); e.status = 409; e.detail = { code: "too_soon", message: e.message }; throw e; }, "Wait 3 s before you switch PC socket again."],
    ["off", "ch1", runs({ machine: undefined }), "Desk PC is still running. Shut it down first."],
    ["off", "ch1", runs({ machine: "", message: "" }), "The machine on this socket is still running. Nothing was switched."]]) {
    answer = ans; flashes.length = 0;
    click({ source: "relay/desk-bar", socket: sock2, to }); await tick();
    if (!modal.hidden || JSON.stringify(flashes) !== JSON.stringify([[line, true]])) fail("a dialog for " + to + " / " + line + ": " + JSON.stringify(flashes) + modal.html);
  }
  modal.hidden = false; modal.innerHTML = "<p>another dialog</p>"; flashes.length = 0;
  answer = runs(); off(); await tick();
  if (modal.html !== "<p>another dialog</p>" || flashes.length !== 1) fail("the dialog took another dialog's place");
  modal.hidden = true; modal.innerHTML = "";

  // 6. a machine's name is text
  answer = runs({ machine: "<b>x</b>", message: "<i>m</i> is still running. Shut it down first." });
  off(); await tick();
  if (/<b>x<\/b>|<i>m<\/i>/.test(modal.html + info.html["sock-run"]) || !info.html["sock-run"].includes("<p>&lt;i&gt;m&lt;/i&gt; is still running. Shut it down first.</p>") ||
    !modal.html.includes('<h3 id="sr-t">&lt;b&gt;x&lt;/b&gt; is still running</h3>')) fail("markup from the panel's sentence or the machine's name: " + modal.html + info.html["sock-run"]);
  $("#sr-cut").onclick();
  if (/<b>x<\/b>/.test(modal.html) || !modal.html.includes("<h3 id=\"td-t\">Cut power to &lt;b&gt;x&lt;/b&gt;?</h3>") || !modal.html.includes('placeholder="&lt;b&gt;x&lt;/b&gt;"')) fail("markup from the machine's name: " + modal.html);
  // every request of this test: one socket, one direction, and "cut" only with a typed name
  for (const [path, o] of sent) { const b = JSON.parse(o.body); const keys = Object.keys(b).join();
    if (path !== "/api/power/socket" || (keys !== "source,socket,to,tag" && keys !== "source,socket,to,tag,cut,confirm") || (b.cut && (b.to !== "off" || !b.confirm))) fail("a request: " + o.body); }
})().catch((e) => fail("the script: " + (e && e.stack || e)));
`)
	for _, want := range []string{
		`if (to === "off" && x.detail?.code === "machine_running")`,
		`"This is like pulling the plug. Nothing is shut down first: unsaved work is lost and files can be damaged."`,
		`{ cut: true, confirm: typed }`,
		`Cut power anyway is there for a machine that hangs.`,
		`Cutting power now can lose unsaved work. Shut it down first.`,
		`<button class="btn danger-solid" type="button" id="sr-cut">${I.shield}Cut power anyway</button>`,
		`focusAtTop("#sr-cancel");`,
	} {
		if strings.Count(js, want) != 1 {
			t.Errorf("app.js lacks (or has twice) %q", want)
		}
	}
	if n := strings.Count(js, "cut: true"); n != 2 { // the request, and the comment that explains it
		t.Errorf("places that ask for a cut: %d", n)
	}
}
