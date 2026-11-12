package settings

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image/gif"
	"image/jpeg"
	"image/png"
)

// Whole reports whether d is a picture of the media type mime (as Sniff names it) from its first byte to its
// last part: not only a file that begins like one.
//
// The first bytes say what a file claims to be. A file that is a PNG signature, one header chunk and then a
// page of HTML claimed to be a PNG, was stored, and was handed out as image/png to everyone who opened the
// sign-in page (audit 5, r1-p3-05). Nothing ran (nosniff, a policy of its own), but it was not a picture: the
// sign-in page stayed black while Settings said "PNG, 105 B. Shown on the sign-in page."
//
// What is checked, with the standard library only, and without decoding a single pixel (a picture of 8 MiB
// can unpack to gigabytes):
//
//   - PNG:  the header as image/png reads it (DecodeConfig: signature, IHDR with its checksum, a size), then
//     every chunk to the end: lengths that fit, checksums that match, image data, IEND as the last
//     chunk, nothing after it.
//   - JPEG: the header as image/jpeg reads it (DecodeConfig: the segments up to the frame header, a size),
//     then every segment and every scan to the end-of-image marker.
//   - GIF:  the header as image/gif reads it (DecodeConfig), then every block to the trailer, with at least
//     one image in between.
//   - WebP: the RIFF container (the standard library has no WebP decoder): the size it states is the file's,
//     the first chunk is a bitstream or the extended header with a size that is not zero, every
//     chunk fits, and there is image data.
//   - AVIF: the box structure (no decoder either): every box fits, the file type box names AVIF, and there
//     is a meta box and media data.
func Whole(mime string, d []byte) bool {
	switch mime {
	case "image/png":
		return wholePNG(d)
	case "image/jpeg":
		return wholeJPEG(d)
	case "image/gif":
		return wholeGIF(d)
	case "image/webp":
		return wholeWebP(d)
	case "image/avif":
		return wholeAVIF(d)
	}
	return false
}

func wholePNG(d []byte) bool {
	if c, err := png.DecodeConfig(bytes.NewReader(d)); err != nil || c.Width <= 0 || c.Height <= 0 {
		return false
	}
	data := false
	for p, first := 8, true; ; first = false {
		if len(d)-p < 12 {
			return false
		}
		n := int(binary.BigEndian.Uint32(d[p:]))
		if n < 0 || n > len(d)-p-12 {
			return false
		}
		kind, end := string(d[p+4:p+8]), p+8+n
		if crc32.ChecksumIEEE(d[p+4:end]) != binary.BigEndian.Uint32(d[end:]) {
			return false
		}
		p = end + 4
		switch {
		case first != (kind == "IHDR"): // the header is the first chunk, and there is one
			return false
		case kind == "IDAT":
			data = data || n > 0
		case kind == "IEND":
			return data && n == 0 && p == len(d)
		}
	}
}

func wholeJPEG(d []byte) bool {
	if c, err := jpeg.DecodeConfig(bytes.NewReader(d)); err != nil || c.Width <= 0 || c.Height <= 0 {
		return false
	}
	if len(d) < 4 || d[0] != 0xff || d[1] != 0xd8 {
		return false
	}
	frame, scan := false, false
	for p := 2; ; {
		if len(d)-p < 2 || d[p] != 0xff {
			return false
		}
		m := d[p+1]
		p += 2
		switch {
		case m == 0xff: // a fill byte before a marker
			p--
			continue
		case m == 0xd9: // end of image; cameras add data of their own after it
			return frame && scan
		case m == 0x00 || m == 0x01 || (m >= 0xd0 && m <= 0xd8): // no marker for this place
			return false
		}
		if len(d)-p < 2 {
			return false
		}
		n := int(binary.BigEndian.Uint16(d[p:]))
		if n < 2 || n > len(d)-p {
			return false
		}
		p += n
		switch {
		case m >= 0xc0 && m <= 0xcf && m != 0xc4 && m != 0xc8 && m != 0xcc: // a frame header
			frame = true
		case m == 0xda: // a scan: its data runs to the next marker that is not a restart or a stuffed byte
			if !frame {
				return false
			}
			scan = true
			for {
				i := bytes.IndexByte(d[p:], 0xff)
				if i < 0 || len(d)-p-i < 2 {
					return false
				}
				p += i
				if x := d[p+1]; x == 0x00 || (x >= 0xd0 && x <= 0xd7) {
					p += 2
					continue
				} else if x == 0xff {
					p++
					continue
				}
				break
			}
		}
	}
}

