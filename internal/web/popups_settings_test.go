package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// The three settings are part of a user's settings: read with them, saved with them, checked like them.
func TestPopupSettingsAPI(t *testing.T) {
	e := setup(t)
	c := e.cookie(viewer, 0)
	csrf := []string{"Cookie", c, "X-Requested-With", "fairy"}
	popups := func(r resp) string { b, _ := json.Marshal(r.body["popups"]); return string(b) }
	const def = `{"enabled":true,"level":"critical","seconds":5}`
	if r := e.do("GET", "/api/settings", nil, "Cookie", c); r.code != 200 || popups(r) != def {
		t.Fatal("defaults:", r.code, popups(r))
	}
	base := func(p any) map[string]any {
		m := map[string]any{"theme": "black", "background": map[string]any{"kind": "none"}, "dim": 50, "blur": 10, "card_opacity": 70}
		if p != nil {
			m["popups"] = p
		}
		return m
	}
	const mine = `{"enabled":false,"level":"warning","seconds":9}`
	if r := e.do("PUT", "/api/settings", base(map[string]any{"enabled": false, "seconds": 9, "level": "warning"}), csrf...); r.code != 200 || popups(r) != mine {
		t.Fatal("save:", r.code, string(r.raw))
	}
	if r := e.do("GET", "/api/settings", nil, "Cookie", c); popups(r) != mine || r.body["dim"] != 50.0 {
		t.Fatal("read back:", string(r.raw))
	}
	for _, bad := range []map[string]any{{"seconds": 2}, {"seconds": 31}, {"seconds": 5.5}, {"seconds": "5"}, {"level": "info"}, {"level": ""},
		{"enabled": "yes"}, {"more": true}} {
		if r := e.do("PUT", "/api/settings", base(bad), csrf...); r.code != 422 {
			t.Errorf("%v: %d %s", bad, r.code, r.raw)
		}
	}
	if r := e.do("GET", "/api/settings", nil, "Cookie", c); popups(r) != mine {
		t.Fatal("a refused request changed the settings:", popups(r))
	}
	// the limits themselves are fine
	for _, ok := range []int{3, 30} {
		if r := e.do("PUT", "/api/settings", base(map[string]any{"enabled": true, "seconds": ok, "level": "critical"}), csrf...); r.code != 200 {
			t.Errorf("seconds %d: %d", ok, r.code)
		}
	}
	// a request without them (as dim, blur and card_opacity): the defaults
	if r := e.do("PUT", "/api/settings", base(nil), csrf...); r.code != 200 || popups(r) != def {
		t.Fatal("without popups:", r.code, popups(r))
	}
	// the audit log says what was chosen
	var words []string
	for _, a := range e.auditEntries(viewer.Sub, "settings.update") {
		p, _ := a["params"].(map[string]any)
		w, _ := p["popups"].(string)
		words = append(words, w)
	}
	if got := strings.Join(words, " | "); !strings.Contains(got, "off") || !strings.Contains(got, "30 s, critical") || !strings.Contains(got, "5 s, critical") {
		t.Fatal("audit:", got)
	}
}
