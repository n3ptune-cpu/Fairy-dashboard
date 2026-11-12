package web

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The owner's test of 2026-11-12: the Client ID of the SIGN-IN client was put into approval.client_id by
// mistake, and the Turn on dialog showed "Client: Fairy PC actions · fda18865-…": the panel printed the name
// "Fairy PC actions" next to an id whose name it cannot know, which hid the mix-up until Pocket ID refused the
// request. The panel knows a client's ID, never its name: it prints no name. Everywhere the configured client
// is shown, it is "Client ID" and the id, and behind the info button what it must be.

const clientIDMust = "It must be the Client ID of the separate client for PC actions in Pocket ID, not the sign-in client's."

func TestThePanelPrintsNoClientName(t *testing.T) {
	const name = "Fairy PC actions"
	a := newApprovalEnv(t)
	// the approval page and the Turn on dialog
	page, _ := a.page(t)
	if _, has := page["client"]; has || page["client_id"] != testApprovalClient {
		t.Errorf("the approval page: client %v, client_id %v", page["client"], page["client_id"])
	}
	r := a.turnOn("laptop")
	if _, _, ok := approvalAsked(r); !ok {
		t.Fatalf("Turn on: %d %s", r.code, r.raw)
	}
	d := r.body["detail"].(map[string]any)
	if items := fmt.Sprint(d["items"]); !strings.Contains(items, "Pocket ID: "+a.iss.URL()+" · Client ID "+testApprovalClient) || strings.Contains(string(r.raw), name) {
		t.Errorf("the Turn on dialog: %s", r.raw)
	}
	// "Confirm it's you" for an action a machine checks
	a.require(t, a.tower)
	r = a.post("/api/systems/tower/fan/mode", map[string]any{"mode": "full"})
	if _, _, ok := approvalAsked(r); !ok {
		t.Fatalf("fan mode: %d %s", r.code, r.raw)
	}
	ap := r.body["detail"].(map[string]any)["approval"].(map[string]any)
	if _, has := ap["client"]; has || ap["client_id"] != testApprovalClient || strings.Contains(string(r.raw), name) {
		t.Errorf("the dialog's answer: %s", r.raw)
	}
	// no answer of a panel without the approval: block names a client either
	e := setup(t)
	adm := []string{"Cookie", e.cookie(controller(), 0), "X-Requested-With", "fairy", "Content-Type", "application/json"}
	for _, x := range []resp{e.do("GET", "/api/admin/approval", nil, adm...), e.do("POST", "/api/admin/approval/turn-on", map[string]any{"hosts": []string{"laptop"}}, adm...)} {
		if strings.Contains(string(x.raw), name) {
			t.Errorf("without approval: %d %s", x.code, x.raw)
		}
	}
	// the page script: the name is in none of its texts; the three places say "Client ID", with the id
	// through esc() and the one line behind the info button
	js := appJS(t)
	if strings.Contains(js, name) {
		t.Error("app.js prints the client's name")
	}
	for _, want := range []string{
		`const CLIENT_ID_MUST = "` + clientIDMust + `";`,
		"const clientIdInfo = (key) => infoBtn(key, \"Client ID\", `<p>${CLIENT_ID_MUST}</p>`);",
		"<span class=\"note\">Client ID${clientIdInfo(\"su-client-id\")}</span><b id=\"su-client\"><code>${esc(appr.client_id || \"–\")}</code></b>",             // Confirm it's you
		"<div class=\"list-row\"><span>Client ID${clientIdInfo(\"ap-client-id\")}</span><span><code id=\"ap-client-id\">${esc(d.client_id)}</code></span></div>", // Turn on
		"${row(`Client ID${clientIdInfo(`apbox-client-${s.id}`)}`, `<code>${esc(a.client_id || \"–\")}</code>`)}",                                                // Approval box
		`Pocket ID will ask for your passkey. It confirms only the action below.`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// the server's own texts: no Go source of the panel has the name in a line that is not a comment
	for _, dir := range []string{".", "../approval", "../admin", "../config", "../../cmd/fairy"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(raw), "\n") {
				if code, _, _ := strings.Cut(line, "//"); strings.Contains(code, name) {
					t.Errorf("%s:%d prints the client's name: %s", f, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
}
