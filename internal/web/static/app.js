// Fairy UI. Plain JS, no build step, strict-CSP friendly (no inline script/style).
// Data: /api/me, /api/settings, /api/stream (SSE: full `systems` once, then `systems_delta` every second),
// /api/ups, /api/power/history, per-page fetches. The DOM is patched in place (morph), so bars animate and
// scroll / selection / open menus survive the 1 s updates.
"use strict";

const $ = (s, root = document) => root.querySelector(s);
// Everything that is looked up by a name the server supplies (a system id, a site name, a source id, a state)
// is an object WITHOUT a prototype. A plain {} also "has" constructor, toString, __proto__ and the rest of
// what every object inherits, and a site named "constructor" then found a function where a list was expected:
// the Overview and the sidebar stopped for everyone who could see a host of that site (audit 5, r1-p3-01).
//   dict(entries)  a map made by the page (state.byId, state.sites, groups by site, ...)
//   table({...})   a fixed table of the page's own words, looked up by a word of the server's
//   own(o, k)      a value by name out of an object that came as JSON (that one has a prototype)
const dict = (entries) => { const d = Object.create(null); for (const [k, v] of entries || []) d[k] = v; return d; };
const table = (o) => Object.freeze(Object.assign(Object.create(null), o));
const own = (o, k) => (o != null && Object.hasOwn(o, k) ? o[k] : undefined);
const state = {
  me: null, systems: [], byId: dict(), summary: null, power: null, history: [], settings: null, timer: null,
  ups: null, upsHist: dict(), upsEvents: [], menu: null, poll: dict(), caps: dict(), watch: undefined, es: null, flash: null,
  sites: dict(), open: dict(), notes: { unread: 0, active_alerts: 0 }, managed: dict(),
};
const OPEN_KEY = "fairy-open-hosts";  // hosts whose guests are shown in the sidebar (this browser)
try { for (const id of JSON.parse(localStorage.getItem(OPEN_KEY) || "[]")) state.open[id] = true; } catch { /* ignore */ }
const saveOpen = () => { try { localStorage.setItem(OPEN_KEY, JSON.stringify(Object.keys(state.open).filter((k) => state.open[k]))); } catch { /* ignore */ } };
const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const enc = encodeURIComponent;
// Server values go into the page through esc() (text and attribute values), enc() (URL parts), cssName() (a class
// name) or safeHref() (a link target), also where the server only ever sends a number or one of a few words:
// the page does not rely on that.
const cssName = (v) => (typeof v === "string" && /^[a-z0-9_-]{1,32}$/.test(v) ? v : "");
// A link target from the server: http(s) addresses only (never javascript:, data:, ...); anything else leads nowhere.
const safeHref = (u) => { try { const x = new URL(String(u), location.origin); return x.protocol === "https:" || x.protocol === "http:" ? esc(x.href) : "#"; } catch { return "#"; } };
const controlOff = () => state.summary?.control_disabled || state.me?.control_disabled || null;
const canControl = () => !!state.me?.can_control && !controlOff();
const controlWhyNot = () => (controlOff() ? `Control is disabled by an administrator (${controlOff().by}). Re-enable on the server: admin control enable`
  : !state.me?.can_control ? "Read-only: only admins can use control actions." : "");

// ------------------------------------------------------------ formatting
const fmtBytes = (b) => {
  if (b == null) return "–";
  const u = ["B", "KiB", "MiB", "GiB", "TiB"]; let i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return `${b.toFixed(b >= 100 || i === 0 ? 0 : 1)} ${u[i]}`;
};
const fmtRate = (b) => (b == null ? "–" : `${fmtBytes(b)}/s`);
const fmtUptime = (s) => {
  if (s == null) return "–";
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  return d ? `${d}d ${h}h` : h ? `${h}h ${m}m` : `${m}m`;
};
const pct = (u, t) => (u != null && t ? Math.min(100, (100 * u) / t) : null);
const fmtP = (p) => (p == null ? "–" : `${p.toFixed(0)}%`);
const fmtW = (w) => (w == null ? "–" : `${w >= 100 ? w.toFixed(0) : w.toFixed(1)} W`);
const ago = (iso) => {
  if (!iso) return "never";
  const s = Math.max(0, (Date.now() - new Date(iso)) / 1000);
  return s < 60 ? `${s.toFixed(0)}s ago` : s < 3600 ? `${(s / 60).toFixed(0)}m ago` : `${(s / 3600).toFixed(1)}h ago`;
};
const kindLabel = (s) => table({ node: "node", qemu: "VM", lxc: "CT", agent: s.platform || "agent" })[s.kind];
const live = (s) => s.status === "online" || s.status === "stale" || s.status === "paused";  // paused = frozen values
// A system whose data the panel could not put into its answer (impossible numbers from an agent or Proxmox)
// arrives as a small entry { id, name, kind, status: "error", error }. It is never dropped or hidden: it gets an
// error strip on every page (renderAlerts), an error card as its own page, and a red dot wherever it is listed.
const broken = (s) => s?.status === "error";
const brokenWhy = (s) => s.error || "the panel could not read this system's data";

// ------------------------------------------------------------ icons (inline SVG, currentColor)
const svg = (d, size = 16) => `<svg width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${d}</svg>`;
const I = {
  overview: svg('<rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/>'),
  cluster: svg('<rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/><path d="M7 7h.01M7 17h.01"/>'),
  machine: svg('<rect x="3" y="4" width="18" height="12" rx="2"/><path d="M8 20h8M12 16v4"/>'),
  power: svg('<path d="M13 2 4 14h7l-1 8 9-12h-7z"/>'),
  settings: svg('<circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z"/>', 18),
  caret: svg('<path d="m9 6 6 6-6 6"/>', 14),
  collapse: svg('<path d="m15 6-6 6 6 6"/>', 18),
  menu: svg('<path d="M4 6h16M4 12h16M4 18h16"/>', 20),
  guests: svg('<rect x="3" y="3" width="8" height="8" rx="1.5"/><rect x="13" y="3" width="8" height="8" rx="1.5"/><rect x="3" y="13" width="8" height="8" rx="1.5"/><path d="M17 13v8M13 17h8"/>'),
  ext: svg('<path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/>', 13),
};

I.ups = svg('<rect x="6" y="3" width="12" height="18" rx="1"/><path d="M12 7l-2.5 5h5L12 17"/>');
I.battery = svg('<rect x="2" y="7" width="17" height="10" rx="2"/><path d="M22 11v2"/>');
I.more = svg('<circle cx="5" cy="12" r="1.3"/><circle cx="12" cy="12" r="1.3"/><circle cx="19" cy="12" r="1.3"/>', 16);
I.shield = svg('<path d="M12 3 4 6v6c0 4.5 3.4 8.3 8 9 4.6-.7 8-4.5 8-9V6z"/><path d="m9 12 2 2 4-4"/>', 18);
I.audit = svg('<path d="M8 4h11v16H8z"/><path d="M5 7v13h11"/><path d="M11 8h5M11 12h5M11 16h3"/>');
I.users = svg('<circle cx="9" cy="8" r="3.2"/><path d="M3.5 19c.6-3.2 2.8-5 5.5-5s4.9 1.8 5.5 5"/><circle cx="17" cy="9" r="2.4"/><path d="M16 14.2c2.4.1 4 1.7 4.5 4.3"/>');
I.burger = svg('<path d="M4 6h16M4 12h16M4 18h16"/>', 18);
I.panel = svg('<rect x="3" y="4" width="18" height="16" rx="2"/><path d="M9 4v16"/>', 20);
I.signout = svg('<path d="M15 4h4a1 1 0 0 1 1 1v14a1 1 0 0 1-1 1h-4"/><path d="M10 17l-5-5 5-5M5 12h11"/>');
I.eye = svg('<path d="M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>');
I.uparrow = svg('<circle cx="12" cy="12" r="9"/><path d="M12 16.5v-9M8.5 11 12 7.5l3.5 3.5"/>', 15);
I.update = svg('<path d="M12 3v12"/><path d="m7 10 5 5 5-5"/><path d="M5 21h14"/>');
I.recheck = svg('<path d="M20 11a8 8 0 0 0-14.3-4.9L4 8"/><path d="M4 3v5h5"/><path d="M4 13a8 8 0 0 0 14.3 4.9L20 16"/><path d="M20 21v-5h-5"/>');
I.bell = svg('<path d="M6 16V11a6 6 0 0 1 12 0v5l1.5 2h-15z"/><path d="M10 20a2 2 0 0 0 4 0"/>', 18);
I.plus = svg('<path d="M12 5v14M5 12h14"/>', 15);
I.copy = svg('<rect x="9" y="9" width="11" height="11" rx="1.5"/><path d="M5 15V5a1 1 0 0 1 1-1h10"/>', 14);
I.onoff = svg('<path d="M12 3v8"/><path d="M6.3 6.3a8 8 0 1 0 11.4 0"/>', 15);
I.sessions = svg('<rect x="2.5" y="4" width="13" height="10" rx="1.5"/><path d="M6 18h6M9 14v4"/><rect x="16.5" y="9" width="5" height="11" rx="1.2"/>');
I.close = svg('<path d="M6 6l12 12M18 6 6 18"/>', 14);
I.helperOk = svg('<path d="M12 3 4 6v6c0 4.5 3.4 8.3 8 9 4.6-.7 8-4.5 8-9V6z"/><path d="m9 12 2 2 4-4"/>', 16);
I.helperDown = svg('<path d="M12 3 4 6v6c0 4.5 3.4 8.3 8 9 4.6-.7 8-4.5 8-9V6z"/><path d="M12 8v4.6M12 15.6v.01"/>', 16);
I.info = svg('<circle cx="12" cy="12" r="9"/><path d="M12 11v5.5M12 7.5v.01"/>', 16);

const fmtRuntime = (s) => (s == null ? "–" : `${Math.floor(s / 60)}:${String(Math.floor(s % 60)).padStart(2, "0")}`);
const fmtIv = (v) => (v == null ? "–" : `${+v.toFixed(1)}s`);
const secsSince = (iso) => (iso ? Math.max(0, (Date.now() - new Date(iso)) / 1000) : null);

// ------------------------------------------------------------ helpers
// ---- activity: the idle sign-out (session_idle_timeout) counts a person using this page, not the page's own
// traffic. The server only counts writes and requests marked X-Fairy-Active: 1. api() marks a request when
// there was pointer, key, touch or wheel input in the last minute (loading the page counts as input); the live
// stream and every timer-driven fetch of an untouched page stay unmarked. Someone who only reads the live page
// sends nothing by themselves, so input also posts /api/me/active, at most once a minute.
const ACTIVE_MS = 60000;
let lastInput = Date.now(), lastActiveSent = 0;
const userActive = () => Date.now() - lastInput < ACTIVE_MS;
function noteInput() {
  lastInput = Date.now();
  if (!state.me || state.me.demo || signedOut || lastInput - lastActiveSent < ACTIVE_MS) return;
  lastActiveSent = lastInput;
  api("/api/me/active", { method: "POST" }).catch(() => { /* the next input tries again */ });
}
for (const ev of ["pointerdown", "pointermove", "keydown", "wheel", "touchstart"]) window.addEventListener(ev, noteInput, { capture: true, passive: true });
// ---- the session ended (signed out elsewhere, revoked, too old, idle): any 401 that is not a step-up request.
// Stop everything that would ask again (the stream, its reconnect, every timer), close what is open and show
// the sign-in view with a short notice. After this the page sends nothing until the user signs in.
let signedOut = false;
function sessionEnded() {
  if (signedOut) return;
  signedOut = true;
  const had = !!state.me;  // false: nobody was signed in on this page (first load), the plain sign-in view
  if (state.es) { state.es.close(); state.es = null; }
  for (const t of [state.timer, upd.bgT, enr.timer, streamRetry]) clearTimeout(t);
  stepUpOpen = null; state.menu = null; closeInfo(false); popClear();
  const m = $("#modal"); m.hidden = true; m.innerHTML = ""; m.onclick = null;
  $("#shell").hidden = true; $("#waiting").hidden = true; $("#flash").hidden = true;
  $("#login-note").hidden = !had;
  $("#login").hidden = false;
}

