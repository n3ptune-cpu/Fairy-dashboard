package adminstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What a custom model name may be, class by class: what is kept, what is tidied, what is refused.
func TestModelNameRules(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Lenovo ThinkCentre M720q", "Lenovo ThinkCentre M720q"},
		{"  Gaming PC  ", "Gaming PC"},                 // spaces at the ends go
		{"Gaming    PC", "Gaming PC"},                  // a run of spaces is one space
		{"Gaming\u00a0\u3000PC", "Gaming PC"},          // also of the other space characters (no-break, ideographic)
		{"", ""},                                       // nothing: no custom name
		{"   ", ""},                                    // only spaces: the same
		{"Büro-PC (Keller) №2", "Büro-PC (Keller) №2"}, // letters of any script, punctuation
		{"游戏电脑 🎮", "游戏电脑 🎮"},
		{`<img src=x onerror=alert(1)>"'&`, `<img src=x onerror=alert(1)>"'&`}, // markup is text: kept as typed
		{strings.Repeat("x", MaxModelName), strings.Repeat("x", MaxModelName)},
		{strings.Repeat("й", MaxModelName), strings.Repeat("й", MaxModelName)}, // characters, not bytes
		// combining marks as scripts use them: one or two on a letter, up to four
		{"Büro Tiếng Việt", "Büro Tiếng Việt"},
		{"हिन्दी सर्वर", "हिन्दी सर्वर"}, {"เซิร์ฟเวอร์ ที่บ้าน", "เซิร์ฟเวอร์ ที่บ้าน"}, {"بَيْتٌ", "بَيْتٌ"},
		{"é̂̃̄", "é̂̃̄"},
		{"🏳️ ❤️ 1️⃣", "🏳️ ❤️ 1️⃣"}, // the picture form of a sign, a key cap
		{strings.Repeat("😀", MaxModelName), strings.Repeat("😀", MaxModelName)},
		{"-", "-"}, {"7", "7"}, {"№", "№"},
	} {
		got, err := ModelName(c.in)
		if err != nil || got != c.want {
			t.Errorf("ModelName(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for name, in := range map[string]string{
		"too long":                      strings.Repeat("x", MaxModelName+1),
		"too long, counted after trim":  " " + strings.Repeat("й", MaxModelName+1) + " ",
		"a line feed":                   "Gaming\nPC",
		"a carriage return":             "Gaming\rPC",
		"a tab":                         "Gaming\tPC",
		"a line feed at the end":        "Gaming PC\n",
		"NUL":                           "Gaming\x00PC",
		"escape (ANSI colour)":          "\x1b[31mred\x1b[0m",
		"DEL":                           "Gaming\x7fPC",
		"a C1 control (NEL)":            "Gaming\u0085PC",
		"the line separator":            "Gaming\u2028PC",
		"the paragraph separator":       "Gaming\u2029PC",
		"right-to-left override":        "PC \u202egnimaG",
		"left-to-right embedding":       "\u202aGaming PC",
		"a direction isolate":           "\u2066Gaming\u2069 PC",
		"right-to-left mark":            "Gaming\u200fPC",
		"Arabic letter mark":            "Gaming\u061cPC",
		"zero-width space":              "Gaming\u200bPC",
		"zero-width joiner":             "Gaming\u200dPC",
		"word joiner":                   "Gaming\u2060PC",
		"byte order mark":               "\ufeffGaming PC",
		"soft hyphen":                   "Gam\u00ading PC",
		"a tag character":               "Gaming PC\U000e0041",
		"a private-use character":       "Gaming \ue000 PC",
		"Hangul filler (shows nothing)": "\u3164",
		"empty Braille pattern":         "Gaming\u2800PC",
		"not UTF-8":                     "Gaming \xff PC",
		"a cut-off UTF-8 sequence":      "Gaming PC \xe6\xb8",
		"the replacement character":     "Gaming \ufffd PC",
		// r1-p2-10 (audit 5): a name in which nothing shows, and a tower of marks on one letter
		"only combining marks":          "\u0301\u0302\u0303",
		"one combining mark":            "\u0301",
		"marks and spaces only":         " \u0301 \u0308 ",
		"only variation selectors":      "\ufe0f\ufe0f",
		"five marks on one letter":      "Gaming PCe\u0301\u0302\u0303\u0304\u0305",
		"64 marks on one letter":        "Z" + strings.Repeat("\u0351", MaxModelName-1),
		"marks that enclose, stacked":   "A\u20dd\u20dd\u20dd\u20dd\u20dd",
		"an unassigned character alone": "\U000e0fff",
	} {
		if got, err := ModelName(in); err == nil {
			t.Errorf("%s: ModelName(%q) = %q, want an error", name, in, got)
		} else if !strings.HasPrefix(err.Error(), "model name ") || len(err.Error()) > 60 {
			t.Errorf("%s: the error is a short sentence for the dialog, got %q", name, err)
		}
	}
	// r1-b1-02 (audit 5, bug run): an emoji that is several joined into one was refused with "invisible character:
	// type it again", and typing it again gives the same for ever. It is still refused (the joiner is a format
	// character), with a sentence that says what to do. Every other invisible character keeps the old sentence.
	const joined, invisible = "model name has an emoji made of several: use a simple one", "model name has an invisible character: type it again"
	for in, want := range map[string]string{
		"Tower 👨\u200d👩\u200d👧\u200d👦": joined, // a family
		"🧑\u200d💻":          joined, // a person with a laptop
		"🏳\ufe0f\u200d🌈 PC": joined, // a flag with a rainbow (after the picture form of the flag)
		"❤\ufe0f\u200d🔥":    joined,
		"🏴\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f": joined, // the flag of a region: tag characters
		"Gaming\u200dPC":      invisible, // a joiner between letters is no emoji
		"\u200d👩":             invisible, // nor one in front of a picture
		"Gaming PC\U000e0041": invisible, // a tag character after a letter
		"PC \u202egnimaG":     invisible,
		"👩\u200b👧":            invisible, // a zero-width space is not a joiner
		"👩\ufeff":             invisible,
	} {
		if got, err := ModelName(in); err == nil || err.Error() != want {
			t.Errorf("ModelName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"Tower 👨👩👧👦", "🎮 Gaming", "🏳\ufe0f", "❤\ufe0f PC", "1\ufe0f\u20e3", "🇩🇪 Büro"} { // simple ones stay
		if got, err := ModelName(in); err != nil || got != in {
			t.Errorf("ModelName(%q) = %q, %v", in, got, err)
		}
	}
}

// Set, read, replace and remove; another host's name is left alone; the file is written in one step (no
// temporary file stays, mode 0600) and the panel's other state in it is kept.
func TestSetHostModel(t *testing.T) {
	dir := t.TempDir()
	a := New(dir)
	if err := a.SetWakeUpdate("tower", true); err != nil {
		t.Fatal(err)
	}
	if got := a.HostModels(); len(got) != 0 || a.HostModel("laptop") != "" {
		t.Fatalf("a new panel has no custom model names: %v", got)
	}
	if prev, err := a.SetHostModel("laptop", "  Gaming   PC "); err != nil || prev != "" || a.HostModel("laptop") != "Gaming PC" {
		t.Fatalf("set: prev %q, err %v, now %q", prev, err, a.HostModel("laptop"))
	}
	if prev, err := a.SetHostModel("tower", "Server"); err != nil || prev != "" {
		t.Fatal(prev, err)
	}
	if prev, err := a.SetHostModel("laptop", "Office PC"); err != nil || prev != "Gaming PC" || a.HostModel("laptop") != "Office PC" {
		t.Fatalf("replace: prev %q, err %v", prev, err)
	}
	if _, err := a.SetHostModel("laptop", "bad\nname"); err == nil || a.HostModel("laptop") != "Office PC" {
		t.Fatalf("a name that is refused changes nothing: %v, %q", err, a.HostModel("laptop"))
	}
	if _, err := a.SetHostModel("", "x"); err == nil {
		t.Fatal("no system id")
	}
	// the same name again: nothing is written
	before, _ := os.Stat(a.StatePath)
	if prev, err := a.SetHostModel("laptop", "Office PC"); err != nil || prev != "Office PC" {
		t.Fatal(prev, err)
	}
	if after, _ := os.Stat(a.StatePath); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("setting the name a host already has rewrote state.json")
	}
	if prev, err := a.SetHostModel("laptop", ""); err != nil || prev != "Office PC" || a.HostModel("laptop") != "" {
		t.Fatalf("remove: prev %q, err %v, now %q", prev, err, a.HostModel("laptop"))
	}
	if prev, err := a.SetHostModel("laptop", ""); err != nil || prev != "" {
		t.Fatalf("removing none: %q, %v", prev, err)
	}
	if a.HostModel("tower") != "Server" || !a.WakeUpdateEnabled("tower") {
		t.Fatal("another host's name, or the panel's other state, changed")
	}
	// a panel that starts again on the same folder reads the same
	if b := New(dir); b.HostModel("tower") != "Server" || b.HostModel("laptop") != "" || !b.WakeUpdateEnabled("tower") {
		t.Fatalf("after a restart: %v", b.HostModels())
	}
	st, err := os.Stat(a.StatePath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("state.json: %v, mode %v", err, st.Mode().Perm())
	}
	files, _ := os.ReadDir(dir)
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".tmp") {
			t.Fatalf("a temporary file stayed: %s", f.Name())
		}
	}
	// the last name removed: the key itself goes
	if _, err := a.SetHostModel("tower", ""); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(a.StatePath); strings.Contains(string(raw), hostModelsKey) {
		t.Fatalf("state.json still has %s: %s", hostModelsKey, raw)
	}
}

