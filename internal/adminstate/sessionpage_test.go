package adminstate

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// oldSessionsFile is a sessions.json as the release before the Sessions page wrote it (build-11): per session
// sub, name, email, ip, gen, created and created_ts, and nothing else. One entry is older still (no gen).
func oldSessionsFile(t *testing.T, a *State) (ts1, ts2 float64) {
	t.Helper()
	ts1, ts2 = math.Floor(now())-7200, math.Floor(now())-600
	body := fmt.Sprintf(`{
 "sessions": {
  "sid-old-one-0001": {
   "created": "2026-11-11T08:00:00.123456789Z",
   "created_ts": %d,
   "email": "ann@example.com",
   "gen": 2,
   "ip": "203.0.113.7",
   "name": "Ann",
   "sub": "sub-ann"
  },
  "sid-old-two-0002": {
   "created": "2026-11-11T09:50:00Z",
   "created_ts": %d,
   "email": null,
   "gen": 2,
   "ip": "2001:db8::7",
   "name": null,
   "sub": "sub-bob"
  },
  "sid-older-0003": {
   "created": "2026-10-01T09:50:00Z",
   "created_ts": %d,
   "email": null,
   "ip": "198.51.100.1",
   "name": "Cy",
   "sub": "sub-cy"
  }
 }
}`, int64(ts1), int64(ts2), int64(ts2))
	if err := os.WriteFile(a.SessionsPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return ts1, ts2
}

func rawSessions(t *testing.T, a *State) map[string]map[string]any {
	t.Helper()
	b, err := os.ReadFile(a.SessionsPath)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Sessions map[string]map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d.Sessions
}

// A sessions.json written before the Sessions page loads as it is: its sessions are still valid, what the page
// needs and the file does not have reads as "not known", and writing next to the old entries (a sign-in, a
// use, a sign-out) leaves them as they were and the file's mode at 0600.
func TestSessionsFileOfTheReleaseBeforeLoads(t *testing.T) {
	a := New(t.TempDir())
	ts1, ts2 := oldSessionsFile(t, a)
	before := rawSessions(t, a)

	list, err := a.Sessions()
	if err != nil || len(list) != 2 {
		t.Fatalf("the two sessions of the old file: %v %v", list, err)
	}
	if fromFile, err := a.ListSessions(); err != nil || !reflect.DeepEqual(fromFile, list) {
		t.Fatalf("the CLI's list differs: %v %v", fromFile, err)
	}
	one, two := list[0], list[1]
	if one.SID != "sid-old-one-0001" || one.Sub != "sub-ann" || one.Name != "Ann" || one.Email != "ann@example.com" ||
		one.IP != "203.0.113.7" || one.Created != "2026-11-11T08:00:00.123456789Z" || one.CreatedTS != ts1 {
		t.Fatalf("what the old entry had: %+v", one)
	}
	for _, s := range list {
		if s.UA != "" || s.LoginTS != 0 || s.LoginIP != "" || s.SeenTS != 0 || s.SeenIP != "" {
			t.Errorf("an old entry has no browser, no last use: %+v", s)
		}
	}
	if one.SignedIn() != ts1 || one.SignedInIP() != "203.0.113.7" || two.SignedIn() != ts2 || two.SignedInIP() != "2001:db8::7" {
		t.Fatal("the sign-in of an old entry is when and where it was registered")
	}
	iat := float64(time.Now().Unix())
	if !a.SessionValid("sid-old-one-0001", "sub-ann", iat) || !a.SessionValid("sid-old-two-0002", "sub-bob", iat) {
		t.Fatal("the old file's sessions must stay valid")
	}
	if a.SessionValid("sid-older-0003", "sub-cy", iat) {
		t.Fatal("an entry without gen was not a session before and is none now")
	}

	// a use of one old session: only that entry gets the two new fields
	if err := a.TouchSession("sid-old-one-0001", 1234567, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	after := rawSessions(t, a)
	want := map[string]any{}
	for k, v := range before["sid-old-one-0001"] {
		want[k] = v
	}
	want["seen_ts"], want["seen_ip"] = float64(1234567), "203.0.113.9"
	if !reflect.DeepEqual(after["sid-old-one-0001"], want) {
		t.Fatalf("the used entry: %v, want %v", after["sid-old-one-0001"], want)
	}
	if !reflect.DeepEqual(after["sid-old-two-0002"], before["sid-old-two-0002"]) || !reflect.DeepEqual(after["sid-older-0003"], before["sid-older-0003"]) {
		t.Fatal("a use of one session changed another entry")
	}
	// a sign-in next to them (it drops the entry without gen, as before), then a sign-out
	if err := a.AddSession("sid-new-0004", NewSession{Sub: "sub-ann", IP: "203.0.113.9", UA: "Fakefox/1.0"}); err != nil {
		t.Fatal(err)
	}
	after = rawSessions(t, a)
	if !reflect.DeepEqual(after["sid-old-two-0002"], before["sid-old-two-0002"]) || len(after) != 3 || after["sid-older-0003"] != nil {
		t.Fatalf("after a sign-in: %v", after)
	}
	if err := a.RemoveSession("sid-new-0004"); err != nil {
		t.Fatal(err)
	}
	if list, _ = a.Sessions(); len(list) != 2 || list[0].SeenTS != 1234567 || list[0].SeenIP != "203.0.113.9" || list[1].SeenTS != 0 {
		t.Fatalf("at the end: %+v", list)
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(a.SessionsPath); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("sessions.json must stay 0600: %v %v", st.Mode().Perm(), err)
		}
	}
}

// What a sign-in records for the Sessions page, and what a renewed id ("Confirm it's you") keeps of it.
func TestSessionEntryForTheSessionsPage(t *testing.T) {
	a := New(t.TempDir())
	if err := a.AddSession("sid-a-0001", NewSession{Sub: "sub-ann", IP: "203.0.113.7", UA: "Fakefox/1.0 (TestOS)"}); err != nil {
		t.Fatal(err)
	}
	list, _ := a.Sessions()
	s := list[0]
	if s.UA != "Fakefox/1.0 (TestOS)" || s.LoginIP != "203.0.113.7" || s.LoginTS != s.CreatedTS || s.SeenTS != s.CreatedTS ||
		s.SeenIP != "203.0.113.7" || s.SignedIn() != s.CreatedTS {
		t.Fatalf("a sign-in: %+v", s)
	}
	// the same session under a new id, confirmed from another address a moment later
	time.Sleep(5 * time.Millisecond)
	if err := a.AddSession("sid-a-0002", NewSession{Sub: "sub-ann", IP: "198.51.100.2", UA: "Fakefox/1.0 (TestOS)", Replaces: "sid-a-0001"}); err != nil {
		t.Fatal(err)
	}
	_ = a.RemoveSession("sid-a-0001")
	list, _ = a.Sessions()
	r := list[0]
	if len(list) != 1 || r.SID != "sid-a-0002" || r.LoginTS != s.LoginTS || r.LoginIP != "203.0.113.7" || r.IP != "198.51.100.2" ||
		r.CreatedTS <= s.CreatedTS || r.SeenIP != "198.51.100.2" || r.SeenTS != r.CreatedTS {
		t.Fatalf("a renewed id keeps the sign-in and nothing else: %+v (was %+v)", r, s)
	}
	// nothing is taken from another account's session, or from one that is not there
	if err := a.AddSession("sid-b-0003", NewSession{Sub: "sub-bob", IP: "192.0.2.3", Replaces: "sid-a-0002"}); err != nil {
		t.Fatal(err)
	}
	if err := a.AddSession("sid-b-0004", NewSession{Sub: "sub-bob", IP: "192.0.2.4", Replaces: "sid-gone"}); err != nil {
		t.Fatal(err)
	}
	list, _ = a.Sessions()
	for _, x := range list[1:] {
		if x.LoginIP != x.IP || x.LoginTS != x.CreatedTS {
			t.Errorf("%s took a sign-in that is not its own: %+v", x.SID, x)
		}
	}
	// a renewed id of an entry from before the Sessions page: the sign-in is what that entry knew
	b := New(t.TempDir())
	ts1, _ := oldSessionsFile(t, b)
	if err := b.AddSession("sid-new-0005", NewSession{Sub: "sub-ann", IP: "192.0.2.5", Replaces: "sid-old-one-0001"}); err != nil {
		t.Fatal(err)
	}
	list, _ = b.Sessions()
	if n := list[len(list)-1]; n.SID != "sid-new-0005" || n.LoginTS != ts1 || n.LoginIP != "203.0.113.7" {
		t.Fatalf("renewed from an old entry: %+v", n)
	}
}

// A use is recorded for a registered session only, and never makes an entry or a file.
func TestTouchSessionLeavesUnknownSessionsAlone(t *testing.T) {
	a := New(t.TempDir())
	if err := a.TouchSession("sid-none", 5, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.SessionsPath); !os.IsNotExist(err) {
		t.Fatal("a use of an unknown session wrote sessions.json")
	}
	oldSessionsFile(t, a)
	before, _ := os.ReadFile(a.SessionsPath)
	for _, sid := range []string{"sid-none", "", "sid-older-0003"} { // unknown, empty, an entry that is no session
		if err := a.TouchSession(sid, 5, "192.0.2.1"); err != nil {
			t.Fatal(err)
		}
	}
	if after, _ := os.ReadFile(a.SessionsPath); string(after) != string(before) {
		t.Fatal("a use of an unknown session changed sessions.json")
	}
}

func TestRemoveSessions(t *testing.T) {
	a := New(t.TempDir())
	for i := 1; i <= 4; i++ {
		if err := a.AddSession(fmt.Sprintf("sid-x-%04d", i), NewSession{Sub: "sub-ann"}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := a.RemoveSessions([]string{"sid-x-0001", "sid-x-0003", "sid-gone", ""})
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	list, _ := a.Sessions()
	if len(list) != 2 || list[0].SID != "sid-x-0002" || list[1].SID != "sid-x-0004" {
		t.Fatalf("%+v", list)
	}
	if n, err := a.RemoveSessions(nil); n != 0 || err != nil {
		t.Fatal(n, err)
	}
}

// The User-Agent header is the client's text: what is kept of it is valid UTF-8 without control or direction
// characters, at most MaxUA bytes, and an entry has a largest size whatever was sent.
func TestCleanUAAndTheSizeOfAnEntry(t *testing.T) {
	for in, want := range map[string]string{
		"Fakefox/1.0":                         "Fakefox/1.0",
		"  spaced  ":                          "spaced",
		"a\x00b\x1fc\x7fd\re\nf\tg":           "abcdefg",
		"x\u2028y\u2029z\u202e!\u2066?\ufeff": "xyz!?",
		"bad\xff\xfebytes":                    "bad\ufffdbytes",
		"<img src=x onerror=alert(1)>":        "<img src=x onerror=alert(1)>", // kept as text: the page escapes it
		"":                                    "",
		"\x00\x01":                            "",
	} {
		if got := CleanUA(in); got != want {
			t.Errorf("CleanUA(%q) = %q, want %q", in, got, want)
		}
	}
	if got := CleanUA(strings.Repeat("\xff", 5000)); got != "\ufffd" { // a run of bad bytes is one mark
		t.Errorf("5000 bytes that are not UTF-8: %q", got)
	}
	for _, in := range []string{strings.Repeat("A", 5000), strings.Repeat("é", 5000), strings.Repeat("\xffa", 5000),
		strings.Repeat("a", 255) + "€", strings.Repeat("a", 254) + "€"} {
		got := CleanUA(in)
		if len(got) > MaxUA || !utf8.ValidString(got) || len(got) < MaxUA-3 {
			t.Errorf("a long header: %d bytes kept, valid %v", len(got), utf8.ValidString(got))
		}
	}
	a := New(t.TempDir())
	long := strings.Repeat("\u202eZ\x00", 4000)
	if err := a.AddSession("sid-big-0001", NewSession{Sub: "sub-ann", IP: long, UA: long}); err != nil {
		t.Fatal(err)
	}
	if err := a.TouchSession("sid-big-0001", 7, long); err != nil {
		t.Fatal(err)
	}
	if err := a.AddSession("sid-big-0002", NewSession{Sub: "sub-ann", IP: long, UA: long, Replaces: "sid-big-0001"}); err != nil {
		t.Fatal(err)
	}
	for sid, rec := range rawSessions(t, a) {
		b, _ := json.Marshal(rec)
		if len(b) > 800 {
			t.Errorf("%s: an entry of %d bytes", sid, len(b))
		}
		for _, k := range []string{"ip", "login_ip", "seen_ip"} {
			if v, _ := rec[k].(string); len(v) > maxAddr || strings.ContainsAny(v, "\x00\u202e") {
				t.Errorf("%s %s: %q", sid, k, v)
			}
		}
		if v, _ := rec["ua"].(string); len(v) != MaxUA || strings.ContainsAny(v, "\x00\u202e") {
			t.Errorf("%s ua: %d bytes", sid, len(v))
		}
	}
}