// A header value may only hold Latin-1: fetch() throws for anything else, so a host or user with a Cyrillic or
// Chinese name, or an emoji, could not be controlled at all. The label goes percent-encoded (the server decodes
// it): at most 120 characters, line breaks and broken surrogate halves replaced first.
const headerText = (v) => encodeURIComponent([...String(v).replace(/[\r\n]+/g, " ")].slice(0, 120).join("").replace(/[\uD800-\uDFFF]/gu, "\uFFFD"));
let ctlLabel = null;  // set by controlCall while it starts its request: sent as X-Fairy-Action (the step-up dialog's label)
// ---- is the panel reachable? The live data comes over one stream. While the panel cannot be reached (it is
// stopped, the network is gone) the page must not show the last data as if it were live. `net` knows when the
// page last heard from the panel: a stream event, the stream opening, an answer to a request, or an answer
// to the knock below. After NET_QUIET without any of them the page knocks (GET /healthz, at most 4 s; an
// answer below 500 is the panel's, 502 to 504 are a proxy's that cannot reach it). No answer: the strip "Panel
// not reachable" on every page, no "live", grey dots (body.unreachable), a knock every NET_KNOCK, until the
// panel answers again.
const NET_QUIET = 10000, NET_KNOCK = 5000;
const net = { heard: Date.now(), data: Date.now(), down: false, knocking: false, knocked: 0 };
function netHeard(data) {
  net.heard = Date.now();
  if (data) net.data = net.heard;
  if (!net.down) return;
  net.down = false;
  document.body.classList.remove("unreachable");
  renderAlerts();
  // the stream may be waiting out a retry of up to 30 s: connect now
  if (!state.es && !signedOut && !leaving) { clearTimeout(streamRetry); streamBackoff = 0; connect(state.watch); }
}
async function netKnock() {
  const now = Date.now();
  if (signedOut || leaving || !state.me || net.knocking || now - net.heard < NET_QUIET || now - net.knocked < NET_KNOCK) return;
  net.knocking = true; net.knocked = now;
  let ok = false;
  const ac = new AbortController(), t = setTimeout(() => ac.abort(), 4000);
  try { ok = (await fetch("/healthz", { cache: "no-store", credentials: "same-origin", signal: ac.signal })).status < 500; } catch { /* no answer */ }
  clearTimeout(t);
  net.knocking = false;
  if (ok) return netHeard(false);
  if (net.down || signedOut || leaving) return;
  net.down = true;
  document.body.classList.add("unreachable");
  renderAlerts();
}
// For a request that got, or would get, no answer: knock now instead of after NET_QUIET, and the error to show.
const NET_ASK = 3000;
function netKnockNow() { net.heard = Math.min(net.heard, Date.now() - NET_QUIET); net.knocked = 0; return netKnock(); }
const netError = (msg) => { const e = new Error(msg); e.network = true; return e; };
async function api(path, opts = {}) {
  const write = opts.method && opts.method !== "GET";
  if (write && !opts.label && ctlLabel) opts = { ...opts, label: ctlLabel };
  if (signedOut) { const e = new Error("signed out"); e.status = 401; throw e; }  // nothing is sent any more
  // A write while the page has heard nothing from the panel for NET_ASK (its stream sends every second): knock
  // first. No answer: the request is not sent at all, and the page says so in its own words.
  if (write && !opts.keepalive && (net.down || Date.now() - net.heard > NET_ASK)) {
    await netKnockNow();
    if (net.down) throw netError("Panel not reachable. Nothing was sent.");
  }
  const active = userActive();
  if (active) lastActiveSent = Date.now();  // this request tells the server: no extra signal needed for a minute
  let res;
  try {
    res = await fetch(path, { credentials: "same-origin", ...opts,
      headers: { Accept: "application/json", ...(write ? { "X-Requested-With": "fairy" } : {}), ...(active ? { "X-Fairy-Active": "1" } : {}),
        ...(opts.body && typeof opts.body === "string" ? { "Content-Type": "application/json" } : {}),
        ...(opts.label ? { "X-Fairy-Action": headerText(opts.label) } : {}), ...(opts.headers || {}) } });
  } catch (x) {
    // No answer at all (the panel was stopped a moment ago, the network is gone). Not the browser's words for it
    // ("Failed to fetch", "Load failed", "NetworkError when attempting to fetch resource."); and the strip at once.
    if (x?.name === "AbortError") throw x;
    netKnockNow();
    throw netError(write ? "Panel not reachable. No answer: it is not known whether this was done." : "Panel not reachable.");
  }
  const data = res.status === 204 ? null : await res.json().catch(() => ({}));
  if (res.status < 500) netHeard(false);
  if (!res.ok) {
    const d = data?.detail;
    // No JSON detail on a 5xx: a proxy (e.g. Cloudflare on 502/504) answered instead of the panel. The panel
    // itself sends 502 only when an agent or Proxmox cannot be reached; errors they report come as 4xx.
    const proxy = d == null && res.status >= 500 ? `HTTP ${res.status} from the proxy in front of the panel: the panel, or the host it had to reach, did not answer` : null;
    const e = new Error(d?.message || d?.[0]?.msg || (typeof d === "string" ? d : null) || proxy || `HTTP ${res.status}`);
    // A rate limit (429) says in Retry-After how long to wait: "too many control requests; try again in 42 s"
    const wait = res.status === 429 ? parseInt(res.headers.get("Retry-After"), 10) : 0;
    if (wait > 0 && !/again in \d/.test(e.message)) e.message = `${e.message.replace(/; (slow down|try again in a (minute|moment))$/, "")}; try again in ${wait} s`;
    e.status = res.status; e.stepUp = d?.code === "step_up_required"; e.detail = d;
    if (res.status === 401 && !e.stepUp) sessionEnded();
    throw e;
  }
  return data;
}
// ---- step-up re-authentication: control actions need a recent Pocket ID sign-in
const PENDING_KEY = "fairy-pending-action";
// When the server kept the refused request (detail.pending), it runs it by itself right after the sign-in and
// /api/me reports the result: the browser re-sends nothing (no dialog shown again). Otherwise (a large request,
// or a browser or address this session has not confirmed from yet) the "Resume" offer is used.
// The kept action belongs to this dialog and this browser: "Continue" asks the panel to confirm exactly it
// (POST /api/me/step-up with its id; no link carries the id, and a link to the sign-in never runs anything), and
// every way of leaving the dialog without "Continue" (Cancel, Escape, a click outside, reload, closing the tab)
// tells the server to forget it. The dialog shows what the panel itself says it will run (detail.action).
let stepUpOpen = null;  // the open dialog: { kept, going } (going: "Continue to Pocket ID" was pressed)
function showStepUp(pending, detail) {
  state.menu = null; render();
  const kept = !!detail?.pending, perAction = !!detail?.per_action;  // per_action: step_up_per_action is on
  const newClient = !kept && detail?.not_kept === "new_client";  // this browser / address has not confirmed for the session yet
  if (perAction && !kept && !newClient) { flash(`${pending.label}: cannot be confirmed (the request is too large to keep for the sign-in)`, true); return; }
  if (!kept) { try { sessionStorage.setItem(PENDING_KEY, JSON.stringify({ ...pending, ts: Date.now(), hash: location.hash })); } catch { /* ignore */ } }
  // The window is the server's: the refused request names it (window_s), /api/me carries the configured
  // step_up_window_s. A different window for this one request (the panel's own update: 2 minutes) is worded so.
  // The age of the last sign-in comes with the refusal (the server's view right now); the copy from /api/me is as
  // old as the page, so it is only a fallback, with the time since then added.
  const meAge = state.me?.auth_age_s == null ? null : state.me.auth_age_s + (Date.now() - (state.meAt || Date.now())) / 1000;
  const age = typeof detail?.auth_age_s === "number" ? detail.auth_age_s : meAge;
  const cfgWin = state.me?.step_up_window_s || 180, win = detail?.window_s || cfgWin;
  const winText = win % 60 ? `${win} seconds` : `${win / 60} minute${win === 60 ? "" : "s"}`;
  const nextPath = "/" + (location.hash || "#/");
  const runs = kept && typeof detail.action === "string" && detail.action ? `<span class="note">Fairy will run</span><b id="su-runs">${esc(detail.action)}</b>` : "";
  // An action about a list (the machines of "Update unpowered hosts"): the panel's own line for each one.
  const items = kept && Array.isArray(detail.items) && detail.items.length ? `<ul class="su-items" id="su-items">${detail.items.map((x) => `<li>${esc(x)}</li>`).join("")}</ul>` : "";
  const m = $("#modal");
  m.innerHTML = `<div class="modal-box stepup" role="dialog" aria-modal="true" aria-labelledby="stitle">
    <div class="stepup-ico">${I.shield}</div>
    <h3 id="stitle">Confirm it's you</h3>
    <p>${perAction ? "This action needs its own Pocket ID sign-in. It confirms only the action below."
      : `${win !== cfgWin ? "This action needs" : "Control actions need"} a Pocket ID sign-in from the last <b>${esc(winText)}</b>.${age != null && age < 30 * 86400 ? ` Your last sign-in was <b>${fmtUptime(age)}</b> ago.` : ""}`}</p>
    <div class="stepup-action"><span class="note">Pending action</span><b>${esc(pending.label)}</b>${runs}${items}</div>
    <p class="note" id="su-note">${kept ? "You'll sign in again at Pocket ID (passkey). Right after that, Fairy runs this action once by itself and shows you the result here. Nothing has been done yet."
      : newClient ? `This browser, or the address it now has, has not confirmed a sign-in for this session yet, so nothing was kept. Sign in again at Pocket ID (passkey) and you come straight back here to finish it${perAction ? "; it then asks once more, for this action alone" : ""}. Nothing has been sent yet.`
      : "You'll sign in again at Pocket ID (passkey) and come straight back here to finish it. Nothing has been sent yet."}</p>
    <div class="row-gap end"><button class="btn" id="su-cancel">Cancel</button>
      ${kept ? `<button class="btn primary" id="su-go">Continue to Pocket ID</button>`
        : `<a class="btn primary" id="su-go" href="/auth/login?step_up=1&amp;next=${encodeURIComponent(nextPath)}">Continue to Pocket ID</a>`}</div></div>`;
  m.hidden = false;
  stepUpOpen = { kept, going: false };
  $("#su-cancel").onclick = cancelStepUp;
  $("#su-go").onclick = kept ? () => confirmStepUp(detail.pending_id, nextPath) : () => { if (stepUpOpen) stepUpOpen.going = true; };
  m.onclick = (e) => { if (e.target === m) cancelStepUp(); };
}
// "Continue" for a kept action: ask the panel to confirm exactly this action. It answers with the address of
// the sign-in at Pocket ID (or, in the demo, with the page to show after it has run the action).
async function confirmStepUp(id, nextPath) {
  const su = stepUpOpen;
  if (!su || su.going) return;
  su.going = true;
  const go = $("#su-go"); if (go) go.disabled = true;
  let r;
  try { r = await api("/api/me/step-up", { method: "POST", body: JSON.stringify({ pending_id: String(id || ""), next: nextPath }) }); }
  catch (e) {
    if (stepUpOpen !== su) return;
    su.going = false;
    if (e.status === 409) { stepUpOpen = null; const m = $("#modal"); m.hidden = true; m.innerHTML = ""; m.onclick = null; flash(e.message, true); return; }
    if (go) go.disabled = false;
    flash(e.message, true);
    return;
  }
  if (stepUpOpen !== su || typeof r?.url !== "string") return;
  if (/^https?:\/\//.test(r.url)) { location.assign(r.url); return; }
  // anything else is an address of the panel itself ("/..."); never javascript:, data: or another scheme
  if (!r.url.startsWith("/")) { su.going = false; if (go) go.disabled = false; flash("Confirm it's you: the panel named an address this page does not open", true); return; }
  location.href = r.url; location.reload();  // demo: the same page again, loaded anew like after a real sign-in
}
// Leave "Confirm it's you" without confirming: close it and make the server forget the kept action, so a later
// sign-in runs nothing. keepalive lets the request finish while the page is going away.
function cancelStepUp() {
  const su = stepUpOpen;
  if (!su) return;
  stepUpOpen = null;
  const m = $("#modal"); m.hidden = true; m.innerHTML = ""; m.onclick = null;
  dropPending();
  if (su.kept) api("/api/me/pending", { method: "DELETE", keepalive: true }).catch(() => { /* it expires by itself */ });
}
// leaving: this page is going away (a navigation, a reload, the tab closing). Its stream breaks then, and it
// must not ask /api/me because of that: the result of an action the panel ran after "Confirm it's you" is
// handed out once, to the first /api/me that asks, and it belongs to the page that comes next.
let leaving = false;
window.addEventListener("pagehide", () => { leaving = true; if (stepUpOpen && !stepUpOpen.going) cancelStepUp(); });
window.addEventListener("pageshow", (e) => {  // Back from Pocket ID: the dialog is as it was, "Continue" works again
  leaving = false;
  if (e.persisted && stepUpOpen) { stepUpOpen.going = false; const go = $("#su-go"); if (go) go.disabled = false; }
});
// After a step-up sign-in: the result of the action the panel ran by itself (shown once).
function showPendingResult(pr) {
  if (!pr) return;
  dropPending();
  // An Add host that failed after "Confirm it's you": the dialog comes back with what was typed and the whole
  // sentence in it, as it does when the panel refuses the form at once.
  const typed = pr.path === "/api/admin/hosts" ? keptAddHost() : null;
  if (!pr.ok && typed) { openAddHost(typed, pr.message || `HTTP ${pr.status}`); return; }
  flash(`${pr.label}: ${pr.ok ? pr.message || "done" : `failed · ${pr.message || `HTTP ${pr.status}`}`}`, !pr.ok);
  if (pr.ok && pr.ref && /^\/api\/admin\/hosts(\/[^/]+\/repair)?$/.test(pr.path)) revealEnrollment(pr.ref);
  if (pr.path?.startsWith("/api/admin/updates")) upd.fastUntil = Date.now() + 30000;
  if (pr.path?.startsWith("/api/admin/wake-update") || pr.path?.endsWith("/wake-update")) fetchWakeUpdate();
}
// The action waiting for a step-up sign-in (sessionStorage: this tab only), offered as "Resume" once back.
function readPending() {
  let p = null;
  try { p = JSON.parse(sessionStorage.getItem(PENDING_KEY) || "null"); } catch { p = null; }
  return p && Date.now() - p.ts <= 10 * 60 * 1000 ? p : null;
}
function dropPending() { try { sessionStorage.removeItem(PENDING_KEY); } catch { /* ignore */ } }
const onPendingPage = (p) => (p.hash || "#/") === (location.hash || "#/");
const sameAction = (a, b) => !!a && !!b && a.kind === b.kind && (a.kind === "power" ? a.system === b.system && a.action === b.action
  : a.method === b.method && a.path === b.path && JSON.stringify(a.body ?? null) === JSON.stringify(b.body ?? null));
// An action ran (by Resume, or the user simply did it again): its Resume offer is done.
function actionDone(action) { if (sameAction(readPending(), action)) { dropPending(); renderAlerts(); } }
// Run a control call; on "step_up_required" show the interstitial instead of an error.
async function controlCall(pending, fn) {
  let r;
  pending.label ||= "Control action";
  let started;
  ctlLabel = pending.label;
  try { started = fn(); } finally { ctlLabel = null; }
  try { r = await started; }
  catch (e) { if (e.stepUp) { showStepUp(pending, e.detail); return null; } throw e; }
  actionDone(pending);
  return r;
}
function resumePending() {
  const p = readPending();
  if (!p || !onPendingPage(p)) return "";  // only on the page where it was asked for
  return `<div class="alert-strip info resume" data-key="resume">${I.shield}<b>Signed in again.</b><span>Resume: ${esc(p.label)}</span>
    <button class="btn push" data-act="resume">Continue</button><button class="btn" data-act="resume-drop">Dismiss</button></div>`;
}
async function runPending() {
  const p = readPending();
  dropPending();
  renderAlerts();  // the banner goes at once, not when the action has finished
  if (!p) return;
  const s = state.byId[p.system];
  try {
    if (p.kind === "power" && s) { await refreshCaps(s.id); openPowerDialog(s, p.action); }
    else if (p.kind === "request") { const r = await api(p.path, { method: p.method, body: JSON.stringify(p.body) }); flash(r?.message || `${p.label}: done`); }
  } catch (e) {
    const typed = p.kind === "request" && p.method === "POST" && p.path === "/api/admin/hosts" ? keptAddHost() : null;
    if (typed) openAddHost(typed, e.message); else flash(e.message, true);  // Add host: back in its dialog, as in showPendingResult
  }
  render();
  if (current().page === "users") loadAdmin();
  if (current().page === "updates") loadUpdates();
  if (current().page === "sessions") loadSessions();
  if (p.kind === "request" && p.path?.startsWith("/api/admin/updates")) { upd.fastUntil = Date.now() + 30000; updTick(); }
}
function spark(values, cls = "") {
  const pts = values.filter((v) => v != null);
  if (pts.length < 2) return `<svg class="spark ${cls}" viewBox="0 0 100 30" preserveAspectRatio="none"></svg>`;
  const lo = Math.min(...pts), hi = Math.max(...pts), span = hi - lo || 1;
  const xy = values.map((v, i) => [(i / (values.length - 1)) * 100, v == null ? null : 28 - ((v - lo) / span) * 26]).filter((p) => p[1] != null);
  const line = xy.map((p, i) => `${i ? "L" : "M"}${p[0].toFixed(2)},${p[1].toFixed(2)}`).join("");
  return `<svg class="spark ${cls}" viewBox="0 0 100 30" preserveAspectRatio="none" aria-hidden="true"><path class="area" d="${line}L${xy.at(-1)[0]},30L${xy[0][0]},30Z"/><path class="line" d="${line}" vector-effect="non-scaling-stroke"/></svg>`;
}
// Only capacity bars (RAM, disk) turn red when nearly full; high CPU/GPU utilisation or full VRAM is normal.
const barEl = (kind, p) => `<div class="bar ${kind}${(kind === "mem" || kind === "disk") && p != null && p > 92 ? " hot" : ""}"><i data-w="${(p ?? 0).toFixed(1)}"></i></div>`;
const mini = (kind, p) => (p == null ? '<span class="note">–</span>' : `<div class="mini">${barEl(kind, p)}<span>${fmtP(p)}</span></div>`);
// label is HTML; full (plain text) is what the label says in full when it may be cut: a mount path is longer than its column
const metric = (kind, label, p, text, full) => `<div class="metric"><span class="k"${full ? ` title="${esc(full)}"` : ""}>${label}</span>${barEl(kind, p)}<span class="v">${text}</span></div>`;
// CSP has no 'unsafe-inline' for styles: widths go through the CSSOM. Changing data-w on an existing bar
// animates it (CSS transition) because morph() keeps the element.
const applyWidths = (root) => root.querySelectorAll("[data-w]").forEach((el) => { const w = `${el.dataset.w}%`; if (el.style.width !== w) el.style.width = w; });
new MutationObserver(() => applyWidths(document.body)).observe(document.documentElement,
  { childList: true, subtree: true, attributes: true, attributeFilter: ["data-w"] });

// Minimal DOM morph: patch `target` to match `html`, reusing nodes. Elements with data-key are replaced when the
// key changes; [data-keep] subtrees (live tables filled by their own loaders) and form fields keep their content.
function morph(target, html) {
  const tpl = document.createElement("template");
  tpl.innerHTML = html;
  patchChildren(target, tpl.content);
}
function patchChildren(a, b) {
  const an = [...a.childNodes], bn = [...b.childNodes];
  bn.forEach((y, i) => {
    const x = an[i];
    if (!x) return a.appendChild(y);
    if (x.nodeType !== y.nodeType || x.nodeName !== y.nodeName || (x.nodeType === 1 && x.getAttribute("data-key") !== y.getAttribute("data-key"))) return a.replaceChild(y, x);
    if (x.nodeType !== 1) { if (x.nodeValue !== y.nodeValue) x.nodeValue = y.nodeValue; return; }
    if (x.namespaceURI !== "http://www.w3.org/1999/xhtml") { if (x.outerHTML !== y.outerHTML) a.replaceChild(y, x); return; }  // svg: swap
    for (const { name } of [...x.attributes]) if (name !== "style" && !y.hasAttribute(name)) x.removeAttribute(name);
    for (const { name, value } of [...y.attributes]) if (x.getAttribute(name) !== value) x.setAttribute(name, value);
    if (x.hasAttribute("data-keep") || /^(INPUT|TEXTAREA|SELECT)$/.test(x.tagName)) return;
    patchChildren(x, y);
  });
  for (let i = an.length - 1; i >= bn.length; i--) an[i].remove();
}

// ------------------------------------------------------------ the info button (ⓘ)
// A section keeps one short sentence on the page; the longer explanation is behind a small button next to it:
// infoBtn(key, about, html). A click (or Enter / Space) opens a panel with `html`, laid over the page next to the
// button, so nothing on the page moves; a second click, Escape, its × or a click anywhere else closes it. With a mouse,
// pointing at the button shows the panel as well. `html` is the caller's own markup: server values in it go
// through esc() like everywhere. The panel is #info-pop in the page (role="status": a screen reader reads it when
// it opens); it is placed with CSSOM, because the page's CSP allows no inline styles. Sentences that carry a
// safety promise or a limit stay on the page itself, never only in here; they are marked .info-keep, and with
// room for it the panel opens under the ones of its own section (at their left edge), not on top of them.
const info = { html: dict(), key: null, pinned: false, timer: null };
// o (optional): another icon, class, name and tooltip for a button that flags a state, such as "Helper stopped".
function infoBtn(key, about, html, o = {}) {
  info.html[key] = `<button class="info-x" type="button" aria-label="Close">${I.close}</button><div class="info-t">${esc(about)}</div>${html}`;
  return `<button class="info-btn${o.cls ? ` ${o.cls}` : ""}" type="button" data-info="${esc(key)}" aria-label="${esc(o.label || `More about ${about}`)}"${o.label ? ` title="${esc(o.label)}"` : ""} aria-expanded="${info.key === key}">${o.icon || I.info}</button>`;
}
const infoList = (items) => `<ul>${items.map((x) => `<li>${x}</li>`).join("")}</ul>`;
const infoBtnOf = (key) => [...document.querySelectorAll(".info-btn")].find((b) => b.dataset.info === key) || null;
function placeInfo() {
  const el = $("#info-pop"), btn = infoBtnOf(info.key);
  if (!btn) { closeInfo(false); return; }
  el.style.maxHeight = "";
  const b = btn.getBoundingClientRect(), w = el.offsetWidth, h = el.offsetHeight, edge = 8;
  if (b.bottom < 0 || b.top > innerHeight) { closeInfo(false); return; }  // its button was scrolled out of sight
  const keep = [...(btn.closest(".panel, .modal-box, .stack, #page")?.querySelectorAll(".info-keep") || [])].map((k) => k.getBoundingClientRect())
    .filter((r) => r.height && r.bottom > b.top - 4 && r.top < innerHeight);
  const fits = (top) => top >= edge && top + h <= innerHeight - edge;
  const below = b.bottom + 6, above = b.top - h - 6;
  let left = b.left - 12, top = null;
  if (keep.length) {  // under the marked sentences (a little lower than it is high: it scrolls inside), else above them
    const under = Math.max(b.bottom, ...keep.map((r) => r.bottom)) + 6, room = innerHeight - edge - under;
    const over = Math.min(b.top, ...keep.map((r) => r.top)) - h - 6;
    if (room >= Math.min(h, 240)) { top = under; left = Math.min(left, ...keep.map((r) => r.left)); if (room < h) el.style.maxHeight = `${room}px`; }
    else if (fits(over)) top = over;
  }
  if (top === null) top = fits(below) ? below : fits(above) ? above : Math.max(edge, innerHeight - h - edge);
  el.style.left = `${Math.max(edge, Math.min(left, innerWidth - w - edge))}px`;
  el.style.top = `${top}px`;
}
function closeInfo(refocus) {
  clearTimeout(info.timer);
  if (!info.key) return;
  const el = $("#info-pop"), btn = infoBtnOf(info.key);
  el.hidden = true; el.innerHTML = "";
  info.key = null; info.pinned = false;
  if (btn) { btn.setAttribute("aria-expanded", "false"); if (refocus) btn.focus(); }
}
function openInfo(btn, pinned) {
  const key = btn.dataset.info;
  if (!(key in info.html)) return;
  closeInfo(false);
  const el = $("#info-pop");
  el.innerHTML = info.html[key]; el.hidden = false; el.scrollTop = 0;
  info.key = key; info.pinned = pinned; btn.setAttribute("aria-expanded", "true");
  placeInfo();
}
document.addEventListener("click", (e) => {
  const b = e.target.closest(".info-btn");
  if (!b) {
    const x = e.target.closest(".info-x");
    if (x) e.stopPropagation();  // closing the panel is all its × does: a menu the button sits in stays open
    if (info.key && (x || !e.target.closest("#info-pop"))) closeInfo(false);
    return;
  }
  e.preventDefault(); e.stopPropagation();  // never also a click on the row or label it sits in
  clearTimeout(info.timer);
  if (info.key !== b.dataset.info) openInfo(b, true);
  else if (info.pinned) closeInfo(false);
  else info.pinned = true;  // the mouse pointer was showing it: the click keeps it open
}, true);
document.addEventListener("keydown", (e) => { if (e.key === "Escape" && info.key) { e.stopPropagation(); closeInfo(true); } }, true);
// Mouse only: pointing at the button shows the panel, moving away from both hides it again (unless a click opened it).
const canHover = matchMedia("(hover: hover)");
document.addEventListener("mouseover", (e) => {
  if (!canHover.matches || !e.target.closest) return;
  clearTimeout(info.timer);
  const b = e.target.closest(".info-btn");
  if (b) {
    const key = b.dataset.info;
    if (info.key !== key && !info.pinned) info.timer = setTimeout(() => { const x = infoBtnOf(key); if (x) openInfo(x, false); }, 250);
  } else if (info.key && !info.pinned && !e.target.closest("#info-pop")) info.timer = setTimeout(() => closeInfo(false), 300);
});
window.addEventListener("scroll", () => { if (info.key) placeInfo(); }, true);
window.addEventListener("resize", () => { if (info.key) placeInfo(); });
window.addEventListener("hashchange", () => closeInfo(false));

const linkedPower = (id) => {
  const srcs = (state.power?.sources || []).filter((s) => s.readings.some((r) => r.system_id === id));
  const w = srcs.flatMap((s) => s.readings).filter((r) => r.system_id === id && r.watts != null).reduce((a, r) => a + r.watts, 0);
  return { sources: srcs, watts: srcs.length ? w : null, history: state.history.map((h) => srcs.reduce((a, s) => a + (own(h.sources, s.id) || 0), 0) || null) };
};

// ------------------------------------------------------------ grouping: hosts by site
// Every host (Proxmox node or agent) is listed under its site (site: in config.yaml, or set on the Users page);
// without one, under its owner group, else "Other". A cluster name (real multi-node clusters only) is in the tooltip.
const nodesOf = () => state.systems.filter((s) => s.kind === "node");
const isHost = (s) => s.kind === "node" || s.kind === "agent";
const siteOf = (s) => state.sites[s.id] || s.group || s.owner || "Other";
const hostsBySite = () => {  // [[site, hosts]], sites A-Z, "Other" last
  const groups = dict();  // by site name: any name an admin can give a site, "constructor" too
  state.systems.filter(isHost).forEach((s) => (groups[siteOf(s)] ||= []).push(s));
  return Object.entries(groups).sort(([a], [b]) => (a === "Other") - (b === "Other") || a.localeCompare(b));
};
const clusterName = (s) => (s.kind === "node" && s.cluster && nodesOf().filter((n) => n.cluster_id === s.cluster_id).length > 1 ? s.cluster : "");
const guestsOf = (node) => state.systems.filter((g) => (g.kind === "qemu" || g.kind === "lxc") && g.parent_id === node.id && g.cluster_id === node.cluster_id);
const parentOf = (g) => (g.parent ? nodesOf().find((n) => n.id === g.parent_id && n.cluster_id === g.cluster_id) : null);
const guestCount = (s) => {
  const gs = s.kind === "node" ? guestsOf(s) : [];
  return gs.length ? `<span class="guest-count"><b>${gs.length}</b> guest${gs.length === 1 ? "" : "s"} · ${gs.filter((g) => g.status === "online").length} running</span>` : "";
};
// The mark of the system a host runs (s.os_icon from the panel: { key, name }; the paths are in osicons.js), drawn
// in the text colour like every other glyph, with the system's name as its name and tooltip. "" when the system
// is not known (or the mark is not in this page's osicons.js): the caller keeps what it showed before.
const osIcon = (s, size = 16) => {
  const o = s.os_icon, marks = window.FAIRY_OS_ICONS;
  const d = o && marks && typeof o.key === "string" && Object.hasOwn(marks, o.key) ? marks[o.key] : "";
  return d ? `<svg class="os-ico" width="${size}" height="${size}" viewBox="0 0 24 24" fill="currentColor" role="img" aria-label="${esc(o.name)}"><title>${esc(o.name)}</title><path d="${esc(d)}"/></svg>` : "";
};
// A host's glyph: its system's mark, else the plain device glyph (two rack units for a Proxmox node, a screen for a host with an agent).
const hostIcon = (s) => osIcon(s, 18) || (s.kind === "node" ? I.cluster : I.machine);
const upsById = (id) => (state.ups?.ups || []).find((u) => u.id === id);
const upsOf = (s) => (s.ups || []).map(upsById).filter(Boolean);
const onBattery = (u) => u.status === "on_battery" || u.status === "low_battery";
// The model shown for a host: the name an admin set in the panel ("Set model name"), else what the machine reports.
const model = (s) => s.custom_model || s.hardware?.display || null;
// Behind a custom name, never next to it: that it was set by hand, and what the machine itself reports. It is the
// tooltip of the name (modelTitle: Overview) and the text behind the info button on the host's page (modelRow).
const modelTip = (s) => `Set by hand. ${s.hardware?.display ? `Reported by the host: ${s.hardware.display}.` : "The host reports no model."}`;
const modelTitle = (s) => (s.custom_model ? `${s.custom_model} · ${modelTip(s)}` : model(s));
function modelRow(s) {
  if (!model(s)) return "";
  const bios = s.hardware?.bios_version;
  if (!s.custom_model) return `<div class="list-row"><span>Model</span><span>${esc(model(s))}${bios ? ` <span class="note">BIOS ${esc(bios)}</span>` : ""}</span></div>`;
  return `<div class="list-row"><span>Model</span><span title="${esc(modelTip(s))}">${esc(s.custom_model)}${infoBtn("host-model", "Model name",
    `<p>${esc(modelTip(s))}${bios ? ` BIOS ${esc(bios)}.` : ""}</p>`, { label: modelTip(s) })}</span></div>`;
}
const subLine = (s) => s.kind === "node"
  ? ["Proxmox node", model(s) || s.cpu_model, s.cores && `${s.cores} threads`, s.mem_total && fmtBytes(s.mem_total)].filter(Boolean).join(" · ")
  : [model(s), s.os, s.cores && `${s.cores} threads`].filter(Boolean).join(" · ");
// Host list: the UPS that feeds a host as a small sign in the family of the battery's and the helper's: an upright
// unit (not the laptop battery's lying shape), no box and no words. On mains ("online"): quiet, a bolt inside.
// Unreachable, off or not known: quiet with "?" inside, never the bolt, which would say "on mains, fine". Running on
// its battery: amber, its charge as a fill inside. Low battery: red, "!" inside. These are the chip's states
// (onBattery, low_battery). That a UPS runs on battery is also the strip at the top of every page and a
// notification. Name and tooltip: "Powered by UPS <name> · <state> · <charge> · <time left>"; a click, Enter or a
// tap opens the same in the info panel. The host's own page keeps the chip below.
function upsIcon(u) {
  const low = u.status === "low_battery", ch = Number.isFinite(u.battery_charge) ? u.battery_charge : null;
  const h = (ch == null ? 2 : Math.max(1, Math.min(4, Math.ceil(ch / 25)))) * 3;
  return svg(`<rect x="6" y="3" width="12" height="18" rx="1.5"/>${low ? '<path d="M12 8v5M12 16.4v.01"/>'
    : onBattery(u) ? `<rect x="8.6" y="${18.4 - h}" width="6.8" height="${h}" rx=".5" fill="currentColor" stroke="none"/>`
    : u.status === "online" ? '<path d="M12.6 7.5 10 12.4h4l-2.6 4.9"/>' : '<path d="M10 9.6a2.1 2.1 0 1 1 3.1 1.8c-.7.4-1.1.9-1.1 1.8M12 16.6v.01"/>'}`, 18);
}
const upsSigns = (s) => upsOf(s).map((u) => {
  const low = u.status === "low_battery", batt = onBattery(u), state = (Object.hasOwn(UPS_STATUS, u.status) && UPS_STATUS[u.status][1]) || String(u.status ?? "unknown");
  const ch = Number.isFinite(u.battery_charge) ? `${u.battery_charge.toFixed(0)}%` : "", left = u.battery_runtime_s != null ? fmtRuntime(u.battery_runtime_s) : "";
  const title = [`Powered by UPS ${u.name}`, `${state === "unknown" ? "state unknown" : state}${u.stale ? " (stale)" : ""}`, ch, batt && left ? `~${left} left` : ""].filter(Boolean).join(" · ");
  return infoBtn(`ups-${s.id}-${u.id}`, `UPS ${u.name}`, `<div class="list-row"><span>State</span><span>${esc(state)}${u.stale ? " · stale" : ""}</span></div>
    ${ch ? `<div class="list-row"><span>Charge</span><span>${ch}</span></div>` : ""}
    ${left ? `<div class="list-row"><span>Runtime${batt ? " left" : ""}</span><span>${left} <span class="note">mm:ss at current load</span></span></div>` : ""}
    ${u.load_percent != null ? `<div class="list-row"><span>Load</span><span>${u.load_percent.toFixed(0)}%${u.watts != null ? ` · ${upsWatts(u)}` : ""}</span></div>` : ""}
    <p>It powers ${esc(s.name)}. More on the <a href="#/ups">UPS page</a>.</p>`,
    { cls: `hflag ups ${low ? "bad" : batt ? "warn" : u.status === "online" ? "ok" : "na"}`, icon: upsIcon(u), label: title });
}).join("");
const upsChips = (s) => upsOf(s).map((u) => `<span class="chip ups ${u.status === "low_battery" ? "bad" : onBattery(u) ? "warn" : ""}" title="Powered by ${esc(u.name)}${onBattery(u) ? " · ON BATTERY" : ""}">${I.ups}UPS${onBattery(u) ? " · battery" : ""}</span>`).join("");
// Laptop battery (agent's `battery` block): a chip in host rows and a box on the detail page.
const BATT_STATE = table({ charging: "charging", discharging: "on battery", full: "full", idle: "plugged in, not charging", unknown: "unknown" });
const battLow = (b) => b.state === "discharging" && b.percent != null && b.percent < 20;
// Host list: the battery as a small sign in the family of the helper's and the update arrow, no box and no number.
// The fill is the charge in four steps. On mains (charging, full, plugged in) it is quiet and has a bolt beside it;
// on battery it is amber; on battery under 20 % it is red with "!" inside: the same states and the same 20 % the
// chip used for its colours (battLow), and each differs by shape as well. Its name and tooltip are the chip's
// ("Battery 78% · on battery · 2h 10m left"); a click, Enter or a tap opens the same in the info panel.
const battMains = (b) => ["charging", "full", "idle"].includes(b.state);
function battIcon(b) {
  const steps = b.percent == null ? 0 : Math.max(1, Math.min(4, Math.ceil(b.percent / 25))), mains = battMains(b);
  return `<svg width="${mains ? 23 : 18}" height="18" viewBox="0 0 ${mains ? 31 : 24} 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="2" y="7" width="17" height="10" rx="2"/><path d="M22 11v2"/>${steps && !battLow(b)
    ? `<rect x="4.7" y="9.7" width="${(steps * 2.9).toFixed(1)}" height="4.6" rx=".5" fill="currentColor" stroke="none"/>` : ""}${battLow(b)
    ? '<rect x="4.7" y="9.7" width="2.4" height="4.6" rx=".5" fill="currentColor" stroke="none"/><path d="M12.5 9.9v2.4M12.5 14.3v.01"/>' : mains ? '<path d="M28 5.5 24.6 12.5h4L27 18.5"/>' : ""}</svg>`;
}
const battChip = (s) => {
  const b = s.battery;
  if (!b || !live(s)) return "";
  const cls = battLow(b) ? "bad" : b.state === "discharging" ? "warn" : "ok";
  const left = b.time_remaining_s != null ? `${fmtUptime(b.time_remaining_s)} ${b.state === "charging" ? "to full" : "left"}` : "";
  const title = [`Battery ${fmtP(b.percent)}`, BATT_STATE[b.state] || b.state, left].filter(Boolean).join(" · ");
  return infoBtn(`batt-${s.id}`, "Battery", `<div class="list-row"><span>Charge</span><span>${fmtP(b.percent)}</span></div>
    <div class="list-row"><span>State</span><span>${esc(BATT_STATE[b.state] || b.state)}</span></div>
    ${left ? `<div class="list-row"><span>${b.state === "charging" ? "Time to full" : "Time left"}</span><span>${fmtUptime(b.time_remaining_s)}</span></div>` : ""}
    <p>Health and draw are on <a href="#/system/${enc(s.id)}">its page</a>.</p>`, { cls: `hflag ${cls}`, icon: battIcon(b), label: title });
};
function batteryBox(s) {
  const b = s.battery;
  if (!b) return "";
  const p = b.percent, w = b.power_w;
  const rate = w == null ? "" : w > 0 ? `<div class="list-row"><span>Battery draw</span><span>${fmtW(w)}</span></div>`
    : w < 0 ? `<div class="list-row"><span>Charging at</span><span>${fmtW(-w)}</span></div>` : "";
  return `<div class="panel glass pad"><h3>${I.battery} Battery</h3>
    <div class="metric"><span class="k">Charge</span><div class="bar batt${battLow(b) ? " low" : ""}"><i data-w="${(p ?? 0).toFixed(1)}"></i></div><span class="v">${fmtP(p)}</span></div>
    <div class="list-row"><span>State</span><span class="${battLow(b) ? "bad-t" : b.state === "discharging" ? "warn-t" : ""}">${esc(BATT_STATE[b.state] || b.state)}${b.ac_online != null ? ` <span class="note">· AC ${b.ac_online ? "connected" : "unplugged"}</span>` : ""}</span></div>
    ${rate}
    ${b.time_remaining_s != null ? `<div class="list-row"><span>${b.state === "charging" ? "Time to full" : "Time left"}</span><span>${fmtUptime(b.time_remaining_s)}</span></div>` : ""}
    ${b.health_pct != null ? `<div class="list-row"><span>Health</span><span>${b.health_pct.toFixed(0)}% <span class="note">of design capacity</span></span></div>` : ""}
    ${w != null && w > 0 ? '<div class="note">On battery the draw is the whole laptop\'s, measured by the battery (not wall power).</div>' : ""}</div>`;
}
// ---- the Windows agent's elevated helper (Fairy-agent v0.5.0+: Core Temp, BMC sensors, fans, power, updates).
// Shown only on hosts that need it; Linux and older agents have none.
const HELPER_STATE = table({ running: ["ok", "running"], starting: ["warn", "starting…"], stopped: ["warn", "stopped"], error: ["bad", "error"],
  not_installed: ["", "not installed"], unknown: ["", "unknown"] });
const HELPER_READING = table({ coretemp: "Core Temp", ipmi: "BMC sensor" });
const HELPER_FEATURE = table({ coretemp: "Core Temp", ipmi: "BMC sensors", fans: "fans", power: "power actions", update: "updates" });
const helperOf = (s) => (s?.helper?.needed ? s.helper : null);
const helperPaused = (s, g) => (s?.helper?.paused || []).includes(g);
const andList = (xs) => (xs.length < 2 ? xs.join("") : `${xs.slice(0, -1).join(", ")} and ${xs[xs.length - 1]}`);
// Host list: one small sign for the elevated helper, on hosts that have one. Running (or starting): a quiet shield
// with a tick. Stopped or an error: the shield with "!", amber or red, so the shape differs as well as the colour.
// Not installed or not known: nothing. It is a button like the update arrow, so its name and tooltip can be reached
// with the keyboard, and a click or tap opens the same words in the info panel. The host's own page has the state
// in words (helperBadge).
function helperChip(s) {
  const h = helperOf(s);
  if (!h || !live(s) || !["running", "starting", "stopped", "error"].includes(h.state)) return "";
  const err = h.state === "error", down = err || h.state === "stopped";
  const name = err ? "Helper error" : down ? "Helper stopped" : h.state === "starting" ? "Helper starting" : "Helper running";
  const what = err ? h.error || "no details" : down ? "its readings are paused and the fans are on the BMC's automatic control" : "";
  return infoBtn(`helper-${s.id}`, name, `<p>The elevated helper of ${esc(s.name)} ${err ? "reports an error" : down ? "is stopped" : h.state === "starting" ? "is starting" : "is running"}${what ? `: ${esc(what)}` : ""}.</p>
    <p>Details and Stop / Start are on <a href="#/system/${enc(s.id)}">its page</a>.</p>`, { cls: `hflag ${err ? "bad" : down ? "warn" : "ok"}`, icon: down ? I.helperDown : I.helperOk, label: what ? `${name}: ${what}` : name });
}
// Host page, next to the name: the state, opening a small menu with the details and Stop / Start.
function helperBadge(s, open) {
  const h = helperOf(s);
  if (!h || !live(s)) return "";
  const [cls, label] = HELPER_STATE[h.state] || HELPER_STATE.unknown;
  const tip = h.state === "error" ? `Error: ${h.error || "no details"}` : "The agent's elevated helper: details, Stop / Start";
  return `<div class="menu-wrap"><button class="chip hbadge ${cls} ${open ? "on" : ""}" type="button" data-act="menu" data-menu="helper" aria-haspopup="menu" aria-expanded="${!!open}"
    title="${esc(tip)}">Elevated helper: ${label}</button>${open ? helperMenu(s, h) : ""}</div>`;
}
function helperMenu(s, h) {
  const why = controlWhyNot(), down = h.state === "stopped" || h.state === "error";
  const rows = [["State", `${(HELPER_STATE[h.state] || HELPER_STATE.unknown)[1]}${h.since ? ` · for ${fmtUptime(secsSince(h.since))}` : ""}`],
    ["Does", (h.features || []).map((f) => HELPER_FEATURE[f]).filter(Boolean).join(", ") || "–"],
    ...((h.features || []).includes("fans") ? [["Fan watchdog", h.watchdog ? "on" : "off"]] : []),
    ...(h.version ? [["Version", h.version]] : [])];
  const act = h.state === "not_installed" ? '<div class="menu-note">Not installed: the agent\'s Windows installer (v0.5.0 or newer) sets it up.</div>'
    : `<button class="menu-item ${down ? "" : "danger"}" data-act="helper" data-action="${down ? "start" : "stop"}" ${why || h.state === "starting" ? "disabled" : ""}>
      ${down ? "Start helper…" : "Stop helper…"}<span class="note">${h.state === "starting" ? "starting" : "type the name"}</span></button>`;
  return `<div class="menu helper" role="menu"><div class="menu-title">Elevated helper · ${esc(s.name)}</div>
    ${rows.map(([k, v]) => `<div class="menu-row"><span>${k}</span><span>${esc(v)}</span></div>`).join("")}
    ${h.state === "error" && h.error ? `<div class="menu-note bad">${esc(h.error)}</div>` : ""}
    <div class="menu-sep"></div>${act}${why ? `<div class="menu-note">${esc(why)}</div>` : ""}</div>`;
}
// One line in the Sensors box instead of empty boxes: what is paused because the helper is not running.
function helperPausedLine(s) {
  const h = s.helper, groups = h?.paused || [];
  if (!groups.length) return "";
  const readings = groups.map((g) => HELPER_READING[g]).filter(Boolean);
  const why = h.state === "starting" ? "helper starting" : h.state === "error" ? "helper error" : "helper stopped";
  const text = `${readings.length ? `${andList(readings)} readings` : "Fan control"} paused — ${why}${groups.includes("fans") ? "; fans on the BMC's automatic control" : ""}`;
  const btn = h.state === "starting" ? "" : `<button class="btn sm" type="button" data-act="helper" data-action="start" ${canControl() ? "" : `disabled title="${esc(controlWhyNot() || "only admins can start it")}"`}>Start</button>`;
  return `<div class="paused-line"><span>${esc(text)}</span>${btn}</div>`;
}
const statusText = (s) => (s.status === "paused" ? "paused" : s.asleep ? "asleep (from panel)" : s.status);

// ------------------------------------------------------------ sidebar
function navLink(href, ico, name, cls = "", extra = "") {
  const active = location.hash === href || (href !== "#/" && location.hash.startsWith(href + "/"));
  return `<a class="nav-link ${cls} ${active ? "active" : ""}" href="${href}" title="${esc(name)}">${ico}<span class="name">${esc(name)}</span>${extra}</a>`;
}
const sysLink = (s, cls) => navLink(`#/system/${enc(s.id)}`, `<span class="ico"><span class="dot ${cssName(s.status)}"></span></span>`, s.name, cls,
  `<span class="cpu">${s.status === "paused" ? "paused" : live(s) && s.cpu_percent != null ? fmtP(s.cpu_percent) : ""}</span>`);

function renderNav() {
  let html = navLink("#/", `<span class="ico">${I.overview}</span>`, "Overview");
  const un = state.notes.unread;
  html += navLink("#/notifications", `<span class="ico">${I.bell}</span>`, "Notifications", state.notes.active_alerts ? "alarm" : "",
    `<span class="cpu">${un ? `${esc(un)} new` : ""}</span>`);
  html += `<div class="nav-sec"><div class="nav-title"><span class="ico">${I.machine}</span><span>Hosts</span></div>`;
  const cur = state.byId[current().id], curParent = cur ? parentOf(cur) : null;
  for (const [site, hosts] of hostsBySite()) {
    html += `<div class="nav-sub">${esc(site)}</div>`;
    for (const h of hosts) {
      const gs = h.kind === "node" ? guestsOf(h) : [];
      if (!gs.length) { html += `<div class="nav-host no-chev">${sysLink(h, "sub")}</div>`; continue; }
      const open = !!state.open[h.id] || curParent?.id === h.id;
      html += `<div class="nav-host"><button class="chev" type="button" data-host-toggle="${esc(h.id)}" aria-expanded="${open}"
        aria-label="${esc(`${open ? "Hide" : "Show"} the ${gs.length} guests of ${h.name}`)}" title="${gs.length} guests">${I.caret}</button>${sysLink(h, "sub")}</div>`;
      if (open) html += `<div class="nav-guests">${gs.map((g) => sysLink(g, "sub2")).join("")}</div>`;
    }
  }
  html += "</div>";
  const ups = state.ups?.ups || [], batt = ups.filter(onBattery);
  html += `<div class="nav-sec">${navLink("#/power", `<span class="ico">${I.power}</span>`, "Power", "",
    `<span class="cpu">${state.power?.total_w != null ? fmtW(state.power.total_w) : ""}</span>`)}
    ${ups.length ? navLink("#/ups", `<span class="ico">${I.ups}</span>`, "UPS", batt.length ? "alarm" : "",
      `<span class="cpu">${batt.length ? "on battery" : `${ups.length}`}</span>`) : ""}
      </div>`;
  const nav = $("#nav"), keep = nav.scrollTop;
  morph(nav, html);
  nav.scrollTop = keep;
}

// ------------------------------------------------------------ global alert strip (UPS on battery, pending shutdowns)
function renderAlerts() {
  const out = [];
  if (net.down) out.push(`<div class="alert-strip warn static" data-key="net-down" role="status">${I.recheck}<b>Panel not reachable</b>
    <span>last data <span data-ago="${esc(new Date(net.data).toISOString())}">${esc(ago(net.data))}</span></span></div>`);
  const off = controlOff();
  if (off) out.push(`<div class="alert-strip bad static" data-key="control-off">${I.onoff}<b>Control is disabled</b>
    <span>by ${esc(off.by)} · ${esc(ago(off.at))}${off.reason ? ` · “${esc(off.reason)}”` : ""}</span>
    <span class="note">fan, power, Wake-on-LAN and polling changes are refused for everyone · re-enable on the server: <code>fairy admin control enable</code></span></div>`);
  const resume = resumePending();
  if (resume && !off) out.push(resume);
  const ub = updBanner();
  if (ub) out.push(ub);
  for (const u of (state.ups?.ups || []).filter(onBattery)) {
    const powered = u.powers.map((id) => state.byId[id]?.name || id);
    // charge and run time only when the UPS gives them: without them the strip read "– · ~– left"
    const left = [Number.isFinite(u.battery_charge) ? `${u.battery_charge.toFixed(0)}%` : "", Number.isFinite(u.battery_runtime_s) ? `~${fmtRuntime(u.battery_runtime_s)} left` : ""].filter(Boolean).join(" · ");
    out.push(`<a class="alert-strip ${u.status === "low_battery" ? "bad" : "warn"}" href="#/ups" data-key="ups-${esc(u.id)}">${I.ups}
      <b>${esc(u.name)}</b><span>${u.status === "low_battery" ? "LOW BATTERY" : "on battery"}</span>
      ${left ? `<span>${esc(left)}</span>` : ""}
      ${u.on_battery_since ? `<span class="note">for ${fmtRuntime(secsSince(u.on_battery_since))}</span>` : ""}
      ${powered.length ? `<span class="note">powers ${esc(powered.join(", "))}</span>` : ""}<span class="push">details →</span></a>`);
  }
  for (const s of state.systems.filter(broken)) {
    out.push(`<a class="alert-strip bad static" href="#/system/${enc(s.id)}" data-key="err-${esc(s.id)}">${I.machine}<b>${esc(s.name || s.id)}</b>
      <span>cannot be shown: its data is not usable</span><span class="note">${esc(brokenWhy(s))}</span><span class="push">open →</span></a>`);
  }
  const here = current().id;  // the system page shows its own countdown strip
  for (const s of state.systems.filter((x) => x.power_pending && x.id !== here)) {
    const p = s.power_pending, due = pendingDue(p);
    out.push(`<a class="alert-strip info" href="#/system/${enc(s.id)}" data-key="pend-${esc(s.id)}">${I.onoff}<b>${esc(s.name)}</b>
      ${due < 0 ? `<span>${esc(p.action)} in <b data-countdown="${esc(p.at)}">${fmtRuntime(-due)}</b></span>
      <span class="note">requested by ${esc(p.requested_by || "?")}</span>` : `<span>${esc(p.action)} requested</span>${pendingLate(s, due)}`}<span class="push">open →</span></a>`);
  }
  morph($("#alerts"), out.join(""));
}

// A shutdown, reboot or sleep with a countdown (power_pending, from the host's agent). When the countdown has run
// out the agent has asked its system for the action, and goes on reporting it for a minute. From then on the page
// does not say "in 0:00" and offers no Cancel (there is nothing left to cancel): it says "requested", and when the
// machine still answers PEND_LATE seconds later, that it is still on. The line goes when the agent stops reporting
// the action or the host stops answering, as before.
const PEND_LATE = 20;  // as long as a run waits before it takes a machine for off (wakerun, offConfirm)
const pendingDue = (p) => (Date.now() - new Date(p.at)) / 1000;  // seconds since it was due; negative while it counts down
// "by Test admin", and from PEND_LATE on " · 25s ago · still on" (the host still answers)
const pendingLate = (s, due) => `<span class="note">by ${esc(s.power_pending.requested_by || "?")}${due >= PEND_LATE && s.status === "online"
  ? ` · <span data-ago="${esc(s.power_pending.at)}">${ago(s.power_pending.at)}</span></span><b>still on</b>` : "</span>"}`;
function pendingStrip(s, why) {
  const p = s.power_pending, due = pendingDue(p);
  if (due >= 0) return `<div class="pending-strip" data-key="due"><span><b>${esc(POWER_LABEL[p.action])}</b> requested</span>${pendingLate(s, due)}</div>`;
  return `<div class="pending-strip"><b>${esc(POWER_LABEL[p.action])}</b> in <b data-countdown="${esc(p.at)}">${fmtRuntime(-due)}</b>
      <span class="note">requested by ${esc(p.requested_by || "?")}</span>
      <button class="btn push" data-act="power-now" data-action="cancel" ${why ? "disabled" : ""}>Cancel</button></div>`;
}
// Once a second (the page's ticker): the second a countdown runs out its line is drawn anew, also when no data arrives.
function pendingTick() {
  if ([...document.querySelectorAll(".pending-strip [data-countdown], #alerts [data-countdown]")].some((el) => new Date(el.dataset.countdown) <= Date.now())) render();
}

// ------------------------------------------------------------ pages
function setTitle(t, sub = "") { $("#page-title").textContent = t; $("#page-sub").textContent = sub; document.title = `${t} · Fairy`; }

const watts = (s) => s.power_w ?? linkedPower(s.id).watts;
const gpuAvg = (s) => ((s.gpus || []).length ? s.gpus.reduce((a, g) => a + (g.util_percent || 0), 0) / s.gpus.length : null);

// One line per host, and under the name, small, the machine's model when it is known (only the model: threads,
// memory, the system and the agent's version are on the host's own page, and here in the name's tooltip).
// The name never wraps; in a narrow window its chips wrap next to it instead of pushing the table out of its panel.
// Only chips that say something about now are in the row (UPS, battery, helper stopped, update available); the
// cluster's name and the agent's version are on the host's page, and in the tooltip.
function hostRow(s) {
  const gpus = s.gpus || [], w = watts(s), ver = state.me?.can_control && upd.bySys[s.id]?.version;
  const about = [subLine(s), clusterName(s) && `cluster ${clusterName(s)}`, ver && `fairy-agent ${ver}${upd.bySys[s.id].version_last_seen ? " (last seen)" : ""}`].filter(Boolean).join(" · ");
  return `<tr class="row host ${live(s) && s.status !== "paused" ? "" : "dim"}" data-key="${esc(s.id)}" data-href="#/system/${enc(s.id)}"><td class="namecell"><div class="host-line"><span class="host-ico">${hostIcon(s)}</span><div class="host-main">
      <div class="host-l1"><a href="#/system/${enc(s.id)}" title="${esc(about)}"><span class="dot ${cssName(s.status)}"></span>${esc(s.name)}</a><span class="host-tags">${upsSigns(s)}${battChip(s)}${helperChip(s)}${guestCount(s)}${verChip(s.id, true)}</span></div>
      ${model(s) ? `<div class="host-model" title="${esc(modelTitle(s))}">${esc(model(s))}</div>` : ""}</div></div></td>
    <td class="col-kind"><span class="note">${esc(s.kind === "node" ? "Proxmox" : s.owner || s.group || "–")}</span></td>
    <td class="col-cpu" data-l="CPU">${s.status === "paused" ? '<span class="note">paused</span>' : live(s) ? mini("cpu", s.cpu_percent) : `<span class="note">${s.asleep ? "asleep (from panel)" : `${esc(s.status)}${s.error ? ` · ${esc(s.error)}` : ""}`}</span>`}</td>
    <td class="col-ram" data-l="RAM">${live(s) && s.status !== "paused" ? mini("mem", pct(s.mem_used, s.mem_total)) : ""}</td>
    <td class="col-gpu" data-l="GPU">${live(s) && s.status !== "paused" && gpus.length ? mini("gpu", gpuAvg(s)) + (gpus.length > 1 ? `<span class="note gpu-n" title="${gpus.length} GPUs: the bar is their average">×${gpus.length}</span>` : "") : ""}</td>
    <td class="num">${w != null ? `<b class="value pwr">${fmtW(w)}</b>` : '<span class="note">–</span>'}</td></tr>`;
}
// In a panel narrower than the six columns need (a narrow window, a phone) every host is two lines instead: name
// and chips, then its values with their names in front (app.css, @container hosts).
const hostTable = (groups) => `<div class="panel glass scroll hosts-panel"><table class="table hosts"><thead><tr><th>Host</th><th class="col-kind">Owner</th>
  <th>CPU</th><th>RAM</th><th class="col-gpu">GPU</th><th class="num">Wall power</th></tr></thead><tbody>${groups.map(([title, rows]) =>
  (title ? `<tr class="group" data-key="g-${esc(title)}"><td colspan="6">${esc(title)}</td></tr>` : "") + rows.map(hostRow).join("")).join("")}</tbody></table></div>`;

function guestRow(s, showNode = true) {
  return `<tr class="row guest ${live(s) && s.status !== "paused" ? "" : "dim"}" data-key="${esc(s.id)}" data-href="#/system/${enc(s.id)}"><td class="namecell"><a href="#/system/${enc(s.id)}"><span class="dot ${cssName(s.status)}"></span>${esc(s.name)}</a></td>
    <td><span class="chip ${cssName(s.kind)}">${esc(kindLabel(s))}</span> <span class="note">#${esc(s.vmid)}</span></td>
    ${showNode ? `<td class="col-kind"><span class="note">${esc(s.parent)}</span></td>` : ""}
    <td>${s.status === "paused" ? '<span class="note">paused</span>' : live(s) ? mini("cpu", s.cpu_percent) : `<span class="note">${esc(s.status)}</span>`}</td>
    <td>${live(s) && s.status !== "paused" ? mini("mem", pct(s.mem_used, s.mem_total)) : ""}</td>
    <td class="col-disk">${live(s) && s.status !== "paused" && s.disk_total ? mini("disk", pct(s.disk_used, s.disk_total)) : ""}</td>
    <td class="num">${live(s) ? `<span class="note">${fmtUptime(s.uptime_s)}</span>` : ""}</td></tr>`;
}
const guestTable = (groups, showNode = true) => `<div class="panel glass scroll"><table class="table guests"><thead><tr><th>Guest</th><th>Type</th>
  ${showNode ? '<th class="col-kind">Node</th>' : ""}<th>CPU</th><th>RAM</th><th class="col-disk">Disk</th><th class="num">Uptime</th></tr></thead><tbody>${groups.map(([title, rows]) =>
  (title ? `<tr class="group" data-key="g-${esc(title)}"><td colspan="7">${esc(title)}</td></tr>` : "") + rows.map((r) => guestRow(r, showNode)).join("")).join("")}</tbody></table></div>`;

// One line instead of a list: the alarms that still hold (host offline, too hot, UPS on battery...) are on the
// Notifications page.
function alertSummary() {
  const n = state.notes.active_alerts;
  if (!n) return "";
  return `<a class="alert-sum" href="#/notifications">${I.bell}<b>${esc(n)}</b> active alert${n === 1 ? "" : "s"} →</a>`;
}
// "5 hosts", or "4 of 5 online" when some are offline (they do not add to the CPU / memory totals).
const hostsOf = (t) => (t.hosts_online === t.hosts ? `${t.hosts} host${t.hosts === 1 ? "" : "s"}` : `${t.hosts_online} of ${t.hosts} online`);
function pageOverview() {
  setTitle("Overview");
  const sm = state.summary, t = sm?.totals;
  if (!sm) return '<div class="empty">Waiting for the first poll…</div>';
  const hostGroups = hostsBySite();
  const ups = state.ups?.ups || [];
  return `<div class="tiles">
    ${t?.hosts ? `<div class="panel glass tile"><div class="label">CPU <span class="label-sub" title="Offline hosts do not add to the CPU and memory totals">all hosts · ${esc(hostsOf(t))}</span></div><div class="value">${fmtP(t.cpu_percent)}<small> of ${esc(t.cores)} threads</small></div>${barEl("cpu", t.cpu_percent)}</div>
    <div class="panel glass tile"><div class="label">Memory <span class="label-sub">all hosts</span></div><div class="value">${t.mem_total ? `${fmtBytes(t.mem_used)}<small> / ${fmtBytes(t.mem_total)}</small>` : `–<small> ${esc(hostsOf(t))}</small>`}</div>${barEl("mem", pct(t.mem_used, t.mem_total))}</div>` : ""}
    <div class="panel glass tile"><div class="label">Systems</div><div class="value">${esc(sm.counts.online)}<small> / ${esc(sm.counts.total)} online</small></div>
      <div class="counts"><span><span class="dot online"></span><b>${esc(sm.counts.online)}</b></span><span><span class="dot"></span><b>${esc(sm.counts.stopped)}</b></span>
      <span><span class="dot stale"></span><b>${esc(sm.counts.stale)}</b></span><span><span class="dot offline"></span><b>${esc(sm.counts.offline)}</b></span>
      ${sm.counts.paused ? `<span><span class="dot paused"></span><b>${esc(sm.counts.paused)}</b> paused</span>` : ""}</div></div>
    <div class="panel glass tile"><div class="label">Wall power${ups.length ? ` · ${ups.length} UPS` : ""}</div><div class="value pwr">${fmtW(state.power?.total_w)}</div>${spark(state.history.map((p) => p.total_w))}</div>
  </div>
  ${alertSummary()}
  <div class="section-title overview-head">Hosts <span class="count">${hostGroups.reduce((a, [, h]) => a + h.length, 0)}</span>${infoBtn("ov-hosts", "Hosts", infoList([
      "A click on a host opens its page.",
      "A Proxmox host's VMs and containers are on its page, and under it in the sidebar (the arrow).",
      "A host's threads, memory and system are on its page. With a mouse, point at its name to see them here.",
      "Offline hosts do not add to the CPU and memory totals."]))}
    ${state.me?.can_manage_users ? `<button class="btn push" type="button" data-act="add-host" ${controlOff() ? `disabled title="${esc(controlWhyNot())}"` : ""}>${I.plus}Add host</button>` : ""}</div>${hostTable(hostGroups)}`;
}

// ---- polling menu (⋯) and power menu on the detail page
const POLL_CHOICES = [[1, "1s"], [2, "2s"], [5, "5s"], [10, "10s"], [30, "30s"], ["paused", "Paused"], ["default", "Default"]];
function liveLabel(s) {
  if (s.status === "paused" || s.poll_mode === "paused") return '<span class="live paused" title="Polling paused for this host">paused</span>';
  if (!live(s)) return "";
  return `<span class="live" title="${s.boosted ? "Boosted to 1 s while this page is open" : `Polled every ${fmtIv(s.poll_interval_s)}`}"><span class="pulse"></span>live · ${fmtIv(s.poll_interval_s)}${s.boosted ? " ⇡" : ""}</span>`;
}
// The ⋯ menu of a host: headings and items only. What used to be lines and notes in it (each source's own rate, the
// boost while viewing, why control is off) is behind the info button next to "Polling rate", and on the items' tooltips.
function pollMenu(s) {
  const st = state.poll[s.id], why = controlWhyNot();
  const cur = s.poll_mode === "paused" ? "paused" : s.poll_mode === "default" ? "default" : st?.requested_s;
  const more = [...(why ? [esc(why)] : []),
    ...(st ? [...st.jobs.map((j) => `<code>${esc(j.job)}</code>: ${j.paused ? "paused" : `every ${fmtIv(j.effective_s)}`}${j.clamped ? ` (this source never polls faster than ${esc(j.floor_s)}s)` : ""}`),
      ...(st.note ? [esc(st.note)] : []), st.boost_enabled ? "Viewing a host boosts it to 1 s (30 s grace)." : "Boost while viewing is off."] : ["loading…"]),
    "Default is the rate from config.yaml."];
  return `<div class="menu poll" role="menu"><div class="menu-title">Polling rate${infoBtn("poll-menu", "Polling rate", infoList(more))}</div>
    ${POLL_CHOICES.map(([v, label]) => `<button class="menu-item ${String(cur) === String(v) ? "on" : ""}" data-act="poll" data-v="${v}" ${why ? `disabled title="${esc(why)}"` : ""}>${label}${v === "default" ? '<span class="note">config</span>' : ""}</button>`).join("")}
    ${modelNameMenu(s)}${wakeUpdateMenu(s)}</div>`;
}
const POWER_LABEL = table({ shutdown: "Shut down", reboot: "Reboot", sleep: "Sleep", cancel: "Cancel pending", start: "Start", stop: "Stop (hard)", wake: "Power on (Wake-on-LAN)", bmc_on: "Power on (via BMC)" });
function powerMenu(s) {
  const caps = state.caps[s.id], why = controlWhyNot();
  if (!caps) return '<div class="menu"><div class="menu-note">loading…</div></div>';
  return `<div class="menu power" role="menu"><div class="menu-title">Power · ${esc(s.name)}</div>
    ${caps.actions.map((a) => `<button class="menu-item ${a.action === "stop" || a.action === "shutdown" ? "danger" : ""}" data-act="power" data-action="${esc(a.action)}" ${a.available && !why ? "" : "disabled"} title="${esc(a.reason || "")}">
      ${esc(POWER_LABEL[a.action])}${a.available ? `<span class="note">${esc(a.via_label ? `via ${a.via_label}` : a.via || "")}${a.delayed ? " · countdown" : ""}</span>` : ""}</button>
      ${!a.available && a.reason ? `<div class="menu-why">${esc(a.reason)}</div>` : ""}`).join("")}
    ${why ? `<div class="menu-note">${esc(why)}</div>` : ""}
    ${caps.warning ? `<div class="menu-note warn">${esc(caps.warning)}</div>` : ""}</div>`;
}

function pageSystem(id, tab) {
  const s = state.byId[id];
  if (!s) { setTitle("Not found"); return '<div class="empty">Unknown system.</div>'; }
  if (broken(s)) {  // nothing else of it is known: one card that says so
    setTitle(s.name || id, "data error");
    return `<div class="panel glass pad errcard" data-key="err"><h3><span class="dot error"></span> ${esc(s.name || id)} cannot be shown</h3>
      <p>Fairy got data for this ${esc(kindLabel(s) || "system")} that it cannot use: <b>${esc(brokenWhy(s))}</b></p>
      <p class="note">The other systems are not affected. This card stays until the system sends usable data again; check the agent (or Proxmox) on that machine.</p></div>`;
  }
  setTitle(s.name, s.kind === "agent" ? s.group || "" : s.parent ? `on ${s.parent}` : s.cluster || s.group || "");
  const tabs = [["overview", "Overview"], ...(s.processes_available || s.kind === "agent" ? [["processes", "Processes"]] : []), ...(s.fan_control ? [["fans", "Fans"]] : [])];
  if (!tabs.some(([k]) => k === tab)) tab = "overview";
  const sub = s.kind === "agent" || s.kind === "node" ? [subLine(s), s.kind === "agent" && s.owner && `owner ${s.owner}`].filter(Boolean).join(" · ")
    : `${kindLabel(s)} #${s.vmid} on ${s.parent}${s.cluster ? ` · ${s.cluster}` : ""}`;
  const menuOpen = state.menu?.id === id ? state.menu.kind : null;
  const why = controlWhyNot();
  const hasPower = (s.power_actions || []).length > 0;
  const head = `<div class="detail-head"><div class="dh-title"><span class="dot ${cssName(s.status)}"></span>${osIcon(s, 22)}<h2>${esc(s.name)}</h2>
    <span class="chip ${s.status === "online" ? "ok" : s.status === "stale" ? "warn" : s.status === "offline" ? "bad" : ""}">${esc(statusText(s))}</span>
    ${helperBadge(s, menuOpen === "helper")}</div>
    <div class="actions">${liveLabel(s)}
      ${s.dwservice_url ? `<a class="btn" href="${safeHref(s.dwservice_url)}" target="_blank" rel="noopener noreferrer">Remote ${I.ext}</a>` : ""}
      <div class="menu-wrap"><button class="btn ${menuOpen === "power" ? "on" : ""}" data-act="menu" data-menu="power" ${hasPower ? "" : "disabled"}
        title="${esc(hasPower ? why || "Power actions" : "No power control configured for this system")}">${I.onoff}Power</button>${menuOpen === "power" ? powerMenu(s) : ""}</div>
      ${state.me?.can_control && upd.bySys[s.id] && (upd.bySys[s.id].behind === true || updBusy(upd.bySys[s.id]))
        ? `<button class="btn" type="button" data-act="upd-pop" data-sys="${esc(s.id)}" aria-haspopup="dialog" aria-expanded="${upd.popSys === s.id}">${I.update}${updBusy(upd.bySys[s.id]) ? "Updating…" : "Update agent"}</button>` : ""}
      <div class="menu-wrap"><button class="btn icon ${menuOpen === "poll" ? "on" : ""}" data-act="menu" data-menu="poll" title="Polling rate and host settings" aria-label="Polling rate and host settings">${I.more}</button>${menuOpen === "poll" ? pollMenu(s) : ""}</div>
    </div>
    <div class="dh-meta"><span class="chip">${esc(kindLabel(s))}</span>${s.kind === "node" && !s.cluster ? '<span class="chip">Proxmox</span>' : ""}
      ${upsChips(s)}${(s.tags || []).map((t) => `<span class="chip">${esc(t)}</span>`).join("")}${verChip(s.id)}</div>
    <div class="sub">${esc(sub)}${s.updated_at ? ` · <span class="note">updated <span data-ago="${esc(s.updated_at)}">${ago(s.updated_at)}</span></span>` : ""}</div></div>
    ${s.power_pending ? pendingStrip(s, why) : ""}
    <nav class="tabs">${tabs.map(([k, label]) => `<a href="#/system/${enc(id)}${k === "overview" ? "" : "/" + k}" class="${k === tab ? "active" : ""}">${label}</a>`).join("")}</nav>`;
  if (tab === "processes") return head + '<div id="tab-body" data-keep><div class="empty">Loading processes…</div></div>';
  if (tab === "fans") return head + '<div id="tab-body" data-keep><div class="empty">Loading fan status…</div></div>';
  return head + systemOverview(s);
}

function wakeBlock(s) {
  return wolBlock(s) + bmcBlock(s);
}
// "Power on (via BMC, through AGENT-2)": a relay agent powers the machine on through its BMC (typed name).
function bmcBlock(s) {
  if (!(s.power_actions || []).includes("bmc_on") || !(s.status === "offline" || s.status === "stale")) return "";
  const cap = state.caps[s.id]?.actions.find((a) => a.action === "bmc_on"), why = controlWhyNot();
  const label = `Power on (via BMC, through ${cap?.via_label || cap?.via?.replace("bmc:", "") || "relay"})`;
  return `<div class="wake"><button class="btn primary" data-act="power" data-action="bmc_on" ${cap?.available && !why ? "" : "disabled"}>${I.onoff}${esc(label)}</button>
    <div class="note">${esc(why || (cap && !cap.available ? cap.reason : "An always-on agent that can reach this machine's BMC asks the BMC to power it on."))}</div></div>`;
}
function wolBlock(s) {
  if (!(s.power_actions || []).includes("wake") || !(s.status === "offline" || s.status === "stale")) return "";
  const cap = state.caps[s.id]?.actions.find((a) => a.action === "wake"), why = controlWhyNot();
  const label = `Power on (WoL via ${cap?.via_label || cap?.via?.replace("wol:", "") || "relay"})`;
  return `<div class="wake"><button class="btn primary" data-act="power-now" data-action="wake" ${cap?.available && !why ? "" : "disabled"}>${I.onoff}${esc(label)}</button>
    <div class="note">${esc(why || (cap && !cap.available ? cap.reason : `Sends a magic packet from an always-on agent on the same LAN.${s.asleep ? " From sleep this needs the network card's \"Allow this device to wake the computer\" / \"Wake on Magic Packet\"." : ""}`))}</div></div>`;
}

function systemOverview(s) {
  if (!live(s)) {
    return `<div class="empty">${s.status === "stopped" ? "Stopped." : s.asleep ? `Asleep (sent to sleep from the panel) · last seen ${ago(s.last_seen)}` : `No data · ${esc(s.error || s.status)} · last seen ${ago(s.last_seen)}`}${wakeBlock(s)}</div>${managedBox(s)}`;
  }
  const lp = linkedPower(s.id), cp = s.component_power;
  const res = `<div class="panel glass pad"><h3>Resources</h3>
    ${metric("cpu", "CPU", s.cpu_percent, `${fmtP(s.cpu_percent)}${s.cores ? ` · ${esc(s.cores)} threads` : ""}`)}
    ${metric("mem", "Memory", pct(s.mem_used, s.mem_total), `${fmtBytes(s.mem_used)} / ${fmtBytes(s.mem_total)}`)}
    ${s.disk_total ? metric("disk", "Disk", pct(s.disk_used, s.disk_total), `${fmtBytes(s.disk_used)} / ${fmtBytes(s.disk_total)}`) : ""}
    ${(s.disks || []).length > 1 ? s.disks.map((d) => metric("disk", esc(d.mount), pct(d.used, d.total), `${fmtBytes(d.used)} / ${fmtBytes(d.total)}`, d.mount)).join("") : ""}
    ${s.net_rx_bps != null || s.net_tx_bps != null ? `<div class="list-row"><span>Network</span><span>↓ ${fmtRate(s.net_rx_bps)} · ↑ ${fmtRate(s.net_tx_bps)}</span></div>` : ""}
    ${s.cpu_model ? `<div class="list-row"><span>Processor</span><span>${esc(s.cpu_model)}</span></div>` : ""}
    ${modelRow(s)}
    <div class="list-row"><span>Uptime</span><span>${fmtUptime(s.uptime_s)}</span></div>
    ${s.load_avg ? `<div class="list-row"><span>Load</span><span>${esc([].concat(s.load_avg).join(" · "))}</span></div>` : ""}
    <div class="list-row"><span>Last change</span><span data-ago="${esc(s.updated_at || s.last_seen)}">${ago(s.updated_at || s.last_seen)}</span></div></div>`;
  const ups = upsOf(s);
  const power = `<div class="panel glass pad"><h3>Power</h3>
    ${lp.watts != null ? `<div class="big-w">${fmtW(lp.watts)} <small>wall · ${lp.sources.map((x) => esc(x.name)).join(", ")}</small></div>${spark(lp.history)}` : '<div class="note">No wall-power source linked.</div>'}
    ${ups.map((u) => `<div class="list-row"><span>${I.ups} UPS · <a href="#/ups">${esc(u.name)}</a></span><span class="${onBattery(u) ? "warn-t" : ""}">${esc(u.status.replace("_", " "))} · ${u.battery_charge?.toFixed(0) ?? "–"}% · ${fmtRuntime(u.battery_runtime_s)}</span></div>`).join("")}
    ${cp ? [["CPU package", cp.cpu_package_w], ["GPU", cp.gpu_w], ["Battery", cp.battery_w != null && !s.battery ? cp.battery_w : null], ["BMC (in-band)", cp.bmc_w]]
      .filter(([, v]) => v != null).map(([k, v]) => `<div class="list-row"><span>${k}${k === "Battery" && cp.battery_state ? ` · ${esc(cp.battery_state)}` : ""}</span><span>${fmtW(v)}</span></div>`).join("")
      + (cp.bmc_note ? `<div class="note">BMC: ${esc(cp.bmc_note)}</div>` : "") + '<div class="note">Component power from the agent, not wall power.</div>' : ""}</div>`;
  const gpuPanel = (s.gpus || []).length ? `<div class="panel glass pad"><h3>GPUs (${s.gpus.length})</h3><div class="gpu-grid">${s.gpus.map((g, i) => `
    <div class="gpu-card"><div class="gpu-head"><span class="chip">GPU ${i}</span><b>${esc(g.name)}</b></div>
      ${metric("gpu", "Util", g.util_percent, fmtP(g.util_percent))}
      ${g.vram_total ? metric("vram", "VRAM", pct(g.vram_used, g.vram_total), `${fmtBytes(g.vram_used)} / ${fmtBytes(g.vram_total)}`) : ""}
      <div class="gpu-facts">${g.temp_c != null ? `<span>${g.temp_c.toFixed(0)}°C</span>` : ""}${g.power_w != null ? `<span>${fmtW(g.power_w)}</span>` : ""}<span class="note">${esc(g.vendor)}${g.source === "windows"
        ? ' · <span title="From the GPU counters Windows keeps (as in Task Manager): no temperature or watts">via Windows</span>'
        : g.source === "amd" ? ' · <span title="Load and VRAM from the GPU counters Windows keeps; temperature and watts from AMD\'s driver library (ADL)">via AMD</span>' : ""}</span></div>
    </div>`).join("")}</div></div>` : "";
  const temps = s.temps || [];  // all of them (a 16-core CPU has 16 "Core n"); two columns when there are many
  const paused = helperPausedLine(s);
  const sensors = temps.length || (s.fans || []).length || (s.voltages || []).length || paused ? `<div class="panel glass pad"><h3>Sensors</h3>${paused}
    <div class="sensor-grid ${temps.length > 8 ? "two" : ""}">${temps.map((t) => `<div class="list-row"><span>${esc(t.label || t.sensor)}</span><span>${t.current_c.toFixed(0)}°C</span></div>`).join("")}</div>
    ${(s.fans || []).map((f) => `<div class="list-row"><span>${esc(f.name)}</span><span>${esc(f.rpm)} rpm</span></div>`).join("")}
    ${(s.voltages || []).map((v) => `<div class="list-row"><span>${esc(v.name)}</span><span>${v.volts.toFixed(2)} V</span></div>`).join("")}</div>` : "";
  const gs = s.kind === "node" ? guestsOf(s) : [];
  const guests = s.kind === "node" ? `<div class="section-title">Guests on this node <span class="count">${gs.length}</span></div>
    ${gs.length ? guestTable([[null, gs]], false) : '<div class="empty">No VMs or containers on this node.</div>'}` : "";
  return `<div class="grid2"><div class="stack">${res}${gpuPanel}</div><div class="stack">${batteryBox(s)}${power}${sensors}${managedBox(s)}</div></div>${guests}`;
}

let procSort = "cpu";
async function loadProcesses(id) {
  const body = $("#tab-body");
  if (!body) return waitForTab(id, "processes", () => loadProcesses(id));
  try {
    const d = await api(`/api/systems/${enc(id)}/processes?limit=25&sort=${procSort}`);
    if (!$("#tab-body") || current().id !== id || current().tab !== "processes") return;
    if (d.disabled_by_host) { body.innerHTML = '<div class="empty">Process view disabled by host.<br><span class="note">The machine owner turned it off on their agent (processes: enabled: false in its config.yaml).</span></div>'; return; }
    morph(body, `<div class="toolbar">Sort by ${["cpu", "mem", "gpu"].map((k) => `<button class="btn ${k === procSort ? "on" : ""}" data-sort="${k}">${k.toUpperCase()}</button>`).join("")}
      <span class="push">${esc(d.total_processes ?? "?")} processes · CPU % is per core · refreshes every 2 s</span></div>
      <div class="panel glass scroll"><table class="table"><thead><tr><th>PID</th><th>Name</th><th>User</th><th class="num">CPU %</th><th class="num">RSS</th><th class="num">GPU mem</th><th>Command line (redacted)</th></tr></thead>
      <tbody>${d.processes.map((p) => `<tr class="row" data-key="p${esc(p.pid)}"><td>${esc(p.pid)}</td><td class="pname" title="${esc(p.name)}"><b>${esc(p.name)}</b></td><td class="pname" title="${esc(p.username || "")}">${esc(p.username || "–")}</td>
        <td class="num">${p.cpu_percent?.toFixed(1) ?? "–"}</td><td class="num">${fmtBytes(p.rss)}</td><td class="num">${p.gpu_mem ? fmtBytes(p.gpu_mem) : "–"}${p.gpu_mem_by_gpu ? `<div class="note">${p.gpu_mem_by_gpu.map((b, i) => `GPU${i} ${fmtBytes(b)}`).join(" · ")}</div>` : ""}</td>
        <td class="cmd" title="${esc(p.cmdline || "")}">${esc(p.cmdline || "–")}</td></tr>`).join("")}</tbody></table></div>`);
    body.querySelectorAll("[data-sort]").forEach((b) => (b.onclick = () => { procSort = b.dataset.sort; loadProcesses(id); }));
  } catch (e) { body.innerHTML = `<div class="empty">${esc(e.message)}</div>`; }
  schedule(() => loadProcesses(id), 2000);
}

const fanMsg = dict();  // the last fan write error per system, shown until the next write succeeds
// Opened by a link or a reload (e.g. coming back from a step-up sign-in), the tab exists only once the host's
// data has arrived: until then the loaders wait for it instead of leaving "Loading…" for good.
const waitForTab = (id, tab, fn) => { const r = current(); if (r.page === "system" && r.id === id && r.tab === tab) schedule(fn, 300); };
async function loadFans(id) {
  const body = $("#tab-body");
  if (!body) return waitForTab(id, "fans", () => loadFans(id));
  if (body.contains(document.activeElement) && document.activeElement.type === "range") return schedule(() => loadFans(id));
  const paused = helperPaused(state.byId[id], "fans"), canCtl = canControl() && !paused;
  try {
    const f = await api(`/api/systems/${enc(id)}/fan`);
    if (!$("#tab-body")) return;
    if (paused || f.available === false) { morph(body, fansPaused(state.byId[id], f)); return schedule(() => loadFans(id)); }
    const full = f.mode === "full", floor = Math.max(30, f.min_duty || 0);
    const unsupported = new Set(f.unsupported_modes || []);
    const limits = f.max_temp_c != null
      ? ` While a manual speed is set, the agent's watchdog sets both zones to <b>100%</b> if a CPU temperature reaches <b>${esc(f.max_temp_c.toFixed(0))}°C</b> or no temperature can be read for 30 s.`
      : "";
    const modeBtn = (m) => {
      const no = unsupported.has(m);
      const title = no ? "this board's BMC doesn't support this mode" : m === f.auto_mode ? "this board's automatic mode (fan_control.auto_mode)" : "";
      return `<button class="btn ${f.mode === m ? "on" : ""} ${no ? "unsupported" : ""}" data-mode="${m}" ${canCtl && !no ? "" : "disabled"}${title ? ` title="${esc(title)}"` : ""}${no ? ' aria-disabled="true"' : ""}>${m}${m === f.auto_mode ? ' <span class="note">auto</span>' : ""}</button>`;
    };
    // patched in place, not built anew: a mode button that opened "Confirm it's you" is still there to get the focus back
    morph(body, `<div class="warnbox">Writes go to the BMC. Manual speeds are never below <b>${esc(floor)}%</b> (the agent's minimum; the panel never sends less than 30%).${limits}${canCtl ? "" : ` ${esc(controlWhyNot())}`}</div>
      ${f.error ? `<div class="error">${esc(f.error)}</div>` : ""}
      <div class="grid2"><div class="panel glass pad"><h3>Mode</h3><div class="row-gap">${["optimal", "standard", "heavyio", "full"].map(modeBtn).join("")}</div>
        ${unsupported.size ? `<div class="note">Greyed out: this board's BMC doesn't support ${esc([...unsupported].join(", "))}.</div>` : ""}
        <h3 class="gap">Zone duty (full mode only)</h3>${[0, 1].map((z) => {
          const known = f.duty[z] != null, v = known ? f.duty[z] : 50;  // the BMC tells a zone's duty in full mode only: no number without one
          return `<div class="duty-row"><span>Zone ${z}</span><input type="range" min="${esc(floor)}" max="100" step="5" value="${esc(v)}" data-zone="${z}" ${canCtl && full ? "" : "disabled"}>
          <b data-duty-label="${z}"${known ? "" : ' title="Not known: the BMC reports a zone\'s duty in full mode only"'}>${known ? `${esc(v)}%` : "–"}</b><button class="btn" data-apply="${z}" ${canCtl && full ? "" : "disabled"}>Set</button></div>`; }).join("")}
        <div class="note">Zone 0: CPU/system headers (FAN1…), zone 1: peripheral (FANA). Verify on your board.</div><div class="error" id="fan-err" role="alert">${esc(fanMsg[id] || "")}</div></div>
      <div class="stack"><div class="panel glass pad"><h3>Fans</h3>${f.fans.map((x) => `<div class="list-row"><span>${esc(x.name)}</span><span>${esc(x.rpm)} rpm</span></div>`).join("") || '<div class="note">no fan sensors</div>'}</div>
        <div class="panel glass pad"><h3>Events</h3>${(f.events || []).slice().reverse().map((e) => `<div class="list-row"><span>${new Date(e.t * 1000).toLocaleTimeString()}</span><span>${esc(e.action)} · ${esc(e.detail)}</span></div>`).join("") || '<div class="note">none</div>'}</div></div></div>`);
    body.querySelectorAll("[data-zone]").forEach((r) => (r.value = r.getAttribute("value")));  // a slider that was moved shows what the BMC holds again
    const post = async (path, payload) => {
      const url = `/api/systems/${enc(id)}${path}`;
      try {
        const r = await controlCall({ kind: "request", label: fanLabel(path, payload, state.byId[id]?.name || id), system: id, method: "POST", path: url, body: payload },
          () => api(url, { method: "POST", body: JSON.stringify(payload) }));
        if (r) delete fanMsg[id];
      } catch (e) { fanMsg[id] = e.message; }
      loadFans(id);  // also shows a mode the BMC just refused as greyed out
    };
    body.querySelectorAll("[data-zone]").forEach((r) => (r.oninput = () => ($(`[data-duty-label="${r.dataset.zone}"]`, body).textContent = `${r.value}%`)));
    body.querySelectorAll("[data-mode]").forEach((b) => (b.onclick = () => post("/fan/mode", { mode: b.dataset.mode })));
    body.querySelectorAll("[data-apply]").forEach((b) => (b.onclick = () => post("/fan/duty", { zone: +b.dataset.apply, percent: +$(`[data-zone="${b.dataset.apply}"]`, body).value })));
  } catch (e) { body.innerHTML = `<div class="empty">${esc(e.message)}</div>`; }
  schedule(() => loadFans(id));
}

// What a fan write is called in "Confirm it's you", in "Resume" and in its result: plain words with the values
// it sets, never the request's JSON.
const fanLabel = (path, p, name) => `${path === "/fan/mode" ? `Set fan mode to ${p.mode}` : `Set fan zone ${p.zone} to ${p.percent}%`} on ${name}`;

// The Fans tab while the helper is not running: the explanation and the mode buttons, disabled.
function fansPaused(s, f) {
  const h = s?.helper || { state: f.reason === "helper_error" ? "error" : "stopped" };
  const why = h.state === "starting" ? "is starting" : h.state === "error" ? "is not answering" : "is stopped";
  return `<div class="calmbox"><span>Fan control paused: the elevated helper ${why}. The fans are on the BMC's own automatic control, and the agent's temperature watchdog is off until the helper runs again.</span>
    ${h.state === "starting" ? "" : `<button class="btn sm" type="button" data-act="helper" data-action="start" ${canControl() ? "" : `disabled title="${esc(controlWhyNot() || "only admins can start it")}"`}>Start helper</button>`}</div>
    <div class="panel glass pad narrow"><h3>Mode</h3><div class="row-gap">${["optimal", "standard", "heavyio", "full"].map((m) => `<button class="btn ${f.mode === m ? "on" : ""}" disabled>${m}</button>`).join("")}</div>
      <div class="note">Zone duty and modes come back when the helper runs.</div></div>`;
}

function pagePower() {
  setTitle("Power", "wall power from plugs, UPS and BMCs");
  const p = state.power;
  if (!p) return '<div class="empty">Waiting for data…</div>';
  const chip = (st) => `<span class="chip ${st === "ok" ? "ok" : st === "stale" ? "warn" : "bad"}">${esc(st)}</span>`;
  return `<div class="panel glass pad power-hero"><div><div class="tile"><div class="label">Total wall power</div></div><div class="big-w">${fmtW(p.total_w)}</div>
    <div class="note">${p.sources.filter((s) => s.include_in_total).length} sources counted · ${state.history.length} samples</div></div>${spark(state.history.map((h) => h.total_w))}</div>
    <div class="section-title">Sources${infoBtn("power", "Sources", infoList([
      "Wall power: what plugs, UPSes and BMCs measure.",
      "CPU, GPU and battery readings on a system's page are component power reported by its agent, not wall power."]))}</div><div class="panel glass scroll"><table class="table"><thead><tr><th>Source</th><th>Type</th><th>Status</th><th class="num">Watts</th>
    <th class="num">Volts</th><th class="num">Amps</th><th class="num">kWh</th><th>Linked</th><th>Last hour</th></tr></thead><tbody>${p.sources.map((s) => {
      const r = s.readings[0] || {};
      const linked = state.systems.find((x) => x.id === r.system_id);
      return `<tr class="row"><td><b>${esc(s.name)}</b>${s.via ? `<div class="note">via agent ${esc(s.via)}</div>` : ""}</td><td>${esc(s.type)}</td>
        <td>${chip(s.status)}${s.error ? `<div class="error">${esc(s.error)}</div>` : ""}</td><td class="num"><b class="value pwr">${fmtW(r.watts)}</b></td>
        <td class="num">${r.voltage?.toFixed(1) ?? "–"}</td><td class="num">${r.current?.toFixed(2) ?? "–"}</td><td class="num">${r.energy_kwh_total?.toFixed(2) ?? "–"}</td>
        <td>${linked ? `<a href="#/system/${enc(linked.id)}">${esc(linked.name)}</a>` : "–"}${s.include_in_total ? "" : ' <span class="chip">not in total</span>'}</td>
        <td>${spark(state.history.map((h) => own(h.sources, s.id) ?? null))}</td></tr>`; }).join("")}</tbody></table></div>`;
}

// ------------------------------------------------------------ UPS page
const UPS_STATUS = table({ online: ["ok", "online"], on_battery: ["warn", "on battery"], low_battery: ["bad", "LOW BATTERY"],
  off: ["", "off"], unknown: ["", "unknown"], unreachable: ["bad", "unreachable"] });
// Watts: estimated from load % × nominal W when the UPS has no power reading. Many small UPSes report 0 % load
// below their resolution, so "0 W" while online is shown as "0 W?" (unknown, not zero).
function upsWatts(u) {
  if (u.watts == null) return "–";
  if (u.watts_estimated && u.watts === 0 && u.status === "online") {
    return `<span title="${esc(`The UPS reports 0 % load. Small UPSes often report 0 below their resolution, so the real draw is unknown (estimate from ${u.nominal_w} W nominal).`)}">0 W?</span>`;
  }
  return `${u.watts_estimated ? "≈" : ""}${fmtW(u.watts)}${u.watts_estimated ? ` (est. from ${esc(u.nominal_w)} W nominal)` : ""}`;
}
function upsCard(u) {
  const [cls, label] = (Object.hasOwn(UPS_STATUS, u.status) && UPS_STATUS[u.status]) || ["", String(u.status ?? "unknown")];
  const hist = state.upsHist[u.id] || [];
  const ch = Number.isFinite(u.battery_charge) ? u.battery_charge : null;
  const powered = (u.powers || []).map((id) => state.byId[id]).filter(Boolean);
  return `<div class="panel glass pad ups-card ${onBattery(u) ? "alarm" : ""} ${u.stale ? "stale" : ""}" data-key="${esc(u.id)}">
    <div class="ups-head">${I.ups}<div><b>${esc(u.name)}</b><div class="note">${esc([u.mfr, u.model].filter(Boolean).join(" ") || u.ups_name)}${u.via ? ` · via agent ${esc(u.via)}` : " · polled by the panel"}</div></div>
      <span class="chip ${cls} push">${esc(label)}${u.stale ? " · stale" : ""}</span></div>
    ${u.error && u.status === "unreachable" ? `<div class="error">${esc(u.error)}</div>` : ""}
    <div class="ups-grid">
      <div><div class="k">Battery</div><div class="big">${ch != null ? `${ch.toFixed(0)}%` : "–"}</div>${barEl(ch != null && ch < 30 ? "hot" : "ok", ch)}</div>
      <div><div class="k">Runtime</div><div class="big">${fmtRuntime(u.battery_runtime_s)}</div><div class="note">mm:ss at current load</div></div>
      <div><div class="k">Load</div><div class="big">${u.load_percent != null ? `${u.load_percent.toFixed(0)}%` : "–"}</div>
        <div class="note">${upsWatts(u)}</div></div>
    </div>
    ${spark(hist.map((p) => p.watts), "ups")}<div class="note">load (W), last ${hist.length ? fmtUptime((Date.now() - new Date(hist[0].t)) / 1000) : "–"}</div>
    <div class="ups-facts">
      <div class="list-row"><span>Input / output</span><span>${u.input_voltage?.toFixed(0) ?? "–"} V / ${u.output_voltage?.toFixed(0) ?? "–"} V${u.input_frequency ? ` · ${u.input_frequency.toFixed(1)} Hz` : ""}</span></div>
      <div class="list-row"><span>Battery voltage</span><span>${u.battery_voltage?.toFixed(1) ?? "–"} V</span></div>
      <div class="list-row"><span>Battery date</span><span>${esc(u.battery_date || "–")}</span></div>
      ${u.test_result ? `<div class="list-row"><span>Self-test</span><span>${esc(u.test_result)}</span></div>` : ""}
      ${u.flags?.length ? `<div class="list-row"><span>Flags</span><span>${u.flags.map((f) => `<span class="chip ${f === "replace_battery" || f === "overload" ? "bad" : ""}">${esc(f.replace("_", " "))}</span>`).join(" ")}</span></div>` : ""}
      <div class="list-row"><span>Powers</span><span>${powered.length ? powered.map((p) => `<a href="#/system/${enc(p.id)}">${esc(p.name)}</a>`).join(", ") : '<span class="note">nothing linked</span>'}</span></div>
      <div class="list-row"><span>Raw status</span><span><code>${esc(u.status_raw || "–")}</code></span></div>
    </div></div>`;
}
function pageUps() {
  setTitle("UPS", "Network UPS Tools · read-only");
  const ups = state.ups?.ups || [];
  if (!ups.length) return '<div class="empty">No UPS configured. See README → UPS (NUT).</div>';
  const ev = state.upsEvents.slice(0, 30);
  return `<div class="ups-cards">${ups.map(upsCard).join("")}</div>
    <div class="section-title">Events <span class="count">${state.upsEvents.length}</span></div>
    <div class="panel glass scroll"><table class="table"><thead><tr><th>When</th><th>UPS</th><th>Event</th><th>Details</th></tr></thead><tbody>
    ${ev.length ? ev.map((e) => `<tr class="row" data-key="e${esc(e.seq)}"><td><span data-ago="${esc(e.t)}">${ago(e.t)}</span> <span class="note">${new Date(e.t).toLocaleTimeString()}</span></td>
      <td>${esc(e.ups_name)}</td><td><span class="chip ${e.severity === "critical" ? "bad" : e.severity === "warning" ? "warn" : "ok"}">${esc(e.kind.replace("_", " "))}</span></td>
      <td>${esc(e.message)}</td></tr>`).join("") : '<tr><td colspan="4" class="note">No transitions since the panel started.</td></tr>'}</tbody></table></div>
    <p class="note info-keep">The panel only reads the UPS: it sends it no commands and changes none of its settings. Shutdown on low battery stays with upsmon on each host.${infoBtn("ups", "UPS", infoList([
      "The panel asks NUT with LIST UPS and LIST VAR only.",
      "No instant commands, no SET VAR."]))}</p>`;
}

// ------------------------------------------------------------ audit page (control group only)
const audit = { entries: [], next: null, q: "", action: "", host: "", loading: false, error: null };
const AUDIT_ACTIONS = [["", "All actions"], ["auth", "Sign-in / step-up / denied"], ["power", "Power"], ["helper", "Elevated helper"], ["fan", "Fans"], ["poll", "Polling"],
  ["settings", "Settings"], ["token", "API tokens"], ["control", "Kill switch"], ["host", "Host admin"], ["sessions", "Sessions"],
  ["user", "Users / roles"], ["site", "Sites"], ["update", "Updates"], ["wakeupdate", "Update unpowered hosts"]];
async function loadAudit(reset) {
  if (audit.loading) return;
  audit.loading = true;
  const qs = new URLSearchParams({ limit: "50" });
  if (audit.q) qs.set("q", audit.q);
  if (audit.action) qs.set("action", audit.action);
  if (audit.host) qs.set("host", audit.host);
  if (!reset && audit.next != null) qs.set("before", audit.next);
  try {
    const d = await api(`/api/audit?${qs}`);
    audit.entries = reset ? d.entries : [...audit.entries, ...d.entries];
    audit.next = d.next; audit.error = null;
  } catch (e) { audit.error = e.message; }
  audit.loading = false;
  render();
}
const auditOutcome = (o) => {
  const t = String(o || "");
  const cls = /^ok|^required|redirected/.test(t) ? (t.startsWith("required") || t.startsWith("redirected") ? "warn" : "ok") : /^(denied|failed|error|4\d\d|5\d\d)/.test(t) ? "bad" : "";
  return `<span class="chip ${cls}" title="${esc(t)}">${esc(t.length > 30 ? t.slice(0, 28) + "…" : t)}</span>`;
};
function pageAudit() {
  setTitle("Audit", "who did what, newest first");
  if (!state.me?.can_control) return '<div class="empty">The audit log is visible to admins only.</div>';
  const hosts = state.systems.map((s) => s.id).sort();
  return `<form class="toolbar audit-filters" id="audit-form" data-keep>
      <input class="input" id="af-q" placeholder="User, token or source…" value="${esc(audit.q)}">
      <select class="select" id="af-action">${AUDIT_ACTIONS.map(([v, l]) => `<option value="${v}" ${audit.action === v ? "selected" : ""}>${l}</option>`).join("")}</select>
      <select class="select" id="af-host"><option value="">All hosts</option>${hosts.map((h) => `<option ${audit.host === h ? "selected" : ""}>${esc(h)}</option>`).join("")}</select>
      <button class="btn" type="submit">Filter</button><span class="push note">append-only · nothing can be deleted from the UI</span></form>
    ${audit.error ? `<div class="error">${esc(audit.error)}</div>` : ""}
    <div class="panel glass scroll"><table class="table audit"><thead><tr><th>When</th><th>Who</th><th>Action</th><th>Target</th><th>Result</th><th>Source</th><th>IP</th></tr></thead><tbody>
    ${audit.entries.map((e) => `<tr class="row" data-key="a${esc(e.id)}"><td class="nowrap"><span data-ago="${esc(e.ts)}">${ago(e.ts)}</span><div class="note">${new Date(e.ts).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" })}</div></td>
      <td>${esc(e.actor)}${e.sub && e.sub !== e.actor ? `<div class="note" title="Account id (OIDC subject): the one part of this entry the owner of the account cannot choose. The name or address above is what the account says it is; '(unverified)' means Pocket ID did not verify that address.">id ${esc(e.sub)}</div>` : ""}</td><td><code>${esc(e.action)}</code>${Object.keys(e.params || {}).length ? `<div class="note">${esc(Object.entries(e.params).map(([k, v]) => `${k}=${typeof v === "object" ? JSON.stringify(v) : v}`).join(" · ")).slice(0, 64)}</div>` : ""}</td>
      <td>${e.target ? (state.byId[e.target] ? `<a href="#/system/${enc(e.target)}">${esc(state.byId[e.target].name)}</a>` : esc(e.target)) : '<span class="note">–</span>'}</td>
      <td>${auditOutcome(e.outcome)}</td><td><span class="chip ${e.source === "cli" ? "warn" : e.source.startsWith("token:") ? "tok" : ""}" title="${esc(e.source)}">${esc(e.source.length > 16 ? e.source.slice(0, 15) + "…" : e.source)}</span></td>
      <td class="note">${esc(e.ip || "–")}</td></tr>`).join("") || `<tr><td colspan="7" class="note">${audit.loading ? "Loading…" : "No entries match."}</td></tr>`}</tbody></table></div>
    ${audit.next != null ? `<div class="row-gap center"><button class="btn" data-act="audit-more">${audit.loading ? "Loading…" : "Older entries"}</button></div>` : ""}`;
}
document.addEventListener("submit", (e) => {
  if (e.target.id !== "audit-form") return;
  e.preventDefault();
  audit.q = $("#af-q").value.trim(); audit.action = $("#af-action").value; audit.host = $("#af-host").value;
  loadAudit(true);
});
document.addEventListener("click", (e) => { if (e.target.closest('[data-act="audit-more"]')) loadAudit(false); });

// ------------------------------------------------------------ updates (admins only)
// Each agent's version and self-update state next to the latest Fairy-agent release. Update makes the agent
// download, verify and install the release itself (and roll back if the new version does not come up); nothing
// updates on its own. Refreshed every 2 s while the page is open.
const upd = { data: null, error: null, busy: dict(), bySys: dict() };
const updBusy = (r) => ["downloading", "applying"].includes(r?.self_update?.state);
async function fetchUpdates() {
  if (!state.me?.can_control) return;
  try { upd.data = await api("/api/admin/updates"); upd.error = null; }
  catch (e) { upd.error = e.message; if (e.status === 403) upd.data = null; }
  updIndex();
  render();
}
function updIndex() {  // after upd.data changed: the per-host lookup and the menu dot
  upd.bySys = dict();
  for (const r of upd.data?.agents || []) upd.bySys[r.system_id || r.id] = r;
  const b = $("#upd-badge");
  if (b) { const n = upd.data?.behind || 0; b.hidden = !n; b.title = `${n} update${n === 1 ? "" : "s"} available`; }
}
async function loadUpdates() {  // the Updates page: every 2 s while it is open
  await Promise.all([fetchPanelUpdate(), fetchUpdates(), fetchWakeUpdate()]);
  if (current().page === "updates") schedule(loadUpdates, 2000);
}
// In the background (admins only): every minute, every 2 s while an agent is updating; feeds the banner, the
// menu dot and the version arrows on the host list and host pages.
function updTick() {
  clearTimeout(upd.bgT);
  const fast = (upd.data?.agents || []).some(updBusy) || Date.now() < (upd.fastUntil || 0);
  const next = () => { if (!signedOut) upd.bgT = setTimeout(updTick, fast ? 2000 : 60000); };
  if (current().page === "updates") { next(); return; }
  fetchUpdates().then(next, next);
}
const LAST_SEEN_TIP = "What its agent reported when the panel last reached it. It does not answer now, so this may no longer be true.";
const buildLabel = (v) => { const m = /^(\d+)-([0-9a-f]{7,})$/.exec(v || ""); return m ? `build ${m[1]} (${m[2].slice(0, 7)})` : v || "–"; };
function updState(r) {
  const su = r.self_update;
  if (!su) return '<span class="note">–</span>';
  const st = su.state || "idle", cls = st === "failed" || st === "unavailable" ? "bad" : st === "idle" ? "" : "warn";
  const at = su.last && Number.isFinite(su.last.at) && su.last.at > 0 && su.last.at < 1e10 ? new Date(su.last.at * 1000).toISOString() : "";
  const last = su.last ? `<div class="note" title="${esc(su.last.message)}">${su.last.ok ? "last update OK" : "last update failed"}${at ? `, <span data-ago="${at}">${ago(at)}</span>` : ""}</div>` : "";
  return `<span class="chip ${cls}">${esc(st)}</span>${su.message ? `<div class="note">${esc(su.message)}</div>` : ""}${last}`;
}
function pageUpdates() {
  setTitle("Updates", "agent versions and self-update");
  if (!state.me?.can_control) return '<div class="empty">The Updates page is for admins only.</div>';
  const d = upd.data;
  if (!d) return upd.error ? `<div class="error">${esc(upd.error)}</div>` : '<div class="empty">Loading…</div>';
  const L = d.latest, why = controlWhyNot();
  const ready = d.agents.filter((r) => r.can_update);
  const latest = L.tag ? `<b>${esc(L.version ? buildLabel(L.version) : L.tag)}</b> ${L.verified ? '<span class="chip ok" title="signature checked with this panel\'s release key">signed</span>' : '<span class="chip warn" title="this panel build has no release key, so the signature was not checked here (agents always check it)">unverified</span>'}
      ${L.published_at ? `<span class="note">published <span data-ago="${esc(L.published_at)}">${ago(L.published_at)}</span></span>` : ""}
      ${L.url ? `<a href="${safeHref(L.url)}" target="_blank" rel="noopener noreferrer">${esc(L.tag)} ${I.ext}</a>` : ""}` : '<span class="note">unknown</span>';
  const row = (r) => {
    const cur = r.version ? buildLabel(r.version) : "–";
    // version_last_seen: the agent does not answer; this is what it reported when the panel last reached it
    const vcls = r.version_last_seen ? "" : r.behind === true ? "warn" : r.behind === false ? "ok" : "";
    const vnote = r.version_last_seen ? `<div class="note" title="${esc(LAST_SEEN_TIP)}">last seen${r.behind === true ? " · older than the latest release" : ""}</div>`
      : r.behind === true ? '<div class="note">update available</div>' : "";
    const busy = upd.busy[r.id];
    return `<tr class="row" data-key="up-${esc(r.id)}"><td class="namecell"><b title="${esc(r.name)}">${state.byId[r.id] ? `<a href="#/system/${enc(r.id)}">${esc(r.name)}</a>` : esc(r.name)}</b><span class="sub" title="${esc(r.id)}">${esc(r.id)}</span></td>
      <td><span class="chip ${vcls}" title="${esc(r.version || "")}">${esc(cur)}</span>${vnote}</td>
      <td><span class="chip ${r.status === "online" ? "ok" : r.status === "offline" ? "bad" : ""}">${esc(r.status)}</span></td>
      <td>${r.configured ? updState(r) : '<span class="note">off</span>'}</td>
      <td class="num"><button class="btn" data-act="upd-one" data-id="${esc(r.id)}" data-name="${esc(r.name)}" ${r.can_update && !why && !busy ? "" : "disabled"}
        title="${esc(why || r.why || `Install the latest release on ${r.name}`)}">${I.update} ${busy ? "Sending…" : "Update"}</button></td></tr>`;
  };
  return `<div class="stack">
    <div class="panel glass pad"><div class="row-gap"><span>Latest Fairy-agent release: ${latest}</span>
      ${updCheckBtn(d)}
      <button class="btn primary" data-act="upd-all" ${ready.length && !why && !upd.busy.all ? "" : "disabled"} title="${esc(why || (ready.length ? "Update every agent that can update itself" : "No agent can update right now"))}">${I.update} Update all (${ready.length})</button></div>
      ${updCheckLine(d)}
      ${L.error ? `<div class="note warn">${esc(L.error)}</div>` : ""}
      ${d.panel_latest?.error && !d.panel_latest.disabled ? `<div class="note warn">Panel release: ${esc(d.panel_latest.error)}</div>` : ""}</div>
    <div class="panel glass pad">${panelUpdateBox(d)}</div>
    ${wakeUpdateBox()}
    ${why ? `<div class="note warn">${esc(why)}</div>` : ""}
    <div class="panel glass scroll"><table class="table updates"><thead><tr><th>Host</th><th>Version</th><th>Status</th><th>Self-update</th><th class="num"></th></tr></thead>
      <tbody>${d.agents.map(row).join("") || '<tr><td colspan="5" class="note">No agents in config.yaml.</td></tr>'}</tbody></table></div>
    <div class="note">Nothing updates by itself: <b>Update</b> installs the latest signed release on that agent.${infoBtn("upd-agents", "Update", infoList([
      "The agent downloads the latest release from GitHub and checks its signature.",
      "It installs the release and restarts: a few seconds offline.",
      "If the new version does not come up, the agent rolls back by itself.",
      "Only agents with <code>self_update: true</code> here and <code>self_update: enabled: true</code> in their own config.yaml can do this.",
      "Every Update asks you to confirm it's you and is written to the Audit log."]))}</div></div>`;
}
// ---- Check for updates: GitHub now instead of the next background check (every 10 min). Read-only, so no
// step-up; at most once every 30 s for the whole panel (the server says how long to wait).
function updCheckBtn(d) {
  const busy = upd.checking || d.check_running, wait = d.check_retry_s || 0;
  const title = busy ? "Checking GitHub…" : wait ? `Checked just now: you can check again in ${wait} s` : "Look for new Fairy-agent and panel releases on GitHub now";
  return `<button class="btn push" type="button" data-act="upd-check" ${busy || wait ? "disabled" : ""} aria-busy="${busy}" title="${esc(title)}">${busy
    ? '<span class="spinner" aria-hidden="true"></span> Checking…' : `${I.recheck} Check for updates`}</button>`;
}
function updCheckLine(d) {
  const at = [d.latest?.checked_at, d.panel_latest?.checked_at].filter(Boolean).sort((a, b) => new Date(a) - new Date(b)).pop();
  const c = upd.check;
  let res = "";
  if (upd.checking) res = " · checking GitHub…";
  else if (c && !c.ok) res = ` · <span class="warn">Check failed: ${esc(c.message)}</span>`;
  else if (c) res = ` · <span class="chip ${/^Up to date/.test(c.message) ? "ok" : "warn"}">${esc(c.message)}</span>`;
  return `<div class="note upd-check" role="status" aria-live="polite">Last checked on GitHub: ${!at ? "not yet" : secsSince(at) < 10 ? "just now" : `<span data-ago="${esc(at)}">${ago(at)}</span>`}${res}</div>`;
}
document.addEventListener("click", async (e) => {
  const t = e.target.closest('[data-act="upd-check"]');
  if (!t || t.disabled || upd.checking) return;
  upd.checking = true; upd.check = null; render();
  try {
    const r = await api("/api/admin/update-check", { method: "POST", body: "{}" });
    upd.data = r.page; upd.error = null; updIndex();
    upd.check = { ok: r.ok, message: r.message };
  } catch (x) {
    upd.check = { ok: false, message: x.message };
    if (x.status === 429 && upd.data) upd.data.check_retry_s = x.detail?.retry_after_s || 30;
  }
  upd.checking = false; render();
});
async function updateCall(label, path, key) {
  upd.busy[key] = true; render();
  try {
    const r = await controlCall({ kind: "request", label, method: "POST", path, body: {} }, () => api(path, { method: "POST", body: "{}" }));
    if (r?.results) {
      const bad = r.results.filter((x) => !x.ok);
      flash(bad.length ? `${r.results.length - bad.length} started, ${bad.length} not: ${bad.map((x) => `${x.id} (${x.error})`).join(", ")}` : `Update started on ${r.results.length} agent(s)`, bad.length > 0);
    } else if (r) flash(`${label}: started`);
  } catch (e) { flash(e.message, true); }
  delete upd.busy[key];
  upd.fastUntil = Date.now() + 30000;  // the agents' new state arrives with the next polls
  await loadUpdates();
  updTick();
}
document.addEventListener("click", (e) => {
  const t = e.target.closest('[data-act="upd-one"],[data-act="upd-all"]');
  if (!t || t.disabled) return;
  const L = upd.data?.latest, to = L?.version ? buildLabel(L.version) : L?.tag || "the latest release";
  if (t.dataset.act === "upd-one") {
    if (confirm(`Update ${t.dataset.name} to ${to}? The agent restarts (a few seconds offline) and rolls back by itself if the new version does not come up.`)) {
      updateCall(`Update ${t.dataset.name}`, `/api/admin/updates/${enc(t.dataset.id)}`, t.dataset.id);
    }
  } else if (confirm(`Update every agent that can update itself to ${to}? Each one restarts and rolls back by itself if needed.`)) {
    updateCall("Update all agents", "/api/admin/updates", "all");
  }
});

// ---- version + "update available" arrow (admins only), like Tailscale's machine list
function verChip(sysId, arrowOnly) {  // arrowOnly (the host list): nothing unless there is an update, then only the arrow
  const r = upd.bySys[sysId];
  if (!r || !state.me?.can_control) return "";
  const stale = !!r.version_last_seen;  // the agent does not answer: no "update available" arrow for what it once reported
  const arrow = (r.behind === true && !stale) || updBusy(r);
  if (arrowOnly && !arrow) return "";
  const L = upd.data?.latest, rel = L?.version ? `: ${buildLabel(L.version)}` : L?.tag ? `: ${L.tag}` : "";  // "Update available: build 42 (1a2b3c4)"
  return `<span class="ver">${arrowOnly ? "" : `<span class="chip ${r.behind === true && !stale ? "warn" : ""}" title="${esc(stale ? `fairy-agent ${r.version}, last seen. ${LAST_SEEN_TIP}` : `fairy-agent ${r.version || "?"}`)}">${esc(r.version ? `${buildLabel(r.version)}${stale ? " (last seen)" : ""}` : "agent ?")}</span>`}${arrow
    ? `<button class="upd-arrow ${updBusy(r) ? "busy" : ""}" type="button" data-act="upd-pop" data-sys="${esc(sysId)}" aria-haspopup="dialog" aria-expanded="${upd.popSys === sysId}"
      aria-label="${esc(updBusy(r) ? `Update in progress on ${r.name}` : `Update available for ${r.name}${rel}`)}" title="${esc(updBusy(r) ? "Update in progress" : `Update available${rel}`)}">${I.uparrow}</button>` : ""}</span>`;
}
function updPopHtml(r) {
  const d = upd.data, L = d.latest, why = controlWhyNot();
  const rel = L.version ? buildLabel(L.version) : L.tag || "a newer release";
  let body;
  if (updBusy(r)) body = `<p>${esc(r.self_update.message || r.self_update.state)}</p>`;
  else if (r.can_update && !why) body = `<button class="btn primary" type="button" data-act="upd-start" data-id="${esc(r.id)}" data-name="${esc(r.name)}">${I.update} Start update</button>`;
  else if (why) body = `<p class="note warn">${esc(why)}</p>`;
  else if (!r.configured) body = `<p class="note">This host cannot update itself yet: add <code>self_update: true</code> to its <code>agents:</code> entry in the panel's config.yaml and <code>self_update: enabled: true</code> in the agent's config.yaml. Or run <code>fairy-agent update</code> on the machine (root / Administrator).</p>`;
  else if (!r.agent_enabled) body = `<p class="note">Self-update is off on the agent: set <code>self_update: enabled: true</code> in its config.yaml (or the agent is older than self-update: install this release by hand once, or run <code>fairy-agent update</code> on the machine).</p>`;
  else body = `<p class="note">${esc(r.why || "cannot update right now")}. On the machine: <code>fairy-agent update</code>.</p>`;
  return `<div class="menu-title" id="upd-pop-t">${updBusy(r) ? "Updating" : "Update available"}</div>
    <div class="upd-pop-body"><p>Release <b>${esc(rel)}</b> is available.${r.version ? ` ${esc(r.name)} ${r.version_last_seen ? "last reported" : "runs"} ${esc(buildLabel(r.version))}.` : ""}</p>
      ${L.url ? `<p><a href="${safeHref(L.url)}" target="_blank" rel="noopener noreferrer">Release notes ${I.ext}</a></p>` : ""}${body}</div>`;
}
function closeUpdPop(refocus) {
  const el = $("#upd-pop");
  if (!el) return;
  el.remove();
  const btn = upd.popBtn;
  upd.popBtn = null; upd.popSys = null;
  if (btn) { btn.setAttribute("aria-expanded", "false"); if (refocus && document.contains(btn)) btn.focus(); }
}
function openUpdPop(btn) {
  const r = upd.bySys[btn.dataset.sys];
  if (!r || !upd.data) return;
  const was = upd.popBtn === btn;
  closeUpdPop(false);
  if (was) return;
  const el = document.createElement("div");
  el.id = "upd-pop"; el.className = "menu upd-pop"; el.setAttribute("role", "dialog"); el.setAttribute("aria-labelledby", "upd-pop-t");
  el.innerHTML = updPopHtml(r);
  document.body.appendChild(el);
  const b = btn.getBoundingClientRect(), w = el.offsetWidth, h = el.offsetHeight;
  el.style.left = `${Math.max(16, Math.min(b.left, innerWidth - w - 16))}px`;
  el.style.top = `${b.bottom + 6 + h > innerHeight && b.top - h - 6 > 0 ? b.top - h - 6 : b.bottom + 6}px`;
  upd.popBtn = btn; upd.popSys = btn.dataset.sys; btn.setAttribute("aria-expanded", "true");
  (el.querySelector('[data-act="upd-start"]') || el.querySelector("button, a") || el).focus();
}
document.addEventListener("click", (e) => {
  const a = e.target.closest('[data-act="upd-pop"]');
  if (a) { e.stopPropagation(); openUpdPop(a); return; }
  const st = e.target.closest('[data-act="upd-start"]');
  if (st) {
    closeUpdPop(false);
    if (confirm(`Update ${st.dataset.name}? The agent restarts (a few seconds offline) and rolls back by itself if the new version does not come up.`)) {
      updateCall(`Update ${st.dataset.name}`, `/api/admin/updates/${enc(st.dataset.id)}`, st.dataset.id);
    }
    return;
  }
  if (e.target.closest('[data-act="upd-dismiss"]')) { try { localStorage.setItem(UPD_DISMISS, updKey()); } catch { /* ignore */ } renderAlerts(); return; }
  if (!e.target.closest("#upd-pop")) closeUpdPop(false);
}, true);
document.addEventListener("keydown", (e) => { if (e.key === "Escape" && $("#upd-pop")) { e.stopPropagation(); closeUpdPop(true); } }, true);
window.addEventListener("scroll", () => closeUpdPop(false), true);
window.addEventListener("resize", () => closeUpdPop(false));
// ---- the "update available" banner: per release pair, dismissed until the next one (this browser)
const UPD_DISMISS = "fairy-update-dismissed";
const updKey = () => `${upd.data?.latest?.tag || ""}|${upd.data?.panel_latest?.tag || ""}`;
function updBanner() {
  const d = upd.data;
  if (!d || !state.me?.can_control || current().page === "updates") return "";
  const agents = d.agents.filter((r) => r.behind === true).length;
  if (!agents && !d.panel_behind) return "";
  let dismissed = null; try { dismissed = localStorage.getItem(UPD_DISMISS); } catch { /* ignore */ }
  if (dismissed === updKey()) return "";
  const what = [agents ? `${agents} agent${agents === 1 ? "" : "s"}` : "", d.panel_behind ? "panel" : ""].filter(Boolean).join(", ");
  return `<div class="alert-strip info static slim" data-key="upd-banner">${I.uparrow}<b>Update available</b><span>${esc(what)}</span>
    <span class="note">${d.latest?.version ? `Fairy-agent ${esc(buildLabel(d.latest.version))}` : ""}${d.panel_behind ? " · panel: Update panel on the Updates page" : ""}</span>
    <a class="push" href="#/updates">Open Updates →</a><button class="strip-x" type="button" data-act="upd-dismiss" aria-label="Dismiss" title="Dismiss until the next release">${I.close}</button></div>`;
}

// ------------------------------------------------------------ Update unpowered hosts (admins, Updates page)
// For machines that are normally switched off: Fairy wakes the ticked ones, asks their agent to update itself and
// switches off again the ones this run woke. The run works in the panel (internal/wakerun); this is its
// dialog, its live progress and its report. Everything shown here that comes from the panel (machine names,
// the sentences of a run, an agent's own error text inside them) goes through esc().
const wu = { data: null, error: null, open: dict() };
async function fetchWakeUpdate() {
  if (!state.me?.can_control) return;
  try { wu.data = await api("/api/admin/wake-update"); wu.error = null; }
  catch (e) { wu.error = e.message; if (e.status === 403) wu.data = null; }
  render();
}
const WU_STATE = table({ queued: ["", "waiting"], waking: ["warn", "waking"], online: ["warn", "online"], awaiting_approval: ["warn", "needs your approval"],
  updating: ["warn", "updating"], settling: ["warn", "pause before shutdown"], shutting_down: ["warn", "switching off"], off: ["ok", "done · off again"],
  stayed_on: ["ok", "done · stays on"], left_on: ["bad", "LEFT ON"], gave_up: ["bad", "gave up"], skipped: ["", "skipped"], interrupted: ["bad", "interrupted"] });
const WU_RUN = table({ running: ["warn", "running"], done: ["ok", "finished"], cancelled: ["", "cancelled"], stopped: ["bad", "stopped"], interrupted: ["bad", "interrupted"] });
const WU_POWER = table({ on: ["warn", "is ON"], off: ["", "is off"], unknown: ["warn", "on or off: not known"], untouched: ["", "not touched"] });
const wuEnded = (h) => ["off", "stayed_on", "left_on", "gave_up", "skipped", "interrupted"].includes(h.state);
const wuUpdateBad = (h) => !!h.update && !["ok", "up_to_date"].includes(h.update);
// The steps of one machine as a row of boxes: done, now, failed.
function wuSteps(h) {
  const upd = h.update ? (wuUpdateBad(h) ? "bad" : "done") : h.state === "updating" ? "now" : "";
  if (h.mode !== "wake" || (h.already_on && !h.not_fresh)) {  // not_fresh: the run sent its wake, so it keeps the wake's row of steps
    const on = h.state !== "queued" && h.state !== "skipped";
    return `<div class="steps">${[["Is on", on ? "done" : ""], ["Update", upd], ["Stays on", h.state === "stayed_on" ? "done" : ""]]
      .map(([l, c]) => `<span class="${c}">${l}</span>`).join("")}</div>`;
  }
  const woke = !!h.wake_sent_at, online = !!h.online_at, shut = !!h.shutdown_at;
  const steps = [["Wake", woke ? "done" : ""], ["Online", online ? "done" : h.state === "waking" ? "now" : h.state === "gave_up" && woke ? "bad" : ""], ["Update", upd],
    ["Shut down", h.state === "off" ? "done" : h.state === "shutting_down" || h.state === "settling" ? "now" : shut || h.state === "left_on" ? "bad" : ""],
    ["Off", h.state === "off" ? "done" : ""]];
  return `<div class="steps">${steps.map(([l, c]) => `<span class="${c}">${l}</span>`).join("")}</div>`;
}
function wuHostRow(r, h) {
  const [cls, said] = WU_STATE[h.state] || ["", h.state], end = wuEnded(h), key = r.id + "/" + h.id;
  // "LEFT ON" is for a machine the run saw as on. When its last look could not see the machine, the chip says that.
  const label = h.state === "left_on" && h.on === "unknown" ? "not confirmed" : said;
  const warnEnd = end && cls === "ok" && wuUpdateBad(h);  // off again or left on as planned, but the update did not work
  const pw = end && WU_POWER[h.on] ? `<span class="chip ${WU_POWER[h.on][0]}">${WU_POWER[h.on][1]}</span>` : "";
  const count = h.state === "shutting_down" && h.shutdown_at ? ` <span class="note">in <b data-countdown="${esc(h.shutdown_at)}">${fmtRuntime(Math.max(0, (new Date(h.shutdown_at) - Date.now()) / 1000))}</b> · cancel it on <a href="#/system/${enc(h.id)}">its page</a></span>`
    : h.state === "waking" && h.wake_sent_at ? ` <span class="note">· sent <span data-ago="${esc(h.wake_sent_at)}">${ago(h.wake_sent_at)}</span></span>` : "";
  const log = h.log || [], open = !!wu.open[key];
  return `<div class="wu-host ${end ? "end" : ""}" data-key="wuh-${esc(key)}">
    <div class="wu-h1"><b>${state.byId[h.id] ? `<a href="#/system/${enc(h.id)}">${esc(h.name)}</a>` : esc(h.name)}</b>
      <span class="chip ${warnEnd ? "warn" : cls}">${esc(warnEnd ? `${label} · not updated` : label)}</span>${pw}</div>
    ${wuSteps(h)}
    <div class="wu-text">${esc(h.text)}${count}</div>
    ${log.length ? `<button class="wu-more" type="button" data-act="wu-log" data-k="${esc(key)}" aria-expanded="${open}">${open ? "Hide" : "Show"} its ${log.length} step${log.length === 1 ? "" : "s"}</button>` : ""}
    ${open ? `<ol class="wu-log">${log.map((l) => `<li><span class="note">${esc(new Date(l.t).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" }))}</span> ${esc(l.text)}</li>`).join("")}</ol>` : ""}</div>`;
}
function wuRunBox(r, live) {
  const [cls, label] = WU_RUN[r.state] || ["", r.state], why = controlWhyNot();
  const c = r.cancel;
  return `<div class="wu-run" data-key="wur-${esc(r.id)}">
    <div class="wu-head"><span class="chip ${cls}">${esc(label)}</span>
      <span>started by <b>${esc(r.by)}</b> <span data-ago="${esc(r.started_at)}">${ago(r.started_at)}</span>${r.ended_at ? `, ended <span data-ago="${esc(r.ended_at)}">${ago(r.ended_at)}</span>` : ""}
        · ${esc(r.hosts.length)} machine${r.hosts.length === 1 ? "" : "s"} · list <code>${esc(r.list_hash)}</code></span>
      ${live && !c ? `<button class="btn danger push" type="button" data-act="wu-cancel" data-id="${esc(r.id)}" ${state.me?.can_control ? "" : "disabled"} title="${esc(why || "Stop the run")}">Cancel run</button>` : ""}</div>
    ${c ? `<div class="note warn wu-note">Cancelled by ${esc(c.by)}: ${!r.hosts.some((h) => h.wake_sent_at) ? "nothing else is started." : c.switch_off ? "the machines it has woken are still switched off again; nothing else is started." : "the machines it has woken stay on."}</div>` : ""}
    ${r.note ? `<div class="note warn wu-note">${esc(r.note)}</div>` : ""}
    ${r.approval === "needed" ? '<div class="warnbox wu-note">The woken machines wait for your approval of their update and shutdown.</div>' : ""}
    ${r.hosts.map((h) => wuHostRow(r, h)).join("")}</div>`;
}
function wakeUpdateBox() {
  const d = wu.data;
  if (!d) return wu.error ? `<div class="panel glass pad wu"><h3>Update unpowered hosts</h3><div class="error">${esc(wu.error)}</div></div>` : "";
  const why = controlWhyNot(), joined = d.hosts.filter((h) => h.opted_in), can = joined.filter((h) => h.choice);
  const able = d.hosts.filter((h) => h.can_join || h.opted_in);
  const row = (h) => `<tr class="row" data-key="wum-${esc(h.id)}"><td class="namecell"><b title="${esc(h.name)}">${state.byId[h.id] ? `<a href="#/system/${enc(h.id)}">${esc(h.name)}</a>` : esc(h.name)}</b></td>
    <td><span class="chip ${h.power === "on" ? "ok" : h.power === "off" ? "" : "warn"}"${h.asleep ? ' title="Sent to sleep from the panel. Asleep and off look the same from here."' : ""}>${esc(h.asleep ? "asleep or off" : h.power)}</span></td>
    <td>${h.can_wake ? `via ${esc(h.relay)}` : `<span class="note" title="Fairy cannot wake this machine: in a run it is only updated while it is on">${esc(h.no_wake || h.why_not)}</span>`}</td>
    <td>${h.opted_in ? '<span class="chip ok">switched on</span>' : '<span class="note">off</span>'}${h.opted_in && !h.choice ? `<div class="note">${esc(h.why)}</div>` : ""}</td>
    <td class="num"><button class="btn sm" type="button" data-act="wu-switch" data-id="${esc(h.id)}" data-name="${esc(h.name)}" data-on="${h.opted_in ? "" : "1"}"
      ${why || (!h.opted_in && !h.can_join) ? "disabled" : ""} title="${esc(why || (h.opted_in ? "Take this machine out of these runs" : h.can_join ? "Let runs you start wake, update and switch off this machine" : h.why_not))}">${h.opted_in ? "Switch off" : "Switch on"}</button></td></tr>`;
  const earlier = d.active ? d.last : d.last.slice(1), showEarlier = !!wu.open.earlier;
  return `<div class="panel glass pad wu" data-key="wu">
    <div class="wu-top"><h3>Update unpowered hosts${infoBtn("wu", "Update unpowered hosts", infoList([
        "For machines that are normally switched off. You choose them and confirm once.",
        `Fairy wakes each one (Wake-on-LAN) and waits up to ${esc(fmtSpan(d.settings.online_timeout_s))} for it to come online.`,
        "It updates the machine's agent, like the Update button does.",
        `About ${esc(fmtSpan(d.settings.settle_s))} after the result it asks for a shutdown with a countdown of ${esc(fmtSpan(d.settings.shutdown_delay_s))}. You can cancel the countdown.`,
        `A machine counts as off only after ${esc(fmtSpan(d.off_after_s))} without an answer. One that stopped answering more recently may be restarting, and is left alone.`,
        `A limit: a machine switched on by a person while the run waits for it (up to ${esc(fmtSpan(d.settings.online_timeout_s))}), or just before the wake, is treated as woken by the run. It is switched off after its update. The report says "came online after the wake".`,
        "You can also switch a machine on for this in the ⋯ menu on its page.",
        "Switching on, and every run, asks you to confirm it's you and is written to the Audit log.",
        "API tokens cannot start a run.",
        "After a restart of the panel a run is not continued: this page then says which machines it had woken."]))}</h3>
      <button class="btn primary" type="button" data-act="wu-open" ${!why && !d.active && !d.problem ? "" : "disabled"}
        title="${esc(why || (d.active ? "A run is going: one run at a time" : d.problem || "Choose the machines and start a run"))}">${I.onoff}Update unpowered hosts</button></div>
    <p class="note info-keep">Wakes the machines you choose, updates their agent and switches them off again.
      <b>Fairy only switches off machines this run woke.</b> A machine that is already on is never switched off.</p>
    ${d.problem ? `<div class="warnbox">${esc(d.problem)}</div>` : ""}
    ${d.active ? wuRunBox(d.active, true) : d.last[0] ? `<div class="wu-sub">Last run</div>${wuRunBox(d.last[0], false)}` : ""}
    ${earlier.length ? `<button class="wu-more" type="button" data-act="wu-log" data-k="earlier" aria-expanded="${showEarlier}">${showEarlier ? "Hide" : "Show"} ${earlier.length} earlier run${earlier.length === 1 ? "" : "s"}</button>
      ${showEarlier ? earlier.map((r) => wuRunBox(r, false)).join("") : ""}` : ""}
    <div class="wu-sub">Machines</div>
    <p class="note wu-rule info-keep">A machine takes part only after you switch it on here, one machine at a time.
      ${joined.length ? `${esc(joined.length)} ${joined.length === 1 ? "is" : "are"} switched on${can.length !== joined.length ? `, ${esc(can.length)} can be chosen right now` : ""}.` : "None is switched on yet."}</p>
    <div class="scroll"><table class="table wu-machines"><thead><tr><th>Machine</th><th>Now</th><th>Woken by</th><th>In these runs</th><th class="num"></th></tr></thead>
      <tbody>${able.map(row).join("") || '<tr><td colspan="5" class="note">No machine with an agent that can update itself from the panel.</td></tr>'}</tbody></table></div></div>`;
}
// The per-host switch in the ⋯ menu of a machine's page (admins).
function wakeUpdateMenu(s) {
  const h = (wu.data?.hosts || []).find((x) => x.id === s.id);
  if (!h || !state.me?.can_control) return "";
  const why = controlWhyNot();
  // the full sentence, word for word what the menu used to show under the item: now behind the info button and in the item's tooltip
  const about = !h.can_join ? `Not possible: ${esc(h.why_not)}.` : h.can_wake ? `In a run you start on the Updates page, Fairy may wake it (via ${esc(h.relay)}), update its agent and switch it off again.`
    : `In a run it is only updated while it is on: ${esc(h.no_wake)}.`;
  return `<div class="menu-sep"></div><div class="menu-title">Update unpowered hosts${infoBtn("wu-menu", "Update unpowered hosts", infoList([about, ...(why ? [esc(why)] : [])]))}</div>
    <button class="menu-item ${h.opted_in ? "on" : ""}" type="button" data-act="wu-switch" data-id="${esc(h.id)}" data-name="${esc(h.name)}" data-on="${h.opted_in ? "" : "1"}"
      ${why || (!h.opted_in && !h.can_join) ? "disabled" : ""} title="${about}">${h.opted_in ? "Switched on for this machine" : "Switch on for this machine"}<span class="note">${h.opted_in ? "click: off" : h.can_join ? "off" : "Not available"}</span></button>`;
}
// The dialog: which machines, in plain words what happens, then "Confirm it's you" for exactly that list.
async function openWakeUpdate() {
  let d;
  try { d = await api("/api/admin/wake-update"); } catch (e) { flash(e.message, true); return; }
  wu.data = d;
  if (d.active) { flash("A run is going: one run at a time", true); render(); return; }
  const joined = d.hosts.filter((h) => h.opted_in);
  // A machine the panel itself sent to sleep is "asleep or off": listed apart and not ticked (it does not look freshly started when it wakes)
  const off = joined.filter((h) => h.choice === "wake" && !h.asleep), asleep = joined.filter((h) => h.choice === "wake" && h.asleep);
  const on = joined.filter((h) => h.choice === "update_only");
  const unsure = joined.filter((h) => !h.choice && h.not_sure), rest = joined.filter((h) => !h.choice && !h.not_sure);
  const cannot = (l) => l.map((h) => `<div class="list-row"><span>${esc(h.name)}</span><span class="note">${esc(h.why)}</span></div>`).join("");
  const tick = (h, kind, checked) => `<label class="check opt"><input type="checkbox" data-wu="${kind}" value="${esc(h.id)}" ${checked ? "checked" : ""}><span>${esc(h.name)}
    <span class="note">${kind === "wake" ? `woken by ${esc(h.relay)}` : "is on"}${h.version ? ` · runs ${esc(buildLabel(h.version))}` : ""}${h.behind === false ? " · already on the newest release" : ""}</span></span></label>`;
  const m = $("#modal");
  m.innerHTML = `<form class="modal-box wide" role="dialog" aria-modal="true" aria-labelledby="wu-t" id="wu-form">
    <h3 id="wu-t">Update unpowered hosts</h3>
    <p>What Fairy does with each ticked machine that is off, by itself, one step after the other:</p>
    <ol class="dlg-list" id="wu-what">
      <li>It sends Wake-on-LAN through the machine named next to it.</li>
      <li>It waits for the machine to come online: up to ${esc(fmtSpan(d.settings.online_timeout_s))}, and it goes on as soon as the machine's agent answers.
        A machine that does not come online in that time is given up on and reported. It may then still be starting, or be on without its network (Tailscale).</li>
      <li>The agent updates itself to the newest signed release${d.latest ? ` (${esc(buildLabel(d.latest))})` : ""}, as with the Update button. If the update fails, that is reported.</li>
      <li>It waits for the update's result, then about ${esc(fmtSpan(d.settings.settle_s))} more, and asks for a shutdown with a countdown of ${esc(fmtSpan(d.settings.shutdown_delay_s))}.
        You can cancel the countdown on the machine's page.</li>
    </ol>
    <div class="warnbox">Fairy only switches off machines <b>this run woke</b>. A machine that is already on is never switched off; a Proxmox node with running guests is left on.
      A machine counts as off only after ${esc(fmtSpan(d.off_after_s))} without an answer. If Fairy is not sure that a machine is off, it leaves it alone and the report says so.
      Nobody is asked: a machine switched on by a person while the run waits for it is treated as woken by the run, and switched off after its update.</div>
    ${joined.length ? "" : '<div class="warnbox">No machine is switched on for this yet. Close this, and switch machines on in the table on the Updates page (or in the ⋯ menu on a machine\'s page).</div>'}
    ${off.length ? `<div class="wu-sub">Off (no answer for ${esc(fmtSpan(d.off_after_s))} or more): wake, update, switch off again</div><div class="opts wu-list">${off.map((h) => tick(h, "wake", true)).join("")}</div>` : ""}
    ${asleep.length ? `<div class="wu-sub">Asleep or off (sent to sleep from the panel): woken; left on if it was asleep</div><div class="opts wu-list">${asleep.map((h) => tick(h, "wake", false)).join("")}</div>` : ""}
    ${off.length || asleep.length ? `<label class="check opt wu-ifon"><input type="checkbox" id="wu-ifon"><span>If one of these turns out to be on after all: update it too.
        <span class="note">It stays on. Without this tick nothing is sent to it.</span></span></label>` : ""}
    ${on.length ? `<div class="wu-sub">On now: update only, never switched off</div><div class="opts wu-list">${on.map((h) => tick(h, "update_only", false)).join("")}</div>` : ""}
    ${unsure.length ? `<div class="wu-sub">Not sure that they are off: left alone</div>${cannot(unsure)}` : ""}
    ${rest.length ? `<div class="wu-sub">Cannot take part right now</div>${cannot(rest)}` : ""}
    <div class="error" id="wu-err" role="alert"></div>
    <div class="row-gap end"><button class="btn" type="button" id="wu-x">Cancel</button><button class="btn primary" type="submit" id="wu-go">Start</button></div></form>`;
  m.hidden = false;
  const picked = (kind) => [...m.querySelectorAll(`input[data-wu="${kind}"]`)].filter((x) => x.checked).map((x) => x.value);
  const most = +d.max_hosts || 16;
  const sync = () => {
    const n = picked("wake").length + picked("update_only").length;
    $("#wu-go").disabled = !n || n > most;
    $("#wu-go").textContent = n ? `Start for ${n} machine${n === 1 ? "" : "s"}` : "Start";
    $("#wu-err").textContent = n > most ? `At most ${most} machines in one run.` : "";
  };
  m.querySelectorAll("input[data-wu]").forEach((x) => (x.onchange = sync));
  sync();
  $("#wu-x").onclick = closeModal;
  m.onclick = (e) => { if (e.target === m) closeModal(); };
  $("#wu-form").onsubmit = async (e) => {
    e.preventDefault();
    const body = { hosts: picked("wake"), update_only: picked("update_only"), update_if_on: !!$("#wu-ifon")?.checked };
    const n = body.hosts.length + body.update_only.length, path = "/api/admin/wake-update";
    $("#wu-go").disabled = true;
    try {
      const r = await controlCall({ kind: "request", label: `Update unpowered hosts: ${n} machine${n === 1 ? "" : "s"}`, method: "POST", path, body },
        () => api(path, { method: "POST", body: JSON.stringify(body) }));
      if (r === null) return;  // "Confirm it's you" took the dialog's place: the run starts after the sign-in
      closeModal(); flash(r.message || "Run started");
      fetchWakeUpdate();
    } catch (x) { if ($("#wu-err")) { $("#wu-err").textContent = x.message; $("#wu-go").disabled = false; } else flash(x.message, true); }
  };
}
// Cancel: nothing new is started. The machines the run has already woken stay on (the default) or are switched
// off again.
function openWakeCancel(id) {
  const m = $("#modal");
  m.innerHTML = `<div class="modal-box" role="dialog" aria-modal="true" aria-labelledby="wc-t"><h3 id="wc-t">Cancel this run?</h3>
    <p>Nothing new is started for any machine: no further wake, no further update. A machine that still waits for its turn is not touched.</p>
    <label class="mfield"><span>The machines the run has already woken</span><select class="select" id="wc-off">
      <option value="">stay on (the report lists them as ON)</option><option value="1">are switched off again (an update that is running is waited for)</option></select></label>
    <div class="error" id="wc-err" role="alert"></div>
    <div class="row-gap end"><button class="btn" type="button" id="wc-x">Keep it running</button><button class="btn danger-solid" type="button" id="wc-go">Cancel the run</button></div></div>`;
  m.hidden = false;
  $("#wc-x").onclick = closeModal;
  m.onclick = (e) => { if (e.target === m) closeModal(); };
  $("#wc-go").onclick = async () => {
    const off = $("#wc-off").value === "1", path = `/api/admin/wake-update/${enc(id)}`, body = { switch_off: off };
    $("#wc-go").disabled = true;
    try {
      const r = await controlCall({ kind: "request", label: `Cancel the run (woken machines ${off ? "are switched off again" : "stay on"})`, method: "DELETE", path, body },
        () => api(path, { method: "DELETE", body: JSON.stringify(body) }));
      if (r === null) return;
      closeModal(); flash(r.message || "Run cancelled");
      fetchWakeUpdate();
    } catch (x) { if ($("#wc-err")) { $("#wc-err").textContent = x.message; $("#wc-go").disabled = false; } else flash(x.message, true); }
  };
}
document.addEventListener("click", async (e) => {
  const t = e.target.closest('[data-act="wu-open"],[data-act="wu-cancel"],[data-act="wu-switch"],[data-act="wu-log"]');
  if (!t || t.disabled) return;
  if (t.dataset.act === "wu-open") { openWakeUpdate(); return; }
  if (t.dataset.act === "wu-cancel") { openWakeCancel(t.dataset.id); return; }
  if (t.dataset.act === "wu-log") { const k = t.dataset.k; if (wu.open[k]) delete wu.open[k]; else wu.open[k] = true; render(); return; }
  const on = t.dataset.on === "1", name = t.dataset.name, path = `/api/systems/${enc(t.dataset.id)}/wake-update`, body = { enabled: on };
  if (on && !confirm(`Switch "Update unpowered hosts" on for ${name}? In a run that you start on the Updates page, Fairy may then wake this machine, update its agent and switch it off again.`)) return;
  const label = `${on ? "Switch on" : "Switch off"} "Update unpowered hosts" for ${name}`;
  try {
    const r = await controlCall({ kind: "request", label, method: "PUT", path, body }, () => api(path, { method: "PUT", body: JSON.stringify(body) }));
    if (r) flash(r.message || `${label}: done`);
  } catch (x) { flash(x.message, true); }
  fetchWakeUpdate();
});

// ------------------------------------------------------------ users, roles and sites (admins only)
const admin = { data: null, error: null };
async function loadAdmin() {
  try { admin.data = await api("/api/admin/users"); admin.error = null; }
  catch (e) { admin.error = e.message; if (e.status === 403) admin.data = null; }
  if (current().page === "users") $("#page").innerHTML = `<div data-key="users">${pageUsers()}</div>`;
}
const ROLE_LABEL = table({ admin: "Admin", viewer: "Viewer", pending: "No access (waiting)" });
// Name and e-mail are what the account claims at Pocket ID; only the account id (OIDC subject) is certain.
// email_verified is true only when Pocket ID vouched for the address at the last sign-in (null = not known yet).
const unverified = (u) => !!u?.email && u.email_verified !== true;
const adminUser = (sub) => admin.data?.users.find((x) => x.sub === sub);
// for confirm() texts and the action label (the label goes percent-encoded in X-Fairy-Action, see headerText)
const userIdent = (u) => !u ? "this user" : `${u.email || u.name || u.sub}${unverified(u) ? " (unverified e-mail)" : ""}${u.email || u.name ? ` [id ${u.sub}]` : ""}`;
const FROM_LABEL = table({ panel: "set here", config: "config.yaml", node: "from its node", link: "follows its hosts" });
const KIND_TITLE = [["hosts", "Hosts", (t) => t.kind === "node" || t.kind === "agent"], ["guests", "VMs & containers", (t) => t.kind === "qemu" || t.kind === "lxc"],
  ["power", "Power sources", (t) => t.kind === "power"], ["ups", "UPS", (t) => t.kind === "ups"]];
function pageUsers() {
  setTitle("Users", "who may see and do what");
  if (!state.me?.can_manage_users) return '<div class="empty">The Users page is for admins only.</div>';
  const d = admin.data;
  if (!d) return admin.error ? `<div class="error">${esc(admin.error)}</div>` : '<div class="empty">Loading…</div>';
  const sites = d.sites.map((x) => x.name);
  const admins = d.users.filter((u) => u.role === "admin").length;
  const userRow = (u) => {
    const last = u.role === "admin" && admins <= 1;
    return `<tr class="row" data-key="u-${esc(u.sub)}"><td class="namecell"><b title="${esc(u.name || "")}">${esc(u.name || "–")}</b>${u.you ? ' <span class="chip ok">you</span>' : ""}
        <span class="sub">${esc(u.email || u.sub)}${unverified(u) ? ' <span class="chip warn" title="Pocket ID did not verify this address (or this person last signed in before the panel recorded it). The owner of the account may have typed it in: go by the id below.">unverified</span>' : ""}</span>
        ${u.email ? `<span class="sub" title="Account id (OIDC subject): the one part of this row the owner of the account cannot change">id ${esc(u.sub)}</span>` : ""}</td>
      <td><select class="select" data-act="u-role" data-sub="${esc(u.sub)}" ${last ? 'disabled title="The last admin cannot be demoted: make someone else admin first"' : ""}>
        ${d.roles.map((r) => `<option value="${esc(r)}" ${u.role === r ? "selected" : ""}>${esc(ROLE_LABEL[r] || r)}</option>`).join("")}</select></td>
      <td>${u.role === "admin" ? '<span class="note">every site</span>' : u.role === "pending" ? '<span class="note">nothing</span>'
        : sites.length ? `<div class="site-checks">${sites.map((x) => `<label class="check"><input type="checkbox" data-act="u-site" data-sub="${esc(u.sub)}" value="${esc(x)}" ${u.sites.includes(x) ? "checked" : ""}>${esc(x)}</label>`).join("")}</div>`
        : '<span class="note">create a site first</span>'}</td>
      <td class="nowrap"><span data-ago="${esc(u.last_seen || "")}">${ago(u.last_seen)}</span><div class="note">${esc(u.via || "")}</div></td>
      <td class="num"><button class="btn danger" data-act="u-remove" data-sub="${esc(u.sub)}" data-name="${esc(userIdent(u))}" ${last ? "disabled" : ""}>Remove</button></td></tr>`;
  };
  const siteRow = (x) => `<tr class="row" data-key="s-${esc(x.name)}"><td><b>${esc(x.name)}</b>${x.in_config ? ' <span class="chip" title="named in config.yaml (site:): rename or remove it there">config.yaml</span>' : ""}</td>
    <td class="num">${esc(x.hosts)}</td><td class="num">${esc(x.users)}</td>
    <td class="num"><button class="btn" data-act="s-rename" data-site="${esc(x.name)}" ${x.in_config ? "disabled" : ""}>Rename</button>
      <button class="btn danger" data-act="s-delete" data-site="${esc(x.name)}" ${x.in_config ? "disabled" : ""}>Delete</button></td></tr>`;
  const targetRow = (t) => {
    const def = t.default ? `${t.default} (${FROM_LABEL[t.default_from] || t.default_from})` : "admins only";
    const where = [t.parent && `on ${t.parent}`, t.follows.length && `follows ${t.follows.join(", ")}`].filter(Boolean).join(" · ");
    return `<tr class="row" data-key="t-${esc(t.target)}"><td class="namecell"><b title="${esc(t.name)}">${esc(t.name)}</b><span class="sub" title="${esc(where ? `${t.target} · ${where}` : t.target)}">${esc(t.target)}${where ? ` · ${esc(where)}` : ""}</span></td>
      <td><select class="select" data-act="a-site" data-target="${esc(t.target)}"><option value="">Default: ${esc(def)}</option>
        ${sites.map((x) => `<option value="${esc(x)}" ${t.assigned === x ? "selected" : ""}>${esc(x)}</option>`).join("")}</select></td>
      <td>${t.site ? `<span class="chip ok">${esc(t.site)}</span>` : '<span class="chip warn">admins only</span>'}</td></tr>`;
  };
  return `<div class="stack">
    <div class="note users-help">Pocket ID decides who may <b>sign in</b>; here you decide what they <b>see</b>.${infoBtn("users", "Users", infoList([
      "<b>Admin</b> sees and controls everything.",
      "<b>Viewer</b> only sees the sites ticked, read-only.",
      "New people wait with <b>No access</b> until you give them a role.",
      "Every change asks you to confirm it's you and is written to the Audit log."]))}</div>
    <div class="section-title">People <span class="count">${d.users.length}</span></div>
    <div class="panel glass scroll"><table class="table users"><thead><tr><th>User</th><th>Role</th><th>Sites they see</th><th>Last seen</th><th class="num"></th></tr></thead>
      <tbody>${d.users.map(userRow).join("") || '<tr><td colspan="5" class="note">Nobody has signed in yet.</td></tr>'}</tbody></table></div>
    <div class="section-title">Sites <span class="count">${d.sites.length}</span></div>
    <form class="toolbar" id="site-form"><input class="input" id="site-name" maxlength="40" placeholder="New site name, e.g. Home" autocomplete="off"><button class="btn" type="submit">Create site</button></form>
    <div class="panel glass scroll"><table class="table sites"><thead><tr><th>Site</th><th class="num">Hosts</th><th class="num">Viewers</th><th class="num"></th></tr></thead>
      <tbody>${d.sites.map(siteRow).join("") || '<tr><td colspan="4" class="note">No sites yet.</td></tr>'}</tbody></table></div>
    ${KIND_TITLE.map(([k, title, f]) => {
      const rows = d.targets.filter(f);
      return rows.length ? `<div class="section-title">${title} <span class="count">${rows.length}</span></div>
        <div class="panel glass scroll"><table class="table assign"><thead><tr><th>${k === "guests" ? "Guest" : "Name"}</th><th>Site</th><th>Visible to</th></tr></thead>
        <tbody>${rows.map(targetRow).join("")}</tbody></table></div>` : "";
    }).join("")}
    <div class="note info-keep">Anything without a site is visible to admins only.${infoBtn("users-sites", "Sites", infoList([
      "A guest follows its node, unless you pick a site for it here.",
      "A power source or a UPS follows the hosts it is attached to, unless you pick a site for it here."]))}</div></div>`;
}
async function adminCall(label, method, path, body) {
  try {
    const r = await controlCall({ kind: "request", label, method, path, body },
      () => api(path, { method, body: body === undefined ? undefined : JSON.stringify(body) }));
    if (r) flash(r.message || `${label}: done`);
  } catch (e) { flash(e.message, true); }
  await loadAdmin();
}
const userPath = (sub) => `/api/admin/users/${enc(sub)}`;
const checkedSites = (sub) => [...document.querySelectorAll(`input[data-act="u-site"]`)].filter((x) => x.dataset.sub === sub && x.checked).map((x) => x.value);
document.addEventListener("change", (e) => {
  const t = e.target, a = t.dataset?.act;
  if (a === "u-role") {
    const u = adminUser(t.dataset.sub);
    if (t.value === "admin" && !confirm(`Make ${userIdent(u)} an admin? An admin controls every host and manages the users.`)) { if (u) t.value = u.role; return; }
    const sites = t.value === "viewer" ? checkedSites(t.dataset.sub) : [];
    adminCall(`Role ${ROLE_LABEL[t.value] || t.value} for ${userIdent(u)}`, "PUT", userPath(t.dataset.sub), { role: t.value, sites });
  } else if (a === "u-site") {
    adminCall(`Sites for ${userIdent(adminUser(t.dataset.sub))}`, "PUT", userPath(t.dataset.sub), { role: "viewer", sites: checkedSites(t.dataset.sub) });
  } else if (a === "a-site") {
    adminCall(t.value ? `Put ${t.dataset.target} in ${t.value}` : `${t.dataset.target}: default site`, "PUT", "/api/admin/assign", { target: t.dataset.target, site: t.value || null });
  }
});
document.addEventListener("click", (e) => {
  const t = e.target.closest('[data-act="u-remove"],[data-act="s-rename"],[data-act="s-delete"]');
  if (!t) return;
  const a = t.dataset.act;
  if (a === "u-remove" && confirm(`Remove ${t.dataset.name} from the list? They are signed out and their API tokens are revoked. If they sign in again they wait for access.`)) {
    adminCall(`Remove ${t.dataset.name}`, "DELETE", userPath(t.dataset.sub));
  } else if (a === "s-rename") {
    const name = prompt(`New name for site “${t.dataset.site}”`, t.dataset.site);
    if (name && name.trim() !== t.dataset.site) adminCall(`Rename site ${t.dataset.site}`, "PUT", `/api/admin/sites/${enc(t.dataset.site)}`, { name: name.trim() });
  } else if (a === "s-delete" && confirm(`Delete site “${t.dataset.site}”? Its hosts become admin-only (or go back to their config.yaml site) and viewers lose it.`)) {
    adminCall(`Delete site ${t.dataset.site}`, "DELETE", `/api/admin/sites/${enc(t.dataset.site)}`);
  }
});
document.addEventListener("submit", (e) => {
  if (e.target.id !== "site-form") return;
  e.preventDefault();
  const name = $("#site-name").value.trim();
  if (name) adminCall(`Create site ${name}`, "POST", "/api/admin/sites", { name });
});

// ------------------------------------------------------------ sessions (everyone: their own; admins: everyone's)
// Where this account is signed in, and "Sign out" for the other ones. The page gets a handle per session (id),
// never a session id. Signing another session out always asks "Confirm it's you" (the server keeps the request
// and runs it after the sign-in); the session this page uses is ended with Sign out in the menu.
// Everything shown comes from the browser that signed in (User-Agent), the network (address) or the identity
// provider (name, e-mail): all of it goes through esc().
const sess = { mine: null, all: null, error: null, allError: null };
async function loadSessions() {
  try { sess.mine = await api("/api/me/sessions"); sess.error = null; }
  catch (e) { sess.error = e.message; }
  if (state.me?.can_manage_users) {
    try { sess.all = await api("/api/admin/sessions"); sess.allError = null; }
    catch (e) { sess.allError = e.message; if (e.status === 403) sess.all = null; }
  }
  render();
  if (current().page === "sessions") schedule(loadSessions, 15000);  // sessions end and begin elsewhere
}
const fmtSpan = (s) => {  // 14400 -> "4 hours"
  const n = +s || 0, [v, u] = n % 86400 === 0 ? [n / 86400, "day"] : n % 3600 === 0 ? [n / 3600, "hour"] : n % 60 === 0 ? [n / 60, "minute"] : [n, "second"];
  return `${v} ${u}${v === 1 ? "" : "s"}`;
};
const sessWhen = (iso) => (iso ? `<span data-ago="${esc(iso)}">${ago(iso)}</span><div class="note">${esc(new Date(iso).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }))}</div>`
  : '<span class="note">unknown</span>');
// for the button's label and the "Confirm it's you" dialog (the label goes percent-encoded in X-Fairy-Action)
const sessName = (x) => `${x.browser || "Unknown browser"}${x.ip || x.signed_in_ip ? ` (${x.ip || x.signed_in_ip})` : ""}`;
const sessWho = (u) => (u ? u.email || u.name || u.sub : "");
function sessRow(x, everyone) {
  const u = x.user, ip = x.ip || x.signed_in_ip;
  const who = everyone ? `<td class="namecell" data-l="User"><b>${esc(u?.name || u?.email || u?.sub || "–")}</b>${u?.you ? ' <span class="chip ok">you</span>' : ""}
      <span class="sub">${esc(u?.email && u?.name ? u.email : "")}</span><span class="sub" title="Account id (OIDC subject)">id ${esc(u?.sub)}</span></td>` : "";
  return `<tr class="row${x.current ? " cur" : ""}" data-key="ss-${esc(x.id)}">${who}
    <td class="namecell" data-l="Browser"><b>${esc(x.browser || "Unknown browser")}</b>${x.current ? ' <span class="chip ok">This session</span>' : ""}
      <span class="sub ua" title="${esc(x.user_agent)}">${x.user_agent ? esc(x.user_agent) : "browser not recorded"}</span></td>
    <td data-l="IP address">${ip ? esc(ip) : '<span class="note">unknown</span>'}${x.location ? `<div class="note">${esc(x.location)}</div>` : ""}
      ${x.ip && x.signed_in_ip && x.ip !== x.signed_in_ip ? `<div class="note">signed in from ${esc(x.signed_in_ip)}</div>` : ""}
      ${!x.ip && x.signed_in_ip ? '<div class="note">at sign-in</div>' : ""}
      ${(x.also_from || []).map((a) => `<div class="note warn-t" title="The same sign-in was also used from this address. A phone or laptop that changes networks does that. If it was not you, sign this session out: that ends every copy of it.">also used from ${esc(a.ip)} · <span data-ago="${esc(a.at)}">${ago(a.at)}</span></div>`).join("")}</td>
    <td class="nowrap" data-l="Signed in">${sessWhen(x.signed_in)}</td>
    <td class="nowrap" data-l="Last active">${sessWhen(x.last_active)}</td>
    <td class="num act">${x.current ? '<span class="note">Sign out is in the menu</span>'
      : `<button class="btn danger" data-act="sess-revoke" data-id="${esc(x.id)}" data-scope="${everyone ? "all" : "me"}"
          data-label="${esc(`${everyone && !u?.you ? `${sessWho(u)}: ` : ""}${sessName(x)}`)}">Sign out</button>`}</td></tr>`;
}
const sessTable = (rows, everyone) => `<div class="panel glass scroll"><table class="table sessions${everyone ? " all" : ""}">
  <thead><tr>${everyone ? "<th>User</th>" : ""}<th>Browser / device</th><th>IP address</th><th>Signed in</th><th>Last active</th><th class="num"></th></tr></thead>
  <tbody>${rows.map((x) => sessRow(x, everyone)).join("")}</tbody></table></div>`;
function pageSessions() {
  setTitle("Sessions", "where you are signed in");
  const d = sess.mine;
  if (!d) return sess.error ? `<div class="error">${esc(sess.error)}</div>` : '<div class="empty">Loading…</div>';
  const mine = d.sessions || [], others = mine.filter((x) => !x.current).length;
  const everyone = state.me?.can_manage_users ? (sess.all ? `<div class="section-title">Everyone's sessions <span class="count">${esc(sess.all.sessions.length)}</span></div>
      ${sess.all.sessions.length ? sessTable(sess.all.sessions, true) : '<div class="empty">Nobody is signed in.</div>'}`
    : sess.allError ? `<div class="error">${esc(sess.allError)}</div>` : "") : "";
  return `<div class="stack">
    <div class="note users-help">Every browser you signed in with is a session; <b>Sign out</b> ends one of the others.${infoBtn("sess", "Sessions", infoList([
      "Sign out asks you to confirm it's you first, every time, so that nobody who got hold of one sign-in can end yours.",
      `A session also ends by itself ${esc(fmtSpan(d.idle_timeout_s))} after it was last used, and ${esc(fmtSpan(d.max_age_s))} after its sign-in.`]))}</div>
    ${sess.error ? `<div class="error">${esc(sess.error)}</div>` : ""}
    <div class="sess-head"><div class="section-title">Your sessions <span class="count">${esc(mine.length)}</span></div>
      <button class="btn danger" data-act="sess-others" ${others ? "" : 'disabled title="You are signed in nowhere else"'}>Sign out everywhere else</button></div>
    ${mine.length ? sessTable(mine, false) : `<div class="empty">No sessions to show${state.me?.demo ? ": the demo has no sign-in." : "."}</div>`}
    ${everyone}</div>`;
}
async function sessCall(label, path, body) {
  try {
    const r = await controlCall({ kind: "request", label, method: "POST", path, body },
      () => api(path, { method: "POST", body: body === undefined ? undefined : JSON.stringify(body) }));
    if (r) flash(r.message || `${label}: done`);
  } catch (e) { flash(e.message, true); }
  loadSessions();
}
document.addEventListener("click", (e) => {
  const t = e.target.closest('[data-act="sess-revoke"],[data-act="sess-others"]');
  if (!t || t.disabled) return;
  if (t.dataset.act === "sess-others") sessCall("Sign out everywhere else", "/api/me/sessions/revoke-others");
  else sessCall(`Sign out ${t.dataset.label}`, t.dataset.scope === "all" ? "/api/admin/sessions/revoke" : "/api/me/sessions/revoke", { session: t.dataset.id });
});

// ------------------------------------------------------------ power confirmation dialog
function openPowerDialog(s, action) {
  const caps = state.caps[s.id], cap = caps?.actions.find((a) => a.action === action);
  if (!cap) return;
  const typed = cap.confirm === "typed", label = POWER_LABEL[action];
  state.menu = null; render();
  const m = $("#modal");
  m.innerHTML = `<div class="modal-box" role="dialog" aria-modal="true" aria-labelledby="mtitle">
    <h3 id="mtitle">${esc(label)} ${esc(s.name)}?</h3>
    <p>${action === "stop" ? "Hard stop: like pulling the power plug. Unsaved data in the guest is lost." :
      action === "shutdown" ? "Clean shutdown (ACPI / OS shutdown)." : action === "reboot" ? "Clean reboot." :
      action === "bmc_on" ? `Power on through the BMC, asked by ${esc(cap.via_label || "the relay agent")}. If it fails, check that the relay is cabled to (or on the network of) the BMC.` :
      action === "sleep" ? "Sleep (suspend to RAM, not hibernate). Wake it with Power on (Wake-on-LAN): from sleep that needs the network card's \"Allow this device to wake the computer\" / \"Wake on Magic Packet\"." : ""}
      <span class="note">via ${esc(cap.via_label || cap.via)}</span></p>
    ${caps.kills_panel ? `<div class="warnbox">${esc(caps.warning)}</div>` : ""}
    ${cap.delayed ? `<label class="mfield">Delay <select id="m-delay"><option value="10">10 s</option><option value="60" selected>1 min</option><option value="300">5 min</option><option value="900">15 min</option></select>
      <span class="note">a countdown with Cancel is shown</span></label>` : ""}
    ${typed ? `<label class="mfield"><span>Type <b>${esc(s.name)}</b> to confirm</span><input class="input" id="m-confirm" autocomplete="off" spellcheck="false" placeholder="${esc(s.name)}"></label>` : ""}
    <div class="error" id="m-err"></div>
    <div class="row-gap end"><button class="btn" id="m-cancel">Cancel</button><button class="btn danger-solid" id="m-go" ${typed ? "disabled" : ""}>${esc(label)}</button></div></div>`;
  m.hidden = false;
  const input = $("#m-confirm"), go = $("#m-go");
  if (input) { input.oninput = () => (go.disabled = input.value.trim().toLowerCase() !== s.name.toLowerCase()); setTimeout(() => input.focus(), 0); }
  const close = () => { m.hidden = true; m.innerHTML = ""; };
  $("#m-cancel").onclick = close;
  m.onclick = (e) => { if (e.target === m) close(); };
  go.onclick = async () => {
    go.disabled = true;
    const body = { action, confirm: input ? input.value : null };
    if ($("#m-delay")) body.delay_s = +$("#m-delay").value;
    // a kept request runs with this countdown, so "Confirm it's you" names it; "Resume" opens this dialog again
    const what = `${label} ${s.name}`, timed = body.delay_s ? `${what} in ${fmtSpan(body.delay_s)}` : what;
    try {
      const r = await api(`/api/systems/${enc(s.id)}/power`, { method: "POST", body: JSON.stringify(body), label: timed });
      close(); state.menu = null; flash(r.message); refreshCaps(s.id);
      actionDone({ kind: "power", system: s.id, action });
    } catch (e) {
      if (e.stepUp) { close(); showStepUp({ kind: "power", action, system: s.id, label: e.detail?.pending ? timed : what }, e.detail); return; }
      $("#m-err").textContent = e.message; go.disabled = false;
    }
  };
}
async function powerNow(s, action) {
  const path = `/api/systems/${enc(s.id)}/power`;
  try {
    const r = await controlCall({ kind: "request", label: `${POWER_LABEL[action]} · ${s.name}`, system: s.id, method: "POST", path, body: { action } },
      () => api(path, { method: "POST", body: JSON.stringify({ action }) }));
    if (r) { flash(r.message); refreshCaps(s.id); }
  } catch (e) { flash(e.message, true); }
}
// The "Power on" buttons (Wake-on-LAN and BMC) are enabled from the power capabilities, so a system page that
// can show one fetches them up front, without waiting for the Power menu to be opened.
const POWER_ON_ACTIONS = ["wake", "bmc_on"];
const hasPowerOnButton = (s) => !!s && (s.power_actions || []).some((a) => POWER_ON_ACTIONS.includes(a));
let capsLoading = null;
function ensurePowerOnCaps() {  // after a snapshot: the page may have opened before the system was known
  const r = current();
  if (r.page !== "system" || state.caps[r.id] || capsLoading === r.id || !hasPowerOnButton(state.byId[r.id])) return;
  capsLoading = r.id;
  refreshCaps(r.id).finally(() => { if (capsLoading === r.id) capsLoading = null; });
}
async function refreshCaps(id) {
  try { state.caps[id] = await api(`/api/systems/${enc(id)}/power`); } catch { /* ignore */ }
  render();
}
async function refreshPoll(id) {
  try { state.poll[id] = await api(`/api/systems/${enc(id)}/poll`); } catch { /* ignore */ }
  render();
}
function flash(msg, bad = false) {
  state.flash = { msg, bad, until: Date.now() + 5000 };
  const f = $("#flash"); f.textContent = msg; f.className = `flash ${bad ? "bad" : ""}`; f.hidden = false;
  clearTimeout(flash.t); flash.t = setTimeout(() => (f.hidden = true), 5000);
}

// Delegated clicks: survive DOM patching.
document.addEventListener("click", async (e) => {
  const ht = e.target.closest("[data-host-toggle]");
  if (ht) {
    const id = ht.dataset.hostToggle, cur = state.byId[current().id], forced = cur && parentOf(cur)?.id === id;
    state.open[id] = !(state.open[id] || forced);
    if (!state.open[id]) delete state.open[id];
    saveOpen(); renderNav(); return;
  }
  const row = e.target.closest("tr[data-href]");
  if (row && !e.target.closest("a,button,input,select,label")) { location.hash = row.dataset.href; return; }
  const t = e.target.closest("[data-act]");
  const r = current();
  if (!t) { if (state.menu && !e.target.closest(".menu")) { state.menu = null; render(); } return; }
  const s = state.byId[r.id];
  const act = t.dataset.act;
  if (act === "menu" && s) {
    const kind = t.dataset.menu;
    state.menu = state.menu?.kind === kind && state.menu.id === s.id ? null : { id: s.id, kind };
    render();
    if (state.menu && kind !== "helper") (kind === "poll" ? refreshPoll : refreshCaps)(s.id);
    if (state.menu && kind === "poll") fetchWakeUpdate();  // the menu also holds the "Update unpowered hosts" switch
  } else if (act === "poll" && s) {
    const v = t.dataset.v, body = { interval_s: v === "paused" || v === "default" ? v : +v };
    const label = `Polling for ${s.name}: ${v === "default" ? "back to default" : v === "paused" ? "paused" : `${v}s`}`;
    try {
      const r = await controlCall({ kind: "request", label, system: s.id, method: "PUT", path: `/api/systems/${enc(s.id)}/poll`, body },
        () => api(`/api/systems/${enc(s.id)}/poll`, { method: "PUT", body: JSON.stringify(body) }));
      if (r) { state.poll[s.id] = r; flash(label); }
    } catch (x) { flash(x.message, true); }
    render();
  } else if (act === "power" && s) {
    const a = t.dataset.action;
    const cap = state.caps[s.id]?.actions.find((c) => c.action === a);
    if (cap?.confirm === "none") powerNow(s, a); else openPowerDialog(s, a);
  } else if (act === "resume") { runPending();
  } else if (act === "resume-drop") { dropPending(); render();
  } else if (act === "power-now" && s) {
    if (!state.caps[s.id]) await refreshCaps(s.id);
    powerNow(s, t.dataset.action);
  }
});
document.addEventListener("keydown", (e) => { if (e.key === "Escape") { state.menu = null; if (!$("#modal").hidden) closeModal(); render(); } });

// ------------------------------------------------------------ dialogs and the keyboard
// Every dialog is built into #modal, so this is the one place for all of them. While a dialog is open, Tab and
// Shift+Tab stay inside it: the page behind cannot be reached with the keyboard. When it opens, the focus is
// put into it unless the dialog did that itself; when it closes, the focus goes back to what had it before
// (the button that opened it), if that is still on the page and nothing else took the focus.
const modalKeys = { open: false, opener: null, sel: "", last: null, lastSel: "" };
// What had the focus last while no dialog was open. An item of a menu goes away with its menu when the dialog
// opens ("Set model name", "Shut down"): the menu's button stands for it, found again by its name if the
// page drew it anew.
document.addEventListener("focusin", (e) => {
  if (!$("#modal").hidden || !(e.target instanceof Element)) return;
  const btn = e.target.closest(".menu-item") ? e.target.closest(".menu-wrap")?.querySelector('[data-act="menu"][data-menu]') : null;
  modalKeys.last = btn || e.target;
  modalKeys.lastSel = btn ? `[data-act="menu"][data-menu="${CSS.escape(btn.dataset.menu)}"]` : "";
});
const modalStops = () => [...$("#modal").querySelectorAll("a[href], button, input, select, textarea, [tabindex]")]
  .filter((el) => !el.disabled && el.tabIndex >= 0 && el.type !== "hidden" && el.getClientRects().length > 0);
new MutationObserver((changes) => {
  const m = $("#modal");
  const intoIt = () => setTimeout(() => { if (!m.hidden && !m.contains(document.activeElement)) modalStops()[0]?.focus(); }, 30);  // after a dialog's own focus (0 ms)
  if (!m.hidden && !modalKeys.open) {
    const a = document.activeElement;
    const here = a && a !== document.body && !m.contains(a) && !a.closest(".menu-item");
    modalKeys.open = true;
    modalKeys.opener = here ? a : modalKeys.last; modalKeys.sel = here ? "" : modalKeys.lastSel;
    intoIt();
  } else if (!m.hidden && changes.some((c) => c.type === "childList")) {
    // Another dialog took this one's place ("Confirm it's you" over the power dialog): the control that had
    // the focus is gone with the old one, so the focus goes to the new dialog's first control, as when a
    // dialog opens. The opener stays the one of the first dialog.
    intoIt();
  } else if (m.hidden && modalKeys.open) {
    const o = modalKeys.opener, sel = modalKeys.sel;
    modalKeys.open = false; modalKeys.opener = null;
    setTimeout(() => {
      if (document.activeElement && document.activeElement !== document.body) return;  // something else has it (a dialog's own way back)
      (o?.isConnected ? o : sel ? document.querySelector(sel) : null)?.focus();
    }, 0);
  }
}).observe($("#modal"), { attributes: true, attributeFilter: ["hidden"], childList: true });
document.addEventListener("keydown", (e) => {
  const m = $("#modal");
  if (e.key !== "Tab" || m.hidden || info.key) return;  // an open info box has its own way out and back (Escape)
  const stops = modalStops(), a = document.activeElement;
  if (!stops.length) { e.preventDefault(); return; }
  const first = stops[0], last = stops[stops.length - 1];
  if (!m.contains(a)) { e.preventDefault(); (e.shiftKey ? last : first).focus(); }
  else if (e.shiftKey && a === first) { e.preventDefault(); last.focus(); }
  else if (!e.shiftKey && a === last) { e.preventDefault(); first.focus(); }
});

function pageSettings() {
  setTitle("Settings", "appearance");
  const s = state.settings;
  const bg = s.background;
  return `<div class="settings stack"><div class="panel glass pad"><h3>Background${infoBtn("set-bg", "Background", infoList([
      "The theme is true black. A picture is optional.",
      "Upload: PNG, JPEG, WebP, GIF or AVIF, at most 8 MiB. No SVG.",
      "Or paste the https URL of a picture."]))}</h3>
    <div class="note">A picture behind your own pages: upload one or paste an https URL.</div>
    <div class="row-gap section-title"><label class="btn" for="bg-file">Upload image…</label><input id="bg-file" type="file" accept="image/png,image/jpeg,image/webp,image/gif,image/avif" hidden>
      <button class="btn danger" id="bg-clear" ${bg.kind === "none" ? "disabled" : ""}>Remove background</button></div>
    <div class="row-gap"><input class="input" id="bg-url" type="url" placeholder="https://example.com/wallpaper.jpg" value="${bg.kind === "url" ? esc(bg.url) : ""}"><button class="btn" id="bg-url-apply">Use URL</button></div>
    <div class="preview" id="bg-preview"></div><div class="error" id="set-err"></div></div>${signInPanel()}
    <div class="panel glass pad"><h3>Readability</h3>
    <div class="field"><label for="dim">Background dim</label><input type="range" id="dim" min="0" max="90" step="5" value="${esc(s.dim)}"><output id="dim-v">${esc(s.dim)}%</output></div>
    <div class="field"><label for="blur">Card blur</label><input type="range" id="blur" min="0" max="40" step="2" value="${esc(s.blur)}"><output id="blur-v">${esc(s.blur)}px</output></div>
    <div class="field"><label for="op">Card opacity</label><input type="range" id="op" min="20" max="100" step="2" value="${esc(s.card_opacity)}"><output id="op-v">${esc(s.card_opacity)}%</output></div>
    <div class="note" id="saved-note">Saved to your account (per Pocket ID user).</div></div>${popupsPanel()}</div>`;
}

function bindSettings() {
  const url = bgUrl(state.settings);
  if (url) $("#bg-preview").style.backgroundImage = cssUrl(url);
  const err = (e) => ($("#set-err").textContent = e.message || String(e));
  $("#bg-file").onchange = async (e) => {
    const file = e.target.files[0];
    if (!file) return;
    try { state.settings = await api("/api/settings/background", { method: "POST", body: file, headers: { "Content-Type": file.type || "application/octet-stream" } }); afterSettings(); }
    catch (x) { err(x); }
  };
  $("#bg-clear").onclick = async () => {
    try {
      state.settings = state.settings.background.kind === "upload" ? await api("/api/settings/background", { method: "DELETE" })
        : { ...state.settings, background: { kind: "none", url: null, version: state.settings.background.version } };
      await saveSettings(); afterSettings();
    } catch (x) { err(x); }
  };
  $("#bg-url-apply").onclick = async () => {
    const v = $("#bg-url").value.trim();
    if (!/^https:\/\//i.test(v)) return err("URL must start with https://");
    state.settings = { ...state.settings, background: { kind: "url", url: v, version: state.settings.background.version } };
    await saveSettings(); afterSettings();
  };
  for (const [id, key, unit] of [["dim", "dim", "%"], ["blur", "blur", "px"], ["op", "card_opacity", "%"]]) {
    const el = $(`#${id}`);
    el.oninput = () => { state.settings[key] = +el.value; $(`#${id}-v`).textContent = `${el.value}${unit}`; applySettings(); };
    el.onchange = () => saveSettings();
  }
  bindSignIn();
  bindPopups();
}
function afterSettings() { applySettings(); route(); }

// ---- the sign-in wallpaper (admins): the picture behind the sign-in page, one for the whole panel. The panel
// keeps a copy and shows it to everyone who can open this address, before they sign in (GET /branding/wallpaper).
let signInWall = null;  // { set, type, bytes, version, max_bytes } as the panel last said; null: not asked yet
function signInPanel() {
  if (state.me?.role !== "admin") return "";
  return `<div class="panel glass pad" id="siw"><h3>Sign-in page${infoBtn("set-siw", "Sign-in page", infoList([
      "Upload only: PNG, JPEG or WebP, at most 8 MiB.",
      "It does not change your own background above."]))}</h3>
    <div class="note info-keep">Picture behind the sign-in page, the same for everyone. Anyone who can open this panel's address sees it before signing in, so pick one that may be public.</div>
    <div class="row-gap section-title"><label class="btn" for="siw-file">Upload picture…</label><input id="siw-file" type="file" accept="image/png,image/jpeg,image/webp" hidden>
      <button class="btn danger" id="siw-clear" disabled>Remove picture</button></div>
    <div class="preview" id="siw-preview"></div><div class="note" id="siw-state">Loading…</div><div class="error" id="siw-err"></div></div>`;
}
function bindSignIn() {
  if (!$("#siw")) return;
  const err = (e) => { if ($("#siw-err")) $("#siw-err").textContent = e ? e.message || String(e) : ""; };
  const show = (w) => {
    signInWall = w;
    if (!$("#siw")) return;  // another page by now
    $("#siw-clear").disabled = !w.set;
    $("#siw-preview").style.backgroundImage = w.set ? cssUrl(`/branding/wallpaper?v=${enc(w.version)}`) : "none";
    $("#siw-state").textContent = w.set ? `${{ "image/png": "PNG", "image/jpeg": "JPEG", "image/webp": "WebP" }[w.type] || "Picture"}, ${fmtBytes(w.bytes)}. Shown on the sign-in page.`
      : "No picture: the sign-in page has the plain black background.";
  };
  if (signInWall) show(signInWall);
  api("/api/admin/signin-wallpaper").then(show).catch(err);
  $("#siw-file").onchange = async (e) => {
    const file = e.target.files[0];
    e.target.value = "";  // the same file can be chosen again
    if (!file) return;
    err(null);
    if (signInWall?.max_bytes && file.size > signInWall.max_bytes) return err(new Error(`picture larger than ${Math.round(signInWall.max_bytes / 2 ** 20)} MiB`));  // the panel's own words for it
    try { show(await api("/api/admin/signin-wallpaper", { method: "POST", body: file, headers: { "Content-Type": "application/octet-stream" } })); flash("Sign-in picture saved"); }
    catch (x) {  // 413 without the panel's own words: the proxy in front of it refused the upload (README, "Sign-in wallpaper")
      err(x.status === 413 && !x.detail ? new Error("The upload was refused as too large before it reached the panel. nginx needs client_max_body_size 9m for /api/admin/signin-wallpaper, as for the background upload.") : x);
    }
  };
  $("#siw-clear").onclick = async () => {
    err(null);
    try { show(await api("/api/admin/signin-wallpaper", { method: "DELETE" })); flash("Sign-in picture removed"); }
    catch (x) { err(x); }
  };
}

// ------------------------------------------------------------ settings persistence (server, localStorage fallback)
const LS_KEY = "fairy-settings";
const cssUrl = (u) => `url("${String(u).replace(/["\\\n\r]/g, encodeURIComponent)}")`;
const bgUrl = (s) => (s.background.kind === "upload" ? `/api/settings/background?v=${enc(s.background.version)}` : s.background.kind === "url" ? s.background.url : null);
function applySettings() {
  const s = state.settings, root = document.documentElement.style, url = bgUrl(s);
  document.body.classList.toggle("has-bg", !!url);
  $("#bg").style.backgroundImage = url ? cssUrl(url) : "none";
  root.setProperty("--dim", String(s.dim / 100));
  root.setProperty("--blur", `${s.blur}px`);
  root.setProperty("--card-alpha", String(s.card_opacity / 100));
}
// The copy kept in this browser (written when a save to the panel failed) is used only when the panel cannot be
// asked, and only if it is what the page itself writes there: the same kinds and ranges the panel takes
// (settings.Validate), a background address only with https. Anything else is not used at all: the defaults.
function storedSettings(raw) {
  let s;
  try { s = JSON.parse(raw); } catch { return null; }
  const obj = (v) => v !== null && typeof v === "object" && !Array.isArray(v);
  const int = (v, lo, hi) => Number.isInteger(v) && v >= lo && v <= hi;
  if (!obj(s) || !obj(s.background) || !int(s.dim, 0, 90) || !int(s.blur, 0, 40) || !int(s.card_opacity, 20, 100)) return null;
  const b = s.background, version = Number.isSafeInteger(b.version) && b.version >= 0 ? b.version : 0;
  let url = null;
  if (b.kind === "url") {
    if (typeof b.url !== "string" || b.url.length > 2083 || /[\u0000-\u0020\u007f]/.test(b.url)) return null;
    try { const u = new URL(b.url); if (u.protocol !== "https:" || !u.host) return null; } catch { return null; }
    url = b.url;
  } else if (b.kind !== "none" && b.kind !== "upload") return null;
  const out = { theme: "black", background: { kind: b.kind, url, version }, dim: s.dim, blur: s.blur, card_opacity: s.card_opacity };
  if (obj(s.popups)) out.popups = { enabled: s.popups.enabled, seconds: s.popups.seconds, level: s.popups.level };  // popPrefs checks each
  return out;
}
async function loadSettings() {
  try { state.settings = await api("/api/settings"); }
  catch { let raw = null; try { raw = localStorage.getItem(LS_KEY); } catch { /* storage blocked */ } state.settings = storedSettings(raw); }
  state.settings ||= { theme: "black", background: { kind: "none", url: null, version: 0 }, dim: 60, blur: 14, card_opacity: 72 };
  state.settings.popups = popPrefs();  // settings kept in this browser before pop-ups existed have none: the defaults
  applySettings();
}
async function saveSettings() {
  try {
    state.settings = await api("/api/settings", { method: "PUT", body: JSON.stringify(state.settings) });
    const n = $("#saved-note"); if (n) n.textContent = "Saved to your account (per Pocket ID user).";
  } catch (e) {
    try { localStorage.setItem(LS_KEY, JSON.stringify(state.settings)); } catch { /* storage blocked */ }
    const n = $("#saved-note"); if (n) n.textContent = `Server save failed (${e.message}); kept in this browser only.`;
  }
}



// ------------------------------------------------------------ notifications (everyone with a role)
// Events made on state changes (host offline / back, too hot / normal, UPS, power, updates, Add host), newest
// first, with this user's read / dismissed marks. The bell and the sidebar show the unread count (from the stream).
const notes = { list: [], next: null, f: { severity: "", type: "", host: "", unread: false }, loading: false, error: null, seen: -1 };
const NOTE_TYPES = [["", "All types"], ["host", "Hosts"], ["temperature", "Temperature"], ["ups", "UPS"], ["power", "Power"], ["update", "Updates"], ["enroll", "Add host"]];
const NOTE_SEV = [["", "All severities"], ["critical", "Critical"], ["warning", "Warning"], ["info", "Info"]];
async function loadNotes(reset = true) {
  if (notes.loading) return;
  notes.loading = true;
  const qs = new URLSearchParams({ limit: "50" });
  for (const k of ["severity", "type", "host"]) if (notes.f[k]) qs.set(k, notes.f[k]);
  if (notes.f.unread) qs.set("unread", "1");
  if (!reset && notes.next != null) qs.set("before", notes.next);
  try {
    const d = await api(`/api/notifications?${qs}`);
    notes.list = reset ? d.events : [...notes.list, ...d.events];
    notes.next = d.next; notes.error = null;
    state.notes = { unread: d.unread, active_alerts: d.active_alerts };
    updateBell();
  } catch (e) { notes.error = e.message; }
  notes.loading = false;
  render();
}
function noteLink(e) {
  if (e.system_id && state.byId[e.system_id]) return `#/system/${enc(e.system_id)}`;
  if (e.ups_id) return "#/ups";
  if (e.type === "update" && state.me?.can_control) return "#/updates";
  return "";
}
// One word for one kind of event, in Settings ("Critical: a host goes offline, ..."), in a pop-up, in this list
// and in its filter: critical is an event of severity critical or one that starts an alert (`alarm`; the
// stream calls both `urgent`). The stored severity of "host offline" is still warning (push priority).
const noteSev = (e) => (e.severity === "critical" || e.alarm === true || e.urgent === true ? "critical" : e.severity);
function noteRow(e) {
  const href = noteLink(e);
  const when = new Date(e.t);
  const host = e.host ? (href ? `<a href="${href}">${esc(e.host)}</a>` : esc(e.host)) : '<span class="note">–</span>';
  return `<tr class="row ${e.read ? "" : "unread"}" data-key="n${esc(e.id)}"><td class="nowrap"><span data-ago="${esc(e.t)}">${ago(e.t)}</span>
      <div class="note">${when.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })}</div></td>
    <td class="nowrap"><span class="sev ${esc(noteSev(e))}" aria-hidden="true"></span>${esc(noteSev(e))}</td>
    <td>${host}</td>
    <td><span class="ntext">${esc(e.text)}</span>${e.active ? ' <span class="chip active" title="this condition still holds">active</span>'
      : e.resolved ? ` <span class="note">· resolved <span data-ago="${esc(e.resolved)}">${ago(e.resolved)}</span></span>` : ""}</td>
    <td class="note">${esc((NOTE_TYPES.find(([k]) => k === e.type) || [, e.type])[1])}</td>
    <td class="num nowrap">${e.read ? "" : `<button class="btn" type="button" data-act="n-read" data-id="${esc(e.id)}" aria-label="Mark read">Mark read</button>`}
      <button class="btn" type="button" data-act="n-dismiss" data-id="${esc(e.id)}" aria-label="Dismiss" title="Hide it from your list">Dismiss</button></td></tr>`;
}
function pageNotifications() {
  setTitle("Notifications", "what changed, newest first");
  const hosts = state.systems.filter(isHost).sort((a, b) => a.name.localeCompare(b.name));
  const f = notes.f;
  return `<form class="toolbar audit-filters" id="notes-form" data-keep>
      <select class="select" id="nf-sev" aria-label="Severity">${NOTE_SEV.map(([v, l]) => `<option value="${v}" ${f.severity === v ? "selected" : ""}>${l}</option>`).join("")}</select>
      <select class="select" id="nf-type" aria-label="Type">${NOTE_TYPES.map(([v, l]) => `<option value="${v}" ${f.type === v ? "selected" : ""}>${l}</option>`).join("")}</select>
      <select class="select" id="nf-host" aria-label="Host"><option value="">All hosts</option>${hosts.map((h) => `<option value="${esc(h.id)}" ${f.host === h.id ? "selected" : ""}>${esc(h.name)}</option>`).join("")}</select>
      <label class="check"><input type="checkbox" id="nf-unread" ${f.unread ? "checked" : ""}>Unread only</label>
      <button class="btn push" type="button" data-act="n-all-read" ${state.notes.unread ? "" : "disabled"}>Mark all read</button></form>
    ${notes.error ? `<div class="error">${esc(notes.error)}</div>` : ""}
    <div class="panel glass scroll"><table class="table notes"><thead><tr><th>When</th><th>Severity</th><th>Host</th><th>What happened</th><th>Type</th><th class="num"></th></tr></thead><tbody>
    ${notes.list.map(noteRow).join("") || `<tr><td colspan="6" class="note">${notes.loading ? "Loading…" : "Nothing here."}</td></tr>`}</tbody></table></div>
    ${notes.next != null ? `<div class="row-gap center"><button class="btn" type="button" data-act="n-more">${notes.loading ? "Loading…" : "Older"}</button></div>` : ""}
    <p class="note info-keep">Kept for 30 days.${infoBtn("notes", "Notifications", infoList([
      "A host shut down from the panel shows as “off”, not as an alert.",
      "A host that keeps going offline and online gets one “unstable” warning."]))}</p>`;
}
function updateBell() {
  const b = $("#bell");
  if (!b) return;
  const n = state.notes.unread;
  const c = b.querySelector(".count");
  c.textContent = n > 99 ? "99+" : String(n); c.hidden = !n;
  b.setAttribute("aria-label", n ? `Notifications: ${n} unread` : "Notifications");
  b.classList.toggle("on", current().page === "notifications");
}
function onNotesCounts(n) {
  const changed = n.unread !== state.notes.unread || n.active_alerts !== state.notes.active_alerts;
  state.notes = n; updateBell();
  if (changed && current().page === "notifications" && !notes.loading) loadNotes(true);
  else render();
}
async function notesWrite(path, body) {
  try { await api(path, { method: "POST", body: JSON.stringify(body) }); } catch (e) { flash(e.message, true); }
  loadNotes(true);
}
document.addEventListener("change", (e) => {
  if (!e.target.closest?.("#notes-form")) return;
  notes.f = { severity: $("#nf-sev").value, type: $("#nf-type").value, host: $("#nf-host").value, unread: $("#nf-unread").checked };
  loadNotes(true);
});
document.addEventListener("submit", (e) => { if (e.target.id === "notes-form") e.preventDefault(); });
document.addEventListener("click", (e) => {
  const t = e.target.closest('[data-act^="n-"]');
  if (!t || t.disabled) return;
  const id = +t.dataset.id;
  if (t.dataset.act === "n-read") notesWrite("/api/notifications/read", { ids: [id] });
  else if (t.dataset.act === "n-dismiss") notesWrite("/api/notifications/dismiss", { ids: [id] });
  else if (t.dataset.act === "n-all-read") notesWrite("/api/notifications/read", { all: true });
  else if (t.dataset.act === "n-more") loadNotes(false);
});

// ------------------------------------------------------------ pop-ups for new notifications
// A critical event that is made while this page is open shows for a few seconds in the lower right corner (a
// strip at the bottom on a phone) and goes away by itself. It stays on the Notifications page and in the bell,
// and showing it marks nothing as read. The stream's `notes_new` carries the events: only ones made after this
// page was opened. `last` of the first one is where the page starts, and an id at or below the highest one seen
// is ignored, so a reconnect shows nothing twice and nothing old.
// At most POP_MAX at a time; up to POP_WAIT more wait for a free place, and when more than that wait they become
// one "N more" pop-up that leads to Notifications. The time (Settings, 3-30 s) stops while the pointer is over
// a pop-up, while the keyboard focus is in one and while the tab is hidden. × closes one; Escape closes the
// newest, unless a dialog, menu or info panel is open (Escape is theirs). A pop-up never takes the focus.
// Host names and texts are an agent's words: they go in with textContent, never as HTML.
const POP_MAX = 3, POP_WAIT = 5, POP_STALE_MS = 60000;
const pop = { after: null, items: [], wait: [], more: null, hover: false, focus: false, x: 0, y: 0 };
function popPrefs() {  // this user's settings; anything missing or out of range is the default
  const p = state.settings?.popups || {};
  return { enabled: p.enabled !== false, seconds: Number.isInteger(p.seconds) && p.seconds >= 3 && p.seconds <= 30 ? p.seconds : 5,
    level: p.level === "warning" ? "warning" : "critical" };
}
// "Critical only": severity critical, and events that start an alert (`urgent`, decided by the panel). "Critical
// and warnings": every warning too.
const popWanted = (e, p) => p.enabled && (e.urgent === true || e.severity === "critical" || (p.level === "warning" && e.severity === "warning"));
const popOpen = () => !!state.me && state.me.role !== "pending" && !signedOut;  // never on the sign-in or waiting view
const popAll = () => (pop.more ? [...pop.items, pop.more] : pop.items);  // oldest first; the "N more" one is the newest
function onNewNotes(d) {
  if (!Number.isSafeInteger(d?.last)) return;
  // the first one says where this page starts; a lower number than before: the panel's list started over
  if (pop.after === null || d.last < pop.after) { pop.after = d.last; return; }
  const fresh = (Array.isArray(d.events) ? d.events : []).filter((e) => Number.isSafeInteger(e?.id) && e.id > pop.after);
  pop.after = Math.max(d.last, ...fresh.map((e) => e.id));
  const p = popPrefs();
  if (!popOpen() || !p.enabled) return;
  for (const e of fresh) {
    if (popWanted(e, p)) popAdd({ sev: e.severity, urgent: e.urgent === true || e.severity === "critical", host: e.host, text: e.text, href: noteLink(e) || "#/notifications" });
  }
  const n = p.level === "warning" ? d.more : d.more_urgent;  // a flood: what did not fit into the event
  if (Number.isSafeInteger(n) && n > 0) popCount(n, p.level !== "warning");
}
function popAdd(n) {
  n.at = Date.now();
  if (pop.items.length < POP_MAX && !pop.wait.length) { popShow(n); return; }
  if (pop.more) { popCount(1, n.urgent); return; }  // a flood is being counted already
  pop.wait.push(n);
  if (pop.wait.length > POP_WAIT) { const w = pop.wait; pop.wait = []; popCount(w.length, w.every((x) => x.urgent)); }
}
const popSpan = (cls, text) => { const s = document.createElement("span"); s.className = cls; s.textContent = text; return s; };
// One pop-up: a sign, the host's name and one line of text (the whole text is its tooltip), and ×. The body is
// a link: a click or Enter opens what the notification's row links to.
function popEl(it, sev, sign, name, text, href) {
  const el = document.createElement("div"), a = document.createElement("a"), x = document.createElement("button"), main = popSpan("toast-main", "");
  el.className = `toast ${sev}`;
  a.className = "toast-body"; a.href = href; a.title = text;
  const s = popSpan("toast-sign", sign); s.setAttribute("aria-hidden", "true");
  main.append(popSpan("sr", sev === "more" ? "" : `${sev}: `));
  if (name) main.append(popSpan("toast-host", name));
  main.append(popSpan("toast-text", text));
  a.append(s, main);
  x.className = "toast-x"; x.type = "button"; x.textContent = "×"; x.setAttribute("aria-label", "Close");
  a.addEventListener("click", (e) => popClose(it, e.detail === 0));  // the link is followed, the pop-up goes (detail 0: Enter, not the mouse)
  x.addEventListener("click", (e) => popClose(it, e.detail === 0));
  el.append(a, x);
  return el;
}
function popShow(n) {
  const box = $("#toasts"), it = { at: n.at, left: popPrefs().seconds * 1000, timer: null, started: 0 };
  it.el = popEl(it, n.urgent ? "critical" : "warning", "!", String(n.host || ""), String(n.text || ""), n.href);  // urgent: what "Critical only" shows
  pop.items.push(it);
  box.insertBefore(it.el, pop.more?.el || null);  // the "N more" one stays the last (shown on top)
  box.hidden = false;
  popRun(it); placePops();
}
// popCount adds n events to the "N more" pop-up (made when there is none) and starts its time again.
function popCount(n, urgent) {
  const box = $("#toasts");
  let it = pop.more;
  if (!it) {
    it = pop.more = { n: 0, urgent: true, timer: null, started: 0 };
    it.el = popEl(it, "more", "+", "", "", "#/notifications");
    box.append(it.el); box.hidden = false;
  }
  it.n += n; it.urgent = it.urgent && urgent; it.at = Date.now();
  const text = `${it.n} more ${it.urgent ? "critical events" : "notifications"}`;
  it.el.querySelector(".toast-text").textContent = text; it.el.querySelector(".toast-body").title = text;
  clearTimeout(it.timer); it.timer = null; it.left = popPrefs().seconds * 1000;
  popRun(it); placePops();
}
const popPaused = () => pop.hover || pop.focus || document.hidden;
function popRun(it) {
  if (it.timer || popPaused()) return;
  it.started = Date.now();
  it.timer = setTimeout(() => { it.timer = null; popClose(it); }, it.left);
}
function popHold(it) {
  if (!it.timer) return;
  clearTimeout(it.timer); it.timer = null;
  it.left = Math.max(1000, it.left - (Date.now() - it.started));  // what is left goes on later, at least a second
}
function popSync() { for (const it of popAll()) (popPaused() ? popHold : popRun)(it); }
function popClose(it, byKey = false) {
  const box = $("#toasts"), focused = byKey && it.el.contains(document.activeElement);
  clearTimeout(it.timer); it.timer = null;
  it.el.remove();
  if (it === pop.more) pop.more = null; else pop.items = pop.items.filter((x) => x !== it);
  while (pop.items.length < POP_MAX && pop.wait.length) popShow(pop.wait.shift());
  const all = popAll();
  if (focused && all.length) all[all.length - 1].el.querySelector(".toast-x").focus();  // closed with the keyboard: the focus goes on to the next one
  pop.focus = box.contains(document.activeElement);
  // the one under the pointer went (no "pointer left" is sent for that): is another one there now?
  if (pop.hover) pop.hover = box.contains(document.elementFromPoint(pop.x, pop.y));
  if (!all.length) { box.hidden = true; pop.hover = false; }
  popSync(); placePops();
}
function popClear() {  // signed out, or switched off
  for (const it of popAll()) { clearTimeout(it.timer); it.el.remove(); }
  pop.items = []; pop.wait = []; pop.more = null; pop.hover = pop.focus = false;
  const box = $("#toasts"); if (box) box.hidden = true;
}
// Where the pop-ups are: above the flash message when one shows, and under an open dialog (.behind) whenever
// they would lie over it, so a dialog's buttons are never covered. Placed with CSSOM (the CSP allows no inline styles).
function placePops() {
  const box = $("#toasts"), f = $("#flash");
  if (!box || box.hidden) return;
  box.style.bottom = f.hidden ? "" : `${Math.round(innerHeight - f.getBoundingClientRect().top + 8)}px`;
  const r = box.getBoundingClientRect(), d = $("#modal").hidden ? null : $("#modal .modal-box")?.getBoundingClientRect();
  box.classList.toggle("behind", !!d && r.left < d.right && r.right > d.left && r.top < d.bottom && r.bottom > d.top);
}
(() => {
  const box = $("#toasts");
  const over = (e) => { pop.hover = true; pop.x = e.clientX; pop.y = e.clientY; };
  box.addEventListener("pointerenter", (e) => { over(e); popSync(); });
  box.addEventListener("pointermove", over);
  box.addEventListener("pointerleave", () => { pop.hover = false; popSync(); });
  box.addEventListener("focusin", () => { pop.focus = true; popSync(); });
  box.addEventListener("focusout", (e) => { pop.focus = box.contains(e.relatedTarget); popSync(); });
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) {  // back on this tab: what waited here for more than a minute is old news (it is in the bell)
      const old = Date.now() - POP_STALE_MS;
      pop.wait = pop.wait.filter((n) => n.at >= old);
      for (const it of popAll()) if (it.at < old) popClose(it);
    }
    popSync();
  });
  window.addEventListener("resize", placePops);
  const moved = new MutationObserver(() => { if (popAll().length) placePops(); });
  moved.observe($("#modal"), { attributes: true, attributeFilter: ["hidden"], childList: true, subtree: true });
  moved.observe($("#flash"), { attributes: true, attributeFilter: ["hidden"], childList: true, characterData: true, subtree: true });
})();
// Escape closes the pop-up the focus is in, else the newest. Not when something else took this Escape (the info
// panel and the update panel, whose listeners come first and stop the event), nor while the account menu, a
// dialog or a row's menu is open: their own listeners close those.
document.addEventListener("keydown", (e) => {
  const all = popAll();
  if (e.key !== "Escape" || !all.length || e.cancelBubble) return;
  const mine = all.find((it) => it.el.contains(document.activeElement));
  if (!mine && (state.menu || acctOpen() || !$("#modal").hidden)) return;
  e.preventDefault(); e.stopPropagation();
  popClose(mine || all[all.length - 1], true);
}, true);
// Settings, "Pop-ups": on / off, how long one stays, which events; saved with the other settings of the account.
function popupsPanel() {
  const p = popPrefs(), off = p.enabled ? "" : "disabled";
  return `<div class="panel glass pad" id="popset"><h3>Pop-ups${infoBtn("set-pop", "Pop-ups", infoList([
      "A new critical event shows in the corner for a few seconds. It stays in Notifications; nothing is marked as read.",
      "Critical: a host goes offline, a temperature passes its limit, a UPS goes on battery, runs low or cannot be reached.",
      "Warnings: also a failed update, an unstable host, a failed Add host, a UPS overload or “replace battery”.",
      "A host shut down from the panel is “off”, not an alert: no pop-up.",
      "At most three at a time; many at once become one “N more”. Pointing at one keeps it open. Escape closes the newest.",
      "The test pop-up makes no notification."]))}</h3>
    <div class="field"><label for="pop-on">Show pop-ups</label><span class="check"><input type="checkbox" id="pop-on" ${p.enabled ? "checked" : ""}></span></div>
    <div class="field"><label for="pop-secs">Stays for</label><input type="range" id="pop-secs" min="3" max="30" step="1" value="${esc(p.seconds)}" ${off}><output id="pop-secs-v">${esc(p.seconds)} s</output></div>
    <div class="field"><label for="pop-level">Events</label><select class="select" id="pop-level" ${off}>
      <option value="critical" ${p.level === "critical" ? "selected" : ""}>Critical only</option><option value="warning" ${p.level === "warning" ? "selected" : ""}>Critical and warnings</option></select></div>
    <div class="row-gap"><button class="btn" type="button" id="pop-test" ${off}>Show a test pop-up</button></div></div>`;
}
function bindPopups() {
  const on = $("#pop-on"), secs = $("#pop-secs"), level = $("#pop-level"), test = $("#pop-test");
  const save = async () => {
    state.settings.popups = { enabled: on.checked, seconds: +secs.value, level: level.value };
    for (const el of [secs, level, test]) el.disabled = !on.checked;
    if (!on.checked) popClear();
    await saveSettings();
  };
  on.onchange = save; level.onchange = save; secs.onchange = save;
  secs.oninput = () => { $("#pop-secs-v").textContent = `${secs.value} s`; };
  // shown in this browser only: no notification is made, nothing is sent
  test.onclick = () => popAdd({ sev: "critical", urgent: true, host: "Test pop-up", text: "This is how a critical event looks.", href: "#/notifications" });
}

