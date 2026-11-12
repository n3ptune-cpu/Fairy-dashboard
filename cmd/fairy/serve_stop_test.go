//go:build fairytest && unix

package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/web"
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

// Audit 5, round 3 (r3-p4-04): the notifications are written every ten seconds "and at the end", and nothing
// waited for that last write: `fairy serve` said "stopped" and the process ended while it was on its way. With
// a real kill -TERM the event of the last seconds was lost in about every second stop, and the temporary file
// of the write stayed in the data folder. serve now waits for it as it waits for the poller, inside the same
// five seconds.
//
// The whole thing as `fairy serve` runs it: an admin asks for a reboot with a countdown, the panel makes its
// notification ("reboot requested by ..."), the panel is stopped at once, and notifications.json is read the
// moment serve returns.
func TestTheLastNotificationIsWrittenWhenThePanelStops(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/metrics":
			_ = json.NewEncoder(w).Encode(map[string]any{"hostname": "pc-a", "agent_version": "16-aaaaaaa", "uptime_s": 100.0,
				"platform": map[string]any{"system": "Linux", "pretty": "Debian 13"}, "cpu": map[string]any{"percent": 1.0, "logical": 4.0},
				"memory": map[string]any{"total": 8 << 30, "used": 1 << 30}, "features": map[string]any{},
				"power_control": map[string]any{"enabled": true, "pending": nil, "min_delay_s": 10.0}})
		case "/power":
			_ = json.NewEncoder(w).Encode(map[string]any{"action": "reboot", "pending": map[string]any{"action": "reboot",
				"at": float64(time.Now().Add(time.Hour).Unix()), "delay_s": 3600.0, "requested_by": "test-admin"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer agent.Close()
	t.Setenv("FAIRY_TEST_AGENT_READ", "the-read-word-of-the-stand-in-agent")
	t.Setenv("FAIRY_TEST_AGENT_CONTROL", "the-control-word-of-the-stand-in-agent")
	c := newCase(t, "request_timeout: 1\nupdates: {check_latest: false}\nagents:\n  - {id: pc-a, name: PC-A, url: \""+agent.URL+
		"\", token_env: FAIRY_TEST_AGENT_READ, control_token_env: FAIRY_TEST_AGENT_CONTROL, power_control: true}\n")
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
	logs := func() string { b, _ := os.ReadFile(logFile.Name()); return string(b) }
	// the page's API, as the browser that opened the address with the start key
	jar, _ := cookiejar.New(nil)
	browser, signedIn := &http.Client{Jar: jar, Timeout: 5 * time.Second}, false
	get := func(path string) string {
		res, err := browser.Get("http://" + addr + path)
		if err != nil {
			return ""
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	api := func(path string) string {
		if !signedIn {
			raw, err := os.ReadFile(filepath.Join(c.data(), testKeyFile))
			if err != nil || !web.ValidTestKey(strings.TrimSpace(string(raw))) {
				return ""
			}
			get("/?key=" + strings.TrimSpace(string(raw)))
			signedIn = strings.Contains(get("/"), web.TestBanner)
		}
		return get(path)
	}
	until := func(what string, cond func() bool) {
		t.Helper()
		for i := 0; !cond(); i++ {
			select {
			case rc := <-done:
				t.Fatalf("serve ended, exit code %d, before %s: %s", rc, what, logs())
			case <-time.After(50 * time.Millisecond):
			}
			if i > 600 {
				t.Fatalf("not within 30 s: %s: %s", what, logs())
			}
		}
	}
	until("the host is online", func() bool { return strings.Contains(api("/api/systems"), `"status":"online"`) })
	time.Sleep(500 * time.Millisecond) // what the start itself had to write is written
	post := func(path, body string) (int, string) {
		req, _ := http.NewRequest("POST", "http://"+addr+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-With", "fairy")
		res, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	// the reboot, and the test build's "Confirm it's you" for it (the kept action runs with the confirmation)
	code, body := post("/api/systems/pc-a/power", `{"action": "reboot", "delay_s": 3600}`)
	var asked struct {
		Detail struct {
			PendingID string `json:"pending_id"`
		} `json:"detail"`
	}
	if code != 401 || json.Unmarshal([]byte(body), &asked) != nil || asked.Detail.PendingID == "" {
		t.Fatalf("set-up: the reboot request: %d %s", code, body)
	}
	if code, body = post("/api/me/step-up", `{"pending_id": "`+asked.Detail.PendingID+`", "next": "/"}`); code != 200 || !strings.Contains(api("/api/notifications"), "power_requested") {
		t.Fatalf("set-up: the confirmation: %d %s; notifications: %.300s; log: %s", code, body, api("/api/notifications"), logs())
	}
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM) // at once: before the next ten-second save
	select {
	case rc := <-done:
		// read at once: nothing may still be on its way when serve returns
		raw, _ := os.ReadFile(filepath.Join(c.data(), "notifications.json"))
		left, _ := filepath.Glob(filepath.Join(c.data(), ".notifications.json.*"))
		if rc != 0 || !strings.Contains(string(raw), "power_requested") || len(left) != 0 {
			t.Fatalf("exit code %d; notifications.json after the stop has the event: %v; temporary files left: %v", rc, strings.Contains(string(raw), "power_requested"), left)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
}
