package demo

import (
	"context"
	"sync"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/config"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/jsonx"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/updates"
)

// Simulated self-update: agents with self_update: true run build 41 until an admin presses Update; then they
// "download" for a few seconds, "install" (offline for a moment) and come back as build 42, the latest release.
const (
	demoOld = "41-0c0ffee"
	demoNew = "42-1a2b3c4"
)

// Latest is the demo's "latest Fairy-agent release".
func Latest() *Releases {
	n := 42
	pub := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Minute)
	return newReleases(updates.Latest{Tag: "build-42", Version: demoNew, Build: &n, Verified: true, Published: &pub,
		URL: "https://github.com/n3ptune-cpu/Fairy-agent/releases/tag/build-42"})
}

// PanelLatest is the demo's "latest Fairy-dashboard release" (the demo panel runs a local build, so the page
// cannot compare and shows no panel update).
func PanelLatest() *Releases {
	n := 17
	pub := time.Now().UTC().Add(-26 * time.Hour).Truncate(time.Minute)
	return newReleases(updates.Latest{Tag: "build-17", Version: "17-5e6f7a8", Build: &n, Verified: true, Published: &pub,
		URL: "https://github.com/n3ptune-cpu/Fairy-dashboard/releases/tag/build-17"})
}

// Releases is a demo release source: a fixed release, "checked" a few minutes before the demo started.
// Check for updates takes a moment (Delay, as if GitHub answered) and only moves the check time: the demo
// never contacts GitHub.
type Releases struct {
	Delay time.Duration

	mu sync.Mutex
	l  updates.Latest
}

func newReleases(l updates.Latest) *Releases {
	at := time.Now().UTC().Add(-7 * time.Minute).Truncate(time.Second)
	l.CheckedAt = &at
	return &Releases{Delay: 900 * time.Millisecond, l: l}
}

// Latest returns the fixed release and when it was last "checked".
func (r *Releases) Latest() updates.Latest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.l
}

// CheckNow waits Delay (or until ctx ends) and records the check.
func (r *Releases) CheckNow(ctx context.Context) updates.Latest {
	t := time.NewTimer(r.Delay)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
		r.mu.Lock()
		defer r.mu.Unlock()
		l := r.l
		l.Error = "cannot check GitHub: " + ctx.Err().Error()
		return l
	}
	now := time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.l.CheckedAt = &now
	return r.l
}

type simUpdate struct {
	start float64
	done  bool
	last  map[string]any
}

// updateState is the agent's version and self_update block now. Caller holds b.mu.
func (b *Backend) updateState(id string, now float64) (string, map[string]any) {
	u := b.updates[id]
	st := map[string]any{"state": "idle", "since": now - 600}
	if u == nil {
		return demoOld, st
	}
	if u.done {
		st["last"] = u.last
		return demoNew, st
	}
	switch t := now - u.start; {
	case t < 4:
		return demoOld, map[string]any{"state": "downloading", "message": "downloading " + demoNew + " (build-42)", "target": demoNew, "since": u.start}
	case t < 7:
		return demoOld, map[string]any{"state": "applying", "message": "installing " + demoNew + "; the agent restarts", "target": demoNew, "since": u.start + 4}
	}
	u.done = true
	u.last = map[string]any{"id": "demo", "ok": true, "from": demoOld, "to": demoNew, "message": "updated from " + demoOld + " to " + demoNew, "at": now}
	b.downUntil[id] = now + 2 // the restart
	st["last"] = u.last
	return demoNew, st
}

func (b *Backend) simUpdate(a config.Agent) (int, jsonx.M, error) {
	if !a.SelfUpdate {
		return 404, jsonx.M{"error": "self update is disabled on this agent", "code": "self_update_disabled"}, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	u := b.updates[a.ID]
	if u != nil && !u.done {
		return 409, jsonx.M{"error": "an update is already in progress"}, nil
	}
	if u != nil && u.done {
		b.updates[a.ID] = &simUpdate{done: true, last: map[string]any{"id": "demo2", "ok": true, "from": demoNew, "to": demoNew,
			"message": "already up to date (" + demoNew + ")", "at": epoch()}}
		return 202, jsonx.M{"state": "downloading", "current": demoNew}, nil
	}
	b.updates[a.ID] = &simUpdate{start: epoch()}
	return 202, jsonx.M{"state": "downloading", "current": demoOld}, nil
}
