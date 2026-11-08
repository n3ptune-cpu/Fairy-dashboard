package web

import (
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
)

func TestIndexReferencesVersionedAssets(t *testing.T) {
	e := setup(t)
	v := e.s.assetV
	if !strings.HasPrefix(v, "test-") || len(v) != len("test-")+8 {
		t.Fatal("asset version", v)
	}
	r := e.do("GET", "/", nil)
	if r.code != 200 || r.hdr.Get("Cache-Control") != "no-cache" {
		t.Fatal("index", r.code, r.hdr.Get("Cache-Control"))
	}
	page := string(r.raw)
	for _, want := range []string{`href="/static/app.css?v=` + v + `"`, `src="/static/app.js?v=` + v + `"`} {
		if !strings.Contains(page, want) {
			t.Fatal("index.html lacks", want)
		}
	}
	if strings.Contains(page, assetVersionPlaceholder) || strings.Contains(page, `"/static/app.js"`) {
		t.Fatal("unversioned asset URL left in index.html")
	}
	if r.hdr.Get("Content-Security-Policy") != CSP {
		t.Fatal("CSP", r.hdr.Get("Content-Security-Policy"))
	}
	// the raw template is not served
	if r := e.do("GET", "/static/index.html", nil); r.code != 404 {
		t.Fatal("raw index.html", r.code)
	}
}

func TestStaticCacheControl(t *testing.T) {
	e := setup(t)
	v := e.s.assetV
	for _, c := range []struct {
		path, cache string
	}{
		{"/static/app.js?v=" + v, "public, max-age=31536000, immutable"},
		{"/static/app.css?v=" + v, "public, max-age=31536000, immutable"},
		{"/static/app.js", "no-cache"},
		{"/static/app.css", "no-cache"},
		{"/static/app.js?v=old-1234abcd", "no-cache"},
		{"/static/app.js?v=", "no-cache"},
	} {
		r := e.do("GET", c.path, nil)
		if r.code != 200 || r.hdr.Get("Cache-Control") != c.cache {
			t.Fatal(c.path, r.code, r.hdr.Get("Cache-Control"))
		}
	}
	if r := e.do("GET", "/static/nope.js?v="+v, nil); r.code != 404 || r.hdr.Get("Cache-Control") == "public, max-age=31536000, immutable" {
		t.Fatal("missing file", r.code, r.hdr.Get("Cache-Control"))
	}
}

func TestAssetVersion(t *testing.T) {
	a := fstest.MapFS{"app.js": {Data: []byte("1")}}
	b := fstest.MapFS{"app.js": {Data: []byte("2")}}
	if assetVersion("dev", a) == assetVersion("dev", b) {
		t.Fatal("same version for different files")
	}
	a2 := fstest.MapFS{"app.js": {Data: []byte("1")}}
	if assetVersion("dev", a) != assetVersion("dev", a2) || assetVersion("1", a) == assetVersion("2", a) {
		t.Fatal("not deterministic or ignores the build version")
	}
	v := assetVersion("1 2&x", a)
	if dec, _ := url.QueryUnescape(v); !strings.HasPrefix(dec, "1 2&x-") || strings.ContainsAny(v, " &") {
		t.Fatal("not query-escaped", v)
	}
}
