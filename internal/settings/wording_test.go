package settings

import (
	"errors"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/apierr"
)

func detail(err error) string {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		s, _ := ae.Detail.(string)
		return s
	}
	return ""
}

// r2-b1-12 (bug run of audit 5): the two upload boxes of the Settings page (your background, the sign-in
// picture) worded the same refusal differently: "image too large" and "picture larger than 8 MiB", "file
// content does not match its declared image type" and "not a PNG, JPEG or WebP picture (...)", and "empty
// body" for a file. One wording per case; only the list of types differs, because the two take different ones.
func TestTheTwoUploadsWordTheSameRefusalAlike(t *testing.T) {
	st := NewStore(t.TempDir())
	big := make([]byte, MaxBackgroundBytes+1)
	text := []byte("just some notes, renamed to notes.png\n")
	bg := func(ct string, d []byte) string { _, err := ValidateUpload(ct, d); return detail(err) }
	sign := func(d []byte) string { _, err := st.SaveSignIn(d); return detail(err) }
	for _, c := range []struct{ what, background, signIn string }{
		{"too large", bg("image/png", big), sign(big)},
		{"empty", bg("image/png", nil), sign(nil)},
	} {
		if c.background == "" || c.background != c.signIn {
			t.Errorf("%s: the background says %q, the sign-in picture %q", c.what, c.background, c.signIn)
		}
	}
	if got := bg("image/png", big); got != "picture larger than 8 MiB" {
		t.Errorf("too large: %q", got)
	}
	if got := bg("image/png", nil); got != "the file is empty" {
		t.Errorf("empty: %q", got)
	}
	// a file that is no picture: the same sentence, each with the types it takes
	const tail = " picture (the file's content decides, not its name; SVG is not accepted)"
	if got := bg("image/png", text); got != "not a PNG, JPEG, WebP, GIF or AVIF"+tail {
		t.Errorf("background, a text file sent as image/png: %q", got)
	}
	if got := sign(text); got != "not a PNG, JPEG or WebP"+tail {
		t.Errorf("sign-in picture, a text file: %q", got)
	}
	// a type the background does not take at all names what was sent
	if got := bg("image/svg+xml", []byte("<svg/>")); got != "not a PNG, JPEG, WebP, GIF or AVIF picture (it was sent as image/svg+xml; SVG is not accepted)" {
		t.Errorf("background, an SVG: %q", got)
	}
	// a real picture under another type's name keeps its own words (the sign-in picture goes by the content alone)
	for name, p := range realPictures(t) {
		if p[0].(string) == "image/png" {
			continue
		}
		if got := bg("image/png", p[1].([]byte)); got != "file content does not match its declared image type" {
			t.Errorf("%s sent as image/png: %q", name, got)
		}
	}
}
