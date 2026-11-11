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

// M2 (UI side): a system the panel could only describe as { id, name, kind, status: "error", error } is shown as
// an error, never dropped: the page has the error strip, the error card and the red dot for it.
func TestUIShowsErrorSystems(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const broken = (s) => s?.status === "error";`,
		`for (const s of state.systems.filter(broken))`, // the strip on every page
		`if (broken(s)) {`, // the system's own page
		`cannot be shown`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil || !strings.Contains(string(css), ".dot.error") {
		t.Error("app.css lacks the error dot", err)
	}
	// nothing filters systems by a list of known statuses (which would hide "error")
	if regexp.MustCompile(`state\.systems\s*=\s*[^;]*filter\([^;]*status`).MatchString(js) {
		t.Error("the systems list must not be filtered by status")
	}
}

// L4 (UI side): the Users page marks an address Pocket ID did not verify, shows the account id of every row,
// and names the account (id and mark) where a change is confirmed. Everything inserted goes through esc().
func TestUIUsersPageShowsAccountIdentity(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const unverified = (u) => !!u?.email && u.email_verified !== true;`, // null ("not known") is not verified
		`>unverified</span>`,
		`id ${esc(u.sub)}</span>`,
		`data-name="${esc(userIdent(u))}"`,
		`if (t.value === "admin" && !confirm(`,
		"for ${userIdent(u)}`",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	for _, raw := range []string{"`Role: ${ROLE_LABEL[t.value]}`", "`Sites for a viewer`", `data-name="${esc(u.email || u.name || u.sub)}"`, `${userIdent(u)}</`} {
		if strings.Contains(js, raw) {
			t.Errorf("app.js still has %q", raw)
		}
	}
}

// The Sessions page: it is a page of the router and an entry of the menu for everyone who has the menu, it
// marks the session of the page, its two actions go through the "Confirm it's you" path, and everything it
// shows (the User-Agent, addresses, names: all of it someone else's text) is inserted through esc().
func TestUISessionsPage(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`if (h.startsWith("#/sessions")) return { page: "sessions" };`, // the router
		`r.page === "sessions" ? pageSessions()`,
		`if (r.page === "sessions") loadSessions();`,
		`${item("#/sessions", I.sessions, "Sessions")}`, // the menu: not behind can_control or can_manage_users
		`["settings", "sessions", "audit", "users", "updates"].includes(page)`,
		`await api("/api/me/sessions")`,
		`if (state.me?.can_manage_users) {` + "\n" + `    try { sess.all = await api("/api/admin/sessions")`,
		`<span class="chip ok">This session</span>`,
		`>Sign out everywhere else</button>`,
		`sessCall("Sign out everywhere else", "/api/me/sessions/revoke-others")`,
		`t.dataset.scope === "all" ? "/api/admin/sessions/revoke" : "/api/me/sessions/revoke", { session: t.dataset.id }`,
		`await controlCall({ kind: "request", label, method: "POST", path, body },`, // a refusal opens "Confirm it's you"
		`<span data-ago="${esc(iso)}">${ago(iso)}</span>`,                           // relative times, kept current by the page's ticker
		`title="${esc(x.user_agent)}">${x.user_agent ? esc(x.user_agent) : "browser not recorded"}</span>`,
		`<b>${esc(x.browser || "Unknown browser")}</b>`,
		`${ip ? esc(ip) : '<span class="note">unknown</span>'}`,
		`${x.location ? ` + "`" + `<div class="note">${esc(x.location)}</div>` + "`" + ` : ""}`, // empty today
		`also used from ${esc(a.ip)}`,
		`signed in from ${esc(x.signed_in_ip)}`,
		`data-id="${esc(x.id)}"`,
		`No sessions to show`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// nothing of a session goes into the page as it came
	page := js[strings.Index(js, "const sess = {"):strings.Index(js, "// ------------------------------------------------------------ power confirmation dialog")]
	if raw := regexp.MustCompile(`\$\{(x|u|a)\??\.[a-z_.?]+\}`).FindAllString(page, -1); len(raw) != 0 {
		t.Errorf("the Sessions page inserts values without esc(): %v", raw)
	}
	if strings.Contains(page, "innerHTML") || strings.Contains(page, "safeHref") {
		t.Error("the Sessions page is rendered by the page's morph, like the other tables")
	}
	// the page never asks for, stores or shows a session id: it has handles only
	for _, bad := range []string{"sid", "cookie"} {
		if regexp.MustCompile(`(?i)\b` + bad + `\b`).MatchString(page) {
			t.Errorf("the Sessions page mentions %q", bad)
		}
	}
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil || !strings.Contains(string(css), ".table.sessions tr.row { display: block;") {
		t.Error("app.css lacks the phone layout of the sessions table", err)
	}
}

// "Update machines that are off" on the Updates page: the entry point, the dialog that says in plain words
// what happens and lists the machines with tick boxes, the request it sends (through the "Confirm it's you"
// path), the live rows, the report, the cancel dialog and the per-host switch. Everything it shows of a run or
// a machine (names, the run's sentences, which can quote an agent's own error text) goes through esc().
func TestUIWakeUpdate(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`await Promise.all([fetchPanelUpdate(), fetchUpdates(), fetchWakeUpdate()]);`, // loaded with the Updates page, every 2 s
		`${wakeUpdateBox()}`,
		`wu.data = await api("/api/admin/wake-update")`,
		`data-act="wu-open"`, `>${I.onoff}Update machines that are off…</button>`,
		`<b>Fairy only switches off machines this run woke.</b> A machine that is already on is never switched off.`,
		// the dialog
		`<h3 id="wu-t">Update machines that are off</h3>`,
		`<li>It sends Wake-on-LAN through the machine named next to it.</li>`,
		`and it goes on as soon as the machine's agent answers.`, `A machine that does not come online in that time is given up on and reported.`,
		`It may then still be starting, or be on without its network (Tailscale).`,
		`Fairy only switches off machines <b>this run woke</b>. A machine that is already on is never switched off; a Proxmox node with running guests is left on.`,
		`Nobody is asked whether someone is using a machine the run woke: after its update it is switched off again.`,
		`Off now: wake, update, switch off again`, `On now: update only, never switched off`, `Cannot take part right now`,
		`${off.map((h) => tick(h, "wake", true)).join("")}`,        // machines that are off: ticked
		`${on.map((h) => tick(h, "update_only", false)).join("")}`, // machines that are on: not ticked
		`const body = { hosts: picked("wake"), update_only: picked("update_only"), update_if_on: !!$("#wu-ifon")?.checked };`,
		`() => api(path, { method: "POST", body: JSON.stringify(body) }));`,
		`if (r === null) return;  // "Confirm it's you" took the dialog's place: the run starts after the sign-in`,
		// "Confirm it's you" shows the panel's line for every machine
		`<ul class="su-items" id="su-items">${detail.items.map((x) => ` + "`" + `<li>${esc(x)}</li>` + "`" + `).join("")}</ul>`,
		// live rows and the report
		`<div class="wu-text">${esc(h.text)}${count}</div>`,
		`<b data-countdown="${esc(h.shutdown_at)}">`,
		`<span data-ago="${esc(h.wake_sent_at)}">`,
		`left_on: ["bad", "LEFT ON"]`, `gave_up: ["bad", "gave up"]`, `off: ["ok", "done · off again"]`, `interrupted: ["bad", "interrupted"]`,
		`${d.active ? wuRunBox(d.active, true) : d.last[0] ? ` + "`" + `<div class="wu-sub">Last run</div>${wuRunBox(d.last[0], false)}` + "`" + ` : ""}`, // the last run stays on the page
		`data-act="wu-cancel" data-id="${esc(r.id)}"`,
		// cancel: the default leaves the woken machines on
		`<option value="">stay on (the report lists them as ON)</option><option value="1">are switched off again (an update that is running is waited for)</option>`,
		`api(path, { method: "DELETE", body: JSON.stringify(body) }));`,
		// the per-host switch, on the page and in a machine's ⋯ menu
		`data-act="wu-switch" data-id="${esc(h.id)}" data-name="${esc(h.name)}" data-on="${h.opted_in ? "" : "1"}"`,
		`${wakeUpdateMenu(s)}</div>` + "`;",
		`api(path, { method: "PUT", body: JSON.stringify(body) }));`,
		`["wakeupdate", "Update machines that are off"]`, // the Audit page's filter
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	start := strings.Index(js, "// ------------------------------------------------------------ Update machines that are off")
	end := strings.Index(js, "// ------------------------------------------------------------ users, roles and sites")
	if start < 0 || end < start {
		t.Fatal("the section is not where this test looks for it")
	}
	sec := js[start:end]
	// nothing of a machine or a run goes into the page as it came
	if raw := regexp.MustCompile(`\$\{(h|r|d|c|l|x|e)\??\.[a-z_.?\[\]0-9]+\}`).FindAllString(sec, -1); len(raw) != 0 {
		t.Errorf("the section inserts values without esc(): %v", raw)
	}
	if n := strings.Count(sec, "innerHTML"); n != 2 { // the two dialogs; the section itself is rendered by the page's morph
		t.Errorf("innerHTML is used %d times in the section, want 2 (the dialogs)", n)
	}
	// a link is only ever made from a machine id through enc(), never from a text of the run
	for _, m := range regexp.MustCompile(`href="[^"]*"`).FindAllString(sec, -1) {
		if m != `href="#/system/${enc(h.id)}"` {
			t.Errorf("unexpected link in the section: %s", m)
		}
	}
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".wu-run {", ".wu-host {", ".wu-text {", ".su-items {", ".wu-log {"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("app.css lacks %q", want)
		}
	}
}