// A state.json from before this feature, and one whose host_models is not what the panel writes: the file is
// read, what is not a name is left out, and nothing else of the state is lost or counted as broken.
func TestHostModelsToleratesOldAndGarbledState(t *testing.T) {
	write := func(t *testing.T, content string) *State {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return New(dir)
	}
	// written by a panel that did not know the field
	old := write(t, `{"poll_overrides": {"laptop": 5}, "wake_update_hosts": ["laptop"], "power_control_disabled": []}`)
	if got := old.HostModels(); len(got) != 0 || old.ControlDisabled() != nil || !old.WakeUpdateEnabled("laptop") {
		t.Fatalf("an old state.json: models %v, control disabled %v", got, old.ControlDisabled())
	}
	if _, err := old.SetHostModel("laptop", "Gaming PC"); err != nil || old.HostModel("laptop") != "Gaming PC" || !old.WakeUpdateEnabled("laptop") {
		t.Fatal("setting a name in an old state.json", err)
	}
	for name, content := range map[string]string{
		"a number": `{"host_models": 7, "wake_update_hosts": ["laptop"]}`,
		"a text":   `{"host_models": "Gaming PC", "wake_update_hosts": ["laptop"]}`,
		"a list":   `{"host_models": ["laptop", "Gaming PC"], "wake_update_hosts": ["laptop"]}`,
		"null":     `{"host_models": null, "wake_update_hosts": ["laptop"]}`,
		"bad items": `{"wake_update_hosts": ["laptop"], "host_models": {"laptop": 5, "tower": {"name": "x"}, "nas": null, "": "no id",` +
			` "pve": "bad\nname", "rtl": "PC \u202egnimaG", "long": "` + strings.Repeat("x", MaxModelName+1) + `", "empty": "   ",` +
			` "ok": "  Mini   PC "}}`,
	} {
		a := write(t, content)
		got := a.HostModels()
		if a.ControlDisabled() != nil || !a.WakeUpdateEnabled("laptop") {
			t.Errorf("%s: the rest of state.json must still be read", name)
		}
		if name == "bad items" {
			if len(got) != 1 || got["ok"] != "Mini PC" {
				t.Errorf("%s: only the one good name stays (tidied): %v", name, got)
			}
			// the next write keeps the good one and drops the rest from the file
			if _, err := a.SetHostModel("laptop", "Gaming PC"); err != nil {
				t.Fatal(err)
			}
			var d map[string]any
			raw, _ := os.ReadFile(a.StatePath)
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatal(err)
			}
			m, _ := d[hostModelsKey].(map[string]any)
			if len(m) != 2 || m["ok"] != "Mini PC" || m["laptop"] != "Gaming PC" {
				t.Errorf("after a write: %v", m)
			}
			continue
		}
		if len(got) != 0 {
			t.Errorf("%s: %v", name, got)
		}
		if _, err := a.SetHostModel("laptop", "Gaming PC"); err != nil || a.HostModel("laptop") != "Gaming PC" {
			t.Errorf("%s: a name can be set afterwards: %v", name, err)
		}
	}
	// a state.json that is not JSON at all: no names (the reported model shows), nothing is written over it,
	// and the panel says control is off, as it always did for such a file
	broken := write(t, `{"host_models": {"laptop": "Gaming`)
	if got := broken.HostModels(); len(got) != 0 || broken.ControlDisabled() == nil {
		t.Fatalf("a corrupt state.json: %v", got)
	}
	if _, err := broken.SetHostModel("laptop", "x"); err == nil {
		t.Fatal("a corrupt state.json was written over")
	}
	if raw, _ := os.ReadFile(broken.StatePath); string(raw) != `{"host_models": {"laptop": "Gaming` {
		t.Fatalf("the corrupt file was changed: %s", raw)
	}
}
