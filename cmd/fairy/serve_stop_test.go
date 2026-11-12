//go:build fairytest && unix

package main

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Second review of the fixes for audit 5, round 2 (P11, MUST FIX 3): an agent's version is written to
// state.json at most once a minute, and the one that still waits is written when the poller stops
// (Poller.FlushVersions). `fairy serve` started the poller and waited for nothing, so the process ended before
// that write: with a real kill -TERM the last version was lost 10 times of 10. serve now waits for the poller
// before it says "stopped".
//
// The whole thing as `fairy serve` runs it: an agent says 16 (written at once), then 17 within the minute (it
// waits), the panel is stopped, and state.json is read the moment serve returns.
func TestTheLastAgentVersionIsWrittenWhenThePanelStops(t *testing.T) {
	var version atomic.Value
	var asked atomic.Int64
	version.Store("16-aaaaaaa")
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		asked.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"hostname": "pc-a", "agent_version": version.Load(), "uptime_s": 100.0,
			"platform": map[string]any{"system": "Linux", "pretty": "Debian 13"}, "cpu": map[string]any{"percent": 1.0, "logical": 4.0},
			"memory": map[string]any{"total": 8 << 30, "used": 1 << 30}, "features": map[string]any{}})
	}))
	defer agent.Close()
	t.Setenv("FAIRY_TEST_AGENT_READ", "the-read-word-of-the-stand-in-agent")
	c := newCase(t, "request_timeout: 1\nupdates: {check_latest: false}\nagents:\n  - {id: pc-a, name: PC-A, url: \""+agent.URL+"\", token_env: FAIRY_TEST_AGENT_READ}\n")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	stdout, stderr, old := os.Stdout, os.Stderr, slog.Default() // serve prints its address and logs to what it finds
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(c.dir, "stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = null, logFile
	t.Cleanup(func() { os.Stdout, os.Stderr = stdout, stderr; slog.SetDefault(old); null.Close(); logFile.Close() })
	done := make(chan int, 1)
	go func() { done <- serve([]string{"--config", c.f.cfgPath, "--listen", addr}) }()
	kept := func() string {
		var st struct {
			AgentVersions map[string]struct{ Version string } `json:"agent_versions"`
		}
		raw, _ := os.ReadFile(filepath.Join(c.data(), "state.json"))
		_ = json.Unmarshal(raw, &st)
		return st.AgentVersions["pc-a"].Version
	}
	logs := func() string { b, _ := os.ReadFile(logFile.Name()); return string(b) }
	for i := 0; kept() != "16-aaaaaaa"; i++ {
		select {
		case rc := <-done:
			t.Fatalf("serve ended at once, exit code %d: %s", rc, logs())
		case <-time.After(50 * time.Millisecond):
		}
		if i > 400 {
			t.Fatalf("the agent's first version was not written within 20 s (kept %q): %s", kept(), logs())
		}
	}
	version.Store("17-bbbbbbb") // within the minute after that write: this one waits
	for n, i := asked.Load(), 0; asked.Load() < n+3; i++ {
		if time.Sleep(50 * time.Millisecond); i > 400 {
			t.Fatal("the agent was not polled again within 20 s")
		}
	}
	if got := kept(); got != "16-aaaaaaa" {
		t.Fatalf("set-up: the second version was written at once (%q), so nothing waits for the stop", got)
	}
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	select {
	case rc := <-done:
		if got := kept(); rc != 0 || got != "17-bbbbbbb" { // read at once: nothing may still be on its way when serve returns
			t.Fatalf("exit code %d; state.json after the stop has %q, the agent's last word was 17-bbbbbbb", rc, got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
}

// The wait for the poller has an end: a poll that hangs cannot keep the panel from stopping.
func TestTheStopDoesNotWaitForeverForThePoller(t *testing.T) {
	never, closed := make(chan struct{}), make(chan struct{})
	close(closed)
	start := time.Now()
	if waitStopped(never, 50*time.Millisecond) || time.Since(start) < 50*time.Millisecond || time.Since(start) > 5*time.Second {
		t.Fatalf("a poller that never stops: waited %s", time.Since(start))
	}
	if !waitStopped(closed, time.Hour) {
		t.Fatal("a poller that has stopped")
	}
	if pollerStopWait < 2*time.Second || pollerStopWait > 30*time.Second {
		t.Fatalf("the stop waits %s for the poller: a few seconds are meant", pollerStopWait)
	}
}
