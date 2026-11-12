package textsafe

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"":             "",
		"plain host-1": "plain host-1",
		"Größe 22 °C · Сервер 服务器 🖥":    "Größe 22 °C · Сервер 服务器 🖥",
		"family 👨\u200d👩\u200d👧 stays":  "family 👨\u200d👩\u200d👧 stays", // zero width joiners belong to the emoji
		"a\x00b":                        "ab",
		"\x00\x01\x02":                  "",
		"\x7f":                          "",
		"line\nbreak":                   "line break",
		"cr\r\nlf":                      "cr  lf",
		"tab\tx":                        "tab x",
		"nel\u0085x":                    "nel x",
		"ls\u2028ps\u2029x":             "ls ps x",
		"\x1b[31mred\x1b[0m":            "red",
		"\x1b[2J\x1b[Hcleared":          "cleared",
		"\x1b]0;window title\x07name":   "name",
		"\x1b]8;;http://evil\x1b\\link": "link",
		"\u009b31mred":                  "red",
		"\u009d0;title\u009cname":       "name",
		"\x1bcreset":                    "reset",
		"cut off \x1b":                  "cut off ",
		"cut off \x1b[":                 "cut off ",
		"cut off \x1b[1;3":              "cut off ",
		"\x1b[\x1b[31mnested":           "nested",
		"evil\u202egnp.exe":             "evilgnp.exe",
		"\u2066iso\u2069late":           "isolate",
		"lrm\u200e rlm\u200f alm\u061c": "lrm rlm alm",
		"\ufeffbom":                     "bom",
		"\xff\xfe":                      "",
		"ok\xc3":                        "ok",
		"c1 \u0080\u0090\u009f end":     "c1  end",
		"bell\x07":                      "bell",
		"back\x08space":                 "backspace",
		"\x1b]0;" + strings.Repeat("t", 400) + "after": strings.Repeat("t", 146) + "after", // one that never ends takes 256 bytes with it
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
	// a clean text is returned as it is, not copied
	s := "nothing to do here: 100 % ünïcödé"
	if got := Clean(s); got != s {
		t.Fatal(got)
	}
}

// Whatever goes in: valid UTF-8 comes out, with no control character, no escape, no direction mark, and
// cleaning it again changes nothing.
func FuzzClean(f *testing.F) {
	for _, s := range []string{"", "host", "\x1b[31m", "\x1b]0;x\x07", "a\nb", "\u202e", "\xff", "\x1b", "\u009b1m", "é\x00", "\x1b[\x1b]\x1b",
		"a\u200bb", "👨\u200d👩", "👨\u200b\u200d👩", "❤\ufe0f\u200d🔥", "👨\ufe0f\ufe0f", "\u200d👩", "👨\u200d", "x\u3164\u115fy", "🏴\U000e0067\U000e007f"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		c := Clean(s)
		if !utf8.ValidString(c) {
			t.Fatalf("Clean(%q) is not UTF-8: %q", s, c)
		}
		for _, r := range c {
			if unicode.IsControl(r) || unwanted(r) || r == 0x2028 || r == 0x2029 {
				t.Fatalf("Clean(%q) = %q still has %U", s, c, r)
			}
		}
		rs := []rune(c)
		for i, r := range rs { // an invisible character is left only where it makes an emoji
			if !Invisible(r) {
				continue
			}
			joins := r == 0x200d && i > 0 && i+1 < len(rs) && picture(rs[i+1]) && (picture(rs[i-1]) || i > 1 && rs[i-1] == 0xfe0f || i > 1 && rs[i-1] == 0xfe0e)
			selects := (r == 0xfe0e || r == 0xfe0f) && i > 0 && picture(rs[i-1])
			if !joins && !selects {
				t.Fatalf("Clean(%q) = %q still has the invisible %U at %d", s, c, r, i)
			}
		}
		if again := Clean(c); again != c {
			t.Fatalf("Clean is not stable: %q -> %q -> %q", s, c, again)
		}
		if len(c) > len(s) {
			t.Fatalf("Clean(%q) grew to %q", s, c)
		}
		if k := Clip(s, 16); len(k) > 16+len("…") || !utf8.ValidString(k) || strings.TrimSpace(k) != k {
			t.Fatalf("Clip(%q, 16) = %q", s, k)
		}
	})
}

