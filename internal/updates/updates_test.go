package updates

import (
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-agent/pkg/release"
	"github.com/n3ptune-cpu/Fairy-agent/pkg/release/releasetest"
)

func wait(t *testing.T, c *Checker, ok func(Latest) bool) Latest {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l := c.Latest(); ok(l) {
			return l
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no answer: %+v", c.Latest())
	return Latest{}
}

func TestCheckerCachesAndToleratesFailures(t *testing.T) {
	gh := releasetest.New(t, release.AgentRepo)
	gh.Publish("build-42", "fairy-agent", "42-1a2b3c4", map[string][]byte{"fairy-agent-linux-amd64": []byte("x")}, nil)
	c := &Checker{Client: gh.Client(), Key: gh.Pub, Program: "fairy-agent", TTL: time.Hour, ErrTTL: 50 * time.Millisecond}
	if l := c.Latest(); l.Error == "" || l.Tag != "" { // never blocks: first call only starts the lookup
		t.Fatalf("%+v", l)
	}
	l := wait(t, c, func(l Latest) bool { return l.Tag != "" })
	if l.Version != "42-1a2b3c4" || !l.Verified || l.Build == nil || *l.Build != 42 || l.Error != "" {
		t.Fatalf("%+v", l)
	}
	hits := len(gh.Hits())
	for i := 0; i < 20; i++ {
		c.Latest()
	}
	if len(gh.Hits()) != hits {
		t.Fatal("not cached")
	}
	// GitHub unreachable: the last good answer stays, with the error
	gh.SetBroken(true)
	c.mu.Lock()
	c.at = time.Now().Add(-2 * time.Hour)
	c.mu.Unlock()
	l = wait(t, c, func(l Latest) bool { return l.Error != "" })
	if l.Tag != "build-42" || !strings.Contains(l.Error, "cannot check GitHub") {
		t.Fatalf("%+v", l)
	}
}

func TestCheckerWithoutKeyShowsTheTag(t *testing.T) {
	gh := releasetest.New(t, release.AgentRepo)
	gh.Publish("build-7", "fairy-agent", "7-1a2b3c4", map[string][]byte{"fairy-agent-linux-amd64": []byte("x")}, nil)
	c := &Checker{Client: gh.Client(), Program: "fairy-agent"}
	l := c.Refresh()
	if l.Tag != "build-7" || l.Verified || l.Version != "" || l.Build == nil || *l.Build != 7 {
		t.Fatalf("%+v", l)
	}
	// a release signed with another key is refused when this build has a key
	other := releasetest.New(t, release.AgentRepo)
	c = &Checker{Client: gh.Client(), Key: other.Pub, Program: "fairy-agent"}
	if l := c.Refresh(); l.Error == "" || l.Tag != "" {
		t.Fatalf("%+v", l)
	}
}
