package web

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/model"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/settings"
)

const (
	wallAPI = "/api/admin/signin-wallpaper"
	wallURL = "/branding/wallpaper"
)

// Real pictures of the three accepted types: a PNG and a JPEG made by the standard library, and the smallest
// WebP there is (1x1, lossless), which no standard library package writes.
func wallPictures(t *testing.T) (pngPic, jpg, webp []byte) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 18))
	for y := 0; y < 18; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 8), uint8(y * 14), 120, 255})
		}
	}
	var p, j bytes.Buffer
	if err := png.Encode(&p, img); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&j, img, nil); err != nil {
		t.Fatal(err)
	}
	return p.Bytes(), j.Bytes(), []byte("RIFF\x1a\x00\x00\x00WEBPVP8L\x0d\x00\x00\x00\x2f\x00\x00\x00\x10\x07\x10\x11\x11\x88\x88\xfe\x07\x00")
}

// wallAdmin is a panel with an admin's cookie and CSRF header, and without the upload limit of 6 a minute.
func wallAdmin(t *testing.T) (*env, []string) {
	t.Helper()
	e := setup(t)
	e.s.uploadRL = newRateLimiter(1000)
	return e, []string{"Cookie", e.cookie(controller(), 0), "X-Requested-With", "fairy"}
}

func (e *env) wallUpload(data []byte, ctype string, hdr ...string) resp {
	return e.raw(probe{method: "POST", path: wallAPI, body: data, ctype: ctype}, hdr...)
}