func wholeGIF(d []byte) bool {
	c, err := gif.DecodeConfig(bytes.NewReader(d))
	if err != nil || c.Width <= 0 || c.Height <= 0 || len(d) < 13 {
		return false
	}
	table := func(flags byte) int { // the bytes of a colour table, from the packed field before it
		if flags&0x80 == 0 {
			return 0
		}
		return 3 << ((flags & 7) + 1)
	}
	p := 13 + table(d[10])
	blocks := func() bool { // data sub-blocks up to the empty one
		for {
			if p >= len(d) {
				return false
			}
			n := int(d[p])
			p += 1 + n
			if n == 0 {
				return p <= len(d)
			}
		}
	}
	images := 0
	for {
		if p >= len(d) {
			return false
		}
		switch d[p] {
		case 0x3b: // the trailer
			return images > 0
		case 0x21: // an extension: its label, then sub-blocks
			p += 2
			if !blocks() {
				return false
			}
		case 0x2c: // an image: its descriptor, its colour table, the code size, then sub-blocks
			if len(d)-p < 10 {
				return false
			}
			p += 10 + table(d[p+9]) + 1
			if !blocks() {
				return false
			}
			images++
		default:
			return false
		}
	}
}

func wholeWebP(d []byte) bool {
	if len(d) < 20 || string(d[:4]) != "RIFF" || string(d[8:12]) != "WEBP" {
		return false
	}
	if size := uint64(binary.LittleEndian.Uint32(d[4:8])); size+8 != uint64(len(d)) || size%2 != 0 {
		return false
	}
	image := false
	for p, first := 12, true; p < len(d); first = false {
		if len(d)-p < 8 {
			return false
		}
		kind, n := string(d[p:p+4]), uint64(binary.LittleEndian.Uint32(d[p+4:p+8]))
		if n > uint64(len(d)-p-8) {
			return false
		}
		body := d[p+8 : p+8+int(n)]
		switch kind {
		case "VP8 ": // lossy: a key frame's tag, the start code, 14 bits of width and of height
			if len(body) < 10 || body[0]&1 != 0 || body[3] != 0x9d || body[4] != 0x01 || body[5] != 0x2a ||
				binary.LittleEndian.Uint16(body[6:])&0x3fff == 0 || binary.LittleEndian.Uint16(body[8:])&0x3fff == 0 {
				return false
			}
			image = true
		case "VP8L": // lossless: the signature, 14 bits of width-1 and height-1, version 0
			if len(body) < 5 || body[0] != 0x2f || body[4]>>5 != 0 {
				return false
			}
			image = true
		case "VP8X": // extended: flags and a canvas; the image data follows in chunks of its own
			if !first || len(body) != 10 {
				return false
			}
		case "ANMF": // a frame of an animation
			image = image || len(body) > 16
		default:
			if first { // the first chunk says what kind of WebP this is
				return false
			}
		}
		p += 8 + int(n) + int(n%2) // chunks are padded to an even length
		if p > len(d) {
			return false
		}
	}
	return image
}

func wholeAVIF(d []byte) bool {
	ftyp, meta, media := false, false, false
	for p, first := 0, true; p < len(d); first = false {
		if len(d)-p < 8 {
			return false
		}
		n, kind, head := uint64(binary.BigEndian.Uint32(d[p:])), string(d[p+4:p+8]), 8
		switch n {
		case 0: // to the end of the file
			n = uint64(len(d) - p)
		case 1: // a 64-bit size follows
			if len(d)-p < 16 {
				return false
			}
			n, head = binary.BigEndian.Uint64(d[p+8:]), 16
		}
		if n < uint64(head) || n > uint64(len(d)-p) {
			return false
		}
		body := d[p+head : p+int(n)]
		switch {
		case first != (kind == "ftyp"): // the file type box is the first box, and there is one
			return false
		case kind == "ftyp":
			if len(body) < 8 || len(body)%4 != 0 {
				return false
			}
			for i := 0; i+4 <= len(body); i += 4 {
				if i != 4 && (string(body[i:i+4]) == "avif" || string(body[i:i+4]) == "avis") { // bytes 4-8 are a version
					ftyp = true
				}
			}
		case kind == "meta":
			meta = len(body) > 4
		case kind == "mdat" || kind == "moov":
			media = media || len(body) > 0
		}
		p += int(n)
	}
	return ftyp && meta && media
}

