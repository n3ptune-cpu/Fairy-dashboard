package web

import (
	"strings"
	"testing"
)

// The Updates page in the Overview's order (13 Nov, the owner: "i'd make it more cleaner and not have too much
// on the page, like overview"): a row of tiles (the agents with their counts and "Update all", the panel, the
// latest release), one table of machines, and "Update unpowered hosts" folded under it. Nothing the page could
// do is gone, and nothing that needs a look waits behind a fold.

// The tiles: what each says, and that a count is the number of rows that show that word or that colour.
func TestUIUpdatesPageSummaryTiles(t *testing.T) {
	js := appJS(t)
	runPageScript(t, "updates-tiles", updatesPageScript(t, js, `
  const tile = (html, key) => (html.match(new RegExp('<div class="panel glass tile" data-key="' + key + '">(.*?)</div>\\s*(?=<div class="panel glass tile"|</div>\\s*<div class="panel glass scroll">|</div>\\s*<div class="note warn">)', "s")) || ["", ""])[1];
  wu.data = null;
  upd.data = updates([
    agent("pc-a", { behind: false, can_update: false }),
    agent("pc-b"), agent("pc-c"),
    agent("pc-d", { self_update: { state: "failed", message: "the download <b>failed</b>" } }),
    agent("pc-e", { behind: false, can_update: false, self_update: { state: "idle", last: { ok: false, at: 1794500000, message: "rolled back: the new version did not come up" } } }),
    agent("pc-f", { can_update: false, self_update: { state: "unavailable", message: "the running helper is older than its agent" }, why: "the running helper is older than its agent" }),
    agent("pc-g", { behind: false, can_update: false, approval: "broken" }),
    agent("pc-h", { status: "offline", can_update: false, version_last_seen: true, self_update: null }),
  ], { latest: { tag: "v0.8.0", version: "18-ccccccc", verified: true, published_at: "2026-11-13T10:00:00Z", url: "https://example.test/release" },
    panel_version: "17-aaaaaaa", panel_latest: { tag: "v0.8.1", version: "18-ccccccc", url: "https://example.test/notes" }, panel_behind: true });
  pupd.data = { ready: true, behind: true, version: "17-aaaaaaa" };
  let html = pageUpdates();
  if ((html.match(/class="panel glass tile"/g) || []).length !== 3 || !html.startsWith('<div class="stack">\n    <div class="tiles upd-tiles">')) fail("three tiles on top: " + html.slice(0, 120));
  // the agents: 3 up to date of 8; 4 rows say "update available"; 4 rows are red; 1 is offline; and the main button
  const agents = words(tile(html, "upt-agents"));
  if (agents !== "Agents 3 / 8 up to date 4 update available 4 to look at 1 offline Update all (3)") fail("the agents' tile: " + agents);
  const all = tag(tile(html, "upt-agents"), 'data-act="upd-all"');
  if (!all || all.disabled) fail("Update all in the tile: " + (all && all.html));
  // ... and each of the four is red in its own row, with its words: nothing of it is behind a fold
  const row = (id) => words((html.match(new RegExp('<tr class="row" data-key="up-' + id + '">.*?</tr>', "s")) || [""])[0]);
  if (!row("pc-d").includes("failed the download <b>failed</b>") || html.includes("<b>failed</b>")) fail("a failed update: " + row("pc-d"));
  if (!row("pc-e").includes("last update failed")) fail("a rolled-back machine: " + row("pc-e"));
  if (!row("pc-f").includes("unavailable the running helper is older than its agent")) fail("helper older than its agent: " + row("pc-f"));
  if (!row("pc-g").includes("approval broken")) fail("a broken check: " + row("pc-g"));
  if ((html.match(/update available<\/div>/g) || []).length !== 4) fail("rows that say update available: " + (html.match(/update available<\/div>/g) || []).length);
  // the panel: its version, that a newer one is known, the button, and what the button does behind the info button
  const panel = words(tile(html, "upt-panel"));
  if (panel !== "Panel build 17 (aaaaaaa) update available latest build 18 (ccccccc) Release notes Update panel") fail("the panel's tile: " + panel);
  const pb = tag(tile(html, "upt-panel"), 'data-act="panel-upd"');
  if (!pb || pb.disabled || !pb.html.includes('title="Install build 18 (ccccccc) on the panel&#39;s host"')) fail("Update panel: " + (pb && pb.html));
  // the latest release, with "Check for updates" and when it was last checked
  const latest = words(tile(html, "upt-latest"));
  if (latest !== "Latest agent release build 18 (ccccccc) signed published 1m ago v0.8.0 Check for updates Last checked on GitHub: not yet") fail("the release's tile: " + latest);
  if (!tag(tile(html, "upt-latest"), 'data-act="upd-check"')) fail("Check for updates is not in its tile");
  // everything up to date, the panel too: no counts, no words about an update, the buttons off with their reason
  upd.data = updates([agent("pc-a", { behind: false, can_update: false }), agent("pc-b", { behind: false, can_update: false })], { panel_version: "18-ccccccc", panel_latest: { tag: "v0.8.1", version: "18-ccccccc" }, panel_behind: false });
  pupd.data = { ready: true, behind: false, version: "18-ccccccc" };
  html = pageUpdates();
  if (words(tile(html, "upt-agents")) !== "Agents 2 / 2 up to date Update all (0)" || !tag(html, 'data-act="upd-all"').disabled) fail("all up to date: " + words(tile(html, "upt-agents")));
  if (words(tile(html, "upt-panel")) !== "Panel build 18 (ccccccc) up to date Update panel" || !tag(html, 'data-act="panel-upd"').disabled) fail("the panel up to date: " + words(tile(html, "upt-panel")));
  // a panel update that is running, or stopped, shows its steps in the tile; a panel that cannot update itself says why
  pupd.data = { ready: false, why: "the panel was not installed with <fairy update>", behind: true, version: "17-aaaaaaa", status: { state: "rolled_back", finished: true, message: "the new version did not come up", updated_at: new Date().toISOString() } };
  const stopped = tile(pageUpdates(), "upt-panel");
  if (!words(stopped).includes("Download Verified Restart Running rolled back the new version did not come up") || !stopped.includes('<div class="note warn">the panel was not installed with &lt;fairy update&gt;</div>')) fail("a panel update that rolled back: " + words(stopped));
  // the release check failed: said in its tile
  upd.data = updates([agent("pc-a")], { latest: { error: "GitHub <answered> 403" } });
  const noRelease = tile(pageUpdates(), "upt-latest");
  if (!words(noRelease).startsWith("Latest agent release not known Check for updates") || !noRelease.includes('<div class="note warn">GitHub &lt;answered&gt; 403</div>')) fail("no release known: " + words(noRelease));
  // no agent at all: the table says so
  upd.data = updates([]);
  if (!pageUpdates().includes('<td colspan="5" class="note">No agents in config.yaml.</td>')) fail("no agents");
`))
}

