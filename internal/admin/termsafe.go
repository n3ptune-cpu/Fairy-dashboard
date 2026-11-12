package admin

import (
	"io"
	"strings"
	"unicode/utf8"
)

// Much of what `fairy admin` prints comes from elsewhere: audit outcomes with agents' error texts, user names
// from the identity provider, host names and errors in the health file. A terminal would act on escape
// sequences in them (colours, moving the cursor, rewriting earlier lines, setting the window title, OSC 52
// clipboard writes on some terminals), so everything the CLI writes goes through termSafe. Text that is one
// field of a line also goes through cell, which makes line breaks and tabs inside it visible instead of real.

// unsafeRune reports runes a terminal may act on or that can disguise text: C0 controls except tab and
// newline (ESC, CR, BEL, backspace...), DEL, C1 controls (8-bit CSI / OSC), bidi overrides and marks, and the
// byte order mark. Together with lineRune it is the set textsafe.Clean takes out of a machine's text before
// the panel keeps it (a test holds the two together); the CLI applies it again to everything it prints,
// because not everything it prints came through there (an older audit log, names from the identity provider).
func unsafeRune(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e || r == 0x200f || r == 0x061c, r == 0xfeff:
		return true
	case r == 0x2028 || r == 0x2029: // Unicode line ends (cell shows them as escapes before this is asked)
		return true
	}
	return false
}

// clean drops unsafe runes and replaces invalid UTF-8 (for one field).
func clean(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if strings.IndexFunc(s, unsafeRune) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unsafeRune(r) {
			return -1
		}
		return r
	}, s)
}

// cell makes one field of one printed row safe. clean alone keeps line breaks and tabs, and inside a field
// they would start a new row or move the columns: an agent's error text, a name from the identity provider or
// a requested address could then print lines that look like other entries. So every character that breaks a
// line or jumps a column is shown as its two-character escape (a line feed becomes a backslash and an n),
// and the rest goes through clean. Every field the CLI prints in a row or a sentence goes through cell; only
// the CLI's own line ends are real line ends.
func cell(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if strings.IndexFunc(s, lineRune) < 0 {
		return clean(s)
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch r {
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\v':
			b.WriteString(`\v`)
		case '\f':
			b.WriteString(`\f`)
		case 0x85:
			b.WriteString(`\u0085`)
		case 0x2028:
			b.WriteString(`\u2028`)
		case 0x2029:
			b.WriteString(`\u2029`)
		default:
			b.WriteRune(r)
		}
	}
	return clean(b.String())
}

// lineRune reports the characters cell shows as an escape: line feed, carriage return, tab, vertical tab, form
// feed, and the Unicode line ends NEL, LINE SEPARATOR and PARAGRAPH SEPARATOR.
func lineRune(r rune) bool {
	switch r {
	case '\n', '\r', '\t', '\v', '\f', 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// cells is cell for every value of a row.
func cells(row ...string) []string {
	for i := range row {
		row[i] = cell(row[i])
	}
	return row
}

// termSafe is an io.Writer that drops unsafe runes and invalid UTF-8 from everything written through it. A
// rune split across two writes is kept until its end arrives.
type termSafe struct {
	w    io.Writer
	tail []byte // the start of a rune whose end has not been written yet
}

func newTermSafe(w io.Writer) *termSafe {
	if t, ok := w.(*termSafe); ok {
		return t
	}
	return &termSafe{w: w}
}

func (t *termSafe) Write(p []byte) (int, error) {
	b := append(t.tail, p...)
	t.tail = nil
	// keep an incomplete rune at the end for the next write (at most 3 bytes)
	if i := lastRuneStart(b); i >= 0 && !utf8.FullRune(b[i:]) {
		t.tail = append([]byte(nil), b[i:]...)
		b = b[:i]
	}
	if _, err := io.WriteString(t.w, clean(string(b))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// lastRuneStart is the index of the last byte that may start a rune within the final 3 bytes, or -1.
func lastRuneStart(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-3; i-- {
		if utf8.RuneStart(b[i]) {
			return i
		}
	}
	return -1
}
