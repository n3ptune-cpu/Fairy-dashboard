package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/demo"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
)

// The demo's Notifications page starts with a day of made-up events, and two of them say who asked: the
// viewer of the demo shut his PC down and woke it again. They named him by an address that no demo account
// has, so both lines read "by (address hidden)" in a demo that otherwise names everybody.
//
// They are stored with the address of the demo's own viewer, as a version before build 17 stored such a line,
// and read by his user name, for the admin and in the preview as the viewer. No seeded line shows an address
// or the words that stand for one.
func TestDemoSeededNotificationsNameTheViewerByHisUserName(t *testing.T) {
	cfg, secrets, p, err := demo.Build(t.TempDir(), "http://127.0.0.1:8090")
	if err != nil {
		t.Fatal(err)
	}
	p.PollOnce(context.Background())
	u := demo.User
	s := New(cfg, secrets, p, &u, "test")
	demo.SeedNotes(s.Notes) // as `fairy serve --demo` does
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	e := &env{t: t, s: s, srv: srv}
	bob := s.Access.Load().Users["demo-bob"]
	if bob.Username != "bob" || !strings.Contains(bob.Email, "@") {
		t.Fatalf("the demo's viewer: %+v", bob)
	}
	stored := 0
	for _, ev := range s.Notes.List("", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events {
		if strings.Contains(ev.Text, "@") {
			stored++
			if !strings.HasSuffix(ev.Text, " by "+bob.Email) || ev.ByName {
				t.Errorf("a seeded line with an address: %q (names people as this version does: %v)", ev.Text, ev.ByName)
			}
		}
	}
	if stored != 2 {
		t.Errorf("%d seeded lines are stored with an address, want the 2 that say who asked", stored)
	}
	for view, lines := range map[string]int{"admin": 11, "viewer:Bob": 8} {
		if err := s.SetDemoView(view); err != nil {
			t.Fatal(err)
		}
		x := e.do("GET", "/api/notifications?limit=200", nil)
		if x.code != 200 {
			t.Fatal(view, x.code, string(x.raw))
		}
		got := map[string]bool{}
		for _, ev := range x.body["events"].([]any) {
			text := fmt.Sprint(ev.(map[string]any)["text"])
			got[text] = true
			if strings.Contains(text, "@") || strings.Contains(text, "hidden") {
				t.Errorf("the demo as %s: a seeded line reads %q", view, text)
			}
		}
		if len(got) != lines {
			t.Errorf("the demo as %s lists %d lines, want %d", view, len(got), lines)
		}
		for _, want := range []string{"shut down in 1 min, requested by bob", "woken with Wake-on-LAN by bob"} {
			if !got[want] {
				t.Errorf("the demo as %s: no line reads %q; the lines: %q", view, want, keysOf(got))
			}
		}
	}
}

func keysOf(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
