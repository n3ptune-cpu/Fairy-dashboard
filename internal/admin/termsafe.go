package admin

import (
	"io"
	"strings"
	"unicode/utf8"
)

// Much of what `fairy admin` prints comes from elsewhere: audit outcomes with agents' error texts, user names
// from the identity provider, host names and errors in the health file. A terminal would act on escape
// sequences in them (colours, moving the cursor, rewriting earlier lines, setting the window title, OSC 52
// clipboard writes on some terminals), so everything the CLI writes goes through termSafe.

// unsafeRune reports runes a terminal may act on or that can disguise text: C0 controls except tab and
// newline (ESC, CR, BEL, backspace...), DEL, C1 controls (8-bit CSI / OSC), and bidi overrides.
func unsafeRune(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e || r == 0x200f || r == 0x061c:
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
