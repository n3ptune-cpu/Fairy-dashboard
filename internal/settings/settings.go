// Package settings stores per-user UI settings (JSON file keyed by OIDC sub) and background-image uploads.
package settings

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// MaxBackgroundBytes is the largest background image accepted (8 MiB).
const MaxBackgroundBytes = 8 * 1024 * 1024

// ImageTypes maps the accepted image types to file extensions: raster formats only. SVG is refused on purpose: it
// can carry script.
var ImageTypes = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif", "image/avif": "avif"}

var typeOrder = []string{"image/png", "image/jpeg", "image/webp", "image/gif", "image/avif"}

// Background is the page background: none, an uploaded image or an https URL.
type Background struct {
	Kind    string  `json:"kind"`    // none | upload | url
	URL     *string `json:"url"`     // only for kind=url (https only)
	Version int     `json:"version"` // bumped on every upload
	// Tag names the uploaded picture: this user's, this file (BackgroundTag). The page puts it into the
	// picture's address and the answer carries it as its ETag, so that no two accounts share an address or a
	// validator in a browser's cache (r3-p3-01). Worked out when the settings are read; never stored, and what
	// a request sends is not used.
	Tag string `json:"tag,omitempty"`
}

// Popups are the pop-ups an open page shows for new notifications (README, "Notifications").
type Popups struct {
	Enabled bool   `json:"enabled"`
	Seconds int    `json:"seconds"` // how long one stays: PopupMinSeconds-PopupMaxSeconds
	Level   string `json:"level"`   // PopupCritical: critical events and alerts only; PopupWarning: every warning too
}

// The pop-up settings' limits and words.
const (
	PopupMinSeconds = 3
	PopupMaxSeconds = 30
	PopupCritical   = "critical"
	PopupWarning    = "warning"
)

// DefaultPopups: on, 5 seconds, critical events only.
func DefaultPopups() Popups { return Popups{Enabled: true, Seconds: 5, Level: PopupCritical} }

func (p Popups) valid() bool {
	return p.Seconds >= PopupMinSeconds && p.Seconds <= PopupMaxSeconds && (p.Level == PopupCritical || p.Level == PopupWarning)
}

// UserSettings are one user's UI settings.
type UserSettings struct {
	Theme       string     `json:"theme"`
	Background  Background `json:"background"`
	Dim         int        `json:"dim"`          // 0-90 %
	Blur        int        `json:"blur"`         // 0-40 px
	CardOpacity int        `json:"card_opacity"` // 20-100 %
	Popups      Popups     `json:"popups"`
}

// Defaults are the settings of a user who never changed them.
func Defaults() UserSettings {
	return UserSettings{Theme: "black", Background: Background{Kind: "none"}, Dim: 60, Blur: 14, CardOpacity: 72, Popups: DefaultPopups()}
}

// Validate checks ranges and the https-only URL (the backend never fetches it: no SSRF).
func (s *UserSettings) Validate() error {
	if s.Theme != "black" {
		return apierr.New(422, "theme must be black")
	}
	switch s.Background.Kind {
	case "none", "upload":
		if s.Background.URL != nil && *s.Background.URL == "" {
			s.Background.URL = nil
		}
	case "url":
		if s.Background.URL == nil {
			return apierr.New(422, "background.url is required for kind=url")
		}
		u, err := url.Parse(*s.Background.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || len(*s.Background.URL) > 2083 {
			return apierr.New(422, "background.url must be an https:// URL")
		}
	default:
		return apierr.New(422, "background.kind must be none, upload or url")
	}
	if s.Dim < 0 || s.Dim > 90 || s.Blur < 0 || s.Blur > 40 || s.CardOpacity < 20 || s.CardOpacity > 100 {
		return apierr.New(422, "dim 0-90, blur 0-40, card_opacity 20-100")
	}
	if !s.Popups.valid() {
		return apierr.Errf(422, "popups.seconds %d-%d, popups.level %s or %s", PopupMinSeconds, PopupMaxSeconds, PopupCritical, PopupWarning)
	}
	return nil
}

// Decode parses a request body strictly (unknown fields rejected), applying defaults for missing ones.
func Decode(b []byte) (UserSettings, error) {
	s := Defaults()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, apierr.New(422, "invalid settings body")
	}
	if s.Background.Kind == "" {
		s.Background.Kind = "none"
	}
	return s, s.Validate()
}

