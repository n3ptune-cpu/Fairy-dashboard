package access

import (
	"strings"
	"testing"
	"unicode"
)

// A user name with a character in it that draws nothing was shown plain next to the admin's: only the format
// characters (Unicode category Cf) were removed, and there are blank characters of other categories: the
// variation selectors, the combining grapheme joiner, the Hangul fillers, the Braille blank. Two spaces between
// two words, a no-break space or an em space in the place of the space were kept too, and a page draws each
// of them like one space.
//
// All of them are removed where a user name comes in (every kind of space becomes one plain space), and the
// form two texts are compared in does the same.
func TestAUsernameHasNoBlankCharacters(t *testing.T) {
	blank := map[string][]rune{
		"a variation selector (U+FE00 to U+FE0F)":                            {0xFE00, 0xFE01, 0xFE0E, 0xFE0F},
		"a variation selector supplement (U+E0100 to E01EF)":                 {0xE0100, 0xE0101, 0xE01EE, 0xE01EF},
		"the combining grapheme joiner U+034F":                               {0x034F},
		"a Hangul filler":                                                    {0x115F, 0x1160, 0x3164, 0xFFA0},
		"the Braille blank U+2800":                                           {0x2800},
		"a Mongolian variation selector or vowel separator":                  {0x180B, 0x180C, 0x180D, 0x180E, 0x180F},
		"a format character (Cf)":                                            {0x200B, 0x200C, 0x200D, 0x2060, 0x00AD, 0xFEFF},
		"another one a text is drawn without (Default_Ignorable_Code_Point)": {0x17B4, 0x17B5, 0x2065, 0xFFF0, 0xFFF8, 0xE0000, 0xE0001, 0xE0020, 0xE007F, 0xE0080, 0xE00FF, 0xE01F0, 0xE0FFF},
	}
	tr := true
	admin := User{Sub: "3f6c1d00-admin", Username: "Juniper", Email: "owner@example.com", EmailVerified: &tr, Name: "Real Name", Role: RoleAdmin, CreatedAt: *at(1)}
	for what, runes := range blank {
		for _, r := range runes {
			for _, name := range []string{"Juniper" + string(r), string(r) + "Juniper", "Juni" + string(r) + "per", "Juni" + string(r) + string(r) + "per" + string(r)} {
				if got := CleanUsername(name); got != "Juniper" {
					t.Errorf("%s, U+%04X: CleanUsername(%+q) = %+q", what, r, name, got)
				}
				if fold(name) != fold("Juniper") {
					t.Errorf("%s, U+%04X: %+q is compared as %+q, not as Juniper is", what, r, name, fold(name))
				}
				// an account that waits and calls itself so is not shown by the plain name next to the admin
				w := User{Sub: "9a1b2c3d-wanda", Username: name, Email: "wanda@example.org", Role: RolePending, CreatedAt: *at(1)}
				p := Data{Users: map[string]User{admin.Sub: admin, w.Sub: w}}.People()
				if a, _ := p.Of(admin.Sub); a != "Juniper" {
					t.Errorf("%s, U+%04X: the admin is shown by %+q", what, r, a)
				}
				if got, _ := p.Of(w.Sub); got != "Juniper · id 9a1b2c" || p.UsernameNote(w.Sub) != UsernameShared {
					t.Errorf("%s, U+%04X: the account that calls itself %+q is shown by %+q (note %q)", what, r, name, got, p.UsernameNote(w.Sub))
				}
			}
			if got := CleanUsername(string(r) + string(r)); got != "" {
				t.Errorf("%s, U+%04X: a user name of nothing else is kept as %+q", what, r, got)
			}
		}
	}
	// every kind of space is one plain space, and a run of them is one
	spaces := []rune{' ', 0x00A0, 0x1680, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200A, 0x202F, 0x205F, 0x3000, '\t', '\n', 0x2028}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.Is(unicode.Zs, r) && !strings.ContainsRune(string(spaces), r) {
			t.Errorf("the list of this test has no U+%04X, a space (category Zs)", r)
		}
	}
	pine := User{Sub: "aaaaaa11-admin", Username: "Pine Top", Email: "pine@example.com", Role: RoleAdmin, CreatedAt: *at(1)}
	for _, r := range spaces {
		for _, name := range []string{"Pine" + string(r) + "Top", "Pine" + string(r) + string(r) + "Top", "Pine " + string(r) + " Top", string(r) + "Pine" + string(r) + "Top" + string(r), "Pine" + string(r) + "​" + string(r) + "Top"} {
			if got := CleanUsername(name); got != "Pine Top" {
				t.Errorf("a space U+%04X: CleanUsername(%+q) = %+q", r, name, got)
			}
			if fold(name) != fold("Pine Top") {
				t.Errorf("a space U+%04X: %+q is compared as %+q", r, name, fold(name))
			}
			w := User{Sub: "bbbbbb22-wait", Username: name, Email: "w@example.org", Role: RolePending, CreatedAt: *at(1)}
			p := Data{Users: map[string]User{pine.Sub: pine, w.Sub: w}}.People()
			if got, _ := p.Of(w.Sub); got != "Pine Top · id bbbbbb" {
				t.Errorf("a space U+%04X: the account that calls itself %+q is shown by %+q", r, name, got)
			}
			if a, _ := p.Of(pine.Sub); a != "Pine Top" {
				t.Errorf("a space U+%04X: the admin is shown by %+q", r, a)
			}
		}
	}
	// cleaned twice is cleaned once, also at the cut
	for _, in := range []string{strings.Repeat("ab ", 30), strings.Repeat("é  ", 40), strings.Repeat("️", 100) + "x", strings.Repeat("a", 63) + " b", strings.Repeat("a", 62) + "　　b"} {
		got := CleanUsername(in)
		if again := CleanUsername(got); again != got || len(got) > MaxUsername || got != strings.TrimSpace(got) || strings.Contains(got, "  ") {
			t.Errorf("CleanUsername(%+q) = %+q, cleaned again %+q", in, got, again)
		}
	}
	// What is NOT the same name: letters of another alphabet that look alike, a letter composed and decomposed,
	// a wide letter. Both accounts are shown by their plain names; the account id tells them apart.
	for _, name := range []string{"Juniрer" /* Cyrillic er */, "Ｊuniper" /* fullwidth J */, "Junipér", "Junıper" /* dotless i */, "Juniper2"} {
		if CleanUsername(name) != name {
			t.Errorf("CleanUsername(%+q) = %+q: a letter was taken out", name, CleanUsername(name))
		}
		w := User{Sub: "9a1b2c3d-wanda", Username: name, Email: "wanda@example.org", Role: RolePending, CreatedAt: *at(1)}
		p := Data{Users: map[string]User{admin.Sub: admin, w.Sub: w}}.People()
		if got, _ := p.Of(w.Sub); got != name || p.UsernameNote(w.Sub) != "" {
			t.Errorf("a look-alike %+q is shown by %+q (note %q); this test says what the panel does, and the README says the same", name, got, p.UsernameNote(w.Sub))
		}
	}
}