// "Update unpowered hosts" is folded: its heading, its button, its one sentence, a line about the last run and
// the button that shows the rest. A run that is going is never folded, and a run that just ended is shown. What
// needs a look is said on the folded section too.
func TestUIUpdatesPageUnpoweredHostsFold(t *testing.T) {
	js := appJS(t)
	for _, want := range []string{
		`const wuOpen = (d) => !!d.active || (wu.open.runs ?? wuJustOver(d));`,
		`if (t.dataset.act === "wu-fold") { wu.open.runs = !(wu.data && wuOpen(wu.data)); render(); return; }`,
		`if (wuLive && wuLive !== was) delete wu.open.runs;`,
	} {
		if strings.Count(js, want) != 1 {
			t.Errorf("app.js lacks (or has twice) %q", want)
		}
	}
	runPageScript(t, "updates-fold", updatesPageScript(t, js, `
  const settings = { online_timeout_s: 300, settle_s: 60, shutdown_delay_s: 120 };
  const host = (id, o) => ({ id, name: id.toUpperCase(), opted_in: true, can_join: true, can_wake: true, relay: "NAS", power: "off", choice: "wake", ...o });
  const runHost = (id, o) => ({ id, name: id.toUpperCase(), mode: "wake", state: "off", on: "off", update: "ok", text: "Updated, and switched off again.", log: [{ t: "2026-11-13T16:19:00Z", text: "Wake sent." }], ...o });
  const minAgo = (m) => new Date(Date.now() - m * 60000).toISOString();
  const run = (id, state, hosts, o) => ({ id, state, by: "nora", started_at: minAgo(200), list_hash: "abc123", hosts, ...o });
  const data = (o) => ({ hosts: [host("pc-a"), host("pc-b"), host("pc-c", { choice: "", why: "it is on, and its agent does not answer" })], active: null, last: [], settings, off_after_s: 900, max_hosts: 16, ...o });
  const box = () => wakeUpdateBox();
  const isOpen = (html) => html.includes('class="table wu-machines"');
  const fold = (html) => tag(html, 'data-act="wu-fold"');
  const acts = (html) => [...new Set([...html.matchAll(/data-act="([a-z-]+)"/g)].map((m) => m[1]))].sort().join(" ");
  upd.data = updates([agent("pc-a")]);

  // 1. no run yet: folded. The heading, the button that starts a run, the sentence with the promise, how many
  //    machines are switched on, and the button that shows the machines
  wu.data = data({});
  let html = box();
  if (isOpen(html) || html.includes("wu-run")) fail("no run: not folded");
  if (words(html) !== "Update unpowered hosts Update unpowered hosts Wakes the machines you choose, updates their agent and switches them off again. Fairy only switches off machines this run woke. A machine that is already on is never switched off. 3 are switched on, 2 can be chosen right now. Show the machines") fail("no run, folded: " + words(html));
  if (fold(html)?.html.includes('aria-expanded="true"') || tag(html, 'data-act="wu-open"').disabled) fail("no run: the buttons: " + fold(html)?.html);
  // the button opens it: the table of machines with each machine's switch, and it folds again
  await click(fold(html), '[data-act="wu-fold"]');
  html = box();
  if (!isOpen(html) || wu.open.runs !== true || !words(html).includes("Hide the machines Machines A machine takes part only after you switch it on here, one machine at a time. 3 are switched on, 2 can be chosen right now.")) fail("opened: " + words(html));
  if ((html.match(/data-act="wu-switch"/g) || []).length !== 3 || html.includes('id="wu-joined"')) fail("opened: the switches: " + acts(html));
  await click(fold(html), '[data-act="wu-fold"]');
  if (isOpen(box()) || wu.open.runs !== false) fail("folded again: " + wu.open.runs);
  delete wu.open.runs;

  // 2. a run that ended long ago, with something to look at: folded, and the line says so in red (and in amber
  //    the machine nothing was sent to). The report is one click away, the earlier runs another one
  const bad = run("r2", "done", [runHost("pc-a"), runHost("pc-b", { state: "left_on", on: "on", text: "It is still on. It is ON." }), runHost("pc-c", { state: "skipped", on: "untouched", text: "Nothing was sent to it." })], { ended_at: minAgo(120) });
  const old = run("r1", "done", [runHost("pc-a")], { ended_at: minAgo(900) });
  wu.data = data({ last: [bad, old] });
  html = box();
  if (isOpen(html) || html.includes("wu-run")) fail("an old run: not folded");
  const last = (html.match(/<div class="wu-last" id="wu-last">.*?<\/div>/s) || [""])[0];
  if (words(last) !== "finished Last run 1m ago · 3 machines 1 to look at 1 left out" || !last.includes('<span class="chip bad">1 to look at</span>') || !last.includes('<span data-ago="' + bad.ended_at + '">')) fail("the folded line: " + words(last) + " " + last);
  if (!words(html).endsWith("Show the machines and the last run Show 1 earlier run")) fail("an old run, the buttons: " + words(html));
  await click(fold(html), '[data-act="wu-fold"]');
  html = box();
  if (!words(html).includes("Last run finished started by nora") || !words(html).includes("LEFT ON") || html.includes('id="wu-last"')) fail("the report, opened: " + words(html));
  // a run that ended well: no red, no amber
  delete wu.open.runs;
  wu.data = data({ last: [old] });
  if (/chip (bad|warn)/.test(box())) fail("a good run has a red or amber sign: " + words(box()));
  // a run that was stopped: its chip is red on the folded line
  wu.data = data({ last: [run("r3", "stopped", [runHost("pc-a", { state: "interrupted", on: "unknown", text: "The panel restarted." })], { ended_at: minAgo(50) })] });
  if (!box().includes('<div class="wu-last" id="wu-last"><span class="chip bad">stopped</span>') || !box().includes('<span class="chip bad">1 to look at</span>')) fail("a stopped run: " + words(box()));

  // 3. a run is going: open, whatever was chosen before, with its Cancel, and no button that folds it
  for (const chosen of [undefined, false, true]) {
    if (chosen === undefined) delete wu.open.runs; else wu.open.runs = chosen;
    wu.data = data({ active: run("r4", "running", [runHost("pc-a", { state: "waking", on: "", update: "", text: "Wake sent." })]), last: [old] });
    html = box();
    if (!isOpen(html) || !html.includes('data-act="wu-cancel" data-id="r4"') || fold(html) || !words(html).includes("running started by nora")) fail("a run is going (chosen: " + chosen + "): " + acts(html));
    if (!tag(html, 'data-act="wu-open"').disabled) fail("a run is going: a second run can be started");
  }
  // ... and the page that sees a new run forgets a fold chosen before it, so its end is shown too
  wu.open.runs = false;
  answer = () => data({ active: run("r5", "running", [runHost("pc-a", { state: "waking", on: "", update: "" })]) });
  await fetchWakeUpdate();
  if (wu.open.runs !== undefined) fail("a new run: the fold chosen before is kept: " + wu.open.runs);
  // 4. it just ended (2 minutes ago): shown, and it can be folded; 11 minutes ago: folded by itself
  answer = () => data({ last: [run("r5", "done", [runHost("pc-a")], { ended_at: minAgo(2) })] });
  await fetchWakeUpdate();
  html = box();
  if (!isOpen(html) || !words(html).includes("Hide the machines and the last run Last run finished")) fail("just over: " + words(html));
  await click(fold(html), '[data-act="wu-fold"]');
  if (isOpen(box())) fail("just over: it cannot be folded");
  delete wu.open.runs;
  wu.data = data({ last: [run("r5", "done", [runHost("pc-a")], { ended_at: minAgo(11) })] });
  if (isOpen(box())) fail("over for 11 minutes: still open");

  // 5. what must not wait behind a fold is on the folded section: why no run can start, and the machine with the check on
  wu.data = data({ problem: "No relay is <online>.", hosts: [host("pc-a", { approval: "required", why: "needs approval: not supported in runs yet" })] });
  html = box();
  if (isOpen(html) || !html.includes('<div class="warnbox">No relay is &lt;online&gt;.</div>') || !html.includes('id="wu-appr"') || !tag(html, 'data-act="wu-open"').disabled) fail("folded, with a problem: " + words(html));

  // 6. everything the page could do is still there: on the page as it opens, or one click away
  flashes.length = 0; delete wu.open.runs;
  wu.data = data({ last: [bad, old] });
  upd.data = updates([agent("pc-a"), agent("pc-b", { behind: false, can_update: false })], { panel_behind: true, panel_latest: { tag: "v0.8.1", version: "19-ddddddd" } });
  pupd.data = { ready: true, behind: true, version: "18-ccccccc" };
  html = pageUpdates();
  if (acts(html) !== "panel-upd upd-all upd-check upd-one wu-fold wu-log wu-open") fail("the page as it opens: " + acts(html));
  if (html.indexOf('class="tiles upd-tiles"') > html.indexOf('class="table updates"') || html.indexOf('class="table updates"') > html.indexOf('data-key="wu"')) fail("the order: tiles, the table, the section");
  if ((html.match(/<table /g) || []).length !== 1) fail("one table on the page as it opens: " + (html.match(/<table /g) || []).length);
  wu.open.runs = true; wu.open.earlier = true; wu.open["r2/pc-a"] = true;
  html = pageUpdates();
  if (acts(html) !== "panel-upd upd-all upd-check upd-one wu-fold wu-log wu-open wu-switch") fail("opened: " + acts(html));
  if ((html.match(/class="wu-run"/g) || []).length !== 2 || !html.includes('<ol class="wu-log">')) fail("opened: the last run, the earlier run and a machine's steps");
  if (html.includes('id="upd-run"')) fail("no run is going, and the page says one is");
  wu.data = data({ active: run("r6", "running", [runHost("pc-a", { state: "waking", on: "", update: "" }), runHost("pc-b", { state: "queued", on: "", update: "" })]), last: [bad] });
  html = pageUpdates();
  if (!acts(html).split(" ").includes("wu-cancel")) fail("a run is going: no Cancel run");
  // ... and it is said above the table too, where the page opens
  const said = words((html.match(/<div class="note" id="upd-run">.*?<\/div>/) || [""])[0]);
  if (said !== 'running "Update unpowered hosts" is going for 2 machines: its progress is under the table.' || html.indexOf('id="upd-run"') > html.indexOf('class="table updates"')) fail("a run is going, above the table: " + said);
`))
}
