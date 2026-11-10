package enrollment

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// N6: the two free texts of a pairing answer, `hostname` and `agent_version`, were stored cut at 64 bytes
// (possibly in the middle of a character) and otherwise as sent: NUL, ESC and line breaks arrived in
// managed-hosts.yaml and in GET /api/admin/hosts. The answer is verified, but its texts are still a machine's
// word: the version is kept only when it is a version, the name only when it is a computer name, and anything
// else is left empty. The machine is still added (a text the panel does not keep is no reason to refuse it).
func TestAnswerTextsAreKeptOnlyWhenTheyAreWhatTheyClaim(t *testing.T) {
	bidi, csi := string(rune(0x202e)), string(rune(0x9b))
	texts := []string{
		strings.Repeat("a", 63) + "é", // was cut in the middle of a character at 64 bytes
		"\x00\x01\x02", "a\x00b", "\x7f", string(rune(0x85)), " x", string(rune(0xfeff)), "\x1b[31mred", "\xff\xfe", "line\nbreak", "tab\tx", "cr\rx",
		"box" + bidi + "gpj.exe", csi + "2J", "\x1b]0;title\x07",
		"!!binary x", "- a", "# c", ": x", "~", "{a: b}", "[a]", "trail ", "a: b\nhosts: []", "\"\\", "`x`", strings.Repeat("v", 65),
		"box", "WIN-PC_01", "tower.lan", "20-abcdef0", "v0.5.3", // real ones
	}
	if testing.Short() {
		texts = append(texts[:6], texts[len(texts)-5:]...)
	}
	for _, field := range []string{"hostname", "agent_version"} {
		for _, txt := range texts {
			machine := newSwap(t, http.NotFoundHandler())
			r := newRig(t, machine.srv.URL)
			if err := r.store.Put(managed.Host{ID: "old", Name: "old", URL: "http://old:9101", OS: "linux"}, managed.Secret{Token: strings.Repeat("t", 32)}); err != nil {
				t.Fatal(err)
			}
			cr := r.add(t, "typed-name")
			answer := goodAnswer
			if field == "hostname" {
				answer.Hostname = txt
			} else {
				answer.AgentVersion = txt
			}
			machine.set(v2Agent(keyOf(t, cr.Code), answer))
			e := r.wait(t, cr.ID, "done", func(e Enrollment) bool { return e.Done })
			if e.State != Online {
				t.Errorf("%s=%q: %s (%s)", field, txt, e.State, e.Message)
				continue
			}
			hosts, err := r.store.Load()
			if err != nil || len(hosts) != 2 {
				t.Errorf("%s=%q: %d hosts, %v", field, txt, len(hosts), err)
				continue
			}
			h := hosts[1]
			wantName, wantVersion := goodAnswer.Hostname, goodAnswer.AgentVersion
			if field == "hostname" {
				wantName = ""
				if shownNameRe.MatchString(txt) {
					wantName = txt
				}
			} else {
				wantVersion = ""
				if release.ValidVersion(txt) {
					wantVersion = txt
				}
			}
			if h.Hostname != wantName || h.AgentVersion != wantVersion {
				t.Errorf("%s=%q: stored hostname %q, agent_version %q", field, txt, h.Hostname, h.AgentVersion)
			}
			// nothing of it in the file, in what the dialog shows or in what goes to the audit log
			file, _ := os.ReadFile(r.store.Path())
			r.mu.Lock()
			seen := string(file) + e.Message + e.AgentHostname + strings.Join(r.events, "\n")
			r.mu.Unlock()
			if textsafe.Clean(txt) != txt && strings.TrimSpace(txt) == txt && strings.Contains(seen, txt) {
				t.Errorf("%s=%q is in the hosts file, the message or an event", field, txt)
			}
			for _, bad := range []string{"\x00", "\x1b", bidi, csi, "\r"} {
				if strings.Contains(seen, bad) {
					t.Errorf("%s=%q: %q reached the hosts file, the message or an event", field, txt, bad)
				}
			}
		}
	}
}

// A hosts file an earlier release wrote can hold such a text. It is cleaned when the file is read, so the
// Hosts page and the CLI never show it, and the next write of the file leaves it out.
func TestHostsFileOfAnEarlierReleaseIsCleanedOnLoad(t *testing.T) {
	st := managed.NewStore(t.TempDir())
	if err := st.Put(managed.Host{ID: "box", Name: "box", URL: "http://box:9101", OS: "linux",
		Hostname: "evil\x1b[31m\nname", AgentVersion: strings.Repeat("a", 63) + "é"}, managed.Secret{Token: strings.Repeat("t", 32)}); err != nil {
		t.Fatal(err)
	}
	file, _ := os.ReadFile(st.Path())
	if !strings.Contains(string(file), `\e[31m`) && !strings.Contains(string(file), `\x1b`) && !strings.Contains(string(file), "\x1b") {
		t.Fatalf("the fixture does not hold the text:\n%s", file)
	}
	hosts, err := st.Load()
	if err != nil || len(hosts) != 1 || hosts[0].Hostname != "evil name" || hosts[0].AgentVersion != strings.Repeat("a", 63) {
		t.Fatalf("%+v %v", hosts, err)
	}
}
