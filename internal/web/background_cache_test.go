package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
)

// r3-p3-01 (audit 5, round 3). Two people use one browser profile. The address of an uploaded background was
// "/api/settings/background?v=1" for nearly everyone and the answer's only validator was the file's time. The
// browser, holding Alice's picture under that address, asked with Bob's session "changed since <the time of
// Alice's file>?", and when Bob's own file was the older one the panel said 304: Bob was shown Alice's picture.
//
// Now the validator is an ETag made from the account and the file, there is no Last-Modified, and the address
// carries the same tag. Whatever validator of another account a browser asks with, the answer is the asking
// user's own picture, whole.
func TestBackgroundIsNeverAnotherUsers(t *testing.T) {
	e := setup(t)
	alice := model.User{Sub: "u-alice", Name: model.P("Alice"), Groups: []string{"viewers"}}
	bob := model.User{Sub: "u-bob", Name: model.P("Bob"), Groups: []string{"viewers"}}
	ca, cb := e.cookie(alice, 0), e.cookie(bob, 0)
	for _, u := range []model.User{alice, bob} {
		if _, _, err := e.s.Access.SetRole(u.Sub, "viewer", []string{}, "test", true); err != nil {
			t.Fatal(err)
		}
	}
	red, blue := pngOfSize(t, 3000), pngOfSize(t, 3100) // Alice's and Bob's; Bob's is the older file
	e.uploadBackground(cb, blue)
	bobFile, _ := e.s.Settings.BackgroundPath(bob.Sub)
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(bobFile, old, old); err != nil {
		t.Fatal(err)
	}
	e.uploadBackground(ca, red)
	get := func(cookie string, hdr ...string) resp {
		return e.do("GET", "/api/settings/background?v=1", nil, append([]string{"Cookie", cookie}, hdr...)...)
	}
	a, b := get(ca), get(cb)
	if a.code != 200 || b.code != 200 || !bytes.Equal(a.raw, red) || !bytes.Equal(b.raw, blue) {
		t.Fatalf("own pictures: %d %d", a.code, b.code)
	}
	ta, tb := a.hdr.Get("ETag"), b.hdr.Get("ETag")
	if !regexp.MustCompile(`^"[0-9a-f]{24}"$`).MatchString(ta) || !regexp.MustCompile(`^"[0-9a-f]{24}"$`).MatchString(tb) || ta == tb {
		t.Fatalf("each answer needs its own ETag: %q %q", ta, tb)
	}
	for _, r := range []resp{a, b} {
		if r.hdr.Get("Last-Modified") != "" || r.hdr.Get("Cache-Control") != "private, no-cache" || !strings.Contains(r.hdr.Get("Vary"), "Cookie") {
			t.Fatalf("headers: Last-Modified %q, Cache-Control %q, Vary %q", r.hdr.Get("Last-Modified"), r.hdr.Get("Cache-Control"), r.hdr.Get("Vary"))
		}
	}
	// the auditor's transcript: Bob's session asks with the time of Alice's (newer) copy
	later := time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)
	if r := get(cb, "If-Modified-Since", later); r.code != 200 || !bytes.Equal(r.raw, blue) {
		t.Fatalf("Bob asking with the time of Alice's copy: %d, %d bytes (304 shows him Alice's picture)", r.code, len(r.raw))
	}
	// what a browser that holds Alice's copy asks with from now on: her ETag
	if r := get(cb, "If-None-Match", ta); r.code != 200 || !bytes.Equal(r.raw, blue) {
		t.Fatalf("Bob asking with Alice's ETag: %d, %d bytes", r.code, len(r.raw))
	}
	if r := get(cb, "If-None-Match", ta, "If-Modified-Since", later); r.code != 200 || !bytes.Equal(r.raw, blue) {
		t.Fatalf("Bob asking with both: %d", r.code)
	}
	// his own validator: not sent again
	if r := get(cb, "If-None-Match", tb); r.code != 304 || len(r.raw) != 0 || r.hdr.Get("ETag") != tb {
		t.Fatalf("Bob asking with his own ETag: %d, %d bytes", r.code, len(r.raw))
	}
	// the settings carry the tag for the page's address; it is the ETag, and it is the panel's, not the request's
	tagOf := func(cookie string) string {
		r := e.do("GET", "/api/settings", nil, "Cookie", cookie)
		bg, _ := r.body["background"].(map[string]any)
		tag, _ := bg["tag"].(string)
		return tag
	}
	if got := tagOf(cb); `"`+got+`"` != tb {
		t.Fatalf("Bob's settings: tag %q, ETag %s", got, tb)
	}
	put := e.do("PUT", "/api/settings", map[string]any{"theme": "black", "background": map[string]any{"kind": "upload", "url": nil, "version": 1,
		"tag": strings.Trim(ta, `"`)}, "dim": 50, "blur": 10, "card_opacity": 70}, "Cookie", cb, "X-Requested-With", "fairy", "Content-Type", "application/json")
	if bg, _ := put.body["background"].(map[string]any); put.code != 200 || `"`+bg["tag"].(string)+`"` != tb {
		t.Fatalf("a save that sends another tag: %d %s", put.code, put.raw)
	}
	raw, err := os.ReadFile(filepath.Join(e.s.Cfg.DataDir, "settings.json"))
	if err != nil || bytes.Contains(raw, []byte(`"tag"`)) {
		t.Fatalf("the tag is not a setting and must not be stored: %v %s", err, raw)
	}
	// a new upload is another file: another tag, and the old one is no longer "not changed"
	e.uploadBackground(cb, red)
	if now := tagOf(cb); now == "" || `"`+now+`"` == tb {
		t.Fatalf("after a new upload the tag is %q (was %s)", now, tb)
	}
	if r := get(cb, "If-None-Match", tb); r.code != 200 || !bytes.Equal(r.raw, red) {
		t.Fatalf("asking with the tag of the picture before: %d", r.code)
	}
	// the same picture, the same time and the same size for both accounts: the tags still differ (the account is in them)
	st, _ := os.Stat(bobFile)
	aliceFile, _ := e.s.Settings.BackgroundPath(alice.Sub)
	if err := os.Chtimes(aliceFile, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	a2, b2 := get(ca), get(cb)
	if a2.hdr.Get("ETag") == b2.hdr.Get("ETag") || a2.hdr.Get("ETag") == "" || !bytes.Equal(a2.raw, b2.raw) {
		t.Fatalf("two accounts, files of the same content, time and size: ETags %q %q", a2.hdr.Get("ETag"), b2.hdr.Get("ETag"))
	}
	if r := get(cb, "If-None-Match", a2.hdr.Get("ETag")); r.code != 200 {
		t.Fatalf("Bob asking with Alice's ETag for the same picture: %d", r.code)
	}
	// no upload, no tag
	if r := e.do("DELETE", "/api/settings/background", nil, "Cookie", cb, "X-Requested-With", "fairy"); r.code != 200 || strings.Contains(string(r.raw), `"tag"`) {
		t.Fatalf("after the picture is removed: %d %s", r.code, r.raw)
	}
	var page struct {
		Background map[string]any `json:"background"`
	}
	_ = json.Unmarshal(e.do("GET", "/api/settings", nil, "Cookie", cb).raw, &page)
	if _, has := page.Background["tag"]; has {
		t.Fatal("settings without an upload carry a tag")
	}
	// the page builds the address from the tag
	js, _ := os.ReadFile("static/app.js")
	if !bytes.Contains(js, []byte("/api/settings/background?v=${enc(s.background.tag || s.background.version)}")) {
		t.Fatal("app.js: the background's address does not carry the tag")
	}
}
