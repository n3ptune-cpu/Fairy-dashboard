// Package textsafe cleans texts that come from machines the panel does not control (agents, the devices they
// relay, a Proxmox API) before the panel keeps, shows or logs them.
//
// Such a text is data, but some characters in it act on whatever shows it: a terminal runs ANSI escape
// sequences (colours, cursor moves, a new window title), a line break starts a new line in a log, a file or a
// chat message, a direction override (U+202E) makes a name read like another one, and a NUL ends the text for
// some programs. None of them belongs in a host name, a sensor label, a version or an error message. HTML is
// not this package's business: the page escapes every text where it inserts it.
package textsafe

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Clean returns s without the characters that act on what shows it:
//
//   - ANSI escape sequences are removed as a whole (ESC [ ... final byte, ESC ] ... BEL or ST, ESC and one
//     more character), also in their one-character C1 forms (U+009B, U+009D);
//   - line breaks and tabs (and the Unicode line ends U+0085, U+2028, U+2029) become one space;
//   - every other C0 and C1 control character and DEL is removed;
//   - direction marks, embeddings, overrides and isolates (U+061C, U+200E, U+200F, U+202A to U+202E, U+2066 to
//     U+2069) and the byte order mark (U+FEFF) are removed;
//   - characters that show as nothing are removed: the format characters (zero-width space and joiners, word
//     joiner, soft hyphen, tags, ...), the blank letters (Hangul fillers, the combining grapheme joiner, ...) and
//     variation selectors. With them two names could read exactly alike. The ones that make an emoji stay: a
//     joiner between two pictures, and the one selector (text or picture form) right after a picture;
//   - bytes that are not UTF-8 are removed.
//
// A text without any of these is returned as it is.
func Clean(s string) string {
	if !dirty(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	// what the last character kept is: nothing special, a picture (an emoji), or a picture and its selector
	const (
		other = iota
		pic
		picSel
	)
	last := other
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if Invisible(r) {
			next, nn := utf8.DecodeRuneInString(s[i+n:])
			if next == utf8.RuneError && nn <= 1 { // the end of the text, or a byte that is not UTF-8
				next = 0
			}
			switch {
			case r == 0x200d && last != other && picture(next): // a joiner between two pictures: one emoji
				b.WriteString(s[i : i+n])
			case (r == 0xfe0e || r == 0xfe0f) && last == pic: // the text or picture form of the emoji before it
				b.WriteString(s[i : i+n])
				last = picSel
			}
			i += n
			continue
		}
		was := last
		last = other
		switch {
		case r == utf8.RuneError && n <= 1: // not UTF-8
			i += n
		case r == 0x1b:
			i += escapeLen(s[i:])
		case r == 0x9b: // CSI in one character
			i += n + csiLen(s[i+n:])
		case r == 0x9d: // OSC in one character
			i += n + oscLen(s[i+n:])
		case r == '\n' || r == '\r' || r == '\t' || r == '\v' || r == '\f' || r == 0x85 || r == 0x2028 || r == 0x2029:
			b.WriteByte(' ')
			i += n
		case unwanted(r): // removed: what was kept before it is still the last character kept
			last = was
			i += n
		default:
			if picture(r) {
				last = pic
			}
			b.WriteString(s[i : i+n])
			i += n
		}
	}
	return b.String()
}

// Invisible reports a character that shows as nothing and is no space: a format character (Unicode Cf: the
// zero-width space and joiners, the word joiner, the soft hyphen, tags, ...), a blank letter (the combining
// grapheme joiner, the Hangul fillers, two Khmer inherent vowels, the empty Braille pattern) or a variation
// selector. "Set model name" refuses the same ones (adminstate.ModelName).
func Invisible(r rune) bool {
	switch {
	case r < 0xad:
		return false
	case unicode.Is(unicode.Cf, r):
		return true
	case r == 0x034f, r == 0x115f, r == 0x1160, r == 0x17b4, r == 0x17b5, r == 0x3164, r == 0xffa0, r == 0x2800:
		return true
	case r >= 0x180b && r <= 0x180f, r >= 0xfe00 && r <= 0xfe0f, r >= 0xe0100 && r <= 0xe01ef:
		return true
	}
	return false
}

// picture reports an emoji or another symbol drawn as a picture.
func picture(r rune) bool {
	return !Invisible(r) && (r >= 0x1f000 && r <= 0x1faff || unicode.Is(unicode.So, r))
}

