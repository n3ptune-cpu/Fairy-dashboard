package web

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// doRaw is e.do with the body exactly as given.
func (e *env) doRaw(method, path, body string, hdr ...string) resp {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	return resp{code: r.StatusCode, raw: raw, hdr: r.Header}
}

// T-30: the "Confirm it's you" dialog and the audit entries of the step-up say what the panel will run
// (describeAction), read from the body's fields by their exact names. The handler's decoder is more
// generous: it takes a field under any spelling of its name and, of a field given twice, the last one. So
// {"role":"pending","ROLE":"admin"} was shown and audited as "role: pending" and made an admin. On the
// routes that can be kept for a step-up, a body must now say each thing once, under its exact name, and
// nothing may follow it; anything else is refused before it is described, kept or run.
func TestABodyThatSaysAFieldTwiceIsRefusedOnStepUpRoutes(t *testing.T) {
	e := setup(t)
	fresh := e.cookie(controller(), 0)              // inside the confirmation window: a request runs at once
	stale := e.staleCookie(controller(), time.Hour) // outside it: a request is kept for "Confirm it's you"
	e.signIn(person("u5"))
	hdr := func(ck string) []string { return []string{"Cookie", ck, "X-Requested-With", "fairy"} }
	role := func() string { return e.s.Access.Load().Users["u5"].Role }

	for _, body := range []string{
		`{"role":"pending","ROLE":"admin"}`,      // another spelling, last
		`{"ROLE":"admin","role":"pending"}`,      // another spelling, first
		`{"role":"pending","role":"admin"}`,      // the same name twice
		`{"ROLE":"admin"}`,                       // only another spelling: the description would not name the role
		`{"role":"pending","Role":"admin"}`,      // (the second auditor's form)
		`{"role":"pending"}{"role":"admin"}`,     // something after the value
		`{"role":"pending"} x`,                   //
		`{"role":"pending","rol\u0065":"admin"}`, // the same name, written with an escape
		`{"\u017fites":["a"],"role":"admin"}`,    // a letter that folds to "s"
	} {
		for _, ck := range []string{stale, fresh} {
			r := e.doRaw("PUT", "/api/admin/users/u5", body, hdr(ck)...)
			if r.code != 422 || strings.Contains(string(r.raw), "pending_id") {
				t.Fatalf("%s: %d %s", body, r.code, r.raw)
			}
			if got := role(); got == "admin" {
				t.Fatalf("%s made the account %s", body, got)
			}
		}
		// nothing of it was kept: the dialog's Continue finds nothing
		if r := e.do("POST", stepUpPath, map[string]any{"pending_id": "x", "next": "/"}, append(hdr(stale), "Content-Type", "application/json")...); r.code != 409 {
			t.Fatalf("after %s something is kept: %d %s", body, r.code, r.raw)
		}
	}
	// a field twice inside a nested object, and a site name twice
	if r := e.doRaw("POST", "/api/admin/hosts", `{"name":"x1","os":"linux","options":{"power_control":false,"power_control":true}}`, hdr(fresh)...); r.code != 422 {
		t.Fatalf("a nested field twice: %d %s", r.code, r.raw)
	}
	if r := e.doRaw("POST", "/api/admin/sites", `{"name":"Shown","NAME":"Real"}`, hdr(fresh)...); r.code != 422 {
		t.Fatalf("a site name twice: %d %s", r.code, r.raw)
	}
	if d := e.s.Access.Load(); len(d.Sites) != 0 && strings.Contains(strings.Join(d.Sites, ","), "Real") {
		t.Fatal("the site was created")
	}

	// what the page sends still works, kept and at once, and is described as what it is
	r := e.doRaw("PUT", "/api/admin/users/u5", `{"role":"viewer","sites":[]}`, hdr(stale)...)
	if r.code != 401 || !strings.Contains(string(r.raw), `(role: viewer)`) {
		t.Fatalf("a normal body outside the window: %d %s", r.code, r.raw)
	}
	if r := e.doRaw("PUT", "/api/admin/users/u5", ` {"role":"viewer", "sites":[]} `+"\n", hdr(fresh)...); r.code != 200 || role() != "viewer" {
		t.Fatalf("a normal body inside the window: %d %s (role %s)", r.code, r.raw, role())
	}
	// routes that are never kept for a step-up decode as before (a user's own settings)
	if r := e.doRaw("PUT", "/api/settings", `{"DIM":40}`, hdr(fresh)...); r.code != 200 {
		t.Fatalf("PUT /api/settings: %d %s", r.code, r.raw)
	}
}

func TestStrictBody(t *testing.T) {
	for body, want := range map[string]bool{
		``:                                    true,
		`  `:                                  true,
		`{}`:                                  true,
		`{"action":"shutdown","delay_s":60}`:  true,
		`{"a":{"B":1,"c":[{"d":1},{"d":2}]}}`: true, // names inside are compared with each other only
		`[1,2]`:                               true, // not an object: the handler refuses it
		`{"a":1,"a":2}`:                       false,
		`{"a":1,"A":2}`:                       false,
		`{"A":1}`:                             false,
		`{"a-b":1}`:                           false,
		`{"a":{"b":1,"B":2}}`:                 false,
		`{"a":[{"b":1,"b":2}]}`:               false,
		`{"a":1}{"a":2}`:                      false,
		`{"a":1} 1`:                           false,
		`{"a":1`:                              false,
		`{"a":1,"\u0061":2}`:                  false,
	} {
		if got := strictBody([]byte(body)); got != want {
			t.Errorf("strictBody(%s) = %v, want %v", body, got, want)
		}
	}
}
