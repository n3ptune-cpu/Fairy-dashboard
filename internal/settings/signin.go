package settings

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// The sign-in wallpaper: one picture for the whole panel, shown behind the sign-in page to everyone who can
// open it, before any sign-in. It is stored the way a user's uploaded background is (a file in
// <data_dir>/backgrounds, mode 0600, at most MaxBackgroundBytes, its type taken from its content), with
// three differences, all because it is public:
//
//   - it is always an uploaded file, never a URL: the sign-in page loads nothing from another site;
//   - only PNG, JPEG and WebP, and the file has to be a whole picture of its type (SignInType, Whole),
//     whatever name or type the browser sent with it;
//   - it has one fixed file name, which no user's background can have (those are 24 hex digits and an
//     extension), and it is kept in memory, so answering the public route never opens a file.

// signInFile is the file's name in the backgrounds directory. The type is not in the name: it is read from the
// content again at every start.
const signInFile = "signin-wallpaper"

// SignInPicture is the stored sign-in wallpaper.
type SignInPicture struct {
	Type    string // image/png, image/jpeg or image/webp: what the content is
	Data    []byte // never changed after it was stored
	Version string // 16 hex digits of the content's SHA-256: the ETag and the ?v= of its address
}

// SignInType returns the media type of a sign-in wallpaper from its content: image/png, image/jpeg or
// image/webp when the file is a whole picture of that type (Whole), and "" for everything else: SVG, HTML,
// GIF, AVIF, a picture that is cut off, and a file that only begins like a picture.
func SignInType(d []byte) string {
	switch mime := Sniff(d); mime {
	case "image/png", "image/jpeg", "image/webp":
		if Whole(mime, d) {
			return mime
		}
	}
	return ""
}

func newSignIn(data []byte) *SignInPicture {
	mime := SignInType(data)
	if mime == "" || len(data) > MaxBackgroundBytes {
		return nil
	}
	sum := sha256.Sum256(data)
	return &SignInPicture{Type: mime, Data: data, Version: hex.EncodeToString(sum[:8])}
}

// SignIn returns the sign-in wallpaper, nil when none is set. The first call reads it from the data directory
// (a file there that is too large or not one of the three types is not used); after that it is answered from
// memory.
func (s *Store) SignIn() *SignInPicture {
	s.signInOnce.Do(func() {
		// a regular file of at most that size, opened without waiting: a named pipe or a link under that name
		// is not read (it used to hang the public picture route for good, r2-p2-10)
		data, err := statefile.ReadFile(filepath.Join(s.bgDir, signInFile), MaxBackgroundBytes)
		if err != nil {
			if statefile.IsNotAFile(err) {
				slog.Warn("sign-in wallpaper: not used", "error", err)
			}
			return
		}
		s.signIn.Store(newSignIn(data))
	})
	return s.signIn.Load()
}

// CheckSignIn is what SaveSignIn checks before it stores anything: the size, that the content is a whole PNG,
// JPEG or WebP picture, and its pixels. The upload's handler asks it before it asks anybody to confirm.
func CheckSignIn(data []byte) (*SignInPicture, error) {
	if len(data) > MaxBackgroundBytes {
		return nil, apierr.Errf(413, "picture larger than %d MiB", MaxBackgroundBytes>>20)
	}
	if len(data) == 0 {
		return nil, apierr.New(400, "the file is empty")
	}
	p := newSignIn(data)
	if p == nil {
		return nil, apierr.New(415, "not a PNG, JPEG or WebP picture (the file's content decides, not its name; SVG is not accepted)")
	}
	if why := PixelProblem(p.Type, data); why != "" { // every visitor's browser has to paint it (r2-p3-02)
		return nil, apierr.New(413, why)
	}
	return p, nil
}

// SaveSignIn stores data as the sign-in wallpaper, replacing the one before (413 too large, 400 empty, 415
// not a PNG, JPEG or WebP picture). The file is written under another name and renamed, so the directory never
// holds half a picture under the name that is read at start.
func (s *Store) SaveSignIn(data []byte) (*SignInPicture, error) {
	p, err := CheckSignIn(data)
	if err != nil {
		return nil, err
	}
	s.SignIn() // what is on disk is read before it is replaced, never after
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.bgDir, 0o700); err != nil {
		return nil, err
	}
	target := filepath.Join(s.bgDir, signInFile)
	if err := statefile.WriteAtomic(target, data, 0o600); err != nil {
		return nil, err
	}
	s.signIn.Store(p)
	return p, nil
}

// DeleteSignIn removes the sign-in wallpaper; with none set it does nothing.
func (s *Store) DeleteSignIn() error {
	s.SignIn()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(filepath.Join(s.bgDir, signInFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	s.signIn.Store(nil)
	return nil
}

// OversizePictures looks, once at the panel's start, at the pictures that are already stored: the sign-in
// picture and every user's background. It reports how many are larger in pixels than an upload may be now
// (PixelProblem). They were accepted before there was a limit; they are kept and shown as before, and the
// caller logs one warning.
func (s *Store) OversizePictures() (signIn bool, backgrounds int) {
	entries, err := os.ReadDir(s.bgDir)
	if err != nil {
		return false, 0
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		f, err := os.Open(filepath.Join(s.bgDir, e.Name()))
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, MaxBackgroundBytes+1))
		f.Close()
		mime := Sniff(data)
		if err != nil || mime == "" || len(data) > MaxBackgroundBytes || !Whole(mime, data) || PixelProblem(mime, data) == "" {
			continue
		}
		if e.Name() == signInFile {
			signIn = true
		} else {
			backgrounds++
		}
	}
	return signIn, backgrounds
}