// The upload takes PNG, JPEG and WebP and nothing else, and it is the content that decides: not the type the
// browser sent, which says what the file was called.
func TestSignInWallpaperUploadTakesThreeTypesByContent(t *testing.T) {
	e, admin := wallAdmin(t)
	pngPic, jpg, webp := wallPictures(t)
	for _, ok := range []struct {
		name, declared, want string
		data                 []byte
	}{
		{"a PNG", "image/png", "image/png", pngPic},
		{"a JPEG", "image/jpeg", "image/jpeg", jpg},
		{"a WebP", "image/webp", "image/webp", webp},
		{"a PNG sent without a type", "application/octet-stream", "image/png", pngPic},
		{"a JPEG that was renamed to .png", "image/png", "image/jpeg", jpg},
		{"a WebP sent as text", "text/plain", "image/webp", webp},
	} {
		r := e.wallUpload(ok.data, ok.declared, admin...)
		if r.code != 201 || r.body["set"] != true || r.body["type"] != ok.want || r.body["bytes"] != float64(len(ok.data)) {
			t.Fatalf("%s: %d %s", ok.name, r.code, r.raw)
		}
		g := e.do("GET", wallURL, nil)
		if g.code != 200 || g.hdr.Get("Content-Type") != ok.want || !bytes.Equal(g.raw, ok.data) {
			t.Fatalf("%s: the public route answers %d %s, %d bytes", ok.name, g.code, g.hdr.Get("Content-Type"), len(g.raw))
		}
	}
	// the WebP is the stored picture now; nothing below may replace it
	before := e.do("GET", wallURL, nil).hdr.Get("ETag")
	html := []byte("<!doctype html><html><body><script>alert(document.cookie)</script></body></html>")
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><script>alert(1)</script></svg>`)
	noWidth := append([]byte{}, pngPic...)
	copy(noWidth[16:20], []byte{0, 0, 0, 0})
	for _, bad := range []struct {
		name, declared string
		data           []byte
		want           int
	}{
		{"an SVG", "image/svg+xml", svg, 415},
		{"an SVG renamed to .png", "image/png", svg, 415},
		{"an SVG with an XML head, as octet-stream", "application/octet-stream", append([]byte(`<?xml version="1.0"?>`), svg...), 415},
		{"HTML renamed to .png", "image/png", html, 415},
		{"HTML renamed to .jpg", "image/jpeg", html, 415},
		{"HTML as HTML", "text/html", html, 415},
		{"HTML after a PNG signature", "image/png", append(append([]byte{}, pngPic[:8]...), html...), 415},
		{"HTML after the JPEG magic bytes", "image/jpeg", append([]byte("\xff\xd8\xff"), html...), 415},
		{"a GIF", "image/gif", []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), 415},
		{"an AVIF", "image/avif", []byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00avifmif1miaf"), 415},
		{"text", "text/plain", []byte("hello"), 415},
		{"a form upload (multipart)", "multipart/form-data; boundary=x", append([]byte("--x\r\nContent-Disposition: form-data; name=\"f\"; filename=\"a.png\"\r\nContent-Type: image/png\r\n\r\n"), pngPic...), 415},
		{"a PNG signature alone", "image/png", pngPic[:8], 415},
		{"a PNG cut inside its header", "image/png", pngPic[:24], 415},
		{"a PNG without a width", "image/png", noWidth, 415},
		{"the three JPEG magic bytes", "image/jpeg", jpg[:3], 415},
		{"a JPEG cut inside its first segment", "image/jpeg", jpg[:8], 415},
		{"a WebP cut after RIFF....WEBP", "image/webp", webp[:12], 415},
		{"an empty body", "image/png", []byte{}, 400},
		{"one byte over the limit", "image/png", append(append([]byte{}, pngPic...), make([]byte, settings.MaxBackgroundBytes+1-len(pngPic))...), 413},
	} {
		r := e.wallUpload(bad.data, bad.declared, admin...)
		if r.code != bad.want {
			t.Errorf("%s: %d %s (want %d)", bad.name, r.code, r.raw, bad.want)
		}
		if g := e.do("GET", wallURL, nil); g.code != 200 || g.hdr.Get("ETag") != before || !bytes.Equal(g.raw, webp) {
			t.Fatalf("%s changed the stored picture: %d %s", bad.name, g.code, g.hdr.Get("Content-Type"))
		}
	}
	// exactly the limit is taken (a PNG followed by padding is still a PNG to every reader)
	full := append(append([]byte{}, pngPic...), make([]byte, settings.MaxBackgroundBytes-len(pngPic))...)
	if r := e.wallUpload(full, "image/png", admin...); r.code != 201 || r.body["bytes"] != float64(settings.MaxBackgroundBytes) {
		t.Fatalf("a picture of exactly the limit: %d %s", r.code, r.raw)
	}
	// a body without a length (chunked) is refused: it would be read ahead in part and stored cut off
	req, _ := http.NewRequest("POST", e.srv.URL+wallAPI, io.MultiReader(bytes.NewReader(pngPic)))
	for i := 0; i+1 < len(admin); i += 2 {
		req.Header.Set(admin[i], admin[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 411 {
		t.Errorf("an upload without a Content-Length: %d", res.StatusCode)
	}
	if g := e.do("GET", wallURL, nil); !bytes.Equal(g.raw, full) {
		t.Error("the upload without a length changed the stored picture")
	}
	// uploads are limited like a user's background uploads (6 a minute)
	e.s.uploadRL = newRateLimiter(uploadsPerMin)
	limited := false
	for i := 0; i <= uploadsPerMin && !limited; i++ {
		limited = e.wallUpload(pngPic, "image/png", admin...).code == 429
	}
	if !limited {
		t.Error("uploads are not rate limited")
	}
}

// The public route: no sign-in, one picture or 404, the headers that make a browser treat it as a picture and
// nothing else, and nothing in the request says which file is sent.
func TestSignInWallpaperPublicRoute(t *testing.T) {
	e, admin := wallAdmin(t)
	pngPic, jpg, _ := wallPictures(t)
	none := e.do("GET", wallURL, nil)
	if none.code != 404 || none.body["detail"] != "no sign-in wallpaper" || none.hdr.Get("Content-Type") != "application/json" ||
		none.hdr.Get("X-Content-Type-Options") != "nosniff" || none.hdr.Get("Set-Cookie") != "" {
		t.Fatalf("with no picture set: %d %s %v", none.code, none.raw, none.hdr)
	}
	// a user's own background is not the sign-in wallpaper: with one uploaded, the public route still has nothing
	if r := e.raw(probe{method: "POST", path: "/api/settings/background", body: jpg, ctype: "image/jpeg"}, admin...); r.code != 201 {
		t.Fatal("background upload", r.code, string(r.raw))
	}
	if r := e.do("GET", wallURL, nil); r.code != 404 {
		t.Fatal("a user's background is handed out as the sign-in wallpaper:", r.code)
	}
	up := e.wallUpload(pngPic, "image/png", admin...)
	version, _ := up.body["version"].(string)
	if up.code != 201 || len(version) != 16 {
		t.Fatal(up.code, string(up.raw))
	}
	g := e.do("GET", wallURL, nil) // nobody is signed in
	for name, want := range map[string]string{
		"Content-Type":                 "image/png",
		"X-Content-Type-Options":       "nosniff",
		"Content-Security-Policy":      "default-src 'none'",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cache-Control":                "no-cache",
		"ETag":                         `"` + version + `"`,
		"Content-Length":               itoa(int64(len(pngPic))),
		"X-Frame-Options":              "DENY",
		"Set-Cookie":                   "",
	} {
		if got := g.hdr.Get(name); got != want {
			t.Errorf("%s: %q (want %q)", name, got, want)
		}
	}
	if g.code != 200 || !bytes.Equal(g.raw, pngPic) {
		t.Fatalf("the picture: %d, %d bytes", g.code, len(g.raw))
	}
	if r := e.do("GET", wallURL, nil, "If-None-Match", g.hdr.Get("ETag")); r.code != 304 || len(r.raw) != 0 {
		t.Error("If-None-Match with the ETag:", r.code, len(r.raw))
	}
	if r := e.do("GET", wallURL+"?v="+version, nil); r.code != 200 || r.hdr.Get("Cache-Control") != cacheImmutable || !bytes.Equal(r.raw, pngPic) {
		t.Error("the address with the picture's version:", r.code, r.hdr.Get("Cache-Control"))
	}
	if r := e.do("GET", wallURL+"?v=0000000000000000", nil); r.code != 200 || r.hdr.Get("Cache-Control") != "no-cache" || !bytes.Equal(r.raw, pngPic) {
		t.Error("an address with another version must not be kept:", r.code, r.hdr.Get("Cache-Control"))
	}
	if r := e.do("HEAD", wallURL, nil); r.code != 200 || len(r.raw) != 0 || r.hdr.Get("Content-Type") != "image/png" {
		t.Error("HEAD:", r.code, len(r.raw))
	}
	// a session, a token or a header changes nothing; the answer never sets a cookie
	if r := e.do("GET", wallURL, nil, admin[0], admin[1], "Authorization", "Bearer nope", "Accept", "text/html"); r.code != 200 ||
		r.hdr.Get("Content-Type") != "image/png" || r.hdr.Get("Set-Cookie") != "" || !bytes.Equal(r.raw, pngPic) {
		t.Error("with a cookie and other headers:", r.code, r.hdr)
	}
	// nothing in the request names a file. Parameters are not read; other paths are not this route.
	secret := []byte("NOT-FOR-THE-PUBLIC")
	for _, name := range []string{"settings.json", "sessions.json", "backgrounds/other"} {
		p := filepath.Join(e.s.Cfg.DataDir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, secret, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{"?file=../settings.json", "?path=/etc/passwd", "?name=other", "?v=../settings.json", "?type=text/html",
		"?v=" + version + "&file=settings.json", "?download=1", "?v=" + version + "&v=x"} {
		r := e.do("GET", wallURL+q, nil)
		if r.code != 200 || r.hdr.Get("Content-Type") != "image/png" || !bytes.Equal(r.raw, pngPic) {
			t.Errorf("%s: %d %s, %d bytes", q, r.code, r.hdr.Get("Content-Type"), len(r.raw))
		}
	}
	for _, p := range []string{wallURL + "/", wallURL + "/x", wallURL + ".png", wallURL + "/../settings.json", wallURL + "/../../etc/passwd",
		wallURL + "/..%2fsettings.json", wallURL + "%2f..%2fsettings.json", wallURL + "/%2e%2e/settings.json", wallURL + "%00.png",
		"/branding/", "/branding/signin-wallpaper", "/branding/backgrounds/signin-wallpaper", "/branding/settings.json",
		"/branding/../backgrounds/other", "/static/../backgrounds/signin-wallpaper", "/backgrounds/signin-wallpaper", "//branding//wallpaper/..",
		"/branding/WALLPAPER", "/branding/wallpaper;x"} {
		req, _ := http.NewRequest("GET", e.srv.URL, nil)
		req.URL.Opaque = "//" + req.URL.Host + p // sent as written, not cleaned by the client
		r, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(p, err)
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if bytes.Contains(raw, secret) || r.StatusCode == 200 || strings.HasPrefix(r.Header.Get("Content-Type"), "image/") {
			t.Errorf("%s: %d %s %.60q", p, r.StatusCode, r.Header.Get("Content-Type"), raw)
		}
		if loc := r.Header.Get("Location"); loc != "" { // the router's own tidying of a path with dots: followed, still nothing
			if f := e.do("GET", loc, nil); bytes.Contains(f.raw, secret) || (f.code == 200 && loc != wallURL) {
				t.Errorf("%s -> %s: %d %.60q", p, loc, f.code, f.raw)
			}
		}
	}
	// it is a route for reading: writing to it does nothing
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		if r := e.raw(probe{method: m, path: wallURL, body: jpg, ctype: "image/jpeg"}, admin...); r.code != 404 && r.code != 405 {
			t.Errorf("%s %s: %d", m, wallURL, r.code)
		}
	}
	if r := e.do("GET", wallURL, nil); !bytes.Equal(r.raw, pngPic) {
		t.Error("a write to the public route changed the picture")
	}
	// at most 4 downloads at a time for one address: the next is told to come back, and the page shows no picture
	var held []func()
	for i := 0; i < maxWallpapersPerIP; i++ {
		rel, ok := e.s.wall.acquire(testIP)
		if !ok {
			t.Fatal("slot", i)
		}
		held = append(held, rel)
	}
	if r := e.do("GET", wallURL, nil); r.code != 429 || r.hdr.Get("Retry-After") == "" || strings.HasPrefix(r.hdr.Get("Content-Type"), "image/") {
		t.Error("a fifth download at the same time:", r.code)
	}
	for _, rel := range held {
		rel()
	}
	if r := e.do("GET", wallURL, nil); r.code != 200 {
		t.Error("after they ended:", r.code)
	}
}

// Only an admin's browser session changes it, with the CSRF header and from the panel's own origin; a refused
// request stores nothing.
func TestSignInWallpaperIsForAdmins(t *testing.T) {
	e, admin := wallAdmin(t)
	pngPic, _, _ := wallPictures(t)
	csrf := []string{"X-Requested-With", "fairy"}
	vee := model.User{Sub: "u7", Name: model.P("Vee"), Email: model.P("v@example.com"), Groups: []string{"viewers"}}
	vck := e.cookie(vee, 0)
	if _, _, err := e.s.Access.SetRole("u7", "viewer", []string{}, "test", false); err != nil {
		t.Fatal(err)
	}
	waiting := e.cookie(model.User{Sub: "u8", Name: model.P("Pat"), Groups: []string{"viewers"}}, 0)
	ro, _, _ := e.s.Tokens.Create("ha", "system", "read", "cli:test", 0)
	ctl, _, _ := e.s.Tokens.Create("ops", "system", "control", "cli:test", 0)
	for name, c := range map[string]struct {
		hdr  []string
		want int
	}{
		"nobody":                           {csrf, 401},
		"a viewer":                         {append([]string{"Cookie", vck}, csrf...), 403},
		"someone waiting for access":       {append([]string{"Cookie", waiting}, csrf...), 403},
		"a read token":                     {[]string{"Authorization", "Bearer " + ro}, 403},
		"a control token":                  {[]string{"Authorization", "Bearer " + ctl}, 403},
		"an admin without the CSRF header": {[]string{"Cookie", admin[1]}, 403},
		"an admin from another origin":     {append([]string{"Origin", "https://evil.example"}, admin...), 403},
		"an admin, Sec-Fetch-Site":         {append([]string{"Sec-Fetch-Site", "cross-site"}, admin...), 403},
		"an admin, a form post":            {[]string{"Cookie", admin[1], "Content-Type", "multipart/form-data; boundary=x"}, 403},
	} {
		if r := e.wallUpload(pngPic, "image/png", c.hdr...); r.code != c.want {
			t.Errorf("upload by %s: %d %s (want %d)", name, r.code, r.raw, c.want)
		}
		if r := e.raw(probe{method: "DELETE", path: wallAPI}, c.hdr...); r.code != c.want {
			t.Errorf("removal by %s: %d %s (want %d)", name, r.code, r.raw, c.want)
		}
		if r := e.do("GET", wallURL, nil); r.code != 404 {
			t.Fatalf("after the upload by %s a picture is set", name)
		}
	}
	// the state is for admins too (a viewer's page never asks)
	for name, hdr := range map[string][]string{"a viewer": {"Cookie", vck}, "someone waiting": {"Cookie", waiting}, "a token": {"Authorization", "Bearer " + ctl}} {
		if r := e.do("GET", wallAPI, nil, hdr...); r.code != 403 {
			t.Errorf("state for %s: %d", name, r.code)
		}
	}
	if r := e.do("GET", wallAPI, nil); r.code != 401 {
		t.Error("state for nobody:", r.code)
	}
	if got := e.auditEntries("u7", "auth.denied"); len(got) == 0 {
		t.Error("a viewer's refused change is not in the audit log")
	}
	if got := e.auditEntries("", "settings.signin_wallpaper_upload"); len(got) != 0 {
		t.Error("a refused upload is logged as a change:", got)
	}
	// the admin: with a sign-in of any age (no confirmation is asked, as for the other settings)
	old := append([]string{"Cookie", e.staleCookie(controller(), 24*60*60*1e9)}, csrf...)
	if r := e.wallUpload(pngPic, "image/png", old...); r.code != 201 {
		t.Fatalf("an admin: %d %s", r.code, r.raw)
	}
	st := e.do("GET", wallAPI, nil, "Cookie", admin[1])
	if st.code != 200 || st.body["set"] != true || st.body["type"] != "image/png" || st.body["bytes"] != float64(len(pngPic)) ||
		st.body["max_bytes"] != float64(settings.MaxBackgroundBytes) || st.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("state: %d %s", st.code, st.raw)
	}
}

// Removal, the audit log, and a restart: the picture is the panel's state, kept in the data directory.
func TestSignInWallpaperRemovalAuditAndRestart(t *testing.T) {
	e, admin := wallAdmin(t)
	pngPic, jpg, _ := wallPictures(t)
	file := filepath.Join(e.s.Cfg.DataDir, "backgrounds", "signin-wallpaper")
	if r := e.raw(probe{method: "DELETE", path: wallAPI}, admin...); r.code != 200 || r.body["set"] != false {
		t.Fatal("removing nothing:", r.code, string(r.raw))
	}
	if got := e.auditEntries("u2", "settings.signin_wallpaper_delete"); len(got) != 0 {
		t.Error("removing nothing is logged as a change")
	}
	if r := e.wallUpload(pngPic, "image/png", admin...); r.code != 201 {
		t.Fatal(r.code, string(r.raw))
	}
	if r := e.wallUpload(jpg, "image/jpeg", admin...); r.code != 201 { // a change: the JPEG replaces the PNG
		t.Fatal(r.code, string(r.raw))
	}
	ups := e.auditEntries("u2", "settings.signin_wallpaper_upload")
	if len(ups) != 2 {
		t.Fatalf("%d upload entries in the audit log, want 2", len(ups))
	}
	last := ups[0] // newest first
	params, _ := last["params"].(map[string]any)
	if last["outcome"] != "ok" || last["actor"] != "n@example.com (unverified)" || last["source"] != "ui" || last["ip"] != testIP ||
		params["type"] != "image/jpeg" || params["bytes"] != float64(len(jpg)) {
		t.Errorf("the audit entry of an upload: %v", last)
	}
	names, _ := os.ReadDir(filepath.Dir(file))
	st, err := os.Stat(file)
	if err != nil || len(names) != 1 {
		t.Fatalf("the data directory: %v %v", names, err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("the file's mode: %v", st.Mode())
	}
	etag := e.do("GET", wallURL, nil).hdr.Get("ETag")

	// a restart: a new server on the same data directory
	restart := func() *env {
		s := New(e.s.Cfg, config.Secrets{SessionSecret: strings.Repeat("s", 48)}, e.s.Poller, nil, "test")
		srv := httptest.NewServer(s.Handler())
		t.Cleanup(srv.Close)
		return &env{t: t, s: s, srv: srv, b: e.b}
	}
	e2 := restart()
	g := e2.do("GET", wallURL, nil)
	if g.code != 200 || g.hdr.Get("Content-Type") != "image/jpeg" || g.hdr.Get("ETag") != etag || !bytes.Equal(g.raw, jpg) {
		t.Fatalf("after a restart: %d %s", g.code, g.hdr.Get("Content-Type"))
	}
	if r := e2.do("GET", wallAPI, nil, "Cookie", admin[1]); r.code != 200 || r.body["set"] != true || r.body["type"] != "image/jpeg" {
		t.Fatalf("the state after a restart: %d %s", r.code, r.raw)
	}
	// removal
	if r := e2.raw(probe{method: "DELETE", path: wallAPI}, admin...); r.code != 200 || r.body["set"] != false || r.body["version"] != nil {
		t.Fatal("removal:", r.code, string(r.raw))
	}
	if r := e2.do("GET", wallURL, nil); r.code != 404 {
		t.Fatal("after the removal:", r.code)
	}
	if r := e2.do("GET", wallURL, nil, "If-None-Match", etag); r.code != 404 {
		t.Error("after the removal a browser that has the old picture is told 304:", r.code)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("the file is still in the data directory")
	}
	del := e2.auditEntries("u2", "settings.signin_wallpaper_delete")
	if len(del) != 1 || del[0]["outcome"] != "ok" {
		t.Errorf("the audit entry of the removal: %v", del)
	}
	if e3 := restart(); e3.do("GET", wallURL, nil).code != 404 {
		t.Error("the picture is back after a restart")
	}
	// a file put into the data directory by hand is checked like an upload before it is ever handed out
	if err := os.WriteFile(file, []byte("<html><script>alert(1)</script></html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := restart().do("GET", wallURL, nil); r.code != 404 {
		t.Errorf("HTML in the data directory is handed out: %d %s", r.code, r.hdr.Get("Content-Type"))
	}
}
