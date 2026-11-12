package web

import (
	"fmt"
	"strings"
	"testing"

	"github.com/n3ptune-cpu/Fairy-dashboard/internal/notifications"
	"github.com/n3ptune-cpu/Fairy-dashboard/internal/wakerun"
)

// r2-p2-06 (audit 5, round 2): a run's one notification joined a sentence for every machine and was cut at
// 1500 characters. In the auditor's run of sixteen machines it ended in "H12: Left ON: its agent reports no
// uptime, so Fairy cannot tel…": H13 to H16 were not named, and two of them were left ON.
func TestRunNotificationNamesEveryMachineThatNeedsALook(t *testing.T) {
	const onBy = "It is on (its address took the connection and gave no answer: timeout), and its agent does not answer. Nothing was sent to it and it stays on."
	const leftOn = "It is still on 5 min after its shutdown was due: the shutdown did not work. It is ON. It was updated to 17-bbbbbbb."
	mk := func(i int, state, on, text string) *wakerun.Host {
		return &wakerun.Host{ID: fmt.Sprintf("h%02d", i), Name: fmt.Sprintf("H%02d", i), State: state, On: on, Text: text}
	}
	// the auditor's run, in its order: one switched off, ten that are on with a broken agent, then the ones that need a look
	hosts := []*wakerun.Host{mk(1, wakerun.StOff, wakerun.OnNo, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again.")}
	for i := 2; i <= 10; i++ {
		hosts = append(hosts, mk(i, wakerun.StSkipped, wakerun.OnYes, onBy))
	}
	hosts = append(hosts,
		mk(11, wakerun.StLeftOn, wakerun.OnYes, "It answered after the wake but did not look freshly started (up for 3 h): it may have been asleep, or use Fast Startup. It was left on. It was not updated."),
		mk(12, wakerun.StLeftOn, wakerun.OnYes, "Left ON: its agent reports no uptime, so Fairy cannot tell that it started after the wake. It was updated to 17-bbbbbbb."),
		mk(13, wakerun.StOff, wakerun.OnNo, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again."),
		mk(14, wakerun.StLeftOn, wakerun.OnYes, leftOn),
		mk(15, wakerun.StGaveUp, wakerun.OnUnknown, "It did not come online within 5 min after the wake. It may still be starting, or be on without its network connection (Tailscale): if it did switch on, it is ON. Fairy has not switched it off and will not."),
		mk(16, wakerun.StLeftOn, wakerun.OnYes, leftOn))
	lead := "run started by test-admin is over"

	got := wuNoteText(lead, hosts, wuNoteMax)
	t.Logf("%d characters: %s", len(got), got)
	if len(got) > wuNoteMax || strings.Contains(got, "…") {
		t.Fatalf("the text is cut or too long (%d)", len(got))
	}
	// the five that need a look are there with their whole sentences, before anything else
	at := len(lead) + 1
	for _, i := range []int{11, 12, 14, 15, 16} {
		want := hosts[i-1].Name + ": " + hosts[i-1].Text
		j := strings.Index(got, want)
		if j < at {
			t.Fatalf("%s is not there with its whole sentence, in the run's order: %s", hosts[i-1].Name, got)
		}
		at = j
	}
	if !strings.HasSuffix(got, " | The others: 2 switched off again, 9 skipped. Every machine's result: Updates page, Last run.") {
		t.Fatalf("the rest is not counted: %s", got)
	}
	for _, i := range []int{1, 2, 13} { // the ones that need no look are counted, not named
		if strings.Contains(got, hosts[i-1].Name+":") {
			t.Fatalf("%s is named: %s", hosts[i-1].Name, got)
		}
	}

	t.Run("sixteen machines left on with long sentences: all sixteen are named, none is cut", func(t *testing.T) {
		long := "Left ON: the shutdown was not accepted (" + strings.Repeat("the helper is stopped; ", 12) + "start it first). It was updated to 17-bbbbbbb."
		var all []*wakerun.Host
		for i := 1; i <= 16; i++ {
			all = append(all, mk(i, wakerun.StLeftOn, map[bool]string{true: wakerun.OnUnknown, false: wakerun.OnYes}[i == 16], long))
		}
		got := wuNoteText(lead, all, wuNoteMax)
		t.Logf("%d characters: %s", len(got), got)
		if len(got) > wuNoteMax || strings.Contains(got, "…") {
			t.Fatalf("the text is cut or too long (%d)", len(got))
		}
		whole := strings.Count(got, long)
		if whole < 3 || !strings.Contains(got, "H01: "+long+" | H02: "+long) {
			t.Fatalf("%d whole sentences, want the first ones: %s", whole, got)
		}
		for i := whole + 1; i <= 15; i++ {
			if !strings.Contains(got, fmt.Sprintf("H%02d (left ON)", i)) {
				t.Fatalf("H%02d is not named: %s", i, got)
			}
		}
		if !strings.Contains(got, "H16 (not confirmed)") || !strings.HasSuffix(got, " Every machine's result: Updates page, Last run.") {
			t.Fatalf("the end: %s", got)
		}
		// every sentence that is there is there whole: after the last whole one comes the list of names
		if rest := got[strings.LastIndex(got, long)+len(long):]; !strings.HasPrefix(rest, " | Also: H") {
			t.Fatalf("after the last whole sentence: %q", rest)
		}
	})

	t.Run("a run that fits reads as before: a sentence for every machine, the ones that need a look first", func(t *testing.T) {
		two := []*wakerun.Host{mk(1, wakerun.StOff, wakerun.OnNo, "Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again."),
			mk(2, wakerun.StLeftOn, wakerun.OnYes, "Left ON: 1 guest is running on it (win11). It was updated to 17-bbbbbbb.")}
		want := lead + ". H02: Left ON: 1 guest is running on it (win11). It was updated to 17-bbbbbbb. | H01: Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again."
		if got := wuNoteText(lead, two, wuNoteMax); got != want {
			t.Fatalf("%s", got)
		}
		if one := wuNoteText(lead, two[:1], wuNoteMax); one != lead+". H01: Came online after the wake, updated from 16-aaaaaaa to 17-bbbbbbb, and switched off again." {
			t.Fatalf("%s", one)
		}
	})

	t.Run("any limit: never longer, never a cut sentence", func(t *testing.T) {
		for limit := 150; limit <= 4000; limit += 7 {
			got := wuNoteText(lead, hosts, limit)
			if len(got) > limit {
				t.Fatalf("limit %d: %d characters", limit, len(got))
			}
			for _, part := range strings.Split(strings.TrimPrefix(got, lead+". "), " | ") {
				if i := strings.Index(part, ": "); i == 3 && strings.HasPrefix(part, "H") { // a machine's sentence
					whole := false
					for _, h := range hosts {
						whole = whole || strings.HasPrefix(part, h.Name+": "+h.Text)
					}
					if !whole {
						t.Fatalf("limit %d: a cut sentence: %q", limit, part)
					}
				}
			}
			for _, i := range []int{11, 12, 14, 15, 16} { // named, with the sentence or with two words
				if !strings.Contains(got, hosts[i-1].Name+": ") && !strings.Contains(got, hosts[i-1].Name+" (") && !strings.Contains(got, ". 5 of 16 machines need a look.") {
					t.Fatalf("limit %d: %s is not named: %s", limit, hosts[i-1].Name, got)
				}
			}
		}
	})
}

// The whole way: a run of the farm ends, and its notification is the new text.
func TestWakeUpdateRunEndNotification(t *testing.T) {
	e, f := lostFarm(t, "")
	f.wakes = false // pc-a never comes online
	if _, err := e.s.WakeRuns.Start(wakerun.StartRequest{Hosts: []string{"pc-a"}, By: "Alice", BySub: "u2"}); err != nil {
		t.Fatal(err)
	}
	f.finish()
	for _, n := range e.s.Notes.List("u2", func(notifications.Event) bool { return true }, notifications.Filter{Limit: 200}).Events {
		if n.Kind == "wakeupdate_done" {
			want := "run started by Alice is over. PC-A: It did not come online within 5 min after the wake. It may still be starting, or be on without its network " +
				"connection (Tailscale): if it did switch on, it is ON. Fairy has not switched it off and will not."
			if n.Text != want || n.Severity != notifications.Warning {
				t.Fatalf("%s: %s", n.Severity, n.Text)
			}
			return
		}
	}
	t.Fatal("no notification for the run's end")
}