// Sniff returns the media type from magic bytes ("" if unknown). Never trusts the client's Content-Type alone.
func Sniff(d []byte) string {
	switch {
	case bytes.HasPrefix(d, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(d, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case len(d) >= 12 && string(d[:4]) == "RIFF" && string(d[8:12]) == "WEBP":
		return "image/webp"
	case len(d) >= 6 && (string(d[:6]) == "GIF87a" || string(d[:6]) == "GIF89a"):
		return "image/gif"
	case len(d) >= 12 && string(d[4:8]) == "ftyp" && (string(d[8:12]) == "avif" || string(d[8:12]) == "avis"):
		return "image/avif"
	}
	return ""
}

// ValidateUpload returns the verified media type or an error (413 too large, 415 wrong type, 400 empty). The
// same refusal has the same words as for the sign-in picture (SaveSignIn): "picture larger than 8 MiB", "the
// file is empty", "not a ... picture (...)" with the types this upload takes.
func ValidateUpload(contentType string, data []byte) (string, error) {
	if len(data) > MaxBackgroundBytes {
		return "", apierr.Errf(413, "picture larger than %d MiB", MaxBackgroundBytes>>20)
	}
	if len(data) == 0 {
		return "", apierr.New(400, "the file is empty")
	}
	claimed := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if _, ok := ImageTypes[claimed]; !ok {
		c := claimed
		if c == "" {
			c = "?"
		}
		return "", apierr.Errf(415, "not a PNG, JPEG, WebP, GIF or AVIF picture (it was sent as %s; SVG is not accepted)", c)
	}
	if Sniff(data) == "" { // no picture at all (a text file named .png): the words of the sign-in picture's refusal
		return "", apierr.New(415, "not a PNG, JPEG, WebP, GIF or AVIF picture (the file's content decides, not its name; SVG is not accepted)")
	}
	if Sniff(data) != claimed {
		return "", apierr.New(415, "file content does not match its declared image type")
	}
	if !Whole(claimed, data) { // it begins like one and is not one (picture.go)
		return "", apierr.New(415, "the file is not a complete picture of its type (it is cut off or damaged)")
	}
	if p := PixelProblem(claimed, data); p != "" { // a small file can be a very large picture (r2-p3-02)
		return "", apierr.New(413, p)
	}
	return claimed, nil
}

// Store keeps every user's settings in one JSON file and background images in a directory.
type Store struct {
	dir, bgDir, file string
	mu               sync.Mutex
	signIn           atomic.Pointer[SignInPicture] // the sign-in wallpaper (signin.go)
	signInOnce       sync.Once                     // it is read from bgDir once
	stuck            bool                          // a damaged settings.json could not be moved aside, and that was said (loadAll)
	unread           bool                          // settings.json could not be read at the latest try, and that was said (loadAll)
}

// NewStore returns the store in dataDir.
func NewStore(dataDir string) *Store {
	return &Store{dir: dataDir, bgDir: filepath.Join(dataDir, "backgrounds"), file: filepath.Join(dataDir, "settings.json")}
}

// Check looks at settings.json once, at the panel's start, so that a damaged file is moved aside and named in
// the log then, and not at the first request for somebody's settings.
func (s *Store) Check() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.loadAll()
}

// errUnread is what a save answers while settings.json cannot be read (loadAll).
func errUnread() error {
	return apierr.New(503, "Your settings were not saved: the panel could not read settings.json just now, and it never writes over a file it could not read. Try again in a moment.")
}

// loadAll reads everybody's settings. The caller holds s.mu. A file that does not exist is no settings yet.
//
// A file that was read and cannot be used (empty, cut off, not a JSON object), and one that is not a plain
// file of a sane size (a link, a folder), is no settings either, and it used to be read so without a word: the
// next save by any user then wrote a new file with that one user over it, and everybody else's settings were
// gone with no copy (audit 5, r3-p2-07). It is moved aside like the other files the panel may start over
// (statefile.KeepAside: settings.json.corrupt-<UTC time>), with one ERROR that names the copy. Everybody's
// settings start empty (the defaults); the kept copy has what could be read of the old ones.
//
// A file that could not be READ is another matter (second review of fix7a, R7A-07): too many open files, an
// I/O error, a wrong owner after a restore say nothing about what is in it. It was moved aside like a damaged
// one: a good file was gone from its place, everybody's settings started over, and five such moments removed
// the oldest kept copies. It is left where it is, with one ERROR for as long as it lasts, and the error is
// returned (errUnread): the settings show as the defaults meanwhile, and nothing is saved until the file can
// be read again, so that the defaults are never written over a file that was not read.
func (s *Store) loadAll() (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	b, err := statefile.ReadFile(s.file, statefile.MaxBytes) // a plain file of a sane size, or it is not read
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.unread = false
		return out, nil
	case err == nil:
		s.unread = false
		if err = json.Unmarshal(b, &out); err == nil && out != nil {
			return out, nil
		}
		if err == nil { // "null"
			err = errors.New("not a JSON object")
		}
		if len(bytes.TrimSpace(b)) == 0 {
			err = fmt.Errorf("it is empty (%d bytes)", len(b))
		}
	case statefile.IsNotAFile(err): // a link, a folder, a file far too large: nothing the panel wrote
		s.unread = false
	default:
		if !s.unread {
			s.unread = true // said once; the file is looked at again with every request
			slog.Error("settings.json could not be read: it is left in place. Every user's settings show as the defaults, and nothing is saved, until it can be read again",
				"path", s.file, "error", err)
		}
		return map[string]json.RawMessage{}, errUnread()
	}
	const means = "every user's settings start over with the defaults"
	if aside, rerr := statefile.KeepAside(s.file); rerr == nil {
		s.stuck = false
		slog.Error("settings.json cannot be used: "+means+". The file is kept as "+aside, "path", s.file, "error", err)
	} else if !s.stuck {
		s.stuck = true // said once; the file is looked at again with every request
		slog.Error("settings.json cannot be used and could not be moved aside: "+means+", the file is still in place, and the next time somebody saves "+
			"settings it is written over", "path", s.file, "error", err, "move", rerr)
	}
	return map[string]json.RawMessage{}, nil
}

