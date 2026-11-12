package settings

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
)

// Real pictures of the five types. PNG, JPEG and GIF are made by the standard library; WebP and AVIF have no
// writer there, so these are files made by libwebp and libavif (8x8 and 16x16 pixels), kept here as base64.
var realWebP = map[string]string{
	"lossy":     "UklGRkAAAABXRUJQVlA4IDQAAADwAQCdASoIAAgAAsBMJagCdAD0rtHDAAAA/vXxby2tCYXgxCnOxVg/84nv5PetHzvqwAAA",
	"lossless":  "UklGRioAAABXRUJQVlA4TB4AAAAvB8ABALmM6H/sIqL/ASEB4Yj/RzUH5mMiYgKAWzc=",
	"alpha":     "UklGRmQAAABXRUJQVlA4WAoAAAAQAAAABwAABwAAQUxQSAoAAAABB1DkiAhERP8DVlA4IDQAAADwAQCdASoIAAgAAsBMJagCdAD0rtHDAAAA/vXxby2tCYXgxCnOxVg/84nv5PetHzvqwAAA",
	"animation": "UklGRuoAAABXRUJQVlA4WAoAAAACAAAABwAABwAAQU5JTQYAAAAAAAAAAABBTk1GWgAAAAAAAAAAAAcAAAcAAGQAAAJWUDggQgAAANABAJ0BKggACAABQCYlqAADYmeUz5mAAP79KR3HrK7nFcCXNjR7EI8d/7h/4n//qP5oLkkv9/z9ojIf7X/7FLlgAEFOTUZcAAAAAAAAAAAABwAABwAAZAAAAFZQOCBEAAAA1AEAnQEqCAAIAAAAJiWoAANf7RvPHYAA/u+tvsvmw+tNabFVBzk9+1pKqhsJ//6AeaCPZ7+8p/C5H//Q/+s23w/mCAA=",
}

const realAVIF = "AAAAIGZ0eXBhdmlmAAAAAGF2aWZtaWYxbWlhZk1BMUIAAADrbWV0YQAAAAAAAAAhaGRscgAAAAAAAAAAcGljdAAAAAAAAAAAAAAAAAAAAAAOcGl0bQAAAAAAAQAAAB5pbG9jAAAAAEQAAAEAAQAAAAEAAAETAAAAVwAAAChpaW5mAAAAAAABAAAAGmluZmUCAAAAAAEAAGF2MDFDb2xvcgAAAABqaXBycAAAAEtpcGNvAAAAFGlzcGUAAAAAAAAAEAAAABAAAAAQcGl4aQAAAAADCAgIAAAADGF2MUOBAAwAAAAAE2NvbHJuY2x4AAEADQAGgAAAABdpcG1hAAAAAAAAAAEAAQQBAoMEAAAAX21kYXQSAAoJGAz/2iAhoNCAMkgZR4eGIYeeeeaAAQB1d4S374yJwt/McI2g8CRLg7WtMaaU48x99iHS2jat1ZBgW0e0hKehWxYUumFiorUoiAcU7NTQsFnbF3g="

