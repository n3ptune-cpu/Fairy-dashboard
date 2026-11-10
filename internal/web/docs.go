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
	{"GET", "/api/systems/{id}/power", "Power actions for one system: available or why not, confirmation needed, pending shutdown"},
	{"POST", "/api/systems/{id}/power", "Shutdown / reboot / cancel / start / stop / wake (control group + CSRF header; `confirm` = the system name for typed actions)"},
	{"POST", "/api/systems/{id}/helper/stop", "Stop a Windows agent's elevated helper (admins, browser session only: API tokens get 403; CSRF header, recent sign-in; `confirm` = the host name). Fans go back to the BMC's automatic control, Core Temp / BMC readings pause"},
	{"POST", "/api/systems/{id}/helper/start", "Start a Windows agent's elevated helper again (same checks as stop)"},
	{"GET", "/api/systems/{id}/poll", "Polling rate for one system: override, effective interval per source, floors, boost"},
	{"PUT", "/api/systems/{id}/poll", "Set the polling rate: seconds (1-300), \"paused\" or \"default\""},
	{"GET", "/api/stream", "Server-Sent Events: systems, systems_delta, summary, ups, power, ups_event, notes {unread, active_alerts}. Query: watch=<id>. At most 8 open streams per user or API token (200 for the panel): one more gets 429 with Retry-After. An open stream never counts as use of the session and ends when the session ends"},
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
	{"DELETE", "/api/me/pending", "Forget the action that waits for a step-up sign-in (CSRF header)"},
	{"POST", "/api/me/active", "Tell the panel that someone is using this page: keeps the session from idling out (CSRF header; answers 204). The UI sends it at most once a minute while there is input"},
	{"GET", "/api/admin/panel-update", "The panel's own update: ready or why not, running version, newer release known, last run's progress (admins)"},
	{"POST", "/api/admin/panel-update", "Start the panel's update unit (admins, browser session; sign-in from the last 2 minutes + CSRF header; never older)"},
	{"GET", "/api/notifications", "Events newest first, as this user sees them. Query: severity, type, host, unread=1, before, limit. A repeated event is merged into one that carries `count` (2 and up)"},
	{"GET", "/api/notifications/count", "Unread events and active alerts for this user"},
	{"POST", "/api/notifications/read", "Mark events read: {ids} or {all: true} (CSRF header)"},
	{"POST", "/api/notifications/dismiss", "Hide events from this user's list: {ids} (CSRF header)"},
	{"GET", "/api/system-sites", "The site of every visible host and guest"},
	{"GET", "/api/admin/hosts", "Add host: panel address, Proxmox nodes, WoL relays, sites, hosts added with Add host, enrollments (admins)"},
	{"POST", "/api/admin/hosts", "Add host: make a one-time join code and the command to run on the machine (admins, browser session only: API tokens get 403; step-up + CSRF header)"},
	{"POST", "/api/admin/hosts/{id}/repair", "Re-pair a host added with Add host: a new join code (admins, browser session only: API tokens get 403; step-up + CSRF header)"},
	{"DELETE", "/api/admin/hosts/{id}", "Remove a host added with Add host from the panel (admins, browser session only: API tokens get 403; step-up + CSRF header)"},
	{"GET", "/api/admin/enrollments/{id}", "An Add host's progress: waiting, paired, online, failed, expired, cancelled. Query: reveal=1 (the code once, after a step-up)"},
	{"DELETE", "/api/admin/enrollments/{id}", "Cancel a waiting Add host (admins; CSRF header)"},
}

// apiDescription is the text above the route list: what holds for every route.
const apiDescription = "Homelab resource panel API. All `/api/*` routes need a session cookie (log in at `/auth/login`) or `Authorization: Bearer fy_...`; unauthenticated calls get `401` JSON.\n\n" +
	"**401.** `{\"detail\": ...}`. A session that ended (signed out, revoked, older than `session_max_age`, or not really used for `session_idle_timeout`) gets a plain 401 and its cookie is cleared: sign in again. " +
	"A control action or admin change that needs a fresh sign-in gets `detail.code` = `step_up_required` with `message`, `window_s` (how long one confirmation lasts), `auth_age_s` (age of the last sign-in as this check sees it), " +
	"`pending` (the request was kept) and, when it was kept, `pending_id`: open `/auth/login?step_up=1&pending=<pending_id>` and the panel runs the kept request once after that sign-in. " +
	"`per_action: true` is present when `step_up_per_action` is on: every such request needs its own sign-in. API tokens never get a step-up.\n\n" +
	"**Headers.** Writes need `X-Requested-With: fairy`. `X-Fairy-Active: 1` marks a request as made while someone is using the page: only such requests, writes and `POST /api/me/active` move the idle timeout. " +
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
		// The middleware sets no policy for /docs (the page needs its own), so every answer here sets one.
		c.w.Header().Set("Content-Security-Policy", CSP)
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
