package web

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// r3-p4-04, the server's side: Run returns as soon as its listener is closed, and the notifications of the
// last seconds are written by the watcher after that. Saved says when that write is done, so that `fairy
// serve` can wait for it (cmd/fairy: TestTheLastNotificationIsWrittenWhenThePanelStops, with a real stop).
func TestSavedIsClosedAfterTheLastNotificationsAreWritten(t *testing.T) {
	e := setup(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.s.Run(ctx, ln) }()
	select {
	case <-e.s.Saved():
		t.Fatal("Saved is closed while the server runs")
	case <-time.After(300 * time.Millisecond):
	}
	e.s.Notes.Add(notifications.Event{Kind: "panel_update_started", Type: "update", Severity: notifications.Info, Host: "Fairy", Text: "panel update started by Alice"})
	file := filepath.Join(e.s.Cfg.DataDir, "notifications.json")
	if b, _ := os.ReadFile(file); strings.Contains(string(b), "panel_update_started") {
		t.Fatal("set-up: the event was written at once, so nothing waits for the stop")
	}
	cancel()
	select {
	case <-e.s.Saved():
	case <-time.After(5 * time.Second):
		t.Fatal("Saved was not closed within 5 s after the stop")
	}
	b, _ := os.ReadFile(file) // read at once: the write is over when Saved is closed
	left, _ := filepath.Glob(filepath.Join(e.s.Cfg.DataDir, ".notifications.json.*"))
	if !strings.Contains(string(b), "panel_update_started") || len(left) != 0 {
		t.Fatalf("after Saved: the event is in the file: %v; temporary files: %v", strings.Contains(string(b), "panel_update_started"), left)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}
