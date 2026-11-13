package web

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"regexp"
	"strings"
)

// A compact, hand-maintained OpenAPI description of the JSON API (paths, methods, summaries). /docs renders it
// with Swagger UI; both need a signed-in user with a role (the JSON also accepts an API token, /docs only a
// browser session).
var apiDoc = []struct{ method, path, summary string }{
	{"GET", "/api/me", "Current user, their role (admin | viewer | pending) and the sites they see"},
	{"GET", "/api/audit", "Audit log, newest first (admins only). Query: q, action, host, before, limit"},
	{"GET", "/api/systems", "The systems you may see (normalised). Query: kind, status, group. `status` is online | offline | stopped | stale | paused | error; an `error` entry is a system whose data the panel cannot show: every key is present, lists are empty, measured values are null and `error` says why"},
	{"GET", "/api/systems/{id}", "One system"},
	{"GET", "/api/systems/{id}/processes", "Top processes (fetched on demand from the agent). Query: limit, sort=cpu|mem|gpu"},
	{"GET", "/api/summary", "Status counts, per-cluster totals and all-hosts totals (CPU, memory), over what the caller may see"},
	{"GET", "/api/power", "All power sources + total wall power"},
	{"GET", "/api/power/history", "Ring buffer (default 1 h) for sparklines. Query: source"},
	{"GET", "/api/ups", "All UPSes: state, battery, load, linked systems, history. `stale` with `error` can come with any `status` (a value the UPS reported was dropped, or the UPS left its server's answer)"},
	{"GET", "/api/ups/events", "UPS transitions (newest first). Query: limit, after"},
	{"GET", "/api/systems/{id}/fan", "Fan mode, RPMs, watchdog events, modes the BMC refused (unsupported_modes)"},
	{"POST", "/api/systems/{id}/fan/mode", "Set BMC fan mode (control group + `X-Requested-With: fairy` header)"},
	{"POST", "/api/systems/{id}/fan/duty", "Set zone duty % (requires mode=full; agent enforces minimum + temp watchdog)"},
	{"GET", "/api/settings", "Current user's UI settings (session only)"},
	{"PUT", "/api/settings", "Replace UI settings (CSRF header required)"},
	{"POST", "/api/settings/background", "Upload a background image (raw body; PNG/JPEG/WebP/GIF/AVIF, max 8 MiB, no SVG)"},
	{"GET", "/api/settings/background", "The uploaded background image"},
	{"DELETE", "/api/settings/background", "Remove the uploaded background"},
	{"GET", "/api/admin/signin-wallpaper", "The sign-in wallpaper's state: {set, type, bytes, version, max_bytes} (admins, browser session). The picture itself is public: GET /branding/wallpaper, 204 when none is set"},
	{"POST", "/api/admin/signin-wallpaper", "Set the picture behind the sign-in page (admins, browser session; CSRF header; raw body; asks to confirm it is you). PNG, JPEG or WebP by content, max 8 MiB; the file's name and Content-Type are not looked at. 415 for anything else, 413 too large"},
	{"DELETE", "/api/admin/signin-wallpaper", "Remove the sign-in wallpaper (admins, browser session; CSRF header; asks to confirm it is you)"},
	{"GET", "/api/systems/{id}/power", "Power actions for one system: available or why not, confirmation needed, pending shutdown"},
	{"POST", "/api/systems/{id}/power", "Shutdown / reboot / sleep / cancel / start / stop / wake (control group + CSRF header; `confirm` = the system name for typed actions). `cancel` stops a pending shutdown, restart or sleep and is never asked to confirm: no \"Confirm it's you\" and no approval of the machine (stopping is the safe direction, and a countdown can be ten seconds long). 409 when the countdown had already run out: nothing was cancelled. The kill switch (423) and the host's power switch (409) refuse every action but `cancel`: calling a pending shutdown, reboot or sleep off works while control is off (still admins, CSRF header, its own rate limit; the audit entry then carries `control_off`). With the kill switch on that cancel is accepted only from a browser session: an API token gets 423"},
	{"POST", "/api/systems/{id}/helper/stop", "Stop a Windows agent's elevated helper (admins, browser session only: API tokens get 403; CSRF header, recent sign-in; `confirm` = the host name). Fans go back to the BMC's automatic control, Core Temp / BMC readings pause"},
	{"POST", "/api/systems/{id}/helper/start", "Start a Windows agent's elevated helper again (same checks as stop)"},
	{"GET", "/api/systems/{id}/poll", "Polling rate for one system: override, effective interval per source, floors, boost"},
	{"PUT", "/api/systems/{id}/poll", "Set the polling rate: seconds (1-300), \"paused\" or \"default\""},
	{"PUT", "/api/systems/{id}/custom-model", "Set a host's model name in the panel: {custom_model: \"<name>\"}; \"\" removes it. The system then carries `custom_model` (null: none) next to `hardware.display`, which stays what the machine reports. Admins, browser session only (API tokens get 403), CSRF header; no step-up, not stopped by the kill switch. 1 to 64 characters, one line; 422 for control, format (invisible, direction) and private-use characters, text that is not UTF-8, or a longer name; 404 unknown system; 409 a VM or container. Audited as host.model"},
	{"GET", "/api/stream", "Server-Sent Events: systems, systems_delta, summary, ups, power, ups_event, notes {unread, active_alerts}, notes_new {last, events, more, more_urgent}: the warnings and critical events made while the stream is open (what the page shows as pop-ups; `urgent` marks critical events and the ones that start an alert). Query: watch=<id>, notes_after=<id> (go on after that notification instead of now; nothing older than a minute). At most 8 open streams per user or API token (200 for the panel): one more gets 429 with Retry-After. An open stream never counts as use of the session and ends when the session ends"},
	{"GET", "/api/admin/users", "Users, sites and site assignments (admins, browser session only). Every user carries `email_verified`: true, false, or null (not known yet); only true means Pocket ID vouched for the address"},
	{"PUT", "/api/admin/users/{sub}", "Set a user's role and sites: {role, sites} (admins; step-up + CSRF header)"},
	{"DELETE", "/api/admin/users/{sub}", "Forget a user (admins; step-up + CSRF header; never the last admin)"},
	{"POST", "/api/admin/sites", "Create a site: {name} (admins; step-up + CSRF header)"},
	{"PUT", "/api/admin/sites/{name}", "Rename a site: {name} (admins; step-up + CSRF header)"},
	{"DELETE", "/api/admin/sites/{name}", "Delete a site (admins; step-up + CSRF header)"},
	{"PUT", "/api/admin/assign", "Put a host, power source (power:<id>) or UPS (ups:<id>) into a site: {target, site}; site null = default"},
	{"GET", "/api/admin/updates", "Agent versions, self-update state and the latest Fairy-agent release (admins only)"},
	{"POST", "/api/admin/updates/{id}", "Make one agent install the latest signed release (admins; step-up + CSRF header; self_update agents only)"},
	{"POST", "/api/admin/updates", "Update every agent that can update itself (admins; step-up + CSRF header)"},
	{"POST", "/api/admin/update-check", "Check GitHub for new agent and panel releases now, answer with the Updates page (admins, browser session; CSRF header; once per 30 s per panel)"},
	{"GET", "/api/admin/approval", "The check of approvals on the machines themselves (admins): {configured, issuer, client_id (the approval client's ID; the panel does not know a client's name), callback, keys (what Pocket ID publishes now: kid, alg, thumbprint), keys_error, machines, ready, required, broken, max, last}. Per machine (agent): `id`, `system_id`, `name`, `online`, `state` (off | required | broken | unknown; \"\": its agent does not know the check), `enforced_by`, `ready` / `why`, `users` (who would be allowed to approve: sub, name, why), `stale_keys`, `other_trust`, `set_by`, `set_at`, `last_known` (silent since the panel started: the state is from the panel's record), and the panel's decision about which machine it is: `binding` (no_record: it has the check on and no machine id is on record for it; changed | duplicate | vanished | too_old | unreadable; absent: in order) with `binding_text`, `with` (the hosts that have the same machine id), `can_accept` / `accept` (first | id | keep | off) and `held_by` (the hosts that keep the id it shows: it cannot be accepted here). For an admin in a browser only (an API token gets none of them): `machine_id` (the id on record), `shown_id`, `pinned` (turn-on | accepted: an id comes on record through Turn on or an admin's accept only, never because a host shows it), `pinned_at`, `pinned_by`. `last` is the report of the last Turn on: {at, by, results: [{id, name, outcome, text}]}"},
	{"POST", "/api/admin/approval/turn-on", "Switch the machine's own check of approvals on: {hosts: [agent ids]} (1-64). Admins, browser session only (no API token), CSRF header, the kill switch. The panel asks each machine for a challenge for the hash of what it would trust (Pocket ID, the Client ID of approval.client_id, the pinned key(s), the users) and answers 401 step_up_required with `approval`, `action` and `items` (one line per machine with who may approve); after ONE passkey approval the kept request sends each machine its proposal with the approval. 200 {results: [{id, name, outcome: accepted | proof_refused | already_set | offline | refused | not_asked, text}], message}; 409 when no machine could be switched on. Afterwards a machine is changed only on the machine (`fairy-agent helper-trust`): the panel has no route for it"},
	{"POST", "/api/admin/approval/accept/{id}", "Accept what the machine of agent {id} shows about its own check. The body says what the page showed, exactly one of {machine_id: the id the machine shows, in full} and {off: true}: its machine id (the host has the check on and nothing is on record for it; or it showed another id than the one on record, or the same id as another host: this host keeps it, the other stays flagged until it shows one of its own), or that it has no check any more. Admins, browser session only (no API token), CSRF header, the kill switch, and always its own \"Confirm it's you\" (401 step_up_required with `action`: what would be accepted, an id as its first eight and last six characters; and for an id `items`: one line, what to compare it with on the machine first). Nothing is sent to the machine. 200 {ok, accepted: id | off, message}; 409: the machine shows nothing to accept (it must be answering), it shows something else than the body says (it changed while the admin was confirming), or another host keeps the id it shows (one id is accepted for one host only: the machine needs an id of its own first)"},
	{"GET", "/api/admin/wake-update", "\"Update unpowered hosts\" (admins): {hosts, settings, latest, active, last, now, problem, max_hosts, off_after_s (the panel's wake_update.off_after_s; 900 unless set)}. Per host: `off_after_s` / `shutdown_delay_s` (only for a machine whose systems: entry sets its own, and only when it is not the panel's value), `opted_in`, `can_join` / `why_not`, `can_wake` / `no_wake`, `relay`, `power` (off: no answer for the machine's `off_after_s`; on; unclear: anything between, with `not_sure`), `choice` (wake | update_only | \"\" with `why`), `version`, `behind`. `active` is the run that is going (null: none), `last` the finished ones; a run has `state`, `by`, `list_hash`, `hosts` (each with `state`, `text`, `mode`, `update`, `on`, times, a `log`, and its own `off_after_s` / `shutdown_delay_s` where the run used other values for it than its `settings`), `cancel`, `summary`"},
	{"POST", "/api/admin/wake-update", "Start a run: {hosts: [ids that are off: wake, update, switch off again], update_only: [ids that are on: update, never switched off], update_if_on: bool}. Admins, browser session only (no API token), CSRF header, the kill switch, and always its own \"Confirm it's you\" for exactly this list (401 step_up_required with `action` and `items`; the kept request runs after the sign-in). 202 {id, list}. 400: a machine that is unknown, named twice, not switched on for it, or that cannot be woken; 409: a run is already going"},
	{"DELETE", "/api/admin/wake-update/{id}", "Cancel the run that is going: {switch_off: bool} (optional; false: the machines the run has woken stay on and are reported; true: they are still switched off again). Admins, browser session only, CSRF header, step-up; works with the kill switch on. 404: that run is not going"},
	{"PUT", "/api/systems/{id}/wake-update", "Switch a machine on or off for \"Update unpowered hosts\": {enabled: bool}. Off by default. Admins, browser session only, CSRF header, step-up. 409: the machine cannot take part (its agent has no self_update, or the panel runs on it)"},
	{"GET", "/api/me/sessions", "Your own sessions (browser session; not while waiting for access): {sessions, idle_timeout_s, max_age_s}. Per session: `id` (a handle that names it here; never the session id), `current`, `signed_in`, `last_active` (null = not known), `ip` (latest request), `signed_in_ip`, `also_from` (other addresses it was used from since the panel started), `browser`, `user_agent`, `location` (empty in this version)"},
	{"POST", "/api/me/sessions/revoke", "Sign out one of your other sessions: {session: id}. CSRF header, and always its own \"Confirm it's you\" (401 step_up_required, the request is kept and runs after the sign-in). 404: no such session of yours; 409: it is the session you are using (use Sign out)"},
	{"POST", "/api/me/sessions/revoke-others", "Sign out every session of yours except this one (CSRF header; its own \"Confirm it's you\"). Answers {count}; with no other session nothing is asked and count is 0"},
	{"GET", "/api/admin/sessions", "Everyone's sessions, each with `user` {sub, name, email, you} (admins, browser session)"},
	{"POST", "/api/admin/sessions/revoke", "Sign out any session but the one you are using: {session: id} (admins, browser session; CSRF header; its own \"Confirm it's you\")"},
	{"DELETE", "/api/me/pending", "Forget the action that waits for a step-up sign-in (CSRF header)"},
	{"POST", "/api/me/step-up", "Confirm the kept action: {pending_id, next}. Answers {url, action}: the sign-in address to go to, and what the panel will run after it. Browser session only, CSRF header, and only from the browser and address the action was kept for; 409 when nothing is waiting under that id"},
	{"POST", "/api/me/active", "Tell the panel that someone is using this page: keeps the session from idling out (CSRF header; answers 204). The UI sends it at most once a minute while there is input"},
	{"GET", "/api/admin/panel-update", "The panel's own update: ready or why not, running version, newer release known, last run's progress (admins)"},
	{"POST", "/api/admin/panel-update", "Start the panel's update unit (admins, browser session; sign-in from the last 2 minutes + CSRF header; never older)"},
	{"GET", "/api/notifications", "Events newest first, as this user sees them. Query: severity, type, host, unread=1, before, limit. severity=critical: the events with `alarm` (they start an alert: a host offline, a temperature over its limit, a UPS on battery or out of reach) and those whose severity is critical; severity=warning: the other warnings. A repeated event is merged into one that carries `count` (2 and up)"},
	{"GET", "/api/notifications/count", "Unread events and active alerts for this user"},
	{"POST", "/api/notifications/read", "Mark events read: {ids} or {all: true} (CSRF header)"},
	{"POST", "/api/notifications/dismiss", "Hide events from this user's list: {ids} (CSRF header)"},
	{"GET", "/api/system-sites", "The site of every visible host and guest"},
	{"GET", "/api/admin/hosts", "Add host: panel address, Proxmox nodes, WoL relays, sites, hosts added with Add host (pairing: v2 = the agent's pairing answer was verified; absent = paired by an older panel), enrollments (admins)"},
	{"POST", "/api/admin/hosts", "Add host: make a one-time join code and the command to run on the machine (admins, browser session only: API tokens get 403; step-up + CSRF header). 400: an address outside Tailscale (unless enroll.allow_non_tailscale). 409: the Fairy-agent release is too old (its install script lacks pairing v2, or takes the join code only on its command line); the panel never pairs the old way. The command hands the code to the installer on standard input (Linux) or in an environment variable (Windows)"},
	{"POST", "/api/admin/hosts/{id}/repair", "Re-pair a host added with Add host: a new join code (admins, browser session only: API tokens get 403; step-up + CSRF header). Body: confirm (the host's name), node_renamed (accept another Proxmox node name than the one it was added as). The answer is checked like a new host's"},
	{"DELETE", "/api/admin/hosts/{id}", "Remove a host added with Add host from the panel (admins, browser session only: API tokens get 403; step-up + CSRF header)"},
	{"GET", "/api/admin/enrollments/{id}", "An Add host's progress: waiting, paired, online, failed, expired, cancelled. pairing: v2 once the agent proved it has the join code; agent_hostname: what that machine calls itself. hint while waiting: agent_too_old (update the agent first; the same code pairs afterwards), legacy_join_code, unverified_answer (what answers at the address is not a Fairy agent that has the join code: not added, nothing it sent was used; the message says what to do, and the hint stays when the code expires), not_tailscale, rate_limited (the agent's limit of pairing requests a minute was reached; the panel asks less often for a while). warning: the machine runs a newer agent than the command installs. Query: reveal=1 (the code once, after a step-up: browser session only, only for the browser and address that confirmed this Add host, and only while that confirmation still counts; 403 otherwise, and the code stays where it is)"},
	{"DELETE", "/api/admin/enrollments/{id}", "Cancel a waiting Add host or Re-pair (admins, browser session: no API tokens; step-up + CSRF header; works with the kill switch on)"},
}