// The largest picture, in pixels, that is accepted at upload. The limit on the file (MaxBackgroundBytes) says
// nothing about it: a white PNG of 20000 x 20000 pixels is 76 kB, and a browser that paints it needs four
// bytes for each of its 400 million pixels. As the sign-in picture that is every visitor's tab (r2-p3-02).
// 8192 a side and 40 megapixels hold every screen and every camera picture one would use as a background
// (an 8K screen is 7680 x 4320, 33 megapixels).
const (
	MaxPictureSide   = 8192
	MaxPicturePixels = 40_000_000
)

// PictureSize is the width and height in pixels that a picture of the media type mime states in its header
// (as Sniff names the type). ok is false when the file states none that can be read. Nothing is decoded.
func PictureSize(mime string, d []byte) (w, h int, ok bool) {
	switch mime {
	case "image/png":
		c, err := png.DecodeConfig(bytes.NewReader(d))
		return c.Width, c.Height, err == nil
	case "image/jpeg":
		c, err := jpeg.DecodeConfig(bytes.NewReader(d))
		return c.Width, c.Height, err == nil
	case "image/gif":
		c, err := gif.DecodeConfig(bytes.NewReader(d))
		return c.Width, c.Height, err == nil
	case "image/webp": // the first chunk: the canvas of an extended file, else the bitstream's own size
		if len(d) < 30 || string(d[:4]) != "RIFF" || string(d[8:12]) != "WEBP" {
			return 0, 0, false
		}
		body := d[20:]
		switch string(d[12:16]) {
		case "VP8X":
			// 24 bits of width-1 and of height-1. The brackets matter: in Go "+" binds like "|", so without them
			// the 1 was added to the low byte before the OR, and 512 x 512 read as 256 x 256.
			return 1 + (int(body[4]) | int(body[5])<<8 | int(body[6])<<16), 1 + (int(body[7]) | int(body[8])<<8 | int(body[9])<<16), true
		case "VP8 ":
			return int(binary.LittleEndian.Uint16(body[6:]) & 0x3fff), int(binary.LittleEndian.Uint16(body[8:]) & 0x3fff), true
		case "VP8L":
			bits := binary.LittleEndian.Uint32(body[1:])
			return int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, true
		}
		return 0, 0, false
	case "image/avif": // the "image spatial extents" boxes: the largest width and height any item states
		for i := 4; i+16 <= len(d); i++ {
			if string(d[i:i+4]) != "ispe" || binary.BigEndian.Uint32(d[i-4:]) != 20 {
				continue
			}
			w, h, ok = max(w, int(binary.BigEndian.Uint32(d[i+8:]))), max(h, int(binary.BigEndian.Uint32(d[i+12:]))), true
		}
		return w, h, ok
	}
	return 0, 0, false
}

// PixelProblem says in plain words why a picture is not accepted for its size in pixels ("" when it is fine).
func PixelProblem(mime string, d []byte) string {
	w, h, ok := PictureSize(mime, d)
	switch {
	case !ok || w <= 0 || h <= 0:
		return "the picture does not say how many pixels it has, so it is not accepted"
	case w > MaxPictureSide || h > MaxPictureSide || int64(w)*int64(h) > MaxPicturePixels:
		return fmt.Sprintf("the picture is %d x %d pixels; at most %d pixels a side and %d megapixels are accepted (make it smaller and upload it again)",
			w, h, MaxPictureSide, MaxPicturePixels/1_000_000)
	}
	return ""
}