// ------------------------------------------------------------ Add host (admins)
// A form makes a one-time join code (15 min) and the one line to run on the machine; the panel then pairs by
// itself and the dialog follows it: waiting -> paired -> online.
const enr = { id: null, timer: null, page: null };
const OPT_LABEL = [["power_control", "Power control", "shut down / reboot from the panel"], ["self_update", "Self-update", "Update from the Updates page"],
  ["wol_relay", "Wake-on-LAN relay", "always-on machine that wakes others"], ["ups", "Local UPS (NUT)", "a UPS plugged into this machine by USB"],
  ["coretemp", "Core Temp", "Windows: CPU temperatures from Core Temp"], ["ipmi", "IPMI", "Linux: BMC sensors and fans"]];
function closeModal() {
  if (stepUpOpen) { cancelStepUp(); return; }  // "Confirm it's you": closing it any way cancels the kept action
  const m = $("#modal"); m.hidden = true; m.innerHTML = ""; clearTimeout(enr.timer); enr.id = null;
}
// A dialog belongs to the address it was built on (or built anew on: "… removed" after the page went to the
// Overview). When the address changes under it (the browser's Back or Forward, a link), route() closes it the
// way Cancel does: "Shut down ALPHA?" does not stay open, and usable, over another page.
let modalAt = null;
new MutationObserver(() => { modalAt = location.hash; }).observe($("#modal"), { childList: true });
// What was typed into Add host, kept in this tab (sessionStorage) while "Confirm it's you" is on its way: the
// page may leave for Pocket ID in between. Names, an address, options and MAC addresses; no code and no token.
const AH_KEY = "fairy-addhost-form";
function keepAddHost(body) { try { sessionStorage.setItem(AH_KEY, JSON.stringify({ ts: Date.now(), body })); } catch { /* storage blocked: the form is typed again */ } }
function keptAddHost() {
  let k = null;
  try { k = JSON.parse(sessionStorage.getItem(AH_KEY) || "null"); sessionStorage.removeItem(AH_KEY); } catch { k = null; }
  const b = k?.body;
  return b !== null && typeof b === "object" && !Array.isArray(b) && Date.now() - k.ts <= 10 * 60 * 1000 ? b : null;
}
// typed, problem: the dialog as it was left, with the panel's sentence about it (see showPendingResult)
async function openAddHost(typed = null, problem = "") {
  let d;
  if (!typed) keptAddHost();  // a new dialog: an older form is not brought back later
  try { d = await api("/api/admin/hosts"); } catch (e) { flash(problem ? `Add host: failed · ${problem}` : e.message, true); return; }
  enr.page = d;
  const m = $("#modal");
  m.innerHTML = `<form class="modal-box wide" role="dialog" aria-modal="true" aria-labelledby="ah-t" id="ah-form">
    <h3 id="ah-t">Add host</h3>
    <p class="info-keep">Fairy makes a one-time code and one command to run on the machine. ${d.outside_tailscale === true ? "The panel must be able to reach the machine by its name or address." : "The machine must be in your Tailscale network."}${infoBtn("addhost", "Add host", infoList([
      "Run the command on the machine; the panel then pairs with it by itself.",
      ...(d.agent_release ? [`It installs Fairy-agent release <code>${esc(d.agent_release)}</code>.`,
        "The panel checks its install script against the release's signed SHA256SUMS before it shows the command."] : [])]))}</p>
    <div class="form-grid">
      <label class="mfield"><span>${d.outside_tailscale === true ? "Name" : "Name in Tailscale"} <b>*</b></span><input class="input" id="ah-name" required maxlength="63" autocomplete="off" spellcheck="false" placeholder="e.g. agent-1"></label>
      <label class="mfield"><span>System</span><select class="select" id="ah-os"><option value="windows">Windows</option><option value="linux">Linux</option></select></label>
      <label class="mfield"><span>Site</span><select class="select" id="ah-site"><option value="">No site (admins only)</option>${d.sites.map((x) => `<option>${esc(x)}</option>`).join("")}</select></label>
      <label class="mfield"><span>Is it a Proxmox node?</span><select class="select" id="ah-node"><option value="">No</option><option value="__new__" title="The command makes the panel a read-only Proxmox API token on the node (standalone nodes)">Yes: a new node</option>${d.nodes.map((n) => `<option value="${esc(n.name)}">Yes: ${esc(n.name)}${n.cluster ? ` (${esc(n.cluster)})` : ""}, already in the panel</option>`).join("")}</select></label>
      <label class="mfield full"><span>Address <span class="note">optional: only if the panel cannot reach the name (${d.outside_tailscale === true ? "its IP address" : "Tailscale IP"} or full name)</span></span><input class="input" id="ah-addr" maxlength="253" autocomplete="off" spellcheck="false" placeholder="100.x.y.z"></label>
    </div>
    <div class="opts" role="group" aria-label="Options">${OPT_LABEL.map(([k, l, n]) => `<label class="check opt"><input type="checkbox" data-opt="${k}"><span>${esc(l)}<span class="note">${esc(n)}</span></span></label>`).join("")}
      <label class="mfield full" id="ah-ups-row" hidden><span>UPS name in NUT <span class="note">optional</span></span><input class="input" id="ah-ups" maxlength="64" autocomplete="off" placeholder="ups"></label></div>
    <div class="form-grid">
      <label class="mfield"><span>Wake-on-LAN MAC(s) <span class="note">optional</span></span><input class="input" id="ah-macs" autocomplete="off" spellcheck="false" placeholder="aa:bb:cc:dd:ee:ff"></label>
      <label class="mfield"><span>Woken by</span><select class="select" id="ah-relay"><option value="">–</option>${d.relays.map((r) => `<option value="${esc(r.id)}">${esc(r.name)}</option>`).join("")}</select></label>
      <label class="mfield"><span title="An agent with bmc_relay (Fairy-agent v0.4.2) that can reach this machine's BMC">Or powered on via BMC by <span class="note">optional</span></span><select class="select" id="ah-bmc-relay"><option value="">–</option>${(d.bmc_relays || []).map((r) => `<option value="${esc(r.id)}">${esc(r.name)}</option>`).join("")}</select></label>
      <label class="mfield"><span>BMC target id <span class="note">its bmc_relay id</span></span><input class="input" id="ah-bmc-target" maxlength="40" autocomplete="off" spellcheck="false" placeholder="server-bmc"></label>
    </div>
    ${d.panel_address ? `<p class="note" id="ah-panel">Panel address: <code>${esc(d.panel_address)}</code> (${esc(d.panel_address_source || "config")})${d.panel_address_via ? ` <span title="how the panel found its own Tailscale address">· ${esc(d.panel_address_via)}</span>` : ""}${d.panel_address.includes(":") ? ' <b>IPv6 only: agents need the panel\'s Tailscale IPv4; set <code>enroll: {panel_address: 100.x.y.z}</code></b>' : ""}</p>`
      : '<div class="warnbox">The panel does not know its own Tailscale address (no Tailscale interface or tailscaled status found): set <code>enroll: {panel_address: 100.x.y.z}</code> in config.yaml.</div>'}
    <div class="error" id="ah-err" role="alert"></div>
    <div class="row-gap end"><button class="btn" type="button" id="ah-cancel">Cancel</button><button class="btn primary" type="submit" id="ah-go">Make the code</button></div></form>`;
  m.hidden = false;
  const os = $("#ah-os");
  const syncOs = () => {
    const win = os.value === "windows";
    m.querySelector('[data-opt="coretemp"]').disabled = !win;
    m.querySelector('[data-opt="ipmi"]').disabled = win;
    if (win) m.querySelector('[data-opt="ipmi"]').checked = false; else m.querySelector('[data-opt="coretemp"]').checked = false;
  };
  os.onchange = syncOs; syncOs();
  m.querySelector('[data-opt="ups"]').onchange = (e) => ($("#ah-ups-row").hidden = !e.target.checked);
  $("#ah-node").onchange = (e) => { if (e.target.value) os.value = "linux"; syncOs(); };
  $("#ah-cancel").onclick = closeModal;
  if (typed) {  // values go in as properties, and a choice only if the list still has it
    const text = (v) => (typeof v === "string" ? v : "");
    const pick = (sel, v) => { if ([...sel.options].some((o) => o.value === text(v))) sel.value = text(v); };
    $("#ah-name").value = text(typed.name); $("#ah-addr").value = text(typed.address);
    pick(os, typed.os); pick($("#ah-site"), typed.site); pick($("#ah-node"), typed.proxmox_new === true ? "__new__" : typed.node);
    m.querySelectorAll("[data-opt]").forEach((x) => (x.checked = typed.options?.[x.dataset.opt] === true));
    $("#ah-ups").value = text(typed.options?.ups_name); $("#ah-ups-row").hidden = typed.options?.ups !== true;
    $("#ah-macs").value = Array.isArray(typed.wol_macs) ? typed.wol_macs.filter((x) => typeof x === "string").join(" ") : "";
    pick($("#ah-relay"), typed.wol_relay); pick($("#ah-bmc-relay"), typed.bmc_relay); $("#ah-bmc-target").value = text(typed.bmc_target);
    syncOs();
  }
  $("#ah-err").textContent = problem;
  setTimeout(() => $("#ah-name").focus(), 0);
  $("#ah-form").onsubmit = async (e) => {
    e.preventDefault();
    const opts = {};
    m.querySelectorAll("[data-opt]").forEach((x) => (opts[x.dataset.opt] = x.checked && !x.disabled));
    if (opts.ups) opts.ups_name = $("#ah-ups").value.trim();
    const body = { name: $("#ah-name").value.trim(), address: $("#ah-addr").value.trim(), os: os.value, site: $("#ah-site").value,
      node: $("#ah-node").value === "__new__" ? "" : $("#ah-node").value, proxmox_new: $("#ah-node").value === "__new__", options: opts, wol_macs: $("#ah-macs").value.split(/[\s,;]+/).filter(Boolean), wol_relay: $("#ah-relay").value,
      bmc_relay: $("#ah-bmc-relay").value, bmc_target: $("#ah-bmc-target").value.trim() };
    $("#ah-go").disabled = true;
    try {
      const r = await controlCall({ kind: "request", label: `Add host ${body.name}` }, () => api("/api/admin/hosts", { method: "POST", body: JSON.stringify(body) }));
      if (r) openEnrollDialog(r); else keepAddHost(body);  // "Confirm it's you" took the dialog's place: see showPendingResult
    } catch (x) { $("#ah-err").textContent = x.message; $("#ah-go").disabled = false; }
  };
}
const groupCode = (c) => String(c || "");  // the panel sends it grouped (ABCD-EFGH-...)
const ENR_STEPS = [["waiting", "Waiting for the machine"], ["paired", "Paired"], ["online", "Online"]];
function enrollSteps(e) {
  const bad = ["failed", "expired", "cancelled"].includes(e.state);
  const at = ENR_STEPS.findIndex(([k]) => k === e.state);
  return `<div class="steps">${ENR_STEPS.map(([k, l], i) => `<span class="${bad && i === Math.max(0, at) ? "bad" : at > i || e.state === "online" ? "done" : at === i ? "now" : ""}">${esc(l)}</span>`).join("")}
    ${bad ? `<span class="bad">${esc(e.state)}</span>` : ""}</div>`;
}
// What answered at the machine's address is not the agent the code was given to: said in a box of its own,
// above the progress text, because the enrollment itself keeps "waiting" (the real agent can still pair).
const ENR_HINT_BOX = table({ unverified_answer: "Not a Fairy agent: something answers at this address, but it could not prove it has the join code. It was not added." });
const enrollHint = (e) => (ENR_HINT_BOX[e.hint] ? `<div class="warnbox" id="en-hint" role="alert">${esc(ENR_HINT_BOX[e.hint])}</div>` : "");
function openEnrollDialog(cr) {
  const m = $("#modal");
  enr.id = cr.id;
  const win = cr.os === "windows";
  m.innerHTML = `<div class="modal-box wide" role="dialog" aria-modal="true" aria-labelledby="en-t">
    <h3 id="en-t">${cr.repair ? "Re-pair" : "Add"} ${esc(cr.name)}</h3>
    <p>On <b>${esc(cr.name)}</b>, ${win ? "open <b>PowerShell as Administrator</b>" : cr.node ? "open a <b>root shell</b> (the node's Shell in Proxmox)" : "open a terminal"} and run:</p>
    <div class="cmd-box" id="en-cmd">${esc(cr.command)}</div>
    <div class="row-gap"><button class="btn" type="button" id="en-copy">${I.copy}Copy command</button>
      <span class="note">Join code <span class="code-big">${esc(groupCode(cr.code))}</span> · single use · valid until ${new Date(cr.expires_at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</span></div>
    <p class="note info-keep">It installs Fairy-agent ${cr.release?.tag ? `release <code>${esc(cr.release.tag)}</code>${cr.release.version ? ` (${esc(cr.release.version)})` : ""}` : "(signed release)"}, writes the agent's settings and waits for the panel. Nothing else on the machine changes. The code is shown only now.${infoBtn("enroll", "The command", infoList([
      "It runs the install script only if it is exactly the one the panel checked against the signed SHA256SUMS.",
      "The script checks every file it installs."]))}</p>
    ${cr.warning ? `<div class="note warn" id="en-warn">${esc(cr.warning)}</div>` : ""}
    <div id="en-progress" aria-live="polite">${enrollSteps(cr)}${enrollHint(cr)}<div class="note" id="en-msg">${esc(cr.message)}</div></div>
    <div class="row-gap end"><button class="btn" type="button" id="en-cancel">Cancel Add host</button><button class="btn primary" type="button" id="en-close">Close</button></div></div>`;
  m.hidden = false;
  $("#en-copy").onclick = async () => {
    try { await navigator.clipboard.writeText(cr.command); flash("Command copied"); }
    catch { const r = document.createRange(); r.selectNodeContents($("#en-cmd")); getSelection().removeAllRanges(); getSelection().addRange(r); flash("Select and copy the command (Ctrl+C)"); }
  };
  $("#en-close").onclick = closeModal;
  $("#en-cancel").onclick = async () => {
    const path = `/api/admin/enrollments/${enc(cr.id)}`;
    try {
      const r = await controlCall({ kind: "request", label: `Cancel Add host ${cr.name}`, method: "DELETE", path, body: null }, () => api(path, { method: "DELETE" }));
      if (r === null) return;  // "Confirm it's you" took the dialog's place: the cancel runs after the sign-in
      flash(`Add host ${cr.name}: cancelled`);
    } catch (x) { flash(x.message, true); }
    closeModal();
  };
  pollEnroll(cr.id);
}
async function pollEnroll(id) {
  clearTimeout(enr.timer);
  if (enr.id !== id) return;
  let e;
  try { e = await api(`/api/admin/enrollments/${enc(id)}`); } catch (x) { const msg = $("#en-msg"); if (msg) msg.textContent = x.message; return; }
  if (enr.id !== id || !$("#en-progress")) return;
  $("#en-progress").innerHTML = `${enrollSteps(e)}${enrollHint(e)}<div class="note" id="en-msg">${esc(e.message)}${e.state === "online" && state.byId[e.host_id]
    ? ` · <a href="#/system/${enc(e.host_id)}">open ${esc(e.name)} →</a>` : ""}</div>`;
  if (e.done) { $("#en-cancel").hidden = true; if (e.state === "online") loadManaged(); return; }
  if (!signedOut) enr.timer = setTimeout(() => pollEnroll(id), 2000);
}
// After a step-up sign-in the panel made the code itself: fetch it once and show it.
async function revealEnrollment(id) {
  try { openEnrollDialog(await api(`/api/admin/enrollments/${enc(id)}?reveal=1`)); } catch (e) { flash(e.message, true); }
}
// Hosts added with Add host (admins): Re-pair and Remove on their page.
async function loadManaged() {
  if (!state.me?.can_manage_users) return;
  try {
    const d = await api("/api/admin/hosts");
    state.managed = dict((d.managed || []).map((h) => [h.system_id, h]));
  } catch { /* keep */ }
  render();
}
function managedBox(s) {
  const h = state.managed[s.id];
  if (!h || !state.me?.can_manage_users) return "";
  const why = controlWhyNot();
  return `<div class="panel glass pad"><h3>Added with Add host${infoBtn("managed", "Added with Add host", infoList([
      "Remove from the panel stops polling the host and deletes the panel's tokens for it.",
      "Re-pair makes a new join code: for when the panel lost this host's tokens."]))}</h3>
    <div class="list-row"><span>Added</span><span>${h.added_at ? new Date(h.added_at).toLocaleDateString() : "–"}${h.added_by ? ` · by ${esc(h.added_by)}` : ""}</span></div>
    <div class="list-row"><span>Address</span><span><code>${esc(h.url)}</code></span></div>
    <div class="list-row"><span>Pairing</span><span>${h.pairing === "v2" ? "verified: the agent proved it has the join code" : '<span class="note">made by an older panel, not verified: Re-pair to replace it with a verified pairing</span>'}</span></div>
    ${h.proxmox ? `<div class="list-row"><span>Proxmox API</span><span><code>${esc(h.proxmox.url)}</code> · read-only token <code>${esc(h.proxmox.token_id)}</code></span></div>`
      : h.proxmox_wanted ? '<div class="list-row"><span>Proxmox API</span><span class="note">no token came with the pairing (a cluster, or the command could not make one)</span></div>' : ""}
    <div class="row-gap"><button class="btn" type="button" data-act="mh-repair" ${why ? "disabled" : ""} title="A new join code, when the panel lost this host's tokens">Re-pair</button>
      <button class="btn danger" type="button" data-act="mh-remove" ${why ? "disabled" : ""}>Remove from the panel</button></div>
    <div class="note info-keep">After Remove, the agent keeps running on the machine until you uninstall it there.</div></div>`;
}
// Re-pair / Remove: type the host's name (checked by the panel too), then the usual step-up.
function typedDialog(title, text, name, go, fn, goClass = "danger-solid") {
  const m = $("#modal");
  m.innerHTML = `<div class="modal-box" role="dialog" aria-modal="true" aria-labelledby="td-t"><h3 id="td-t">${esc(title)}</h3>${text.startsWith("<") ? text : `<p>${text}</p>`}
    <label class="mfield"><span>Type <b>${esc(name)}</b> to confirm</span><input class="input" id="td-in" autocomplete="off" spellcheck="false" placeholder="${esc(name)}"></label>
    <div class="error" id="td-err" role="alert"></div>
    <div class="row-gap end"><button class="btn" type="button" id="td-cancel">Cancel</button><button class="btn ${goClass}" type="button" id="td-go" disabled>${esc(go)}</button></div></div>`;
  m.hidden = false;
  const input = $("#td-in"), b = $("#td-go");
  input.oninput = () => (b.disabled = input.value.trim().toLowerCase() !== name.toLowerCase());
  setTimeout(() => input.focus(), 0);
  $("#td-cancel").onclick = closeModal;
  b.onclick = async () => {
    b.disabled = true;
    try { await fn(input.value); } catch (x) { $("#td-err").textContent = x.message; b.disabled = false; }
  };
}
document.addEventListener("click", (e) => {
  const t = e.target.closest('[data-act="add-host"],[data-act="mh-repair"],[data-act="mh-remove"]');
  if (!t || t.disabled) return;
  if (t.dataset.act === "add-host") { openAddHost(); return; }
  const s = state.byId[current().id], h = s && state.managed[s.id];
  if (!h) return;
  const path = `/api/admin/hosts/${enc(h.id)}`;
  if (t.dataset.act === "mh-repair") {
    // A host that is its own Proxmox node must answer with the node name it was added as, unless the admin says it was renamed.
    const renamedBox = h.proxmox ? `<label class="note"><input type="checkbox" id="td-renamed"> The node was renamed: accept another Proxmox node name than <b>${esc(h.proxmox.node)}</b></label>` : "";
    typedDialog(`Re-pair ${h.name}?`, `<p>Makes a new join code. Run the new command on the machine; the old tokens stop working once it pairs.</p>${renamedBox}`, h.name, "Make a new code", async (v) => {
      const renamed = !!$("#td-renamed")?.checked;
      const r = await controlCall({ kind: "request", label: `Re-pair ${h.name}${renamed ? " (node renamed)" : ""}` }, () => api(`${path}/repair`, { method: "POST", body: JSON.stringify(renamed ? { confirm: v, node_renamed: true } : { confirm: v }) }));
      if (r) openEnrollDialog(r);
    });
  } else {
    typedDialog(`Remove ${h.name} from the panel?`, `The panel stops polling it and deletes its tokens${h.proxmox ? " (and its Proxmox node)" : ""}. The agent keeps running on the machine until you uninstall it there.`, h.name, "Remove", async (v) => {
      const r = await controlCall({ kind: "request", label: `Remove ${h.name}` }, () => api(path, { method: "DELETE", body: JSON.stringify({ confirm: v }) }));
      if (!r) return;
      location.hash = "#/"; loadManaged();
      if (!h.proxmox) { closeModal(); flash(r.message || `${h.name} removed`); return; }
      const m = $("#modal"); // the node's token stays until deleted there: show the one command to do it
      m.innerHTML = `<div class="modal-box" role="dialog" aria-modal="true" aria-labelledby="rm-t"><h3 id="rm-t">${esc(h.name)} removed</h3>
        <p>Its read-only Proxmox token still exists on the node. To delete it, run this there (as root):</p>
        <div class="cmd-box">pveum user token remove fairy@pve ${esc(h.proxmox.token_id.split("!")[1] || "")}</div>
        <div class="row-gap end"><button class="btn primary" type="button" id="rm-ok">Done</button></div></div>`;
      m.hidden = false;
      $("#rm-ok").onclick = closeModal;
    });
  }
});

