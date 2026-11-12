//go:build fairytest && unix

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/web"
)

// The whole thing once, as `fairy serve` runs it: the page comes without a sign-in and with the banner, the
// one user is the test admin, and the panel's own update is off.
func TestTestBuildServes(t *testing.T) {
	c := newCase(t, "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	// what the program prints on its standard output: the one line with the address to open
	stdout := os.Stdout
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = pw
	t.Cleanup(func() { os.Stdout = stdout })
	printed := make(chan string, 1)
	go func() { b, _ := io.ReadAll(pr); printed <- string(b) }()
	stderr, old := os.Stderr, slog.Default() // and what it logs (serve logs to the standard error it finds)
	logFile, err := os.Create(filepath.Join(c.dir, "stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = logFile
	t.Cleanup(func() { os.Stderr = stderr; slog.SetDefault(old); logFile.Close() })
	done := make(chan int, 1)
	go func() { done <- serve([]string{"--config", c.f.cfgPath, "--listen", addr, "--log-level", "debug"}) }()
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 5 * time.Second}
	get := func(path string) (int, string) {
		res, err := hc.Get("http://" + addr + path)
		if err != nil {
			return 0, err.Error()
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
	code, page := 0, ""
	for i := 0; i < 200 && code == 0; i++ {
		select {
		case rc := <-done:
			t.Fatal("serve ended at once, exit code", rc)
		case <-time.After(50 * time.Millisecond):
		}
		code, page = get("/")
	}
	// r1-p1-01 / r1-p4-01: whoever only reaches the port (through a proxy that leaves no trace in the request,
	// or a forwarded port) gets one line and no session; the start key is in the data folder, mode 0600, and
	// the address with it leads to the page.
	if code != 403 || strings.TrimSpace(page) != "Test build: open the address printed in its console window." || len(jar.Cookies(&url.URL{Scheme: "http", Host: addr})) != 0 {
		t.Fatalf("without the start key: %d %.200s", code, page)
	}
	if code, body := get("/api/me"); code != 403 || !strings.Contains(body, "open the address printed") {
		t.Fatalf("/api/me without the start key: %d %s", code, body)
	}
	keyFile := filepath.Join(c.data(), testKeyFile)
	raw, err := os.ReadFile(keyFile)
	st, _ := os.Stat(keyFile)
	key := strings.TrimSpace(string(raw))
	if err != nil || !web.ValidTestKey(key) || len(key) < 43 || st.Mode().Perm() != 0o600 {
		t.Fatalf("the start key's file: %v, %d characters, mode %v", err, len(key), st.Mode().Perm())
	}
	code, page = get("/?key=" + key) // the client follows the redirect to the clean address
	if code != 200 || !strings.Contains(page, web.TestBanner) || strings.Contains(page, key) {
		t.Fatalf("the page: %d %.200s", code, page)
	}
	var me struct{ Sub, Role string }
	if code, body := get("/api/me"); code != 200 || json.Unmarshal([]byte(body), &me) != nil || me.Sub != "test-admin" || me.Role != "admin" {
		t.Fatalf("/api/me: %d %s", code, body)
	}
	var upd struct {
		Ready        bool
		Why, Version string
	}
	if code, body := get("/api/admin/panel-update"); code != 200 || json.Unmarshal([]byte(body), &upd) != nil || upd.Ready ||
		!strings.Contains(upd.Why, "test build") || !strings.HasSuffix(upd.Version, "TEST BUILD") {
		t.Fatalf("the Updates page must say test build and offer no update: %d %s", code, body)
	}
	_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT) // Ctrl+C
	select {
	case rc := <-done:
		if rc != 0 {
			t.Fatal("exit code", rc)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
	// the address was printed once, with the key; the log never has the key
	os.Stdout = stdout
	pw.Close()
	out := <-printed
	if strings.Count(out, key) != 1 || !strings.Contains(out, "http://"+addr+"/?key="+key+"\n") || strings.Count(out, "\n") != 1 {
		t.Fatalf("standard output must be the one line with the address: %q", strings.ReplaceAll(out, key, "KEY"))
	}
	audit, _ := os.ReadFile(filepath.Join(c.data(), "audit.log"))
	logs, _ := os.ReadFile(logFile.Name())
	if !bytes.Contains(logs, []byte("fairy listening")) || !bytes.Contains(audit, []byte("test-admin")) {
		t.Fatalf("the log and the audit log were not read: %d and %d bytes", len(logs), len(audit))
	}
	if bytes.Contains(logs, []byte(key)) || bytes.Contains(audit, []byte(key)) {
		t.Fatal("the start key is in the log or the audit log")
	}
}