// apiDescription is the text above the route list: what holds for every route.
const apiDescription = "Homelab resource panel API. All `/api/*` routes need a session cookie (log in at `/auth/login`) or `Authorization: Bearer fy_...`; unauthenticated calls get `401` JSON.\n\n" +
	"**401.** `{\"detail\": ...}`. A session that ended (signed out, revoked, older than `session_max_age`, or not really used for `session_idle_timeout`) gets a plain 401 and its cookie is cleared: sign in again. " +
	"A control action or admin change that needs a fresh sign-in gets `detail.code` = `step_up_required` with `message`, `window_s` (how long one confirmation lasts), `auth_age_s` (age of the last sign-in as this check sees it), " +
	"`pending` (the request was kept) and, when it was kept, `pending_id` and `action` (the panel's own description of what would run): `POST /api/me/step-up` with `{\"pending_id\": ...}` answers with the sign-in address, and the panel runs the kept request once after that sign-in. " +
	"A request is kept only for the browser and address the session was signed in or last confirmed from; otherwise `not_kept` says why (`new_client`: sign in again with `/auth/login?step_up=1`, then repeat the request; `too_large`). A link never runs a kept request. " +
	"`per_action: true` is present when `step_up_per_action` is on: every such request needs its own sign-in. API tokens never get a step-up.\n\n" +
	"**Headers.** Writes need `X-Requested-With: fairy`. `X-Fairy-Active: 1` marks a request as made while someone is using the page: only such requests, writes and `POST /api/me/active` move the idle timeout, and only when they are answered without an error. " +
	"`X-Fairy-Action` (optional) is the action's label for the step-up dialog, percent-encoded UTF-8.\n\n" +
	"**429** carries `Retry-After`."