// ------------------------------------------------------------ Set model name (admins, the ⋯ menu of a host's page)
// A name of the panel's own for a host's model, shown in place of the one the machine reports (model()). The
// dialog is a title, the field and three buttons, nothing to read: the field's placeholder is the reported
// model, and what Clear does is its tooltip. The panel checks the name (1 to 64 characters, one line, no
// invisible characters) and says what is wrong; the dialog shows that sentence. The host's name and both model
// names are put into the dialog as properties (value, placeholder, ariaLabel, title), never as markup.
function modelNameMenu(s) {
  if (!isHost(s) || !state.me?.can_manage_users) return "";
  return `<div class="menu-sep"></div><button class="menu-item" type="button" data-act="model-name">Set model name</button>`;
}
function openModelName(s) {
  const reported = s.hardware?.display || "", custom = s.custom_model || "";
  state.menu = null; render();
  const m = $("#modal");
  m.innerHTML = `<form class="modal-box" role="dialog" aria-modal="true" aria-labelledby="mn-t" id="mn-form" novalidate>
    <h3 id="mn-t">Set model name</h3>
    <div class="mfield"><input class="input" id="mn-in" maxlength="64" autocomplete="off" spellcheck="false" aria-describedby="mn-err"></div>
    <div class="error" id="mn-err" role="alert"></div>
    <div class="row-gap end"><button class="btn" type="button" id="mn-clear">Clear</button><button class="btn" type="button" id="mn-cancel">Cancel</button><button class="btn primary" type="submit" id="mn-save">Save</button></div></form>`;
  const input = $("#mn-in"), err = $("#mn-err"), clear = $("#mn-clear");
  input.ariaLabel = `Model name for ${s.name}`;
  input.value = custom; input.placeholder = reported;
  clear.title = reported ? `Show the reported model again: ${reported}` : "Show no model: the host reports none";
  clear.disabled = !custom;
  input.oninput = () => { err.textContent = ""; };  // the panel's sentence was about the name before this change
  m.hidden = false;
  setTimeout(() => { input.focus(); input.select(); }, 0);
  const close = () => { closeModal(); document.querySelector('[data-act="menu"][data-menu="poll"]')?.focus(); };
  $("#mn-cancel").onclick = close;
  const send = async (name) => {
    const btns = [...m.querySelectorAll("button")];
    btns.forEach((b) => (b.disabled = true)); err.textContent = "";
    try {
      const r = await api(`/api/systems/${enc(s.id)}/custom-model`, { method: "PUT", body: JSON.stringify({ custom_model: name }) });
      const cur = state.byId[s.id];
      if (cur) cur.custom_model = r.custom_model;  // shown at once; the stream brings the same with the next snapshot
      close(); flash(r.message); render();
    } catch (x) {
      if (!err.isConnected) return;  // the dialog is gone (the session ended)
      err.textContent = x.message; btns.forEach((b) => (b.disabled = false)); clear.disabled = !custom; input.focus();
    }
  };
  clear.onclick = () => send("");
  $("#mn-form").onsubmit = (e) => { e.preventDefault(); send(input.value); };
}
document.addEventListener("click", (e) => {
  const t = e.target.closest('[data-act="model-name"]');
  const s = t && !t.disabled && state.byId[current().id];
  if (s) openModelName(s);
});