func b64(t *testing.T, s string) []byte {
	t.Helper()
	d, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// realPictures: name -> [media type, bytes].
func realPictures(t *testing.T) map[string][2]any {
	t.Helper()
	rgba := image.NewRGBA(image.Rect(0, 0, 40, 30))
	pal := image.NewPaletted(image.Rect(0, 0, 40, 30), color.Palette{color.Black, color.White, color.RGBA{200, 30, 30, 255}})
	for y := 0; y < 30; y++ {
		for x := 0; x < 40; x++ {
			rgba.Set(x, y, color.RGBA{uint8(x * 6), uint8(y * 8), uint8(x ^ y), 255})
			pal.SetColorIndex(x, y, uint8((x+y)%3))
		}
	}
	enc := func(f func(*bytes.Buffer) error) []byte {
		var b bytes.Buffer
		if err := f(&b); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	out := map[string][2]any{
		"png, truecolour": {"image/png", enc(func(b *bytes.Buffer) error { return png.Encode(b, rgba) })},
		"png, palette":    {"image/png", enc(func(b *bytes.Buffer) error { return png.Encode(b, pal) })},
		"jpeg":            {"image/jpeg", enc(func(b *bytes.Buffer) error { return jpeg.Encode(b, rgba, nil) })},
		"jpeg, grey": {"image/jpeg", enc(func(b *bytes.Buffer) error {
			return jpeg.Encode(b, image.NewGray(rgba.Bounds()), &jpeg.Options{Quality: 40})
		})},
		"gif": {"image/gif", enc(func(b *bytes.Buffer) error { return gif.Encode(b, pal, nil) })},
		"gif, animation": {"image/gif", enc(func(b *bytes.Buffer) error {
			return gif.EncodeAll(b, &gif.GIF{Image: []*image.Paletted{pal, pal}, Delay: []int{10, 10}})
		})},
		"avif": {"image/avif", b64(t, realAVIF)},
	}
	for name, s := range realWebP {
		out["webp, "+name] = [2]any{"image/webp", b64(t, s)}
	}
	return out
}

// pngChunk is one PNG chunk with its length and checksum.
func pngChunk(kind string, data []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	out = append(append(out, kind...), data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out[4:]))
}

// r1-p3-05 (audit 5): a picture upload was accepted by its first bytes. The auditor's file, a PNG signature,
// one valid header chunk and then a page of HTML, was stored as the sign-in wallpaper and handed out as
// image/png. Now a file has to be a whole picture of its type, from its first byte to its last part.
func TestAPictureIsWholeOrRefused(t *testing.T) {
	for name, p := range realPictures(t) {
		mime, d := p[0].(string), p[1].([]byte)
		if Sniff(d) != mime || !Whole(mime, d) {
			t.Errorf("%s: a real picture is refused (sniffed as %q)", name, Sniff(d))
		}
		if got, err := ValidateUpload(mime, d); err != nil || got != mime {
			t.Errorf("%s as a background: %v", name, err)
		}
		// cut off anywhere: never whole
		for _, n := range []int{len(d) - 1, len(d) - 2, len(d) * 3 / 4, len(d) / 2, len(d) / 4, 24, 12} {
			if n > 0 && n < len(d) && Whole(mime, d[:n]) {
				t.Errorf("%s, cut to %d of %d bytes, is taken for a whole picture", name, n, len(d))
			}
		}
		// another type's name for it
		for _, other := range []string{"image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/svg+xml", ""} {
			if other != mime && Whole(other, d) {
				t.Errorf("%s is taken for a whole %s", name, other)
			}
		}
		// the same picture with a page of HTML in the place of its second half
		half := append(append([]byte{}, d[:len(d)/2]...), []byte("<html><body><script>alert(document.domain)</script><svg onload=alert(1)>")...)
		if Whole(mime, half) {
			t.Errorf("%s with HTML in the place of its second half is taken for a whole picture", name)
		}
	}
	html := []byte("<html><body><script>alert(document.domain)</script><svg onload=alert(1)>")
	ihdr := pngChunk("IHDR", []byte{0, 0, 0, 1, 0, 0, 0, 1, 8, 2, 0, 0, 0})
	sig := []byte("\x89PNG\r\n\x1a\n")
	idat := pngChunk("IDAT", []byte{0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0, 0, 3, 1, 1, 0})
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	jpg := realPictures(t)["jpeg"][1].([]byte)
	sos := bytes.Index(jpg, []byte{0xff, 0xda})
	gifPic := realPictures(t)["gif"][1].([]byte)
	webp := b64(t, realWebP["lossless"])
	wrongSize := append([]byte{}, webp...)
	wrongSize[4]++ // RIFF says one byte more than the file has
	avif := b64(t, realAVIF)
	for name, c := range map[string]struct {
		mime string
		data []byte
	}{
		"the auditor's file: PNG signature, header chunk, HTML": {"image/png", join(sig, ihdr, html)},
		"png: header and image data, no end chunk":              {"image/png", join(sig, ihdr, idat)},
		"png: header and end, no image data":                    {"image/png", join(sig, ihdr, pngChunk("IEND", nil))},
		"png: whole, then HTML after the end chunk":             {"image/png", join(sig, ihdr, idat, pngChunk("IEND", nil), html)},
		"png: a chunk with a wrong checksum":                    {"image/png", join(sig, ihdr, idat[:len(idat)-1], []byte{0}, pngChunk("IEND", nil))},
		"png: a chunk longer than the file":                     {"image/png", join(sig, ihdr, []byte{0x7f, 0xff, 0xff, 0xff}, []byte("IDAT"), html)},
		"png: two header chunks":                                {"image/png", join(sig, ihdr, ihdr, idat, pngChunk("IEND", nil))},
		"png: HTML in a text chunk only":                        {"image/png", join(sig, ihdr, pngChunk("tEXt", html), pngChunk("IEND", nil))},
		"jpeg: the headers up to the scan, then HTML":           {"image/jpeg", join(jpg[:sos], html)},
		"jpeg: the scan's header, then HTML and no end marker":  {"image/jpeg", join(jpg[:sos+14], html)},
		"jpeg: no frame, only a comment with HTML and the end":  {"image/jpeg", join([]byte{0xff, 0xd8, 0xff, 0xfe, 0, byte(len(html) + 2)}, html, []byte{0xff, 0xd9})},
		"gif: header and screen, then HTML":                     {"image/gif", join(gifPic[:13], html)},
		"gif: no image, only the trailer":                       {"image/gif", join([]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"))},
		"gif: the trailer is missing":                           {"image/gif", gifPic[:len(gifPic)-1]},
		"webp: the size in the header is not the file's":        {"image/webp", wrongSize},
		"webp: RIFF and WEBP, then HTML":                        {"image/webp", join(webp[:12], html)},
		"webp: a first chunk that is no image":                  {"image/webp", []byte("RIFF\x1a\x00\x00\x00WEBPJUNK\x0e\x00\x00\x00<html><script>")},
		"webp: the extended header and nothing else":            {"image/webp", []byte("RIFF\x16\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x00\x00\x00\x00\x07\x00\x00\x07\x00\x00")},
		"webp: lossless with a wrong signature byte":            {"image/webp", join(webp[:20], []byte{0x3c}, webp[21:])},
		"avif: the file type box alone":                         {"image/avif", avif[:32]},
		"avif: file type box, then HTML":                        {"image/avif", join(avif[:32], html)},
		"avif: a box longer than the file":                      {"image/avif", join(avif[:32], []byte{0, 0, 0xff, 0xff}, []byte("meta"), html)},
		"avif: another brand (a plain mp4)":                     {"image/avif", join([]byte("\x00\x00\x00\x14ftypisom\x00\x00\x00\x00mp41"), avif[32:])},
		"svg, called a PNG":                                     {"image/png", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`)},
		"nothing":                                               {"image/png", nil},
	} {
		if Whole(c.mime, c.data) {
			t.Errorf("%s is taken for a whole %s", name, c.mime)
		}
		if _, err := ValidateUpload(c.mime, c.data); err == nil {
			t.Errorf("%s is accepted as a background", name)
		}
		if got := SignInType(c.data); got != "" {
			t.Errorf("%s is accepted as a sign-in wallpaper (%s)", name, got)
		}
	}
	// the size limit is what it was, and is checked before anything is parsed
	big := append(append([]byte{}, realPictures(t)["png, truecolour"][1].([]byte)...), make([]byte, MaxBackgroundBytes)...)
	if _, err := ValidateUpload("image/png", big); status(err) != 413 {
		t.Errorf("a file over the limit: %v", err)
	}
	if _, err := NewStore(t.TempDir()).SaveSignIn(big); status(err) != 413 {
		t.Errorf("a sign-in wallpaper over the limit: %v", err)
	}
	// a JPEG may carry data of its own after the end marker (cameras do that): still a whole picture
	if !Whole("image/jpeg", append(append([]byte{}, jpg...), []byte("MPF trailer of a camera")...)) {
		t.Error("a JPEG with a trailer after its end marker is refused")
	}
}

// resized returns the picture with another size written into its header, and nothing else changed. The image
// data no longer fits that size, which the check never looks at (no pixel is decoded): for the check it is
// the auditor's file, a small file that states a huge size.
func resized(t *testing.T, mime string, d []byte, w, h int) []byte {
	t.Helper()
	out := bytes.Clone(d)
	switch mime {
	case "image/png": // IHDR is the first chunk: width and height at 16 and 20, its checksum after 13 bytes of data
		binary.BigEndian.PutUint32(out[16:], uint32(w))
		binary.BigEndian.PutUint32(out[20:], uint32(h))
		binary.BigEndian.PutUint32(out[29:], crc32.ChecksumIEEE(out[12:29]))
	case "image/jpeg": // the frame header: marker, length, precision, height, width
		for p := 2; p+9 < len(out); {
			m, n := out[p+1], int(binary.BigEndian.Uint16(out[p+2:]))
			if m >= 0xc0 && m <= 0xcf && m != 0xc4 && m != 0xc8 && m != 0xcc {
				binary.BigEndian.PutUint16(out[p+5:], uint16(h))
				binary.BigEndian.PutUint16(out[p+7:], uint16(w))
				return out
			}
			p += 2 + n
		}
		t.Fatal("no frame header")
	case "image/gif":
		binary.LittleEndian.PutUint16(out[6:], uint16(w))
		binary.LittleEndian.PutUint16(out[8:], uint16(h))
	case "image/webp":
		body := out[20:]
		switch string(out[12:16]) {
		case "VP8X":
			body[4], body[5], body[6] = byte(w-1), byte((w-1)>>8), byte((w-1)>>16)
			body[7], body[8], body[9] = byte(h-1), byte((h-1)>>8), byte((h-1)>>16)
		case "VP8 ":
			binary.LittleEndian.PutUint16(body[6:], uint16(w)&0x3fff)
			binary.LittleEndian.PutUint16(body[8:], uint16(h)&0x3fff)
		case "VP8L":
			bits := binary.LittleEndian.Uint32(body[1:])&^0x0fffffff | uint32(w-1)&0x3fff | (uint32(h-1)&0x3fff)<<14
			binary.LittleEndian.PutUint32(body[1:], bits)
		}
	case "image/avif":
		i := bytes.Index(out, []byte("ispe"))
		binary.BigEndian.PutUint32(out[i+8:], uint32(w))
		binary.BigEndian.PutUint32(out[i+12:], uint32(h))
	}
	return out
}

// r2-p3-02 (audit 5): an upload was limited in bytes only. A white PNG of 20000 x 20000 pixels has 76 kB; as
// the sign-in picture it made every visitor's tab take about 2 GiB. WebP had no look at its size at all. An
// upload of more than 8192 pixels a side or 40 megapixels is refused, for the sign-in picture and for a
// user's background, with a message that says the size and the limit.
func TestAPictureWithTooManyPixelsIsRefused(t *testing.T) {
	checked := map[string]int{}
	defer func() {
		for _, mime := range []string{"image/png", "image/jpeg", "image/gif", "image/webp", "image/avif"} {
			if checked[mime] < 4 {
				t.Errorf("%s: only %d oversize files were looked at", mime, checked[mime])
			}
		}
	}()
	for name, pic := range realPictures(t) {
		mime, data := pic[0].(string), pic[1].([]byte)
		w, h, ok := PictureSize(mime, data)
		if !ok || w <= 0 || h <= 0 || w > 4096 || h > 4096 || PixelProblem(mime, data) != "" {
			t.Errorf("%s: a real picture: %d x %d (%v): %q", name, w, h, ok, PixelProblem(mime, data))
			continue
		}
		limit := 16383 // what every one of these headers can state
		for _, size := range [][2]int{{limit, limit}, {MaxPictureSide + 1, 16}, {16, MaxPictureSide + 1}, {7000, 6000}} {
			big := resized(t, mime, data, size[0], size[1])
			if !Whole(mime, big) {
				continue // this header cannot be changed without breaking the file for the structure check (not the case today)
			}
			checked[mime]++
			gw, gh, _ := PictureSize(mime, big)
			why := PixelProblem(mime, big)
			if gw != size[0] || gh != size[1] || !strings.Contains(why, fmt.Sprintf("the picture is %d x %d pixels; at most 8192 pixels a side and 40 megapixels", size[0], size[1])) {
				t.Errorf("%s as %d x %d: read as %d x %d: %q", name, size[0], size[1], gw, gh, why)
			}
			if _, err := ValidateUpload(mime, big); err == nil || !strings.Contains(err.Error(), "megapixels") {
				t.Errorf("%s as %d x %d: a user's background accepted it: %v", name, size[0], size[1], err)
			}
		}
		for _, size := range [][2]int{{MaxPictureSide, 4882}, {4882, MaxPictureSide}, {6324, 6324}, {1, 1}} { // at the limits
			if ok := resized(t, mime, data, size[0], size[1]); Whole(mime, ok) && PixelProblem(mime, ok) != "" {
				t.Errorf("%s as %d x %d must be accepted: %s", name, size[0], size[1], PixelProblem(mime, ok))
			}
		}
	}
	// the auditor's case: a PNG that states 20000 x 20000
	var small bytes.Buffer
	_ = png.Encode(&small, image.NewGray(image.Rect(0, 0, 8, 8)))
	bomb := resized(t, "image/png", small.Bytes(), 20000, 20000)
	if SignInType(bomb) != "image/png" {
		t.Fatal("setup: the file must be a whole PNG for the structure check")
	}
	st := NewStore(t.TempDir())
	_, err := st.SaveSignIn(bomb)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 413 || !strings.Contains(err.Error(), "the picture is 20000 x 20000 pixels") || st.SignIn() != nil {
		t.Fatalf("the sign-in picture: %v", err)
	}
	if _, err := ValidateUpload("image/png", bomb); !errors.As(err, &ae) || ae.Status != 413 {
		t.Fatalf("a user's background: %v", err)
	}
	if p, err := st.SaveSignIn(small.Bytes()); err != nil || p == nil {
		t.Fatal("a small picture:", err)
	}
	// a file that states no size
	if PixelProblem("image/avif", []byte("....ftypavif....")) == "" || PixelProblem("image/webp", []byte("RIFF....WEBPVP8?..............")) == "" {
		t.Fatal("no size must be a problem")
	}

	// Pictures stored before there was a limit are kept and served, and counted once at start for one warning.
	dir := t.TempDir()
	old := NewStore(dir)
	if si, bg := old.OversizePictures(); si || bg != 0 {
		t.Fatal("an empty folder:", si, bg)
	}
	_ = os.MkdirAll(filepath.Join(dir, "backgrounds"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "backgrounds", signInFile), bomb, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "backgrounds", "0123456789abcdef01234567.png"), bomb, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "backgrounds", "89abcdef0123456701234567.png"), small.Bytes(), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "backgrounds", "notes.txt"), []byte("not a picture"), 0o600)
	if si, bg := old.OversizePictures(); !si || bg != 1 {
		t.Fatalf("stored before the limit: sign-in %v, backgrounds %d", si, bg)
	}
	if p := old.SignIn(); p == nil || p.Type != "image/png" || len(p.Data) != len(bomb) {
		t.Fatal("the stored sign-in picture must still be served")
	}
	if left, _ := os.ReadDir(filepath.Join(dir, "backgrounds")); len(left) != 4 {
		t.Fatal("nothing may be deleted")
	}
}