var paramRe = regexp.MustCompile(`\{(\w+)\}`)

func openAPI(version string) map[string]any {
	paths := map[string]any{}
	for _, d := range apiDoc {
		item, _ := paths[d.path].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[d.path] = item
		}
		op := map[string]any{"summary": d.summary, "responses": map[string]any{
			"200": map[string]any{"description": "OK"}, "401": map[string]any{"description": "not authenticated"}}}
		var params []any
		for _, m := range paramRe.FindAllStringSubmatch(d.path, -1) {
			params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
		}
		if params != nil {
			op["parameters"] = params
		}
		item[strings.ToLower(d.method)] = op
	}
	return map[string]any{"openapi": "3.1.0", "info": map[string]any{"title": "Fairy", "version": version,
		"description": apiDescription},
		"paths": paths}
}

// Swagger UI is the one thing a Fairy page loads from another host, and only on /docs (enable_docs, off by
// default). Its two files are pinned to one release and to the SHA-384 of their exact bytes: the browser checks
// the hash before it runs or applies a file (Subresource Integrity) and refuses anything else, so a changed
// file on the CDN cannot run in the panel with the signed-in user's session.
//
// The hashes are of the files jsDelivr serves for swagger-ui-dist 5.17.14 (downloaded 2026-11-10):
//
//	swagger-ui.css        152071 bytes  sha256 40170f0ee859d17f92131ba707329a88a070e4f66874d11365e9a77d232f6117
//	swagger-ui-bundle.js 1452753 bytes  sha256 c2e4a9ef08144839ff47c14202063ecfe4e59e70a4e7154a26bd50d880c88ba1
//
// To move to another release change swaggerVersion and both hashes together:
//
//	curl -sL https://cdn.jsdelivr.net/npm/swagger-ui-dist@VERSION/FILE | openssl dgst -sha384 -binary | openssl base64 -A
const (
	swaggerVersion = "5.17.14"
	swaggerBase    = "https://cdn.jsdelivr.net/npm/swagger-ui-dist@" + swaggerVersion + "/"
	swaggerCSS     = swaggerBase + "swagger-ui.css"
	swaggerCSSHash = "sha384-wxLW6kwyHktdDGr6Pv1zgm/VGJh99lfUbzSn6HNHBENZlCN7W602k9VkGdxuFvPn"
	swaggerJS      = swaggerBase + "swagger-ui-bundle.js"
	swaggerJSHash  = "sha384-wmyclcVGX/WhUkdkATwhaK1X1JtiNrr2EoYJ+diV3vj4v6OC5yCeSu+yW13SYJep"

	// swaggerInit is the page's only inline script. validatorUrl null: no badge request to validator.swagger.io
	// (which would tell that site the panel's address).
	swaggerInit = `SwaggerUIBundle({url: "/api/openapi.json", dom_id: "#swagger-ui", validatorUrl: null})`
)

