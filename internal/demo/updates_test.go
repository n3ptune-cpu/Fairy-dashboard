package demo

import (
	"context"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/updates"
)

// In demo mode Check for updates never contacts GitHub: it takes a moment and only moves the check time.
func TestDemoCheckNow(t *testing.T) {
	var _ updates.CheckerNow = Latest()
	r := Latest()
	r.Delay = 20 * time.Millisecond
	before := r.Latest()
	if before.Tag != "build-42" || before.CheckedAt == nil || time.Since(*before.CheckedAt) < 5*time.Minute {
		t.Fatalf("%+v", before)
	}
	start := time.Now()
	l := updates.CheckNow(context.Background(), r)
	if time.Since(start) < r.Delay || l.Tag != "build-42" || l.Error != "" || l.CheckedAt == nil || time.Since(*l.CheckedAt) > time.Second {
		t.Fatalf("%+v", l)
	}
	if got := r.Latest(); got.CheckedAt == nil || !got.CheckedAt.Equal(*l.CheckedAt) {
		t.Fatalf("not kept: %+v", got)
	}
	// a cancelled request gives up and keeps the old check time
	r.Delay = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if l2 := r.CheckNow(ctx); l2.Error == "" || !l2.CheckedAt.Equal(*l.CheckedAt) {
		t.Fatalf("%+v", l2)
	}
	if p := PanelLatest().Latest(); p.Tag != "build-17" || p.CheckedAt == nil {
		t.Fatalf("%+v", p)
	}
}
