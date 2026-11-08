package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// assetVersionPlaceholder is replaced in index.html with the asset version (see assetVersion).
const assetVersionPlaceholder = "{{ASSET_VERSION}}"

// Cache-Control of the static assets: a URL with the current ?v= never changes (a new build has a new v), so
// the browser and Cloudflare may keep it for a year; any other URL (no v, or an old one) must be revalidated, so
// no stale copy can ever be cached under it.
const (
	cacheImmutable = "public, max-age=31536000, immutable"
	cacheNoCache   = "no-cache"
)

// assetVersion is the ?v= of /static/* URLs: the build version plus a short hash of the embedded files, so even
// two local builds that are both "dev" get different URLs when the UI changed. Query-escaped.
func assetVersion(version string, static fs.FS) string {
	h := sha256.New()
	var names []string
	_ = fs.WalkDir(static, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	sort.Strings(names)
	for _, n := range names {
		b, _ := fs.ReadFile(static, n)
		sum := sha256.Sum256(b)
		h.Write([]byte(n))
		h.Write(sum[:])
	}
	if version == "" {
		version = "dev"
	}
	return url.QueryEscape(version + "-" + hex.EncodeToString(h.Sum(nil))[:8])
}

// renderIndex returns index.html with the asset version filled in.
func renderIndex(static fs.FS, v string) []byte {
	b, _ := fs.ReadFile(static, "index.html")
	return []byte(strings.ReplaceAll(string(b), assetVersionPlaceholder, v))
}

// staticURL is the versioned URL of a static asset, e.g. staticURL("app.css") = "/static/app.css?v=...".
func (s *Server) staticURL(name string) string { return "/static/" + name + "?v=" + s.assetV }

// serveStatic serves the embedded files (no directory listings) with the Cache-Control described above.
func (s *Server) serveStatic(static fs.FS) http.Handler {
	files := http.FileServerFS(static)
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") { // files only, no directory listings
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "index.html" { // only GET / serves the page (with the version filled in)
			http.NotFound(w, r)
			return
		}
		if url.QueryEscape(r.URL.Query().Get("v")) == s.assetV {
			w.Header().Set("Cache-Control", cacheImmutable)
		} else {
			w.Header().Set("Cache-Control", cacheNoCache)
		}
		files.ServeHTTP(w, r)
	}))
}