// ------------------------------------------------------------ elevated helper Stop / Start
// Same prompt as a power action: the host's name typed, then the step-up sign-in if needed; audited by the panel.
function openHelperDialog(s, action) {
  const f = s.helper?.features || [], stop = action === "stop";
  state.menu = null; render();
  const has = (x) => f.includes(x);
  const readings = [has("coretemp") && "Core Temp", has("ipmi") && "BMC sensor"].filter(Boolean);
  const waits = [has("power") && "Shut down / reboot", has("update") && "agent updates"].filter(Boolean);
  const lines = stop ? [
    has("fans") && "Fans go back to the BMC's own automatic control; the agent's temperature watchdog stops with the helper.",
    readings.length && `${andList(readings)} readings pause until the helper is started again.`,
    waits.length && `${andList(waits)} from the panel wait for it as well.`,
    "CPU, memory, disks, GPUs and network keep reporting: the agent itself keeps running."]
    : [readings.length && `${andList(readings)} readings come back within a few seconds.`,
      has("fans") && "Fan control from the panel works again, with the agent's temperature watchdog.",
      "The helper is the agent's local service with administrator rights; it has no network access of its own."];
  typedDialog(`${stop ? "Stop" : "Start"} the elevated helper on ${s.name}?`,
    `<ul class="dlg-list">${lines.filter(Boolean).map((l) => `<li>${esc(l)}</li>`).join("")}</ul>`, s.name, stop ? "Stop helper" : "Start helper",
    async (v) => {
      const path = `/api/systems/${enc(s.id)}/helper/${action}`, body = { confirm: v };
      const r = await controlCall({ kind: "request", label: `${stop ? "Stop" : "Start"} elevated helper on ${s.name}`, system: s.id, method: "POST", path, body },
        () => api(path, { method: "POST", body: JSON.stringify(body) }));
      if (!r) return;  // the step-up sign-in took over
      closeModal(); flash(`${s.name}: ${r.message}`);
      const cur = state.byId[s.id];
      if (cur?.helper) { cur.helper.state = r.state; cur.helper.since = new Date().toISOString(); }
      render();
    }, stop ? "danger-solid" : "go");
}
document.addEventListener("click", (e) => {
  const t = e.target.closest('[data-act="helper"]');
  const s = t && !t.disabled && state.byId[current().id];
  if (s?.helper) openHelperDialog(s, t.dataset.action);
});

