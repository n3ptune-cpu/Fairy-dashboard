package web

import (
	"net/http"
	"regexp"
	"strings"
)

// A compact, hand-maintained OpenAPI description of the JSON API (paths, methods, summaries). /docs renders it
// with Swagger UI; both need a signed-in session (the JSON also accepts an API token).
var apiDoc = []struct{ method, path, summary string }{
	{"GET", "/api/me", "Current user, their role (admin | viewer | pending) and the sites they see"},
	{"GET", "/api/audit", "Audit log, newest first (admins only). Query: q, action, host, before, limit"},
	{"GET", "/api/systems", "The systems you may see (normalised). Query: kind, status, group"},
	{"GET", "/api/systems/{id}", "One system"},
	{"GET", "/api/systems/{id}/processes", "Top processes (fetched on demand from the agent). Query: limit, sort=cpu|mem|gpu"},
	{"GET", "/api/summary", "Cluster totals and status counts"},
	{"GET", "/api/power", "All power sources + total wall power"},
	{"GET", "/api/power/history", "Ring buffer (default 1 h) for sparklines. Query: source"},
	{"GET", "/api/ups", "All UPSes: state, battery, load, linked systems, history"},
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
	{"GET", "/api/systems/{id}/poll", "Polling rate for one system: override, effective interval per source, floors, boost"},
	{"PUT", "/api/systems/{id}/poll", "Set the polling rate: seconds (1-300), \"paused\" or \"default\""},
	{"GET", "/api/stream", "Server-Sent Events: systems, systems_delta, summary, ups, power, ups_event, notes {unread, active_alerts}. Query: watch=<id>"},
	{"GET", "/api/admin/users", "Users, sites and site assignments (admins, browser session only)"},
	{"PUT", "/api/admin/users/{sub}", "Set a user's role and sites: {role, sites} (admins; step-up + CSRF header)"},
	{"DELETE", "/api/admin/users/{sub}", "Forget a user (admins; step-up + CSRF header; never the last admin)"},
	{"POST", "/api/admin/sites", "Create a site: {name} (admins; step-up + CSRF header)"},
	{"PUT", "/api/admin/sites/{name}", "Rename a site: {name} (admins; step-up + CSRF header)"},
	{"DELETE", "/api/admin/sites/{name}", "Delete a site (admins; step-up + CSRF header)"},
	{"PUT", "/api/admin/assign", "Put a host, power source (power:<id>) or UPS (ups:<id>) into a site: {target, site}; site null = default"},
	{"GET", "/api/admin/updates", "Agent versions, self-update state and the latest Fairy-agent release (admins only)"},
	{"POST", "/api/admin/updates/{id}", "Make one agent install the latest signed release (admins; step-up + CSRF header; self_update agents only)"},
	{"POST", "/api/admin/updates", "Update every agent that can update itself (admins; step-up + CSRF header)"},
	{"DELETE", "/api/me/pending", "Forget the action that waits for a step-up sign-in (CSRF header)"},
	{"GET", "/api/admin/panel-update", "The panel's own update: ready or why not, running version, newer release known, last run's progress (admins)"},
	{"POST", "/api/admin/panel-update", "Start the panel's update unit (admins, browser session; sign-in from the last 2 minutes + CSRF header; never older)"},
	{"GET", "/api/notifications", "Events newest first, as this user sees them. Query: severity, type, host, unread=1, before, limit"},
	{"GET", "/api/notifications/count", "Unread events and active alerts for this user"},
	{"POST", "/api/notifications/read", "Mark events read: {ids} or {all: true} (CSRF header)"},
	{"POST", "/api/notifications/dismiss", "Hide events from this user's list: {ids} (CSRF header)"},
	{"GET", "/api/system-sites", "The site of every visible host and guest"},
	{"GET", "/api/admin/hosts", "Add host: panel address, Proxmox nodes, WoL relays, sites, hosts added with Add host, enrollments (admins)"},
	{"POST", "/api/admin/hosts", "Add host: make a one-time join code and the command to run on the machine (admins; step-up + CSRF header)"},
	{"POST", "/api/admin/hosts/{id}/repair", "Re-pair a host added with Add host: a new join code (admins; step-up + CSRF header)"},
	{"DELETE", "/api/admin/hosts/{id}", "Remove a host added with Add host from the panel (admins; step-up + CSRF header)"},
	{"GET", "/api/admin/enrollments/{id}", "An Add host's progress: waiting, paired, online, failed, expired, cancelled. Query: reveal=1 (the code once, after a step-up)"},
	{"DELETE", "/api/admin/enrollments/{id}", "Cancel a waiting Add host (admins; CSRF header)"},
}

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
		"description": "Homelab resource panel API. All `/api/*` routes need a session cookie (log in at `/auth/login`) or `Authorization: Bearer fy_...`; unauthenticated calls get `401` JSON."},
		"paths": paths}
}

const swaggerHTML = `<!doctype html><html><head><meta charset="utf-8"><title>Fairy API</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css"></head>
<body><div id="swagger-ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js"></script>
<script>SwaggerUIBundle({url: "/api/openapi.json", dom_id: "#swagger-ui"})</script></body></html>`

func (s *Server) docsRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/openapi.json", s.h(func(c *reqCtx) error {
		if _, err := c.view(); err != nil {
			return err
		}
		c.json(200, openAPI(s.Version))
		return nil
	}))
	m.HandleFunc("GET /docs", s.h(func(c *reqCtx) error {
		if c.sessionUser() == nil {
			c.redirect("/auth/login?next=/docs")
			return nil
		}
		c.flush()
		c.w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = c.w.Write([]byte(swaggerHTML))
		return nil
	}))
}