func (s *Store) saveAll(d map[string]json.RawMessage) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(d, "", " ")
	return statefile.WriteAtomic(s.file, b, 0o600)
}

// key: filenames never contain the raw sub.
func key(sub string) string {
	h := sha256.Sum256([]byte(sub))
	return hex.EncodeToString(h[:])[:24]
}

// Get returns a user's settings (the defaults when none are saved).
func (s *Store) Get(sub string) UserSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(sub)
}

func (s *Store) get(sub string) UserSettings {
	all, _ := s.loadAll() // a file that cannot be read shows as the defaults; a save is what is refused then (put)
	raw, ok := all[sub]
	if !ok {
		return Defaults()
	}
	st := Defaults() // what the saved settings do not name keeps its default: settings saved before a field existed load as they are
	if json.Unmarshal(raw, &st) != nil {
		return Defaults()
	}
	if !st.Popups.valid() { // a saved value outside the limits: the default for it, the rest is kept
		p := DefaultPopups()
		if st.Popups.Seconds >= PopupMinSeconds && st.Popups.Seconds <= PopupMaxSeconds {
			p.Seconds = st.Popups.Seconds
		}
		if st.Popups.Level == PopupWarning {
			p.Level = PopupWarning
		}
		p.Enabled = st.Popups.Enabled
		st.Popups = p
	}
	if st.Validate() != nil {
		return Defaults()
	}
	st.Background.Tag = s.tagFor(sub, st.Background.Kind)
	return st
}

// Put validates and saves a user's settings.
func (s *Store) Put(sub string, st UserSettings) (UserSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.Background.Tag = s.tagFor(sub, st.Background.Kind)
	return st, s.put(sub, st)
}

func (s *Store) put(sub string, st UserSettings) error {
	d, err := s.loadAll()
	if err != nil { // the file could not be read: what is in it is not known, and it is not written over
		return err
	}
	st.Background.Tag = "" // never stored: it is the file's, not a setting
	b, _ := json.Marshal(st)
	d[sub] = b
	return s.saveAll(d)
}

// BackgroundTag names one user's uploaded picture as it is now: a hash of the account, the file's time (to the
// nanosecond) and its size. Another account's picture, and this account's next upload, have another tag. ""
// when there is no file. It is not a secret and opens nothing: the picture is only ever sent to its owner.
func BackgroundTag(sub string, st os.FileInfo) string {
	if st == nil || !st.Mode().IsRegular() {
		return ""
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("background\x00%s\x00%d\x00%d", sub, st.ModTime().UnixNano(), st.Size())))
	return hex.EncodeToString(h[:])[:24]
}

// tagFor is the tag of sub's uploaded picture when the background is that picture ("" otherwise).
func (s *Store) tagFor(sub, kind string) string {
	if kind != "upload" {
		return ""
	}
	p, _ := s.BackgroundPath(sub)
	if p == "" {
		return ""
	}
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	return BackgroundTag(sub, st)
}

// BackgroundPath returns the uploaded file and its type ("" if none).
func (s *Store) BackgroundPath(sub string) (string, string) {
	for _, mime := range typeOrder {
		p := filepath.Join(s.bgDir, key(sub)+"."+ImageTypes[mime])
		if _, err := os.Stat(p); err == nil {
			return p, mime
		}
	}
	return "", ""
}

func (s *Store) removeAll(sub string) {
	for _, ext := range ImageTypes { // one file per user
		_ = os.Remove(filepath.Join(s.bgDir, key(sub)+"."+ext))
	}
}

// SaveBackground stores an uploaded image (replacing the previous one) and switches the background to it.
func (s *Store) SaveBackground(sub, mime string, data []byte) (UserSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.loadAll(); err != nil { // before the picture is replaced: its settings could not be saved
		return UserSettings{}, err
	}
	if err := os.MkdirAll(s.bgDir, 0o700); err != nil {
		return UserSettings{}, err
	}
	s.removeAll(sub)
	target := filepath.Join(s.bgDir, key(sub)+"."+ImageTypes[mime])
	if err := statefile.WriteAtomic(target, data, 0o600); err != nil {
		return UserSettings{}, fmt.Errorf("saving background: %w", err)
	}
	st := s.get(sub)
	st.Background = Background{Kind: "upload", Version: st.Background.Version + 1, Tag: s.tagFor(sub, "upload")}
	return st, s.put(sub, st)
}

// DeleteBackground removes the uploaded image and switches the background back to none.
func (s *Store) DeleteBackground(sub string) (UserSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.loadAll(); err != nil { // before the picture is removed: its settings could not be saved
		return UserSettings{}, err
	}
	s.removeAll(sub)
	st := s.get(sub)
	st.Background = Background{Kind: "none", Version: st.Background.Version}
	return st, s.put(sub, st)
}