// ------------------------------------------------------------ Update panel (admins, Updates page)
// The panel asks systemd to start fairy-panel-update.service (nothing from the request reaches it); the unit
// writes its steps to a status file that survives the panel's restart. A sign-in from the last 2 minutes is needed.
const pupd = { data: null, error: null, startedFrom: null, restarting: false };
const PANEL_STEPS = [["downloading", "Download"], ["staged", "Verified"], ["restarting", "Restart"], ["done", "Running"]];
async function fetchPanelUpdate() {
  if (!state.me?.can_control) return;
  try {
    const d = await api("/api/admin/panel-update");
    if (pupd.startedFrom && d.version !== pupd.startedFrom && d.status?.finished) {
      flash(d.status.state === "done" ? `The panel is back, now on ${buildLabel(d.version)}` : `Panel update: ${d.status.state.replace("_", " ")} · ${d.status.message}`, d.status.state !== "done");
      pupd.startedFrom = null;
    } else if (pupd.startedFrom && d.status?.finished && d.status.state !== "done") {
      flash(`Panel update: ${d.status.state.replace("_", " ")} · ${d.status.message}`, true); pupd.startedFrom = null;
    }
    pupd.data = d; pupd.error = null; pupd.restarting = false;
  } catch (e) {
    if (pupd.startedFrom || (pupd.data?.status && !pupd.data.status.finished)) pupd.restarting = true;  // the panel is restarting
    else pupd.error = e.message;
  }
}
function panelUpdateBox(d) {
  const p = pupd.data, why = controlWhyNot();
  const st = p?.status, running = (st && !st.finished) || pupd.restarting;
  const to = d.panel_latest?.version ? buildLabel(d.panel_latest.version) : d.panel_latest?.tag || "the latest release";
  const canGo = p?.ready && p?.behind === true && !running && !why;
  const title = why || (!p ? "loading…" : !p.ready ? p.why : p.behind !== true ? "No newer signed panel release" : running ? "An update is running" : `Install ${to} on the panel's host`);
  const at = st ? PANEL_STEPS.findIndex(([k]) => k === st.state) : -1;
  const bad = st && ["failed", "rolled_back", "stopped"].includes(st.state);  // stopped: the run did not end by itself
  const steps = st && (running || Date.now() - new Date(st.updated_at) < 3600e3) ? `<div class="steps">${PANEL_STEPS.map(([k, l], i) =>
    `<span class="${st.state === "done" || at > i ? "done" : at === i ? (st.finished ? "done" : "now") : ""}">${l}</span>`).join("")}${bad ? `<span class="bad">${esc(st.state.replace("_", " "))}</span>` : ""}</div>
    <div class="note">${pupd.restarting ? "The panel is restarting… this page reconnects by itself." : esc(st.message)}${st.started_at ? ` · started <span data-ago="${esc(st.started_at)}">${ago(st.started_at)}</span>` : ""}</div>` : "";
  return `<div class="row-gap"><span>Panel: fairy <b>${esc(buildLabel(d.panel_version))}</b>${d.panel_latest?.tag ? ` · latest ${esc(d.panel_latest.version ? buildLabel(d.panel_latest.version) : d.panel_latest.tag)}` : ""}
      ${d.panel_behind === false ? ' <span class="chip ok">up to date</span>' : ""}${d.panel_latest?.url ? ` <a href="${safeHref(d.panel_latest.url)}" target="_blank" rel="noopener noreferrer">Release notes ${I.ext}</a>` : ""}</span>
    <button class="btn push" type="button" data-act="panel-upd" ${canGo ? "" : "disabled"} title="${esc(title)}">${I.update} ${running ? "Updating…" : "Update panel"}</button></div>
    ${steps}
    ${p && !p.ready ? `<div class="note warn">${esc(p.why)}</div>` : ""}
    <div class="note">Update panel installs the latest signed release and restarts the panel.${infoBtn("upd-panel", "Update panel", infoList([
      "The release is installed on the panel's host.",
      "The panel restarts. If the new version does not come up, it rolls back.",
      "It never installs an older version.",
      "It needs a Pocket ID sign-in from the last 2 minutes.",
      "By hand: <code>fairy update</code> on the panel's host (as root)."]))}</div>`;
}
document.addEventListener("click", async (e) => {
  const t = e.target.closest('[data-act="panel-upd"]');
  if (!t || t.disabled) return;
  const d = upd.data, to = d?.panel_latest?.version ? buildLabel(d.panel_latest.version) : d?.panel_latest?.tag || "the latest release";
  if (!confirm(`Update the panel to ${to}? It restarts (about a minute without the panel) and rolls back by itself if the new version does not come up.`)) return;
  t.disabled = true;
  try {
    const from = pupd.data?.version;
    const r = await controlCall({ kind: "request", label: "Update panel" }, () => api("/api/admin/panel-update", { method: "POST", body: "{}" }));
    if (r) { pupd.startedFrom = from; flash(r.message || "Panel update started"); }
  } catch (x) { flash(x.message, true); }
  await fetchPanelUpdate(); render();
});

