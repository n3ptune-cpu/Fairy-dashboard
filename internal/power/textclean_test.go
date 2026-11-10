package power

import "testing"

// hostile: an ANSI colour sequence, a NUL, a line break, a direction override (U+202E), a one-character CSI
// (U+009B) and a sequence that sets the window title; hostileClean is what is left of it.
var hostile = "a\x1b[31m\x00b\r\n" + string(rune(0x202e)) + string(rune(0x9b)) + "2Jc\x1b]0;title\x07"

const hostileClean = "ab  c"

// N6: the name, the type and the error of a device an agent relays are cleaned like every other text of an
// agent (no control characters, escape sequences or direction overrides), and a cut never splits a character.
func TestRelayedTextsAreCleaned(t *testing.T) {
	pm := manager(t, nil, nil, nil)
	pm.UpdateRemote("laptop", []map[string]any{
		{"id": "desk", "name": "Desk" + hostile, "type": "tuya" + hostile, "ok": true, "watts": 5.0},
		{"id": "tv", "name": hostile, "type": "\x1b[2J", "ok": false, "error": "timeout" + hostile},
		{"id": "long", "name": "\x1b[31m" + repeat("é", 100), "type": "x", "ok": true, "watts": 1.0},
		{"id": "bad" + hostile, "name": "dropped", "type": "x", "ok": true, "watts": 1.0},
	}, false, nil, nil)
	by := byID(pm.Overview())
	if len(by) != 3 {
		t.Fatalf("%d sources (a row whose id is not an id is dropped)", len(by))
	}
	if d := by["laptop/desk"]; d.Name != "Desk"+hostileClean || d.Type != "tuya"+hostileClean || d.Readings[0].Name != "Desk"+hostileClean {
		t.Fatalf("%+v", d)
	}
	if tv := by["laptop/tv"]; tv.Name != hostileClean || tv.Type != "unknown" || tv.Error == nil || *tv.Error != "timeout"+hostileClean {
		t.Fatalf("%+v %q", tv, *tv.Error)
	}
	if l := by["laptop/long"]; l.Name != repeat("é", maxRemoteName/2) {
		t.Fatalf("%q", l.Name)
	}
}

func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}
