package web

import (
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// "Confirm it's you" names the action it is about in plain words: the first line is what the page calls the
// action (its label, which the page also sends as X-Fairy-Action), the second is the panel's own description.
// Neither may ever be the request's JSON: the dialog once showed `Fan mode {"mode":"heavyio"} on WIN-TEST`.
//
// confirmLabels is, for every route that asks to confirm (the rows of the route / auth matrix with stepUp),
// the place in app.js where the page puts that action into words. A new route that asks to confirm fails
// TestConfirmDialogSaysEveryActionInPlainWords until its words are written and listed here.
var confirmLabels = map[string][]string{
	"POST /api/me/sessions/revoke":        {"sessCall(`Sign out ${t.dataset.label}`, t.dataset.scope === \"all\" ? \"/api/admin/sessions/revoke\" : \"/api/me/sessions/revoke\""},
	"POST /api/me/sessions/revoke-others": {`sessCall("Sign out everywhere else", "/api/me/sessions/revoke-others")`},
	"POST /api/admin/sessions/revoke":     {"sessCall(`Sign out ${t.dataset.label}`, t.dataset.scope === \"all\" ? \"/api/admin/sessions/revoke\" : \"/api/me/sessions/revoke\""},
	"POST /api/systems/{id}/fan/mode":     {"`Set fan mode to ${p.mode}`", `label: fanLabel(path, payload, state.byId[id]?.name || id)`, `post("/fan/mode", { mode: b.dataset.mode })`},
	"POST /api/systems/{id}/fan/duty":     {"`Set fan zone ${p.zone} to ${p.percent}%`", `label: fanLabel(path, payload, state.byId[id]?.name || id)`, `post("/fan/duty", { zone: +b.dataset.apply, percent: `},
	// with the countdown the kept request carries; without a dialog (Power on, Start) there is none
	"POST /api/systems/{id}/power": {"const what = `${label} ${s.name}`, timed = body.delay_s ? `${what} in ${fmtSpan(body.delay_s)}` : what;",
		"label: e.detail?.pending ? timed : what }, e.detail)", "label: `${POWER_LABEL[action]} · ${s.name}`, system: s.id, method: \"POST\", path, body: { action } }"},
	"PUT /api/systems/{id}/poll":          {"const label = `Polling for ${s.name}: ${v === \"default\" ? \"back to default\" : v === \"paused\" ? \"paused\" : `${v}s`}`;"},
	"POST /api/systems/{id}/helper/stop":  {"label: `${stop ? \"Stop\" : \"Start\"} elevated helper on ${s.name}`"},
	"POST /api/systems/{id}/helper/start": {"label: `${stop ? \"Stop\" : \"Start\"} elevated helper on ${s.name}`"},
	"POST /api/admin/updates":             {`updateCall("Update all agents", "/api/admin/updates", "all")`},
	"POST /api/admin/updates/{id}":        {"updateCall(`Update ${t.dataset.name}`, `/api/admin/updates/${enc(t.dataset.id)}`", "updateCall(`Update ${st.dataset.name}`, `/api/admin/updates/${enc(st.dataset.id)}`"},
	"POST /api/admin/panel-update":        {`label: "Update panel" }, () => api("/api/admin/panel-update"`},
	"PUT /api/admin/users/{sub}":          {"adminCall(`Role ${ROLE_LABEL[t.value] || t.value} for ${userIdent(u)}`, \"PUT\", userPath(", "adminCall(`Sites for ${userIdent(adminUser(t.dataset.sub))}`, \"PUT\", userPath("},
	"DELETE /api/admin/users/{sub}":       {"adminCall(`Remove ${t.dataset.name}`, \"DELETE\", userPath("},
	"POST /api/admin/sites":               {"adminCall(`Create site ${name}`, \"POST\", \"/api/admin/sites\""},
	"PUT /api/admin/sites/{name}":         {"adminCall(`Rename site ${t.dataset.site}`, \"PUT\", `/api/admin/sites/"},
	"DELETE /api/admin/sites/{name}":      {"adminCall(`Delete site ${t.dataset.site}`, \"DELETE\", `/api/admin/sites/"},
	"PUT /api/admin/assign":               {"adminCall(t.value ? `Put ${t.dataset.target} in ${t.value}` : `${t.dataset.target}: default site`, \"PUT\", \"/api/admin/assign\""},
	"POST /api/admin/hosts":               {"label: `Add host ${body.name}` }, () => api(\"/api/admin/hosts\""},
	"POST /api/admin/hosts/{id}/repair":   {"label: `Re-pair ${h.name}${renamed ? \" (node renamed)\" : \"\"}` }, () => api(`${path}/repair`"},
	"DELETE /api/admin/hosts/{id}":        {"label: `Remove ${h.name}` }, () => api(path, { method: \"DELETE\""},
	"DELETE /api/admin/enrollments/{id}":  {"label: `Cancel Add host ${cr.name}`, method: \"DELETE\""},
	"POST /api/admin/wake-update":         {"label: `Update machines that are off: ${n} machine${n === 1 ? \"\" : \"s\"}`, method: \"POST\""},
	"DELETE /api/admin/wake-update/{id}":  {"label: `Cancel the run (woken machines ${off ? \"are switched off again\" : \"stay on\"})`, method: \"DELETE\""},
	"PUT /api/systems/{id}/wake-update":   {"const label = `${on ? \"Switch on\" : \"Switch off\"} \"Update machines that are off\" for ${name}`;"},
}

// jsonInText finds a value turned into JSON inside a text the page builds (a template literal).
var jsonInText = regexp.MustCompile(`\$\{[^{}]*JSON\.stringify\([^)]*\)[^{}]*\}`)

func TestConfirmDialogSaysEveryActionInPlainWords(t *testing.T) {
	js := appJS(t)
	// 1. every route that asks to confirm has its words in the page, and the list names no other route
	asks := map[string]writeRoute{}
	for _, wr := range matrixRoutes {
		if wr.stepUp {
			asks[wr.pattern] = wr
		}
	}
	for pat := range asks {
		places, ok := confirmLabels[pat]
		if !ok || len(places) == 0 {
			t.Errorf("%s asks to confirm, and confirmLabels does not say where app.js puts it into words for \"Confirm it's you\"", pat)
		}
		for _, p := range places {
			if !strings.Contains(js, p) {
				t.Errorf("%s: app.js lacks %q", pat, p)
			}
			if strings.Contains(p, "JSON.stringify") && !strings.Contains(p, "body: JSON.stringify") {
				t.Errorf("%s: its words are made from JSON: %q", pat, p)
			}
		}
	}
	for pat := range confirmLabels {
		if _, ok := asks[pat]; !ok {
			t.Errorf("confirmLabels: %s is not a route that asks to confirm", pat)
		}
	}
	// 2. nowhere does the page put JSON into a text it shows, except the Audit page's cell for a nested value
	for _, m := range jsonInText.FindAllString(js, -1) {
		if m != `${typeof v === "object" ? JSON.stringify(v) : v}` {
			t.Errorf("app.js turns a value into JSON inside a text: %s", m)
		}
	}
	for _, line := range strings.Split(js, "\n") {
		if i := strings.Index(line, "label"); i >= 0 && strings.Contains(line[i:], "${JSON.stringify") {
			t.Errorf("a label made from JSON: %s", strings.TrimSpace(line))
		}
	}
	// 3. the dialog shows the label, the panel's description and the panel's list, each through esc(), and
	// nothing else of the request: no body, no path
	from, to := strings.Index(js, "function showStepUp(pending, detail) {"), strings.Index(js, "async function confirmStepUp(")
	if from < 0 || to < from {
		t.Fatal("showStepUp not found in app.js")
	}
	dlg := js[from:to]
	html := dlg[strings.Index(dlg, "const runs ="):]
	for _, want := range []string{
		`<span class="note">Pending action</span><b>${esc(pending.label)}</b>${runs}${items}`,
		`<span class="note">Fairy will run</span><b id="su-runs">${esc(detail.action)}</b>`,
		"detail.items.map((x) => `<li>${esc(x)}</li>`)",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the dialog lacks %q", want)
		}
	}
	for _, bad := range []string{"JSON.stringify", "pending.body", "pending.path", "pending.method", ".body)"} {
		if strings.Contains(html, bad) {
			t.Errorf("the dialog's text is made with %q", bad)
		}
	}
	if raw := regexp.MustCompile(`\$\{(pending|detail)\??\.[A-Za-z_.?]+\}`).FindAllString(html, -1); len(raw) != 0 {
		t.Errorf("the dialog inserts values without esc(): %v", raw)
	}

	// 4. the panel's own description (the second line, and the audit log) of each of these routes, for the
	// request the matrix sends: words, no JSON, and not the bare method and path
	samples := map[any]any{
		"POWER":   map[string]any{"action": "shutdown", "delay_s": 300, "confirm": "LAPTOP"},
		"CONFIRM": map[string]any{"confirm": "LAPTOP"},
		"SESSION": map[string]any{"session": "handle"},
		"WAKERUN": map[string]any{"update_only": []string{"laptop"}},
	}
	for pat, wr := range asks {
		method, route, _ := strings.Cut(pat, " ")
		req := httptest.NewRequest(method, wr.path, nil)
		req.Pattern = pat
		vals, rs, ps := map[string]string{}, strings.Split(route, "/"), strings.Split(wr.path, "/")
		for i, seg := range rs {
			if m := wildcardRe.FindStringSubmatch(seg); m != nil && i < len(ps) {
				vals[m[1]] = ps[i]
			}
		}
		body := wr.body
		if s, ok := body.(string); ok {
			body = samples[s]
		}
		b, _ := json.Marshal(body)
		got := describeAction(req, vals, b)
		if got == "" || strings.HasPrefix(got, method+" ") || strings.ContainsAny(got, "{}[]") || strings.Contains(got, `":`) {
			t.Errorf("%s: the panel describes it as %q", pat, got)
		}
		for k, v := range vals { // the host, user or site its text has a place for
			if strings.Contains(actionText[pat], "{"+k+"}") && !strings.Contains(got, v) {
				t.Errorf("%s: the description %q does not name %q", pat, got, v)
			}
		}
	}
}
