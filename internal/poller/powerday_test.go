package poller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/power"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
)

// The last 24 hours of a power source are still there after a restart of the panel: the poller records them
// with its history job, writes them when it stops, and reads them when it is made (power/dayfile.go). The
// laptop relays a plug ("desk", testutil.AgentMetrics): its source is laptop/desk.
func TestPowerDayIsKeptOverARestart(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	file := filepath.Join(cfg.DataDir, power.DayFile)
	run := func() *Poller {
		t.Helper()
		p, err := New(cfg, config.Secrets{}, testutil.NewBackend())
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := run()
	if p.Power.Day.Has("laptop/desk", time.Now()) {
		t.Fatal("a day before the first poll")
	}
	if _, err := os.Lstat(file); err == nil {
		t.Fatal("the file is there before anything was recorded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	for i := 0; !p.Power.Day.Has("laptop/desk", time.Now()); i++ {
		if i > 5000 {
			t.Fatal("the history job recorded nothing for the plug the laptop relays")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the poller did not stop")
	}
	st, err := os.Lstat(file)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		t.Fatalf("the stop did not write %s: %v", power.DayFile, err)
	}
	before := p.Power.Day.Series("laptop/desk", time.Now())

	q := run() // the next start
	after := q.Power.Day.Series("laptop/desk", time.Now())
	if after.Summary == nil || after.Start != before.Start || after.Summary.MaxW != before.Summary.MaxW || after.Summary.MaxW != 80 {
		t.Fatalf("after the restart: %+v, before it: %+v", after.Summary, before.Summary)
	}

	// Remove of the host: the days of the devices it relayed go with it, at once, in memory and in the file.
	// The file is written by the Remove itself, before it returns: nothing is left to be written behind it.
	old, _ := os.Stat(file)
	q.RemoveAgent("laptop")
	if q.Power.Day.Has("laptop/desk", time.Now()) {
		t.Error("the day of a removed host's plug is still there")
	}
	if now, err := os.Stat(file); err != nil || os.SameFile(old, now) {
		t.Errorf("the Remove did not write the file anew (%v)", err)
	}
	if r := run(); r.Power.Day.Has("laptop/desk", time.Now()) {
		t.Error("the day of a removed host's plug came back from the file")
	}
	// a Remove of a host that relays nothing writes nothing
	old, _ = os.Stat(file)
	q.RemoveAgent("tower")
	if now, _ := os.Stat(file); !os.SameFile(old, now) {
		t.Error("the Remove of a host without a relayed device wrote the day file")
	}
}

// A day file that cannot be used does not keep the poller from being made, whatever is at its name.
func TestPowerDayFileThatCannotBeUsedDoesNotStopThePanel(t *testing.T) {
	for name, put := range map[string]func(path string) error{
		"not JSON": func(path string) error { return os.WriteFile(path, []byte("{{{{"), 0o600) },
		"empty":    func(path string) error { return os.WriteFile(path, nil, 0o600) },
		"a folder": func(path string) error { return os.Mkdir(path, 0o700) },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
			if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := put(filepath.Join(cfg.DataDir, power.DayFile)); err != nil {
				t.Fatal(err)
			}
			p, err := New(cfg, config.Secrets{}, testutil.NewBackend())
			if err != nil {
				t.Fatalf("the panel does not start: %v", err)
			}
			p.PollOnce(context.Background())
			if !p.Power.Day.Has("laptop/desk", time.Now()) {
				t.Error("nothing is recorded after a start with a file that cannot be used")
			}
		})
	}
}