// ------------------------------------------------------------ routing + rendering
function schedule(fn, ms = 3000) { clearTimeout(state.timer); if (!signedOut) state.timer = setTimeout(fn, ms); }
// A part of the address after "#". A broken %-escape (#/system/%E0%A4%A) is not an error of the page: the part
// is taken as it stands, which is an id no system has, and the page says "Unknown system."
const hashPart = (v) => { try { return decodeURIComponent(v); } catch { return v; } };
// What the stream takes as the id of the host to watch (stream.go, watchRe). An address with anything else after
// #/system/ names no system: the page says so and keeps its live stream, instead of asking for a stream the
// panel refuses (422) again and again.
const WATCHABLE = /^[a-z0-9][a-z0-9-]{0,79}$/;
function current() {
  const h = location.hash || "#/";
  const m = h.match(/^#\/system\/([^/]+)(?:\/(\w+))?$/);
  if (m) return { page: "system", id: hashPart(m[1]), tab: m[2] || "overview" };
  if (h.startsWith("#/power")) return { page: "power" };
  if (h.startsWith("#/ups")) return { page: "ups" };
  if (h.startsWith("#/settings")) return { page: "settings" };
  if (h.startsWith("#/sessions")) return { page: "sessions" };
  if (h.startsWith("#/audit")) return { page: "audit" };
  if (h.startsWith("#/users")) return { page: "users" };
  if (h.startsWith("#/updates")) return { page: "updates" };
  if (h.startsWith("#/notifications")) return { page: "notifications" };
  return { page: "overview" };
}
const routeKey = (r) => `${r.page}:${r.id || ""}:${r.tab || ""}`;
function pageHtml(r) {
  return r.page === "system" ? pageSystem(r.id, r.tab) : r.page === "power" ? pagePower() : r.page === "ups" ? pageUps()
    : r.page === "settings" ? pageSettings() : r.page === "audit" ? pageAudit() : r.page === "users" ? pageUsers()
    : r.page === "updates" ? pageUpdates() : r.page === "notifications" ? pageNotifications()
    : r.page === "sessions" ? pageSessions() : pageOverview();
}
let renderQueued = false;
function render() {  // coalesced to one DOM patch per frame
  if (renderQueued) return;
  renderQueued = true;
  requestAnimationFrame(() => { renderQueued = false; renderNow(); });
}
function renderNow() {
  const r = current();
  renderNav(); renderAlerts();
  if (r.page === "settings" || r.page === "users") return;  // live inputs; rendered on route / after changes
  morph($("#page"), `<div data-key="${esc(routeKey(r))}">${pageHtml(r)}</div>`);
}
function route() {
  clearTimeout(state.timer);
  const pend = readPending();
  if (pend && !onPendingPage(pend)) dropPending();  // navigated away: the offer is gone
  closeUpdPop(false);
  if (!$("#modal").hidden && modalAt !== location.hash) closeModal();  // never confirmed: closed as Cancel closes it
  state.menu = null;
  const r = current();
  if (r.page === "system") {
    const s = state.byId[r.id];
    if (hasPowerOnButton(s)) refreshCaps(r.id);
  }
  $("#page").innerHTML = `<div data-key="${esc(routeKey(r))}">${pageHtml(r)}</div>`;
  if (r.page === "settings") bindSettings();
  if (r.page === "system" && r.tab === "processes") loadProcesses(r.id);
  if (r.page === "system" && r.tab === "fans") loadFans(r.id);
  if (r.page === "ups") loadUpsEvents();
  if (r.page === "audit") loadAudit(true);
  if (r.page === "users") loadAdmin();
  if (r.page === "updates") loadUpdates();
  if (r.page === "notifications") loadNotes(true);
  if (r.page === "sessions") loadSessions();
  if (r.page === "system" && state.me?.can_manage_users) loadManaged();
  updateBell();
  renderNav(); renderAlerts(); closeAcct(false); syncAcct();
  $("#shell").classList.remove("drawer"); $("#scrim").hidden = true;
  connect(r.page === "system" && WATCHABLE.test(r.id) ? r.id : null);  // boost the viewed host to 1 s polling
  window.scrollTo(0, 0);
}

// ------------------------------------------------------------ live data
function indexSystems() { state.byId = dict(state.systems.map((s) => [s.id, s])); }
function pushUpsHistory(ov) {
  for (const u of ov.ups) {
    const h = (state.upsHist[u.id] ||= []);
    h.push({ t: ov.generated_at, watts: u.watts, charge: u.battery_charge });
    if (h.length > 720) h.shift();
  }
}
// When the stream is refused or lost for good (the browser gave up: readyState CLOSED), ask /api/me first: a 401
// means the session ended (api() shows the sign-in view and nothing reconnects); anything else is the network
// or the panel restarting, retried after 3 s, then 6, 12 ... up to 30 s. A stream the server merely ended is
// reconnected by the browser itself; if that reconnect is answered 401 it ends up here.
let streamRetry = null, streamBackoff = 0;
async function streamLost() {
  if (leaving || stepUpOpen?.going) return;  // the page is on its way out: see leaving
  let reachable = false;
  // should this page be handed the result of an action meant for another tab, it is shown here, not lost
  try { showPendingResult((await api("/api/me")).pending_result); reachable = true; } catch { /* 401: sessionEnded() ran; otherwise retry below */ }
  if (signedOut) return;
  streamBackoff = Math.min(30000, streamBackoff ? streamBackoff * 2 : 3000);
  // signed in, but the stream is still refused: most likely the limit of 8 live connections per user (L8)
  if (reachable && streamBackoff >= 12000) flash("Live updates are not connecting. Too many Fairy tabs open? (at most 8 per user) Retrying…", true);
  clearTimeout(streamRetry);
  streamRetry = setTimeout(() => connect(state.watch), streamBackoff);
}
function connect(watch) {
  if (signedOut) return;
  if (state.es && state.watch === watch) return;
  if (state.es) state.es.close();
  state.watch = watch;
  // notes_after: a new stream (another host's page) goes on after the last notification this page was sent
  const q = [watch ? `watch=${enc(watch)}` : "", pop.after !== null ? `notes_after=${enc(pop.after)}` : ""].filter(Boolean).join("&");
  const es = new EventSource(`/api/stream${q ? `?${q}` : ""}`);
  state.es = es;
  for (const ev of ["open", "systems", "systems_delta", "summary", "ups", "notes", "notes_new", "ups_event", "power"]) es.addEventListener(ev, () => netHeard(ev !== "open"));
  es.addEventListener("open", () => { streamBackoff = 0; if (state.streamed) checkAccess(); state.streamed = true; });
  es.addEventListener("systems", (e) => { state.systems = JSON.parse(e.data).systems; indexSystems(); loadSites(); render(); ensurePowerOnCaps(); });
  es.addEventListener("systems_delta", (e) => {
    const d = JSON.parse(e.data), gone = new Set(d.removed);
    d.changed.forEach((s) => (state.byId[s.id] = s));
    gone.forEach((id) => delete state.byId[id]);
    // keep server order for existing ids; new ids go at the end until the next full snapshot
    const seen = new Set();
    state.systems = state.systems.filter((s) => !gone.has(s.id)).map((s) => (seen.add(s.id), state.byId[s.id]));
    let added = false;
    d.changed.forEach((s) => { if (!seen.has(s.id)) { state.systems.push(s); added = true; } });
    if (added) loadSites();
    render(); ensurePowerOnCaps();
  });
  es.addEventListener("summary", (e) => { state.summary = JSON.parse(e.data); render(); });
  es.addEventListener("ups", (e) => { state.ups = JSON.parse(e.data); pushUpsHistory(state.ups); render(); });
  es.addEventListener("notes", (e) => onNotesCounts(JSON.parse(e.data)));
  es.addEventListener("notes_new", (e) => onNewNotes(JSON.parse(e.data)));
  es.addEventListener("ups_event", (e) => { state.upsEvents.unshift(JSON.parse(e.data)); render(); });
  es.addEventListener("power", (e) => {
    state.power = JSON.parse(e.data);
    const sources = dict();
    state.power.sources.forEach((s) => (sources[s.id] = s.readings.reduce((a, r) => a + (r.watts || 0), 0) || null));
    state.history.push({ t: state.power.generated_at, total_w: state.power.total_w, sources });
    if (state.history.length > 720) state.history.shift();
    render();
  });
  es.onerror = () => { if (es.readyState === EventSource.CLOSED && state.es === es) { state.es = null; streamLost(); } };
}
async function loadSites() {  // the site of each host, for the sidebar and Overview groups
  try { state.sites = dict(Object.entries((await api("/api/system-sites")).sites || {})); render(); } catch { /* keep the last */ }
}
async function loadUpsEvents() {
  try { state.upsEvents = await api("/api/ups/events?limit=50"); render(); } catch { /* ignore */ }
}
// 1 s ticker for relative times and countdowns (no re-render needed), and for the knock when the panel is quiet
setInterval(() => {
  netKnock();
  document.querySelectorAll("[data-ago]").forEach((el) => { const v = ago(el.dataset.ago); if (el.textContent !== v) el.textContent = v; });
  document.querySelectorAll("[data-countdown]").forEach((el) => { el.textContent = fmtRuntime(Math.max(0, (new Date(el.dataset.countdown) - Date.now()) / 1000)); });
  pendingTick();
}, 1000);

// The stream ends when an admin changes your role or sites; on reconnect, reload if access changed.
async function checkAccess() {
  if (leaving || stepUpOpen?.going) return;  // see leaving
  try {
    const me = await api("/api/me");
    showPendingResult(me.pending_result);  // as in streamLost
    if (me.role !== state.me.role || (me.sites || []).join() !== (state.me.sites || []).join()) location.reload();
  } catch { /* a 401 already showed the sign-in view (api) */ }
}
const demoSwitcher = () => (state.me.demo ? `<label class="demo-view" title="Demo only: see the panel as this role would">
  <span class="note">Preview as</span><select class="select" id="demo-view">${(state.me.demo_views || []).map((v) =>
    `<option value="${esc(v)}" ${v === state.me.demo_view ? "selected" : ""}>${esc(v === "admin" ? "Admin (everything)" : v === "pending" ? "Waiting for access" : `Viewer · ${v.slice(7)}`)}</option>`).join("")}</select></label>` : "");
document.addEventListener("change", async (e) => {
  if (e.target.id !== "demo-view") return;
  try { await api("/api/demo/view", { method: "POST", body: JSON.stringify({ view: e.target.value }) }); location.hash = "#/"; location.reload(); }
  catch (x) { flash(x.message, true); }
});

// ------------------------------------------------------------ account menu (top bar, hamburger)
// The signed-in user, Settings / Sessions / Audit / Users and Sign out. Static DOM in the top bar (not morphed); the
// active page is marked on every route. Keyboard: arrows / Home / End move, Escape closes and refocuses the button.
const demoViewLabel = (v) => (v === "admin" ? "Admin (everything)" : v === "pending" ? "Waiting for access" : `Viewer · ${v.slice(7)}`);
function buildAcct() {
  const me = state.me, who = me.name || me.preferred_username || me.sub;
  const sites = me.all_sites ? "every site" : (me.sites || []).join(", ") || "no sites yet";
  const roleChip = me.role === "admin" ? '<span class="chip ok">admin</span>' : `<span class="chip" title="${esc(sites)}">viewer</span>`;
  const sub = [me.email && me.email !== who ? me.email : "", me.role === "admin" ? "" : `Sees: ${sites}`].filter(Boolean);
  const item = (href, ico, name) => `<a class="menu-item" role="menuitem" tabindex="-1" href="${href}" data-page="${href.slice(2)}"><span class="ico">${ico}</span><span class="name">${esc(name)}</span></a>`;
  let html = `<div class="acct-head" role="none"><span class="avatar">${esc((who[0] || "?").toUpperCase())}</span>
    <span class="acct-who"><span class="acct-name"><b>${esc(who)}</b>${roleChip}</span>${sub.map((x) => `<span class="note">${esc(x)}</span>`).join("")}</span></div>
    <div class="menu-sep" role="separator"></div>
    ${item("#/settings", I.settings, "Settings")}
    ${item("#/sessions", I.sessions, "Sessions")}
    ${me.can_control ? item("#/audit", I.audit, "Audit") : ""}
    ${me.can_manage_users ? item("#/users", I.users, "Users") : ""}
    ${me.can_control ? item("#/updates", I.update, "Updates").replace("</span></a>", '</span><span class="upd-dot" id="upd-badge" hidden></span></a>') : ""}`;
  if (me.demo && (me.demo_views || []).length) {
    html += `<div class="menu-sep" role="separator"></div><div role="group" aria-labelledby="acct-demo-t"><div class="menu-title" id="acct-demo-t" role="none">Preview as · demo</div>
      ${me.demo_views.map((v) => `<button class="menu-item" type="button" role="menuitemradio" tabindex="-1" aria-checked="${v === me.demo_view}" data-demo-view="${esc(v)}">
        <span class="ico">${v === me.demo_view ? I.eye : ""}</span><span class="name">${esc(demoViewLabel(v))}</span></button>`).join("")}</div>`;
  }
  if (!me.demo) {
    html += `<div class="menu-sep" role="separator"></div><form method="post" action="/auth/logout" role="none">
      <button class="menu-item" type="submit" role="menuitem" tabindex="-1"><span class="ico">${I.signout}</span><span class="name">Sign out</span></button></form>`;
  }
  const menu = $("#acct-menu");
  menu.innerHTML = html;
  menu.setAttribute("aria-label", `Signed in as ${who} (${me.role})`);
  syncAcct();
}
function syncAcct() {
  const page = current().page;
  document.querySelectorAll("#acct-menu [data-page]").forEach((a) => {
    const on = a.dataset.page === page;
    a.classList.toggle("on", on);
    if (on) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  });
  $("#acct-btn")?.classList.toggle("on", ["settings", "sessions", "audit", "users", "updates"].includes(page));
}
const acctItems = () => [...document.querySelectorAll('#acct-menu [role^="menuitem"]')];
const acctOpen = () => !$("#acct-menu").hidden;
function openAcct(focus = "first") {
  const menu = $("#acct-menu"), btn = $("#acct-btn");
  menu.hidden = false; btn.setAttribute("aria-expanded", "true");
  const items = acctItems();
  (focus === "last" ? items[items.length - 1] : items[0])?.focus();
}
function closeAcct(refocus = true) {
  if (!acctOpen()) return;
  $("#acct-menu").hidden = true; $("#acct-btn").setAttribute("aria-expanded", "false");
  if (refocus) $("#acct-btn").focus();
}
document.addEventListener("click", async (e) => {
  const btn = e.target.closest("#acct-btn");
  if (btn) { if (acctOpen()) closeAcct(); else openAcct(); return; }
  if (!e.target.closest("#acct")) { closeAcct(false); return; }
  const dv = e.target.closest("[data-demo-view]");
  if (dv) {
    try { await api("/api/demo/view", { method: "POST", body: JSON.stringify({ view: dv.dataset.demoView }) }); location.hash = "#/"; location.reload(); }
    catch (x) { flash(x.message, true); }
    return;
  }
  if (e.target.closest('a[role="menuitem"]')) closeAcct();
});
document.addEventListener("keydown", (e) => {
  const btn = e.target.closest?.("#acct-btn");
  if (btn && (e.key === "ArrowDown" || e.key === "ArrowUp")) { e.preventDefault(); openAcct(e.key === "ArrowUp" ? "last" : "first"); return; }
  if (!acctOpen()) return;
  if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); closeAcct(); return; }
  if (!e.target.closest?.("#acct-menu")) return;
  const items = acctItems(), i = items.indexOf(document.activeElement);
  const go = (n) => { e.preventDefault(); items[(n + items.length) % items.length]?.focus(); };
  if (e.key === "ArrowDown") go(i + 1);
  else if (e.key === "ArrowUp") go(i - 1);
  else if (e.key === "Home") go(0);
  else if (e.key === "End") go(items.length - 1);
  else if (e.key === "Tab") closeAcct(false);
  else if (e.key === " " && document.activeElement?.tagName === "A") { e.preventDefault(); document.activeElement.click(); }
}, true);

