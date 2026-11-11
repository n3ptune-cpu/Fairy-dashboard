package settings

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
)

// The sign-in wallpaper: one picture for the whole panel, shown behind the sign-in page to everyone who can
// open it, before any sign-in. It is stored the way a user's uploaded background is (a file in
// <data_dir>/backgrounds, mode 0600, at most MaxBackgroundBytes, its type taken from its first bytes), with
// three differences, all because it is public:
//
//   - it is always an uploaded file, never a URL: the sign-in page loads nothing from another site;
//   - only PNG, JPEG and WebP, and the header has to be whole (SignInType), whatever name or type the browser
//     sent with it;
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

// SignInType returns the media type of a sign-in wallpaper from its first bytes: image/png, image/jpeg or
// image/webp, and "" for everything else (SVG, HTML, GIF, AVIF, a picture whose header is cut off).
func SignInType(d []byte) string {
	switch Sniff(d) {
	case "image/png": // the signature, then the IHDR chunk (13 bytes) with a width and a height
		if len(d) >= 33 && string(d[8:16]) == "\x00\x00\x00\x0dIHDR" && binary.BigEndian.Uint32(d[16:20]) != 0 &&
			binary.BigEndian.Uint32(d[20:24]) != 0 {
			return "image/png"
		}
	case "image/jpeg": // start of image, then a first segment that is there in full
		if len(d) >= 6 && d[3] >= 0xc0 && d[3] != 0xff {
			if n := int(binary.BigEndian.Uint16(d[4:6])); n >= 2 && len(d) >= 4+n {
				return "image/jpeg"
			}
		}
	case "image/webp": // RIFF, size, WEBP, then the first chunk: lossy, lossless or extended
		if len(d) >= 20 {
			switch string(d[12:16]) {
			case "VP8 ", "VP8L", "VP8X":
				return "image/webp"
			}
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
		f, err := os.Open(filepath.Join(s.bgDir, signInFile))
		if err != nil {
			return
		}
		defer f.Close()
		if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
			return
		}
		if data, err := io.ReadAll(io.LimitReader(f, MaxBackgroundBytes+1)); err == nil {
			s.signIn.Store(newSignIn(data))
		}
	})
	return s.signIn.Load()
}

// SaveSignIn stores data as the sign-in wallpaper, replacing the one before (413 too large, 400 empty, 415
// not a PNG, JPEG or WebP picture). The file is written under another name and renamed, so the directory never
// holds half a picture under the name that is read at start.
func (s *Store) SaveSignIn(data []byte) (*SignInPicture, error) {
	if len(data) > MaxBackgroundBytes {
		return nil, apierr.Errf(413, "picture larger than %d MiB", MaxBackgroundBytes>>20)
	}
	if len(data) == 0 {
		return nil, apierr.New(400, "empty body")
	}
	p := newSignIn(data)
	if p == nil {
		return nil, apierr.New(415, "not a PNG, JPEG or WebP picture (the file's content decides, not its name; SVG is not accepted)")
	}
	s.SignIn() // what is on disk is read before it is replaced, never after
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.bgDir, 0o700); err != nil {
		return nil, err
	}
	target := filepath.Join(s.bgDir, signInFile)
	if err := os.WriteFile(target+".tmp", data, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(target+".tmp", target); err != nil {
		_ = os.Remove(target + ".tmp")
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
