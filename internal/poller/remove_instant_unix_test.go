//go:build unix

package poller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/adminstate"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/testutil"
	"golang.org/x/sys/unix"
)

// Fourth review of build 14, F3. A poll stores an agent's answer under the poller's lock and writes two things
// to the data folder after it has released it: the version the agent reported (state.json) and when the
// machine was last seen (last-seen.json). A host that is removed in that instant (Remove, and the first half
// of every Re-pair) has both taken out again, in memory under the lock and in the files after it; whichever
// of the two writes came last stayed. When it was the poll's, the files kept the version and the time of a
// host that was gone, and after the next start of the panel a host added under that id later was shown with
// them, "last seen", until it answered itself.

// until waits for a condition that another goroutine brings about.
func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !ok(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}
	}
}

// The version. The poll has taken the new version in under the lock (noteVersionLocked says "write it"), the
// host is removed, and then the poll writes: what it writes is what the panel knows of the id at that moment,
// which is nothing, and not what it knew when it decided to write.
func TestAVersionIsNotWrittenForAHostThatWasRemovedInThatInstant(t *testing.T) {
	cfg := testutil.Config(t, []string{"https://pve1:8006"}, "http://laptop", "http://tower")
	state := filepath.Join(cfg.DataDir, "state.json")
	b := testutil.NewBackend()
	b.Metrics["laptop"] = versioned("16-a791c55")
	p, err := New(cfg, config.Secrets{}, b)
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	if raw, _ := os.ReadFile(state); !strings.Contains(string(raw), `"version": "16-a791c55"`) {
		t.Fatalf("the fixture: state.json after the first poll: %s", raw)
	}
	// the next answer has another version: what pollEntry does with it under the lock ...
	now, _ := p.now()
	now = now.Add(2 * versionSaveEvery)
	p.mu.Lock()
	write := p.noteVersionLocked("laptop", "17-bbbbbbb", now)
	p.mu.Unlock()
	if !write {
		t.Fatal("the fixture: the new version is not due to be written")
	}
	p.RemoveAgent("laptop")      // ... the host is removed in that instant ...
	p.saveVersion("laptop", now) // ... and what pollEntry does next
	if raw, _ := os.ReadFile(state); strings.Contains(string(raw), "laptop") || strings.Contains(string(raw), "17-bbbbbbb") {
		t.Errorf("state.json keeps a version for a host that was removed: %s", raw)
	}
	if v := adminstate.New(cfg.DataDir).AgentVersions()["laptop"]; v != "" {
		t.Errorf("after the next start the removed host would have the version %q", v)
	}
}

// When it was last seen. The write of the poll is on its way to the file (it waits for the file's lock, which
// the test holds, as another process of the panel's user would), the host is removed, and the write lands:
// the host's first time is in the file now, and the Remove, which found nothing of it in the file, took
// nothing out.
func TestALastSeenTimeIsNotLeftInTheFileForAHostThatWasRemovedInThatInstant(t *testing.T) {
	e := newSeenEnv(t, nil, seenStart)
	e.set(true)
	if err := os.MkdirAll(e.cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(e.cfg.DataDir, adminstate.LastSeenFile+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	e.p.mu.Lock()
	e.p.seenDrop = []string{"nobody"} // the write takes this with it when it makes its copy: the test sees that
	e.p.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); e.poll(time.Second) }() // the laptop answers for the first time
	until(t, "the poll's write has made its copy of the times", func() bool {
		e.p.mu.Lock()
		defer e.p.mu.Unlock()
		_, seen := e.p.lastSeen["laptop"]
		return seen && e.p.seenDrop == nil
	})
	wg.Add(1)
	go func() { defer wg.Done(); e.p.RemoveAgent("laptop") }()
	until(t, "the Remove has forgotten the host", func() bool {
		e.p.mu.Lock()
		defer e.p.mu.Unlock()
		_, seen := e.p.lastSeen["laptop"]
		return !seen
	})
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil { // the poll's write lands, and then the Remove's
		t.Fatal(err)
	}
	wg.Wait()
	if got := e.file("laptop"); got != "" {
		t.Errorf("last-seen.json keeps %q for a host that was removed", got)
	}
	e.p.FlushSeen()
	if got := e.file("laptop"); got != "" {
		t.Errorf("after FlushSeen: last-seen.json keeps %q for a host that was removed", got)
	}
}