// Custom logo (branding.logo in config.yaml): when the server has one, it replaces the built-in logo on the
// sign-in, waiting and main pages and becomes the favicon. Without one (204: no picture) nothing changes.
(() => {
  const probe = new Image();
  probe.onload = () => {
    document.querySelectorAll(".logo").forEach((el) => {
      const img = new Image(); img.src = "/branding/logo"; img.alt = "";
      el.classList.add("has-img"); el.replaceChildren(img);
    });
    const icon = document.querySelector('link[rel="icon"]');
    if (icon) icon.href = "/branding/logo";
  };
  probe.src = "/branding/logo";
})();

// Sign out: the server wants the CSRF header, so the "Sign out" forms are posted with fetch.
document.addEventListener("submit", async (e) => {
  if (!e.target.matches?.('form[action="/auth/logout"]')) return;
  e.preventDefault();
  try { await api("/auth/logout", { method: "POST" }); } catch (x) { flash(x.message, true); return; }
  location.href = "/";
});

async function boot() {
  try { state.me = await api("/api/me"); state.meAt = Date.now(); }
  catch (e) { if (e.status === 401) return; throw e; }  // not signed in: api() showed the sign-in view
  // A fresh page has no "Confirm it's you" dialog, so no action may be waiting for a sign-in (a reload or the
  // Back button left one behind): the server forgets it. One that just ran is already gone.
  api("/api/me/pending", { method: "DELETE" }).catch(() => { /* it expires by itself */ });
  $("#demo-banner").hidden = !state.me.demo;
  $("#doclink").hidden = state.me.docs !== true;  // /docs exists only with enable_docs: true; the link led to a bare 404 otherwise
  if (state.me.role === "pending") {  // signed in, but no role yet: nothing else to load
    const w = $("#waiting");
    $("#waiting-who").textContent = state.me.email || state.me.name || state.me.sub;
    $("#waiting-demo").innerHTML = demoSwitcher();
    if (state.me.demo) w.querySelector("form").hidden = true;
    w.hidden = false;
    return;
  }
  $("#shell").hidden = false;
  const bannerH = () => document.documentElement.style.setProperty("--banner-h", `${$("#demo-banner").hidden ? 0 : $("#demo-banner").offsetHeight}px`);
  bannerH(); window.addEventListener("resize", bannerH);
  $("#collapse").innerHTML = I.collapse; $("#menu").innerHTML = I.panel; $("#acct-btn").innerHTML = I.burger;
  $("#bell").innerHTML = `${I.bell}<span class="count" hidden></span>`;
  const wide = matchMedia("(min-width: 761px)");
  let collapsed = false; try { collapsed = localStorage.getItem("fairy-collapsed") === "1"; } catch { /* ignore */ }
  const setCollapsed = (v) => { collapsed = v; $("#shell").classList.toggle("collapsed", v && wide.matches); try { localStorage.setItem("fairy-collapsed", v ? "1" : "0"); } catch { /* ignore */ } };
  setCollapsed(collapsed); wide.addEventListener("change", () => setCollapsed(collapsed));
  $("#collapse").onclick = () => setCollapsed(!collapsed);
  $("#menu").onclick = () => { $("#shell").classList.add("drawer"); $("#scrim").hidden = false; };
  $("#scrim").onclick = () => { $("#shell").classList.remove("drawer"); $("#scrim").hidden = true; };
  buildAcct();
  await loadSettings();
  try {
    const h = await api("/api/power/history");
    state.history = h.points.map((p) => ({ t: p.t, total_w: p.total_w, sources: p.sources }));
  } catch { state.history = []; }
  try {
    const u = await api("/api/ups");
    state.ups = { generated_at: u.generated_at, ups: u.ups };
    for (const [id, pts] of Object.entries(u.history)) state.upsHist[id] = pts.map((p) => ({ t: p.t, watts: p.watts, charge: p.charge }));
  } catch { /* no UPS */ }
  try { state.notes = await api("/api/notifications/count"); } catch { /* the stream sends them */ }
  window.addEventListener("hashchange", route);
  route();
  updTick();
  showPendingResult(state.me.pending_result);  // an action that waited for this sign-in, run by the panel
}
boot();