// swaggerHTML is the /docs page: the two pinned files with their hashes, and the inline start-up line.
const swaggerHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Fairy API</title>
<link rel="stylesheet" href="` + swaggerCSS + `" integrity="` + swaggerCSSHash + `" crossorigin="anonymous" referrerpolicy="no-referrer"></head>
<body><div id="swagger-ui"></div>
<script src="` + swaggerJS + `" integrity="` + swaggerJSHash + `" crossorigin="anonymous" referrerpolicy="no-referrer"></script>
<script>` + swaggerInit + `</script></body></html>`

// docsCSP is the Content-Security-Policy of exactly the /docs page (every other page has the stricter CSP,
// which allows nothing from other hosts). Scripts: only the pinned bundle and the one inline line above, by
// its hash; nothing else can run, whatever the document or the bundle asks for. Styles: the pinned stylesheet,
// plus inline styles, which Swagger UI sets on its elements. Requests: only back to the panel, for
// /api/openapi.json and "Try it out". Images and fonts: the panel and data: URLs (the stylesheet embeds its
// icons). No frames, no forms, no <base>.
var docsCSP = "default-src 'none'; script-src " + swaggerJS + " '" + inlineScriptHash(swaggerInit) + "'; " +
	"style-src " + swaggerCSS + " 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// inlineScriptHash is the CSP source that allows exactly this inline script.
func inlineScriptHash(script string) string {
	sum := sha256.Sum256([]byte(script))
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

func (s *Server) docsRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/openapi.json", s.h(func(c *reqCtx) error {
		if _, err := c.view(); err != nil {
			return err
		}
		c.json(200, openAPI(s.Version))
		return nil
	}))
	m.HandleFunc("GET /docs", s.h(func(c *reqCtx) error {
		// Redirects and refusals keep the panel's policy (set for every answer); only the page itself, below,
		// replaces it with docsCSP.
		if c.r.Header.Get("Authorization") == "" && c.sessionUser() == nil {
			c.redirect("/auth/login?next=/docs")
			return nil
		}
		if _, err := c.requireSession(); err != nil { // a browser session only, as before (no API tokens)
			return err
		}
		if _, g, _ := c.grant(); g.Pending() { // signed in but without a role: no data, and no third-party script either
			return c.errorPage(403, "Waiting for access", "The API documentation is for people with a role. "+
				"Ask an admin to give you one on the Users page.", "/")
		}
		c.flush()
		h := c.w.Header()
		h.Set("Content-Security-Policy", docsCSP)
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		_, _ = c.w.Write([]byte(swaggerHTML))
		return nil
	}))
}
