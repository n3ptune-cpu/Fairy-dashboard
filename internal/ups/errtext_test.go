package ups

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/textsafe"
)

// T-63 (the UPS half of T-06, found again in review): a NUT server answers a failed request with a line "ERR
// <text>", up to 4096 bytes of its own choosing. The panel kept that line as the UPS's error as it was: 3888
// characters with ESC, BEL and a right-to-left override in health.json, in ups-events.jsonl and in the
// notification. Now the error of a UPS is cleaned and cut like every other error text (textsafe.ErrorText).
func TestErrorTextOfANUTServerIsShortAndClean(t *testing.T) {
	var hostile atomic.Bool
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	payload := "ACCESS-DENIED \x1b[2J\x1b[31m\x07 \u202egnp.exe " + strings.Repeat("A long text of the server's own choosing. ", 90)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					switch line = strings.TrimSpace(line); {
					case hostile.Load() && strings.HasPrefix(line, "LIST"):
						fmt.Fprint(c, "ERR "+payload+"\n")
					case line == "LIST UPS":
						fmt.Fprint(c, "BEGIN LIST UPS\nUPS cp \"CyberPower\"\nEND LIST UPS\n")
					case line == "LIST VAR cp":
						fmt.Fprint(c, "BEGIN LIST VAR cp\nVAR cp ups.status \"OL\"\nVAR cp battery.charge \"100\"\nEND LIST VAR cp\n")
					case line == "LOGOUT":
						return
					}
				}
			}()
		}
	}()
	m, err := NewManager([]map[string]any{{"id": "rack", "host": "127.0.0.1", "port": ln.Addr().(*net.TCPAddr).Port, "powers": []any{"node-a"}}},
		5, 2*time.Second, 0.2, 3600, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.PollAll(ctx)
	if st := m.State("rack/cp"); st == nil || st.Status != "online" || st.Error != nil {
		t.Fatalf("control case: %+v", st)
	}

	clean := func(what, s string) {
		t.Helper()
		if !utf8.ValidString(s) || strings.ContainsAny(s, "\x1b\x07\r\n\u202e") {
			t.Fatalf("%s is not clean: %q", what, s)
		}
	}
	hostile.Store(true)
	m.PollAll(ctx)
	st := m.State("rack/cp")
	if st == nil || st.Error == nil || !st.Stale {
		t.Fatalf("the UPS did not fail: %+v", st)
	}
	if len(payload) < 3800 || !strings.Contains(*st.Error, "ACCESS-DENIED") {
		t.Fatalf("the test did not get the server's text through (%d bytes sent): %q", len(payload), *st.Error)
	}
	if max := textsafe.MaxError + len("…"); len(*st.Error) > max {
		t.Fatalf("the UPS's error is %d bytes, want at most %d", len(*st.Error), max)
	}
	clean("the UPS's error", *st.Error)

	time.Sleep(250 * time.Millisecond) // longer than offline_after: the next failure is "communication lost"
	before := len(m.EventsSince(0))
	m.PollAll(ctx)
	evs := m.EventsSince(0)[before:]
	if len(evs) != 1 || evs[0].Kind != "communication_lost" {
		t.Fatalf("events: %+v", evs)
	}
	if len(evs[0].Message) > textsafe.MaxError+100 {
		t.Fatalf("the event's message is %d bytes", len(evs[0].Message))
	}
	clean("the event's message", evs[0].Message)
}
