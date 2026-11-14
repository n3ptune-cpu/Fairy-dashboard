package poller

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
)

// A machine's own run timing that was decided in "Edit host" goes on counting when config.yaml gets a block
// for the machine later, or a higher value in it. Where the decision is below the block's value, the start
// of the panel says so once, with the machine and both numbers: nobody is asked again when config.yaml
// changes. A decision that is not below the block's value and a machine without a block are not named. (A
// stored number that is no value of its key never gets here: the store's reader and writer leave it out.)
func TestTheStartNamesARunTimingOfEditHostBelowConfigYAMLs(t *testing.T) {
	const block = "    wake_update: {off_after_s: 900, shutdown_delay_s: 120}\n"
	for _, c := range []struct {
		what string
		more string
		key  string
		e    adminstate.RunEdit
		want []string // parts of the one line, per timing that is named
	}{
		{"nothing decided", block, "lab-b-agent", adminstate.RunEdit{}, nil},
		{"both below", block, "lab-b-agent", adminstate.RunEdit{OffAfterS: 600, ShutdownDelayS: 30},
			[]string{"host lab-b-agent: off_after_s is 600 s|says 900 s|The 600 s of the panel count", "host lab-b-agent: shutdown_delay_s is 30 s|says 120 s|The 30 s of the panel count"}},
		{"kept under the node's id", block, "lab-b-node-pve-b", adminstate.RunEdit{OffAfterS: 300},
			[]string{"host lab-b-node-pve-b: off_after_s is 300 s|says 900 s"}},
		{"one below, one above", block, "lab-b-agent", adminstate.RunEdit{OffAfterS: 1200, ShutdownDelayS: 119},
			[]string{"shutdown_delay_s is 119 s|says 120 s"}},
		{"not below", block, "lab-b-agent", adminstate.RunEdit{OffAfterS: 901, ShutdownDelayS: 3600}, nil},
		{"no block", "", "lab-b-agent", adminstate.RunEdit{OffAfterS: 300, ShutdownDelayS: 10}, nil},
		{"a block with one key", "    wake_update: {shutdown_delay_s: 60}\n", "lab-b-agent", adminstate.RunEdit{OffAfterS: 300, ShutdownDelayS: 10},
			[]string{"shutdown_delay_s is 10 s|says 60 s"}},
	} {
		p, _, _ := twoHosts(t, c.more)
		setHost(t, p, c.key, adminstate.HostEdit{Name: "Rack B", Run: c.e})
		setHost(t, p, "lab-a-agent", adminstate.HostEdit{Name: "Rack A"}) // a host with a name only: never named here
		check := func(when string, got []string) {
			t.Helper()
			if len(got) != len(c.want) {
				t.Errorf("%s, %s: %d lines, want %d: %q", c.what, when, len(got), len(c.want), got)
				return
			}
			for i, w := range c.want {
				for _, part := range strings.Split(w, "|") {
					if !strings.Contains(got[i], part) {
						t.Errorf("%s, %s: line %d is %q, without %q", c.what, when, i, got[i], part)
					}
				}
				if !strings.Contains(got[i], "Reset in Edit host") {
					t.Errorf("%s, %s: the line does not say what to do: %q", c.what, when, got[i])
				}
			}
		}
		check("asked", p.RunTimingWarnings())
		// the start of the panel says each line once, as a WARN
		logs := &syncBuf{}
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
		_, err := New(p.Cfg, config.Secrets{}, nil)
		slog.SetDefault(old)
		if err != nil {
			t.Fatal(err)
		}
		var said []string
		for _, l := range strings.Split(logs.text(), "\n") {
			if strings.Contains(l, "level=WARN") && strings.Contains(l, "state.json: host ") && strings.Contains(l, "wake_update: block") {
				said = append(said, strings.ReplaceAll(l, `\"`, `"`))
			}
		}
		check("at the start", said)
	}
}

func (s *syncBuf) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
