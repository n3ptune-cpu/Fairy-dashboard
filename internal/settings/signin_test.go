package settings

import (
	"bytes"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
)

// Real pictures of the three types: a PNG and a JPEG made by the standard library, and the smallest WebP there
// is (1x1, lossless), which no standard library package writes.
func testPictures(t *testing.T) map[string][]byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 6))
	var p, j bytes.Buffer
	if err := png.Encode(&p, img); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&j, img, nil); err != nil {
		t.Fatal(err)
	}
	webp := []byte("RIFF\x1a\x00\x00\x00WEBPVP8L\x0d\x00\x00\x00\x2f\x00\x00\x00\x10\x07\x10\x11\x11\x88\x88\xfe\x07\x00")
	return map[string][]byte{"image/png": p.Bytes(), "image/jpeg": j.Bytes(), "image/webp": webp}
}

func status(err error) int {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

func TestSignInTypeTakesWholeHeadersOfThreeTypesOnly(t *testing.T) {
	pics := testPictures(t)
	for want, data := range pics {
		if got := SignInType(data); got != want {
			t.Errorf("%s: a real picture is read as %q", want, got)
		}
	}
	pngPic, jpg, webp := pics["image/png"], pics["image/jpeg"], pics["image/webp"]
	noSize := append([]byte{}, pngPic...)
	copy(noSize[16:20], []byte{0, 0, 0, 0}) // width 0
	longSeg := append([]byte{}, jpg[:6]...)
	longSeg[4], longSeg[5] = 0xff, 0xff // a first segment of 65535 bytes in a file of 6
	for name, data := range map[string][]byte{
		"nothing":                         nil,
		"svg":                             []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`),
		"svg with an xml head":            []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`),
		"html":                            []byte("<!doctype html><html><script>alert(1)</script></html>"),
		"text":                            []byte("just text"),
		"gif":                             []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		"avif":                            []byte("\x00\x00\x00\x1cftypavif\x00\x00\x00\x00avifmif1miaf"),
		"png: the signature alone":        pngPic[:8],
		"png: cut inside IHDR":            pngPic[:20],
		"png: one byte short of IHDR":     pngPic[:32],
		"png: no width":                   noSize,
		"png: signature, then html":       append(append([]byte{}, pngPic[:8]...), []byte("<html><script>alert(1)</script></html>")...),
		"jpeg: the three magic bytes":     jpg[:3],
		"jpeg: cut before the length":     jpg[:5],
		"jpeg: first segment not there":   longSeg,
		"jpeg: magic, then html":          []byte("\xff\xd8\xff<html><script>alert(1)</script>"),
		"webp: RIFF and WEBP only":        webp[:12],
		"webp: cut inside the chunk head": webp[:19],
		"webp: another RIFF file (wave)":  []byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00"),
		"webp: unknown first chunk":       []byte("RIFF\x1a\x00\x00\x00WEBPJUNK\x0d\x00\x00\x00\x00\x00\x00\x00"),
	} {
		if got := SignInType(data); got != "" {
			t.Errorf("%s is taken as %s", name, got)
		}
	}
}

func TestSignInPictureIsStoredReplacedAndRemoved(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	file := filepath.Join(dir, "backgrounds", signInFile)
	if s.SignIn() != nil {
		t.Fatal("a new panel has a sign-in wallpaper")
	}
	if err := s.DeleteSignIn(); err != nil {
		t.Fatal("removing nothing:", err)
	}
	pics := testPictures(t)
	for _, bad := range []struct {
		name string
		data []byte
		want int
	}{{"empty", nil, 400}, {"svg", []byte("<svg/>"), 415}, {"gif", []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"), 415},
		{"too large", append(append([]byte{}, pics["image/png"]...), make([]byte, MaxBackgroundBytes)...), 413}} {
		if _, err := s.SaveSignIn(bad.data); status(err) != bad.want {
			t.Errorf("%s: %v (want %d)", bad.name, err, bad.want)
		}
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) || s.SignIn() != nil {
		t.Fatal("a refused picture left something behind")
	}
	var last string
	for _, mime := range []string{"image/png", "image/jpeg", "image/webp"} {
		p, err := s.SaveSignIn(pics[mime])
		if err != nil || p.Type != mime || !bytes.Equal(p.Data, pics[mime]) || len(p.Version) != 16 || p.Version == last {
			t.Fatalf("%s: %+v %v", mime, p, err)
		}
		last = p.Version
		// one file, whatever the type was before; mode 0600; nothing else in the directory
		names, _ := os.ReadDir(filepath.Join(dir, "backgrounds"))
		st, err := os.Stat(file)
		if len(names) != 1 || err != nil {
			t.Fatalf("%s: files %v, %v", mime, names, err)
		}
		if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: mode %v", mime, st.Mode())
		}
		// a restart: another Store on the same directory reads the same picture
		again := NewStore(dir).SignIn()
		if again == nil || again.Type != mime || again.Version != p.Version || !bytes.Equal(again.Data, pics[mime]) {
			t.Fatalf("%s is not there after a restart: %+v", mime, again)
		}
	}
	// a picture that is refused leaves the stored one as it is
	if _, err := s.SaveSignIn([]byte("<html>")); status(err) != 415 || s.SignIn().Version != last || NewStore(dir).SignIn().Version != last {
		t.Fatal("a refused upload changed the stored picture")
	}
	// a user's own background lives in the same directory under another name and is not touched
	if _, err := s.SaveBackground("u1", "image/png", pics["image/png"]); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSignIn(); err != nil || s.SignIn() != nil || NewStore(dir).SignIn() != nil {
		t.Fatal("not removed:", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("the file is still there")
	}
	if p, _ := s.BackgroundPath("u1"); p == "" {
		t.Fatal("removing the sign-in wallpaper removed a user's background")
	}
	if _, err := s.DeleteBackground("u1"); err != nil {
		t.Fatal(err)
	}
}

// What is in the data directory under the picture's name is checked again at every start: a file that is not
// one of the three types, or is too large, is not handed out.
func TestSignInPictureOnDiskIsCheckedAtStart(t *testing.T) {
	pics := testPictures(t)
	for name, data := range map[string][]byte{
		"html":      []byte("<html><script>alert(1)</script></html>"),
		"svg":       []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`),
		"gif":       []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		"too large": append(append([]byte{}, pics["image/png"]...), make([]byte, MaxBackgroundBytes)...),
		"empty":     {},
	} {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "backgrounds"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "backgrounds", signInFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if p := NewStore(dir).SignIn(); p != nil {
			t.Errorf("%s in the data directory is handed out as %s", name, p.Type)
		}
	}
	dir := t.TempDir() // a directory under that name is nothing either
	if err := os.MkdirAll(filepath.Join(dir, "backgrounds", signInFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if NewStore(dir).SignIn() != nil {
		t.Error("a directory is handed out")
	}
}