// dirty reports whether Clean would change s.
func dirty(s string) bool {
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			return true
		}
		if c < utf8.RuneSelf {
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && n <= 1) || unwanted(r) || Invisible(r) || r == 0x85 || r == 0x2028 || r == 0x2029 {
			return true
		}
		i += n
	}
	return false
}

// unwanted reports the characters Clean removes (the ones it turns into a space are handled before).
func unwanted(r rune) bool {
	switch {
	case r < 0x20, r == 0x7f, r >= 0x80 && r < 0xa0:
		return true
	case r == 0x061c, r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0xfeff:
		return true
	}
	return false
}

// escapeLen is the length of the escape sequence at the start of s (s[0] is ESC): the whole sequence when it
// is a CSI or OSC sequence, ESC and the next character for the two-character forms, ESC alone at the end.
func escapeLen(s string) int {
	if len(s) < 2 {
		return 1
	}
	switch s[1] {
	case '[':
		return 2 + csiLen(s[2:])
	case ']', 'P', 'X', '^', '_': // OSC, DCS, SOS, PM, APC: a string up to BEL or ST
		return 2 + oscLen(s[2:])
	}
	if s[1] >= 0x20 && s[1] < 0x7f {
		return 2
	}
	return 1
}

// csiLen is the length of the rest of a CSI sequence: parameter and intermediate bytes (0x20 to 0x3f) and the
// final byte (0x40 to 0x7e). A sequence that is cut off or broken ends where it stops being one.
func csiLen(s string) int {
	i := 0
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x3f {
		i++
	}
	if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
		i++
	}
	return i
}

// oscLen is the length of the rest of a string sequence: up to and including BEL, or ST (ESC \ or U+009C). One
// that never ends takes the rest of the text with it (at most 256 bytes: a real one is a window title).
func oscLen(s string) int {
	for i := 0; i < len(s) && i < 256; i++ {
		switch {
		case s[i] == 0x07:
			return i + 1
		case s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\':
			return i + 2
		case s[i] == 0xc2 && i+1 < len(s) && s[i+1] == 0x9c:
			return i + 2
		}
	}
	return min(len(s), 256)
}

// Clip is Clean, without spaces at either end, cut to at most n bytes (on a character boundary, with "…"
// added when something was cut).
func Clip(s string, n int) string {
	s = strings.TrimSpace(Clean(s))
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// MaxError is how many bytes of an error text the panel keeps for a system or a source (ErrorText).
const MaxError = 256

// ErrorText is the one way an error text gets into what the panel keeps and hands out about a system or a
// source (the snapshot, the stream, health.json, a notification, the log): cleaned and cut to MaxError bytes
// (Clip). An error text can repeat what the other side said (T-06: the names in a certificate), and one text
// is kept for every node and guest of a cluster, so it must be short wherever it comes from. A text that is
// short and clean already is returned as it is.
func ErrorText(s string) string { return Clip(s, MaxError) }

// Cut is Clip without the "…": for a text that is compared or shown inside a sentence of the panel's own.
func Cut(s string, n int) string {
	s = strings.TrimSpace(Clean(s))
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(strings.ToValidUTF8(s[:n], ""))
}

// maxDepth bounds how deep CleanJSON follows nested lists and objects (what is deeper is dropped).
const maxDepth = 32

// CleanJSON cleans every text in a decoded JSON value (the values and the keys of objects, the items of
// lists, at any depth), in place where it can, and returns the value. Numbers, booleans and null are left
// alone. It is applied to every answer of an agent where the answer enters the panel, so no part of the panel
// sees an uncleaned text, whatever it later does with the answer.
func CleanJSON(v any) any { return cleanJSON(v, 0) }

func cleanJSON(v any, depth int) any {
	switch x := v.(type) {
	case string:
		return Clean(x)
	case map[string]any:
		if depth >= maxDepth {
			return nil
		}
		for k, val := range x {
			c := cleanJSON(val, depth+1)
			if ck := Clean(k); ck != k {
				delete(x, k)
				if _, taken := x[ck]; !taken { // a cleaned key never replaces a key that was clean to begin with
					x[ck] = c
				}
				continue
			}
			x[k] = c
		}
		return x
	case []any:
		if depth >= maxDepth {
			return nil
		}
		for i := range x {
			x[i] = cleanJSON(x[i], depth+1)
		}
		return x
	}
	return v
}
