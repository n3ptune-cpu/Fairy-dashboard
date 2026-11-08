package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/settings"
)

// MaxLogoBytes is the largest logo file accepted (branding.logo).
const MaxLogoBytes = 1 << 20

// logoTypes are the accepted logo formats: raster only (an SVG can carry script).
var logoTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}

// brandLogo is the custom logo, read once at startup from the path in config.yaml (never from a request).
type brandLogo struct {
	data []byte
	mime string
	etag string
}

// loadLogo reads branding.logo. An unset path is no logo; a missing, unreadable, too large or non-raster file
// is a warning (the built-in logo is shown), never a reason not to start.
func loadLogo(path string) *brandLogo {
	if path == "" {
		return nil
	}
	l, err := readLogo(path)
	if err != nil {
		slog.Warn("branding.logo not used: showing the built-in logo", "path", path, "error", err)
		return nil
	}
	slog.Info("custom logo loaded", "path", path, "type", l.mime, "bytes", len(l.data))
	return l
}

func readLogo(path string) (*brandLogo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxLogoBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxLogoBytes {
		return nil, fmt.Errorf("larger than %d KiB", MaxLogoBytes>>10)
	}
	mime := settings.Sniff(data)
	if !logoTypes[mime] {
		return nil, errors.New("not a PNG, JPEG, WebP or GIF image (SVG is not accepted)")
	}
	sum := sha256.Sum256(data)
	return &brandLogo{data: data, mime: mime, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}, nil
}

// serveLogo is GET /branding/logo: the configured logo, no sign-in needed (it is shown on the sign-in page and
// as the favicon). It takes no parameters and only ever serves the file read at startup.
func (s *Server) serveLogo(w http.ResponseWriter, r *http.Request) {
	l := s.logo
	if l == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"detail":"no custom logo"}`))
		return
	}
	h := w.Header()
	h.Set("Content-Type", l.mime)
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("Cache-Control", "public, max-age=3600")
	h.Set("ETag", l.etag)
	if r.Header.Get("If-None-Match") == l.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(l.data)
}
