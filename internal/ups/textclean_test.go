package ups

import (
	"path/filepath"
	"testing"
	"time"
)

// hostile: an ANSI colour sequence, a NUL, a line break, a direction override (U+202E), a one-character CSI
// (U+009B) and a sequence that sets the window title; hostileClean is what is left of it.
var hostile = "a\x1b[31m\x00b\r\n" + string(rune(0x202e)) + string(rune(0x9b)) + "2Jc\x1b]0;title\x07"

const hostileClean = "ab  c"

// N6: the texts of a UPS an agent relays (its description, model, maker, serial, raw status, an error) are
// cleaned like every other text of an agent.
func TestUPSTextsAreCleaned(t *testing.T) {
	m, err := NewManager(nil, 5, time.Second, 60, 3600, filepath.Join(t.TempDir(), "ups-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	m.UpdateFromAgent("box", []map[string]any{
		{"name": "ups1", "state": "online", "description": "Rack" + hostile, "model": "Back-UPS" + hostile, "mfr": hostile,
			"serial": hostile, "status_raw": "OL" + hostile, "battery_date": hostile, "test_result": hostile, "transfer_reason": hostile},
	}, false, nil, nil)
	st := m.State("box/ups1")
	if st == nil || st.Name != "Rack"+hostileClean || *st.Model != "Back-UPS"+hostileClean || *st.Mfr != hostileClean || *st.Serial != hostileClean ||
		*st.StatusRaw != "OL"+hostileClean || *st.BatteryDate != hostileClean || *st.TestResult != hostileClean || *st.TransferReason != hostileClean {
		t.Fatalf("%+v", st)
	}
	// a row that failed: its error text, cleaned and cut on a character
	long := ""
	for len(long) < 300 {
		long += "é"
	}
	m.UpdateFromAgent("box", []map[string]any{{"name": "ups1", "ok": false, "error": "\x1b[31m" + long + "\r\n"}}, false, nil, nil)
	st = m.State("box/ups1")
	if st == nil || st.Error == nil || *st.Error != long[:120] {
		t.Fatalf("%+v", st)
	}
}
