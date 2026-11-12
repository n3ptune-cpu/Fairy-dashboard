package settings

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
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