func TestClip(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"  lead and trail  ", 64, "lead and trail"},
		{"\n\x1b[1mname\x1b[0m\n", 64, "name"},
		{strings.Repeat("a", 63) + "é", 64, strings.Repeat("a", 63) + "…"}, // never cut inside a character
		{strings.Repeat("a", 64), 64, strings.Repeat("a", 64)},
		{strings.Repeat("a", 65), 64, strings.Repeat("a", 64) + "…"},
		{"\x00\x00\x00", 4, ""},
	} {
		if got := Clip(tc.in, tc.n); got != tc.want {
			t.Errorf("Clip(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
		// Cut is the same without the mark that something was cut
		if got, want := Cut(tc.in, tc.n), strings.TrimSuffix(tc.want, "…"); got != want {
			t.Errorf("Cut(%q, %d) = %q, want %q", tc.in, tc.n, got, want)
		}
	}
	if got := Cut("ab   cd", 4); got != "ab" { // no space left at the end of a cut
		t.Errorf("%q", got)
	}
}

func TestCleanJSON(t *testing.T) {
	var v any
	in := `{"hostname":"evil\u001b[31m\nhost","n":1.5,"ok":true,"none":null,
		"list":["a\u0000b",{"deep\u202e":"x\ty","fine":"fine"},[["z\r"]],7],
		"key\nwith break":"v","key with break":"the clean key was here first"}`
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatal(err)
	}
	got := CleanJSON(v)
	want := map[string]any{"hostname": "evil host", "n": 1.5, "ok": true, "none": nil,
		"list":           []any{"ab", map[string]any{"deep": "x y", "fine": "fine"}, []any{[]any{"z "}}, 7.0},
		"key with break": "the clean key was here first"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got %#v\nwant %#v", got, want)
	}
	// very deep nesting is dropped, not followed without end
	deep := any("x\x00")
	for i := 0; i < 100; i++ {
		deep = []any{deep}
	}
	out := CleanJSON(deep)
	depth := 0
	for {
		l, ok := out.([]any)
		if !ok || len(l) == 0 {
			break
		}
		out = l[0]
		depth++
	}
	if depth > maxDepth || out != nil {
		t.Fatalf("followed %d levels, ended in %v", depth, out)
	}
	if CleanJSON(nil) != nil || CleanJSON(3.0) != 3.0 || CleanJSON("a\x07") != "a" {
		t.Fatal("scalars")
	}
}

// T-06: an error text the panel keeps is short and clean, and one that is both already is not copied.
func TestErrorText(t *testing.T) {
	if got := ErrorText("timeout"); got != "timeout" {
		t.Fatalf("%q", got)
	}
	long := "x\x1b[2J\r\n\u202e" + strings.Repeat("é", 200_000)
	got := ErrorText(long)
	if len(got) > MaxError+len("…") || Clean(got) != got || !strings.HasPrefix(got, "x  é") || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Fatalf("%d bytes: %.40q", len(got), got)
	}
	if ErrorText(got) != got {
		t.Fatal("a clipped text is clipped again")
	}
}

// r2-p3-09: a host's name kept characters that show as nothing (the auditor's agent a17 was "gnp.exe" + four
// zero-width characters + "x" + a soft hyphen and three blank ones + "end": 19 characters that read
// "gnp.exexend"), so one host could take a name that reads exactly like another's. "Set model name" refused
// these already; now no text from a machine keeps them. What makes an emoji stays.
func TestCleanDropsWhatShowsAsNothing(t *testing.T) {
	a17 := "gnp.exe\u200b\u200c\u200d\u2060x\u00ad\u034f\u3164\u115fend"
	if got := Clean(a17); got != "gnp.exexend" {
		t.Errorf("the auditor's name: %q (%d characters)", got, len([]rune(got)))
	}
	for in, want := range map[string]string{
		"srv\u200b-01":                         "srv-01", // zero-width space
		"srv\u200c\u200d-01":                   "srv-01", // non-joiner and joiner between letters
		"srv\u2060\u2061-01":                   "srv-01", // word joiner, function application
		"soft\u00adhyphen":                     "softhyphen",
		"hangul\u3164\u115f\u1160\uffa0filler": "hangulfiller",
		"braille\u2800blank":                   "brailleblank",
		"cgj\u034fkhmer\u17b4\u17b5":           "cgjkhmer",
		"selector\ufe0f on a letter\ufe00":     "selector on a letter",
		"tags\U000e0041\U000e007f":             "tags",
		"mongolian\u180e\u180b":                "mongolian",
		"ideographic\U000e0100 variant":        "ideographic variant",
		// an emoji keeps what it is made of
		"👨\u200d👩\u200d👧":    "👨\u200d👩\u200d👧",
		"❤\ufe0f\u200d🔥 hot": "❤\ufe0f\u200d🔥 hot",
		"🖥\ufe0f tower":      "🖥\ufe0f tower",
		"text form ☎\ufe0e":  "text form ☎\ufe0e",
		"🧑🏽\u200d💻":          "🧑🏽\u200d💻", // with a skin tone
		// but nothing more than that
		"🖥\ufe0f\ufe0f\ufe0f":             "🖥\ufe0f", // one selector
		"👨\u200d":                         "👨",       // a joiner that joins nothing
		"\u200d👩":                         "👩",
		"👨\u200dx":                        "👨x",       // or joins a letter
		"👨\u200b\u200d👩":                  "👨\u200d👩", // the zero-width space goes, the emoji stays one
		"👨\u200d\u200d👩":                  "👨\u200d👩",
		"🏴\U000e0067\U000e0062\U000e007f": "🏴", // the letters of a region's flag are tags
		// spaces a person sees stay
		"no\u00a0break and ideographic\u3000space": "no\u00a0break and ideographic\u3000space",
	} {
		got := Clean(in)
		if got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
		if again := Clean(got); again != got {
			t.Errorf("Clean is not stable for %q: %q then %q", in, got, again)
		}
	}
	// two hosts that differed only in what cannot be seen now have the same name
	if Clean("nas\u200b") != Clean("nas") || Clean("n\u2060as") != "nas" {
		t.Error("names that read alike still differ")
	}
}
