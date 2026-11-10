package enrollment

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/enroll"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/managed"
)

// L24: the panel knows which Fairy-agent a managed host runs and which release its command installs.
func TestOlderReleaseWarning(t *testing.T) {
	in := Installers{Tag: "build-13", Version: "13-d43fdaf"}
	for installed, warn := range map[string]bool{
		"14-49a8907": true,
		"150-abc":    true,
		"13-d43fdaf": false, // the same build
		"13-0000000": false,
		"12-abc":     false,
		"":           false, // never seen
		"dev":        false, // a local build: no build number, nothing to compare
		"v0.5.3":     false,
		"-5-abc":     false,
	} {
		w := olderRelease(installed, in)
		if (w != "") != warn {
			t.Errorf("%q: %q", installed, w)
		}
		if warn && (!strings.Contains(w, installed) || !strings.Contains(w, "build-13") || !strings.Contains(w, "13-d43fdaf")) {
			t.Errorf("%q: the warning must name both versions: %s", installed, w)
		}
	}
	if olderRelease("14-abc", Installers{Tag: "custom", Version: "local"}) != "" {
		t.Error("a release without a build number cannot be compared")
	}
}

// Re-pair of a machine that runs a newer agent than the pinned release: the admin is told before the command
// is run, and when the agent that answers afterwards is an older build than before, the dialog and the audit
// entry say that an older build was installed.
func TestRepairWarnsAboutAnOlderReleaseAndReportsADowngrade(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stored    string // the version in managed-hosts.yaml
		live      string // what the poller sees now ("" = the host is not answering)
		answers   string // agent_version in the pairing answer
		warn      bool
		downgrade string // the version named as "before"
	}{
		{"newer agent, the command installs the older release", "15-aaaaaaa", "", "7-6155fe6", true, "15-aaaaaaa"},
		{"the live version counts, not the one from pairing day", "7-6155fe6", "16-bbbbbbb", "7-6155fe6", true, "16-bbbbbbb"},
		{"the installer kept the newer agent", "15-aaaaaaa", "", "15-aaaaaaa", true, ""},
		{"same build", "7-6155fe6", "", "7-6155fe6", false, ""},
		{"older agent is updated by the command", "5-ccccccc", "", "7-6155fe6", false, ""},
		{"version never seen", "", "", "7-6155fe6", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := managed.NewStore(t.TempDir())
			h := managed.Host{ID: "box", Name: "box", URL: "http://box:9101", OS: "linux", AgentVersion: tc.stored}
			if err := store.Put(h, managed.Secret{Token: "old-read-token-xxxxxxxxxxxxxxxxxx"}); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			events := map[string]string{}
			m := &Manager{Store: store, Now: time.Now, Interval: time.Millisecond, OnlineWait: time.Second, PanelAddr: "100.64.0.10",
				Scripts: Fixed(testInstallers), items: map[string]*Enrollment{},
				Hooks: Hooks{
					Config: func() *config.Panel { return testConfig(t) },
					Remove: func(string) {}, Add: func(managed.Entry) error { return nil },
					Online: func(string) (bool, string) { return true, "" },
					Event: func(_ Enrollment, kind, detail string) {
						mu.Lock()
						events[kind] = detail
						mu.Unlock()
					},
					AgentVersion: func(id string) string {
						if id != "box" {
							t.Error("asked for", id)
						}
						return tc.live
					},
				},
				Pair: func(context.Context, string, []byte) (int, enroll.Response, string, error) {
					return 200, enroll.Response{AgentVersion: tc.answers, Token: "new-read-token-xxxxxxxxxxxxxxxxxx"}, "", nil
				}}
			cr, err := m.Create(context.Background(), Request{}, &h, "alice", nil)
			if err != nil {
				t.Fatal(err)
			}
			if (cr.Warning != "") != tc.warn {
				t.Fatalf("warning: %q", cr.Warning)
			}
			if tc.warn && (!strings.Contains(cr.Warning, "build-7") || !strings.Contains(cr.Warning, "newer than the release this command installs")) {
				t.Fatal(cr.Warning)
			}
			var e Enrollment
			for i := 0; i < 400 && !e.Done; i++ {
				time.Sleep(5 * time.Millisecond)
				e, _ = m.Get(cr.ID)
			}
			if e.State != Online || e.Warning != cr.Warning {
				t.Fatalf("%+v", e)
			}
			mu.Lock()
			paired := events["paired"]
			mu.Unlock()
			if tc.downgrade == "" {
				if strings.Contains(e.Message, "older build") || paired != tc.answers {
					t.Fatalf("message %q, audit detail %q", e.Message, paired)
				}
				return
			}
			if !strings.Contains(e.Message, "the agent is now "+tc.answers+"; before the Re-pair it was "+tc.downgrade) ||
				paired != tc.answers+" (was "+tc.downgrade+": an older build was installed)" {
				t.Fatalf("message %q, audit detail %q", e.Message, paired)
			}
		})
	}
}

// Add host of a new machine has nothing to compare with: no warning, and no question to the poller.
func TestAddHostHasNoOlderReleaseWarning(t *testing.T) {
	m := &Manager{Store: managed.NewStore(t.TempDir()), Now: time.Now, Interval: time.Hour, PanelAddr: "100.64.0.10",
		Scripts: Fixed(testInstallers), items: map[string]*Enrollment{},
		Hooks: Hooks{Config: func() *config.Panel { return testConfig(t) }, Event: func(Enrollment, string, string) {},
			AgentVersion: func(string) string { t.Error("asked"); return "99-zzz" }},
		Pair: func(context.Context, string, []byte) (int, enroll.Response, string, error) {
			return 404, enroll.Response{}, "pairing_inactive", nil
		}}
	cr, err := m.Create(context.Background(), Request{Name: "new-box", OS: "linux"}, nil, "alice", func(string) string { return "" })
	if err != nil || cr.Warning != "" {
		t.Fatal(err, cr.Warning)
	}
	m.Cancel(cr.ID)
}
