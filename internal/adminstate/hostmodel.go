package adminstate

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/statefile"
)

// A host's custom model name: what an admin typed in the panel to be shown instead of the model the machine
// itself reports ("Set model name" on the host's page). It is the panel's own word about a host, so it is kept
// in state.json next to the other per-host switches, under "host_models": {system id: name}. What the machine
// reports is never changed by it; taking the name away shows the reported model again.

// MaxModelName is how many characters a custom model name may have.
const MaxModelName = 64

// maxMarks is how many combining marks (accents and the like, Unicode Mn and Me) may follow one character. The
// scripts that stack marks use two or three; more than that is a tower of marks over the line above.
const maxMarks = 4

// hostModelsKey is the name of the object in state.json.
const hostModelsKey = "host_models"

// ModelName checks a custom model name as a person typed it and returns it as it is stored: without spaces
// at either end and with every run of spaces as one space. "" (also: only spaces) means "no custom name".
//
// Refused, with a short sentence for the dialog: bytes that are not UTF-8 (and U+FFFD, which is what a decoder
// leaves of them), control characters and line breaks (Unicode Cc, and the line and paragraph separators
// U+2028 / U+2029), format characters, which are invisible and can change how the text around them reads
// (Cf: direction marks, overrides and isolates, zero-width characters, the byte order mark, tags; a joined
// emoji has one, and is refused with a sentence of its own), private-use
// characters, a few characters that show as nothing without being Cf, a name of more than MaxModelName
// characters, a name in which nothing shows (no letter, digit, punctuation or symbol: only combining marks),
// and more than maxMarks combining marks in a row. The name is a label only: nothing is decided by it, and
// the page escapes it like every text.
func ModelName(raw string) (string, error) {
	if !utf8.ValidString(raw) {
		return "", errors.New("model name is not valid text")
	}
	picture := false // the character before this one is a picture (an emoji), or belongs to one
	for _, r := range raw {
		switch {
		case r == utf8.RuneError:
			return "", errors.New("model name is not valid text")
		case unicode.Is(unicode.Cc, r), r == 0x2028, r == 0x2029:
			return "", errors.New("model name must be one line, without control characters")
		case picture && (r == 0x200d || r >= 0xe0020 && r <= 0xe007f):
			// An emoji that is several joined into one (a family, a person with a tool, a flag of a region): the
			// joiner between them is a format character and stays refused, but "type it again" would send the
			// person in a circle, because the emoji keyboard types the same again. The sentence says what to do.
			return "", errors.New("model name has an emoji made of several: use a simple one")
		case unicode.Is(unicode.Cf, r), unicode.Is(unicode.Co, r), blank(r):
			return "", errors.New("model name has an invisible character: type it again")
		}
		picture = r >= 0x1f000 && r <= 0x1faff || unicode.Is(unicode.So, r) || picture && r == 0xfe0f
	}
	name := strings.Join(strings.FieldsFunc(raw, unicode.IsSpace), " ")
	if n := utf8.RuneCountInString(name); n > MaxModelName {
		return "", fmt.Errorf("model name too long: at most %d characters", MaxModelName)
	}
	shows, marks := false, 0
	for _, r := range name {
		switch {
		case unicode.In(r, unicode.Mn, unicode.Me):
			if marks++; marks > maxMarks {
				return "", errors.New("model name has too many accents on one character")
			}
			continue
		case unicode.In(r, unicode.L, unicode.N, unicode.P, unicode.S, unicode.Mc):
			shows = true
		}
		marks = 0
	}
	if name != "" && !shows {
		return "", errors.New("model name needs a letter, a digit or a sign")
	}
	return name, nil
}

// blank reports the characters that show as nothing and are neither a space nor a format character (Cf): the
// combining grapheme joiner, the Hangul fillers, two Khmer inherent vowels and the empty Braille pattern.
func blank(r rune) bool {
	switch r {
	case 0x034f, 0x115f, 0x1160, 0x17b4, 0x17b5, 0x3164, 0xffa0, 0x2800:
		return true
	}
	return false
}

// hostModels reads the custom model names out of a state.json object. Whatever is not a name ModelName
// accepts is left out: an entry that is not a text, an empty one, one with characters a name cannot have (a
// file edited by hand, or written by something else). state.json as a whole is still read; the host shows the
// model it reports.
func hostModels(d statefile.Data) map[string]string {
	out := map[string]string{}
	m, _ := d[hostModelsKey].(map[string]any)
	for id, v := range m {
		s, ok := v.(string)
		if !ok || id == "" {
			continue
		}
		if name, err := ModelName(s); err == nil && name != "" {
			out[id] = name
		}
	}
	return out
}

// HostModels returns every host's custom model name by system id (none while state.json is unreadable).
func (a *State) HostModels() map[string]string {
	if a.state.Err() != nil {
		return map[string]string{}
	}
	return hostModels(a.state.Get())
}

// HostModel returns one host's custom model name ("" when it has none).
func (a *State) HostModel(id string) string { return a.HostModels()[id] }

// SetHostModel stores a host's custom model name and returns the one it had ("" for none). name is what
// ModelName returned; "" takes the custom name away. Nothing is written when that is the name it has already.
// Entries of the file that are not names are dropped by the same write.
func (a *State) SetHostModel(id, name string) (prev string, err error) {
	if id == "" {
		return "", errors.New("no system id")
	}
	if name, err = ModelName(name); err != nil {
		return "", err
	}
	_, err = statefile.Update(a.StatePath, func(d statefile.Data) error {
		cur := hostModels(d)
		prev = cur[id]
		if prev == name {
			return errNoChange // nothing is written for a name the host already has (or for removing none)
		}
		if name == "" {
			delete(cur, id)
		} else {
			cur[id] = name
		}
		if len(cur) == 0 {
			delete(d, hostModelsKey)
			return nil
		}
		out := make(map[string]any, len(cur))
		for k, v := range cur {
			out[k] = v
		}
		d[hostModelsKey] = out
		return nil
	})
	if errors.Is(err, errNoChange) {
		return prev, nil
	}
	a.state.Refresh()
	if err != nil {
		return "", err
	}
	return prev, nil
}
