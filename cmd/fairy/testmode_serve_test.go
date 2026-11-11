//go:build fairytest && unix

package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
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
	done := make(chan int, 1)
	go func() { done <- serve([]string{"--config", c.f.cfgPath, "--listen", addr, "--log-level", "error"}) }()
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
	if code != 200 || !strings.Contains(page, web.TestBanner) {
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
}
